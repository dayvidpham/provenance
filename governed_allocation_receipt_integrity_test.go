package provenance_test

import (
	"context"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// each test owns a private t.TempDir database and tampers only with its own rows.

func TestGovernedAllocationReceiptRejectsCanonicalTaskProjectionTampering(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participantCalls := 0
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "allocation-receipt-task-integrity", func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		participantCalls++
		return nil
	})
	actor := registerGovernedActor(t, fused.Tracker(), "allocation-receipt-task-integrity")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "allocation-receipt-task-integrity-root")
	request := governedRequest("allocation-receipt-task-integrity", actor, root.AssignmentID, 1)
	if _, err := fused.RunAllocate(ctx, "allocation-receipt-task-integrity-original", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("commit allocation fixture: %v", err)
	}
	child := request.Children[0]
	if _, err := db.Exec(`UPDATE tasks SET title='tampered',status_id=1,owner_id=NULL WHERE id=?1`, child.TaskID.String()); err != nil {
		t.Fatalf("tamper task projection: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_attributions SET first_journal_id=(SELECT last_journal_id FROM tasks WHERE id=?1) WHERE task_id=?1 AND actor_id=?2`, child.TaskID.String(), actor.String()); err != nil {
		t.Fatalf("tamper task attribution watermark: %v", err)
	}
	before := snapshotGovernedTables(t, db)
	result, err := fused.RunAllocate(ctx, "allocation-receipt-task-integrity-retry", root.AssignmentRow.JournalID, request)
	mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	assertEmptyClosure(t, result)
	if participantCalls != 1 {
		t.Fatalf("corrupt replay participant calls=%d, want 1", participantCalls)
	}
	assertNoGovernedWrites(t, before, db)
}

func TestComposedConflictProofRejectsMutatedAuthorityOwnerAndSupplement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participantCalls := 0
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "allocation-conflict-proof-integrity", func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		participantCalls++
		return nil
	})
	actor := registerGovernedActor(t, fused.Tracker(), "allocation-conflict-proof-integrity")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "allocation-conflict-proof-integrity-root")
	request := composedGovernedRequest("allocation-conflict-proof-integrity", actor, root, 1)
	if _, err := fused.RunAllocateComposed(ctx, "allocation-conflict-proof-integrity-original", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("commit composed fixture: %v", err)
	}
	if _, err := db.Exec(`UPDATE journal_operations SET authority_journal_id=(SELECT MAX(journal_id) FROM journal_authorities) WHERE operation_id=?1`, string(request.Allocation.OperationID)); err != nil {
		t.Fatalf("tamper allocation authority: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM governed_composed_supplement_owners WHERE governed_operation_id=?1`, string(request.Allocation.OperationID)); err != nil {
		t.Fatalf("remove composed owner: %v", err)
	}
	if _, err := db.Exec(`UPDATE journal_task_events SET payload='{"tampered":true}' WHERE event_kind='provenance.slice.created'`); err != nil {
		t.Fatalf("tamper composed supplement: %v", err)
	}
	changed := composedGovernedRequest("allocation-conflict-proof-integrity", actor, root, 1)
	changed.Allocation.Children[0].Title = "changed request"
	before := snapshotGovernedTables(t, db)
	result, err := fused.RunAllocateComposed(ctx, "allocation-conflict-proof-integrity-conflict", root.AssignmentRow.JournalID, changed)
	mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	if len(result.Closure().Children()) != 0 || len(result.SupplementalResultSlots()) != 0 || len(result.SupplementalEmittedEvents()) != 0 {
		t.Fatalf("corrupt conflict proof returned a receipt: %+v", result)
	}
	if participantCalls != 1 {
		t.Fatalf("corrupt conflict proof participant calls=%d, want 1", participantCalls)
	}
	assertNoGovernedWrites(t, before, db)
}
