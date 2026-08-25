package provenance_test

import (
	"context"
	"errors"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

// TestGovernedAllocationV1ReducerParity proves the Session admission path
// rejects a request exactly as the DBOS-owned path does, for the same reason
// and with the same absence of writes.
//
// It used to also re-run TestFusedGovernedAllocationComposedPersistsAllowedSupplementsAndReplays
// and TestSessionAllocateGovernedComposedUsesSameReducer as subtests. Both are
// ordinary top-level tests in this package, so the suite already runs them; the
// second invocation proved nothing new and duplicated roughly three seconds of
// race-detector time on every run.
func TestGovernedAllocationV1ReducerParity(t *testing.T) {
	ctx := context.Background()

	t.Run("zero authority", func(t *testing.T) {
		tr, actor, db := openGovernedTrackerWithDatabase(t)
		root := initializeRoot(t, tr, actor)
		simple := governedRequest("parity-zero-simple", actor, root.AssignmentID, 1)
		composed := composedGovernedRequest("parity-zero-composed", actor, root, 1)
		before := snapshotGovernedTables(t, db)
		_, simpleErr := tr.As(actor, 0).AllocateGoverned(ctx, simple)
		assertNoGovernedWrites(t, before, db)
		_, composedErr := tr.As(actor, 0).AllocateGovernedComposed(ctx, composed)
		assertNoGovernedWrites(t, before, db)
		assertMatchingGovernedFailure(t, simpleErr, composedErr, provenance.GovernedAllocationAuthority)
	})

	t.Run("wrong authority", func(t *testing.T) {
		tr, actor, db := openGovernedTrackerWithDatabase(t)
		root := initializeRoot(t, tr, actor)
		side, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("parity-wrong-side", actor, root.AssignmentID, 1))
		if err != nil {
			t.Fatal(err)
		}
		wrong := side.Children()[0].AssignmentRow.JournalID
		before := snapshotGovernedTables(t, db)
		_, simpleErr := tr.As(actor, wrong).AllocateGoverned(ctx, governedRequest("parity-wrong-simple", actor, root.AssignmentID, 1))
		assertNoGovernedWrites(t, before, db)
		_, composedErr := tr.As(actor, wrong).AllocateGovernedComposed(ctx, composedGovernedRequest("parity-wrong-composed", actor, root, 1))
		assertNoGovernedWrites(t, before, db)
		assertMatchingGovernedFailure(t, simpleErr, composedErr, provenance.GovernedAllocationAuthority)
	})

	t.Run("revoked authority", func(t *testing.T) {
		tr, actor, db := openGovernedTrackerWithDatabase(t)
		root := initializeRoot(t, tr, actor)
		if _, err := tr.Journal().Apply(provenance.OperationInput{
			OperationID: "parity-revoke-root", ActorID: actor, AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
			CommandDigest: []byte("parity-revoke-root"), MutationDigest: []byte("parity-revoke-root"),
			Effects: []provenance.Effect{{Sort: provenance.EffectAssignmentEnd, AssignmentID: root.AssignmentID, TaskID: root.TaskID, SlotID: provenance.SlotOwnerResponsibility}},
		}); err != nil {
			t.Fatal(err)
		}
		before := snapshotGovernedTables(t, db)
		_, simpleErr := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("parity-revoked-simple", actor, root.AssignmentID, 1))
		assertNoGovernedWrites(t, before, db)
		_, composedErr := tr.As(actor, root.AssignmentRow.JournalID).AllocateGovernedComposed(ctx, composedGovernedRequest("parity-revoked-composed", actor, root, 1))
		assertNoGovernedWrites(t, before, db)
		assertMatchingGovernedFailure(t, simpleErr, composedErr, provenance.GovernedAllocationRevoked)
	})

	t.Run("child collision", func(t *testing.T) {
		tr, actor, db := openGovernedTrackerWithDatabase(t)
		root := initializeRoot(t, tr, actor)
		composed := composedGovernedRequest("parity-collision-composed", actor, root, 1)
		collision := composed.Allocation.Children[0]
		seed := governedRequest("parity-collision-seed", actor, root.AssignmentID, 1)
		seed.Children[0] = collision
		if _, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, seed); err != nil {
			t.Fatal(err)
		}
		simple := governedRequest("parity-collision-simple", actor, root.AssignmentID, 1)
		simple.Children[0] = collision
		before := snapshotGovernedTables(t, db)
		_, simpleErr := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, simple)
		assertNoGovernedWrites(t, before, db)
		_, composedErr := tr.As(actor, root.AssignmentRow.JournalID).AllocateGovernedComposed(ctx, composed)
		assertNoGovernedWrites(t, before, db)
		assertMatchingGovernedFailure(t, simpleErr, composedErr, provenance.GovernedAllocationCollision)
	})

	t.Run("replay after revocation", func(t *testing.T) {
		tr, actor, db := openGovernedTrackerWithDatabase(t)
		root := initializeRoot(t, tr, actor)
		simple := governedRequest("parity-replay-simple", actor, root.AssignmentID, 1)
		composed := composedGovernedRequest("parity-replay-composed", actor, root, 1)
		firstSimple, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, simple)
		if err != nil {
			t.Fatal(err)
		}
		firstComposed, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGovernedComposed(ctx, composed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Journal().Apply(provenance.OperationInput{
			OperationID: "parity-replay-revoke", ActorID: actor, AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
			CommandDigest: []byte("parity-replay-revoke"), MutationDigest: []byte("parity-replay-revoke"),
			Effects: []provenance.Effect{{Sort: provenance.EffectAssignmentEnd, AssignmentID: root.AssignmentID, TaskID: root.TaskID, SlotID: provenance.SlotOwnerResponsibility}},
		}); err != nil {
			t.Fatal(err)
		}
		before := snapshotGovernedTables(t, db)
		replayedSimple, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, simple)
		if err != nil {
			t.Fatal(err)
		}
		assertNoGovernedWrites(t, before, db)
		replayedComposed, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGovernedComposed(ctx, composed)
		if err != nil {
			t.Fatal(err)
		}
		assertNoGovernedWrites(t, before, db)
		assertSameClosure(t, replayedSimple, firstSimple)
		assertSameClosure(t, replayedComposed.Closure(), firstComposed.Closure())
	})
}

