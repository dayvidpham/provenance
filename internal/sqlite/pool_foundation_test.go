package sqlite

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const poolTestTimeout = 5 * time.Second

// poolDrainSettle bounds the negative "Close is still draining" assertions.
//
// Those assertions are the only proof that Pool.Close blocks until every
// outstanding lease is returned, which is the entire safety argument for
// DB.Close routing shutdown solely through the pool. A non-blocking probe
// (select with default) samples the channel at one instant and would let a
// genuinely early Close return go unobserved whenever the closing goroutine had
// not been scheduled yet. A bounded settle window converts that scheduling
// coincidence into a real observation: an early return has this long to appear.
const poolDrainSettle = 50 * time.Millisecond

// requirePoolCloseStillDraining fails if Close publishes a result within
// poolDrainSettle while outstanding leases are still held.
func requirePoolCloseStillDraining(t *testing.T, subject string, outstanding int, closeOperation *poolCloseOperation) {
	t.Helper()
	select {
	case <-closeOperation.done:
		t.Fatalf("Close returned while %d outstanding %s lease(s) were still held: %v", outstanding, subject, closeOperation.err)
	case <-time.After(poolDrainSettle):
	}
}

type poolPragma uint8

const (
	poolPragmaForeignKeys poolPragma = iota
	poolPragmaBusyTimeout
	poolPragmaJournalMode
)

// observedDoneContext proves a consumer evaluated Done before signaling. The
// wrapped cancellation behavior is unchanged.
type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (ctx *observedDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.observed) })
	return ctx.Context.Done()
}

type poolBindResult struct {
	scope *connScope
	err   error
}

type poolTakeResult struct {
	conn *zs.Conn
	err  error
}

func waitPoolSignal(t *testing.T, operation string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(poolTestTimeout):
		t.Fatalf("%s was not observed within %v", operation, poolTestTimeout)
	}
}

func openPoolFileDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir()+"/pool.db", nil)
	if err != nil {
		t.Fatalf("Open file DB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close file DB: %v", err)
		}
	})
	return db
}

func openPoolMemoryDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:", nil)
	if err != nil {
		t.Fatalf("Open memory DB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close memory DB: %v", err)
		}
	})
	return db
}

func takePoolScope(t *testing.T, db *DB) *connScope {
	t.Helper()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bindScope: %v", err)
	}
	return scope
}

func waitPoolError(t *testing.T, operation string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(poolTestTimeout):
		t.Fatalf("%s did not complete within %v", operation, poolTestTimeout)
		return nil
	}
}

func waitPoolTakeResult(t *testing.T, operation string, done <-chan poolTakeResult) poolTakeResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(poolTestTimeout):
		t.Fatalf("%s did not complete within %v", operation, poolTestTimeout)
		return poolTakeResult{}
	}
}

// unboundedPoolSQL is one statement goroutine running on a leased connection.
//
// done carries the statement's error and is then CLOSED by that goroutine. The
// close is what makes goroutine TERMINATION observable, separately from the
// error VALUE: a receive on done blocks while the statement is still executing
// and completes immediately once the goroutine is gone, whether or not the
// error was already consumed by an assertion. Without the close, a cleanup path
// could observe the result at most once and so could not distinguish "statement
// finished" from "statement still live on this connection".
type unboundedPoolSQL struct {
	started <-chan struct{}
	done    <-chan error
}

// poolCloseOperation publishes one DB.Close result without consuming it. The
// completion channel is the happens-before edge for err, so an assertion and
// cleanup can both observe the same close result.
type poolCloseOperation struct {
	done chan struct{}
	err  error
}

// poolSQLTestOwnership owns every pool lease and active statement started by a
// test. Its cleanup starts Close before waiting for statement termination, so a
// failed assertion cannot leave the automatic cleanup blocked in DB.Close or
// return a live statement-bearing connection through Pool.Put.
type poolSQLTestOwnership struct {
	db      *DB
	scopes  []*connScope
	queries []<-chan error
	close   *poolCloseOperation
}

func ownPoolSQLTest(t *testing.T, db *DB) *poolSQLTestOwnership {
	t.Helper()
	owner := &poolSQLTestOwnership{db: db}
	t.Cleanup(func() { owner.cleanup(t) })
	return owner
}

func (owner *poolSQLTestOwnership) trackScope(scope *connScope) {
	owner.scopes = append(owner.scopes, scope)
}

func (owner *poolSQLTestOwnership) trackSQL(sql unboundedPoolSQL) {
	owner.queries = append(owner.queries, sql.done)
}

func (owner *poolSQLTestOwnership) startClose() *poolCloseOperation {
	if owner.close == nil {
		operation := &poolCloseOperation{done: make(chan struct{})}
		owner.close = operation
		go func() {
			operation.err = owner.db.Close()
			close(operation.done)
		}()
	}
	return owner.close
}

func (owner *poolSQLTestOwnership) cleanup(t *testing.T) {
	t.Helper()
	closeOperation := owner.startClose()
	if !awaitPoolSQLGoroutinesSettled(t, owner.queries) {
		// A live statement must not be returned to the pool. The close goroutine
		// is deliberately allowed to remain blocked on that lease after this
		// bounded failure-path escape; waiting here would hang the test suite.
		return
	}
	for _, scope := range owner.scopes {
		scope.release()
	}
	if err, completed := waitPoolClose(t, "cleanup DB.Close", closeOperation); !completed {
		return
	} else if err != nil {
		t.Errorf("cleanup DB.Close: %v", err)
	}
}

