package sqlite

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// activationHoldWindow is how long the deterministic lock holder keeps the
// file's write lock while a contended activation is expected to wait. It is a
// hold duration, never a synchronization device: every wait below is a bounded
// wait on a channel condition.
const activationHoldWindow = 250 * time.Millisecond

// holdWriteLock opens a second pool on path, takes SQLite's write lock with an
// explicit BEGIN IMMEDIATE, and returns a release function. The lock is real
// file-level contention, exactly what a concurrent migrator produces.
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	holder, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open write-lock holder on %q: %v", path, err)
	}
	scope := takePoolScope(t, holder)
	if _, err := scope.conn.ExecContext(scope.ctx, "BEGIN IMMEDIATE"); err != nil {
		scope.release()
		_ = holder.Close()
		t.Fatalf("acquire deterministic write lock on %q: %v", path, err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if _, err := scope.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Errorf("release deterministic write lock: %v", err)
		}
		scope.release()
		if err := holder.Close(); err != nil {
			t.Errorf("close write-lock holder: %v", err)
		}
	}
	t.Cleanup(release)
	return release
}

// borrowedPool is a caller-owned pool on the same file, the shape OpenBorrowed
// is given in production.
func borrowedPool(t *testing.T, path string) *sql.DB {
	t.Helper()
	target, err := resolveOpenTarget(path)
	if err != nil {
		t.Fatalf("resolve borrowed target %q: %v", path, err)
	}
	pool, err := openConfiguredSQLDB(target.runtimeDSN, 1)
	if err != nil {
		t.Fatalf("open borrowed pool on %q: %v", path, err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("close borrowed pool: %v", err)
		}
	})
	return pool
}

// TestBorrowedActivationWaitsOutConcurrentWriter is the contention regression:
// with a deferred BEGIN the first seed write needed a read-to-write promotion,
// on which SQLite never calls the busy handler, so a contended activation failed
// instantly with SQLITE_BUSY and busy_timeout was never honoured. Activation must
// now wait for the holder and then succeed.
func TestBorrowedActivationWaitsOutConcurrentWriter(t *testing.T) {
	path := t.TempDir() + "/contended-activation.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	release := holdWriteLock(t, path)
	pool := borrowedPool(t, path)

	type activation struct {
		db      *DB
		err     error
		elapsed time.Duration
	}
	done := make(chan activation, 1)
	start := time.Now()
	go func() {
		db, err := OpenBorrowed(pool, nil)
		done <- activation{db: db, err: err, elapsed: time.Since(start)}
	}()

	// The defect signature is an immediate return, so the first assertion is that
	// activation is still waiting while the holder owns the lock.
	select {
	case result := <-done:
		t.Fatalf("contended activation returned after %s while the write lock was held (err=%v); "+
			"want it to wait for the lock instead of failing instantly", result.elapsed, result.err)
	case <-time.After(activationHoldWindow):
	}

	release()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("activation after lock release: %v", result.err)
		}
		if result.elapsed < activationHoldWindow {
			t.Fatalf("activation elapsed %s, want at least the %s lock hold", result.elapsed, activationHoldWindow)
		}
		if err := result.db.Close(); err != nil {
			t.Errorf("close activated borrowed store: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("activation did not complete after the write lock was released")
	}
}

// TestSingleActivationAttemptHonoursBusyTimeout is the layer-1 pin, isolated
// from the retry loop: one activation attempt against a held write lock must
// spend its whole busy_timeout inside SQLite's busy handler before failing. With
// the old deferred BEGIN it returned in about zero seconds, because SQLite never
// invokes the busy handler for a read-to-write promotion. The lower bound is
// asserted; no upper bound is, so a slow machine cannot make this flake.
func TestSingleActivationAttemptHonoursBusyTimeout(t *testing.T) {
	path := t.TempDir() + "/single-attempt-activation.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	holdWriteLock(t, path)

	pool := borrowedPool(t, path)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatalf("lease activation connection: %v", err)
	}
	defer conn.Close()
	scope := borrowConnScope(conn, projectionTargetLive)

	const attemptBusyTimeout = 300 * time.Millisecond
	// The busy handler may return marginally early; require most of the budget.
	const minimumWait = attemptBusyTimeout / 2
	policy := activationRetryPolicy{attemptBusyTimeout: attemptBusyTimeout}
	start := time.Now()
	err = activateSchema(scope, nil, policy)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("activation unexpectedly succeeded while another connection held the write lock")
	}
	if !isBusyError(err) {
		t.Fatalf("contended activation failed with a non-contention error: %v", err)
	}
	if elapsed < minimumWait {
		t.Fatalf("contended activation attempt failed after %s, want it to wait out its %s busy_timeout "+
			"(a near-instant failure means the transaction is deferred and SQLite skipped the busy handler "+
			"on the read-to-write promotion)", elapsed, attemptBusyTimeout)
	}
}

