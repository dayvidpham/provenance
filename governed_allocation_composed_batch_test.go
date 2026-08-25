package provenance_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// each test owns a private t.TempDir database and DBOS application name, and its
// participant counters live in its own closure.

func TestFusedGovernedAllocationComposedBatchCommitsOrderedCompleteClosureAndReplays(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participantChildren := 0
	participant := provenance.GovernedAllocationParticipant(func(_ context.Context, _ provenance.GovernedAllocationTransaction, _ provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		participantChildren = len(closure.Children())
		return nil
	})
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "composed-batch", participant)
	actor := registerGovernedActor(t, fused.Tracker(), "composed-batch")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-batch")
	request := composedGovernedRequest("composed-batch", actor, root, 3)

	first, err := fused.RunAllocateComposedBatch(ctx, "composed-batch-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run composed batch: %v", err)
	}
	children := first.Closure().Children()
	if len(children) != 3 || participantChildren != 3 {
		t.Fatalf("complete closure children=%d participant children=%d, want 3", len(children), participantChildren)
	}
	for i := range children {
		if children[i].Ordinal != i || children[i].TaskID != request.Allocation.Children[i].TaskID || children[i].AssignmentID != request.Allocation.Children[i].AssignmentID {
			t.Fatalf("child %d lost submitted order or identity: %+v", i, children[i])
		}
	}
	replay, err := fused.RunAllocateComposedBatch(ctx, "composed-batch-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("replay composed batch: %v", err)
	}
	if !replay.Replayed() || !first.Closure().Equal(replay.Closure()) || !reflect.DeepEqual(first.SupplementalResultSlots(), replay.SupplementalResultSlots()) || !reflect.DeepEqual(first.SupplementalEmittedEvents(), replay.SupplementalEmittedEvents()) {
		t.Fatal("exact replay did not preserve the complete ordered closure and shared supplemental receipt")
	}
	beforeTasks := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`)
	beforeJournal := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM journal`)
	beforeOutputs := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`)

	for _, mutate := range []func(*provenance.GovernedAllocationComposedRequest){
		func(changed *provenance.GovernedAllocationComposedRequest) {
			// Supplements intentionally retain the old child reference. Existing
			// workflow identity must win over this now-stale reference preflight.
			changed.Allocation.Children[0].TaskID = governedChild("composed-batch-stale-reference", changed.Allocation.Children[0].Occupant).TaskID
		},
		func(changed *provenance.GovernedAllocationComposedRequest) {
			changed.Allocation.Children[0], changed.Allocation.Children[1] = changed.Allocation.Children[1], changed.Allocation.Children[0]
		},
		func(changed *provenance.GovernedAllocationComposedRequest) {
			changed.SupplementalEffects[0].Payload = []byte(`{"changed":true}`)
		},
		func(changed *provenance.GovernedAllocationComposedRequest) {
			changed.Allocation.Children[1].Title += " changed metadata"
		},
	} {
		changed := request
		changed.Allocation.Children = append([]provenance.GovernedChildSpec(nil), request.Allocation.Children...)
		changed.SupplementalEffects = append([]provenance.Effect(nil), request.SupplementalEffects...)
		mutate(&changed)
		_, err := fused.RunAllocateComposedBatch(ctx, "composed-batch-workflow", root.AssignmentRow.JournalID, changed)
		mustBatchGovernedError(t, err, provenance.GovernedAllocationConflict, request.Allocation.OperationID, "RunAllocateComposedBatch workflow replay identity")
		if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`); got != beforeTasks {
			t.Fatalf("identity conflict changed tasks: before=%d after=%d", beforeTasks, got)
		}
		if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM journal`); got != beforeJournal {
			t.Fatalf("identity conflict changed journal: before=%d after=%d", beforeJournal, got)
		}
		if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs`); got != beforeOutputs {
			t.Fatalf("identity conflict changed DBOS outputs: before=%d after=%d", beforeOutputs, got)
		}
	}
	changed := request
	changed.Allocation.Children = append([]provenance.GovernedChildSpec(nil), request.Allocation.Children...)
	changed.SupplementalEffects = append([]provenance.Effect(nil), request.SupplementalEffects...)
	changed.Allocation.Children[0].TaskID = governedChild("composed-batch-distinct-workflow-stale-reference", changed.Allocation.Children[0].Occupant).TaskID
	_, err = fused.RunAllocateComposedBatch(ctx, "composed-batch-distinct-workflow", root.AssignmentRow.JournalID, changed)
	mustBatchGovernedError(t, err, provenance.GovernedAllocationConflict, request.Allocation.OperationID, "governed operation identity check")
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`); got != beforeTasks {
		t.Fatalf("distinct-workflow identity conflict changed tasks: before=%d after=%d", beforeTasks, got)
	}
}

func TestFusedComposedBatchConditionIsCanonicalAtomicAndReplaySafe(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participant := provenance.GovernedAllocationParticipant(func(ctx context.Context, tx provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, _ provenance.OperationClosure) error {
		_, err := tx.Exec(ctx, `INSERT INTO condition_participant_audit(operation_id) VALUES (?1)`, request.OperationID)
		return err
	})
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "composed-condition", participant)
	if _, err := db.Exec(`CREATE TABLE condition_participant_audit(operation_id TEXT PRIMARY KEY) STRICT`); err != nil {
		t.Fatal(err)
	}
	actor := registerGovernedActor(t, fused.Tracker(), "composed-condition")
	if err := fused.Launch(); err != nil {
		t.Fatal(err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-condition")
	condition := provenance.Condition{Kind: provenance.ConditionCurrentFact, Selector: provenance.FactSelector{
		Kind: provenance.FactEvidence, EvidenceKind: "provenance.review.round", Filter: provenance.FactFilter{TaskScope: provenance.FactTaskScope{Kind: provenance.FactTaskUnscoped}},
	}, AssertedJournalID: 0}
	winner := composedGovernedRequest("composed-condition-winner", actor, root, 2)
	winner.Conditions = []provenance.Condition{condition}
	winner.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectEvidence, EvidenceKind: "provenance.review.round", ContentDigest: []byte("round-1"), Payload: []byte(`{"round":1}`), ResultSlot: "round"}}
	first, err := fused.RunAllocateComposedBatch(ctx, "composed-condition-winner-workflow", root.AssignmentRow.JournalID, winner)
	if err != nil {
		t.Fatalf("condition-protected winner: %v", err)
	}

	changed := winner
	changed.Conditions = []provenance.Condition{{Kind: provenance.ConditionExactFact, Selector: condition.Selector, AssertedJournalID: 1}}
	if _, err := fused.RunAllocateComposedBatch(ctx, "composed-condition-winner-workflow", root.AssignmentRow.JournalID, changed); err == nil {
		t.Fatal("changed condition attached to an existing workflow")
	}
	// Current state no longer satisfies absence, but exact replay must reconstruct
	// the original durable receipt without reevaluating it.
	replayed, err := fused.RunAllocateComposedBatch(ctx, "composed-condition-winner-workflow", root.AssignmentRow.JournalID, winner)
	if err != nil || !replayed.Replayed() || !first.Closure().Equal(replayed.Closure()) {
		t.Fatalf("exact condition replay changed or re-evaluated state: replay=%v err=%v", replayed.Replayed(), err)
	}
	distinctReplay, err := fused.RunAllocateComposedBatch(ctx, "composed-condition-winner-distinct-replay", root.AssignmentRow.JournalID, winner)
	if err != nil || !distinctReplay.Replayed() || !first.Closure().Equal(distinctReplay.Closure()) {
		t.Fatalf("distinct-workflow exact replay re-evaluated changed fact state: replay=%v err=%v", distinctReplay.Replayed(), err)
	}

	loser := composedGovernedRequest("composed-condition-loser", actor, root, 2)
	loser.Conditions = []provenance.Condition{condition}
	loser.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectEvidence, EvidenceKind: "provenance.review.round", ContentDigest: []byte("round-2"), Payload: []byte(`{"round":2}`)}}
	beforeTasks := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`)
	beforeParticipant := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM condition_participant_audit`)
	_, err = fused.RunAllocateComposedBatch(ctx, "composed-condition-loser-workflow", root.AssignmentRow.JournalID, loser)
	var failure *provenance.ConditionFailure
	if !errors.As(err, &failure) || failure.Kind != provenance.ConditionCurrentFact || failure.ActualJournalID == 0 {
		t.Fatalf("loser error=%v, want typed zero-write current-fact failure", err)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`); got != beforeTasks {
		t.Fatalf("failed condition leaked child allocations: before=%d after=%d", beforeTasks, got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM condition_participant_audit`); got != beforeParticipant {
		t.Fatalf("failed condition leaked participant audit: before=%d after=%d", beforeParticipant, got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1 AND error IS NULL`, "composed-condition-loser-workflow"); got != 0 {
		t.Fatalf("failed condition left successful operation output rows=%d", got)
	}
}

