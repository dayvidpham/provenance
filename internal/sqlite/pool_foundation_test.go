package sqlite

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"

	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const poolTestTimeout = 5 * time.Second

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
	scope, err := db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bindConn: %v", err)
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

// startUnboundedPoolSQL uses a scalar callback as a causal execution barrier.
// The watchdog bounds failures but elapsed time is not used as correctness proof.
func startUnboundedPoolSQL(t *testing.T, conn *zs.Conn) <-chan error {
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
	}()
	select {
	case <-started:
		return done
	case err := <-done:
		t.Fatalf("unbounded SQL exited before start barrier: %v", err)
		return nil
	case <-time.After(poolTestTimeout):
		t.Fatalf("SQL did not reach start barrier within %v", poolTestTimeout)
		return nil
	}
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
	db.Lock()
	legacy := db.Conn()
	db.Unlock()
	scopes := make([]*connScope, 0, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
		scopes = append(scopes, takePoolScope(t, db))
	}
	defer func() {
		for _, scope := range scopes {
			scope.release()
		}
	}()

	connections := []*zs.Conn{legacy}
	seen := map[*zs.Conn]struct{}{legacy: {}}
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

func TestPoolExhaustionHonorsCancellationAndReturn(t *testing.T) {
	db := openPoolFileDB(t)
	held := make([]*connScope, 0, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
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
		scope, err := db.bindConn(ctx)
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
	scope, err := db.bindScope(context.Background(), projectionTargetShadow)
	if err != nil {
		t.Fatalf("bind shadow scope: %v", err)
	}
	if scope.projectionTarget != projectionTargetShadow {
		t.Fatalf("scope target = %s, want %s", scope.projectionTarget.label(), projectionTargetShadow.label())
	}
	bound := scope.boundDB()
	if bound.conn != scope.conn {
		t.Fatal("bound DB did not preserve scoped connection")
	}
	if bound.projectionTarget != projectionTargetShadow {
		t.Fatalf("bound DB target = %s, want %s", bound.projectionTarget.label(), projectionTargetShadow.label())
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

	// The runtime pool has one reserved legacy lease. Exactly-once release makes
	// all remaining runtime leases available without duplicate connection IDs.
	held := make([]*connScope, 0, runtimePoolSize-1)
	seen := make(map[*zs.Conn]struct{}, runtimePoolSize-1)
	for range runtimePoolSize - 1 {
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
	bound := scope.boundDB()
	if bound.conn != activationConn || bound.projectionTarget != projectionTargetLive {
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

func TestActivationPathsUseBorrowedScopeBridge(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "db.go", nil, 0)
	if err != nil {
		t.Fatalf("parse db.go activation bridge contract: %v", err)
	}
	want := map[string]struct {
		assigned string
		conn     string
	}{
		"openInMemory":              {assigned: "activationDB", conn: "activationConn"},
		"openFileBacked":            {assigned: "activationDB", conn: "activationConn"},
		"preflightExistingReadOnly": {assigned: "db", conn: "conn"},
		"preflightActivationClone":  {assigned: "db", conn: "clone"},
	}
	found := make(map[string]int, len(want))
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
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
				return true
			}
			assigned, ok := assignment.Lhs[0].(*ast.Ident)
			if !ok || assigned.Name != expected.assigned {
				return true
			}
			selector, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || len(selector.Args) != 0 {
				return true
			}
			boundDB, ok := selector.Fun.(*ast.SelectorExpr)
			if !ok || boundDB.Sel.Name != "boundDB" {
				return true
			}
			borrow, ok := boundDB.X.(*ast.CallExpr)
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
			t.Errorf("%s borrowed scope bridge assignments = %d, want exactly 1", function, found[function])
		}
	}
}

func TestPoolCallerCancellationInterruptsActiveSQL(t *testing.T) {
	db := openPoolFileDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	scope, err := db.bindConn(ctx)
	if err != nil {
		t.Fatalf("bindConn: %v", err)
	}
	defer scope.release()
	queryDone := startUnboundedPoolSQL(t, scope.conn)
	cancel()
	requirePoolInterrupt(t, "caller-canceled active SQL", queryDone)
}

func TestPoolCloseInterruptsAndDrainsActiveScopedSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) *DB
	}{
		{name: "file", open: openPoolFileDB},
		{name: "memory", open: openPoolMemoryDB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.open(t)
			scope := takePoolScope(t, db)
			defer scope.release()
			queryDone := startUnboundedPoolSQL(t, scope.conn)

			closeDone := make(chan error, 1)
			go func() { closeDone <- db.Close() }()
			requirePoolInterrupt(t, "Close-interrupted active "+tc.name+" SQL", queryDone)
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned before interrupted %s scope was released: %v", tc.name, err)
			default:
			}
			scope.release()
			if err := waitPoolError(t, "Close after releasing "+tc.name+" scope", closeDone); err != nil {
				t.Fatalf("Close after releasing %s scope: %v", tc.name, err)
			}
		})
	}
}

func TestPoolCloseInterruptsLegacySQLBeforeMutexWait(t *testing.T) {
	db, err := Open(t.TempDir()+"/legacy-close.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	locked := true
	db.Lock()
	defer func() {
		if locked {
			db.Unlock()
		}
		_ = db.Close()
	}()
	queryDone := startUnboundedPoolSQL(t, db.Conn())

	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()
	requirePoolInterrupt(t, "Close-interrupted active legacy SQL", queryDone)
	db.Unlock()
	locked = false
	if err := waitPoolError(t, "Close after legacy SQL unlock", closeDone); err != nil {
		t.Fatalf("Close after legacy SQL unlock: %v", err)
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
		scope, err := db.bindConn(canceledCtx)
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
		scope, err := db.bindConn(progressCtx)
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
	probe, err := db.bindConn(probeCtx)
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
