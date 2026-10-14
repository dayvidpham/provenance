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

// makeStoreStale removes one object activation guarantees from an otherwise
// fully activated store, so the next open must run activation instead of taking
// the current-store skip. governed_allocation_genesis is empty and carries no
// data, and activation recreates it with CREATE TABLE IF NOT EXISTS.
func makeStoreStale(t *testing.T, path string) {
	t.Helper()
	stale, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open stale-store handle on %q: %v", path, err)
	}
	defer func() {
		if err := stale.Close(); err != nil {
			t.Errorf("close stale-store handle: %v", err)
		}
	}()
	if _, err := stale.Exec("DROP TABLE governed_allocation_genesis"); err != nil {
		t.Fatalf("make store %q stale: %v", path, err)
	}
}

// holdRawWriteLock takes SQLite's write lock on path from a raw connection that
// never runs activation, so a test can hold the lock over a deliberately stale
// store without a prior open restoring it.
func holdRawWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	holder, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw write-lock holder on %q: %v", path, err)
	}
	holder.SetMaxOpenConns(1)
	holder.SetMaxIdleConns(1)
	conn, err := holder.Conn(context.Background())
	if err != nil {
		_ = holder.Close()
		t.Fatalf("lease raw write-lock holder on %q: %v", path, err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		_ = holder.Close()
		t.Fatalf("acquire raw write lock on %q: %v", path, err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Errorf("release raw write lock: %v", err)
		}
		_ = conn.Close()
		if err := holder.Close(); err != nil {
			t.Errorf("close raw write-lock holder: %v", err)
		}
	}
	t.Cleanup(release)
	return release
}

// activationObjectExists reports whether one sqlite_master object is present.
func activationObjectExists(t *testing.T, pool *sql.DB, kind, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type=?1 AND name=?2)", kind, name).Scan(&exists); err != nil {
		t.Fatalf("probe %s %q: %v", kind, name, err)
	}
	return exists
}

// TestBorrowedActivationWaitsOutConcurrentWriter is the contention regression:
// with a deferred BEGIN the first seed write needed a read-to-write promotion,
// on which SQLite never calls the busy handler, so a contended activation failed
// instantly with SQLITE_BUSY and busy_timeout was never honoured. Activation must
// now wait for the holder and then succeed.
func TestBorrowedActivationWaitsOutConcurrentWriter(t *testing.T) {
	// Deliberately serial: this test asserts how long a contended activation
	// waits, so a CPU-saturated parallel phase would change the very quantity
	// under test.
	path := t.TempDir() + "/contended-activation.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	// A current store now skips activation, so the write-lock wait this test
	// pins is only reachable when activation is actually required. The holder is
	// a raw connection (not Open) so it cannot restore the stale object first.
	makeStoreStale(t, path)
	release := holdRawWriteLock(t, path)
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
	// Deliberately serial: this test asserts a lower bound on the wall time one
	// attempt spends inside SQLite's busy handler, which competing parallel
	// tests would distort.
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
	// Deliberately serial: this test asserts the elapsed budget of an exhausted
	// retry ceiling, a wall-clock quantity.
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
	// Deliberately serial: this test asserts a non-busy failure returns without
	// spending the retry budget, which is a wall-clock claim.
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
	t.Parallel()
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

// TestCurrentSchemaBorrowedOpenSkipsActivationWriteLock is the currency
// regression: an already-current borrowed store must open without taking the
// write lock. A deterministic holder owns BEGIN IMMEDIATE for the whole call, so
// if activation ran it would block on that lock (up to the 5s busy timeout) and
// fail; a fast success proves the activation write transaction — and therefore
// the write lock — is gone.
func TestCurrentSchemaBorrowedOpenSkipsActivationWriteLock(t *testing.T) {
	// Deliberately serial: this test holds a real file write lock and asserts the
	// open returns without waiting for it, a wall-clock claim.
	path := t.TempDir() + "/current-borrowed-open.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	pool := borrowedPool(t, path)
	release := holdWriteLock(t, path)

	start := time.Now()
	store, err := OpenBorrowed(pool, nil)
	elapsed := time.Since(start)
	release()
	if err != nil {
		t.Fatalf("current borrowed open while the write lock was held: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("close current borrowed store: %v", err)
	}
	if elapsed >= activationHoldWindow {
		t.Fatalf("current borrowed open took %s while the write lock was held; a current store must not take the activation write transaction", elapsed)
	}
}

// TestStaleSchemaBorrowedOpenStillActivates proves the skip is not unconditional:
// removing one activation-owned object forces the borrowed open to activate and
// restore it.
func TestStaleSchemaBorrowedOpenStillActivates(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/stale-borrowed-open.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	makeStoreStale(t, path)
	pool := borrowedPool(t, path)

	store, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatalf("stale borrowed open: %v", err)
	}
	defer store.Close()
	if !activationObjectExists(t, pool, "table", "governed_allocation_genesis") {
		t.Fatal("stale store was not activated: governed_allocation_genesis is still missing")
	}
}

// TestStaleSchemaFileOpenStillActivates proves the same for the plain file open.
func TestStaleSchemaFileOpenStillActivates(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/stale-file-open.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	makeStoreStale(t, path)

	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("stale file open: %v", err)
	}
	defer reopened.Close()
	if !activationObjectExists(t, borrowedPool(t, path), "table", "governed_allocation_genesis") {
		t.Fatal("stale store was not activated: governed_allocation_genesis is still missing")
	}
}

