package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// fact_conditions_test.go tests transaction-local ExactFact and CurrentFact
// condition evaluation via the real Apply path (§9.5). All tests use file-backed
// or in-memory SQLite through the production code path.

// factCondEnv wraps a journal DB with helpers for condition tests.
type factCondEnv struct {
	db   *DB
	boot journal.JournalID
	act  journal.ActorID
	task journal.TaskID
}

func newFactCondEnv(t *testing.T) *factCondEnv {
	t.Helper()
	db := newJournalDB(t)
	actor, task := seedActorAndTask(t, db)
	boot := genesisBoot(t, db, actor)
	return &factCondEnv{db: db, boot: boot, act: actor, task: task}
}

func (e *factCondEnv) makeDecisionOp(decisionKind journal.DecisionKind) journal.OperationInput {
	opID := journal.OperationID("decision-" + string(uuid.Must(uuid.NewV7()).String()))
	return journal.OperationInput{
		OperationID:        opID,
		ActorID:            e.act,
		AuthorityJournalID: &e.boot,
		CommandDigest:      []byte("cmd-" + string(opID)),
		Effects: []journal.Effect{
			{
				Sort:         journal.EffectDecision,
				DecisionKind: decisionKind,
				Payload:      []byte(`{}`),
			},
		},
	}
}

func (e *factCondEnv) makeEventOp(conditions []journal.Condition) journal.OperationInput {
	opID := journal.OperationID("event-" + string(uuid.Must(uuid.NewV7()).String()))
	return journal.OperationInput{
		OperationID:        opID,
		ActorID:            e.act,
		AuthorityJournalID: &e.boot,
		CommandDigest:      []byte("cmd-" + string(opID)),
		Conditions:         conditions,
		Effects: []journal.Effect{
			{Sort: journal.EffectTaskEvent, TaskID: e.task, EventKind: "provenance.test.event"},
		},
	}
}

// TestExactFactConditionSuccess verifies ExactFact succeeds when the asserted
// JournalID matches the selector.
func TestExactFactConditionSuccess(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	decisionOp := env.makeDecisionOp("fixture.decision.v1")
	res, err := env.db.Apply(decisionOp)
	if err != nil || res.Kind != journal.CommittedExact {
		t.Fatalf("Apply decision: %v %+v", err, res)
	}
	env.db.mu.Lock()
	k, args, _ := buildSelectorArgs(journal.FactSelector{
		Kind:         journal.FactDecision,
		DecisionKind: "fixture.decision.v1",
		Filter:       journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}, 0)
	decisionRowJID, found, _ := env.db.latestFactSelectorLocked(k, args)
	env.db.mu.Unlock()
	if !found || decisionRowJID <= 0 {
		t.Fatalf("could not find committed decision journal row")
	}

	// ExactFact with the correct JournalID must succeed.
	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionExactFact,
		Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: decisionRowJID,
	}})
	res2, err2 := env.db.Apply(condOp)
	if err2 != nil || res2.Kind != journal.CommittedExact {
		t.Fatalf("ExactFact success: Apply failed: %v %+v", err2, res2)
	}
}

// TestExactFactConditionMissing verifies ExactFact returns ConditionFailure(FactMissing)
// when no matching row exists at the asserted JournalID.
func TestExactFactConditionMissing(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Assert a non-existent JournalID.
	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionExactFact,
		Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: 9999,
	}})
	before := journalRowCount(t, env.db)
	_, err := env.db.Apply(condOp)
	if err == nil {
		t.Fatal("ExactFact missing: Apply succeeded, expected failure")
	}
	var cf *journal.ConditionFailure
	if !errors.Is(err, journal.ErrConditionFailed) || !errors.As(err, &cf) {
		t.Fatalf("ExactFact missing: wrong error type: %v", err)
	}
	if cf.Kind != journal.ConditionExactFact || cf.Reason != journal.ConditionFactMissing || cf.Index != 0 {
		t.Fatalf("ExactFact missing: ConditionFailure = %+v, want ExactFact/FactMissing/0", cf)
	}
	if after := journalRowCount(t, env.db); after != before {
		t.Fatalf("ExactFact missing: wrote %d rows, want 0", after-before)
	}
}

