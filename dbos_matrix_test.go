package provenance_test

// dbos_matrix_test.go drives the complete required checkpoint/store matrix (issue
// dayvidpham/provenance#6) with exact callback/write/error oracles. Divergence rows
// are produced by dependency-injecting a tracker whose LookupCommitted returns the
// matrix variant while its Apply commits normally, so the adapter's post-validation
// is exercised against real DBOS checkpoints.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/dayvidpham/provenance"
	"github.com/google/uuid"
)

// fakeJournal wraps a real Journal, counting fold ATTEMPTS (every Apply call,
// which the step skips entirely on a §9.4/DBOS replay) and successful COMMITS (the
// "execute once" oracle, tolerant of DBOS retry attempts that do not commit),
// and optionally transforming LookupCommitted to inject a matrix
// divergence.
type fakeJournal struct {
	provenance.Journal
	attempts *int
	commits  *int
	lookup   func(real provenance.CommittedResult, realErr error) (provenance.CommittedResult, error)
}

func (f *fakeJournal) Apply(in provenance.OperationInput) (provenance.CommittedResult, error) {
	if f.attempts != nil {
		*f.attempts++
	}
	res, err := f.Journal.Apply(in)
	if err == nil && f.commits != nil {
		*f.commits++
	}
	return res, err
}

func (f *fakeJournal) LookupCommitted(op provenance.OperationID) (provenance.CommittedResult, error) {
	real, err := f.Journal.LookupCommitted(op)
	if f.lookup != nil {
		return f.lookup(real, err)
	}
	return real, err
}

// wrappedTracker embeds a real Tracker and swaps in a fixed fakeJournal.
type wrappedTracker struct {
	provenance.Tracker
	journal *fakeJournal
}

func (w *wrappedTracker) Journal() provenance.Journal { return w.journal }

// counters holds the attempt/commit counters a matrix test asserts against.
type counters struct {
	attempts int
	commits  int
}

// stackWithJournal wires a stack whose adapter folds through a fakeJournal that
// records into c (may be nil) and applies the optional lookup transform.
func stackWithJournal(t *testing.T, c *counters, lookup func(provenance.CommittedResult, error) (provenance.CommittedResult, error)) *dbosStack {
	t.Helper()
	stack := stackWithJournalUnlaunched(t, c, lookup)
	launchDBOSStack(t, stack)
	return stack
}

func stackWithJournalUnlaunched(t *testing.T, c *counters, lookup func(provenance.CommittedResult, error) (provenance.CommittedResult, error)) *dbosStack {
	t.Helper()
	return newDBOSStackUnlaunched(t, func(real provenance.Tracker) provenance.Tracker {
		fj := &fakeJournal{Journal: real.Journal(), lookup: lookup}
		if c != nil {
			fj.attempts = &c.attempts
			fj.commits = &c.commits
		}
		return &wrappedTracker{Tracker: real, journal: fj}
	})
}

// Row 1: absent (DBOS) | absent (Provenance) → execute once, commit, checkpoint,
// post-validate, succeed.
func TestMatrix_AbsentAbsent_Succeeds(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournal(t, &c, nil)
	res, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r1", "aura", "r1"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact || res.ShortCircuited {
		t.Errorf("res = %+v, want fresh CommittedExact", res)
	}
	if c.commits != 1 {
		t.Errorf("committed folds = %d, want exactly 1 (execute once)", c.commits)
	}
}

// Row 2: absent (DBOS) | exact (Provenance) → Provenance replay short-circuit,
// checkpoint, post-validate, succeed. The operation is committed directly first, so
// the adapter's step folds onto an already-committed operation (§9.4).
func TestMatrix_AbsentExact_ReplaySucceeds(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournalUnlaunched(t, &c, nil)
	op := s.createTaskOp("op-r2", "aura", "r2")

	// Commit directly (bypassing the adapter) so DBOS has no workflow but Provenance
	// already holds the exact operation.
	if _, err := s.tracker.Journal().Apply(op); err != nil {
		t.Fatalf("direct Apply: %v", err)
	}
	launchDBOSStack(t, s)
	before := c.commits
	res, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("adapter.Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact {
		t.Errorf("res = %+v, want CommittedExact", res)
	}
	// The adapter step folded once and that fold short-circuited on the
	// already-committed operation (§9.4) — the domain is unchanged, so the anchor
	// matches the directly-committed one.
	if c.commits-before != 2 {
		t.Errorf("successful replay folds = %d, want preflight plus first DBOS callback", c.commits-before)
	}
}

