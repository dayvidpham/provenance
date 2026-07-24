package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
)

// fact_conditions.go evaluates the transaction-local ExactFact and CurrentFact
// conditions defined in §9.5. Evaluation runs inside the Apply write transaction
// after the exact-replay lookup and before any effects are folded, so conditions
// observe the same snapshot as the effects they gate.
//
// Concurrent contenders: two goroutines racing a CurrentFact condition both
// acquire the SQLite write lock via BEGIN IMMEDIATE (db.beginWriteOwnershipLocked).
// The loser finds the winner's committed fact as the current row and receives
// ConditionFailure rather than BUSY_SNAPSHOT.
//
// All evaluation is bounded (MaxCanonicalConditions = 64); the canonical layer
// enforces this before Apply is called.

// checkConditionsLocked is the temporary P0 adapter for operations.go. The
// caller already owns db.conn and its write transaction; P2 deletes this adapter
// when Apply binds its own scope.
func (db *DB) checkConditionsLocked(in journal.OperationInput) error {
	return checkConditions(&connScope{conn: db.conn}, in)
}

// checkConditions evaluates conditions through the caller-owned connection
// scope and returns the first typed *journal.ConditionFailure.
func checkConditions(scope *connScope, in journal.OperationInput) error {
	for i, cond := range in.Conditions {
		if err := checkOneCondition(scope, cond, i); err != nil {
			return err
		}
	}
	return nil
}

// checkOneConditionLocked evaluates one condition and returns a typed
// *journal.ConditionFailure when the assertion is not satisfied.
func checkOneCondition(scope *connScope, cond journal.Condition, index int) error {
	switch cond.Kind {
	case journal.ConditionExactFact:
		return checkExactFact(scope, cond, index)
	case journal.ConditionCurrentFact:
		return checkCurrentFact(scope, cond, index)
	default:
		// Canonical validation rejects unknown kinds before Apply; treat as internal error.
		return fmt.Errorf(
			"checkOneConditionLocked: unrecognized condition kind %s at index %d — "+
				"where: Apply condition evaluation; when: before effects; "+
				"impact: operation rejected; "+
				"fix: use ConditionExactFact or ConditionCurrentFact",
			cond.Kind, index)
	}
}

func checkExactFact(scope *connScope, cond journal.Condition, index int) error {
	actual, matched, err := evaluateExactFactSelector(scope, cond.Selector, cond.AssertedJournalID)
	if err != nil {
		return fmt.Errorf("Apply: condition[%d] ExactFact evaluation: %w", index, err)
	}
	if matched {
		return nil
	}
	// Not matched: determine precise reason.
	reason := journal.ConditionFactMissing
	if actual != 0 {
		reason = journal.ConditionFactMismatch
	}
	return &journal.ConditionFailure{
		Index:             index,
		Kind:              journal.ConditionExactFact,
		Reason:            reason,
		AssertedJournalID: cond.AssertedJournalID,
		ActualJournalID:   actual,
	}
}

func checkCurrentFact(scope *connScope, cond journal.Condition, index int) error {
	actual, found, err := evaluateCurrentFactSelector(scope, cond.Selector)
	if err != nil {
		return fmt.Errorf("Apply: condition[%d] CurrentFact evaluation: %w", index, err)
	}

	if cond.AssertedJournalID == 0 {
		// Absence assertion: no matching row must exist.
		if !found {
			return nil // success
		}
		return &journal.ConditionFailure{
			Index:             index,
			Kind:              journal.ConditionCurrentFact,
			Reason:            journal.ConditionCurrentMismatch,
			AssertedJournalID: 0,
			ActualJournalID:   actual,
		}
	}

	// Presence assertion: the asserted JournalID must be the highest match.
	if found && actual == cond.AssertedJournalID {
		return nil // success
	}
	reason := journal.ConditionFactMissing
	if found {
		reason = journal.ConditionCurrentMismatch
	}
	return &journal.ConditionFailure{
		Index:             index,
		Kind:              journal.ConditionCurrentFact,
		Reason:            reason,
		AssertedJournalID: cond.AssertedJournalID,
		ActualJournalID:   actual,
	}
}
