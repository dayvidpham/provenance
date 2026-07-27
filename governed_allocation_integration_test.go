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

	"github.com/dayvidpham/provenance"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// These integration tests exercise the public governed-allocation API against
// real Modernc SQLite and, below, the exact-handle DBOS transaction path. They
// intentionally use fixed caller identities so exact retries and reopen results
// compare byte-for-byte by their immutable closure value.

func TestGovernedGenesisRetryAndConflictingSecondGenesis(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := governedChild("root", actor)
	req := provenance.RootGenesisRequest{OperationID: "governed-genesis", ActorID: actor, Command: "test.genesis", Root: root}

	first, err := tr.InitializeGovernedRoot(ctx, req)
	if err != nil {
		t.Fatalf("initialize root: %v", err)
	}
	retry, err := tr.InitializeGovernedRoot(ctx, req)
	if err != nil {
		t.Fatalf("retry root: %v", err)
	}
	if !first.Equal(retry) {
		t.Fatalf("genesis retry closure differs: first=%+v retry=%+v", first, retry)
	}
	if _, ok := first.Root(); !ok {
		t.Fatal("genesis closure has no single root binding")
	}

	changed := req
	changed.OperationID = "different-governed-genesis"
	changed.Root = governedChild("second-root", actor)
	_, err = tr.InitializeGovernedRoot(ctx, changed)
	mustGovernedError(t, err, provenance.GovernedAllocationGenesis)
	if got := taskCount(t, tr); got != 1 {
		t.Fatalf("different genesis wrote %d tasks, want one root only", got)
	}
}

func TestGovernedAllocationBatchBoundariesRetriesAndOrder(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)

	for _, count := range []int{1, provenance.MaxGovernedAllocationChildren} {
		t.Run(fmt.Sprintf("%d children", count), func(t *testing.T) {
			request := governedRequest(fmt.Sprintf("batch-%d", count), actor, root.AssignmentID, count)
			first, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
			if err != nil {
				t.Fatalf("allocate %d: %v", count, err)
			}
			children := first.Children()
			if len(children) != count {
				t.Fatalf("closure children = %d, want %d", len(children), count)
			}
			for i, child := range children {
				if child.Ordinal != i || child.TaskID != request.Children[i].TaskID || child.AssignmentID != request.Children[i].AssignmentID {
					t.Fatalf("closure child %d = %+v, want submitted child %+v", i, child, request.Children[i])
				}
				if child.TaskRow.OperationID != request.OperationID || child.TaskRow.EffectOrdinal != i || child.TaskRow.Subordinal != 0 || child.AssignmentRow.OperationID != request.OperationID || child.AssignmentRow.EffectOrdinal != i || child.AssignmentRow.Subordinal != 1 {
					t.Fatalf("closure child %d has unstable produced-row positions: %+v", i, child)
				}
			}
			retry, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
			if err != nil {
				t.Fatalf("exact retry: %v", err)
			}
			if !first.Equal(retry) {
				t.Fatalf("exact retry closure differs: first=%+v retry=%+v", first, retry)
			}

			changed := request
			changed.Children = append([]provenance.GovernedChildSpec(nil), request.Children...)
			changed.Children[0].Title = "changed title"
			before := taskCount(t, tr)
			_, err = tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, changed)
			mustGovernedError(t, err, provenance.GovernedAllocationConflict)
			if got := taskCount(t, tr); got != before {
				t.Fatalf("changed retry wrote tasks: before=%d after=%d", before, got)
			}
			if count > 1 {
				reordered := request
				reordered.Children = append([]provenance.GovernedChildSpec(nil), request.Children...)
				reordered.Children[0], reordered.Children[1] = reordered.Children[1], reordered.Children[0]
				_, err = tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, reordered)
				mustGovernedError(t, err, provenance.GovernedAllocationConflict)
				if got := taskCount(t, tr); got != before {
					t.Fatalf("reordered retry wrote tasks: before=%d after=%d", before, got)
				}
			}
		})
	}
}

func TestGovernedAllocationRejectsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	valid := governedRequest("reject-base", actor, root.AssignmentID, 1)
	if _, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, valid); err != nil {
		t.Fatalf("seed allocation: %v", err)
	}

	for _, test := range []struct {
		name    string
		request provenance.GovernedAllocationRequest
		kind    provenance.GovernedAllocationErrorKind
	}{
		{
			name:    "129 children",
			request: governedRequest("too-many", actor, root.AssignmentID, provenance.MaxGovernedAllocationChildren+1),
			kind:    provenance.GovernedAllocationValidation,
		},
		{
			name: "duplicate task id",
			request: func() provenance.GovernedAllocationRequest {
				r := governedRequest("duplicate-task", actor, root.AssignmentID, 2)
				r.Children[1].TaskID = r.Children[0].TaskID
				return r
			}(),
			kind: provenance.GovernedAllocationValidation,
		},
		{
			name: "duplicate assignment id",
			request: func() provenance.GovernedAllocationRequest {
				r := governedRequest("duplicate-assignment", actor, root.AssignmentID, 2)
				r.Children[1].AssignmentID = r.Children[0].AssignmentID
				return r
			}(),
			kind: provenance.GovernedAllocationValidation,
		},
		{
			name: "existing task collision",
			request: func() provenance.GovernedAllocationRequest {
				r := governedRequest("task-collision", actor, root.AssignmentID, 1)
				r.Children[0].TaskID = valid.Children[0].TaskID
				return r
			}(),
			kind: provenance.GovernedAllocationCollision,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := taskCount(t, tr)
			_, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, test.request)
			mustGovernedError(t, err, test.kind)
			if got := taskCount(t, tr); got != before {
				t.Fatalf("rejected request wrote tasks: before=%d after=%d", before, got)
			}
		})
	}
}

func TestSessionAllocateGovernedRejectsDifferentActiveParentAuthorityWithoutWrites(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	childClosure, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("active-parent-b", actor, root.AssignmentID, 1))
	if err != nil {
		t.Fatalf("create active parent B: %v", err)
	}
	parentB := childClosure.Children()[0]
	before := taskCount(t, tr)
	rejected := governedRequest("wrong-session-authority", actor, parentB.AssignmentID, 1)
	_, err = tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, rejected)
	mustGovernedError(t, err, provenance.GovernedAllocationAuthority)
	if got := taskCount(t, tr); got != before {
		t.Fatalf("mismatched Session authority wrote tasks: before=%d after=%d", before, got)
	}
	assertGovernedOperationAbsent(t, tr, rejected.OperationID)
}

func TestGovernedAllocationRejectsRevokedMiddleAncestorWithoutWrites(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	childClosure, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("chain-child", actor, root.AssignmentID, 1))
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	child := childClosure.Children()[0]
	grandchildClosure, err := tr.As(actor, child.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("chain-grandchild", actor, child.AssignmentID, 1))
	if err != nil {
		t.Fatalf("create grandchild: %v", err)
	}
	grandchild := grandchildClosure.Children()[0]
	if _, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:        "end-middle-assignment",
		ActorID:            actor,
		AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
		CommandDigest:      []byte("end-middle"),
		MutationDigest:     []byte("end-middle"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectAssignmentEnd, AssignmentID: child.AssignmentID, TaskID: child.TaskID, SlotID: provenance.SlotOwnerResponsibility,
		}},
	}); err != nil {
		t.Fatalf("end middle assignment: %v", err)
	}
	before := taskCount(t, tr)
	rejected := governedRequest("under-revoked-ancestor", actor, grandchild.AssignmentID, 1)
	_, err = tr.As(actor, grandchild.AssignmentRow.JournalID).AllocateGoverned(ctx, rejected)
	mustGovernedError(t, err, provenance.GovernedAllocationRevoked)
	if got := taskCount(t, tr); got != before {
		t.Fatalf("revoked ancestor wrote tasks: before=%d after=%d", before, got)
	}
	assertGovernedOperationAbsent(t, tr, rejected.OperationID)
}

