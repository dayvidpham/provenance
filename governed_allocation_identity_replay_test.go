package provenance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

func TestGovernedPublicIngressRejectsReservedOperationIDsBeforeDBOS(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "reserved-governed-ingress")
	actor := registerGovernedActor(t, fused.Tracker(), "reserved-governed-ingress")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "reserved-governed-ingress-root")

	for _, test := range []struct {
		name       string
		workflowID string
		run        func(provenance.OperationID) error
	}{
		{
			name: "simple allocation", workflowID: "reserved-simple-workflow",
			run: func(id provenance.OperationID) error {
				request := governedRequest(string(id), actor, root.AssignmentID, 1)
				_, err := fused.RunAllocate(ctx, "reserved-simple-workflow", root.AssignmentRow.JournalID, request)
				return err
			},
		},
		{
			name: "composed allocation", workflowID: "reserved-composed-workflow",
			run: func(id provenance.OperationID) error {
				request := composedGovernedRequest(string(id), actor, root, 1)
				_, err := fused.RunAllocateComposed(ctx, "reserved-composed-workflow", root.AssignmentRow.JournalID, request)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := composedSupplementOperationIDForTest(provenance.OperationID("caller-" + strings.ReplaceAll(test.name, " ", "-")))
			before := snapshotGovernedTables(t, db)
			if err := test.run(id); err == nil {
				t.Fatalf("reserved operation %q was accepted", id)
			}
			assertNoGovernedWrites(t, before, db)
			if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, test.workflowID); got != 0 {
				t.Fatalf("reserved request entered DBOS: operation_outputs=%d, want 0", got)
			}
		})
	}
}

func TestSessionGovernedIngressRejectsReservedOperationIDsWithoutWrites(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "reserved-session-ingress")
	tracker := fused.Tracker()
	actor := registerGovernedActor(t, tracker, "reserved-session-ingress")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "reserved-session-ingress-root")
	session := tracker.As(actor, root.AssignmentRow.JournalID)

	for _, test := range []struct {
		name string
		run  func(provenance.OperationID) error
	}{
		{
			name: "simple allocation",
			run: func(id provenance.OperationID) error {
				_, err := session.AllocateGoverned(ctx, governedRequest(string(id), actor, root.AssignmentID, 1))
				return err
			},
		},
		{
			name: "composed allocation",
			run: func(id provenance.OperationID) error {
				request := composedGovernedRequest("reserved-session-composed", actor, root, 1)
				request.Allocation.OperationID = id
				_, err := session.AllocateGovernedComposed(ctx, request)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := composedSupplementOperationIDForTest(provenance.OperationID("session-" + strings.ReplaceAll(test.name, " ", "-")))
			before := snapshotGovernedTables(t, db)
			err := test.run(id)
			mustGovernedError(t, err, provenance.GovernedAllocationValidation)
			assertNoGovernedWrites(t, before, db)
		})
	}
}

func TestRunInitializeRootRejectsReservedOperationIDBeforeDBOS(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "reserved-root-ingress")
	actor := registerGovernedActor(t, fused.Tracker(), "reserved-root-ingress")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	workflowID := "reserved-root-workflow"
	reserved := composedSupplementOperationIDForTest("reserved-root-caller")
	before := snapshotGovernedTables(t, db)
	_, err := fused.RunInitializeRoot(ctx, workflowID, provenance.RootGenesisRequest{
		OperationID: reserved,
		ActorID:     actor,
		Command:     "test.genesis",
		Root:        governedChild("reserved-root", actor),
	})
	mustGovernedError(t, err, provenance.GovernedAllocationValidation)
	assertNoGovernedWrites(t, before, db)
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflowID); got != 0 {
		t.Fatalf("reserved root entered DBOS: operation_outputs=%d, want 0", got)
	}
}

