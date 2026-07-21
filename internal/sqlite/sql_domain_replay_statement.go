package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type replayStatement uint16

const (
	replayInsertShadowTasks659b replayStatement = iota + 1
	replayInsertShadowTasks93b3
	replayInsertShadowTasksdbeb
	replaySelectCommentscd77
	replaySelectEdges707e
	replaySelectJournal17ea
	replaySelectJournal4f1e
	replaySelectJournal4f3e
	replaySelectJournal7295
	replaySelectJournalAttributed70dd
	replaySelectJournalAuthorities174f
	replaySelectJournalAuthorityAssignmentEpisodes3c0a
	replaySelectJournalAuthorityAssignmentTransitions70c5
	replaySelectJournalAuthorityAssignmentTransitions79ed
	replaySelectJournalAuthorityAssignmentTransitionse07b
	replaySelectJournalDecisions2490
	replaySelectJournalDecisionscad7
	replaySelectJournalEvidence13bb
	replaySelectJournalEvidenceccc4
	replaySelectJournalOperationResultSlotsa304
	replaySelectJournalOperations19b2
	replaySelectJournalOperations571e
	replaySelectJournalTaskEventContexts0a57
	replaySelectJournalTaskEvents4a71
	replaySelectJournalTaskEvents520f
	replaySelectJournalTaskEventsd7aa
	replaySelectJournalf459
	replaySelectLabelsa071
	replaySelectShadowComments2e53
	replaySelectShadowEdges38a7
	replaySelectShadowLabels47d0
	replaySelectShadowTaskAttributions836a
	replaySelectShadowTasks1015
	replaySelectShadowTasks21a5
	replaySelectShadowTasks7408
	replaySelectTaskAttributionsfd41
	replaySelectTasks9c6c
	replaySelectTasksd8ec
	replaySelectTasksddad
	replayUpdateShadowTasks7195
	replayUpdateShadowTasks76f4
	replayUpdateTaskse066
)

