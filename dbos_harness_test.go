package provenance_test

// dbos_harness_test.go builds the real DBOS + borrowed-tracker stack every adapter
// test drives, in the issue's startup order (open *sql.DB → NewDBOSContext →
// OpenBorrowedSQLite → NewDBOSAdapter → Launch). It uses the pinned
// modernc.org/sqlite driver DBOS itself uses, on a shared temp file, so the DBOS
// checkpoints and the Provenance domain rows are co-located in one database.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	"github.com/dayvidpham/provenance"
	_ "modernc.org/sqlite"
)

const dbosTestPoolSize = 16

// uuidV7 mints a fresh UUIDv7 string, matching how Session.Create mints task ids.
func uuidV7() string { return uuid.Must(uuid.NewV7()).String() }

// dbosStack is one fully-wired adapter over a shared SQLite file.
type dbosStack struct {
	root    dbos.DBOSContext
	db      *sql.DB
	tracker provenance.Tracker
	adapter *provenance.DBOSAdapter
	path    string
	actor   provenance.ActorID
	boot    provenance.JournalID
}

// newDBOSStack wires the full stack and establishes a genesis authority, returning
// a stack ready for Apply. It registers cleanup that shuts DBOS down and closes the
// bridge. tracker is the tracker the adapter folds through: pass nil for the real
// borrowed tracker, or a wrapper for divergence injection.
func newDBOSStack(t *testing.T, wrap func(provenance.Tracker) provenance.Tracker) *dbosStack {
	t.Helper()
	stack := newDBOSStackUnlaunched(t, wrap)
	launchDBOSStack(t, stack)
	return stack
}

func newDBOSStackUnlaunched(t *testing.T, wrap func(provenance.Tracker) provenance.Tracker) *dbosStack {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.db")

	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(dbosTestPoolSize)
	db.SetMaxIdleConns(dbosTestPoolSize / 2)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping shared db: %v", err)
	}

	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName:            "provenance-dbos-test",
		SqliteSystemDB:     db,
		ApplicationVersion: "test-v1",
	})
	if err != nil {
		t.Fatalf("NewDBOSContext: %v", err)
	}

	borrowed, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	tracker := provenance.Tracker(borrowed)
	if wrap != nil {
		tracker = wrap(borrowed)
	}

	adapter, err := provenance.NewDBOSAdapter(root, tracker, provenance.DBOSAdapterConfig{})
	if err != nil {
		t.Fatalf("NewDBOSAdapter: %v", err)
	}

	// Establish the committing actor and genesis authority BEFORE Launch: these are
	// pure domain (zombiezen) writes, and doing them before DBOS starts its recovery/
	// queue writers avoids WAL write contention on the shared file at startup.
	sys, err := borrowed.RegisterSoftwareAgent("provenance-test", "pasture-system", "0", "test")
	if err != nil {
		t.Fatalf("RegisterSoftwareAgent: %v", err)
	}
	boot := establishGenesisBorrowed(t, borrowed, sys.ID)

	t.Cleanup(func() {
		root.Shutdown(5 * time.Second)
		_ = borrowed.Close()
	})

	return &dbosStack{
		root: root, db: db, tracker: tracker, adapter: adapter,
		path: path, actor: sys.ID, boot: boot,
	}
}

func launchDBOSStack(t *testing.T, stack *dbosStack) {
	t.Helper()
	if err := dbos.Launch(stack.root); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}

// newUnlaunchedRoot wires a DBOS root + borrowed tracker WITHOUT launching or
// registering an adapter, for construction-time assertions (e.g. version checks).
func newUnlaunchedRoot(t *testing.T, appVersion string) (dbos.DBOSContext, provenance.Tracker) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unlaunched.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(dbosTestPoolSize)
	db.SetMaxIdleConns(dbosTestPoolSize / 2)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	root, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
		AppName: "provenance-unlaunched-test", SqliteSystemDB: db, ApplicationVersion: appVersion,
	})
	if err != nil {
		t.Fatalf("NewDBOSContext: %v", err)
	}
	borrowed, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatalf("OpenBorrowedSQLite: %v", err)
	}
	t.Cleanup(func() {
		root.Shutdown(3 * time.Second)
		_ = borrowed.Close()
	})
	return root, borrowed
}

// establishGenesisBorrowed applies one genesis bootstrap operation directly through
// the tracker's journal (not the adapter) and returns the produced bootstrap
// authority JournalID.
func establishGenesisBorrowed(t *testing.T, tr provenance.Tracker, actor provenance.ActorID) provenance.JournalID {
	t.Helper()
	res, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:    "op-genesis",
		ActorID:        actor,
		CommandDigest:  []byte("genesis-c"),
		MutationDigest: []byte("genesis-m"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectBootstrapAuthority, BootstrapLabel: "pasture-system", ResultSlot: "auth",
		}},
	})
	if err != nil {
		t.Fatalf("establishGenesisBorrowed: %v", err)
	}
	for i := range res.ResultSlots {
		if string(res.ResultSlots[i].Slot) == "auth" {
			return res.ResultSlots[i].ProducedJournalID
		}
	}
	t.Fatal("establishGenesisBorrowed: no bootstrap authority slot")
	return 0
}

// createTaskOp builds a journaled task-create OperationInput under the stack's
// genesis authority, keyed by opID, for a task in namespace with title.
func (s *dbosStack) createTaskOp(opID, namespace, title string) provenance.OperationInput {
	auth := s.boot
	return provenance.OperationInput{
		OperationID:        provenance.OperationID(opID),
		ActorID:            s.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte("cmd:" + opID),
		MutationDigest:     []byte("mut:" + opID),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []provenance.Effect{{
			Sort:        provenance.EffectTaskCreate,
			ResultSlot:  "task",
			TaskID:      newTaskID(namespace),
			Title:       title,
			Description: "created via DBOS adapter",
			Type:        provenance.TaskTypeTask,
			Priority:    provenance.PriorityMedium,
			Phase:       provenance.PhaseWorkerSlices,
		}},
	}
}

// newTaskID mints a fresh UUIDv7 TaskID in namespace, matching how Session.Create
// mints ids.
func newTaskID(namespace string) provenance.TaskID {
	id, err := provenance.ParseTaskID(namespace + "--" + uuidV7())
	if err != nil {
		panic(fmt.Sprintf("newTaskID: %v", err))
	}
	return id
}

// leakCheck records the goroutine baseline and returns an assertion that no
// goroutine leaked (bounded settle), a dependency-free stand-in for goleak that
// honors the single-new-dependency constraint.
func leakCheck(t *testing.T) func() {
	t.Helper()
	base := runtime.NumGoroutine()
	return func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			runtime.GC()
			n := runtime.NumGoroutine()
			if n <= base+1 { // +1 tolerance for scheduler/GC transients
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("goroutine leak: baseline %d, now %d", base, n)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func mustStoreUnavailable(t *testing.T, err error, op string) {
	t.Helper()
	var su *provenance.StoreUnavailableError
	if !errors.As(err, &su) {
		t.Fatalf("%s: expected *StoreUnavailableError, got %v", op, err)
	}
	if su.Operation == "" || su.Store == "" || su.Stage == "" || su.Impact == "" || su.Fix == "" || su.Cause == nil {
		t.Errorf("%s: StoreUnavailableError has empty actionable fields: %+v", op, su)
	}
}