func TestGovernedAllocationRejectsRevocationAndDepth65WithoutWrites(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	preRevocation := governedRequest("before-revocation", actor, root.AssignmentID, 1)
	first, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, preRevocation)
	if err != nil {
		t.Fatalf("allocate before revocation: %v", err)
	}

	// A root assignment ended through the established Journal API cannot allocate.
	if _, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID: "end-root", ActorID: actor, AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
		CommandDigest: []byte("end-root"), MutationDigest: []byte("end-root"),
		Effects: []provenance.Effect{{Sort: provenance.EffectAssignmentEnd, AssignmentID: root.AssignmentID, TaskID: root.TaskID, SlotID: provenance.SlotOwnerResponsibility}},
	}); err != nil {
		t.Fatalf("end root assignment: %v", err)
	}
	retry, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, preRevocation)
	if err != nil {
		t.Fatalf("exact retry after revocation: %v", err)
	}
	if !first.Equal(retry) {
		t.Fatalf("retry after revocation differs from original closure")
	}
	before := taskCount(t, tr)
	_, err = tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("revoked", actor, root.AssignmentID, 1))
	mustGovernedError(t, err, provenance.GovernedAllocationRevoked)
	if got := taskCount(t, tr); got != before {
		t.Fatalf("revoked parent wrote tasks: before=%d after=%d", before, got)
	}

	depthTracker, depthActor := openGovernedTracker(t)
	parent := initializeRoot(t, depthTracker, depthActor)
	for depth := 2; depth <= provenance.MaxGovernedAuthorityDepth; depth++ {
		closure, err := depthTracker.As(depthActor, parent.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest(fmt.Sprintf("depth-%d", depth), depthActor, parent.AssignmentID, 1))
		if err != nil {
			t.Fatalf("allocate at depth %d: %v", depth, err)
		}
		parent = closure.Children()[0]
	}
	before = taskCount(t, depthTracker)
	_, err = depthTracker.As(depthActor, parent.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("depth-65", depthActor, parent.AssignmentID, 1))
	mustGovernedError(t, err, provenance.GovernedAllocationDepth)
	if got := taskCount(t, depthTracker); got != before {
		t.Fatalf("depth 65 wrote tasks: before=%d after=%d", before, got)
	}
}