func assertMatchingGovernedFailure(t *testing.T, simpleErr, composedErr error, want provenance.GovernedAllocationErrorKind) {
	t.Helper()
	var simple, composed *provenance.GovernedAllocationError
	if !errors.As(simpleErr, &simple) || !errors.As(composedErr, &composed) {
		t.Fatalf("non-governed parity errors: simple=%v composed=%v", simpleErr, composedErr)
	}
	if simple.Kind != want || composed.Kind != want || simple.Where != composed.Where {
		t.Fatalf("failure parity mismatch: simple=(%s,%q) composed=(%s,%q), want kind %s", simple.Kind, simple.Where, composed.Kind, composed.Where, want)
	}
}

func TestGovernedAllocationV1CloseGateMatchesOrdinaryReducer(t *testing.T) {
	fused, db := openFusedAllocatorWithDatabase(t, "v1-close-gate")
	actor := registerGovernedActor(t, fused.Tracker(), "v1-close-gate")
	if err := fused.Launch(); err != nil {
		t.Fatal(err)
	}
	root := initializeFusedRoot(t, fused, actor, "v1-close-gate-root")
	_, ordinaryErr := fused.Tracker().Journal().Apply(provenance.OperationInput{
		OperationID: "v1-close-gate-ordinary", ActorID: actor,
		AuthorityJournalID: &root.AssignmentRow.JournalID,
		CommandDigest:      []byte("v1-close-gate-ordinary"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectTaskEvent, TaskID: root.TaskID,
			EventKind: provenance.EventKindTaskClosed, CloseReason: "parity gate",
		}},
	})
	if !errors.Is(ordinaryErr, provenance.ErrCloseWithoutEnding) {
		t.Fatalf("ordinary close with active seeded owner error=%v, want ErrCloseWithoutEnding", ordinaryErr)
	}
	request := composedGovernedRequest("v1-close-gate", actor, root, 1)
	child := request.Allocation.Children[0]
	request.SupplementalEffects = append(request.SupplementalEffects, provenance.Effect{
		Sort: provenance.EffectTaskEvent, TaskID: child.TaskID,
		EventKind: provenance.EventKindTaskClosed, CloseReason: "parity gate",
	})
	_, err := fused.RunAllocateComposed(context.Background(), "v1-close-gate-workflow", root.AssignmentRow.JournalID, request)
	if !errors.Is(err, provenance.ErrCloseWithoutEnding) {
		t.Fatalf("close with active owner error=%v, want ErrCloseWithoutEnding", err)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM governed_allocation_operations WHERE operation_id=?1`, request.Allocation.OperationID); got != 0 {
		t.Fatalf("close-gate rejection retained allocation receipt: %d", got)
	}
}
