package provenance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	provenance "github.com/dayvidpham/provenance"
	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "modernc.org/sqlite"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// each test builds its own bound or host-borrowed allocator over a private
// t.TempDir database, with participant counters local to its own closure.

func TestBoundGovernedAllocatorUsesHostRootAndReportsReplay(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "host-bound.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	participantCalls := 0
	participantChildren := 0
	bound, err := provenance.OpenBoundGovernedAllocator(ctx, provenance.FusedGovernedAllocatorConfig{
		SQLiteDSN: dsn, AppName: "bound-host", ApplicationVersion: "test-v1", Logger: slog.Default(),
		Participant: func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
			participantCalls++
			participantChildren = len(closure.Children())
			_, err := tx.Exec(ctx, `INSERT INTO host_bound_participant(operation_id) VALUES (?1)`, request.OperationID)
			return err
		},
	})
	if err != nil {
		t.Fatalf("open certified bound allocator: %v", err)
	}
	t.Cleanup(func() { _ = bound.Close(30 * time.Second) })
	actor := registerGovernedActor(t, bound.Tracker(), "bound-runtime")
	setupDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open setup observer: %v", err)
	}
	t.Cleanup(func() { _ = setupDB.Close() })
	if _, err := setupDB.Exec(`CREATE TABLE host_bound_participant (operation_id TEXT PRIMARY KEY) STRICT`); err != nil {
		t.Fatalf("create participant table: %v", err)
	}
	if err := bound.Launch(); err != nil {
		t.Fatalf("launch host DBOS root: %v", err)
	}
	rootRequest := provenance.RootGenesisRequest{
		OperationID: "bound-runtime-genesis", ActorID: actor, Command: "test.genesis",
		Root: governedChild("bound-runtime-root", actor),
	}
	root, err := bound.RunInitializeRoot(ctx, "bound-runtime-root-workflow", rootRequest)
	if err != nil {
		t.Fatalf("initialize bound root: %v", err)
	}
	rootBinding, ok := root.Root()
	if !ok {
		t.Fatal("bound root closure has no root binding")
	}

	request := composedGovernedRequest("bound-runtime-allocation", actor, rootBinding, 2)
	request.SupplementalEffects = request.SupplementalEffects[3:]
	first, err := bound.RunAllocateComposedBatch(ctx, "bound-runtime-workflow", rootBinding.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run host-bound allocation: %v", err)
	}
	if first.Replayed() {
		t.Fatal("first host-bound invocation reported replay")
	}
	retrieved, err := bound.RunAllocateComposedBatch(ctx, "bound-runtime-workflow", rootBinding.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("retrieve host-bound allocation: %v", err)
	}
	if !retrieved.Replayed() {
		t.Fatal("same-workflow retrieval did not report replay")
	}
	if !first.Closure().Equal(retrieved.Closure()) || participantCalls != 1 || participantChildren != 2 {
		t.Fatalf("retrieval changed receipt or reran participant: calls=%d", participantCalls)
	}
	legacyRequest := composedGovernedRequest("bound-legacy-one", actor, rootBinding, 1)
	legacy, err := bound.RunAllocateComposed(ctx, "bound-legacy-one-workflow", rootBinding.AssignmentRow.JournalID, legacyRequest)
	if err != nil {
		t.Fatalf("run bound legacy one-child wrapper: %v", err)
	}
	batchRequest := composedGovernedRequest("bound-batch-one", actor, rootBinding, 1)
	batch, err := bound.RunAllocateComposedBatch(ctx, "bound-batch-one-workflow", rootBinding.AssignmentRow.JournalID, batchRequest)
	if err != nil {
		t.Fatalf("run bound batch one-child path: %v", err)
	}
	if len(legacy.Closure().Children()) != 1 || len(batch.Closure().Children()) != 1 {
		t.Fatalf("bound one-child parity failed: legacy=%d batch=%d", len(legacy.Closure().Children()), len(batch.Closure().Children()))
	}

	var participantRows, completionTables int
	if err := setupDB.QueryRow(`SELECT COUNT(*) FROM host_bound_participant WHERE operation_id=?1`, request.Allocation.OperationID).Scan(&participantRows); err != nil {
		t.Fatalf("query participant through exact host handle: %v", err)
	}
	if err := setupDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='transaction_completion'`).Scan(&completionTables); err != nil {
		t.Fatalf("query DBOS transaction table through exact host handle: %v", err)
	}
	if participantRows != 1 || completionTables != 0 {
		t.Fatalf("host handle state participant=%d transaction_completion tables=%d, want 1 and 0 (DBOS uses the host data source without creating a separate transaction table)", participantRows, completionTables)
	}
	var attached int
	rows, err := setupDB.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatalf("list host databases: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var name, file string
		if err := rows.Scan(&sequence, &name, &file); err != nil {
			t.Fatalf("scan host database list: %v", err)
		}
		if name != "temp" {
			attached++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate host database list: %v", err)
	}
	if attached != 1 {
		t.Fatal(fmt.Sprintf("host system handle has %d non-temp databases, want only the exact main database", attached))
	}
}

func TestHostBoundGovernedAllocatorBorrowsEngineLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participantCalls := 0
	participant := provenance.GovernedAllocationParticipant(func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		participantCalls++
		if len(closure.Children()) != 2 {
			t.Fatalf("host-borrowed participant closure children=%d, want 2", len(closure.Children()))
		}
		_, err := tx.Exec(ctx, `INSERT INTO engine_owned_participant(operation_id, child_count) VALUES (?1,?2)`, request.OperationID, len(closure.Children()))
		return err
	})
	dsn := "file:" + filepath.Join(t.TempDir(), "engine-owned.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE engine_owned_participant(operation_id TEXT PRIMARY KEY, child_count INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	root, err := dbos.NewDBOSContext(ctx, dbos.Config{AppName: "engine-owned", ApplicationVersion: "test-v1", SqliteSystemDB: db, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := provenance.NewHostBoundGovernedAllocator(ctx, root, db, participant)
	if err != nil {
		t.Fatalf("construct borrowed Provenance runner: %v", err)
	}
	tracker, err := provenance.OpenBorrowedSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	actor := registerGovernedActor(t, tracker, "engine-owned")
	t.Cleanup(func() { _ = tracker.Close() })
	// Only the engine launches and shuts down its root. The runner deliberately
	// exposes neither lifecycle operation.
	if err := dbos.Launch(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbos.Shutdown(root, 30*time.Second) })
	closure, err := runner.RunInitializeRoot(ctx, "engine-owned-genesis-workflow", provenance.RootGenesisRequest{
		OperationID: "engine-owned-genesis", ActorID: actor, Command: "test.genesis", Root: governedChild("engine-owned-root", actor),
	})
	if err != nil {
		t.Fatalf("run borrowed workflow on engine root: %v", err)
	}
	if _, ok := closure.Root(); !ok {
		t.Fatal("borrowed runner returned no root closure")
	}
	rootBinding, _ := closure.Root()
	request := composedGovernedRequest("engine-owned-batch", actor, rootBinding, 2)
	committed, err := runner.RunAllocateComposedBatch(ctx, "engine-owned-batch-w1", rootBinding.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run host-borrowed two-child batch: %v", err)
	}
	if len(committed.Closure().Children()) != 2 || len(committed.SupplementalEmittedEvents()) == 0 {
		t.Fatalf("host commit omitted ordered children or supplement: children=%d supplement=%d", len(committed.Closure().Children()), len(committed.SupplementalEmittedEvents()))
	}
	replayed, err := runner.RunAllocateComposedBatch(ctx, "engine-owned-batch-w2", rootBinding.AssignmentRow.JournalID, request)
	if err != nil || !replayed.Replayed() || !committed.Closure().Equal(replayed.Closure()) || participantCalls != 1 {
		t.Fatalf("host replay: replayed=%v participant calls=%d err=%v", replayed.Replayed(), participantCalls, err)
	}
	var participantRows, childCount, successfulCheckpoint int
	if err := db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(child_count),0) FROM engine_owned_participant WHERE operation_id=?1`, request.Allocation.OperationID).Scan(&participantRows, &childCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid IN (?1,?2) AND error IS NULL`, "engine-owned-batch-w1", "engine-owned-batch-w2").Scan(&successfulCheckpoint); err != nil {
		t.Fatal(err)
	}
	if participantRows != 1 || childCount != 2 || successfulCheckpoint == 0 {
		t.Fatalf("host durable proof participant rows=%d children=%d successful checkpoints=%d", participantRows, childCount, successfulCheckpoint)
	}

	// Global OperationID occupancy is classified before composed reference
	// preflight.  Keep the old child's task reference stale so this specifically
	// proves that a valid generic owner wins over reference diagnostics.
	for _, test := range []struct {
		name                       string
		operation                  provenance.OperationID
		malform                    bool
		deleteUnslottedProducedRow bool
		want                       provenance.GovernedAllocationErrorKind
	}{
		{name: "valid generic receipt", operation: "engine-owned-generic-collision", want: provenance.GovernedAllocationConflict},
		{name: "malformed generic receipt", operation: "engine-owned-malformed-generic", malform: true, want: provenance.GovernedAllocationCorruption},
		{name: "incomplete generic receipt closure", operation: "engine-owned-incomplete-generic", deleteUnslottedProducedRow: true, want: provenance.GovernedAllocationCorruption},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := rootBinding.AssignmentRow.JournalID
			generic, applyErr := tracker.Journal().Apply(provenance.OperationInput{
				OperationID: test.operation, ActorID: actor, AuthorityJournalID: &authority,
				CommandDigest: []byte("engine-owned-generic"), Effects: []provenance.Effect{{
					Sort: provenance.EffectTaskEvent, TaskID: rootBinding.TaskID, EventKind: "engine-owned.generic",
				}},
			})
			if applyErr != nil {
				t.Fatalf("persist generic collision owner: %v", applyErr)
			}
			if test.malform {
				if _, updateErr := db.Exec(`UPDATE journal_operations SET canonical_mutation=x'01' WHERE journal_id=?1`, generic.AnchorJournalID); updateErr != nil {
					t.Fatalf("malform generic collision owner: %v", updateErr)
				}
			}
			if test.deleteUnslottedProducedRow {
				// Foreign-key relaxation is connection-local in SQLite. Pin exactly
				// this test connection so no pooled caller can observe it disabled.
				conn, connErr := db.Conn(ctx)
				if connErr != nil {
					t.Fatalf("pin generic receipt tamper connection: %v", connErr)
				}
				if _, pragmaErr := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); pragmaErr != nil {
					_ = conn.Close()
					t.Fatalf("disable foreign keys on pinned tamper connection: %v", pragmaErr)
				}
				if _, deleteErr := conn.ExecContext(ctx, `DELETE FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id NOT IN (SELECT produced_journal_id FROM journal_operation_result_slots WHERE journal_id=?1)`, generic.AnchorJournalID); deleteErr != nil {
					_ = conn.Close()
					t.Fatalf("delete un-slotted generic produced row: %v", deleteErr)
				}
				if _, pragmaErr := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); pragmaErr != nil {
					_ = conn.Close()
					t.Fatalf("restore foreign keys on pinned tamper connection: %v", pragmaErr)
				}
				if closeErr := conn.Close(); closeErr != nil {
					t.Fatalf("release pinned generic receipt tamper connection: %v", closeErr)
				}
			}
			collision := request
			collision.Allocation.OperationID = test.operation
			collision.Allocation.Children[0].Title = "changed while retaining stale supplemental reference"
			beforeCalls := participantCalls
			beforeOutputs := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`)
			beforeGoverned := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM governed_allocation_operations`)
			_, collisionErr := runner.RunAllocateComposedBatch(ctx, "engine-owned-collision-"+test.name, rootBinding.AssignmentRow.JournalID, collision)
			var governedErr *provenance.GovernedAllocationError
			if !errors.As(collisionErr, &governedErr) || governedErr.Kind != test.want {
				t.Fatalf("generic collision error=%v, want typed kind %v", collisionErr, test.want)
			}
			if participantCalls != beforeCalls {
				t.Fatalf("generic collision reran participant: before=%d after=%d", beforeCalls, participantCalls)
			}
			if after := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`); after != beforeOutputs {
				t.Fatalf("identity classification entered DBOS: outputs before=%d after=%d", beforeOutputs, after)
			}
			if after := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM governed_allocation_operations`); after != beforeGoverned {
				t.Fatalf("identity classification wrote governed domain state: before=%d after=%d", beforeGoverned, after)
			}
		})
	}
}

func TestBoundGovernedAllocatorReopenReplaySuppressesParticipant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "bound-reopen.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	calls, children := 0, 0
	participant := func(_ context.Context, _ provenance.GovernedAllocationTransaction, _ provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		calls++
		children = len(closure.Children())
		return nil
	}
	config := provenance.FusedGovernedAllocatorConfig{SQLiteDSN: dsn, AppName: "bound-reopen", ApplicationVersion: "test-v1", Logger: slog.Default(), Participant: participant}
	first, err := provenance.OpenBoundGovernedAllocator(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	actor := registerGovernedActor(t, first.Tracker(), "bound-reopen")
	if err := first.Launch(); err != nil {
		t.Fatal(err)
	}
	rootClosure, err := first.RunInitializeRoot(ctx, "bound-reopen-root-workflow", provenance.RootGenesisRequest{OperationID: "bound-reopen-root", ActorID: actor, Command: "test.genesis", Root: governedChild("bound-reopen-root", actor)})
	if err != nil {
		t.Fatal(err)
	}
	root, ok := rootClosure.Root()
	if !ok {
		t.Fatal("missing bound root")
	}
	request := composedGovernedRequest("bound-reopen-allocation", actor, root, 3)
	committed, err := first.RunAllocateComposedBatch(ctx, "bound-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || children != 3 {
		t.Fatalf("participant calls=%d children=%d", calls, children)
	}
	if err := first.Close(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	reopened, err := provenance.OpenBoundGovernedAllocator(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(30 * time.Second) })
	if err := reopened.Launch(); err != nil {
		t.Fatal(err)
	}
	replay, err := reopened.RunAllocateComposedBatch(ctx, "bound-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed() || !committed.Closure().Equal(replay.Closure()) || calls != 1 || children != 3 {
		t.Fatalf("reopen replay reran or changed participant closure: replay=%v calls=%d children=%d", replay.Replayed(), calls, children)
	}
}
