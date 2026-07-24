package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	"zombiezen.com/go/sqlite/sqlitex"
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

func latestFactInLeasedTransaction(t *testing.T, db *DB, sel journal.FactSelector) (journal.JournalID, bool) {
	t.Helper()
	scope, err := db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind fact matcher connection: %v", err)
	}
	defer scope.release()
	var txErr error
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&txErr)
	kind, args, err := buildSelectorArgs(sel, 0)
	if err != nil {
		t.Fatalf("build fact selector: %v", err)
	}
	latest, found, err := latestFactSelector(scope, kind, args)
	if err != nil {
		t.Fatalf("evaluate latest fact on leased transaction: %v", err)
	}
	return latest, found
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
	decisionRowJID, found := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind:         journal.FactDecision,
		DecisionKind: "fixture.decision.v1",
		Filter:       journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	})
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
	actual, _ := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	})

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

	latest, _ := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	})

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

	// Capture the second decision, then make it stale with a third decision.
	for i := range 2 {
		dec := env.makeDecisionOp("fixture.decision.v1")
		if _, err := env.db.Apply(dec); err != nil {
			t.Fatalf("Apply decision %d: %v", i, err)
		}
	}
	sel := journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}}
	stale, found := latestFactInLeasedTransaction(t, env.db, sel)
	if !found {
		t.Fatal("CurrentFact stale: second decision not found")
	}
	if _, err := env.db.Apply(env.makeDecisionOp("fixture.decision.v1")); err != nil {
		t.Fatalf("Apply third decision: %v", err)
	}
	current, found := latestFactInLeasedTransaction(t, env.db, sel)
	if !found || current <= stale {
		t.Fatalf("CurrentFact stale: current=%d stale=%d found=%v", current, stale, found)
	}

	condOp := env.makeEventOp([]journal.Condition{{
		Kind:              journal.ConditionCurrentFact,
		Selector:          sel,
		AssertedJournalID: stale,
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
	if cf.Kind != journal.ConditionCurrentFact || cf.Reason != journal.ConditionCurrentMismatch || cf.ActualJournalID != current {
		t.Fatalf("CurrentFact stale: failure=%+v, want CurrentFact/CurrentMismatch actual=%d", cf, current)
	}
	if after := journalRowCount(t, env.db); after != before {
		t.Fatalf("CurrentFact stale: wrote rows, want 0")
	}
}

// TestConditionNonzeroFirstFailureIndex verifies a condition after a passing
// condition reports its actual index. The matching decision is effect index 1.
func TestConditionNonzeroFirstFailureIndex(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)

	decision := env.makeDecisionOp("fixture.decision.v1")
	decision.Effects = append([]journal.Effect{{Sort: journal.EffectTaskEvent, TaskID: env.task, EventKind: "provenance.test.event"}}, decision.Effects...)
	if _, err := env.db.Apply(decision); err != nil {
		t.Fatalf("Apply decision at effect index 1: %v", err)
	}
	decisionSelector := journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}}
	decisionJID, found := latestFactInLeasedTransaction(t, env.db, decisionSelector)
	if !found {
		t.Fatal("decision at effect index 1 not found")
	}

	condOp := env.makeEventOp([]journal.Condition{
		{
			Kind:              journal.ConditionCurrentFact,
			Selector:          decisionSelector,
			AssertedJournalID: decisionJID,
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
	if cf.Index != 1 || cf.Kind != journal.ConditionCurrentFact || cf.Reason != journal.ConditionFactMissing {
		t.Fatalf("nonzero condition index: failure=%+v, want index=1 CurrentFact/FactMissing", cf)
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
	evJID, found := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactEvidence, EvidenceKind: "fixture.evidence.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	})
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
	unscopedJID, found := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}},
	})
	if !found || unscopedJID <= 0 {
		t.Fatal("Unscoped selector found no row")
	}

	// Any selector must find a row (the latest of either).
	anyJID, foundAny := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}},
	})
	if !foundAny || anyJID <= 0 {
		t.Fatal("Any selector found no row")
	}

	// The "Any" result should be >= the Unscoped result (both committed).
	if anyJID < unscopedJID {
		t.Fatalf("Any selector returned smaller JID (%d) than Unscoped (%d)", anyJID, unscopedJID)
	}
}

// TestFactMatcherOnLeasedTransaction verifies match and mismatch through the
// explicit P0 scope while its caller owns a transaction.
func TestFactMatcherOnLeasedTransaction(t *testing.T) {
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

	scope, err := env.db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind matcher connection: %v", err)
	}
	defer scope.release()
	if err := sqlitex.ExecuteTransient(scope.conn, "BEGIN IMMEDIATE", nil); err != nil {
		t.Fatalf("begin matcher transaction: %v", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(scope.conn, "ROLLBACK", nil) }()

	latest, found, err := evaluateCurrentFactSelector(scope, sel)
	if err != nil || !found || latest <= 0 {
		t.Fatalf("evaluateCurrentFactSelector: found=%v latest=%d err=%v", found, latest, err)
	}

	actual, matched, err2 := evaluateExactFactSelector(scope, sel, latest)
	if err2 != nil || !matched || actual != latest {
		t.Fatalf("evaluateExactFactSelector correct: matched=%v actual=%d err=%v", matched, actual, err2)
	}

	actual2, matched2, err3 := evaluateExactFactSelector(scope, sel, latest+100)
	if err3 != nil || matched2 || actual2 != latest {
		t.Fatalf("evaluateExactFactSelector wrong JID: matched=%v actual=%d latest=%d err=%v", matched2, actual2, latest, err3)
	}
}

