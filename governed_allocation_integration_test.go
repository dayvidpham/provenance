package provenance_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
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

func TestGovernedAllocationRejectsRevocationWithoutWrites(t *testing.T) {
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
	var receipt struct {
		Version     int                            `json:"version"`
		Kind        provenance.GovernedRequestKind `json:"kind"`
		OperationID string                         `json:"operationID"`
	}
	var canonical []byte
	if err := db.QueryRow(`SELECT canonical_request FROM governed_allocation_operations WHERE operation_id=?1`, request.OperationID).Scan(&canonical); err != nil {
		t.Fatalf("read simple governed allocation receipt: %v", err)
	}
	if err := json.Unmarshal(canonical, &receipt); err != nil {
		t.Fatalf("decode simple governed allocation receipt: %v", err)
	}
	if receipt.Version != 1 || receipt.Kind != provenance.GovernedRequestAllocation || receipt.OperationID != string(request.OperationID) {
		t.Fatalf("simple allocation receipt=%+v, want the baseline allocation canonical form", receipt)
	}
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

func TestFusedGovernedAllocationParticipantCommitsDomainAuditAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	var calls int
	participant := provenance.GovernedAllocationParticipant(func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		calls++
		children := closure.Children()
		if len(children) != 1 || closure.OperationID() != request.OperationID {
			return fmt.Errorf("participant received inconsistent governed closure for operation %q", request.OperationID)
		}

		var domainRows int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?1`, children[0].TaskID.String()).Scan(&domainRows); err != nil {
			return fmt.Errorf("participant query committed child task: %w", err)
		}
		if domainRows != 1 {
			return fmt.Errorf("participant found %d committed child task rows, want one", domainRows)
		}
		rows, err := tx.Query(ctx, `SELECT title FROM tasks WHERE id = ?1`, children[0].TaskID.String())
		if err != nil {
			return fmt.Errorf("participant query child title: %w", err)
		}
		if !rows.Next() {
			_ = rows.Close()
			return fmt.Errorf("participant found no title row for committed child task")
		}
		var title string
		if err := rows.Scan(&title); err != nil {
			_ = rows.Close()
			return fmt.Errorf("participant scan child title: %w", err)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("participant iterate child title: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("participant close child title rows: %w", err)
		}
		if title != request.Children[0].Title {
			return fmt.Errorf("participant saw child title %q, want %q", title, request.Children[0].Title)
		}

		result, err := tx.Exec(ctx, `INSERT INTO fused_governed_participant_audit (operation_id, anchor_journal_id, child_task_id) VALUES (?1, ?2, ?3)`, request.OperationID, closure.AnchorJournalID(), children[0].TaskID.String())
		if err != nil {
			return fmt.Errorf("participant insert audit sentinel: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("participant audit sentinel affected rows=%d err=%v, want 1 and nil", affected, err)
		}
		return nil
	})
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "participant-commit", participant)
	createFusedGovernedParticipantAuditTable(t, db)
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "participant-commit")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused capability: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "participant-commit-root")
	request := governedRequest("participant-commit", actor, root.AssignmentID, 1)
	closure, err := fused.RunAllocate(ctx, "participant-commit-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run fused allocation with participant: %v", err)
	}
	if calls != 1 {
		t.Fatalf("participant calls = %d, want 1", calls)
	}
	child := closure.Children()[0]
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks WHERE id = ?1`, child.TaskID.String()); got != 1 {
		t.Fatalf("committed child task rows = %d, want 1", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM governed_allocation_operations WHERE operation_id = ?1`, request.OperationID); got != 1 {
		t.Fatalf("committed governed operation rows = %d, want 1", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM fused_governed_participant_audit WHERE operation_id = ?1`, request.OperationID); got != 1 {
		t.Fatalf("committed participant audit rows = %d, want 1", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?1 AND error IS NULL`, "participant-commit-workflow"); got != 1 {
		t.Fatalf("successful DBOS checkpoints = %d, want 1", got)
	}
}

func TestFusedGovernedAllocationParticipantErrorRollsBackDomainAuditAndSuccessfulCheckpoint(t *testing.T) {
	ctx := context.Background()
	const participantFailure = "participant audit dependency unavailable"
	participant := provenance.GovernedAllocationParticipant(func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		if _, err := tx.Exec(ctx, `INSERT INTO fused_governed_participant_audit (operation_id, anchor_journal_id, child_task_id) VALUES (?1, ?2, ?3)`, request.OperationID, closure.AnchorJournalID(), closure.Children()[0].TaskID.String()); err != nil {
			return fmt.Errorf("participant insert audit sentinel: %w", err)
		}
		return errors.New(participantFailure)
	})
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "participant-rollback", participant)
	createFusedGovernedParticipantAuditTable(t, db)
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "participant-rollback")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused capability: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "participant-rollback-root")
	request := governedRequest("participant-rollback", actor, root.AssignmentID, 1)
	_, err := fused.RunAllocate(ctx, "participant-rollback-workflow", root.AssignmentRow.JournalID, request)
	if err == nil || !strings.Contains(err.Error(), participantFailure) {
		t.Fatalf("participant failure error = %v, want infrastructure error containing %q", err, participantFailure)
	}
	var domainError *provenance.GovernedAllocationError
	if errors.As(err, &domainError) {
		t.Fatalf("participant failure was returned as typed domain rejection: %+v", domainError)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks WHERE id = ?1`, request.Children[0].TaskID.String()); got != 0 {
		t.Fatalf("rolled-back child task rows = %d, want 0", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM governed_allocation_operations WHERE operation_id = ?1`, request.OperationID); got != 0 {
		t.Fatalf("rolled-back governed operation rows = %d, want 0", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM fused_governed_participant_audit WHERE operation_id = ?1`, request.OperationID); got != 0 {
		t.Fatalf("rolled-back participant audit rows = %d, want 0", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?1 AND error IS NULL`, "participant-rollback-workflow"); got != 0 {
		t.Fatalf("successful DBOS checkpoints after participant failure = %d, want 0", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?1 AND error IS NOT NULL`, "participant-rollback-workflow"); got != 1 {
		t.Fatalf("failed DBOS checkpoints after participant failure = %d, want 1", got)
	}
	assertGovernedOperationAbsent(t, tr, request.OperationID)
}

func TestFusedGovernedAllocationParticipantExactReplaySkipsCallbackAndDistinctWorkflowIsIdempotent(t *testing.T) {
	ctx := context.Background()
	var calls int
	participant := provenance.GovernedAllocationParticipant(func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		calls++
		child := closure.Children()[0]
		if _, err := tx.Exec(ctx, `INSERT INTO fused_governed_participant_audit (operation_id, anchor_journal_id, child_task_id)
			VALUES (?1, ?2, ?3) ON CONFLICT(operation_id) DO NOTHING`, request.OperationID, closure.AnchorJournalID(), child.TaskID.String()); err != nil {
			return fmt.Errorf("participant idempotent audit insert: %w", err)
		}
		var storedAnchor int64
		var storedTaskID string
		if err := tx.QueryRow(ctx, `SELECT anchor_journal_id, child_task_id FROM fused_governed_participant_audit WHERE operation_id = ?1`, request.OperationID).Scan(&storedAnchor, &storedTaskID); err != nil {
			return fmt.Errorf("participant load idempotent audit binding: %w", err)
		}
		if storedAnchor != int64(closure.AnchorJournalID()) || storedTaskID != child.TaskID.String() {
			return fmt.Errorf("participant immutable audit binding differs for operation %q", request.OperationID)
		}
		return nil
	})
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "participant-replay", participant)
	createFusedGovernedParticipantAuditTable(t, db)
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "participant-replay")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused capability: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "participant-replay-root")
	request := governedRequest("participant-replay", actor, root.AssignmentID, 1)
	first, err := fused.RunAllocate(ctx, "participant-replay-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("first fused allocation: %v", err)
	}
	exactReplay, err := fused.RunAllocate(ctx, "participant-replay-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("exact DBOS workflow replay: %v", err)
	}
	assertSameClosure(t, first, exactReplay)
	if calls != 1 {
		t.Fatalf("participant calls after exact workflow replay = %d, want 1", calls)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?1 AND error IS NULL`, "participant-replay-workflow"); got != 1 {
		t.Fatalf("successful checkpoints for exact workflow replay = %d, want 1", got)
	}

	distinctWorkflowReplay, err := fused.RunAllocate(ctx, "participant-replay-distinct-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("distinct workflow operation replay: %v", err)
	}
	assertSameClosure(t, first, distinctWorkflowReplay)
	if calls != 2 {
		t.Fatalf("participant calls after distinct workflow replay = %d, want 2", calls)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM fused_governed_participant_audit WHERE operation_id = ?1`, request.OperationID); got != 1 {
		t.Fatalf("idempotent participant audit rows = %d, want 1", got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid = ?1 AND error IS NULL`, "participant-replay-distinct-workflow"); got != 1 {
		t.Fatalf("successful checkpoints for distinct workflow replay = %d, want 1", got)
	}

	rejected := request
	rejected.Children = append([]provenance.GovernedChildSpec(nil), request.Children...)
	rejected.Children[0].Title = "changed participant replay child"
	if _, err := fused.RunAllocate(ctx, "participant-replay-domain-rejection", root.AssignmentRow.JournalID, rejected); err == nil {
		t.Fatal("changed allocation request did not return a typed domain rejection")
	} else {
		mustGovernedError(t, err, provenance.GovernedAllocationConflict)
	}
	if calls != 2 {
		t.Fatalf("participant calls after typed domain rejection = %d, want 2", calls)
	}
}

func TestFusedGovernedAllocationParticipantReceivesDefensiveRequestAndClosureCopies(t *testing.T) {
	ctx := context.Background()
	participant := provenance.GovernedAllocationParticipant(func(_ context.Context, _ provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		request.Children[0].Title = "participant-mutated-title"
		request.Children = nil
		children := closure.Children()
		children[0].AssignmentID = "participant-mutated-assignment"
		if closure.Children()[0].AssignmentID == "participant-mutated-assignment" {
			return errors.New("participant mutated closure through returned children")
		}
		return nil
	})
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "participant-copies", participant)
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "participant-copies")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused capability: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "participant-copies-root")
	request := governedRequest("participant-copies", actor, root.AssignmentID, 1)
	wantTitle := request.Children[0].Title
	wantAssignmentID := request.Children[0].AssignmentID
	closure, err := fused.RunAllocate(ctx, "participant-copies-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("fused allocation with copying participant: %v", err)
	}
	if len(request.Children) != 1 || request.Children[0].Title != wantTitle || request.Children[0].AssignmentID != wantAssignmentID {
		t.Fatalf("participant mutated caller request: %+v", request)
	}
	child := closure.Children()[0]
	if child.AssignmentID != wantAssignmentID {
		t.Fatalf("participant mutated returned closure assignment = %q, want %q", child.AssignmentID, wantAssignmentID)
	}
	var persistedTitle string
	if err := db.QueryRow(`SELECT title FROM tasks WHERE id = ?1`, child.TaskID.String()).Scan(&persistedTitle); err != nil {
		t.Fatalf("load child title after participant mutation: %v", err)
	}
	if persistedTitle != wantTitle {
		t.Fatalf("participant mutated persisted child title = %q, want %q", persistedTitle, wantTitle)
	}
}

func TestFusedGovernedAllocationComposedPersistsAllowedSupplementsAndReplays(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "composed-success")
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "composed-success")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-success-root")
	request := composedGovernedRequest("composed-success", actor, root, 1)

	first, err := fused.RunAllocateComposed(ctx, "composed-success-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run composed allocation: %v", err)
	}
	closure := first.Closure()
	child := closure.Children()[0]
	if got := len(first.SupplementalResultSlots()); got != 4 {
		t.Fatalf("supplemental result slots=%d, want 4", got)
	}
	if got := len(first.SupplementalEmittedEvents()); got != 2 {
		t.Fatalf("supplemental emitted events=%d, want edge and task event", got)
	}
	for _, check := range []struct {
		name  string
		query string
		args  []any
		want  int
	}{
		{"allocation", `SELECT COUNT(*) FROM tasks WHERE id=?1`, []any{child.TaskID.String()}, 1},
		{"evidence", `SELECT COUNT(*) FROM journal_evidence WHERE task_id=?1`, []any{child.TaskID.String()}, 1},
		{"edge", `SELECT COUNT(*) FROM edges WHERE source_id=?1 AND target_id=?2`, []any{root.TaskID.String(), child.TaskID.String()}, 1},
		{"task event", `SELECT COUNT(*) FROM journal_task_events WHERE task_id=?1 AND event_kind=?2`, []any{child.TaskID.String(), "provenance.slice.created"}, 1},
		{"activity", `SELECT COUNT(*) FROM activities WHERE id=?1`, []any{request.SupplementalEffects[3].ActivityID.String()}, 1},
		{"checkpoint", `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, []any{"composed-success-workflow"}, 1},
	} {
		t.Run(check.name, func(t *testing.T) {
			if got := countFusedGovernedRows(t, db, check.query, check.args...); got != check.want {
				t.Fatalf("%s rows=%d, want %d", check.name, got, check.want)
			}
		})
	}

	replayed, err := fused.RunAllocateComposed(ctx, "composed-success-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("exact composed workflow replay: %v", err)
	}
	assertSameClosure(t, closure, replayed.Closure())
	if !reflect.DeepEqual(first.SupplementalResultSlots(), replayed.SupplementalResultSlots()) || !reflect.DeepEqual(first.SupplementalEmittedEvents(), replayed.SupplementalEmittedEvents()) {
		t.Fatalf("exact composed replay changed supplemental receipt: first=%+v replay=%+v", first, replayed)
	}
}

func TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*testing.T, *sql.DB, provenance.GovernedChildBinding, provenance.GovernedAllocationComposedRequest)
	}{
		{"task event kind", composedReceiptSQL(`UPDATE journal_task_events SET event_kind='forged.event' WHERE event_kind='provenance.slice.created'`)},
		{"task event payload", composedReceiptSQL(`UPDATE journal_task_events SET payload='{"forged":true}' WHERE event_kind='provenance.slice.created'`)},
		{"task event task ID", func(t *testing.T, db *sql.DB, root provenance.GovernedChildBinding, _ provenance.GovernedAllocationComposedRequest) {
			mustMutateComposedReceipt(t, db, `UPDATE journal_task_events SET task_id=?1 WHERE event_kind='provenance.slice.created'`, root.TaskID.String())
		}},
		{"task event context", func(t *testing.T, db *sql.DB, root provenance.GovernedChildBinding, _ provenance.GovernedAllocationComposedRequest) {
			mustMutateComposedReceipt(t, db, `UPDATE journal_task_event_contexts SET context_identity=?1`, root.TaskID.String())
		}},
		{"foreign task event attachment", func(t *testing.T, db *sql.DB, root provenance.GovernedChildBinding, _ provenance.GovernedAllocationComposedRequest) {
			mustMutateComposedReceipt(t, db, `UPDATE journal_task_event_contexts SET attached_by_journal_id=?1`, root.TaskRow.JournalID)
		}},
		{"subordinate recorded at", composedReceiptSQL(`UPDATE journal SET recorded_at=recorded_at+1 WHERE produced_by_operation_journal_id IS NOT NULL`)},
		{"foreign produced row", composedReceiptSQL(`UPDATE journal SET produced_by_operation_journal_id=(SELECT MIN(journal_id) FROM journal_operations) WHERE journal_id=(SELECT MAX(journal_id) FROM journal WHERE produced_by_operation_journal_id IS NOT NULL)`)},
		{"evidence payload", composedReceiptSQL(`UPDATE journal_evidence SET payload='{"forged":true}' WHERE evidence_kind='provenance.assignment.command'`)},
		{"evidence operand", composedReceiptSQL(`UPDATE journal_evidence SET content_digest=x'00' WHERE evidence_kind='provenance.assignment.command'`)},
		{"activity operands", composedReceiptSQL(`UPDATE activities SET notes='forged' WHERE id IN (SELECT activity_id FROM journal_activity_creations)`)},
		{"activity ID", func(t *testing.T, db *sql.DB, _ provenance.GovernedChildBinding, request provenance.GovernedAllocationComposedRequest) {
			forged := provenance.ActivityID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("forged/activity"))}
			mustMutateComposedReceipt(t, db, `INSERT INTO activities (id,agent_id,phase_id,stage_id,started_at,notes) SELECT ?1,agent_id,phase_id,stage_id,started_at,notes FROM activities WHERE id=?2`, forged.String(), request.SupplementalEffects[3].ActivityID.String())
			mustMutateComposedReceipt(t, db, `UPDATE journal_activity_creations SET activity_id=?1`, forged.String())
		}},
		{"deleted activity subtype", composedReceiptSQL(`DELETE FROM journal_activity_creations`)},
		{"deleted un-slotted subtype", composedReceiptSQL(`DELETE FROM journal_task_events WHERE event_kind='provenance.un-slotted'`)},
		{"deleted result slot", composedReceiptSQL(`DELETE FROM journal_operation_result_slots WHERE result_slot_id=(SELECT MIN(result_slot_id) FROM journal_operation_result_slots)`)},
		{"repointed result slot", func(t *testing.T, db *sql.DB, root provenance.GovernedChildBinding, _ provenance.GovernedAllocationComposedRequest) {
			mustMutateComposedReceipt(t, db, `UPDATE journal_operation_result_slots SET produced_journal_id=?1 WHERE result_slot_id='slice-event'`, root.TaskRow.JournalID)
		}},
		{"extra result slot", composedReceiptSQL(`INSERT INTO journal_operation_result_slots (journal_id,result_slot_id,produced_journal_id) SELECT journal_id,'unexpected-slot',produced_journal_id FROM journal_operation_result_slots WHERE result_slot_id='slice-event'`)},
		{"extra event", func(t *testing.T, db *sql.DB, root provenance.GovernedChildBinding, _ provenance.GovernedAllocationComposedRequest) {
			var anchor int64
			if err := db.QueryRow(`SELECT journal_id FROM journal_operation_result_slots WHERE result_slot_id='slice-event'`).Scan(&anchor); err != nil {
				t.Fatal(err)
			}
			result, err := db.Exec(`INSERT INTO journal (kind_id,recorded_at,produced_by_operation_journal_id) VALUES (?1,?2,?3) RETURNING journal_id`, int(provenance.JournalKindTaskEvent), time.Now().UnixNano(), anchor)
			if err != nil {
				t.Fatal(err)
			}
			extra, _ := result.LastInsertId()
			mustMutateComposedReceipt(t, db, `INSERT INTO journal_task_events (journal_id,task_id,event_kind,payload) VALUES (?1,?2,'provenance.extra','{}')`, extra, root.TaskID.String())
		}},
	}
	for _, mutation := range mutations {
		for _, replayMode := range []string{"distinct-workflow", "reopen"} {
			t.Run(mutation.name+"/"+replayMode, func(t *testing.T) {
				// Each case owns a private t.TempDir database, a private DBOS
				// application name, and a participant counter local to this
				// closure. Nothing here touches package state, the working
				// directory, or another case's file, so the cases are isolated.
				t.Parallel()
				ctx := context.Background()
				participantCalls := 0
				name := "forged-receipt-" + strings.ReplaceAll(mutation.name+"-"+replayMode, " ", "-")
				path := filepath.Join(t.TempDir(), name+".db")
				participant := provenance.GovernedAllocationParticipant(func(_ context.Context, _ provenance.GovernedAllocationTransaction, _ provenance.GovernedAllocationRequest, _ provenance.OperationClosure) error {
					participantCalls++
					return nil
				})
				fused, db := openFusedReceiptProof(t, path, name, participant)
				actor := registerGovernedActor(t, fused.Tracker(), "forged-receipt-"+mutation.name)
				rootClosure, err := fused.Tracker().InitializeGovernedRoot(ctx, provenance.RootGenesisRequest{
					OperationID: provenance.OperationID("forged-receipt-root-" + mutation.name + "-genesis"),
					ActorID:     actor,
					Command:     "test.genesis",
					Root:        governedChild("forged-receipt-root-"+mutation.name+"-root", actor),
				})
				if err != nil {
					t.Fatalf("initialize forged receipt root: %v", err)
				}
				root, ok := rootClosure.Root()
				if !ok {
					t.Fatal("forged receipt root closure has no root binding")
				}
				request := composedGovernedRequest("forged-receipt-"+mutation.name, actor, root, 1)
				contextValue, err := provenance.TaskContext(request.Allocation.Children[0].TaskID)
				if err != nil {
					t.Fatal(err)
				}
				request.SupplementalEffects[2].Contexts = []provenance.EventContext{contextValue}
				request.SupplementalEffects = append(request.SupplementalEffects, provenance.Effect{Sort: provenance.EffectTaskEvent, TaskID: request.Allocation.Children[0].TaskID, EventKind: "provenance.un-slotted", Payload: []byte(`{}`)})
				if _, err := fused.RunAllocateComposed(ctx, "forged-receipt-original-"+mutation.name, root.AssignmentRow.JournalID, request); err != nil {
					t.Fatalf("commit fixture: %v", err)
				}
				if participantCalls != 1 {
					t.Fatalf("initial participant calls=%d, want 1", participantCalls)
				}
				if replayMode == "reopen" {
					if err := fused.Close(30 * time.Second); err != nil {
						t.Fatal(err)
					}
					_ = db.Close()
					fused, db = openFusedReceiptProof(t, path, name, participant)
				}
				mutation.mutate(t, db, root, request)
				beforeJournal := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM journal`)
				beforeTasks := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`)
				replayed, err := fused.RunAllocateComposed(ctx, "forged-receipt-retry-"+mutation.name, root.AssignmentRow.JournalID, request)
				mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
				if len(replayed.Closure().Children()) != 0 || len(replayed.SupplementalResultSlots()) != 0 || len(replayed.SupplementalEmittedEvents()) != 0 {
					t.Fatalf("corrupt replay returned a result: %+v", replayed)
				}
				if participantCalls != 1 {
					t.Fatalf("corrupt replay reran participant: calls=%d", participantCalls)
				}
				if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM journal`); got != beforeJournal {
					t.Fatalf("corrupt replay changed journal rows: before=%d after=%d", beforeJournal, got)
				}
				if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`); got != beforeTasks {
					t.Fatalf("corrupt replay changed task rows: before=%d after=%d", beforeTasks, got)
				}
				if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, "forged-receipt-original-"+mutation.name); got != 1 {
					t.Fatalf("original operation_outputs=%d, want 1", got)
				}
				if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NOT NULL`, "forged-receipt-retry-"+mutation.name); got != 0 {
					t.Fatalf("read-only identity classification wrote retry error operation_outputs=%d, want 0", got)
				}
				if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, "forged-receipt-retry-"+mutation.name); got != 0 {
					t.Fatalf("retry success operation_outputs=%d, want 0", got)
				}
			})
		}
	}
}

func TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen(t *testing.T) {
	for _, name := range []string{"simple", "composed-activity", "composed-event"} {
		t.Run(name, func(t *testing.T) {
			composed := name != "simple"
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "forged-output-"+name+".db")
			calls := 0
			participant := provenance.GovernedAllocationParticipant(func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
				calls++
				return nil
			})
			fused, db := openFusedReceiptProof(t, path, "forged-output-"+name, participant)
			actor := registerGovernedActor(t, fused.Tracker(), "forged-output-"+name)
			root := initializeFusedRoot(t, fused, actor, "forged-output-root-"+name)
			workflow := "forged-output-workflow-" + name
			request := governedRequest("forged-output-"+name, actor, root.AssignmentID, 1)
			if composed {
				composedRequest := composedGovernedRequest("forged-output-"+name, actor, root, 1)
				if _, err := fused.RunAllocateComposed(ctx, workflow, root.AssignmentRow.JournalID, composedRequest); err != nil {
					t.Fatal(err)
				}
			} else if _, err := fused.RunAllocate(ctx, workflow, root.AssignmentRow.JournalID, request); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("initial participant calls=%d, want 1", calls)
			}
			mutateDBOSWorkflowOutput(t, db, workflow, name)
			before := snapshotGovernedTables(t, db)
			if err := fused.Close(30 * time.Second); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			fused, db = openFusedReceiptProof(t, path, "forged-output-"+name, participant)
			if composed {
				result, err := fused.RunAllocateComposed(ctx, workflow, root.AssignmentRow.JournalID, composedGovernedRequest("forged-output-"+name, actor, root, 1))
				mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
				if len(result.Closure().Children()) != 0 || len(result.SupplementalEmittedEvents()) != 0 || len(result.SupplementalResultSlots()) != 0 {
					t.Fatalf("forged composed DBOS output returned usable receipt: %+v", result)
				}
			} else {
				result, err := fused.RunAllocate(ctx, workflow, root.AssignmentRow.JournalID, request)
				mustGovernedError(t, err, provenance.GovernedAllocationCorruption)
				assertEmptyClosure(t, result)
			}
			if calls != 1 {
				t.Fatalf("forged output replay reran participant: calls=%d", calls)
			}
			assertNoGovernedWrites(t, before, db)
			if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflow); got != 1 {
				t.Fatalf("same-workflow operation_outputs=%d, want original 1", got)
			}
		})
	}
}

func composedReceiptSQL(statement string) func(*testing.T, *sql.DB, provenance.GovernedChildBinding, provenance.GovernedAllocationComposedRequest) {
	return func(t *testing.T, db *sql.DB, _ provenance.GovernedChildBinding, _ provenance.GovernedAllocationComposedRequest) {
		mustMutateComposedReceipt(t, db, statement)
	}
}

func mustMutateComposedReceipt(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	result, err := db.Exec(statement, args...)
	if err != nil {
		t.Fatalf("apply receipt mutation: %v", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		t.Fatal("receipt mutation changed no rows")
	}
}

func openFusedReceiptProof(t *testing.T, path, name string, participant provenance.GovernedAllocationParticipant) (*provenance.FusedGovernedAllocator, *sql.DB) {
	t.Helper()
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	allocator, err := provenance.OpenFusedGovernedAllocator(context.Background(), provenance.FusedGovernedAllocatorConfig{SQLiteDSN: dsn, AppName: "provenance-" + name, ApplicationVersion: "test-v1", Logger: slog.Default(), Participant: participant})
	if err != nil {
		t.Fatalf("open fused receipt proof allocator: %v", err)
	}
	if err := allocator.Launch(); err != nil {
		_ = allocator.Close(30 * time.Second)
		t.Fatalf("launch fused receipt proof allocator: %v", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = allocator.Close(30 * time.Second)
		t.Fatalf("open fused receipt proof database: %v", err)
	}
	t.Cleanup(func() { _ = allocator.Close(30 * time.Second) })
	t.Cleanup(func() { _ = db.Close() })
	return allocator, db
}

func mutateDBOSWorkflowOutput(t *testing.T, db *sql.DB, workflow, variant string) {
	t.Helper()
	var encoded string
	if err := db.QueryRow(`SELECT output FROM workflow_status WHERE workflow_uuid=?1`, workflow).Scan(&encoded); err != nil {
		t.Fatalf("read DBOS workflow output: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode DBOS workflow output: %v", err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode DBOS workflow output JSON: %v", err)
	}
	mutated := false
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if variant == "simple" && key == "AssignmentID" && !mutated {
					typed[key], mutated = "assignment/forged-dbOS-output", true
					continue
				}
				if variant == "composed-activity" && key == "ActivityID" && child != nil && !mutated {
					if activity, ok := child.(map[string]any); ok {
						activity["UUID"], mutated = uuid.NewSHA1(uuid.NameSpaceURL, []byte("forged/dbos/activity")).String(), true
					}
				}
				if variant == "composed-event" && key == "emittedEvents" && !mutated {
					if events, ok := child.([]any); ok && len(events) > 0 {
						events[0], mutated = events[0].(float64)-1, true
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	if !mutated {
		t.Fatalf("DBOS %s output did not contain the expected mutable receipt field: %s", variant, raw)
	}
	forged, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	mustMutateComposedReceipt(t, db, `UPDATE workflow_status SET output=?1 WHERE workflow_uuid=?2`, base64.StdEncoding.EncodeToString(forged), workflow)
}

func TestSessionAllocateGovernedComposedUsesSameReducer(t *testing.T) {
	ctx := context.Background()
	tr, actor := openGovernedTracker(t)
	root := initializeRoot(t, tr, actor)
	request := composedGovernedRequest("composed-session", actor, root, 2)
	result, err := tr.As(actor, root.AssignmentRow.JournalID).AllocateGovernedComposedBatch(ctx, request)
	if err != nil {
		t.Fatalf("standalone composed allocation: %v", err)
	}
	children := result.Closure().Children()
	if len(children) != 2 {
		t.Fatalf("standalone composed closure children=%d, want 2", len(children))
	}
	for _, child := range children {
		if _, err := tr.Show(child.TaskID); err != nil {
			t.Fatalf("standalone composed child not persisted: %v", err)
		}
	}
	edges, err := tr.Edges(root.TaskID, nil)
	if err != nil {
		t.Fatalf("read standalone composed edge: %v", err)
	}
	if len(edges) != 1 || edges[0].TargetID != children[0].TaskID.String() {
		t.Fatalf("standalone composed edge=%+v, want root->child", edges)
	}
	if got := len(result.SupplementalResultSlots()); got != 4 {
		t.Fatalf("standalone result slots=%d, want 4", got)
	}
}

func TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name        string
		participant provenance.GovernedAllocationParticipant
		prepare     func(*provenance.GovernedAllocationComposedRequest)
	}{
		{
			name: "supplemental reducer failure",
			prepare: func(request *provenance.GovernedAllocationComposedRequest) {
				request.SupplementalEffects[3].ActivityAgentID = provenance.ActorID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("unregistered-composed-activity-agent"))}
			},
		},
		{
			name: "participant failure",
			participant: func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
				if _, err := tx.Exec(ctx, `INSERT INTO fused_governed_participant_audit (operation_id,anchor_journal_id,child_task_id) VALUES (?1,?2,?3)`, request.OperationID, closure.AnchorJournalID(), closure.Children()[0].TaskID.String()); err != nil {
					return err
				}
				return errors.New("composed participant dependency unavailable")
			},
			prepare: func(*provenance.GovernedAllocationComposedRequest) {},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "composed-rollback-"+strings.ReplaceAll(test.name, " ", "-"), test.participant)
			if test.participant != nil {
				createFusedGovernedParticipantAuditTable(t, db)
			}
			actor := registerGovernedActor(t, fused.Tracker(), "composed-rollback")
			if err := fused.Launch(); err != nil {
				t.Fatalf("launch fused allocator: %v", err)
			}
			root := initializeFusedRoot(t, fused, actor, "composed-rollback-root-"+strings.ReplaceAll(test.name, " ", "-"))
			request := composedGovernedRequest("composed-rollback-"+strings.ReplaceAll(test.name, " ", "-"), actor, root, 1)
			test.prepare(&request)
			if _, err := fused.RunAllocateComposed(ctx, "composed-rollback-workflow-"+strings.ReplaceAll(test.name, " ", "-"), root.AssignmentRow.JournalID, request); err == nil {
				t.Fatal("composed allocation succeeded despite injected failure")
			}
			child := request.Allocation.Children[0]
			for _, check := range []struct {
				name  string
				query string
				args  []any
			}{
				{"child task", `SELECT COUNT(*) FROM tasks WHERE id=?1`, []any{child.TaskID.String()}},
				{"supplemental evidence", `SELECT COUNT(*) FROM journal_evidence WHERE task_id=?1`, []any{child.TaskID.String()}},
				{"allocation receipt", `SELECT COUNT(*) FROM governed_allocation_operations WHERE operation_id=?1`, []any{request.Allocation.OperationID}},
			} {
				t.Run(check.name, func(t *testing.T) {
					if got := countFusedGovernedRows(t, db, check.query, check.args...); got != 0 {
						t.Fatalf("rolled-back %s rows=%d, want 0", check.name, got)
					}
				})
			}
			if test.participant != nil && countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM fused_governed_participant_audit`) != 0 {
				t.Fatal("participant row survived rollback")
			}
			if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, "composed-rollback-workflow-"+strings.ReplaceAll(test.name, " ", "-")); got != 0 {
				t.Fatalf("successful operation_outputs after rollback=%d, want 0", got)
			}
		})
	}
}

