package sqlite

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// operations_batch_test.go proves the batched-apply contract on the concrete
// store: one transaction, a per-record outcome aligned by index, the §9.4
// short-circuit and §11 conflict taxonomy preserved per record, all-or-nothing
// rollback, and that the batch leaves the store passing the same integrity and
// currency-ledger checks a per-record Apply would.

const batchDecisionKind journal.DecisionKind = "provenance.test.batch.decision"

func batchDecisionInput(operationID string, actor journal.ActorID, authority journal.JournalID, payload string) journal.OperationInput {
	return journal.OperationInput{
		OperationID:        journal.OperationID(operationID),
		ActorID:            actor,
		AuthorityJournalID: &authority,
		CommandDigest:      []byte(operationID),
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort:         journal.EffectDecision,
			DecisionKind: batchDecisionKind,
			Payload:      []byte(payload),
			ResultSlot:   "decision",
		}},
	}
}

// TestApplyBatchCommitsEveryRecordWithPerRecordOutcomes pins the happy path: a
// batch of distinct new records commits in one call and every per-record outcome
// is BatchRecordCommitted, with a distinct reconstructed anchor per record.
func TestApplyBatchCommitsEveryRecordWithPerRecordOutcomes(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	const n = 24
	inputs := make([]journal.OperationInput, n)
	for i := range inputs {
		inputs[i] = batchDecisionInput(fmt.Sprintf("batch-commit-%02d", i), actor, authority, fmt.Sprintf(`{"i":%d}`, i))
	}

	result, err := db.ApplyBatch(inputs)
	if err != nil {
		t.Fatalf("ApplyBatch: %v", err)
	}
	if !result.Committed {
		t.Fatalf("batch of %d new records did not commit: %+v", n, result)
	}
	if len(result.Records) != n {
		t.Fatalf("per-record results = %d, want %d (one per input)", len(result.Records), n)
	}
	anchors := make(map[journal.JournalID]struct{}, n)
	for i, record := range result.Records {
		if record.Outcome != journal.BatchRecordCommitted {
			t.Fatalf("record %d outcome = %s, want committed (err=%v)", i, record.Outcome, record.Err)
		}
		if record.Err != nil {
			t.Fatalf("record %d committed with error %v", i, record.Err)
		}
		if record.Result.ShortCircuited {
			t.Fatalf("record %d reported ShortCircuited on a new commit", i)
		}
		if record.Result.AnchorJournalID == 0 {
			t.Fatalf("record %d committed with a zero anchor", i)
		}
		if _, dup := anchors[record.Result.AnchorJournalID]; dup {
			t.Fatalf("record %d reused anchor %d", i, record.Result.AnchorJournalID)
		}
		anchors[record.Result.AnchorJournalID] = struct{}{}
	}
	// Every record is independently visible by its OperationID.
	for i, in := range inputs {
		lookup, err := db.LookupCommitted(in.OperationID)
		if err != nil {
			t.Fatalf("LookupCommitted(%q): %v", in.OperationID, err)
		}
		if lookup.Kind != journal.CommittedExact {
			t.Fatalf("record %d lookup kind = %s, want CommittedExact", i, lookup.Kind)
		}
		if lookup.AnchorJournalID != result.Records[i].Result.AnchorJournalID {
			t.Fatalf("record %d lookup anchor = %d, want %d", i, lookup.AnchorJournalID, result.Records[i].Result.AnchorJournalID)
		}
	}
}

