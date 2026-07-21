package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type tasksStatement uint16

const (
	tasksDeleteEdges64b4 tasksStatement = iota + 1
	tasksDeleteLabelsdf09
	tasksInsertComments66d3
	tasksInsertEdges3b0d
	tasksInsertLabelscc95
	tasksSelectCommentsc91f
	tasksSelectCommentsd80b
	tasksSelectEdges0685
	tasksSelectEdges9d03
	tasksSelectEdgesfb60
	tasksSelectLabels9e23
	tasksSelectTasks4bcc
	tasksSelectTasks5ced
	tasksSelectTasksce15
	tasksSelectTaskse1a0
	tasksSelectTasksf73a
)

func (tasksStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement tasksStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case tasksDeleteEdges64b4:
		return sqlitex.Execute(conn, "DELETE FROM edges WHERE source_id = ?1 AND target_id = ?2 AND kind_id = ?3", options)
	case tasksDeleteLabelsdf09:
		return sqlitex.Execute(conn, "DELETE FROM labels WHERE task_id = ?1 AND name = ?2", options)
	case tasksInsertComments66d3:
		return sqlitex.Execute(conn, "INSERT INTO comments (id, task_id, author_id, body, created_at) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case tasksInsertEdges3b0d:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO edges (source_id, target_id, kind_id, created_at) VALUES (?1, ?2, ?3, ?4)", options)
	case tasksInsertLabelscc95:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO labels (task_id, name) VALUES (?1, ?2)", options)
	case tasksSelectCommentsc91f:
		return sqlitex.Execute(conn, "SELECT id, task_id, author_id, body, created_at FROM comments WHERE id = ?1", options)
	case tasksSelectCommentsd80b:
		return sqlitex.Execute(conn, "SELECT id, task_id, author_id, body, created_at\n\t\t FROM comments WHERE task_id = ?1 ORDER BY created_at ASC", options)
	case tasksSelectEdges0685:
		return sqlitex.Execute(conn, "SELECT source_id,target_id,kind_id FROM edges WHERE source_id=?1 AND (NOT ?2 OR kind_id=?3) ORDER BY created_at ASC", options)
	case tasksSelectEdges9d03:
		return sqlitex.Execute(conn, "SELECT source_id, target_id FROM edges WHERE kind_id = ?1", options)
	case tasksSelectEdgesfb60:
		return sqlitex.Execute(conn, "SELECT source_id, target_id, kind_id FROM edges WHERE kind_id = ?1 ORDER BY created_at ASC", options)
	case tasksSelectLabels9e23:
		return sqlitex.Execute(conn, "SELECT name FROM labels WHERE task_id = ?1 ORDER BY name ASC", options)
	case tasksSelectTasks4bcc:
		return sqlitex.Execute(conn, "\n\t\tSELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,\n\t\t       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,\n\t\t       t.closed_at, t.close_reason\n\t\tFROM tasks t\n\t\tWHERE t.status_id != ?1\n\t\tAND EXISTS (\n\t\t\tSELECT ?3 FROM edges e\n\t\t\tJOIN tasks blocker ON e.target_id = blocker.id\n\t\t\tWHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1\n\t\t)\n\t\tORDER BY t.priority_id ASC, t.created_at ASC", appendStaticSQLArgs(options, 1))
	case tasksSelectTasks5ced:
		return sqlitex.Execute(conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,\n\t\tphase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks\n\t\tWHERE (NOT ?1 OR status_id=?2)\n\t\t  AND (NOT ?3 OR priority_id=?4)\n\t\t  AND (NOT ?5 OR type_id=?6)\n\t\t  AND (NOT ?7 OR phase_id=?8)\n\t\t  AND (NOT ?9 OR namespace=?10)\n\t\t  AND (NOT ?11 OR EXISTS (SELECT ?13 FROM labels l WHERE l.task_id=tasks.id AND l.name=?12))\n\t\tORDER BY created_at ASC", appendStaticSQLArgs(options, 1))
	case tasksSelectTasksce15:
		return sqlitex.Execute(conn, "SELECT id, namespace, title, description, status_id, priority_id, type_id,\n\t\t        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason\n\t\t FROM tasks WHERE id = ?1", options)
	case tasksSelectTaskse1a0:
		return sqlitex.Execute(conn, "\n\t\tSELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,\n\t\t       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,\n\t\t       t.closed_at, t.close_reason\n\t\tFROM tasks t\n\t\tWHERE t.status_id != ?1\n\t\tAND NOT EXISTS (\n\t\t\tSELECT ?3 FROM edges e\n\t\t\tJOIN tasks blocker ON e.target_id = blocker.id\n\t\t\tWHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1\n\t\t)\n\t\tORDER BY t.priority_id ASC, t.created_at ASC", appendStaticSQLArgs(options, 1))
	case tasksSelectTasksf73a:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM tasks", options)
	default:
		return unknownSQLStatementError("tasksStatement", uint16(statement))
	}
}
