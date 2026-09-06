package sqlite

type taskEventQueryOrder uint8

const (
	taskEventQueryJournalOrder taskEventQueryOrder = iota + 1
	taskEventQueryTimelineOrder
)

func (order taskEventQueryOrder) query() string {
	switch order {
	case taskEventQueryJournalOrder:
		return `SELECT j.journal_id,j.effective_actor_id,j.recorded_at,te.task_id,te.event_kind,te.payload,j.produced_by_operation_journal_id
			FROM journal_attributed j JOIN journal_task_events te ON te.journal_id=j.journal_id
			WHERE j.journal_id<=?1 AND j.journal_id>?3
			  AND (NOT ?4 OR te.task_id IN (SELECT value FROM json_each(?5)))
			  AND (NOT ?6 OR te.event_kind IN (SELECT value FROM json_each(?7)))
			  AND (NOT ?8 OR EXISTS (SELECT ?13 FROM journal_task_event_contexts ctx JOIN json_each(?9) f ON ctx.context_kind=json_extract(f.value,?10) AND ctx.context_identity=json_extract(f.value,?11) WHERE ctx.event_journal_id=te.journal_id))
			ORDER BY j.journal_id ASC LIMIT ?12`
	case taskEventQueryTimelineOrder:
		return `SELECT j.journal_id,j.effective_actor_id,j.recorded_at,te.task_id,te.event_kind,te.payload,j.produced_by_operation_journal_id
			FROM journal_attributed j JOIN journal_task_events te ON te.journal_id=j.journal_id
			WHERE j.journal_id<=?1 AND (j.recorded_at>?2 OR (j.recorded_at=?2 AND j.journal_id>?3))
			  AND (NOT ?4 OR te.task_id IN (SELECT value FROM json_each(?5)))
			  AND (NOT ?6 OR te.event_kind IN (SELECT value FROM json_each(?7)))
			  AND (NOT ?8 OR EXISTS (SELECT ?13 FROM journal_task_event_contexts ctx JOIN json_each(?9) f ON ctx.context_kind=json_extract(f.value,?10) AND ctx.context_identity=json_extract(f.value,?11) WHERE ctx.event_journal_id=te.journal_id))
			ORDER BY j.recorded_at ASC,j.journal_id ASC LIMIT ?12`
	default:
		panic("unknown task-event query order")
	}
}