// TestApplyBatchShortCircuitsIdenticalAndConflictsOnDifferentIdentity pins the
// §9.4/§11 per-record taxonomy: an identical replay short-circuits (durable, no
// new write) and a same-OperationID-different-identity reuse is a typed conflict.
func TestApplyBatchShortCircuitsIdenticalAndConflictsOnDifferentIdentity(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	original := batchDecisionInput("batch-replay", actor, authority, `{"v":1}`)
	first, err := db.Apply(original)
	if err != nil {
		t.Fatalf("Apply original: %v", err)
	}

	// Identical input: an exact §9.4 replay.
	short, err := db.ApplyBatch([]journal.OperationInput{original})
	if err != nil {
		t.Fatalf("ApplyBatch identical replay: %v", err)
	}
	if !short.Committed || short.Records[0].Outcome != journal.BatchRecordShortCircuited {
		t.Fatalf("identical replay outcome = %+v, want a committed short-circuit", short)
	}
	if !short.Records[0].Result.ShortCircuited || short.Records[0].Result.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("short-circuit result = %+v, want the original anchor %d with ShortCircuited", short.Records[0].Result, first.AnchorJournalID)
	}

	// Same OperationID, different payload: a typed conflict that rolls the batch
	// back and preserves errors.Is / errors.As.
	conflicting := batchDecisionInput("batch-replay", actor, authority, `{"v":2}`)
	conflict, err := db.ApplyBatch([]journal.OperationInput{conflicting})
	if err == nil {
		t.Fatal("ApplyBatch with a conflicting identity returned no error")
	}
	if conflict.Committed {
		t.Fatal("conflicting batch reported Committed")
	}
	if conflict.Records[0].Outcome != journal.BatchRecordConflict {
		t.Fatalf("conflict outcome = %s, want conflict", conflict.Records[0].Outcome)
	}
	if !errors.Is(err, journal.ErrOperationConflict) {
		t.Fatalf("conflict error = %v, want errors.Is ErrOperationConflict", err)
	}
	var operationConflict *journal.OperationConflict
	if !errors.As(err, &operationConflict) {
		t.Fatalf("conflict error = %v, want errors.As *OperationConflict", err)
	}
	var batchErr *journal.BatchApplyError
	if !errors.As(err, &batchErr) || batchErr.Index != 0 {
		t.Fatalf("conflict error = %v, want *BatchApplyError at index 0", err)
	}
	// The original operation is untouched and no second write exists.
	lookup, err := db.LookupCommitted(original.OperationID)
	if err != nil || lookup.AnchorJournalID != first.AnchorJournalID {
		t.Fatalf("conflicting batch changed the committed operation: %+v, %v", lookup, err)
	}
}

// TestApplyBatchRollsBackWholeBatchOnIntraBatchFailure pins all-or-nothing: a
// failure in the last record leaves NONE of the earlier records durably written,
// and the earlier records are reported honestly as not recorded.
func TestApplyBatchRollsBackWholeBatchOnIntraBatchFailure(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	// Record 0 creates a task. Record 1 creates the SAME task, which fails. The
	// duplicate create proves record 1 observed record 0's in-transaction write,
	// and the rollback must remove it.
	task := ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()}
	createTask := func(opID string) journal.OperationInput {
		return journal.OperationInput{
			OperationID:        journal.OperationID(opID),
			ActorID:            actor,
			AuthorityJournalID: &authority,
			CommandDigest:      []byte(opID),
			RecordedAt:         time.Now().UTC().UnixNano(),
			Effects: []journal.Effect{{
				Sort: journal.EffectTaskCreate, TaskID: task, Title: "batch task",
				Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped,
				ResultSlot: "task",
			}},
		}
	}
	inputs := []journal.OperationInput{
		batchDecisionInput("batch-rollback-0", actor, authority, `{}`),
		createTask("batch-rollback-1"),
		createTask("batch-rollback-2"),
		batchDecisionInput("batch-rollback-3", actor, authority, `{}`),
	}

	result, err := db.ApplyBatch(inputs)
	if err == nil {
		t.Fatal("batch with a duplicate task-create returned no error")
	}
	if result.Committed {
		t.Fatal("failed batch reported Committed")
	}
	var batchErr *journal.BatchApplyError
	if !errors.As(err, &batchErr) || batchErr.Index != 2 {
		t.Fatalf("batch error = %v, want *BatchApplyError at index 2", err)
	}
	wantOutcomes := []journal.BatchRecordOutcome{
		journal.BatchRecordNotRecorded,
		journal.BatchRecordNotRecorded,
		journal.BatchRecordFailed,
		journal.BatchRecordNotRecorded,
	}
	for i, want := range wantOutcomes {
		if got := result.Records[i].Outcome; got != want {
			t.Fatalf("record %d outcome = %s, want %s", i, got, want)
		}
	}
	if result.Records[2].Err == nil {
		t.Fatal("failing record carries no error")
	}
	// NOTHING was durably written: no operation, no task.
	for _, in := range inputs {
		lookup, err := db.LookupCommitted(in.OperationID)
		if err != nil {
			t.Fatalf("LookupCommitted(%q): %v", in.OperationID, err)
		}
		if lookup.Kind != journal.CommittedAbsent {
			t.Fatalf("operation %q survived the rollback: %+v", in.OperationID, lookup)
		}
	}
	if _, found, err := db.GetTask(task); err != nil || found {
		t.Fatalf("task survived the rollback: found=%v err=%v", found, err)
	}
	// The store still passes the same guards a per-record path guarantees.
	if err := db.VerifyIntegrity(); err != nil {
		t.Fatalf("VerifyIntegrity after rollback: %v", err)
	}
	if _, err := db.ReplayProjections(); err != nil {
		t.Fatalf("ReplayProjections after rollback: %v", err)
	}
}