// Row 3: absent (DBOS) | conflict (Provenance) → typed conflict; no new domain/
// checkpoint success. Same OperationID committed first with different digests.
func TestMatrix_AbsentConflict_TypedConflict(t *testing.T) {
	t.Parallel()
	s := stackWithJournalUnlaunched(t, nil, nil)
	op := s.createTaskOp("op-r3", "aura", "r3")
	if _, err := s.tracker.Journal().Apply(op); err != nil {
		t.Fatalf("direct Apply: %v", err)
	}
	launchDBOSStack(t, s)
	// Reuse the OperationID with a different command digest → different fingerprint,
	// absent DBOS workflow, Provenance conflict at the fold.
	conflicting := op
	conflicting.CommandDigest = []byte("different-command")
	_, err := s.adapter.Apply(context.Background(), conflicting)
	if !errors.Is(err, provenance.ErrOperationConflict) {
		t.Fatalf("err = %v, want ErrOperationConflict", err)
	}
	var oc *provenance.OperationConflict
	if !errors.As(err, &oc) {
		t.Errorf("conflict not errors.As-discoverable: %v", err)
	}
	if oc.Axis != provenance.ConflictCommand || oc.Index != -1 {
		t.Fatalf("conflict metadata=%#v, want ConflictCommand/-1", oc)
	}
}

// Row 4: present-success (DBOS) | exact equal canonical mutation → the read-only
// replay preflight runs once, then DBOS returns the completed workflow without
// re-running its step callback.
func TestMatrix_PresentSuccessExact_ZeroCallback(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournal(t, &c, nil)
	op := s.createTaskOp("op-r4", "aura", "r4")
	if _, err := s.adapter.Apply(context.Background(), op); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if c.commits != 1 {
		t.Fatalf("first Apply committed folds = %d, want 1", c.commits)
	}
	attemptsBefore := c.attempts
	res, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if res.Kind != provenance.CommittedExact {
		t.Errorf("res.Kind = %v, want CommittedExact", res.Kind)
	}
	// One journal replay preflight, zero DBOS callbacks. A second attempt would mean
	// the durable workflow step ran again.
	if c.attempts-attemptsBefore != 1 {
		t.Errorf("fold attempts on re-Apply = %d, want 1 preflight and zero DBOS callbacks", c.attempts-attemptsBefore)
	}
	if c.commits != 2 {
		t.Errorf("successful folds after re-Apply = %d, want initial commit plus read-only replay", c.commits)
	}
}

func TestMatrix_PresentSuccessChangedCanonicalOperand_ConflictsBeforeWorkflow(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournal(t, &c, nil)
	op := s.createTaskOp("op-canonical-conflict", "aura", "original")
	first, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	tasksBefore, err := s.tracker.List(provenance.ListFilter{})
	if err != nil {
		t.Fatalf("List before retry: %v", err)
	}
	attemptsBefore := c.attempts
	changed := op
	changed.Effects = append([]provenance.Effect(nil), op.Effects...)
	changed.Effects[0].Title = "changed while caller digest stays identical"
	_, err = s.adapter.Apply(context.Background(), changed)
	if !errors.Is(err, provenance.ErrOperationConflict) {
		t.Fatalf("changed canonical retry err = %v, want ErrOperationConflict", err)
	}
	var conflict *provenance.OperationConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("changed canonical retry lacks typed OperationConflict: %v", err)
	}
	if c.attempts-attemptsBefore != 1 {
		t.Fatalf("retry fold attempts = %d, want one replay preflight and zero DBOS callbacks", c.attempts-attemptsBefore)
	}
	tasksAfter, err := s.tracker.List(provenance.ListFilter{})
	if err != nil {
		t.Fatalf("List after retry: %v", err)
	}
	looked, err := s.tracker.Journal().LookupCommitted(op.OperationID)
	if err != nil {
		t.Fatalf("LookupCommitted after retry: %v", err)
	}
	if len(tasksAfter) != len(tasksBefore) || looked.AnchorJournalID != first.AnchorJournalID || len(looked.EmittedEvents) != len(first.EmittedEvents) || len(looked.ResultSlots) != len(first.ResultSlots) {
		t.Fatalf("conflicting retry changed durable state: tasks %d->%d, first=%+v looked=%+v", len(tasksBefore), len(tasksAfter), first, looked)
	}
}