func TestGovernedClosureReopensAndCorruptionFailsClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")
	tr, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	actor := registerGovernedActor(t, tr, "reopen")
	root := initializeRoot(t, tr, actor)
	request := governedRequest("reopen-allocation", actor, root.AssignmentID, 2)
	first, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
	if err != nil {
		t.Fatalf("allocation: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	retry, err := reopened.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
	if err != nil {
		t.Fatalf("reopened exact retry: %v", err)
	}
	if !first.Equal(retry) {
		t.Fatalf("reopened closure differs: first=%+v retry=%+v", first, retry)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open corruption handle: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE governed_operation_effect_rows
		SET produced_journal_id = (SELECT anchor_journal_id FROM governed_allocation_operations WHERE operation_id = ?1)
		WHERE anchor_journal_id = (SELECT anchor_journal_id FROM governed_allocation_operations WHERE operation_id = ?1)
		  AND effect_ordinal = 0 AND subordinal = 0`, request.OperationID); err != nil {
		t.Fatalf("inject mapping corruption: %v", err)
	}
	_, err = reopened.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
	mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
	var obsoleteBindings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'governed_operation_result_bindings'`).Scan(&obsoleteBindings); err != nil {
		t.Fatalf("inspect obsolete binding table: %v", err)
	}
	if obsoleteBindings != 0 {
		t.Fatalf("obsolete governed_operation_result_bindings table remains")
	}
}

func TestGovernedAllocationStandaloneAndExactHandleFusedParity(t *testing.T) {
	ctx := context.Background()
	standalone, actor := openGovernedTracker(t)
	root := initializeRoot(t, standalone, actor)
	direct, err := standalone.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("parity-allocation", actor, root.AssignmentID, 2))
	if err != nil {
		t.Fatalf("standalone allocation: %v", err)
	}

	fused := openFusedAllocator(t, "parity")
	fusedTracker := fused.Tracker()
	fusedActor := registerGovernedActor(t, fusedTracker, "governed")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused capability: %v", err)
	}
	fusedRootRequest := provenance.RootGenesisRequest{OperationID: "governed-fused-genesis", ActorID: fusedActor, Command: "test.genesis", Root: governedChild("fused-root", fusedActor)}
	fusedRootClosure, err := fused.RunInitializeRoot(ctx, "governed-fused-genesis-a", fusedRootRequest)
	if err != nil {
		t.Fatalf("run fused genesis: %v", err)
	}
	fusedRoot, ok := fusedRootClosure.Root()
	if !ok {
		fatalf(t, "fused genesis produced no root")
	}

	fusedRequest := governedRequest("parity-allocation", fusedActor, fusedRoot.AssignmentID, 2)
	fusedClosure, err := fused.RunAllocate(ctx, "governed-fused-allocation-a", fusedRoot.AssignmentRow.JournalID, fusedRequest)
	if err != nil {
		t.Fatalf("run fused allocation: %v", err)
	}
	if len(fusedClosure.Children()) != len(direct.Children()) {
		t.Fatalf("fused child count=%d standalone=%d", len(fusedClosure.Children()), len(direct.Children()))
	}
	for i := range direct.Children() {
		if fusedClosure.Children()[i].Ordinal != direct.Children()[i].Ordinal || fusedClosure.Children()[i].TaskRow.Subordinal != direct.Children()[i].TaskRow.Subordinal {
			t.Fatalf("fused child %d does not preserve structural parity", i)
		}
	}
	retry, err := fused.RunAllocate(ctx, "governed-fused-allocation-b", fusedRoot.AssignmentRow.JournalID, fusedRequest)
	if err != nil {
		t.Fatalf("run fused exact retry through distinct workflow: %v", err)
	}
	assertSameClosure(t, fusedClosure, retry)

	changed := fusedRequest
	changed.Children = append([]provenance.GovernedChildSpec(nil), fusedRequest.Children...)
	changed.Children[0].Title = "changed fused child title"
	before := taskCount(t, fusedTracker)
	_, err = fused.RunAllocate(ctx, "governed-fused-allocation-changed", fusedRoot.AssignmentRow.JournalID, changed)
	mustGovernedError(t, err, provenance.GovernedAllocationConflict)
	if got := taskCount(t, fusedTracker); got != before {
		t.Fatalf("changed fused retry wrote tasks: before=%d after=%d", before, got)
	}
}

func TestSessionAllocationReplayRequiresExactAuthorityAndSurvivesLaterRevocation(t *testing.T) {
	ctx := context.Background()
	tr, actor, db := openGovernedTrackerWithDatabase(t)
	root := initializeRoot(t, tr, actor)
	request := governedRequest("replay-authority", actor, root.AssignmentID, 1)
	first, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
	if err != nil {
		t.Fatalf("allocate replay subject: %v", err)
	}

	beforeZero := snapshotGovernedTables(t, db)
	closure, err := tr.As(actor, 0).AllocateGoverned(ctx, request)
	mustGovernedError(t, err, provenance.GovernedAllocationAuthority)
	assertEmptyClosure(t, closure)
	assertNoGovernedWrites(t, beforeZero, db)

	sideClosure, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, governedRequest("replay-other-authority", actor, root.AssignmentID, 1))
	if err != nil {
		t.Fatalf("allocate independent Session authority: %v", err)
	}
	wrongAuthority := sideClosure.Children()[0].AssignmentRow.JournalID
	beforeWrong := snapshotGovernedTables(t, db)
	closure, err = tr.As(actor, wrongAuthority).AllocateGoverned(ctx, request)
	mustGovernedError(t, err, provenance.GovernedAllocationAuthority)
	assertEmptyClosure(t, closure)
	assertNoGovernedWrites(t, beforeWrong, db)

	if _, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:        "end-root-after-replay-subject",
		ActorID:            actor,
		AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
		CommandDigest:      []byte("end-root-after-replay-subject"),
		MutationDigest:     []byte("end-root-after-replay-subject"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectAssignmentEnd, AssignmentID: root.AssignmentID, TaskID: root.TaskID, SlotID: provenance.SlotOwnerResponsibility,
		}},
	}); err != nil {
		t.Fatalf("revoke replay parent: %v", err)
	}

	beforeRetry := snapshotGovernedTables(t, db)
	retry, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, request)
	if err != nil {
		t.Fatalf("replay after parent revocation: %v", err)
	}
	assertSameClosure(t, first, retry)
	assertNoGovernedWrites(t, beforeRetry, db)

	revokedRequest := governedRequest("replay-revoked-new-operation", actor, root.AssignmentID, 1)
	beforeRevoked := snapshotGovernedTables(t, db)
	closure, err = tr.As(actor, root.AssignmentRow.JournalID).AllocateGoverned(ctx, revokedRequest)
	mustGovernedError(t, err, provenance.GovernedAllocationRevoked)
	assertEmptyClosure(t, closure)
	assertNoGovernedWrites(t, beforeRevoked, db)
	assertGovernedOperationAbsent(t, tr, revokedRequest.OperationID)
}