func TestFusedComposedReferenceScopeProvesDescendantAndRejectsUnrelated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fused, _ := openFusedAllocatorWithDatabase(t, "composed-reference-scope")
	actor := registerGovernedActor(t, fused.Tracker(), "composed-reference-scope")
	if err := fused.Launch(); err != nil {
		t.Fatal(err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-reference-scope")
	descendantClosure, err := fused.RunAllocate(ctx, "composed-reference-descendant-workflow", root.AssignmentRow.JournalID,
		governedRequest("composed-reference-descendant", actor, root.AssignmentID, 1))
	if err != nil {
		t.Fatal(err)
	}
	descendant := descendantClosure.Children()[0]
	request := composedGovernedRequest("composed-reference-valid", actor, root, 1)
	request.ReferenceScope = provenance.GovernedAllocationReferenceScope{
		Kind: provenance.GovernedAllocationReferenceDescendants, Subjects: []provenance.TaskID{descendant.TaskID},
	}
	request.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectTaskEvent, TaskID: descendant.TaskID, EventKind: "provenance.reference.recorded"}}
	if _, err := fused.RunAllocateComposedBatch(ctx, "composed-reference-valid-workflow", root.AssignmentRow.JournalID, request); err != nil {
		t.Fatalf("ancestor-authorized descendant reference was rejected: %v", err)
	}

	unrelated := governedChild("composed-reference-unrelated", actor).TaskID
	rejected := composedGovernedRequest("composed-reference-invalid", actor, root, 1)
	rejected.ReferenceScope = provenance.GovernedAllocationReferenceScope{
		Kind: provenance.GovernedAllocationReferenceDescendants, Subjects: []provenance.TaskID{unrelated},
	}
	rejected.SupplementalEffects = []provenance.Effect{{Sort: provenance.EffectTaskEvent, TaskID: unrelated, EventKind: "provenance.reference.recorded"}}
	if _, err := fused.RunAllocateComposedBatch(ctx, "composed-reference-invalid-workflow", root.AssignmentRow.JournalID, rejected); err == nil {
		t.Fatal("unrelated explicit reference scope was accepted")
	}
}

