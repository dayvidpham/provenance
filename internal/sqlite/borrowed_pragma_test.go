package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// borrowedPoolTestConns matches the caller-pool size these tests configure. Every
// connection is leased at once so the sample covers the whole pool, not just the
// connection database/sql happens to hand out first.
const borrowedPoolTestConns = 4

// borrowedCallerPool opens a caller-owned pool shaped like the DBOS root's:
// file-backed, WAL, an explicit busy_timeout, and whatever foreign-key
// configuration the caller chose.
func borrowedCallerPool(t *testing.T, extraPragmas string) (*sql.DB, string) {
	t.Helper()
	path := t.TempDir() + "/borrowed-pragma.db"
	dsn := "file:" + path + "?_pragma=busy_timeout(4321)&_pragma=journal_mode(WAL)" + extraPragmas
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open caller-owned pool: %v", err)
	}
	pool.SetMaxOpenConns(borrowedPoolTestConns)
	// Idle capacity matches open capacity so the pool-wide oracle really samples
	// the connections operations used. With the default idle cap, database/sql
	// would close the surplus on release and hand the oracle fresh connections
	// carrying the DSN's pragmas — hiding exactly the leak these tests look for.
	pool.SetMaxIdleConns(borrowedPoolTestConns)
	t.Cleanup(func() { _ = pool.Close() })
	return pool, path
}

// readPoolPragma leases every connection in the pool simultaneously and reports
// each connection's value for one connection-local pragma. Holding all leases at
// once is what makes this a pool-wide oracle: no two samples can be the same
// physical connection.
func readPoolPragma(t *testing.T, pool *sql.DB, pragma string) []int64 {
	t.Helper()
	conns := make([]*sql.Conn, 0, borrowedPoolTestConns)
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	values := make([]int64, 0, borrowedPoolTestConns)
	for i := 0; i < borrowedPoolTestConns; i++ {
		conn, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatalf("lease pool connection %d for %s sample: %v", i, pragma, err)
		}
		conns = append(conns, conn)
		var value int64
		if err := conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&value); err != nil {
			t.Fatalf("read PRAGMA %s on pool connection %d: %v", pragma, i, err)
		}
		values = append(values, value)
	}
	return values
}

// exerciseBorrowedStore runs one real borrowed write and one real borrowed read,
// so the pragma oracle observes the state left by production lease paths rather
// than by a test-only helper.
func exerciseBorrowedStore(t *testing.T, store *DB, name string) {
	t.Helper()
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, store, actor)
	input := journal.OperationInput{
		OperationID:   journal.OperationID(name),
		ActorID:       actor,
		CommandDigest: []byte("borrowed-pragma-command"),
		Effects:       []journal.Effect{{Sort: journal.EffectBootstrapAuthority, BootstrapLabel: "borrowed-pragma", ResultSlot: "authority"}},
	}
	if _, err := store.Apply(input); err != nil {
		t.Fatalf("borrowed Apply: %v", err)
	}
	if _, err := store.LookupCommitted(input.OperationID); err != nil {
		t.Fatalf("borrowed LookupCommitted: %v", err)
	}
}

// TestBorrowedPoolReturnsCallerPragmaState pins OpenBorrowed's contract that the
// caller retains pool configuration: after activation and real operations, every
// connection in the caller's pool still reports the caller's own foreign_keys and
// busy_timeout values, so behavior that depends on them (ON DELETE CASCADE for the
// caller's statements) cannot vary by which connection the caller draws.
func TestBorrowedPoolReturnsCallerPragmaState(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name            string
		callerPragmas   string
		wantForeignKeys int64
	}{
		{name: "caller leaves foreign keys off", callerPragmas: "", wantForeignKeys: 0},
		{name: "caller enables foreign keys", callerPragmas: "&_pragma=foreign_keys(1)", wantForeignKeys: 1},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			pool, _ := borrowedCallerPool(t, arm.callerPragmas)
			store, err := OpenBorrowed(pool, nil)
			if err != nil {
				t.Fatalf("OpenBorrowed: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			exerciseBorrowedStore(t, store, "borrowed-pragma-"+fmt.Sprint(arm.wantForeignKeys))

			for i, got := range readPoolPragma(t, pool, "foreign_keys") {
				if got != arm.wantForeignKeys {
					t.Errorf("pool connection %d foreign_keys = %d after borrowed operations, want the caller's %d on every connection", i, got, arm.wantForeignKeys)
				}
			}
			for i, got := range readPoolPragma(t, pool, "busy_timeout") {
				if got != 4321 {
					t.Errorf("pool connection %d busy_timeout = %d after borrowed operations, want the caller's 4321 on every connection", i, got)
				}
			}
		})
	}
}

