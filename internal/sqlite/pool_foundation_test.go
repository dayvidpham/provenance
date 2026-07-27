package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const poolTestTimeout = 5 * time.Second

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
	ctx, cancel := context.WithTimeout(context.Background(), poolTestTimeout)
	t.Cleanup(cancel)
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		t.Fatalf("bindScope: %v", err)
	}
	return scope
}

func TestPoolSizesMatchRatifiedTargets(t *testing.T) {
	if runtimePoolSize != 4 {
		t.Errorf("runtimePoolSize = %d, want 4", runtimePoolSize)
	}
	if memoryPoolSize != 1 {
		t.Errorf("memoryPoolSize = %d, want 1", memoryPoolSize)
	}
	if uri, size, memory := resolvePoolTarget(":memory:"); !memory || size != memoryPoolSize || uri == "" {
		t.Errorf("resolvePoolTarget(:memory:) = (%q, %d, %t), want configured unique memory DSN / %d / true", uri, size, memory, memoryPoolSize)
	}
	if uri, size, memory := resolvePoolTarget(t.TempDir() + "/path with spaces.db"); memory || size != runtimePoolSize || uri == "" {
		t.Errorf("resolvePoolTarget(file) = (%q, %d, %t), want configured file DSN / %d / false", uri, size, memory, runtimePoolSize)
	}
}

func TestFilePoolConnectionsShareStateAndRuntimePragmas(t *testing.T) {
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
	if _, err := scopes[0].conn.ExecContext(scopes[0].ctx, "CREATE TABLE pool_rows (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create shared table: %v", err)
	}
	if _, err := scopes[0].conn.ExecContext(scopes[0].ctx, "INSERT INTO pool_rows (id, value) VALUES (?1, ?2)", 1, "committed"); err != nil {
		t.Fatalf("insert shared row: %v", err)
	}
	for i, scope := range scopes {
		var count, foreignKeys, busyTimeout, synchronous int
		var journalMode string
		if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM pool_rows").Scan(&count); err != nil {
			t.Fatalf("connection %d read shared row: %v", i, err)
		}
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("connection %d foreign_keys: %v", i, err)
		}
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("connection %d busy_timeout: %v", i, err)
		}
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("connection %d journal_mode: %v", i, err)
		}
		if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatalf("connection %d synchronous: %v", i, err)
		}
		if count != 1 || foreignKeys != 1 || busyTimeout != busyTimeoutMS || journalMode != "wal" || synchronous != 1 {
			t.Errorf("connection %d state=(rows=%d fk=%d busy=%d journal=%q sync=%d), want (1,1,%d,wal,1)", i, count, foreignKeys, busyTimeout, journalMode, synchronous, busyTimeoutMS)
		}
	}
}

func TestPoolCapacityHonorsContextCancellation(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if scope, err := db.bindScope(ctx, projectionTargetLive); scope != nil || !errors.Is(err, context.DeadlineExceeded) {
		if scope != nil {
			scope.release()
		}
		t.Fatalf("exhausted bind = (%v, %v), want (nil, deadline exceeded)", scope, err)
	}
	held[len(held)-1].release()
	held = held[:len(held)-1]
	returned := takePoolScope(t, db)
	returned.release()
}

func TestConnScopeReleaseIsIdempotentAndPreservesTarget(t *testing.T) {
	db := openPoolFileDB(t)
	scope, err := db.bindScope(context.Background(), projectionTargetShadow)
	if err != nil {
		t.Fatalf("bind shadow scope: %v", err)
	}
	const releasers = 8
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(releasers)
	for range releasers {
		go func() {
			defer wait.Done()
			<-start
			scope.release()
		}()
	}
	close(start)
	wait.Wait()
	if scope.projectionTarget != projectionTargetShadow {
		t.Fatalf("released target = %s, want %s", scope.projectionTarget.label(), projectionTargetShadow.label())
	}
	for range runtimePoolSize {
		takePoolScope(t, db).release()
	}
}