// TestApplyBatchKeepsShortCircuitDurableWhenAnotherRecordFails pins the one
// asymmetry batching introduces: a short-circuited record is durable from its
// earlier transaction, so it stays reported as recorded even when the batch
// rolls back around it.
func TestApplyBatchKeepsShortCircuitDurableWhenAnotherRecordFails(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	already := batchDecisionInput("batch-durable-replay", actor, authority, `{"v":1}`)
	if _, err := db.Apply(already); err != nil {
		t.Fatalf("Apply pre-existing record: %v", err)
	}
	// A pre-existing task whose id a later record re-creates, making record 2 fail
	// deterministically without a conflict.
	task := ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.New()}
	if _, err := db.Apply(journal.OperationInput{
		OperationID: "batch-durable-preexisting-task", ActorID: actor, AuthorityJournalID: &authority,
		CommandDigest: []byte("batch-durable-preexisting-task"), RecordedAt: time.Now().UTC().UnixNano(),
		Effects: []journal.Effect{{
			Sort: journal.EffectTaskCreate, TaskID: task, Title: "pre-existing",
			Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped, ResultSlot: "task",
		}},
	}); err != nil {
		t.Fatalf("Apply pre-existing task: %v", err)
	}
	// Order: [new record, short-circuit, failing duplicate create]. The
	// short-circuit sits before the failure and must stay durable.
	inputs := []journal.OperationInput{
		batchDecisionInput("batch-durable-new", actor, authority, `{}`),
		already,
		{
			OperationID: "batch-durable-failing-create", ActorID: actor, AuthorityJournalID: &authority,
			CommandDigest: []byte("batch-durable-failing-create"), RecordedAt: time.Now().UTC().UnixNano(),
			Effects: []journal.Effect{{
				Sort: journal.EffectTaskCreate, TaskID: task, Title: "duplicate",
				Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped, ResultSlot: "task",
			}},
		},
	}

	result, err := db.ApplyBatch(inputs)
	if err == nil {
		t.Fatal("batch with a failing last record returned no error")
	}
	if result.Committed {
		t.Fatal("failed batch reported Committed")
	}
	if result.Records[0].Outcome != journal.BatchRecordNotRecorded {
		t.Fatalf("new record before the failure = %s, want not-recorded", result.Records[0].Outcome)
	}
	if result.Records[1].Outcome != journal.BatchRecordShortCircuited {
		t.Fatalf("short-circuited record = %s, want short-circuited (durable)", result.Records[1].Outcome)
	}
	if result.Records[2].Outcome != journal.BatchRecordFailed {
		t.Fatalf("failing record = %s, want failed", result.Records[2].Outcome)
	}
	// The pre-existing short-circuited record is still committed.
	lookup, err := db.LookupCommitted(already.OperationID)
	if err != nil || lookup.Kind != journal.CommittedExact {
		t.Fatalf("short-circuited record lost durability: %+v, %v", lookup, err)
	}
}