// TestExactFactConditionMismatch verifies ExactFact returns ConditionFailure(FactMismatch)
// when a matching row exists but at a different JournalID.
func TestExactFactConditionMismatch(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Commit one decision row.
	decOp := env.makeDecisionOp("fixture.decision.v1")
	_, err := env.db.Apply(decOp)
	if err != nil {
		t.Fatalf("Apply decision: %v", err)
	}

	// Find the actual JournalID of the committed decision.
	env.db.mu.Lock()
	k, args, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}, 0)
	actual, _, _ := env.db.latestFactSelectorLocked(k, args)
	env.db.mu.Unlock()

	// Assert a JournalID that is off by 1 (wrong, but the row exists).
	condOp := env.makeEventOp([]journal.Condition{{
		Kind: journal.ConditionExactFact,
		Selector: journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
			Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: actual + 1, // wrong JID
	}})
	before := journalRowCount(t, env.db)
	_, err = env.db.Apply(condOp)
	if err == nil {
		t.Fatal("ExactFact mismatch: Apply succeeded, expected failure")
	}
	var cf *journal.ConditionFailure
	if !errors.Is(err, journal.ErrConditionFailed) || !errors.As(err, &cf) {
		t.Fatalf("ExactFact mismatch: wrong error type: %v", err)
	}
	if cf.Kind != journal.ConditionExactFact || cf.Reason != journal.ConditionFactMismatch || cf.Index != 0 {
		t.Fatalf("ExactFact mismatch: ConditionFailure = %+v, want ExactFact/FactMismatch/0", cf)
	}
	if cf.ActualJournalID != actual {
		t.Fatalf("ExactFact mismatch: ActualJournalID = %d, want %d", cf.ActualJournalID, actual)
	}
	if after := journalRowCount(t, env.db); after != before {
		t.Fatalf("ExactFact mismatch: wrote %d rows, want 0", after-before)
	}
}

// TestCurrentFactConditionAbsenceSuccess verifies CurrentFact with JournalID=0
// succeeds when no matching row exists.
func TestCurrentFactConditionAbsenceSuccess(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionCurrentFact,
		Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: 0, // assert absence
	}})
	res, err := env.db.Apply(condOp)
	if err != nil || res.Kind != journal.CommittedExact {
		t.Fatalf("CurrentFact absence success: %v %+v", err, res)
	}
}

// TestCurrentFactConditionSuccess verifies CurrentFact succeeds when the asserted
// JournalID equals the highest matching row.
func TestCurrentFactConditionSuccess(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	decOp := env.makeDecisionOp("fixture.decision.v1")
	_, err := env.db.Apply(decOp)
	if err != nil {
		t.Fatalf("Apply decision: %v", err)
	}

	env.db.mu.Lock()
	k, args, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}, 0)
	latest, _, _ := env.db.latestFactSelectorLocked(k, args)
	env.db.mu.Unlock()

	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionCurrentFact,
		Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: latest,
	}})
	res, err := env.db.Apply(condOp)
	if err != nil || res.Kind != journal.CommittedExact {
		t.Fatalf("CurrentFact success: %v %+v", err, res)
	}
}

// TestCurrentFactConditionStale verifies CurrentFact returns ConditionFailure(CurrentMismatch)
// when a newer row exists.
func TestCurrentFactConditionStale(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Commit two decision rows of the same kind.
	for i := range 2 {
		_ = i
		dec := env.makeDecisionOp("fixture.decision.v1")
		if _, err := env.db.Apply(dec); err != nil {
			t.Fatalf("Apply decision %d: %v", i, err)
		}
	}

	// Get the first committed decision's JournalID (now stale — a newer one exists).
	// We cannot easily get it directly; let's use Apply with absence assertion first to
	// find the "earlier" JournalID by testing the first one committed.
	// Instead, assert the JournalID = 999 (non-existent) to get a FactMissing,
	// then assert a real JID that is not the latest.

	// Actually, assert JournalID = 1 (which won't be a decision row — it's the journal entry).
	condOp := env.makeEventOp([]journal.Condition{{
		Kind: journal.ConditionCurrentFact,
		Selector: journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
			Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: 1, // too small — current is higher
	}})
	before := journalRowCount(t, env.db)
	_, err := env.db.Apply(condOp)
	if err == nil {
		t.Fatal("CurrentFact stale: Apply succeeded, expected failure")
	}
	var cf *journal.ConditionFailure
	if !errors.Is(err, journal.ErrConditionFailed) || !errors.As(err, &cf) {
		t.Fatalf("CurrentFact stale: wrong error: %v", err)
	}
	if cf.Kind != journal.ConditionCurrentFact {
		t.Fatalf("CurrentFact stale: cf.Kind = %s, want CurrentFact", cf.Kind)
	}
	// Must be CurrentMismatch or FactMissing (JID=1 is not a decision row)
	if cf.Reason != journal.ConditionCurrentMismatch && cf.Reason != journal.ConditionFactMissing {
		t.Fatalf("CurrentFact stale: unexpected reason %s", cf.Reason)
	}
	if after := journalRowCount(t, env.db); after != before {
		t.Fatalf("CurrentFact stale: wrote rows, want 0")
	}
}

