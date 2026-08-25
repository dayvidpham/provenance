package provenance_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/dayvidpham/provenance"
)

const acyclicAuthorityProofDepth = 66

func TestGovernedAllocationAcceptsDeepAcyclicAncestry(t *testing.T) {
	ctx := context.Background()

	t.Run("standalone", func(t *testing.T) {
		tracker, actor := openGovernedTracker(t)
		parent := initializeRoot(t, tracker, actor)
		for depth := 2; depth <= acyclicAuthorityProofDepth; depth++ {
			closure, err := tracker.As(actor, parent.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest(fmt.Sprintf("standalone-depth-%d", depth), actor, parent.AssignmentID, 1))
			if err != nil {
				t.Fatalf("allocate at depth %d: %v", depth, err)
			}
			parent = closure.Children()[0]
		}
		if got := taskCount(t, tracker); got != acyclicAuthorityProofDepth {
			t.Fatalf("standalone task count = %d, want %d after deep acyclic allocation", got, acyclicAuthorityProofDepth)
		}
	})

	t.Run("fused composed reference scope", func(t *testing.T) {
		fused, db := openFusedAllocatorWithDatabase(t, "deep-authority")
		actor := registerGovernedActor(t, fused.Tracker(), "deep-authority")
		if err := fused.Launch(); err != nil {
			t.Fatalf("launch fused allocator: %v", err)
		}
		root := initializeFusedRoot(t, fused, actor, "deep-authority")
		parent := root
		for depth := 2; depth <= acyclicAuthorityProofDepth; depth++ {
			// Session.AllocateGoverned is the standalone production path that
			// creates the ancestry consumed by the final DBOS workflow.
			closure, err := fused.Tracker().As(actor, parent.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest(fmt.Sprintf("fused-depth-%d", depth), actor, parent.AssignmentID, 1))
			if err != nil {
				t.Fatalf("seed fused ancestry at depth %d: %v", depth, err)
			}
			parent = closure.Children()[0]
		}

		deepParentRequest := composedGovernedRequest("fused-deep-parent", actor, parent, 1)
		deepParentResult, err := fused.RunAllocateComposedBatch(ctx, "fused-deep-parent-workflow", parent.AssignmentRow.JournalID, deepParentRequest)
		if err != nil {
			t.Fatalf("run composed allocation below deep parent: %v", err)
		}
		if got := len(deepParentResult.Closure().Children()); got != 1 {
			t.Fatalf("composed deep-parent allocation children = %d, want 1", got)
		}
		if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, "fused-deep-parent-workflow"); got != 1 {
			t.Fatalf("composed deep-parent allocation successful outputs = %d, want 1", got)
		}

		referenceRequest := composedGovernedRequest("fused-deep-reference", actor, root, 1)
		referenceRequest.ReferenceScope = provenance.GovernedAllocationReferenceScope{
			Kind: provenance.GovernedAllocationReferenceDescendants, Subjects: []provenance.TaskID{parent.TaskID},
		}
		referenceRequest.SupplementalEffects = []provenance.Effect{{
			Sort: provenance.EffectTaskEvent, TaskID: parent.TaskID, EventKind: "provenance.authority.depth.checked",
		}}
		committed, err := fused.RunAllocateComposedBatch(ctx, "fused-deep-reference-workflow", root.AssignmentRow.JournalID, referenceRequest)
		if err != nil {
			t.Fatalf("run composed allocation with deep reference scope: %v", err)
		}
		if got := len(committed.Closure().Children()); got != 1 {
			t.Fatalf("composed deep allocation children = %d, want 1", got)
		}
		if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, "fused-deep-reference-workflow"); got != 1 {
			t.Fatalf("composed deep allocation successful outputs = %d, want 1", got)
		}
	})
}

func TestGovernedAllocationRejectsCyclicAncestryWithoutWrites(t *testing.T) {
	ctx := context.Background()
	tracker, actor, db := openGovernedTrackerWithDatabase(t)
	root := initializeRoot(t, tracker, actor)
	childClosure, err := tracker.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("cyclic-parent-child", actor, root.AssignmentID, 1))
	if err != nil {
		t.Fatalf("create child before corruption: %v", err)
	}
	child := childClosure.Children()[0]
	if _, err := db.Exec(`UPDATE journal_authority_assignment_episodes SET parent_assignment_id=?1 WHERE assignment_id=?2`, child.AssignmentID, root.AssignmentID); err != nil {
		t.Fatalf("seed cyclic parent ancestry: %v", err)
	}

	before := snapshotGovernedTables(t, db)
	request := governedRequest("cyclic-parent-rejection", actor, child.AssignmentID, 1)
	_, err = tracker.As(actor, child.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
	mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	assertNoGovernedWrites(t, before, db)
	assertGovernedOperationAbsent(t, tracker, request.OperationID)
}