func TestFusedGovernedAllocationComposedConflictsAndDefensiveCopies(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "composed-conflicts")
	actor := registerGovernedActor(t, fused.Tracker(), "composed-conflicts")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-conflicts-root")
	request := composedGovernedRequest("composed-conflicts", actor, root, 1)
	first, err := fused.RunAllocateComposed(ctx, "composed-conflicts-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("first composed allocation: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*provenance.GovernedAllocationComposedRequest)
	}{
		{
			name: "payload",
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects[0].Payload = []byte(`{"kind":"changed"}`)
			},
		},
		{
			name: "order",
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects[0], value.SupplementalEffects[1] = value.SupplementalEffects[1], value.SupplementalEffects[0]
			},
		},
		{
			name: "result slot",
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects[2].ResultSlot = "changed-slot"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			changed.SupplementalEffects = append([]provenance.Effect(nil), request.SupplementalEffects...)
			test.mutate(&changed)
			if _, err := fused.RunAllocateComposed(ctx, "composed-conflicts-workflow-"+test.name, root.AssignmentRow.JournalID, changed); err == nil {
				t.Fatalf("changed %s did not conflict", test.name)
			} else {
				mustGovernedError(t, err, provenance.GovernedAllocationConflict)
			}
		})
	}

	request.SupplementalEffects[0].Payload[0] = '!'
	request.SupplementalEffects[0].ContentDigest[0] = '!'
	child := first.Closure().Children()[0]
	var payload string
	if err := db.QueryRow(`SELECT payload FROM journal_evidence WHERE task_id=?1`, child.TaskID.String()).Scan(&payload); err != nil {
		t.Fatalf("read persisted evidence payload: %v", err)
	}
	if payload != `{"kind":"assignment-command"}` {
		t.Fatalf("caller mutation changed persisted payload=%q", payload)
	}
	slots := first.SupplementalResultSlots()
	slots[0].Slot = "mutated"
	if slots[0].TaskID != nil {
		slots[0].TaskID.Namespace = "mutated"
	}
	if got := first.SupplementalResultSlots()[0].Slot; got == "mutated" {
		t.Fatal("mutating returned result slots changed composed result")
	}
}