func TestMatrix_SuccessCheckpointRejectsResultSlotDivergence(t *testing.T) {

	tests := map[string]func(*provenance.CommittedResult){
		"over-limit": func(result *provenance.CommittedResult) {
			result.ResultSlots = make([]provenance.ResultSlotBinding, provenance.MaxCanonicalResultSlots+1)
			for i := range result.ResultSlots {
				result.ResultSlots[i] = provenance.ResultSlotBinding{Slot: provenance.ResultSlotID(fmt.Sprintf("slot-%03d", i)), ProducedJournalID: provenance.JournalID(i + 1), Kind: provenance.JournalKindAuthority}
			}
		},
		// Activity slots reserved for later vertical; test non-entity wrong-arm instead.
		"wrong-arm-decision": func(result *provenance.CommittedResult) {
			result.ResultSlots[0].Kind = provenance.JournalKindDecision
			result.ResultSlots[0].TaskID = nil
		},
		"missing": func(result *provenance.CommittedResult) {
			result.ResultSlots = nil
		},
		"extra": func(result *provenance.CommittedResult) {
			result.ResultSlots = append(result.ResultSlots, provenance.ResultSlotBinding{
				Slot: "zz-extra", ProducedJournalID: result.AnchorJournalID, Kind: provenance.JournalKindAuthority,
			})
		},
		"duplicate": func(result *provenance.CommittedResult) {
			result.ResultSlots = append(result.ResultSlots, result.ResultSlots[0])
		},
		"wrong-kind": func(result *provenance.CommittedResult) {
			result.ResultSlots[0].Kind = provenance.JournalKindAuthority
			result.ResultSlots[0].TaskID = nil
		},
		"wrong-arm": func(result *provenance.CommittedResult) {
			result.ResultSlots[0].Kind = provenance.JournalKindEvidence
			result.ResultSlots[0].TaskID = nil
		},
		"foreign-row": func(result *provenance.CommittedResult) {
			result.ResultSlots[0].ProducedJournalID++
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			lookup := func(result provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
				if err == nil && result.Kind == provenance.CommittedExact {
					result.ResultSlots = append([]provenance.ResultSlotBinding(nil), result.ResultSlots...)
					mutate(&result)
				}
				return result, err
			}
			s := stackWithJournal(t, nil, lookup)
			_, err := s.adapter.Apply(context.Background(), s.createTaskOp("slot-divergence-"+name, "matrix", name))
			var divergence *provenance.CheckpointDivergenceError
			if !errors.As(err, &divergence) || divergence.Stage == "" || divergence.Impact == "" || divergence.Fix == "" || divergence.Cause == nil {
				t.Fatalf("slot divergence err=%v, want actionable CheckpointDivergenceError", err)
			}
		})
	}
}

