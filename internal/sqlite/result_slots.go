package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// result_slots.go owns the complete committed-result reconstruction with
// ActivityID slot support and the ValidateResultSlotBinding call-through.
//
// reconstructAndValidateCommittedLocked replaces the prior inline reconstruction
// in operations_helpers.go. The new version resolves ActivityID for
// JournalKindActivity result slots using lookupActivityIDForJournalRowLocked
// (activity_replay.go) and then validates all bindings via
// journal.ValidateResultSlotBinding.

// reconstructAndValidateCommittedLocked builds the complete CommittedResult for
// an anchor journal_id, resolves all result slot bindings (including ActivityID
// for JournalKindActivity slots), validates each binding, and returns the result.
// It does NOT set ShortCircuited — the caller sets that for replay paths.
// The caller must hold db.mu and be inside a transaction.
func (db *DB) reconstructAndValidateCommittedLocked(anchor int64) (journal.CommittedResult, error) {
	res := journal.CommittedResult{Kind: journal.CommittedExact, AnchorJournalID: journal.JournalID(anchor)}

	// EmittedEvents: flat task_event closure in JournalID order (§2.1, §3.2).
	if err := sqlitex.Execute(db.conn,
		`SELECT journal_id FROM journal WHERE produced_by_operation_journal_id = ?1 AND kind_id = ?2 ORDER BY journal_id ASC`,
		&sqlitex.ExecOptions{
			Args: []any{anchor, int(journal.JournalKindTaskEvent)},
			ResultFunc: func(stmt *zs.Stmt) error {
				res.EmittedEvents = append(res.EmittedEvents, journal.JournalID(stmt.ColumnInt64(0)))
				return nil
			},
		}); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct emitted events: %w", err)
	}

	// Slot-keyed result map (§3.2): resolve TaskID for task_event slots and
	// ActivityID for activity slots. Other slot kinds carry neither.
	if err := sqlitex.Execute(db.conn,
		`SELECT s.result_slot_id, s.produced_journal_id, j.kind_id, te.task_id FROM journal_operation_result_slots s JOIN journal j ON j.journal_id = s.produced_journal_id LEFT JOIN journal_task_events te ON te.journal_id = s.produced_journal_id WHERE s.journal_id = ?1 ORDER BY s.result_slot_id ASC`,
		&sqlitex.ExecOptions{
			Args: []any{anchor},
			ResultFunc: func(stmt *zs.Stmt) error {
				binding := journal.ResultSlotBinding{
					Slot:              journal.ResultSlotID(stmt.ColumnText(0)),
					ProducedJournalID: journal.JournalID(stmt.ColumnInt64(1)),
					Kind:              journal.JournalKind(stmt.ColumnInt(2)),
				}
				if stmt.ColumnType(3) != zs.TypeNull {
					if tid, err := journalParseTask(stmt.ColumnText(3)); err == nil {
						binding.TaskID = &tid
					}
				}
				res.ResultSlots = append(res.ResultSlots, binding)
				return nil
			},
		}); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct result slots: %w", err)
	}

	// Resolve ActivityID for JournalKindActivity slots.
	for i := range res.ResultSlots {
		if res.ResultSlots[i].Kind != journal.JournalKindActivity {
			continue
		}
		jid := int64(res.ResultSlots[i].ProducedJournalID)
		actID, found, err := db.lookupActivityIDForJournalRowLocked(jid)
		if err != nil {
			return journal.CommittedResult{}, fmt.Errorf(
				"reconstruct activity result slot %q (journal row %d): %w",
				res.ResultSlots[i].Slot, jid, err)
		}
		if !found {
			return journal.CommittedResult{}, fmt.Errorf(
				"%w: result slot %q maps to journal row %d (kind=activity) "+
					"but no journal_activity_creations row exists — "+
					"where: result slot reconstruction; when: after Apply commit; "+
					"impact: Apply returns an integrity error and must not be used; "+
					"fix: restore journal_activity_creations from a known-good backup",
				journal.ErrResultSlotIntegrity, res.ResultSlots[i].Slot, jid)
		}
		res.ResultSlots[i].ActivityID = &actID
	}

	// Validate all resolved bindings atomically.
	for _, binding := range res.ResultSlots {
		if err := journal.ValidateResultSlotBinding(binding); err != nil {
			return journal.CommittedResult{}, fmt.Errorf(
				"%w: result slot %q on operation anchor %d failed validation: %v — "+
					"where: Apply post-commit slot validation; when: after effects committed; "+
					"impact: Apply fails with this error; the rows are already committed; "+
					"fix: this indicates a bug in the effect fold or schema; "+
					"restore from a known-good backup or contact the maintainer",
				journal.ErrResultSlotIntegrity, binding.Slot, anchor, err)
		}
	}

	return res, nil
}