// TestApplyBatchConditionFailureMatchesStandaloneApply pins "at least as strict
// per record": a pre-condition failure inside a batch surfaces the identical
// typed *ConditionFailure a standalone Apply produces for the same input, and
// writes nothing.
func TestApplyBatchConditionFailureMatchesStandaloneApply(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	selector := journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: batchDecisionKind,
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}
	base, err := db.Apply(batchDecisionInput("batch-condition-base", actor, authority, `{"base":true}`))
	if err != nil {
		t.Fatalf("Apply base decision: %v", err)
	}
	baseFact := base.ResultSlots[0].ProducedJournalID
	// Advance the current fact so the base fact becomes stale.
	advancing := batchDecisionInput("batch-condition-advance", actor, authority, `{"advance":true}`)
	advancing.Conditions = []journal.Condition{{Kind: journal.ConditionCurrentFact, Selector: selector, AssertedJournalID: baseFact}}
	if _, err := db.Apply(advancing); err != nil {
		t.Fatalf("Apply advancing decision: %v", err)
	}

	stale := batchDecisionInput("batch-condition-stale", actor, authority, `{"stale":true}`)
	stale.Conditions = []journal.Condition{{Kind: journal.ConditionCurrentFact, Selector: selector, AssertedJournalID: baseFact}}

	_, standaloneErr := db.Apply(stale)
	var standalone *journal.ConditionFailure
	if !errors.As(standaloneErr, &standalone) {
		t.Fatalf("standalone Apply error = %v, want *ConditionFailure", standaloneErr)
	}

	result, batchErr := db.ApplyBatch([]journal.OperationInput{stale})
	var batched *journal.ConditionFailure
	if !errors.As(batchErr, &batched) {
		t.Fatalf("batch error = %v, want *ConditionFailure", batchErr)
	}
	if !errors.Is(batchErr, journal.ErrConditionFailed) {
		t.Fatalf("batch error = %v, want errors.Is ErrConditionFailed", batchErr)
	}
	if *batched != *standalone {
		t.Fatalf("batch condition failure = %+v, want the standalone %+v", *batched, *standalone)
	}
	if result.Committed || result.Records[0].Outcome != journal.BatchRecordFailed {
		t.Fatalf("condition-failed batch = %+v, want rolled-back failed record", result)
	}
	lookup, err := db.LookupCommitted(stale.OperationID)
	if err != nil || lookup.Kind != journal.CommittedAbsent {
		t.Fatalf("condition-failed record was written: %+v, %v", lookup, err)
	}
}

// TestApplyBatchRejectsEmptyAndOversizedBatches pins the bounds.
func TestApplyBatchRejectsEmptyAndOversizedBatches(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	if _, err := db.ApplyBatch(nil); !errors.Is(err, journal.ErrBatchEmpty) {
		t.Fatalf("empty batch error = %v, want ErrBatchEmpty", err)
	}
	oversized := make([]journal.OperationInput, journal.MaxBatchRecords+1)
	if _, err := db.ApplyBatch(oversized); !errors.Is(err, journal.ErrBatchTooLarge) {
		t.Fatalf("oversized batch error = %v, want ErrBatchTooLarge", err)
	}
}

