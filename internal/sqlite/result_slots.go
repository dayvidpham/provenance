package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// result_slots.go owns the complete committed-result reconstruction with
// ActivityID slot support and the ValidateResultSlotBinding call-through.
//
// Result-slot reconstruction resolves task and activity identities with direct
// joins to journal_task_events and journal_activity_creations, then validates
// every binding via journal.ValidateResultSlotBinding.

// reconstructAndValidateCommitted builds the complete CommittedResult for
// an anchor journal_id, resolves all result slot bindings (including ActivityID
// for JournalKindActivity slots), validates each binding, and returns the result.
// It does NOT set ShortCircuited — the caller sets that for replay paths.
// The caller owns scope.conn and its transaction.
func (scope *connScope) reconstructAndValidateCommitted(anchor int64) (journal.CommittedResult, error) {
	return reconstructCommitted(scope.ctx, allocationSQLTx{conn: scope.conn}, anchor)
}

// reconstructCommitted is the transaction-neutral committed-result reader used
// by both ordinary Apply and governed composition. Keeping reconstruction on the
// narrow SQLTx contract prevents the two production paths from drifting in event
// ordering, slot typing, or integrity validation.
func reconstructCommitted(ctx context.Context, tx fusedtx.SQLTx, anchor int64) (journal.CommittedResult, error) {
	res := journal.CommittedResult{Kind: journal.CommittedExact, AnchorJournalID: journal.JournalID(anchor)}

	// EmittedEvents: flat task_event closure in JournalID order (§2.1, §3.2).
	rows, err := tx.Query(ctx, `SELECT journal_id FROM journal WHERE produced_by_operation_journal_id = ?1 AND kind_id = ?2 ORDER BY journal_id ASC`, anchor, int(journal.JournalKindTaskEvent))
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct emitted events: %w", err)
	}
	for rows.Next() {
		var journalID int64
		if err := rows.Scan(&journalID); err != nil {
			_ = rows.Close()
			return journal.CommittedResult{}, err
		}
		res.EmittedEvents = append(res.EmittedEvents, journal.JournalID(journalID))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return journal.CommittedResult{}, fmt.Errorf("reconstruct emitted events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("close emitted-event reconstruction rows: %w", err)
	}

	// Slot-keyed result map (§3.2): resolve TaskID for task_event slots and
	// ActivityID for activity slots. Other slot kinds carry neither.
	rows, err = tx.Query(ctx, `SELECT s.result_slot_id, s.produced_journal_id, j.kind_id, j.produced_by_operation_journal_id, te.task_id, ac.activity_id FROM journal_operation_result_slots s JOIN journal j ON j.journal_id = s.produced_journal_id LEFT JOIN journal_task_events te ON te.journal_id = s.produced_journal_id LEFT JOIN journal_activity_creations ac ON ac.journal_id = s.produced_journal_id WHERE s.journal_id = ?1 ORDER BY s.result_slot_id ASC`, anchor)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct result slots: %w", err)
	}
	for rows.Next() {
		var slot, taskID, activityID sql.NullString
		var producedJID int64
		var producer sql.NullInt64
		var kind int
		if err := rows.Scan(&slot, &producedJID, &kind, &producer, &taskID, &activityID); err != nil {
			_ = rows.Close()
			return journal.CommittedResult{}, err
		}
		if !slot.Valid || slot.String == "" || !producer.Valid || producer.Int64 != anchor {
			_ = rows.Close()
			return journal.CommittedResult{}, fmt.Errorf("%w: result slot on operation anchor %d is empty or points to journal row %d produced by another operation", journal.ErrResultSlotIntegrity, anchor, producedJID)
		}
		binding := journal.ResultSlotBinding{
			Slot:              journal.ResultSlotID(slot.String),
			ProducedJournalID: journal.JournalID(producedJID),
			Kind:              journal.JournalKind(kind),
		}
		if taskID.Valid {
			tid, err := journalParseTask(taskID.String)
			if err != nil {
				_ = rows.Close()
				return journal.CommittedResult{}, err
			}
			binding.TaskID = &tid
		}
		if activityID.Valid {
			id, err := ptypes.ParseActivityID(activityID.String)
			if err != nil {
				_ = rows.Close()
				return journal.CommittedResult{}, fmt.Errorf("decode activity result slot %q: %w", slot.String, err)
			}
			binding.ActivityID = &id
		}
		if err := journal.ValidateResultSlotBinding(binding); err != nil {
			_ = rows.Close()
			return journal.CommittedResult{}, fmt.Errorf("%w: result slot %q on operation anchor %d failed validation: %v", journal.ErrResultSlotIntegrity, binding.Slot, anchor, err)
		}
		res.ResultSlots = append(res.ResultSlots, binding)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return journal.CommittedResult{}, fmt.Errorf("reconstruct result slots: %w", err)
	}
	if err := rows.Close(); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("close result-slot reconstruction rows: %w", err)
	}

	return res, nil
}