func TestGovernedAllocationComposedRejectsUnsupportedAndUnrelatedReferencesBeforeAllocation(t *testing.T) {
	ctx := context.Background()
	participantCalls := 0
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "composed-reject", func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		participantCalls++
		return nil
	})
	actor := registerGovernedActor(t, fused.Tracker(), "composed-reject")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-reject-root")
	// Each case pins the exact rejection it proves: the typed kind, the boundary
	// that rejected it, and the reason. "some error was returned" would pass just
	// as happily if a request were rejected for the wrong reason, at the wrong
	// boundary, or by an unrelated fixture bug.
	for _, test := range []struct {
		name       string
		beforeDBOS bool
		wantKind   provenance.GovernedAllocationErrorKind
		wantWhere  string
		wantWhy    string
		mutate     func(*provenance.GovernedAllocationComposedRequest)
	}{
		{
			name:       "task creation effect",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].Sort`,
			wantWhy:    `effect sort task_create is not permitted by static`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectTaskCreate, TaskID: value.Allocation.Children[0].TaskID, Title: "forbidden", Type: provenance.TaskTypeTask, Priority: provenance.PriorityMedium, Phase: provenance.PhaseWorkerSlices}}
			},
		},
		{
			name:       "allocated task creation effect",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].Sort`,
			wantWhy:    `effect sort task_create_allocated is not permitted by static`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectTaskCreateAllocated, TaskID: value.Allocation.Children[0].TaskID, Title: "forbidden", Type: provenance.TaskTypeTask, Priority: provenance.PriorityMedium, Phase: provenance.PhaseWorkerSlices, ResultSlot: "forbidden-task"}}
			},
		},
		{
			name:       "assignment effect",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].Sort`,
			wantWhy:    `effect sort assignment_start is not permitted by static`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectAssignmentStart, TaskID: value.Allocation.Children[0].TaskID, AssignmentID: value.Allocation.Children[0].AssignmentID, SlotID: provenance.SlotOwnerResponsibility, Occupant: actor}}
			},
		},
		{
			name:       "assignment end effect",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].Sort`,
			wantWhy:    `effect sort assignment_end is not permitted by static`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectAssignmentEnd, TaskID: value.Allocation.Children[0].TaskID, AssignmentID: value.Allocation.Children[0].AssignmentID, SlotID: provenance.SlotOwnerResponsibility}}
			},
		},
		{
			name:       "unsupported decision effect",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].Sort`,
			wantWhy:    `effect sort decision is not permitted by static`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectDecision, TaskID: value.Allocation.Children[0].TaskID, DecisionKind: "provenance.unsupported"}}
			},
		},
		{
			name:       "unrelated task event",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationAuthority,
			wantWhere:  `composed supplemental reference validation`,
			wantWhy:    `supplemental effect 0 references unrelated task`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectTaskEvent, TaskID: provenance.TaskID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("unrelated"))}, EventKind: "provenance.unrelated"}}
			},
		},
		{
			name:       "unrelated task context",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationAuthority,
			wantWhere:  `composed supplemental reference validation`,
			wantWhy:    `supplemental effect 2 references unrelated task`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				unrelated := provenance.TaskID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("unrelated-context"))}
				context, err := provenance.TaskContext(unrelated)
				if err != nil {
					t.Fatalf("construct unrelated task context: %v", err)
				}
				value.SupplementalEffects[2].Contexts = []provenance.EventContext{context}
			},
		},
		{
			name:       "mutation-family task event",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].EventKind`,
			wantWhy:    `is reducer-derived and cannot be supplied as a generic supplemental task event`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{
					Sort: provenance.EffectTaskEvent, TaskID: value.Allocation.Children[0].TaskID,
					EventKind: provenance.EventKindEdgeAdded,
				}}
			},
		},
		{
			name:       "task-created pseudo event",
			beforeDBOS: true,
			wantKind:   provenance.GovernedAllocationValidation,
			wantWhere:  `canonical request validation: SupplementalEffects[0].EventKind`,
			wantWhy:    `is reducer-derived and cannot be supplied as a generic supplemental task event`,
			mutate: func(value *provenance.GovernedAllocationComposedRequest) {
				value.SupplementalEffects = []provenance.Effect{{
					Sort: provenance.EffectTaskEvent, TaskID: value.Allocation.Children[0].TaskID,
					EventKind: provenance.EventKindTaskCreated,
				}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := composedGovernedRequest("composed-reject-"+strings.ReplaceAll(test.name, " ", "-"), actor, root, 1)
			test.mutate(&request)
			before := snapshotGovernedTables(t, db)
			beforeParticipants := participantCalls
			_, err := fused.RunAllocateComposed(ctx, "composed-reject-workflow-"+strings.ReplaceAll(test.name, " ", "-"), root.AssignmentRow.JournalID, request)
			if err == nil {
				t.Fatal("invalid composition was accepted")
			}
			var governed *provenance.GovernedAllocationError
			if !errors.As(err, &governed) {
				t.Fatalf("rejection is not a typed governed-allocation error: %v", err)
			}
			if governed.Kind != test.wantKind {
				t.Errorf("rejection kind = %s, want %s: %v", governed.Kind, test.wantKind, err)
			}
			if governed.Where != test.wantWhere {
				t.Errorf("rejection boundary = %q, want %q", governed.Where, test.wantWhere)
			}
			if !strings.Contains(governed.Why, test.wantWhy) {
				t.Errorf("rejection reason = %q, want it to contain %q", governed.Why, test.wantWhy)
			}
			assertNoGovernedWrites(t, before, db)
			if participantCalls != beforeParticipants {
				t.Fatalf("rejected composition invoked participant: before=%d after=%d", beforeParticipants, participantCalls)
			}
			if test.beforeDBOS {
				workflowID := "composed-reject-workflow-" + strings.ReplaceAll(test.name, " ", "-")
				if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, workflowID); got != 0 {
					t.Fatalf("unsupported effect entered DBOS workflow: operation_outputs=%d, want 0", got)
				}
			}
		})
	}
}