func waitPoolClose(t *testing.T, operation string, closeOperation *poolCloseOperation) (error, bool) {
	t.Helper()
	select {
	case <-closeOperation.done:
		return closeOperation.err, true
	case <-time.After(poolTestTimeout):
		t.Errorf("%s did not complete within %v", operation, poolTestTimeout)
		return nil, false
	}
}

// launchUnboundedPoolSQL starts the statement goroutine and returns immediately,
// WITHOUT waiting for the start barrier. A caller that must be able to wait for
// the goroutine on every failure path records the returned handle first and only
// then calls requireUnboundedPoolSQLStarted, so a start-barrier failure cannot
// strand a running statement on a connection its cleanup path is about to
// return to the pool.
func launchUnboundedPoolSQL(t *testing.T, conn *zs.Conn) unboundedPoolSQL {
	t.Helper()
	started := make(chan struct{})
	var signalOnce sync.Once
	if err := conn.CreateFunction("pool_test_started", &zs.FunctionImpl{
		NArgs: 0,
		Scalar: func(zs.Context, []zs.Value) (zs.Value, error) {
			signalOnce.Do(func() { close(started) })
			return zs.IntegerValue(0), nil
		},
	}); err != nil {
		t.Fatalf("register SQL-start barrier: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- sqlitex.ExecuteTransient(conn, `WITH RECURSIVE forever(n) AS (
			SELECT pool_test_started()
			UNION ALL SELECT n + 1 FROM forever
		) SELECT sum(n) FROM forever`, nil)
		close(done)
	}()
	return unboundedPoolSQL{started: started, done: done}
}

// requireUnboundedPoolSQLStarted blocks until the statement has executed the
// scalar callback, which proves it is genuinely in flight on the connection.
func requireUnboundedPoolSQLStarted(t *testing.T, sql unboundedPoolSQL) {
	t.Helper()
	select {
	case <-sql.started:
	case err := <-sql.done:
		t.Fatalf("unbounded SQL exited before start barrier: %v", err)
	case <-time.After(poolTestTimeout):
		t.Fatalf("SQL did not reach start barrier within %v", poolTestTimeout)
	}
}

// awaitPoolSQLGoroutinesSettled reports whether every statement goroutine has
// returned, so a cleanup path can decide whether returning those connections to
// the pool is safe. It returns true only after every goroutine has returned; a
// single watchdog bounds a failure-path escape, which returns false without
// releasing any tracked scope. This is a causal gate, not a timing heuristic.
//
// It exists because sqlitex.Pool.Put calls Conn.CheckReset and panics with
// "connection returned to pool has active statement" when a connection is
// returned while a statement is still live, and *sqlite.Conn is not
// goroutine-safe. A cleanup path that runs during t.Fatalf's runtime.Goexit
// would otherwise report an SUT regression as a process-level panic or a race
// report — which under -shuffle=on takes down the whole package binary — rather
// than as the assertion the test was written to make.
//
// On a passing run every channel has already been closed by its goroutine, so
// every receive here completes immediately.
func awaitPoolSQLGoroutinesSettled(t *testing.T, queries []<-chan error) bool {
	t.Helper()
	settleTimer := time.NewTimer(poolTestTimeout)
	defer settleTimer.Stop()
	for i, queryDone := range queries {
		settled := false
		for !settled {
			select {
			case _, open := <-queryDone:
				settled = !open
			case <-settleTimer.C:
				t.Errorf(
					"unbounded SQL goroutine %d did not return within %v, so its connection still has an active statement; "+
						"this means shutdown failed to interrupt that lease; leaving the lease outstanding instead of returning it, "+
						"because sqlitex.Pool.Put would panic on a connection with a live statement; "+
						"fix: make Close interrupt every leased connection",
					i, poolTestTimeout,
				)
				return false
			}
		}
	}
	return true
}

// startUnboundedPoolSQL uses a scalar callback as a causal execution barrier.
// The watchdog bounds failures but elapsed time is not used as correctness proof.
func startUnboundedPoolSQL(t *testing.T, conn *zs.Conn) <-chan error {
	t.Helper()
	sql := launchUnboundedPoolSQL(t, conn)
	requireUnboundedPoolSQLStarted(t, sql)
	return sql.done
}

func requirePoolInterrupt(t *testing.T, operation string, done <-chan error) {
	t.Helper()
	err := waitPoolError(t, operation, done)
	if code := zs.ErrCode(err); code != zs.ResultInterrupt {
		t.Fatalf("%s error = %v (%v), want SQLITE_INTERRUPT", operation, err, code)
	}
}

func poolPragmaText(t *testing.T, conn *zs.Conn, pragma poolPragma) string {
	t.Helper()
	var value string
	result := &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		value = stmt.ColumnText(0)
		return nil
	}}
	var err error
	switch pragma {
	case poolPragmaForeignKeys:
		err = sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys", result)
	case poolPragmaBusyTimeout:
		err = sqlitex.ExecuteTransient(conn, "PRAGMA busy_timeout", result)
	case poolPragmaJournalMode:
		err = sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode", result)
	default:
		t.Fatalf("unknown pool pragma %d", pragma)
	}
	if err != nil {
		t.Fatalf("read pool pragma %d: %v", pragma, err)
	}
	return value
}

func createPoolRows(t *testing.T, conn *zs.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE pool_rows (id INTEGER PRIMARY KEY, value TEXT NOT NULL)", nil); err != nil {
		t.Fatalf("create pool_rows: %v", err)
	}
}

func insertPoolRow(t *testing.T, conn *zs.Conn, id int) {
	t.Helper()
	if err := sqlitex.Execute(conn, "INSERT INTO pool_rows (id, value) VALUES (?1, ?2)", &sqlitex.ExecOptions{Args: []any{id, "committed"}}); err != nil {
		t.Fatalf("insert pool row %d: %v", id, err)
	}
}

func countPoolRows(t *testing.T, conn *zs.Conn) int {
	t.Helper()
	var count int
	if err := sqlitex.ExecuteTransient(conn, "SELECT count(*) FROM pool_rows", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		count = stmt.ColumnInt(0)
		return nil
	}}); err != nil {
		t.Fatalf("count pool_rows: %v", err)
	}
	return count
}

func TestPoolFileConnectionsShareCommittedStateAndPragmas(t *testing.T) {
	db := openPoolFileDB(t)
	scopes := make([]*connScope, 0, runtimePoolSize)
	for range runtimePoolSize {
		scopes = append(scopes, takePoolScope(t, db))
	}
	defer func() {
		for _, scope := range scopes {
			scope.release()
		}
	}()

	connections := make([]*zs.Conn, 0, runtimePoolSize)
	seen := map[*zs.Conn]struct{}{}
	for _, scope := range scopes {
		if _, exists := seen[scope.conn]; exists {
			t.Fatalf("pool returned duplicate simultaneous connection %p", scope.conn)
		}
		seen[scope.conn] = struct{}{}
		connections = append(connections, scope.conn)
	}
	if len(connections) != runtimePoolSize {
		t.Fatalf("connection count = %d, want %d", len(connections), runtimePoolSize)
	}

	createPoolRows(t, scopes[0].conn)
	insertPoolRow(t, scopes[0].conn, 1)
	for i, conn := range connections {
		if got := countPoolRows(t, conn); got != 1 {
			t.Errorf("connection %d row count = %d, want 1", i, got)
		}
		if got := poolPragmaText(t, conn, poolPragmaForeignKeys); got != "1" {
			t.Errorf("connection %d foreign_keys = %q, want 1", i, got)
		}
		if got := poolPragmaText(t, conn, poolPragmaBusyTimeout); got != "5000" {
			t.Errorf("connection %d busy_timeout = %q, want 5000", i, got)
		}
		if got := poolPragmaText(t, conn, poolPragmaJournalMode); got != "wal" {
			t.Errorf("connection %d journal_mode = %q, want wal", i, got)
		}
	}
}

// TestPoolSizesMatchRatifiedTargets pins the two ratified pool sizes and the
// resolver that selects between them. providence-j8i.1.2.6 ratified a file pool
// of 4 and, in its P0 handoff decision, a unique shared-memory pool of 1 once
// the reserved lease was removed. These constants are also load-bearing test
// infrastructure: holdRemainingApplyLeases in transaction_ownership_test.go
// computes free capacity as runtimePoolSize minus the leases already held, so a
// divergence between the constant and the pool would silently stop pinning the
// instrumented connection instead of failing.
func TestPoolSizesMatchRatifiedTargets(t *testing.T) {
	if runtimePoolSize != 4 {
		t.Errorf("runtimePoolSize = %d, want 4 (ratified file-pool size, providence-j8i.1.2.6)", runtimePoolSize)
	}
	if memoryPoolSize != 1 {
		t.Errorf("memoryPoolSize = %d, want 1 (ratified unique shared-memory pool size, providence-j8i.1.2.6 P0 handoff decision)", memoryPoolSize)
	}
	if uri, poolSize, isMemory := resolvePoolTarget(":memory:"); !isMemory || poolSize != memoryPoolSize {
		t.Errorf("resolvePoolTarget(\":memory:\") = (%q, poolSize %d, isMemory %t), want (a unique shared-cache URI, %d, true)", uri, poolSize, isMemory, memoryPoolSize)
	}
	if uri, poolSize, isMemory := resolvePoolTarget("/tmp/pool-size-target.db"); isMemory || poolSize != runtimePoolSize || uri != "/tmp/pool-size-target.db" {
		t.Errorf("resolvePoolTarget(file path) = (%q, poolSize %d, isMemory %t), want (the path unchanged, %d, false)", uri, poolSize, isMemory, runtimePoolSize)
	}
}

// poolCapacityProbe bounds the N+1th lease attempt in
// TestPoolCapacityIsExactlyTheRatifiedSize. It is an observation ceiling, not a
// correctness threshold — see that test's determinism argument.
const poolCapacityProbe = 250 * time.Millisecond

// TestPoolCapacityIsExactlyTheRatifiedSize closes the upper bound on both pools
// by set cardinality: it holds every lease the pool is supposed to have, proves
// they are N genuinely distinct simultaneous connections, and then proves the
// N+1th lease cannot be obtained. Distinctness alone only proves capacity >= N;
// the failed N+1th is what proves capacity <= N.
//
// Why the N+1th probe is not a 50/50 select race. sqlitex v1.4.2 Pool.Take is a
// single three-way select over p.free, ctx.Done() and p.closed, with no early
// context check and no priority between cases. Go picks uniformly at random
// only when more than one case is ready, which is exactly the defect in
// cancel-first probes: they hand Take a context that is ALREADY done, so a
// pool with spare capacity has two ready cases and reports cancellation half
// the time. Here the probe context is constructed immediately before the call
// and its deadline has not elapsed when the select is polled, so:
//
//   - If the pool held more than N connections, p.free would be the only ready
//     case and select MUST take it. The probe returns a connection and this
//     test fails. Detection of an oversized pool does not depend on timing.
//   - If the pool holds exactly N, no case is ready, Take blocks, and the
//     deadline is merely how "blocked" becomes observable.
func TestPoolCapacityIsExactlyTheRatifiedSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) *DB
		size int
	}{
		{name: "file", open: openPoolFileDB, size: runtimePoolSize},
		{name: "memory", open: openPoolMemoryDB, size: memoryPoolSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.open(t)
			acquireCtx, acquireCancel := context.WithTimeout(context.Background(), poolTestTimeout)
			held := make([]*connScope, 0, tc.size)
			// Deferred order is LIFO: every lease is released before the
			// acquisition context is cancelled, and the DB Close registered by
			// the opener runs after both.
			defer acquireCancel()
			defer func() {
				for _, scope := range held {
					scope.release()
				}
			}()

			distinct := make(map[*zs.Conn]struct{}, tc.size)
			for i := range tc.size {
				scope, err := db.bindScope(acquireCtx, projectionTargetLive)
				if err != nil {
					t.Fatalf("%s pool did not deliver lease %d of %d within %v: %v; capacity is below the ratified size %d", tc.name, i+1, tc.size, poolTestTimeout, err, tc.size)
				}
				held = append(held, scope)
				if _, duplicate := distinct[scope.conn]; duplicate {
					t.Fatalf("%s pool handed out connection %p twice simultaneously at lease %d", tc.name, scope.conn, i+1)
				}
				distinct[scope.conn] = struct{}{}
			}
			if len(distinct) != tc.size {
				t.Fatalf("%s pool delivered %d distinct simultaneous connections, want %d", tc.name, len(distinct), tc.size)
			}

			probeCtx, probeCancel := context.WithTimeout(context.Background(), poolCapacityProbe)
			defer probeCancel()
			extra, err := db.bindScope(probeCtx, projectionTargetLive)
			if extra != nil {
				extra.release()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s pool delivered lease %d while all %d ratified leases were held (error %v); capacity exceeds %d", tc.name, tc.size+1, tc.size, err, tc.size)
			}
		})
	}
}

func TestPoolExhaustionHonorsCancellationAndReturn(t *testing.T) {
	db := openPoolFileDB(t)
	held := make([]*connScope, 0, runtimePoolSize)
	for range runtimePoolSize {
		held = append(held, takePoolScope(t, db))
	}
	defer func() {
		for _, scope := range held {
			scope.release()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	waiterStarted := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		close(waiterStarted)
		scope, err := db.bindScope(ctx, projectionTargetLive)
		if scope != nil {
			scope.release()
		}
		waiterDone <- err
	}()
	<-waiterStarted
	cancel()
	if err := waitPoolError(t, "canceled exhausted file bind", waiterDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exhausted file bind error = %v, want context.Canceled", err)
	}

	returned := held[len(held)-1]
	returned.release()
	held = held[:len(held)-1]
	next := takePoolScope(t, db)
	next.release()
}

func TestConnScopeLeasedOwnershipIsIdempotentAndPreservesTarget(t *testing.T) {
	db := openPoolFileDB(t)
	live, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		t.Fatalf("bind live scope: %v", err)
	}
	if live.projectionTarget != projectionTargetLive {
		t.Fatalf("freshly leased scope target = %s, want %s", live.projectionTarget.label(), projectionTargetLive.label())
	}
	if live.conn == nil {
		t.Fatal("leased scope carries no connection")
	}
	live.release()

	// The projection target is chosen at the bind site, so a caller can obtain a
	// shadow-targeted scope through the public binder rather than by mutating an
	// owned scope's field. Release must not disturb the chosen target.
	scope, err := db.bindScope(context.Background(), projectionTargetShadow)
	if err != nil {
		t.Fatalf("bind shadow scope: %v", err)
	}
	if scope.conn == nil {
		t.Fatal("leased shadow scope carries no connection")
	}

	const releasers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(releasers)
	for range releasers {
		go func() {
			defer wg.Done()
			<-start
			scope.release()
		}()
	}
	close(start)
	wg.Wait()

	if scope.projectionTarget != projectionTargetShadow {
		t.Fatalf("released scope target = %s, want %s preserved across release", scope.projectionTarget.label(), projectionTargetShadow.label())
	}

	// The runtime pool reserves nothing. Exactly-once release makes every pool
	// lease available again without duplicate connection IDs.
	held := make([]*connScope, 0, runtimePoolSize)
	seen := make(map[*zs.Conn]struct{}, runtimePoolSize)
	for range runtimePoolSize {
		next := takePoolScope(t, db)
		if _, duplicate := seen[next.conn]; duplicate {
			t.Fatalf("idempotent release returned duplicate simultaneous connection %p", next.conn)
		}
		seen[next.conn] = struct{}{}
		held = append(held, next)
	}
	for _, next := range held {
		next.release()
	}
}

func TestConnScopeActivationBorrowRetainsPoolLeaseUntilOwnerPut(t *testing.T) {
	poolURI, _, _ := resolvePoolTarget(":memory:")
	pool, err := sqlitex.NewPool(poolURI, sqlitex.PoolOptions{
		Flags:    zs.OpenReadWrite | zs.OpenCreate | zs.OpenURI,
		PoolSize: 1,
	})
	if err != nil {
		t.Fatalf("open size-1 activation pool: %v", err)
	}
	activationConn, err := pool.Take(context.Background())
	if err != nil {
		_ = pool.Close()
		t.Fatalf("take activation owner connection: %v", err)
	}
	activationOwned := true
	var waiterConn *zs.Conn
	defer func() {
		if waiterConn != nil {
			pool.Put(waiterConn)
		}
		if activationOwned {
			pool.Put(activationConn)
		}
		if err := pool.Close(); err != nil {
			t.Errorf("close size-1 activation pool: %v", err)
		}
	}()

	scope := borrowConnScope(activationConn, projectionTargetLive)
	if scope.conn != activationConn || scope.projectionTarget != projectionTargetLive {
		t.Fatal("borrowed activation scope did not preserve connection and live target")
	}

	blockedBase, blockedCancel := context.WithCancel(context.Background())
	blockedCtx := &observedDoneContext{Context: blockedBase, observed: make(chan struct{})}
	blockedDone := make(chan poolTakeResult, 1)
	go func() {
		conn, err := pool.Take(blockedCtx)
		blockedDone <- poolTakeResult{conn: conn, err: err}
	}()
	waitPoolSignal(t, "pre-Put activation waiter Pool.Take Done", blockedCtx.observed)

	const releasers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(releasers)
	for range releasers {
		go func() {
			defer wg.Done()
			<-start
			scope.release()
		}()
	}
	close(start)
	wg.Wait()
	blockedCancel()
	blockedResult := waitPoolTakeResult(t, "pre-Put canceled activation waiter", blockedDone)
	if blockedResult.conn != nil || !errors.Is(blockedResult.err, context.Canceled) {
		t.Fatalf("pre-Put activation waiter = (%p, %v), want (nil, context.Canceled)", blockedResult.conn, blockedResult.err)
	}

	type queuedWaiter struct {
		cancel context.CancelFunc
		done   chan poolTakeResult
	}
	waiters := make([]queuedWaiter, 2)
	for i := range waiters {
		base, cancel := context.WithTimeout(context.Background(), poolTestTimeout)
		ctx := &observedDoneContext{Context: base, observed: make(chan struct{})}
		done := make(chan poolTakeResult, 1)
		waiters[i] = queuedWaiter{cancel: cancel, done: done}
		go func() {
			conn, err := pool.Take(ctx)
			done <- poolTakeResult{conn: conn, err: err}
		}()
		waitPoolSignal(t, "queued activation waiter Pool.Take Done", ctx.observed)
	}
	defer func() {
		for _, waiter := range waiters {
			waiter.cancel()
		}
	}()

	pool.Put(activationConn)
	activationOwned = false
	var winner int
	var winnerResult poolTakeResult
	select {
	case winnerResult = <-waiters[0].done:
		winner = 0
	case winnerResult = <-waiters[1].done:
		winner = 1
	case <-time.After(poolTestTimeout):
		t.Fatalf("no queued activation waiter progressed within %v", poolTestTimeout)
	}
	waiterConn = winnerResult.conn
	if winnerResult.err != nil || winnerResult.conn != activationConn {
		t.Fatalf("first post-Put waiter = (%p, %v), want activation connection %p", winnerResult.conn, winnerResult.err, activationConn)
	}

	loser := 1 - winner
	waiters[loser].cancel()
	loserResult := waitPoolTakeResult(t, "second queued activation waiter cancellation", waiters[loser].done)
	if loserResult.conn != nil || !errors.Is(loserResult.err, context.Canceled) {
		t.Fatalf("second post-Put waiter = (%p, %v), want (nil, context.Canceled)", loserResult.conn, loserResult.err)
	}
	pool.Put(waiterConn)
	waiterConn = nil
}

// TestActivationPathsBorrowExactlyOneScope proves every startup path threads a
// SINGLE explicitly borrowed connScope — not a bound-DB adapter and not a
// second borrow — through its schema/seed/integrity/replay/pragma helpers.
func TestActivationPathsBorrowExactlyOneScope(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "db.go", nil, 0)
	if err != nil {
		t.Fatalf("parse db.go activation scope contract: %v", err)
	}
	want := map[string]struct {
		assigned string
		conn     string
	}{
		"openInMemory":              {assigned: "activation", conn: "activationConn"},
		"openFileBacked":            {assigned: "activation", conn: "activationConn"},
		"preflightExistingReadOnly": {assigned: "preflight", conn: "conn"},
		"preflightActivationClone":  {assigned: "activation", conn: "clone"},
	}
	found := make(map[string]int, len(want))
	borrows := make(map[string]int, len(want))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		expected, inspect := want[fn.Name.Name]
		if !inspect {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok {
				if name, isIdent := call.Fun.(*ast.Ident); isIdent && name.Name == "borrowConnScope" {
					borrows[fn.Name.Name]++
				}
				return true
			}
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
				return true
			}
			assigned, ok := assignment.Lhs[0].(*ast.Ident)
			if !ok || assigned.Name != expected.assigned {
				return true
			}
			borrow, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || len(borrow.Args) != 2 {
				return true
			}
			borrowName, ok := borrow.Fun.(*ast.Ident)
			conn, connOK := borrow.Args[0].(*ast.Ident)
			target, targetOK := borrow.Args[1].(*ast.Ident)
			if ok && connOK && targetOK && borrowName.Name == "borrowConnScope" && conn.Name == expected.conn && target.Name == "projectionTargetLive" {
				found[fn.Name.Name]++
			}
			return true
		})
	}
	for function := range want {
		if found[function] != 1 {
			t.Errorf("%s borrowed activation scope assignments = %d, want exactly 1", function, found[function])
		}
		if borrows[function] != 1 {
			t.Errorf("%s borrowConnScope calls = %d, want exactly 1 threaded activation scope", function, borrows[function])
		}
	}
}

// packageGoFiles parses every Go file in this one directory, production and
// test, keyed by file name. It is a flat declaration scan by construction: no
// import graph, no alias resolution, no def-use or call-graph analysis.
//
// go/parser.ParseDir has been deprecated since Go 1.22 and would also introduce
// a package-keyed outer loop this guard never wants, since it only ever intends
// to scan the single directory it lives in.
func packageGoFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory for legacy-seam contract: %v", err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s for legacy-seam contract: %v", name, err)
		}
		files[name] = parsed
	}
	if len(files) == 0 {
		t.Fatal("legacy-seam contract scanned no Go files in the package directory")
	}
	return fset, files
}

// receiverTypeName returns the undecorated receiver type name of a method
// declaration, or "" when the declaration is a package-level function.
func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) != 1 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch typed := expr.(type) {
	case *ast.IndexExpr: // generic receiver T[P]
		expr = typed.X
	case *ast.IndexListExpr: // generic receiver T[P, Q]
		expr = typed.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}

// TestPackageRetainsNoLegacyConnectionSeamAndDBOwnership proves, over every Go
// file in this package directory (production and test), that the P1/P2 legacy
// single-connection seam is structurally gone and DB ownership has the intended
// shape. It enforces three rules:
//
//  1. DB declares exactly the fields {pool, close} — it can neither retain a
//     removed legacy field nor regrow any new one.
//  2. The deleted bridge names {boundDB, bindOperationDB, bindConn} do not
//     exist in ANY form. A package-level func is rejected as well as a method
//     on any receiver, because the free-function form is the easiest
//     reintroduction of the scope-to-bound-DB bridge.
//  3. The ordinary Go names {Conn, Lock, Unlock} are rejected ONLY on a DB
//     receiver. The contract is that *DB never re-exposes a raw connection or a
//     serialization handle; the same names on any other type (a synchronization
//     implementation, an accessor on another struct) are unrelated and must not
//     be misdiagnosed as the legacy seam.
//
// The guard deliberately does not ban synchronization primitives package-wide;
// their choice is governed by the project policy rather than this legacy-seam
// contract.
func TestPackageRetainsNoLegacyConnectionSeamAndDBOwnership(t *testing.T) {
	forbiddenDBFields := map[string]struct{}{
		"mu": {}, "conn": {}, "legacyCancel": {}, "projectionTarget": {},
	}
	allowedDBFields := map[string]struct{}{"pool": {}, "close": {}}
	// Rule 2: deleted bridge names, forbidden regardless of receiver.
	deletedSeamFuncs := map[string]struct{}{
		"boundDB": {}, "bindOperationDB": {}, "bindConn": {},
	}
	// Rule 3: general Go vocabulary, forbidden only on a DB receiver.
	forbiddenDBMethods := map[string]struct{}{
		"Conn": {}, "Lock": {}, "Unlock": {},
	}
	_, files := packageGoFiles(t)
	dbStructsSeen := 0
	for name, file := range files {
		for _, decl := range file.Decls {
			switch typed := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || typeSpec.Name.Name != "DB" {
						continue
					}
					structType, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						continue
					}
					dbStructsSeen++
					seenDBFields := make(map[string]struct{}, len(structType.Fields.List))
					for _, field := range structType.Fields.List {
						if len(field.Names) == 0 {
							t.Errorf("%s: DB embeds an anonymous field; DB owns only the named pool and close lifecycle state", name)
							continue
						}
						for _, fieldName := range field.Names {
							seenDBFields[fieldName.Name] = struct{}{}
							if _, forbidden := forbiddenDBFields[fieldName.Name]; forbidden {
								t.Errorf("%s: DB retains removed legacy field %q", name, fieldName.Name)
							}
							if _, allowed := allowedDBFields[fieldName.Name]; !allowed {
								t.Errorf("%s: DB carries unexpected field %q; DB owns only the pool and close lifecycle state", name, fieldName.Name)
							}
						}
					}
					for required := range allowedDBFields {
						if _, present := seenDBFields[required]; !present {
							t.Errorf("%s: DB is missing required field %q; DB must own the pool and close lifecycle state", name, required)
						}
					}
				}
			case *ast.FuncDecl:
				receiver := receiverTypeName(typed.Recv)
				if _, deleted := deletedSeamFuncs[typed.Name.Name]; deleted {
					if receiver == "" {
						t.Errorf("%s: package-level func %s reintroduces the removed scope-to-bound-DB bridge", name, typed.Name.Name)
					} else {
						t.Errorf("%s: (%s).%s reintroduces the removed scope-to-bound-DB bridge", name, receiver, typed.Name.Name)
					}
					continue
				}
				if _, forbidden := forbiddenDBMethods[typed.Name.Name]; !forbidden || receiver != "DB" {
					continue
				}
				t.Errorf("%s: (*DB).%s is a removed legacy connection-seam method; DB must not re-expose a raw connection or a serialization handle", name, typed.Name.Name)
			}
		}
	}
	if dbStructsSeen != 1 {
		t.Fatalf("DB struct declarations = %d, want exactly 1", dbStructsSeen)
	}
}

func TestPoolCallerCancellationInterruptsActiveSQL(t *testing.T) {
	db, err := Open(t.TempDir()+"/caller-cancel.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	owner := ownPoolSQLTest(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		t.Fatalf("bindScope: %v", err)
	}
	owner.trackScope(scope)
	sql := launchUnboundedPoolSQL(t, scope.conn)
	owner.trackSQL(sql)
	requireUnboundedPoolSQLStarted(t, sql)
	cancel()
	requirePoolInterrupt(t, "caller-canceled active SQL", sql.done)
}

func TestPoolCloseInterruptsAndDrainsActiveScopedSQL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory bool
	}{
		{name: "file"},
		{name: "memory", memory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := t.TempDir() + "/close-scoped.db"
			if tc.memory {
				dbPath = ":memory:"
			}
			db, err := Open(dbPath, nil)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			owner := ownPoolSQLTest(t, db)
			scope := takePoolScope(t, db)
			owner.trackScope(scope)
			sql := launchUnboundedPoolSQL(t, scope.conn)
			owner.trackSQL(sql)
			requireUnboundedPoolSQLStarted(t, sql)

			closeOperation := owner.startClose()
			requirePoolInterrupt(t, "Close-interrupted active "+tc.name+" SQL", sql.done)
			requirePoolCloseStillDraining(t, tc.name, 1, closeOperation)
			scope.release()
			if err, completed := waitPoolClose(t, "Close after releasing "+tc.name+" scope", closeOperation); !completed {
				return
			} else if err != nil {
				t.Fatalf("Close after releasing %s scope: %v", tc.name, err)
			}
		})
	}
}

// TestPoolCloseInterruptsAndDrainsEveryOutstandingLease proves Close routes
// shutdown solely through the pool across the WHOLE pool: every simultaneously
// leased connection running SQL is interrupted, and Close does not return until
// the last outstanding scope has been released.
func TestPoolCloseInterruptsAndDrainsEveryOutstandingLease(t *testing.T) {
	db, err := Open(t.TempDir()+"/drain-close.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	owner := ownPoolSQLTest(t, db)
	held := make([]*connScope, 0, runtimePoolSize)
	queries := make([]<-chan error, 0, runtimePoolSize)
	for range runtimePoolSize {
		scope := takePoolScope(t, db)
		held = append(held, scope)
		owner.trackScope(scope)
		// Record the handle before the start barrier can fail, so a barrier
		// failure still leaves this goroutine waitable by the cleanup above.
		sql := launchUnboundedPoolSQL(t, scope.conn)
		owner.trackSQL(sql)
		queries = append(queries, sql.done)
		requireUnboundedPoolSQLStarted(t, sql)
	}

	closeOperation := owner.startClose()
	for i, queryDone := range queries {
		requirePoolInterrupt(t, fmt.Sprintf("Close-interrupted active SQL on lease %d", i), queryDone)
	}
	// Release every lease but the last: Close must still be draining.
	for i := range len(held) - 1 {
		held[i].release()
		requirePoolCloseStillDraining(t, "file", len(held)-1-i, closeOperation)
	}
	held[len(held)-1].release()
	if err, completed := waitPoolClose(t, "Close after every lease was returned", closeOperation); !completed {
		return
	} else if err != nil {
		t.Fatalf("Close after every lease was returned: %v", err)
	}
}

func TestPoolMemoryContendedWaitersCancelAndProgress(t *testing.T) {
	db, err := Open(":memory:", nil)
	if err != nil {
		t.Fatalf("Open memory DB: %v", err)
	}
	first := takePoolScope(t, db)
	defer first.release()

	canceledBase, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceledCtx := &observedDoneContext{Context: canceledBase, observed: make(chan struct{})}
	canceledDone := make(chan poolBindResult, 1)
	go func() {
		scope, err := db.bindScope(canceledCtx, projectionTargetLive)
		canceledDone <- poolBindResult{scope: scope, err: err}
	}()
	waitPoolSignal(t, "contended memory canceled waiter Pool.Take Done", canceledCtx.observed)
	cancel()
	var canceledResult poolBindResult
	select {
	case canceledResult = <-canceledDone:
	case <-time.After(poolTestTimeout):
		t.Fatalf("contended memory canceled waiter did not complete within %v", poolTestTimeout)
	}
	if canceledResult.scope != nil {
		canceledResult.scope.release()
		t.Fatal("contended memory canceled waiter acquired a scope")
	}
	if !errors.Is(canceledResult.err, context.Canceled) {
		t.Fatalf("contended memory canceled waiter error = %v, want context.Canceled", canceledResult.err)
	}

	progressBase, progressCancel := context.WithTimeout(context.Background(), poolTestTimeout)
	defer progressCancel()
	progressCtx := &observedDoneContext{Context: progressBase, observed: make(chan struct{})}
	progressDone := make(chan poolBindResult, 1)
	go func() {
		scope, err := db.bindScope(progressCtx, projectionTargetLive)
		progressDone <- poolBindResult{scope: scope, err: err}
	}()
	waitPoolSignal(t, "contended memory progressing waiter Pool.Take Done", progressCtx.observed)
	first.release()
	var progressResult poolBindResult
	select {
	case progressResult = <-progressDone:
	case <-time.After(poolTestTimeout):
		t.Fatalf("contended memory progressing waiter did not complete within %v", poolTestTimeout)
	}
	if progressResult.err != nil {
		if progressResult.scope != nil {
			progressResult.scope.release()
		}
		t.Fatalf("contended memory progressing waiter: %v", progressResult.err)
	}
	if progressResult.scope == nil {
		t.Fatal("contended memory progressing waiter returned nil scope")
	}
	progressResult.scope.release()

	probeCtx, probeCancel := context.WithTimeout(context.Background(), poolTestTimeout)
	probe, err := db.bindScope(probeCtx, projectionTargetLive)
	if err != nil {
		probeCancel()
		t.Fatalf("memory capacity probe bind: %v", err)
	}
	if probe == nil {
		probeCancel()
		t.Fatal("memory capacity probe returned nil scope")
	}
	probe.release()
	probeCancel()

	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()
	if err := waitPoolError(t, "memory Close after contended waiters", closeDone); err != nil {
		t.Fatalf("memory Close after contended waiters: %v", err)
	}
}

func TestCloseResultPublishesOneError(t *testing.T) {
	sentinel := errors.New("sentinel close failure")
	var result closeResult
	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	calls := 0
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			results <- result.do(func() error {
				calls++
				return sentinel
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	if calls != 1 {
		t.Fatalf("close function calls = %d, want 1", calls)
	}
	for err := range results {
		if !errors.Is(err, sentinel) {
			t.Errorf("concurrent close result = %v, want sentinel cause", err)
		}
	}
	if err := result.do(func() error {
		t.Fatal("close function executed on later call")
		return nil
	}); !errors.Is(err, sentinel) {
		t.Fatalf("later close result = %v, want sentinel cause", err)
	}
}

func TestPoolConcurrentCloseReturnsSameResult(t *testing.T) {
	db, err := Open(t.TempDir()+"/concurrent-close.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			results <- db.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent Close: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Errorf("repeated Close: %v", err)
	}
}

func TestPoolMemoryScopedBindingPreservesState(t *testing.T) {
	db := openPoolMemoryDB(t)
	writeScope := takePoolScope(t, db)
	defer writeScope.release()
	createPoolRows(t, writeScope.conn)
	insertPoolRow(t, writeScope.conn, 1)
	writeScope.release()

	readScope := takePoolScope(t, db)
	defer readScope.release()
	if got := countPoolRows(t, readScope.conn); got != 1 {
		t.Fatalf("in-memory row count = %d, want 1", got)
	}
	if got := poolPragmaText(t, readScope.conn, poolPragmaForeignKeys); got != "1" {
		t.Errorf("in-memory foreign_keys = %q, want 1", got)
	}
	if got := poolPragmaText(t, readScope.conn, poolPragmaBusyTimeout); got != "5000" {
		t.Errorf("in-memory busy_timeout = %q, want 5000", got)
	}
}

func TestPoolMemoryOpenCallsAreIsolated(t *testing.T) {
	db1 := openPoolMemoryDB(t)
	db2 := openPoolMemoryDB(t)
	scope1 := takePoolScope(t, db1)
	defer scope1.release()
	createPoolRows(t, scope1.conn)
	insertPoolRow(t, scope1.conn, 1)
	scope1.release()

	scope2 := takePoolScope(t, db2)
	defer scope2.release()
	var count int
	err := sqlitex.ExecuteTransient(scope2.conn, "SELECT count(*) FROM pool_rows", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		count = stmt.ColumnInt(0)
		return nil
	}})
	if err == nil || count != 0 {
		t.Fatalf("second :memory: DB query = (count %d, error %v), want isolated missing table", count, err)
	}
}