// TestActivationCurrencyMatchesFreshActivation pins the currency probe to
// activation itself. It asserts the expected-object list equals a freshly
// activated store's sqlite_master object set exactly, and that the probe accepts
// that store as current. A new or renamed schema object, or a seed list that
// drifts from activation's seeding, fails here rather than silently making the
// probe skip a store activation would still change.
func TestActivationCurrencyMatchesFreshActivation(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/fresh-activation.db"
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("activate fresh store on %q: %v", path, err)
	}
	defer db.Close()
	scope := takePoolScope(t, db)
	defer scope.release()

	actual := make(map[string]struct{})
	if err := scope.queryRows("SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'", nil, func(rows *sql.Rows) error {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return err
		}
		actual[kind+"\x00"+name] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("enumerate activated schema objects: %v", err)
	}
	want := make(map[string]struct{}, len(expectedActivationObjects))
	for _, object := range expectedActivationObjects {
		want[object.kind+"\x00"+object.name] = struct{}{}
	}
	for key := range want {
		if _, ok := actual[key]; !ok {
			t.Errorf("expectedActivationObjects lists %q but activation did not create it; update activation_currency.go", key)
		}
	}
	for key := range actual {
		if _, ok := want[key]; !ok {
			t.Errorf("activation created %q but expectedActivationObjects omits it; update activation_currency.go", key)
		}
	}
	if !scope.activationWouldChangeNothing(nil) {
		t.Fatal("a freshly activated store must be current; the currency probe or its seed lists have drifted from activation")
	}
}

// TestMissingSeedRowStillActivates proves the seed half of the currency probe:
// with every schema object present, deleting one closed-lookup row still makes
// the store non-current, so the next open re-seeds it instead of skipping.
func TestMissingSeedRowStillActivates(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/missing-seed.db"
	seed, err := Open(path, nil)
	if err != nil {
		t.Fatalf("seed schema on %q: %v", path, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}
	stale, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open stale-store handle on %q: %v", path, err)
	}
	if _, err := stale.Exec("DELETE FROM phases WHERE id = 12"); err != nil {
		_ = stale.Close()
		t.Fatalf("delete seeded phase row: %v", err)
	}
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale-store handle: %v", err)
	}
	pool := borrowedPool(t, path)

	store, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatalf("open store with a missing seed row: %v", err)
	}
	defer store.Close()
	var name string
	if err := pool.QueryRow("SELECT name FROM phases WHERE id = 12").Scan(&name); err != nil {
		t.Fatalf("seeded phase row 12 was not restored: %v", err)
	}
	if name != "unscoped" {
		t.Fatalf("restored phase row 12 = %q, want %q", name, "unscoped")
	}
}
