package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
)

// fact_conditions.go evaluates the transaction-local ExactFact and CurrentFact
// conditions defined in §9.5. Evaluation runs inside the Apply write transaction
// after the exact-replay lookup and before any effects are folded, so conditions
// observe the same snapshot as the effects they gate.
//
// Concurrent contenders: two goroutines racing a CurrentFact condition both
// acquire the SQLite write lock via BEGIN IMMEDIATE (runImmediateTransaction).
// The loser finds the winner's committed fact as the current row and receives
// ConditionFailure rather than BUSY_SNAPSHOT.
//
// All evaluation is bounded (MaxCanonicalConditions = 64); the canonical layer
// enforces this before Apply is called.

// checkConditions evaluates conditions through the caller-owned connection
// scope and returns the first typed *journal.ConditionFailure.
func checkConditions(scope *connScope, in journal.OperationInput) error {
	reader := allocationSQLTx{conn: scope.conn}
	return checkConditionsInTransaction(scope.ctx, reader, in.Conditions, func(relation factContextRelation, id journal.JournalID) error {
		_, err := verifySelectedFactContextInTransaction(scope.ctx, reader, relation, int64(id))
		return err
	})
}

type selectedFactVerifier func(factContextRelation, journal.JournalID) error

// checkConditionsInTransaction is the single condition/fact-selection engine
// used by ordinary Apply and composed allocation.  Callers provide only the
// transaction reader and the selected-row integrity verifier appropriate to
// their transaction adapter.
func checkConditionsInTransaction(ctx context.Context, reader fusedtx.SQLReader, conditions []journal.Condition, verify selectedFactVerifier) error {
	for i, cond := range conditions {
		if err := checkOneConditionInTransaction(ctx, reader, verify, cond, i); err != nil {
			return err
		}
	}
	return nil
}

func checkOneConditionInTransaction(ctx context.Context, reader fusedtx.SQLReader, verify selectedFactVerifier, cond journal.Condition, index int) error {
	binding, err := buildFactMatchBinding(cond.Selector, 0, 0)
	if err != nil {
		return fmt.Errorf("condition[%d] selector binding: %w", index, err)
	}
	latest, found, err := latestFactMatchReader(ctx, reader, binding)
	if err != nil {
		return fmt.Errorf("condition[%d] current-fact lookup: %w", index, err)
	}
	if found {
		if err := verify(binding.contexts, latest); err != nil {
			return err
		}
	}
	matched := false
	switch cond.Kind {
	case journal.ConditionCurrentFact:
		matched = (!found && cond.AssertedJournalID == 0) || (found && latest == cond.AssertedJournalID)
	case journal.ConditionExactFact:
		if cond.AssertedJournalID > 0 {
			exactArgs := append(append([]any(nil), binding.args...), int64(cond.AssertedJournalID))
			var exact int64
			exactErr := reader.QueryRow(ctx, binding.kind.exactMatchSQL(), exactArgs...).Scan(&exact)
			matched = exactErr == nil
			if exactErr != nil && !fusedtx.IsNoRows(exactErr) {
				return fmt.Errorf("condition[%d] exact-fact lookup: %w", index, exactErr)
			}
			if matched {
				if err := verify(binding.contexts, cond.AssertedJournalID); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("condition[%d] has unsupported kind %s", index, cond.Kind)
	}
	if matched {
		return nil
	}
	reason := journal.ConditionFactMissing
	if found {
		if cond.Kind == journal.ConditionExactFact {
			reason = journal.ConditionFactMismatch
		} else {
			reason = journal.ConditionCurrentMismatch
		}
	}
	return &journal.ConditionFailure{Index: index, Kind: cond.Kind, Reason: reason, AssertedJournalID: cond.AssertedJournalID, ActualJournalID: latest}
}

func latestFactMatchReader(ctx context.Context, reader fusedtx.SQLReader, binding factMatchBinding) (journal.JournalID, bool, error) {
	var latest sql.NullInt64
	if err := reader.QueryRow(ctx, binding.kind.latestMatchSQL(), binding.args...).Scan(&latest); err != nil {
		return 0, false, err
	}
	if !latest.Valid {
		return 0, false, nil
	}
	return journal.JournalID(latest.Int64), true, nil
}
