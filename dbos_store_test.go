package provenance_test

// dbos_store_test.go covers the borrowed-storage lifecycle (issue #6): handle
// validation and path derivation, in-memory rejection, the post-shutdown
// StoreUnavailableError liveness gate, migration coexistence with DBOS tables,
// repeat cleanup, standalone source-compatibility, and read-only queries creating
// no journal event.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

func openFileDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "borrow.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenBorrowedSQLite_NilHandle(t *testing.T) {
	_, err := provenance.OpenBorrowedSQLite(nil)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("err = %v, want a nil-handle rejection", err)
	}
}

func TestOpenBorrowedSQLite_InMemoryRejected(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = provenance.OpenBorrowedSQLite(db)
	if err == nil {
		t.Fatal("expected in-memory rejection, got nil")
	}
	if !strings.Contains(err.Error(), "OpenMemory") {
		t.Errorf("in-memory rejection must name OpenMemory as the alternative: %v", err)
	}
}

func TestOpenBorrowedSQLite_SharesFileWithCaller(t *testing.T) {
	db, path := openFileDB(t)
	tr, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	// The Provenance schema landed in the borrowed handle's database: the caller
	// sees the tasks table through its OWN handle (no second file).
	var name string
	row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='tasks'")
	if err := row.Scan(&name); err != nil {
		t.Fatalf("caller cannot see Provenance schema through its own handle: %v", err)
	}
	if name != "tasks" {
		t.Errorf("scanned %q, want tasks", name)
	}
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Errorf("borrowed file %q unusable by caller: %v", path, err)
	}
}

func TestBorrowed_PostShutdown_StoreUnavailable(t *testing.T) {
	db, _ := openFileDB(t)
	tr, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	// Simulate DBOS terminal shutdown closing the borrowed handle.
	if err := db.Close(); err != nil {
		t.Fatalf("close borrowed handle: %v", err)
	}

	_, showErr := tr.Show(provenance.TaskID{})
	mustStoreUnavailable(t, showErr, "Show")

	_, applyErr := tr.Journal().Apply(provenance.OperationInput{
		OperationID: "op-x", ActorID: nonZeroActor(), CommandDigest: []byte("c"), MutationDigest: []byte("m"),
	})
	mustStoreUnavailable(t, applyErr, "Journal.Apply")

	_, lookErr := tr.Journal().LookupCommitted("op-x")
	mustStoreUnavailable(t, lookErr, "Journal.LookupCommitted")

	// Cleanup after shutdown is safe and repeat-safe (closes only the bridge).
	if err := tr.Close(); err != nil {
		t.Errorf("first Close after shutdown: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("repeat Close: %v", err)
	}
}

func TestBorrowed_MigrationsCoexist_FreshExistingRepeat(t *testing.T) {
	db, _ := openFileDB(t)
	// Fresh open applies the schema.
	tr1, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("fresh open: %v", err)
	}
	sys, err := tr1.RegisterSoftwareAgent("provenance-test", "sys", "0", "t")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_ = sys
	_ = tr1.Close()

	// Existing/repeat open on the same file is idempotent (schema already present).
	tr2, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("existing open: %v", err)
	}
	t.Cleanup(func() { _ = tr2.Close() })
	if _, err := tr2.Agent(sys.ID); err != nil {
		t.Errorf("agent from prior open not visible after repeat open: %v", err)
	}
}

func TestBorrowed_ReadOnlyQueriesCreateNoEvent(t *testing.T) {
	s := newDBOSStack(t, nil)
	if _, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-ro", "aura", "ro")); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	before := journalMax(t, s.tracker)

	// A batch of read-only queries.
	if _, err := s.tracker.List(provenance.ListFilter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := s.tracker.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if _, err := s.tracker.Journal().QueryTaskEvents(provenance.JournalQueryV1{}); err != nil {
		t.Fatalf("QueryTaskEvents: %v", err)
	}

	if after := journalMax(t, s.tracker); after != before {
		t.Errorf("read-only queries created journal rows: max %d → %d", before, after)
	}
}

func TestStandalone_OpenSQLiteMemory_SourceCompatible(t *testing.T) {
	mem, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	_ = mem.Close()

	path := filepath.Join(t.TempDir(), "standalone.db")
	fileTr, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	_ = fileTr.Close()
}

// journalMax returns the current max journal_id via the ordered query surface's
// snapshot watermark, without inspecting private schema.
func journalMax(t *testing.T, tr provenance.Tracker) provenance.JournalID {
	t.Helper()
	page, err := tr.Journal().QueryTaskEvents(provenance.JournalQueryV1{})
	if err != nil {
		t.Fatalf("journalMax: %v", err)
	}
	return page.SnapshotMaxJournalID
}

func nonZeroActor() provenance.ActorID {
	id, err := provenance.ParseActorID("provenance-test--019f0000-0000-7000-8000-000000000001")
	if err != nil {
		panic(err)
	}
	return id
}