func TestFusedWorkflowIDReplayMatchesCanonicalRequestAndAuthority(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "workflow-input-replay")
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "fused-workflow-input")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused capability: %v", err)
	}

	rootRequest := provenance.RootGenesisRequest{
		OperationID: "fused-workflow-input-genesis",
		ActorID:     actor,
		Command:     "test.genesis",
		Root:        governedChild("fused-workflow-input-root", actor),
	}
	root, err := fused.RunInitializeRoot(ctx, "fused-workflow-input-genesis", rootRequest)
	if err != nil {
		t.Fatalf("initialize fused root: %v", err)
	}
	beforeGenesisReplay := snapshotGovernedTables(t, db)
	retriedRoot, err := fused.RunInitializeRoot(ctx, "fused-workflow-input-genesis", rootRequest)
	if err != nil {
		t.Fatalf("exact fused genesis workflow replay: %v", err)
	}
	assertSameClosure(t, root, retriedRoot)
	assertNoGovernedWrites(t, beforeGenesisReplay, db)

	changedRoot := rootRequest
	changedRoot.Root.Title = "changed root title"
	beforeChangedGenesis := snapshotGovernedTables(t, db)
	closure, err := fused.RunInitializeRoot(ctx, "fused-workflow-input-genesis", changedRoot)
	mustGovernedError(t, err, provenance.GovernedAllocationConflict)
	assertEmptyClosure(t, closure)
	assertNoGovernedWrites(t, beforeChangedGenesis, db)

	rootBinding, ok := root.Root()
	if !ok {
		t.Fatal("fused root closure has no root binding")
	}
	request := governedRequest("fused-workflow-input-allocation", actor, rootBinding.AssignmentID, 1)
	allocation, err := fused.RunAllocate(ctx, "fused-workflow-input-allocation", rootBinding.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run fused allocation: %v", err)
	}
	beforeExactReplay := snapshotGovernedTables(t, db)
	retriedAllocation, err := fused.RunAllocate(ctx, "fused-workflow-input-allocation", rootBinding.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("exact fused allocation workflow replay: %v", err)
	}
	assertSameClosure(t, allocation, retriedAllocation)
	assertNoGovernedWrites(t, beforeExactReplay, db)

	changedRequest := request
	changedRequest.Children = append([]provenance.GovernedChildSpec(nil), request.Children...)
	changedRequest.Children[0].Title = "changed fused workflow child title"
	beforeChangedRequest := snapshotGovernedTables(t, db)
	closure, err = fused.RunAllocate(ctx, "fused-workflow-input-allocation", rootBinding.AssignmentRow.JournalID, changedRequest)
	mustGovernedError(t, err, provenance.GovernedAllocationConflict)
	assertEmptyClosure(t, closure)
	assertNoGovernedWrites(t, beforeChangedRequest, db)

	beforeChangedAuthority := snapshotGovernedTables(t, db)
	closure, err = fused.RunAllocate(ctx, "fused-workflow-input-allocation", rootBinding.AssignmentRow.JournalID+1, request)
	mustGovernedError(t, err, provenance.GovernedAllocationConflict)
	assertEmptyClosure(t, closure)
	assertNoGovernedWrites(t, beforeChangedAuthority, db)
}