func TestComposedAllocationReservesItsDerivedInternalOperationID(t *testing.T) {
	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "composed-reserved-operation")
	tr := fused.Tracker()
	actor := registerGovernedActor(t, tr, "composed-reserved-operation")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-reserved-operation-root")
	request := composedGovernedRequest("composed-reserved-operation", actor, root, 1)
	reserved := composedSupplementOperationIDForTest(request.Allocation.OperationID)

	before := snapshotGovernedTables(t, db)
	_, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:        reserved,
		ActorID:            actor,
		AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
		CommandDigest:      []byte("attempt-to-claim-composed-internal-operation"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectTaskEvent, TaskID: root.TaskID, EventKind: "governed.reserved-attempt",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved internal") {
		t.Fatalf("generic Journal.Apply reserved operation error=%v, want reserved-internal rejection", err)
	}
	assertNoGovernedWrites(t, before, db)
	assertGovernedOperationAbsent(t, tr, reserved)

	if _, err := fused.RunAllocateComposed(ctx, "composed-reserved-operation-workflow", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("composed allocation could not use its reserved internal operation: %v", err)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM journal_operations WHERE operation_id=?1`, reserved); got != 1 {
		t.Fatalf("internal composed operation rows=%d, want 1", got)
	}
	if _, err := tr.Journal().Apply(provenance.OperationInput{
		OperationID:        "ordinary-external-operation",
		ActorID:            actor,
		AuthorityJournalID: ptr(root.AssignmentRow.JournalID),
		CommandDigest:      []byte("ordinary-external-operation"),
		Effects: []provenance.Effect{{
			Sort: provenance.EffectTaskEvent, TaskID: root.TaskID, EventKind: "governed.ordinary",
		}},
	}); err != nil {
		t.Fatalf("ordinary external Journal.Apply was rejected: %v", err)
	}
}

func TestFusedGovernedAllocationComposedExactReopenReplayIsStableAndSkipsParticipant(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "composed-reopen.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	var calls int
	participant := provenance.GovernedAllocationParticipant(func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		calls++
		return nil
	})
	open := func(t *testing.T) *provenance.FusedGovernedAllocator {
		t.Helper()
		allocator, err := provenance.OpenFusedGovernedAllocator(ctx, provenance.FusedGovernedAllocatorConfig{
			SQLiteDSN: dsn, AppName: "provenance-composed-reopen", ApplicationVersion: "test-v1", Logger: slog.Default(), Participant: participant,
		})
		if err != nil {
			t.Fatalf("open fused allocator: %v", err)
		}
		if err := allocator.Launch(); err != nil {
			_ = allocator.Close(30 * time.Second)
			t.Fatalf("launch fused allocator: %v", err)
		}
		return allocator
	}

	firstAllocator := open(t)
	actor := registerGovernedActor(t, firstAllocator.Tracker(), "composed-reopen")
	root := initializeFusedRoot(t, firstAllocator, actor, "composed-reopen-root")
	request := composedGovernedRequest("composed-reopen", actor, root, 1)
	request.SupplementalEffects[0].TaskID = provenance.TaskID{}
	first, err := firstAllocator.RunAllocateComposed(ctx, "composed-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("first composed allocation: %v", err)
	}
	if calls != 1 {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("participant calls after first allocation=%d, want 1", calls)
	}
	immediate, err := firstAllocator.RunAllocateComposed(ctx, "composed-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("immediate exact replay: %v", err)
	}
	assertSameClosure(t, first.Closure(), immediate.Closure())
	if !reflect.DeepEqual(first.SupplementalResultSlots(), immediate.SupplementalResultSlots()) || calls != 1 {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("immediate replay changed receipt or reran participant: calls=%d", calls)
	}
	distinct, err := firstAllocator.RunAllocateComposed(ctx, "composed-reopen-distinct-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("distinct-workflow exact replay: %v", err)
	}
	assertSameClosure(t, first.Closure(), distinct.Closure())
	if !reflect.DeepEqual(first.SupplementalResultSlots(), distinct.SupplementalResultSlots()) || calls != 1 {
		_ = firstAllocator.Close(30 * time.Second)
		t.Fatalf("distinct exact retry changed receipt or reran participant: calls=%d", calls)
	}
	if err := firstAllocator.Close(30 * time.Second); err != nil {
		t.Fatalf("close first fused allocator: %v", err)
	}

	reopened := open(t)
	t.Cleanup(func() { _ = reopened.Close(30 * time.Second) })
	replayed, err := reopened.RunAllocateComposed(ctx, "composed-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("reopened exact replay: %v", err)
	}
	assertSameClosure(t, first.Closure(), replayed.Closure())
	if !reflect.DeepEqual(first.SupplementalResultSlots(), replayed.SupplementalResultSlots()) || !reflect.DeepEqual(first.SupplementalEmittedEvents(), replayed.SupplementalEmittedEvents()) {
		t.Fatalf("reopened replay changed composed receipt: first=%+v replay=%+v", first, replayed)
	}
	if calls != 1 {
		t.Fatalf("participant calls after reopened exact replay=%d, want 1", calls)
	}
	inspection, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspection.Close() })
	if got := countFusedGovernedRows(t, inspection, `SELECT COUNT(*) FROM journal_evidence WHERE evidence_kind='provenance.assignment.command' AND task_id IS NULL`); got != 1 {
		t.Fatalf("untasked evidence rows=%d, want 1 SQL NULL task", got)
	}
}

func composedGovernedRequest(name string, actor provenance.ActorID, parent provenance.GovernedChildBinding, childCount int) provenance.GovernedAllocationComposedRequest {
	allocation := governedRequest(name, actor, parent.AssignmentID, childCount)
	child := allocation.Children[0]
	activityID := provenance.ActivityID{Namespace: "governed", UUID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("activity/"+name))}
	return provenance.GovernedAllocationComposedRequest{
		Version:    provenance.GovernedAllocationCompositionV1,
		Allocation: allocation,
		SupplementalEffects: []provenance.Effect{
			{Sort: provenance.EffectEvidence, TaskID: child.TaskID, EvidenceKind: "provenance.assignment.command", ContentDigest: []byte("assignment-command-" + name), Payload: []byte(`{"kind":"assignment-command"}`), ResultSlot: "evidence"},
			{Sort: provenance.EffectEdgeAdd, TaskID: parent.TaskID, EdgeTargetID: child.TaskID.String(), EdgeRelKind: provenance.EdgeBlockedBy, ResultSlot: "edge"},
			{Sort: provenance.EffectTaskEvent, TaskID: child.TaskID, EventKind: "provenance.slice.created", Payload: []byte(`{"kind":"slice-created"}`), ResultSlot: "slice-event"},
			{Sort: provenance.EffectActivityCreate, ActivityID: activityID, ActivityAgentID: actor, ActivityPhase: provenance.PhaseWorkerSlices, ActivityStage: provenance.StageInProgress, ActivityNotes: "composed allocation", ResultSlot: "activity"},
		},
	}
}

func composedSupplementOperationIDForTest(external provenance.OperationID) provenance.OperationID {
	sum := sha256.Sum256(append([]byte("provenance.governed-allocation.supplement.v1\x00"), []byte(external)...))
	return provenance.OperationID("provenance.governed-supplement.v1." + fmt.Sprintf("%x", sum[:]))
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
	return openFusedAllocatorWithParticipantAndDatabase(t, name, nil)
}

func openFusedAllocatorWithParticipantAndDatabase(t *testing.T, name string, participant provenance.GovernedAllocationParticipant) (*provenance.FusedGovernedAllocator, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	allocator, err := provenance.OpenFusedGovernedAllocator(context.Background(), provenance.FusedGovernedAllocatorConfig{
		SQLiteDSN:          dsn,
		AppName:            "provenance-governed-" + name,
		ApplicationVersion: "test-v1",
		Logger:             slog.Default(),
		Participant:        participant,
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

func initializeFusedRoot(t *testing.T, fused *provenance.FusedGovernedAllocator, actor provenance.ActorID, name string) provenance.GovernedChildBinding {
	t.Helper()
	closure, err := fused.RunInitializeRoot(context.Background(), name+"-genesis-workflow", provenance.RootGenesisRequest{
		OperationID: provenance.OperationID(name + "-genesis"),
		ActorID:     actor,
		Command:     "test.genesis",
		Root:        governedChild(name+"-root", actor),
	})
	if err != nil {
		t.Fatalf("initialize fused root: %v", err)
	}
	root, ok := closure.Root()
	if !ok {
		t.Fatal("fused root closure has no root binding")
	}
	return root
}

func createFusedGovernedParticipantAuditTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE fused_governed_participant_audit (
		operation_id TEXT PRIMARY KEY,
		anchor_journal_id INTEGER NOT NULL,
		child_task_id TEXT NOT NULL
	) STRICT`); err != nil {
		t.Fatalf("create fused governed participant audit table: %v", err)
	}
}

func countFusedGovernedRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count fused governed rows with %q: %v", query, err)
	}
	return count
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