func mustBatchGovernedError(t *testing.T, err error, kind provenance.GovernedAllocationErrorKind, operation provenance.OperationID, stage string) {
	t.Helper()
	var governed *provenance.GovernedAllocationError
	if !errors.As(err, &governed) {
		t.Fatalf("error %v is not a governed allocation error", err)
	}
	if governed.Kind != kind || governed.Operation != operation || !strings.Contains(governed.Where, stage) || governed.Why == "" || governed.Impact == "" || governed.Fix == "" {
		t.Fatalf("batch diagnostic mismatch: kind=%v operation=%q where=%q why=%q impact=%q fix=%q", governed.Kind, governed.Operation, governed.Where, governed.Why, governed.Impact, governed.Fix)
	}
}

func TestFusedGovernedAllocationComposedBatchReopenReplayPreservesOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "composed-batch-reopen.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	config := provenance.FusedGovernedAllocatorConfig{SQLiteDSN: dsn, AppName: "composed-batch-reopen", ApplicationVersion: "test-v1", Logger: slog.Default()}
	firstAllocator, err := provenance.OpenFusedGovernedAllocator(ctx, config)
	if err != nil {
		t.Fatalf("open first fused allocator: %v", err)
	}
	actor := registerGovernedActor(t, firstAllocator.Tracker(), "composed-batch-reopen")
	if err := firstAllocator.Launch(); err != nil {
		t.Fatalf("launch first fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, firstAllocator, actor, "composed-batch-reopen")
	request := composedGovernedRequest("composed-batch-reopen", actor, root, 3)
	first, err := firstAllocator.RunAllocateComposedBatch(ctx, "composed-batch-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("run first composed batch: %v", err)
	}
	if err := firstAllocator.Close(30 * time.Second); err != nil {
		t.Fatalf("close first fused allocator: %v", err)
	}

	reopened, err := provenance.OpenFusedGovernedAllocator(ctx, config)
	if err != nil {
		t.Fatalf("reopen fused allocator: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(30 * time.Second) })
	if err := reopened.Launch(); err != nil {
		t.Fatalf("launch reopened fused allocator: %v", err)
	}
	replay, err := reopened.RunAllocateComposedBatch(ctx, "composed-batch-reopen-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("replay reopened composed batch: %v", err)
	}
	if !replay.Replayed() || !first.Closure().Equal(replay.Closure()) || !reflect.DeepEqual(first.SupplementalResultSlots(), replay.SupplementalResultSlots()) || !reflect.DeepEqual(first.SupplementalEmittedEvents(), replay.SupplementalEmittedEvents()) {
		t.Fatal("reopen replay changed the complete ordered closure or shared supplemental receipt")
	}
}

func TestFusedGovernedAllocationComposedBatchInvalidSecondChildWritesNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fused, db := openFusedAllocatorWithDatabase(t, "composed-batch-invalid")
	actor := registerGovernedActor(t, fused.Tracker(), "composed-batch-invalid")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-batch-invalid")
	request := composedGovernedRequest("composed-batch-invalid", actor, root, 2)
	request.Allocation.Children[1].TaskID = request.Allocation.Children[0].TaskID
	beforeTasks := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`)

	_, err := fused.RunAllocateComposedBatch(ctx, "composed-batch-invalid-workflow", root.AssignmentRow.JournalID, request)
	mustGovernedError(t, err, provenance.GovernedAllocationValidation)
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM tasks`); got != beforeTasks {
		t.Fatalf("invalid second child partially allocated tasks: before=%d after=%d", beforeTasks, got)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, "composed-batch-invalid-workflow"); got != 0 {
		t.Fatalf("invalid second child created %d DBOS operation outputs, want zero", got)
	}
}

func TestGovernedAllocationComposedRejectsMoreThanOneChild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fused, _ := openFusedAllocatorWithDatabase(t, "composed-one-child-entry")
	actor := registerGovernedActor(t, fused.Tracker(), "composed-one-child-entry")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-one-child-entry")
	single := composedGovernedRequest("composed-one-child", actor, root, 1)
	result, err := fused.RunAllocateComposed(ctx, "composed-one-child-workflow", root.AssignmentRow.JournalID, single)
	if err != nil || len(result.Closure().Children()) != 1 {
		t.Fatalf("one-child composed entry point failed: result=%+v err=%v", result, err)
	}
	multi := composedGovernedRequest("composed-two-child", actor, root, 2)
	_, err = fused.RunAllocateComposed(ctx, "composed-two-child-workflow", root.AssignmentRow.JournalID, multi)
	mustGovernedError(t, err, provenance.GovernedAllocationValidation)
}