func TestMatrix_AllocatedCreateChangedProvisionalUUIDReturnsOriginal(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournal(t, &c, nil)
	op := s.createTaskOp("op-allocated-retry", "aura", "allocated")
	op.Effects[0].Sort = provenance.EffectTaskCreateAllocated
	first, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	wantTask := *first.ResultSlots[0].TaskID
	if wantTask.UUID.Version() != uuid.Version(7) {
		t.Fatalf("allocated task UUID version = %d, want genuine UUIDv7", wantTask.UUID.Version())
	}
	tasksBefore, err := s.tracker.List(provenance.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := s.tracker.Journal().QueryTaskEvents(provenance.JournalQueryV1{OrderBy: provenance.OrderByJournalID})
	if err != nil {
		t.Fatal(err)
	}
	var workflowsBefore, outputsBefore int
	if err := s.db.QueryRow(`SELECT count(*) FROM workflow_status`).Scan(&workflowsBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM operation_outputs`).Scan(&outputsBefore); err != nil {
		t.Fatal(err)
	}
	retry := op
	retry.Effects = append([]provenance.Effect(nil), op.Effects...)
	retry.Effects[0].TaskID = newTaskID("aura")
	attemptsBefore := c.attempts
	got, err := s.adapter.Apply(context.Background(), retry)
	if err != nil {
		t.Fatalf("allocated retry: %v", err)
	}
	if c.attempts-attemptsBefore != 1 {
		t.Fatalf("allocated retry fold attempts = %d, want one replay preflight and zero DBOS callbacks", c.attempts-attemptsBefore)
	}
	if len(got.ResultSlots) != 1 || got.ResultSlots[0].TaskID == nil || *got.ResultSlots[0].TaskID != wantTask {
		t.Fatalf("allocated retry result = %+v, want original task %s", got, wantTask)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("allocated retry complete result=%#v want original=%#v", got, first)
	}
	tasksAfter, _ := s.tracker.List(provenance.ListFilter{})
	eventsAfter, _ := s.tracker.Journal().QueryTaskEvents(provenance.JournalQueryV1{OrderBy: provenance.OrderByJournalID})
	var workflowsAfter, outputsAfter int
	_ = s.db.QueryRow(`SELECT count(*) FROM workflow_status`).Scan(&workflowsAfter)
	_ = s.db.QueryRow(`SELECT count(*) FROM operation_outputs`).Scan(&outputsAfter)
	if !reflect.DeepEqual(tasksAfter, tasksBefore) || !reflect.DeepEqual(eventsAfter, eventsBefore) || workflowsAfter != workflowsBefore || outputsAfter != outputsBefore {
		t.Fatalf("allocation-only retry changed durable tuples: tasks=%v events=%v workflows=%d->%d outputs=%d->%d", !reflect.DeepEqual(tasksAfter, tasksBefore), !reflect.DeepEqual(eventsAfter, eventsBefore), workflowsBefore, workflowsAfter, outputsBefore, outputsAfter)
	}

	ctx, err := provenance.TaskContext(wantTask)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*provenance.OperationInput){
		"namespace": func(v *provenance.OperationInput) { v.Effects[0].TaskID = newTaskID("other") },
		"metadata":  func(v *provenance.OperationInput) { v.Effects[0].Description = "changed" },
		"contexts":  func(v *provenance.OperationInput) { v.Effects[0].Contexts = []provenance.EventContext{ctx} },
		"slot":      func(v *provenance.OperationInput) { v.Effects[0].ResultSlot = "other" },
		"family":    func(v *provenance.OperationInput) { v.Effects[0].Sort = provenance.EffectTaskCreate },
	} {
		t.Run("forbidden-"+name, func(t *testing.T) {
			changed := op
			changed.Effects = append([]provenance.Effect(nil), op.Effects...)
			mutate(&changed)
			beforeAttempts := c.attempts
			_, err := s.adapter.Apply(context.Background(), changed)
			if !errors.Is(err, provenance.ErrOperationConflict) {
				t.Fatalf("error=%v want ErrOperationConflict", err)
			}
			if c.attempts-beforeAttempts != 1 {
				t.Fatalf("forbidden allocation retry attempts=%d, want preflight only", c.attempts-beforeAttempts)
			}
		})
	}
}

func TestMatrix_TimestampOnlyRetryAttachesCompletedWorkflow(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournal(t, &c, nil)
	op := s.createTaskOp("op-timestamp-retry", "aura", "timestamp")
	want, err := s.adapter.Apply(context.Background(), op)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	retry := op
	retry.RecordedAt += 987654321
	attemptsBefore := c.attempts
	got, err := s.adapter.Apply(context.Background(), retry)
	if err != nil {
		t.Fatalf("timestamp-only retry: %v", err)
	}
	if c.attempts-attemptsBefore != 1 {
		t.Fatalf("timestamp retry attempts = %d, want one read-only preflight and zero workflow callbacks", c.attempts-attemptsBefore)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("timestamp retry result = %#v, want complete original %#v", got, want)
	}
}

// Row 5: present-success (DBOS) | absent (Provenance) → typed divergence; no writes.
func TestMatrix_PresentSuccessAbsent_Divergence(t *testing.T) {
	t.Parallel()
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		return provenance.CommittedResult{Kind: provenance.CommittedAbsent}, nil
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r5", "aura", "r5"))
	assertDivergence(t, err)
}

// Row 6: present-success (DBOS) | conflict (Provenance) → typed divergence.
func TestMatrix_PresentSuccessConflict_Divergence(t *testing.T) {
	t.Parallel()
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		return provenance.CommittedResult{Kind: provenance.CommittedConflict}, nil
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r6", "aura", "r6"))
	assertDivergence(t, err)
}

// Row 7: present-success (DBOS) | exact digest/result mismatch → typed divergence.
func TestMatrix_PresentSuccessMismatch_Divergence(t *testing.T) {
	t.Parallel()
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		real.AnchorJournalID += 9999 // perturb the journal-anchored result
		return real, err
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r7", "aura", "r7"))
	assertDivergence(t, err)
}

// Row 8: present-failure-outcome (DBOS) | corresponding failure → decode pinned
// typed failure with errors.As; no writes. Re-Apply proves the failure is permanent
// for the same OperationID.
func TestMatrix_PresentFailureOutcome_TypedFailurePermanent(t *testing.T) {
	t.Parallel()
	var c counters
	s := stackWithJournal(t, &c, nil)
	// A second genesis (nil authority + bootstrap effect) against a non-empty
	// journal is a deterministic §4.6 genesis-discipline domain failure.
	op := provenance.OperationInput{
		OperationID:    "op-r8",
		ActorID:        s.actor,
		CommandDigest:  []byte("cmd-r8"),
		MutationDigest: []byte("mut-r8"),
		Effects:        []provenance.Effect{{Sort: provenance.EffectBootstrapAuthority, BootstrapLabel: "second", ResultSlot: "a"}},
	}
	_, err := s.adapter.Apply(context.Background(), op)
	if !errors.Is(err, provenance.ErrGenesis) {
		t.Fatalf("err = %v, want ErrGenesis decoded from checkpointed failure", err)
	}
	attemptsAfterFirst := c.attempts
	if attemptsAfterFirst != 1 {
		t.Fatalf("first durable domain failure fold attempts=%d, want exactly 1", attemptsAfterFirst)
	}
	// Re-Apply: DBOS returns the checkpointed failure outcome without re-running the
	// step, so the fold is not attempted again — the failure is permanent.
	_, err2 := s.adapter.Apply(context.Background(), op)
	if !errors.Is(err2, provenance.ErrGenesis) {
		t.Fatalf("re-Apply err = %v, want permanent ErrGenesis", err2)
	}
	if c.attempts != attemptsAfterFirst {
		t.Errorf("fold attempted again on re-Apply (%d→%d): failure not permanent", attemptsAfterFirst, c.attempts)
	}
}

func TestMatrix_FailureCheckpointRejectsCommittedJournal(t *testing.T) {
	t.Parallel()
	variants := map[string]func() (provenance.CommittedResult, error){
		"exact": func() (provenance.CommittedResult, error) {
			return provenance.CommittedResult{Kind: provenance.CommittedExact, AnchorJournalID: 99}, nil
		},
		"conflict": func() (provenance.CommittedResult, error) {
			return provenance.CommittedResult{Kind: provenance.CommittedConflict}, nil
		},
		"unknown": func() (provenance.CommittedResult, error) {
			return provenance.CommittedResult{Kind: provenance.CommittedResultKind(99)}, nil
		},
		"unavailable": func() (provenance.CommittedResult, error) {
			return provenance.CommittedResult{}, errors.New("journal unavailable during checkpoint reconciliation")
		},
	}
	for name, terminal := range variants {
		t.Run(name, func(t *testing.T) {
			var lookups int
			lookup := func(real provenance.CommittedResult, realErr error) (provenance.CommittedResult, error) {
				lookups++
				if lookups == 1 {
					return provenance.CommittedResult{Kind: provenance.CommittedAbsent}, nil
				}
				return terminal()
			}
			s := stackWithJournal(t, nil, lookup)
			op := s.createTaskOp("failure-checkpoint-"+name, "aura", name)
			op.Conditions = []provenance.Condition{{Kind: provenance.ConditionExactFact, Selector: provenance.FactSelector{Kind: provenance.FactDecision, Filter: provenance.FactFilter{TaskScope: provenance.FactTaskScope{Kind: provenance.FactTaskAny}}, DecisionKind: "fixture.missing"}, AssertedJournalID: 1}}
			_, err := s.adapter.Apply(context.Background(), op)
			var divergence *provenance.CheckpointDivergenceError
			if !errors.As(err, &divergence) || divergence.Operation != op.OperationID || divergence.Stage == "" || divergence.Impact == "" || divergence.Fix == "" || divergence.Cause == nil {
				t.Fatalf("failure checkpoint variant %s err=%v divergence=%+v", name, err, divergence)
			}
			if errors.Is(err, provenance.ErrConditionFailed) {
				t.Fatalf("failure checkpoint variant %s surfaced typed domain failure despite journal divergence: %v", name, err)
			}
		})
	}
}

// Row 9: unknown outcome/lookup variant → fail closed actionably; no writes.
func TestMatrix_UnknownLookupVariant_FailClosed(t *testing.T) {
	t.Parallel()
	lookup := func(real provenance.CommittedResult, err error) (provenance.CommittedResult, error) {
		return provenance.CommittedResult{Kind: provenance.CommittedResultKind(99)}, nil
	}
	s := stackWithJournal(t, nil, lookup)
	_, err := s.adapter.Apply(context.Background(), s.createTaskOp("op-r9", "aura", "r9"))
	assertDivergence(t, err)
}

func assertDivergence(t *testing.T, err error) {
	t.Helper()
	var div *provenance.CheckpointDivergenceError
	if !errors.As(err, &div) {
		t.Fatalf("err = %v, want *CheckpointDivergenceError", err)
	}
	if div.Operation == "" || div.Stage == "" || div.Impact == "" || div.Fix == "" {
		t.Errorf("CheckpointDivergenceError missing actionable fields: %+v", div)
	}
}