func (replayStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement replayStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case replayInsertShadowTasks659b:
		return sqlitex.Execute(conn, "INSERT INTO shadow_tasks (id,namespace,title,description,owner_id,status_id,priority_id,type_id,phase_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT t.id,t.namespace,t.title,t.description,?4,?1,t.priority_id,t.type_id,t.phase_id,t.notes,t.created_at,t.updated_at,?5,t.close_reason,?6 FROM tasks t WHERE EXISTS (SELECT ?7 FROM journal_task_events e JOIN journal j ON j.journal_id=e.journal_id JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE e.task_id=t.id AND ((e.event_kind=?2 AND o.canonical_mutation IS ?8) OR e.event_kind=?3))", appendStaticSQLArgs(options, nil, nil, nil, 1, nil))
	case replayInsertShadowTasks93b3:
		return sqlitex.Execute(conn, "INSERT INTO shadow_tasks\n\t\t (id, namespace, title, description, owner_id, status_id, priority_id, type_id,\n\t\t  phase_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?12, ?5, ?6, ?7, ?8, ?9, ?10, ?10, ?13, ?9, ?11)", appendStaticSQLArgs(options, nil, nil))
	case replayInsertShadowTasksdbeb:
		return sqlitex.Execute(conn, "INSERT INTO shadow_tasks (id,namespace,title,description,owner_id,status_id,priority_id,type_id,phase_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT t.id,t.namespace,t.title,t.description,?4,?1,t.priority_id,t.type_id,t.phase_id,t.notes,t.created_at,t.updated_at,?5,t.close_reason,?6 FROM tasks t WHERE EXISTS (SELECT ?7 FROM journal_task_events e JOIN journal j ON j.journal_id=e.journal_id JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE e.task_id=t.id AND (e.event_kind=?2 OR e.event_kind=?3))", appendStaticSQLArgs(options, nil, nil, nil, 1))
	case replaySelectCommentscd77:
		return sqlitex.Execute(conn, "SELECT id,task_id,author_id,body,created_at FROM comments", options)
	case replaySelectEdges707e:
		return sqlitex.Execute(conn, "SELECT source_id,target_id,kind_id,created_at FROM edges", options)
	case replaySelectJournal17ea:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id<=?2", options)
	case replaySelectJournal4f1e:
		return sqlitex.Execute(conn, "SELECT o.journal_id,o.mutation_encoding_version,o.canonical_mutation FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.journal_id=?1", options)
	case replaySelectJournal4f3e:
		return sqlitex.Execute(conn, "SELECT kind_id, recorded_at FROM journal WHERE journal_id=?1", options)
	case replaySelectJournal7295:
		return sqlitex.Execute(conn, "SELECT o.journal_id,?2,?3 FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.journal_id=?1", appendStaticSQLArgs(options, nil, nil))
	case replaySelectJournalAttributed70dd:
		return sqlitex.Execute(conn, "SELECT kind_id, effective_actor_id, recorded_at FROM journal_attributed WHERE journal_id = ?1", options)
	case replaySelectJournalAuthorities174f:
		return sqlitex.Execute(conn, "SELECT a.operation_authority_id, b.label FROM journal_authorities a JOIN journal_authority_bootstraps b ON b.journal_id=a.journal_id WHERE a.journal_id=?1", options)
	case replaySelectJournalAuthorityAssignmentEpisodes3c0a:
		return sqlitex.Execute(conn, "SELECT task_id, actor_id, slot_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", options)
	case replaySelectJournalAuthorityAssignmentTransitions70c5:
		return sqlitex.Execute(conn, "SELECT assignment_id, transition_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", options)
	case replaySelectJournalAuthorityAssignmentTransitions79ed:
		return sqlitex.Execute(conn, "SELECT e.assignment_id,e.task_id,e.slot_id,e.actor_id,e.predecessor_assignment_id,e.parent_assignment_id,t.transition_id FROM journal_authority_assignment_transitions t JOIN journal_authority_assignment_episodes e ON e.assignment_id=t.assignment_id WHERE t.journal_id=?1", options)
	case replaySelectJournalAuthorityAssignmentTransitionse07b:
		return sqlitex.Execute(conn, "SELECT assignment_id,transition_id FROM journal_authority_assignment_transitions WHERE journal_id=?1", options)
	case replaySelectJournalDecisions2490:
		return sqlitex.Execute(conn, "SELECT decision_kind,task_id,payload FROM journal_decisions WHERE journal_id=?1", options)
	case replaySelectJournalDecisionscad7:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_decisions WHERE journal_id=?1", options)
	case replaySelectJournalEvidence13bb:
		return sqlitex.Execute(conn, "SELECT evidence_kind,task_id,hex(content_digest),payload FROM journal_evidence WHERE journal_id=?1", options)
	case replaySelectJournalEvidenceccc4:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_evidence WHERE journal_id=?1", options)
	case replaySelectJournalOperationResultSlotsa304:
		return sqlitex.Execute(conn, "SELECT result_slot_id,produced_journal_id FROM journal_operation_result_slots WHERE journal_id=?1", options)
	case replaySelectJournalOperations19b2:
		return sqlitex.Execute(conn, "SELECT o.journal_id,o.authority_journal_id,o.mutation_encoding_version,o.canonical_mutation,o.mutation_digest,j.actor_id,j.recorded_at FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id ORDER BY o.journal_id", options)
	case replaySelectJournalOperations571e:
		return sqlitex.Execute(conn, "SELECT o.journal_id,o.authority_journal_id,?1,?2,o.mutation_digest,j.actor_id,j.recorded_at FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id ORDER BY o.journal_id", appendStaticSQLArgs(options, nil, nil))
	case replaySelectJournalTaskEventContexts0a57:
		return sqlitex.Execute(conn, "SELECT attached_by_journal_id FROM journal_task_event_contexts WHERE event_journal_id=?1 ORDER BY context_kind,context_identity", options)
	case replaySelectJournalTaskEvents4a71:
		return sqlitex.Execute(conn, "SELECT task_id,event_kind,payload FROM journal_task_events WHERE journal_id=?1", options)
	case replaySelectJournalTaskEvents520f:
		return sqlitex.Execute(conn, "SELECT task_id, event_kind, payload FROM journal_task_events WHERE journal_id = ?1", options)
	case replaySelectJournalTaskEventsd7aa:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_task_events\n\t\t UNION SELECT task_id FROM journal_authority_assignment_episodes\n\t\t UNION SELECT task_id FROM journal_decisions WHERE task_id IS NOT ?1\n\t\t UNION SELECT task_id FROM journal_evidence WHERE task_id IS NOT ?2", appendStaticSQLArgs(options, nil, nil))
	case replaySelectJournalf459:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal ORDER BY journal_id ASC", options)
	case replaySelectLabelsa071:
		return sqlitex.Execute(conn, "SELECT task_id,name FROM labels", options)
	case replaySelectShadowComments2e53:
		return sqlitex.Execute(conn, "SELECT id,task_id,author_id,body,created_at FROM shadow_comments", options)
	case replaySelectShadowEdges38a7:
		return sqlitex.Execute(conn, "SELECT source_id,target_id,kind_id,created_at FROM shadow_edges", options)
	case replaySelectShadowLabels47d0:
		return sqlitex.Execute(conn, "SELECT task_id,name FROM shadow_labels", options)
	case replaySelectShadowTaskAttributions836a:
		return sqlitex.Execute(conn, "SELECT task_id,actor_id,first_journal_id FROM shadow_task_attributions", options)
	case replaySelectShadowTasks1015:
		return sqlitex.Execute(conn, "SELECT status_id FROM shadow_tasks WHERE id=?1", options)
	case replaySelectShadowTasks21a5:
		return sqlitex.Execute(conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM shadow_tasks", options)
	case replaySelectShadowTasks7408:
		return sqlitex.Execute(conn, "SELECT id,owner_id,status_id,last_journal_id FROM shadow_tasks", options)
	case replaySelectTaskAttributionsfd41:
		return sqlitex.Execute(conn, "SELECT task_id,actor_id,first_journal_id FROM task_attributions", options)
	case replaySelectTasks9c6c:
		return sqlitex.Execute(conn, "SELECT status_id FROM tasks WHERE id=?1", options)
	case replaySelectTasksd8ec:
		return sqlitex.Execute(conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks", options)
	case replaySelectTasksddad:
		return sqlitex.Execute(conn, "SELECT id,owner_id,status_id,last_journal_id FROM tasks", options)
	case replayUpdateShadowTasks7195:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET\n\t\tupdated_at=?1,\n\t\ttitle=CASE WHEN ?2 THEN ?3 ELSE title END,\n\t\tdescription=CASE WHEN ?4 THEN ?5 ELSE description END,\n\t\tpriority_id=CASE WHEN ?6 THEN ?7 ELSE priority_id END,\n\t\tphase_id=CASE WHEN ?8 THEN ?9 ELSE phase_id END,\n\t\tnotes=CASE WHEN ?10 THEN ?11 ELSE notes END,\n\t\tclose_reason=CASE WHEN ?12 THEN ?13 ELSE close_reason END\n\t\tWHERE id=?14", options)
	case replayUpdateShadowTasks76f4:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4", options)
	case replayUpdateTaskse066:
		return sqlitex.Execute(conn, "UPDATE tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4", options)
	default:
		return unknownSQLStatementError("replayStatement", uint16(statement))
	}
}