func TestGenericReservedIdentityPreservesOnlyUnmarkedHistoricalReplay(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "reserved-generic-replay")
	tracker := fused.Tracker()
	actor := registerGovernedActor(t, tracker, "reserved-generic-replay")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "reserved-generic-replay-root")
	authority := root.AssignmentRow.JournalID
	input := provenance.OperationInput{
		OperationID:        "pre-upgrade-generic-operation",
		ActorID:            actor,
		AuthorityJournalID: &authority,
		CommandDigest:      []byte("pre-upgrade-command"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectTaskEvent, TaskID: root.TaskID, EventKind: "pre-upgrade.generic.event",
		}},
	}
	first, err := tracker.Journal().Apply(input)
	if err != nil {
		t.Fatalf("persist baseline-compatible generic operation: %v", err)
	}
	reserved := composedSupplementOperationIDForTest("historical-generic-owner")
	if _, err := db.ExecContext(ctx, `UPDATE journal_operations SET operation_id=?1 WHERE operation_id=?2`, string(reserved), string(input.OperationID)); err != nil {
		t.Fatalf("represent unmarked pre-upgrade reserved-prefix row: %v", err)
	}
	input.OperationID = reserved
	replayed, err := tracker.Journal().Apply(input)
	if err != nil {
		t.Fatalf("exact replay of unmarked pre-upgrade reserved-prefix row: %v", err)
	}
	if replayed.Kind != first.Kind || replayed.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("historical replay changed receipt: first=%+v replayed=%+v", first, replayed)
	}
	changed := input
	changed.CommandDigest = []byte("changed-command")
	beforeChanged := snapshotGovernedTables(t, db)
	beforeChangedOutputs := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`)
	if _, err := tracker.Journal().Apply(changed); err == nil {
		t.Fatal("changed historical reserved-prefix input exact-replayed")
	} else {
		var conflict *provenance.OperationConflict
		if !errors.Is(err, provenance.ErrOperationConflict) || !errors.As(err, &conflict) {
			t.Fatalf("changed historical replay error=%v, want ErrOperationConflict with *OperationConflict payload", err)
		}
	}
	assertNoGovernedWrites(t, beforeChanged, db)
	if after := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`); after != beforeChangedOutputs {
		t.Fatalf("changed historical replay wrote operation_outputs: before=%d after=%d", beforeChangedOutputs, after)
	}

	composed := composedGovernedRequest("owned-composed-operation", actor, root, 1)
	if _, err := fused.RunAllocateComposed(ctx, "owned-composed-workflow", authority, composed); err != nil {
		t.Fatalf("persist composed operation and durable owner marker: %v", err)
	}
	owned := input
	owned.OperationID = composedSupplementOperationIDForTest(composed.Allocation.OperationID)
	beforeOwned := snapshotGovernedTables(t, db)
	beforeOwnedOutputs := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`)
	if _, err := tracker.Journal().Apply(owned); !errors.Is(err, provenance.ErrOperationConflict) {
		t.Fatalf("composition-owned reserved identity error=%v, want ErrOperationConflict", err)
	} else {
		var conflict *provenance.OperationConflict
		if errors.As(err, &conflict) {
			t.Fatalf("composition-owned reserved identity exposed historical conflict payload: %+v", conflict)
		}
	}
	assertNoGovernedWrites(t, beforeOwned, db)
	if after := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`); after != beforeOwnedOutputs {
		t.Fatalf("composition-owned reserved identity wrote operation_outputs: before=%d after=%d", beforeOwnedOutputs, after)
	}
}

func TestDBOSAdapterReservedIdentityAdmission(t *testing.T) {
	ctx := context.Background()
	stack := newDBOSStack(t, nil)
	input := provenance.OperationInput{
		OperationID:        "dbos-historical-generic",
		ActorID:            stack.actor,
		AuthorityJournalID: ptr(stack.boot),
		CommandDigest:      []byte("dbos-historical-command"),
		Effects: []provenance.Effect{{
			Sort:      provenance.EffectTaskEvent,
			EventKind: "dbos.historical.generic",
		}},
	}
	task, err := stack.tracker.As(stack.actor, stack.boot).Create(
		"dbos-reserved", "DBOS reserved admission", "historical replay fixture",
		provenance.TaskTypeTask, provenance.PriorityMedium, provenance.PhaseWorkerSlices,
		provenance.WithOperationID("dbos-reserved-task-create"),
	)
	if err != nil {
		t.Fatalf("create DBOS historical target task: %v", err)
	}
	input.Effects[0].TaskID = task.ID
	first, err := stack.tracker.Journal().Apply(input)
	if err != nil {
		t.Fatalf("persist DBOS historical baseline: %v", err)
	}
	reserved := composedSupplementOperationIDForTest("dbos-historical-owner")
	if _, err := stack.db.ExecContext(ctx, `UPDATE journal_operations SET operation_id=?1 WHERE operation_id=?2`, string(reserved), string(input.OperationID)); err != nil {
		t.Fatalf("represent unmarked DBOS historical reserved-prefix row: %v", err)
	}
	input.OperationID = reserved
	replayed, err := stack.adapter.Apply(ctx, input)
	if err != nil {
		t.Fatalf("DBOS exact historical replay: %v", err)
	}
	if replayed.Kind != first.Kind || replayed.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("DBOS historical replay changed receipt: first=%+v replayed=%+v", first, replayed)
	}

	ownedExternal := provenance.OperationID("dbos-composition-owner")
	ownedReserved := composedSupplementOperationIDForTest(ownedExternal)
	conn, err := stack.db.Conn(ctx)
	if err != nil {
		t.Fatalf("lease DBOS owner-marker fixture connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("disable fixture foreign keys: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO governed_composed_supplement_owners (supplement_operation_id,governed_operation_id) VALUES (?1,?2)`, string(ownedReserved), string(ownedExternal)); err != nil {
		t.Fatalf("represent durable DBOS composition owner marker: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("restore fixture foreign keys: %v", err)
	}
	for _, test := range []struct {
		name string
		id   provenance.OperationID
	}{
		{name: "fresh", id: composedSupplementOperationIDForTest("dbos-fresh-owner")},
		{name: "composition owned", id: ownedReserved},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			candidate.OperationID = test.id
			before := snapshotGovernedTables(t, stack.db)
			beforeOutputs := countFusedGovernedRows(t, stack.db, `SELECT COUNT(*) FROM operation_outputs`)
			_, err := stack.adapter.Apply(ctx, candidate)
			if !errors.Is(err, provenance.ErrOperationConflict) {
				t.Fatalf("reserved DBOS admission error=%v, want ErrOperationConflict", err)
			}
			var conflict *provenance.OperationConflict
			if errors.As(err, &conflict) {
				t.Fatalf("reserved DBOS admission exposed historical conflict payload: %+v", conflict)
			}
			assertNoGovernedWrites(t, before, stack.db)
			if after := countFusedGovernedRows(t, stack.db, `SELECT COUNT(*) FROM operation_outputs`); after != beforeOutputs {
				t.Fatalf("reserved DBOS admission wrote operation_outputs: before=%d after=%d", beforeOutputs, after)
			}
		})
	}
}