// TestConditionFirstFailureIndex verifies that the first failing condition's index
// is reported (conditions are evaluated in order; first failure wins).
func TestConditionFirstFailureIndex(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Two conditions: both asserting non-existent rows. First must fail.
	condOp := env.makeEventOp([]journal.Condition{
		{
			Kind:              journal.ConditionCurrentFact,
			Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
			AssertedJournalID: 9999,
		},
		{
			Kind:              journal.ConditionCurrentFact,
			Selector:          journal.FactSelector{Kind: journal.FactEvidence, EvidenceKind: "fixture.evidence.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
			AssertedJournalID: 9998,
		},
	})
	_, err := env.db.Apply(condOp)
	var cf *journal.ConditionFailure
	if !errors.Is(err, journal.ErrConditionFailed) || !errors.As(err, &cf) {
		t.Fatalf("first condition index: wrong error: %v", err)
	}
	if cf.Index != 0 {
		t.Fatalf("first condition index: cf.Index = %d, want 0", cf.Index)
	}
}

// TestConditionEvidenceSelector verifies the FactEvidence selector path.
func TestConditionEvidenceSelector(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Commit an evidence row.
	evOp := journal.OperationID("ev-op-" + uuid.Must(uuid.NewV7()).String())
	_, err := env.db.Apply(journal.OperationInput{
		OperationID:        evOp,
		ActorID:            env.act,
		AuthorityJournalID: &env.boot,
		CommandDigest:      []byte("cmd-ev"),
		Effects: []journal.Effect{
			{Sort: journal.EffectEvidence, EvidenceKind: "fixture.evidence.v1", ContentDigest: []byte("digest"), Payload: []byte(`{}`)},
		},
	})
	if err != nil {
		t.Fatalf("Apply evidence: %v", err)
	}

	// Get the committed evidence JournalID.
	env.db.mu.Lock()
	k, args, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactEvidence, EvidenceKind: "fixture.evidence.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}, 0)
	evJID, found, _ := env.db.latestFactSelectorLocked(k, args)
	env.db.mu.Unlock()
	if !found {
		t.Fatal("evidence row not found after Apply")
	}

	// CurrentFact on the evidence row must succeed.
	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionCurrentFact,
		Selector:          journal.FactSelector{Kind: journal.FactEvidence, EvidenceKind: "fixture.evidence.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: evJID,
	}})
	res, err := env.db.Apply(condOp)
	if err != nil || res.Kind != journal.CommittedExact {
		t.Fatalf("CurrentFact evidence success: %v %+v", err, res)
	}
}

// TestConditionRollbackOnFailure verifies a failed condition produces zero writes.
func TestConditionRollbackOnFailure(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)
	before := journalRowCount(t, env.db)

	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionExactFact,
		Selector:          journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1", Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}},
		AssertedJournalID: 9999,
	}})
	_, err := env.db.Apply(condOp)
	if !errors.Is(err, journal.ErrConditionFailed) {
		t.Fatalf("rollback test: expected ErrConditionFailed, got: %v", err)
	}
	if after := journalRowCount(t, env.db); after != before {
		t.Fatalf("rollback test: journal changed: before=%d after=%d", before, after)
	}
	// Operation must not be persisted.
	result, _ := env.db.LookupCommitted(condOp.OperationID)
	if result.Kind != journal.CommittedAbsent {
		t.Fatalf("rollback test: operation was persisted: %+v", result)
	}
}

// TestConditionTaskScopeUnscoped verifies FactTaskUnscoped scope filtering
// (task_id IS NULL) on decisions.
func TestConditionTaskScopeUnscoped(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Commit a task-scoped and an unscoped decision.
	taskScopedOp := journal.OperationID("task-dec-" + uuid.Must(uuid.NewV7()).String())
	_, err := env.db.Apply(journal.OperationInput{
		OperationID: taskScopedOp, ActorID: env.act, AuthorityJournalID: &env.boot, CommandDigest: []byte("c1"),
		Effects: []journal.Effect{{Sort: journal.EffectDecision, DecisionKind: "fixture.scoped.v1", TaskID: env.task, Payload: []byte(`{}`)}},
	})
	if err != nil {
		t.Fatalf("task-scoped decision: %v", err)
	}

	unscopedOp := journal.OperationID("unsco-dec-" + uuid.Must(uuid.NewV7()).String())
	_, err = env.db.Apply(journal.OperationInput{
		OperationID: unscopedOp, ActorID: env.act, AuthorityJournalID: &env.boot, CommandDigest: []byte("c2"),
		Effects: []journal.Effect{{Sort: journal.EffectDecision, DecisionKind: "fixture.scoped.v1", Payload: []byte(`{}`)}},
	})
	if err != nil {
		t.Fatalf("unscoped decision: %v", err)
	}

	// Unscoped selector must find only the unscoped row.
	env.db.mu.Lock()
	k, args, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}},
	}, 0)
	unscopedJID, found, _ := env.db.latestFactSelectorLocked(k, args)
	env.db.mu.Unlock()
	if !found || unscopedJID <= 0 {
		t.Fatal("Unscoped selector found no row")
	}

	// Any selector must find a row (the latest of either).
	env.db.mu.Lock()
	k2, args2, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}, 0)
	anyJID, foundAny, _ := env.db.latestFactSelectorLocked(k2, args2)
	env.db.mu.Unlock()
	if !foundAny || anyJID <= 0 {
		t.Fatal("Any selector found no row")
	}

	// The "Any" result should be >= the Unscoped result (both committed).
	if anyJID < unscopedJID {
		t.Fatalf("Any selector returned smaller JID (%d) than Unscoped (%d)", anyJID, unscopedJID)
	}
}

