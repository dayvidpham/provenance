package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type replayDDL uint16

const (
	replayDDLCreateShadowComments42a6 replayDDL = iota + 1
	replayDDLCreateShadowEdges8e48
	replayDDLCreateShadowLabels0541
	replayDDLCreateShadowTaskAttributionsdb81
	replayDDLCreateShadowTasks2548
	replayDDLDropShadowComments22a4
	replayDDLDropShadowEdges42d9
	replayDDLDropShadowLabels076c
	replayDDLDropShadowTaskAttributionse1b1
	replayDDLDropShadowTasks814c
)

func (replayDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement replayDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case replayDDLCreateShadowComments42a6:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_comments (\n\t\t\tid         TEXT PRIMARY KEY,\n\t\t\ttask_id    TEXT NOT NULL,\n\t\t\tauthor_id  TEXT NOT NULL,\n\t\t\tbody       TEXT NOT NULL,\n\t\t\tcreated_at INTEGER NOT NULL\n\t\t)", options)
	case replayDDLCreateShadowEdges8e48:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_edges (\n\t\t\tsource_id  TEXT NOT NULL,\n\t\t\ttarget_id  TEXT NOT NULL,\n\t\t\tkind_id    INTEGER NOT NULL,\n\t\t\tcreated_at INTEGER NOT NULL,\n\t\t\tPRIMARY KEY (source_id, target_id, kind_id)\n\t\t)", options)
	case replayDDLCreateShadowLabels0541:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_labels (\n\t\t\ttask_id TEXT NOT NULL,\n\t\t\tname    TEXT NOT NULL,\n\t\t\tPRIMARY KEY (task_id, name)\n\t\t)", options)
	case replayDDLCreateShadowTaskAttributionsdb81:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_task_attributions (\n\t\t\ttask_id          TEXT NOT NULL,\n\t\t\tactor_id         TEXT NOT NULL,\n\t\t\tfirst_journal_id INTEGER NOT NULL,\n\t\t\tPRIMARY KEY (task_id, actor_id)\n\t\t)", options)
	case replayDDLCreateShadowTasks2548:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_tasks (\n\t\t\tid              TEXT PRIMARY KEY,\n\t\t\tnamespace       TEXT,\n\t\t\ttitle           TEXT,\n\t\t\tdescription     TEXT,\n\t\t\towner_id        TEXT,\n\t\t\tstatus_id       INTEGER,\n\t\t\tpriority_id     INTEGER,\n\t\t\ttype_id         INTEGER,\n\t\t\tphase_id        INTEGER,\n\t\t\tnotes           TEXT,\n\t\t\tcreated_at      INTEGER,\n\t\t\tupdated_at      INTEGER,\n\t\t\tclosed_at       INTEGER,\n\t\t\tclose_reason    TEXT,\n\t\t\tlast_journal_id INTEGER\n\t\t)", options)
	case replayDDLDropShadowComments22a4:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_comments", options)
	case replayDDLDropShadowEdges42d9:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_edges", options)
	case replayDDLDropShadowLabels076c:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_labels", options)
	case replayDDLDropShadowTaskAttributionse1b1:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_task_attributions", options)
	case replayDDLDropShadowTasks814c:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_tasks", options)
	default:
		return unknownSQLStatementError("replayDDL", uint16(statement))
	}
}