func TestFactConditionsObserveSuppliedTransactionAndRollback(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)
	if _, err := env.db.Apply(env.makeDecisionOp("fixture.committed.v1")); err != nil {
		t.Fatalf("Apply committed decision: %v", err)
	}
	committedSel := journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.committed.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}}
	jid, found := latestFactInLeasedTransaction(t, env.db, committedSel)
	if !found {
		t.Fatal("committed decision not found")
	}

	scope, err := env.db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind condition connection: %v", err)
	}
	defer scope.release()
	if err := sqlitex.ExecuteTransient(scope.conn, "BEGIN IMMEDIATE", nil); err != nil {
		t.Fatalf("begin condition transaction: %v", err)
	}
	if err := sqlitex.Execute(scope.conn, "UPDATE journal_decisions SET decision_kind=?1 WHERE journal_id=?2", &sqlitex.ExecOptions{Args: []any{"fixture.transaction.v1", int64(jid)}}); err != nil {
		t.Fatalf("update transaction-local decision: %v", err)
	}
	txSel := journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.transaction.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}}
	conditions := journal.OperationInput{Conditions: []journal.Condition{
		{Kind: journal.ConditionExactFact, Selector: txSel, AssertedJournalID: jid},
		{Kind: journal.ConditionCurrentFact, Selector: txSel, AssertedJournalID: jid},
	}}
	if err := checkConditions(scope, conditions); err != nil {
		t.Fatalf("transaction-local conditions: %v", err)
	}
	if err := sqlitex.ExecuteTransient(scope.conn, "ROLLBACK", nil); err != nil {
		t.Fatalf("rollback condition transaction: %v", err)
	}

	if err := sqlitex.ExecuteTransient(scope.conn, "BEGIN", nil); err != nil {
		t.Fatalf("begin post-rollback read transaction: %v", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(scope.conn, "ROLLBACK", nil) }()
	if _, found, err := evaluateCurrentFactSelector(scope, txSel); err != nil || found {
		t.Fatalf("rolled-back selector: found=%v err=%v, want absent", found, err)
	}
	if actual, found, err := evaluateCurrentFactSelector(scope, committedSel); err != nil || !found || actual != jid {
		t.Fatalf("committed selector after rollback: actual=%d found=%v err=%v, want %d", actual, found, err, jid)
	}
}

func TestFactMatcherLookupFailureRollsBackCleanly(t *testing.T) {
	t.Parallel()
	env := newFactCondEnv(t)
	if _, err := env.db.Apply(env.makeDecisionOp("fixture.decision.v1")); err != nil {
		t.Fatalf("Apply decision: %v", err)
	}
	sel := journal.FactSelector{Kind: journal.FactDecision, DecisionKind: "fixture.decision.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskAny}}}

	scope, err := env.db.bindConn(context.Background())
	if err != nil {
		t.Fatalf("bind matcher connection: %v", err)
	}
	defer scope.release()
	if err := sqlitex.ExecuteTransient(scope.conn, "BEGIN IMMEDIATE", nil); err != nil {
		t.Fatalf("begin lookup-failure transaction: %v", err)
	}
	if err := sqlitex.ExecuteTransient(scope.conn, "DROP VIEW journal_attributed", nil); err != nil {
		t.Fatalf("drop matcher dependency in transaction: %v", err)
	}
	_, _, lookupErr := evaluateExactFactSelector(scope, sel, 1)
	if lookupErr == nil || !strings.Contains(lookupErr.Error(), "evaluateExactFactSelector") {
		t.Fatalf("lookup failure = %v, want contextual matcher error", lookupErr)
	}
	if err := sqlitex.ExecuteTransient(scope.conn, "ROLLBACK", nil); err != nil {
		t.Fatalf("rollback lookup-failure transaction: %v", err)
	}

	if err := sqlitex.ExecuteTransient(scope.conn, "BEGIN", nil); err != nil {
		t.Fatalf("begin recovered lookup transaction: %v", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(scope.conn, "ROLLBACK", nil) }()
	if _, found, err := evaluateCurrentFactSelector(scope, sel); err != nil || !found {
		t.Fatalf("matcher after rollback: found=%v err=%v", found, err)
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
	jid, found := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: env.task}},
	})
	if !found || jid <= 0 {
		t.Fatal("Exact scope for env.task found no row")
	}

	// Exact scope for task2 must find no row.
	_, found2 := latestFactInLeasedTransaction(t, env.db, journal.FactSelector{
		Kind: journal.FactDecision, DecisionKind: "fixture.scoped.v1",
		Filter: journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task2}},
	})
	if found2 {
		t.Fatal("Exact scope for task2 found a row, expected none")
	}
}