func openGovernedTracker(t *testing.T) (provenance.Tracker, provenance.ActorID) {
	t.Helper()
	tr, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr, registerGovernedActor(t, tr, "governed")
}

func openGovernedTrackerWithDatabase(t *testing.T) (provenance.Tracker, provenance.ActorID, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "governed-counts.db")
	tr, err := provenance.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open governed SQLite tracker: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		_ = tr.Close()
		t.Fatalf("open governed SQLite counter: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() { _ = tr.Close() })
	return tr, registerGovernedActor(t, tr, "governed-counts"), db
}

func openFusedAllocator(t *testing.T, name string) *provenance.FusedGovernedAllocator {
	t.Helper()
	allocator, _ := openFusedAllocatorWithDatabase(t, name)
	return allocator
}

func openFusedAllocatorWithDatabase(t *testing.T, name string) (*provenance.FusedGovernedAllocator, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	allocator, err := provenance.OpenFusedGovernedAllocator(context.Background(), provenance.FusedGovernedAllocatorConfig{
		SQLiteDSN:          dsn,
		AppName:            "provenance-governed-" + name,
		ApplicationVersion: "test-v1",
		Logger:             slog.Default(),
	})
	if err != nil {
		t.Fatalf("open fused allocator: %v", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = allocator.Close(30 * time.Second)
		t.Fatalf("open fused allocation counter: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() {
		if err := allocator.Close(30 * time.Second); err != nil {
			t.Errorf("close fused allocator: %v", err)
		}
	})
	return allocator, db
}

func registerGovernedActor(t *testing.T, tr provenance.Tracker, namespace string) provenance.ActorID {
	t.Helper()
	actor, err := tr.RegisterSoftwareAgent(namespace, "governed-test", "1", "test")
	if err != nil {
		t.Fatalf("register actor: %v", err)
	}
	return actor.ID
}

func initializeRoot(t *testing.T, tr provenance.Tracker, actor provenance.ActorID) provenance.GovernedChildBinding {
	t.Helper()
	closure, err := tr.InitializeGovernedRoot(context.Background(), provenance.RootGenesisRequest{OperationID: "governed-genesis", ActorID: actor, Command: "test.genesis", Root: governedChild("root", actor)})
	if err != nil {
		t.Fatalf("initialize root: %v", err)
	}
	root, ok := closure.Root()
	if !ok {
		t.Fatal("no root binding")
	}
	return root
}

func governedRequest(operation string, actor provenance.ActorID, parent provenance.AssignmentID, count int) provenance.GovernedAllocationRequest {
	children := make([]provenance.GovernedChildSpec, count)
	for i := range children {
		children[i] = governedChild(fmt.Sprintf("%s-child-%d", operation, i), actor)
	}
	return provenance.GovernedAllocationRequest{OperationID: provenance.OperationID(operation), ActorID: actor, Command: "test.allocate", ParentAssignmentID: parent, Children: children}
}

func governedChild(name string, occupant provenance.ActorID) provenance.GovernedChildSpec {
	return provenance.GovernedChildSpec{
		TaskID:       provenance.TaskID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("task/"+name))},
		AssignmentID: provenance.AssignmentID("assignment/" + name),
		Occupant:     occupant,
		Title:        "task " + name,
		Description:  "governed allocation integration test",
		Type:         provenance.TaskTypeTask,
		Priority:     provenance.PriorityMedium,
		Phase:        provenance.PhaseWorkerSlices,
	}
}