// TestBorrowedLeaseEnforcesForeignKeysWhileHeld proves the other half of the
// contract: Provenance's own writes always run with enforcement on, even over a
// caller pool that has foreign keys off, and the caller's value comes back the
// moment the lease releases.
func TestBorrowedLeaseEnforcesForeignKeysWhileHeld(t *testing.T) {
	t.Parallel()
	pool, _ := borrowedCallerPool(t, "")
	store, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatalf("OpenBorrowed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	scope := takePoolScope(t, store)
	var duringLease int64
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA foreign_keys").Scan(&duringLease); err != nil {
		scope.release()
		t.Fatalf("read foreign_keys during borrowed lease: %v", err)
	}
	if duringLease != 1 {
		scope.release()
		t.Fatalf("foreign_keys during borrowed lease = %d, want 1: Provenance writes must be enforced regardless of caller configuration", duringLease)
	}
	scope.release()

	for i, got := range readPoolPragma(t, pool, "foreign_keys") {
		if got != 0 {
			t.Errorf("pool connection %d foreign_keys = %d after the lease released, want the caller's 0", i, got)
		}
	}
}

// TestOwnedPoolLeaseReArmsForeignKeys pins the owned-pool half of the discipline:
// enforcement is re-armed per lease, so a connection left unenforced by a failed
// restore heals on its next lease instead of staying unenforced for the process.
func TestOwnedPoolLeaseReArmsForeignKeys(t *testing.T) {
	t.Parallel()
	db, err := Open(t.TempDir()+"/owned-fk-rearm.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// One connection makes the next lease provably the same physical connection.
	db.db.SetMaxOpenConns(1)

	drifted := takePoolScope(t, db)
	if _, err := drifted.conn.ExecContext(drifted.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		drifted.release()
		t.Fatalf("simulate a rebuild whose restore failed: %v", err)
	}
	drifted.release()

	healed := takePoolScope(t, db)
	defer healed.release()
	var enforced int64
	if err := healed.conn.QueryRowContext(healed.ctx, "PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatalf("read foreign_keys on the re-leased owned connection: %v", err)
	}
	if enforced != 1 {
		t.Fatalf("owned pool foreign_keys = %d on the next lease, want 1: a failed restore must not outlive one lease", enforced)
	}
}

// TestBorrowedReleaseRetiresConnectionWhenRestoreCannotLand covers the silent
// hole in an unverified restore. SQLite ignores PRAGMA foreign_keys while a
// transaction is open on the connection: the restore statement reports success
// and changes nothing. A release that trusts that success would hand the caller
// back a connection carrying Provenance's foreign_keys=ON instead of the caller's
// OFF, permanently and invisibly, for every later statement that drew it. The
// contract is that an unprovable restore retires the connection instead.
func TestBorrowedReleaseRetiresConnectionWhenRestoreCannotLand(t *testing.T) {
	t.Parallel()
	pool, _ := borrowedCallerPool(t, "")
	store, err := OpenBorrowed(pool, nil)
	if err != nil {
		t.Fatalf("OpenBorrowed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	scope := takePoolScope(t, store)
	if !scope.restoreForeignKeysOff {
		scope.release()
		t.Fatal("lease over a caller pool with foreign_keys off did not record the restore it owes the caller")
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "BEGIN"); err != nil {
		scope.release()
		t.Fatalf("open a transaction on the leased connection: %v", err)
	}

	// Demonstrate the hole directly: the naive restore succeeds and does nothing.
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		scope.release()
		t.Fatalf("unverified restore statement: %v", err)
	}
	var afterNaiveRestore int64
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA foreign_keys").Scan(&afterNaiveRestore); err != nil {
		scope.release()
		t.Fatalf("read foreign_keys after the unverified restore: %v", err)
	}
	if afterNaiveRestore != 1 {
		scope.release()
		t.Fatalf("foreign_keys after the unverified restore = %d, want 1: this test no longer reproduces the no-op restore it guards", afterNaiveRestore)
	}

	scope.restoreBorrowedPragmas()
	if !scope.discarded {
		scope.release()
		t.Fatal("connection was returned to the caller's pool with Provenance's foreign_keys=ON; an unprovable restore must retire the connection")
	}
	if scope.restoreForeignKeysOff != true {
		t.Error("a failed restore cleared the debt it still owes the caller")
	}
	scope.release()

	for i, got := range readPoolPragma(t, pool, "foreign_keys") {
		if got != 0 {
			t.Errorf("pool connection %d foreign_keys = %d after the retirement, want the caller's 0", i, got)
		}
	}
}