// TestApplyBatchInputValidationWritesNothing pins the pre-transaction failure:
// an invalid record rejects the whole batch with no transaction opened and no
// write, and the failing index is named.
func TestApplyBatchInputValidationWritesNothing(t *testing.T) {
	t.Parallel()
	db := newJournalDB(t)
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	inputs := []journal.OperationInput{
		batchDecisionInput("batch-validate-0", actor, authority, `{}`),
		{OperationID: "bad\x01operation\x01id", ActorID: actor, AuthorityJournalID: &authority, CommandDigest: []byte("x")},
		batchDecisionInput("batch-validate-2", actor, authority, `{}`),
	}
	result, err := db.ApplyBatch(inputs)
	if err == nil {
		t.Fatal("batch with an invalid OperationID returned no error")
	}
	if !errors.Is(err, journal.ErrBatchApply) {
		t.Fatalf("validation batch error = %v, want errors.Is ErrBatchApply", err)
	}
	var batchErr *journal.BatchApplyError
	if !errors.As(err, &batchErr) || batchErr.Index != 1 {
		t.Fatalf("validation batch error = %v, want *BatchApplyError at index 1", err)
	}
	if result.Committed {
		t.Fatal("validation failure reported Committed")
	}
	if result.Records[1].Outcome != journal.BatchRecordFailed || result.Records[1].Err == nil {
		t.Fatalf("invalid record outcome = %+v, want failed with an error", result.Records[1])
	}
	if result.Records[0].Outcome != journal.BatchRecordNotRecorded || result.Records[2].Outcome != journal.BatchRecordNotRecorded {
		t.Fatalf("records around a validation failure = %+v, want not-recorded", result.Records)
	}
	for i, in := range inputs {
		if i == 1 {
			continue // the invalid OperationID cannot be looked up
		}
		lookup, err := db.LookupCommitted(in.OperationID)
		if err != nil {
			t.Fatalf("LookupCommitted(%q): %v", in.OperationID, err)
		}
		if lookup.Kind != journal.CommittedAbsent {
			t.Fatalf("operation %q was written despite an input validation failure: %+v", in.OperationID, lookup)
		}
	}
}

// TestApplyBatchAdvancesCurrencyGenerationAndLedger pins the verified-as-of
// ledger rule on a file-backed store under the v3 induction: the batch's writes
// advance the generation in the same transaction, and the sanctioned batch
// advances the ledger to the post-batch identity in that same transaction — so
// the ledger still matches and the next open takes the fast path with
// verified_count untouched. (Under the retired v2 rule the ledger stopped
// matching here; an out-of-band write still does — see
// TestOutOfBandWriteStillTakesFullPath.)
func TestApplyBatchAdvancesCurrencyGenerationAndLedger(t *testing.T) {
	// Deliberately serial: file-backed store with one path and one ledger row.
	path := t.TempDir() + "/batch-ledger.db"
	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	actor := ptypes.ActorID{Namespace: "provenance-test", UUID: uuid.New()}
	seedActor(t, db, actor)
	authority := genesisBoot(t, db, actor)

	scope := takePoolScope(t, db)
	defer scope.release()
	// Seeding and genesis advanced the generation after activation wrote its
	// ledger, so write a fresh ledger row for the current state before the batch.
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error { return scope.writeVerifiedLedger() }); err != nil {
		t.Fatalf("persist verified-as-of ledger for the seeded store: %v", err)
	}
	if !scope.verifiedLedgerMatches() {
		t.Fatal("seeded store's verified-as-of ledger does not match; cannot test invalidation")
	}
	before, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		t.Fatalf("read pre-batch store identity: %v", err)
	}

	inputs := make([]journal.OperationInput, 8)
	for i := range inputs {
		inputs[i] = batchDecisionInput(fmt.Sprintf("batch-ledger-%02d", i), actor, authority, `{}`)
	}
	result, err := db.ApplyBatch(inputs)
	if err != nil || !result.Committed {
		t.Fatalf("ApplyBatch: result=%+v err=%v", result, err)
	}

	after, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		t.Fatalf("read post-batch store identity: %v", err)
	}
	if after.generation <= before.generation {
		t.Fatalf("batch did not advance the store generation (%d -> %d); the ledger would falsely still match", before.generation, after.generation)
	}
	if !scope.verifiedLedgerMatches() {
		t.Fatal("verified-as-of ledger does not match after a sanctioned batch; the advance did not commit with the batch")
	}
	// And the batch left the store passing the guards the full path runs.
	if err := db.VerifyIntegrity(); err != nil {
		t.Fatalf("VerifyIntegrity after batch: %v", err)
	}
	replay, err := db.ReplayProjections()
	if err != nil {
		t.Fatalf("ReplayProjections after batch: %v", err)
	}
	if replay.RowsFolded == 0 {
		t.Fatal("ReplayProjections folded no rows after a committed batch")
	}
}