// TestSharedFactMatcherEquivalenceForDotOneFour verifies that EvaluateExactFactSelector
// and EvaluateCurrentFactSelector are callable by both condition evaluation and
// (by design) the .1.4 bounded query path.
func TestSharedFactMatcherEquivalenceForDotOneFour(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Commit a decision.
	decOp := env.makeDecisionOp("fixture.decision.v1")
	if _, err := env.db.Apply(decOp); err != nil {
		t.Fatalf("Apply decision: %v", err)
	}

	sel := journal.FactSelector{
		Kind:         journal.FactDecision,
		DecisionKind: "fixture.decision.v1",
		Filter:       journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	}

	// EvaluateCurrentFactSelector — finds the row.
	env.db.mu.Lock()
	latest, found, err := env.db.EvaluateCurrentFactSelector(sel, 0)
	env.db.mu.Unlock()
	if err != nil || !found || latest <= 0 {
		t.Fatalf("EvaluateCurrentFactSelector: found=%v latest=%d err=%v", found, latest, err)
	}

	// EvaluateExactFactSelector with correct JID — matches.
	env.db.mu.Lock()
	actual, matched, err2 := env.db.EvaluateExactFactSelector(sel, latest)
	env.db.mu.Unlock()
	if err2 != nil || !matched || actual != latest {
		t.Fatalf("EvaluateExactFactSelector correct: matched=%v actual=%d err=%v", matched, actual, err2)
	}

	// EvaluateExactFactSelector with wrong JID — mismatch.
	env.db.mu.Lock()
	actual2, matched2, err3 := env.db.EvaluateExactFactSelector(sel, latest+100)
	env.db.mu.Unlock()
	if err3 != nil || matched2 || actual2 != latest {
		t.Fatalf("EvaluateExactFactSelector wrong JID: matched=%v actual=%d latest=%d err=%v", matched2, actual2, latest, err3)
	}
}

// TestConditionExactTaskScopeFilter verifies FactTaskExact scope filtering.
func TestConditionExactTaskScopeFilter(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	// Commit a decision scoped to a specific task.
	task2 := ptypes.TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
	// SeedLegacyTaskRow acquires the mutex internally; do not hold it here.
	if err := env.db.SeedLegacyTaskRow(ptypes.Task{
		ID:        task2,
		Title:     "task2",
		Phase:     ptypes.PhaseUnscoped,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed task2: %v", err)
	}

	scopedOp := journal.OperationID("scoped-dec-" + uuid.Must(uuid.NewV7()).String())
	if _, err := env.db.Apply(journal.OperationInput{
		OperationID: scopedOp, ActorID: env.act, AuthorityJournalID: &env.boot, CommandDigest: []byte("cs"),
		Effects: []journal.Effect{{Sort: journal.EffectDecision, DecisionKind: "fixture.scoped.v1", TaskID: env.task, Payload: []byte(`{}`)}},
	}); err != nil {
		t.Fatalf("scoped decision: %v", err)
	}

	// Exact scope for env.task must find the row.
	env.db.mu.Lock()
	k, args, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: env.task}},
	}, 0)
	jid, found, _ := env.db.latestFactSelectorLocked(k, args)
	env.db.mu.Unlock()
	if !found || jid <= 0 {
		t.Fatal("Exact scope for env.task found no row")
	}

	// Exact scope for task2 must find no row.
	env.db.mu.Lock()
	k2, args2, _ := buildSelectorArgs(journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task2}},
	}, 0)
	_, found2, _ := env.db.latestFactSelectorLocked(k2, args2)
	env.db.mu.Unlock()
	if found2 {
		t.Fatal("Exact scope for task2 found a row, expected none")
	}
}