func taskCount(t *testing.T, tr provenance.Tracker) int {
	t.Helper()
	tasks, err := tr.List(provenance.ListFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	return len(tasks)
}

func assertGovernedOperationAbsent(t *testing.T, tr provenance.Tracker, operation provenance.OperationID) {
	t.Helper()
	result, err := tr.Journal().LookupCommitted(operation)
	if err != nil || result.Kind != provenance.CommittedAbsent {
		t.Fatalf("rejected allocation left a committed operation: result=%+v err=%v", result, err)
	}
}

func ptr(value provenance.JournalID) *provenance.JournalID { return &value }

func mustGovernedError(t *testing.T, err error, want provenance.GovernedAllocationErrorKind) {
	t.Helper()
	var governed *provenance.GovernedAllocationError
	if !errors.As(err, &governed) {
		t.Fatalf("error %v is not a governed allocation error", err)
	}
	if governed.Kind != want {
		t.Fatalf("governed error kind=%s, want %s: %v", governed.Kind, want, err)
	}
}

func assertSameClosure(t *testing.T, got, want provenance.OperationClosure) {
	t.Helper()
	if got.OperationID() != want.OperationID() || got.Kind() != want.Kind() || got.AnchorJournalID() != want.AnchorJournalID() {
		t.Fatalf("closure header mismatch: got=%+v want=%+v", got, want)
	}
	gotChildren, wantChildren := got.Children(), want.Children()
	if len(gotChildren) != len(wantChildren) {
		t.Fatalf("closure child count=%d want=%d", len(gotChildren), len(wantChildren))
	}
	for i := range gotChildren {
		actual, expected := gotChildren[i], wantChildren[i]
		if actual.Ordinal != expected.Ordinal || actual.TaskID != expected.TaskID || actual.AssignmentID != expected.AssignmentID || actual.Occupant != expected.Occupant || actual.TaskRow.OperationID != expected.TaskRow.OperationID || actual.TaskRow.EffectOrdinal != expected.TaskRow.EffectOrdinal || actual.TaskRow.Subordinal != expected.TaskRow.Subordinal || actual.TaskRow.JournalID != expected.TaskRow.JournalID || actual.AssignmentRow.OperationID != expected.AssignmentRow.OperationID || actual.AssignmentRow.EffectOrdinal != expected.AssignmentRow.EffectOrdinal || actual.AssignmentRow.Subordinal != expected.AssignmentRow.Subordinal || actual.AssignmentRow.JournalID != expected.AssignmentRow.JournalID {
			t.Fatalf("closure child %d mismatch: got=%+v want=%+v", i, actual, expected)
		}
	}
}

type governedTableCounts struct {
	tasks                 int
	journal               int
	journalOperations     int
	governedOperations    int
	governedOperationRows int
	assignmentEpisodes    int
	assignmentTransitions int
}

func snapshotGovernedTables(t *testing.T, db *sql.DB) governedTableCounts {
	t.Helper()
	counts := governedTableCounts{}
	for _, target := range []struct {
		name string
		into *int
	}{
		{"tasks", &counts.tasks},
		{"journal", &counts.journal},
		{"journal_operations", &counts.journalOperations},
		{"governed_allocation_operations", &counts.governedOperations},
		{"governed_operation_effect_rows", &counts.governedOperationRows},
		{"journal_authority_assignment_episodes", &counts.assignmentEpisodes},
		{"journal_authority_assignment_transitions", &counts.assignmentTransitions},
	} {
		if err := db.QueryRow("SELECT COUNT(*) FROM " + target.name).Scan(target.into); err != nil {
			t.Fatalf("count %s before/after governed rejection: %v", target.name, err)
		}
	}
	return counts
}

func assertNoGovernedWrites(t *testing.T, before governedTableCounts, db *sql.DB) {
	t.Helper()
	if after := snapshotGovernedTables(t, db); after != before {
		t.Fatalf("governed rejection/replay changed durable rows: before=%+v after=%+v", before, after)
	}
}

func assertEmptyClosure(t *testing.T, closure provenance.OperationClosure) {
	t.Helper()
	if closure.OperationID() != "" || closure.Kind() != 0 || closure.AnchorJournalID() != 0 || len(closure.Children()) != 0 {
		t.Fatalf("rejected replay returned a closure: %+v", closure)
	}
}

func fatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf(format, args...)
}