func TestPinnedConnectionKeepsTempStateLocal(t *testing.T) {
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
	if _, err := scopes[0].conn.ExecContext(scopes[0].ctx, "CREATE TEMP TABLE scope_temp (value INTEGER)"); err != nil {
		t.Fatalf("create TEMP table: %v", err)
	}
	if _, err := scopes[0].conn.ExecContext(scopes[0].ctx, "INSERT INTO scope_temp (value) VALUES (1)"); err != nil {
		t.Fatalf("insert TEMP row: %v", err)
	}
	var count int
	if err := scopes[0].conn.QueryRowContext(scopes[0].ctx, "SELECT COUNT(*) FROM scope_temp").Scan(&count); err != nil || count != 1 {
		t.Fatalf("pinned TEMP state = (%d, %v), want (1, nil)", count, err)
	}
	if err := scopes[1].conn.QueryRowContext(scopes[1].ctx, "SELECT COUNT(*) FROM scope_temp").Scan(&count); err == nil {
		t.Fatal("TEMP table created on one pinned connection was visible to another connection")
	}
}

func TestMemoryOpenIsolatedAndPreservesState(t *testing.T) {
	first := openPoolMemoryDB(t)
	second := openPoolMemoryDB(t)
	scope := takePoolScope(t, first)
	if _, err := scope.conn.ExecContext(scope.ctx, "CREATE TABLE memory_rows (id INTEGER PRIMARY KEY)"); err != nil {
		scope.release()
		t.Fatalf("create memory table: %v", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO memory_rows (id) VALUES (1)"); err != nil {
		scope.release()
		t.Fatalf("insert memory row: %v", err)
	}
	scope.release()
	firstRead := takePoolScope(t, first)
	defer firstRead.release()
	var firstCount int
	if err := firstRead.conn.QueryRowContext(firstRead.ctx, "SELECT COUNT(*) FROM memory_rows").Scan(&firstCount); err != nil || firstCount != 1 {
		t.Fatalf("reacquire first memory DB = (%d, %v), want (1, nil)", firstCount, err)
	}
	other := takePoolScope(t, second)
	defer other.release()
	if err := other.conn.QueryRowContext(other.ctx, "SELECT COUNT(*) FROM memory_rows").Scan(new(int)); err == nil {
		t.Fatal("separate :memory: open unexpectedly observed first database state")
	}
}

func TestCloseIsConcurrentAndRejectsNewLeases(t *testing.T) {
	db, err := Open(t.TempDir()+"/close.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			<-start
			results <- db.Close()
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent Close: %v", err)
		}
	}
	if scope, err := db.bindScope(context.Background(), projectionTargetLive); scope != nil || err == nil {
		if scope != nil {
			scope.release()
		}
		t.Fatalf("bind after Close = (%v, %v), want nil scope and error", scope, err)
	}
}

func TestCloseWaitsForCanceledPinnedTransactionBeforeClosingPool(t *testing.T) {
	db, err := Open(t.TempDir()+"/in-flight-close.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		_ = db.Close()
		t.Fatalf("bind close-cancellable scope: %v", err)
	}
	t.Cleanup(func() {
		scope.release()
		_ = db.Close()
	})

	transactionEntered := make(chan struct{})
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
			close(transactionEntered)
			<-scope.ctx.Done()
			return scope.ctx.Err()
		})
	}()
	select {
	case <-transactionEntered:
	case err := <-transactionDone:
		t.Fatalf("pinned transaction ended before close race: %v", err)
	case <-time.After(poolTestTimeout):
		t.Fatal("pinned transaction did not begin")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()

	select {
	case <-scope.ctx.Done():
	case <-time.After(poolTestTimeout):
		t.Fatal("Close did not cancel the admitted scope context")
	}
	select {
	case err := <-transactionDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close-canceled pinned transaction = %v, want context canceled", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("pinned transaction did not stop after Close canceled its scope")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close completed before the pinned scope released: %v", err)
	default:
	}
	scope.release()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after pinned scope release: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("Close did not complete after pinned scope release")
	}
	if next, err := db.bindScope(context.Background(), projectionTargetLive); next != nil || err == nil {
		if next != nil {
			next.release()
		}
		t.Fatalf("bind after drained Close = (%v, %v), want nil scope and closed error", next, err)
	}
}

func TestCloseResultPublishesOneError(t *testing.T) {
	sentinel := errors.New("sentinel close failure")
	var result closeResult
	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			<-start
			results <- result.do(func() error { return sentinel })
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, sentinel) {
			t.Errorf("close result = %v, want sentinel", err)
		}
	}
}
