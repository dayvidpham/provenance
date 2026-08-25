package provenance_test

// dbos_store_test.go covers the borrowed-storage lifecycle (issue #6): handle
// validation and path derivation, in-memory rejection, the post-shutdown
// StoreUnavailableError liveness gate, migration coexistence with DBOS tables,
// repeat cleanup, standalone source-compatibility, and read-only queries creating
// no journal event.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	db.SetMaxOpenConns(dbosTestPoolSize)
	db.SetMaxIdleConns(dbosTestPoolSize / 2)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenBorrowedSQLite_NilHandle(t *testing.T) {
	t.Parallel()
	_, err := provenance.OpenBorrowedSQLite(nil)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("err = %v, want a nil-handle rejection", err)
	}
}

func TestOpenBorrowedSQLite_InMemoryRejected(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(dbosTestPoolSize)
	db.SetMaxIdleConns(dbosTestPoolSize / 2)
	t.Cleanup(func() { _ = db.Close() })
	_, err = provenance.OpenBorrowedSQLite(db)
	if err == nil {
		t.Fatal("expected in-memory rejection, got nil")
	}
	if !strings.Contains(err.Error(), "OpenMemory") {
		t.Errorf("in-memory rejection must name OpenMemory as the alternative: %v", err)
	}
}

func TestOpenBorrowedSQLite_PreservesCallerPoolLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		maxOpen int
	}{
		{name: "deliberate small pool", maxOpen: 1},
		{name: "recommended DBOS test pool", maxOpen: dbosTestPoolSize},
		{name: "unlimited", maxOpen: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pool.db")
			db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
			if err != nil {
				t.Fatal(err)
			}
			if test.maxOpen != 0 {
				db.SetMaxOpenConns(test.maxOpen)
				db.SetMaxIdleConns(test.maxOpen / 2)
			}
			t.Cleanup(func() { _ = db.Close() })

			tr, err := provenance.OpenBorrowedSQLite(db)
			if err != nil {
				t.Fatalf("OpenBorrowedSQLite with MaxOpenConnections=%d: %v", test.maxOpen, err)
			}
			if got := db.Stats().MaxOpenConnections; got != test.maxOpen {
				t.Fatalf("OpenBorrowedSQLite mutated caller pool limit: got %d, want %d", got, test.maxOpen)
			}
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDBOSBorrowedSQLitePool16SupportsConcurrentOperations(t *testing.T) {
	t.Parallel()
	s := newDBOSStack(t, nil)
	if got := s.db.Stats().MaxOpenConnections; got != dbosTestPoolSize {
		t.Fatalf("DBOS test pool limit = %d, want %d", got, dbosTestPoolSize)
	}

	const dbosOperations = 4
	start := make(chan struct{})
	errs := make(chan error, dbosOperations*2)
	var wg sync.WaitGroup
	for i := 0; i < dbosOperations; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			op := s.createTaskOp(fmt.Sprintf("op-pool-%d", i), "pool", fmt.Sprintf("pool-%d", i))
			if _, err := s.adapter.Apply(context.Background(), op); err != nil {
				errs <- fmt.Errorf("concurrent DBOS Apply %d: %w", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.tracker.List(provenance.ListFilter{}); err != nil {
				errs <- fmt.Errorf("concurrent borrowed List %d: %w", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := 0; i < dbosOperations; i++ {
		result, err := s.tracker.Journal().LookupCommitted(provenance.OperationID(fmt.Sprintf("op-pool-%d", i)))
		if err != nil {
			t.Fatalf("LookupCommitted operation %d: %v", i, err)
		}
		if result.Kind != provenance.CommittedExact {
			t.Errorf("operation %d kind = %v, want CommittedExact", i, result.Kind)
		}
	}
}

func TestOpenBorrowedSQLite_SharesFileWithCaller(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

	facts := tr.Journal().Facts()
	_, decisionErr := facts.QueryDecisions(provenance.DecisionQuery{})
	mustStoreUnavailable(t, decisionErr, "Journal.Facts.QueryDecisions")
	_, evidenceErr := facts.QueryEvidence(provenance.EvidenceQuery{})
	mustStoreUnavailable(t, evidenceErr, "Journal.Facts.QueryEvidence")

	// Cleanup after shutdown is safe and repeat-safe (closes only the bridge).
	if err := tr.Close(); err != nil {
		t.Errorf("first Close after shutdown: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("repeat Close: %v", err)
	}
}

// TestBorrowed_PostShutdown_SessionGated proves a Session obtained from a borrowed
// tracker BEFORE the owning DBOS root shuts down is liveness-gated on EVERY verb
// after shutdown — the sentinel-bypass the raw inner Session would otherwise leave
// open (issue #6, review axes B/C). A Session across a shutdown boundary is ordinary
// (a long-lived actor session, or one obtained just before a graceful redeploy), and
// per the relational contract Session is the PRIMARY mutation path, so this is the
// primary write surface, not a niche escape hatch.
func TestBorrowed_PostShutdown_SessionGated(t *testing.T) {
	t.Parallel()
	db, _ := openFileDB(t)
	tr, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	// Obtain the Session BEFORE shutdown.
	sess := tr.As(nonZeroActor(), provenance.JournalID(1))

	// The DBOS root shuts down, closing the borrowed handle (its pool).
	if err := db.Close(); err != nil {
		t.Fatalf("close borrowed handle: %v", err)
	}

	tid := provenance.TaskID{} // the liveness gate fires before the id is ever used
	agentID := provenance.AgentID{}
	ctx := context.Background()

	gated := map[string]bool{}
	mustGate := func(verb string, err error) {
		t.Helper()
		gated[verb] = true
		mustStoreUnavailable(t, err, "Session."+verb)
	}

	_, createErr := sess.Create("aura", "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	mustGate("Create", createErr)

	_, updateErr := sess.Update(tid, provenance.UpdateFields{})
	mustGate("Update", updateErr)

	_, startErr := sess.Start(tid)
	mustGate("Start", startErr)

	_, stopErr := sess.Stop(tid)
	mustGate("Stop", stopErr)

	_, reopenErr := sess.Reopen(tid)
	mustGate("Reopen", reopenErr)

	_, closeErr := sess.CloseTask(tid, "done")
	mustGate("CloseTask", closeErr)

	_, atomicErr := sess.Atomic(func(op *provenance.Operation) {
		op.CreateTask(tid, "t", "", provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseUnscoped)
	})
	mustGate("Atomic", atomicErr)

	mustGate("AddEdge", sess.AddEdge(tid, "aura--x", provenance.EdgeBlockedBy))
	mustGate("RemoveEdge", sess.RemoveEdge(tid, "aura--x", provenance.EdgeBlockedBy))
	mustGate("AddLabel", sess.AddLabel(tid, "label"))
	mustGate("RemoveLabel", sess.RemoveLabel(tid, "label"))

	_, commentErr := sess.AddComment(tid, agentID, "body")
	mustGate("AddComment", commentErr)

	// The governed-allocation verbs are gated on the same boundary. Each is
	// called with a request the store would reject on its own merits, so a
	// StoreUnavailableError proves the gate ran first rather than the request
	// validation answering for it.
	_, allocErr := sess.AllocateGoverned(ctx, provenance.GovernedAllocationRequest{})
	mustGate("AllocateGoverned", allocErr)

	_, composedErr := sess.AllocateGovernedComposed(ctx, provenance.GovernedAllocationComposedRequest{})
	mustGate("AllocateGovernedComposed", composedErr)

	_, batchErr := sess.AllocateGovernedComposedBatch(ctx, provenance.GovernedAllocationComposedBatchRequest{})
	mustGate("AllocateGovernedComposedBatch", batchErr)

	_, transferErr := sess.TransferAssignment(provenance.AssignmentTransferRequest{})
	mustGate("TransferAssignment", transferErr)

	assertEverySessionVerbIsGated(t, sess, gated)

	// Cleanup after shutdown is safe (closes only the bridge).
	if err := tr.Close(); err != nil {
		t.Errorf("Close after shutdown: %v", err)
	}
}

// assertEverySessionVerbIsGated is the completeness half of the liveness gate.
// The list of verbs above is written by hand, and a hand-written list is exactly
// what a newly added verb slips past: the new verb would reach a closed handle
// and report whatever the driver says, while this test still passed. Reflection
// over the Session method set makes the omission fail here instead.
func assertEverySessionVerbIsGated(t *testing.T, session *provenance.Session, gated map[string]bool) {
	t.Helper()
	sessionType := reflect.TypeOf(session)
	if sessionType.NumMethod() == 0 {
		t.Fatal("Session exposes no methods -- where: assertEverySessionVerbIsGated; why: reflection found an empty method set, so the completeness check cannot see a new verb; impact: an ungated verb would pass unnoticed; fix: confirm the Session type passed here is the pointer type the API returns")
	}
	for i := 0; i < sessionType.NumMethod(); i++ {
		verb := sessionType.Method(i).Name
		if !gated[verb] {
			t.Errorf("Session.%s is not covered by the post-shutdown liveness assertions; every public verb must fail with *StoreUnavailableError once the borrowed handle is closed. Add a call for it above (or gate it in session.go if it does not call checkGate).", verb)
		}
	}
}

func TestBorrowed_MigrationsCoexist_FreshExistingRepeat(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
