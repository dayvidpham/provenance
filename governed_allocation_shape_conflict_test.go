package provenance_test

import (
	"context"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// each test owns a private t.TempDir database; its subtests share only that one
// test's allocator and run serially within it.

func TestComposedGovernedAllocationOperationShapeConflictsBeforeOwnerMarker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "composed-shape-conflict")
	actor := registerGovernedActor(t, fused.Tracker(), "composed-shape-conflict")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	rootName := "composed-shape-conflict-root"
	root := initializeFusedRoot(t, fused, actor, rootName)

	t.Run("simple allocation reused by composed request", func(t *testing.T) {
		request := composedGovernedRequest("simple-then-composed", actor, root, 1)
		if _, err := fused.RunAllocate(ctx, "shape-simple-workflow", root.AssignmentRow.JournalID, request.Allocation); err != nil {
			t.Fatalf("persist simple allocation: %v", err)
		}
		before := snapshotGovernedTables(t, db)
		_, err := fused.RunAllocateComposed(ctx, "shape-simple-composed-workflow", root.AssignmentRow.JournalID, request)
		mustGovernedError(t, err, provenance.GovernedAllocationConflict)
		assertNoGovernedWrites(t, before, db)
	})

	t.Run("genesis reused by composed request", func(t *testing.T) {
		request := composedGovernedRequest(rootName+"-genesis", actor, root, 1)
		before := snapshotGovernedTables(t, db)
		_, err := fused.RunAllocateComposed(ctx, "shape-genesis-composed-workflow", root.AssignmentRow.JournalID, request)
		mustGovernedError(t, err, provenance.GovernedAllocationConflict)
		assertNoGovernedWrites(t, before, db)
	})
}

func TestComposedGovernedAllocationExactReceiptMissingOwnerMarkerIsCorruption(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "composed-missing-owner")
	actor := registerGovernedActor(t, fused.Tracker(), "composed-missing-owner")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-missing-owner-root")
	request := composedGovernedRequest("composed-missing-owner-operation", actor, root, 1)
	if _, err := fused.RunAllocateComposed(ctx, "missing-owner-first-workflow", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("persist composed allocation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM governed_composed_supplement_owners WHERE governed_operation_id=?1`, string(request.Allocation.OperationID)); err != nil {
		t.Fatalf("remove composed owner marker: %v", err)
	}

	before := snapshotGovernedTables(t, db)
	_, err := fused.RunAllocateComposed(ctx, "missing-owner-replay-workflow", root.AssignmentRow.JournalID, request)
	mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	assertNoGovernedWrites(t, before, db)
}