// TestActivationRetryCeilingReportsTheContendedFile pins the exhausted-budget
// error: an injected tiny ceiling keeps the test fast while proving the ceiling
// is enforced and that the message names the file, the budget, the likely
// concurrent migrator, and that nothing was written.
func TestActivationRetryCeilingReportsTheContendedFile(t *testing.T) {
	path := t.TempDir() + "/exhausted-activation.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	holdWriteLock(t, path)

	pool := borrowedPool(t, path)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatalf("lease contended activation connection: %v", err)
	}
	defer conn.Close()
	scope := borrowConnScope(conn, projectionTargetLive)

	policy := activationRetryPolicy{
		attemptBusyTimeout: 20 * time.Millisecond,
		ceiling:            60 * time.Millisecond,
		initialDelay:       5 * time.Millisecond,
		maxDelay:           10 * time.Millisecond,
	}
	start := time.Now()
	err = activateSchemaWithRetry(scope, nil, policy)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("contended activation with an exhausted budget unexpectedly succeeded")
	}
	if elapsed >= poolTestTimeout {
		t.Fatalf("contended activation took %s, want the %s ceiling to bound it", elapsed, policy.ceiling)
	}
	message := err.Error()
	// The budget bounds when the last attempt may start, so the measured elapsed
	// time is reported separately from the budget rather than conflated with it.
	if !strings.Contains(message, "gave up after ") {
		t.Errorf("activation ceiling error does not report the measured elapsed time\ngot: %s", message)
	}
	for _, want := range []string{
		"activate SQLite schema on ",
		path,
		"budget 60ms",
		"where: internal/sqlite.activateSchemaWithRetry, startup schema activation",
		"most likely a concurrent migrator",
		"no schema or seed row was written",
		"fix: wait for the other writer to finish and retry the open",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("activation ceiling error missing %q\ngot: %s", want, message)
		}
	}
}

// TestActivationRetryReturnsNonBusyFailuresUnchanged keeps the retry loop honest:
// only lock contention is retryable, and any other SQLite failure must surface on
// the first attempt rather than being retried until the ceiling.
func TestActivationRetryReturnsNonBusyFailuresUnchanged(t *testing.T) {
	path := t.TempDir() + "/non-busy-activation.db"
	pool := borrowedPool(t, path)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatalf("lease activation connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE statuses (unexpected TEXT NOT NULL) STRICT"); err != nil {
		t.Fatalf("install conflicting reference table: %v", err)
	}
	scope := borrowConnScope(conn, projectionTargetLive)

	policy := activationRetryPolicy{
		attemptBusyTimeout: 20 * time.Millisecond,
		ceiling:            30 * time.Second,
		initialDelay:       5 * time.Millisecond,
		maxDelay:           10 * time.Millisecond,
	}
	start := time.Now()
	err = activateSchemaWithRetry(scope, nil, policy)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("activation against a conflicting schema unexpectedly succeeded")
	}
	if isBusyError(err) {
		t.Fatalf("schema conflict misclassified as lock contention: %v", err)
	}
	// Non-busy must not merely mean "some other error": the caller has to receive
	// the seeded conflict itself, unchanged by the retry wrapper.
	message := err.Error()
	if !strings.Contains(message, "apply schema") || !strings.Contains(message, "statuses") {
		t.Errorf("want the seeded statuses schema conflict surfaced verbatim, got: %s", message)
	}
	if elapsed >= poolTestTimeout {
		t.Fatalf("non-retryable failure took %s; it must return on the first attempt", elapsed)
	}
}

// TestActivationTransactionIsImmediate pins the fix structurally: activateSchema
// must open its transaction through runImmediateTransaction and must never name a
// bare deferred BEGIN again. The behavioural proof lives in the contention test
// above; this one stops a future edit from silently reintroducing the defect.
//
// What it does not cover: the pin is syntactic, so a deferred BEGIN reached
// through a named constant, a variable, or a helper call would slip past it. The
// contention tests above are the backstop for that.
func TestActivationTransactionIsImmediate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "db.go", nil, 0)
	if err != nil {
		t.Fatalf("parse db.go: %v", err)
	}
	var activation *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "activateSchema" {
			activation = fn
			break
		}
	}
	if activation == nil {
		t.Fatal("db.go no longer declares activateSchema; update this pin to follow the activation path")
	}
	immediate := false
	ast.Inspect(activation, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			if typed.Name == "runImmediateTransaction" {
				immediate = true
			}
		case *ast.BasicLit:
			if strings.EqualFold(strings.Trim(typed.Value, `"`), "BEGIN") {
				t.Errorf("activateSchema issues a deferred %s: contention would bypass busy_timeout on the "+
					"read-to-write promotion; use runImmediateTransaction", typed.Value)
			}
		}
		return true
	})
	if !immediate {
		t.Error("activateSchema does not use runImmediateTransaction; the activation transaction must take the write lock at BEGIN")
	}
}
