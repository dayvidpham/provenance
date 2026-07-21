package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type schemaDDL uint16

const (
	schemaDDLCreateActivitiesf3ac schemaDDL = iota + 1
	schemaDDLCreateAgentKindsde98
	schemaDDLCreateAgents8133
	schemaDDLCreateAgentsHuman28d6
	schemaDDLCreateAgentsMle8b0
	schemaDDLCreateAgentsSoftware6c89
	schemaDDLCreateComments320c
	schemaDDLCreateEdgeKinds7508
	schemaDDLCreateEdges25c6
	schemaDDLCreateIdxActivitiesAgent206d
	schemaDDLCreateIdxActivitiesPhase7b2e
	schemaDDLCreateIdxCommentsAuthor1138
	schemaDDLCreateIdxCommentsTaskbbf3
	schemaDDLCreateIdxEdgesKindad6f
	schemaDDLCreateIdxEdgesSource8c95
	schemaDDLCreateIdxEdgesTargetd8ae
	schemaDDLCreateIdxLabelsName2879
	schemaDDLCreateIdxTasksOwner7d8b
	schemaDDLCreateIdxTasksPhase5aa3
	schemaDDLCreateIdxTasksPriority3f16
	schemaDDLCreateIdxTasksStatusa4f0
	schemaDDLCreateIdxTasksTypeae99
	schemaDDLCreateLabels4203
	schemaDDLCreateMlModelsb48d
	schemaDDLCreatePhases6e89
	schemaDDLCreatePriorities67d7
	schemaDDLCreateProviders8ba5
	schemaDDLCreateRoles0ada
	schemaDDLCreateStagesdffc
	schemaDDLCreateStatusesf4f1
	schemaDDLCreateTaskTypes4c16
	schemaDDLCreateTaskscc07
	schemaDDLPragmaBusyTimeout44ea
	schemaDDLPragmaForeignKeyscc13
	schemaDDLPragmaJournalMode606c
)

func (schemaDDL) statementClass() sqlStatementClass { return sqlDDLStatement }

func (statement schemaDDL) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case schemaDDLCreateActivitiesf3ac:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS activities (\n\t\t\tid         TEXT PRIMARY KEY,\n\t\t\tagent_id   TEXT NOT NULL REFERENCES agents(id),\n\t\t\tphase_id   INTEGER NOT NULL REFERENCES phases(id),\n\t\t\tstage_id   INTEGER NOT NULL REFERENCES stages(id),\n\t\t\tstarted_at INTEGER NOT NULL,\n\t\t\tended_at   INTEGER,\n\t\t\tnotes      TEXT NOT NULL DEFAULT ''\n\t\t) STRICT", options)
	case schemaDDLCreateAgentKindsde98:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agent_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateAgents8133:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents (\n\t\t\tid      TEXT PRIMARY KEY,\n\t\t\tkind_id INTEGER NOT NULL REFERENCES agent_kinds(id)\n\t\t) STRICT", options)
	case schemaDDLCreateAgentsHuman28d6:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents_human (\n\t\t\tagent_id TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\tname     TEXT NOT NULL,\n\t\t\tcontact  TEXT NOT NULL DEFAULT ''\n\t\t) STRICT, WITHOUT ROWID", options)
	case schemaDDLCreateAgentsMle8b0:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents_ml (\n\t\t\tagent_id TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\trole_id  INTEGER NOT NULL REFERENCES roles(id),\n\t\t\tmodel_id INTEGER NOT NULL REFERENCES ml_models(id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case schemaDDLCreateAgentsSoftware6c89:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents_software (\n\t\t\tagent_id TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\tname     TEXT NOT NULL,\n\t\t\tversion  TEXT NOT NULL DEFAULT '',\n\t\t\tsource   TEXT NOT NULL DEFAULT ''\n\t\t) STRICT, WITHOUT ROWID", options)
	case schemaDDLCreateComments320c:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS comments (\n\t\t\tid         TEXT PRIMARY KEY,\n\t\t\ttask_id    TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tauthor_id  TEXT NOT NULL REFERENCES agents(id),\n\t\t\tbody       TEXT NOT NULL,\n\t\t\tcreated_at INTEGER NOT NULL\n\t\t) STRICT", options)
	case schemaDDLCreateEdgeKinds7508:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS edge_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateEdges25c6:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS edges (\n\t\t\tsource_id  TEXT NOT NULL REFERENCES tasks(id),\n\t\t\ttarget_id  TEXT NOT NULL,\n\t\t\tkind_id    INTEGER NOT NULL REFERENCES edge_kinds(id),\n\t\t\tcreated_at INTEGER NOT NULL,\n\t\t\tPRIMARY KEY (source_id, target_id, kind_id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case schemaDDLCreateIdxActivitiesAgent206d:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_activities_agent ON activities (agent_id)", options)
	case schemaDDLCreateIdxActivitiesPhase7b2e:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_activities_phase ON activities (phase_id)", options)
	case schemaDDLCreateIdxCommentsAuthor1138:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_comments_author ON comments (author_id)", options)
	case schemaDDLCreateIdxCommentsTaskbbf3:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_comments_task   ON comments (task_id)", options)
	case schemaDDLCreateIdxEdgesKindad6f:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_edges_kind   ON edges (kind_id)", options)
	case schemaDDLCreateIdxEdgesSource8c95:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_edges_source ON edges (source_id)", options)
	case schemaDDLCreateIdxEdgesTargetd8ae:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_edges_target ON edges (target_id)", options)
	case schemaDDLCreateIdxLabelsName2879:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_labels_name ON labels (name)", options)
	case schemaDDLCreateIdxTasksOwner7d8b:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_owner     ON tasks (owner_id)", options)
	case schemaDDLCreateIdxTasksPhase5aa3:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_phase     ON tasks (phase_id)", options)
	case schemaDDLCreateIdxTasksPriority3f16:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_priority  ON tasks (priority_id)", options)
	case schemaDDLCreateIdxTasksStatusa4f0:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_status    ON tasks (status_id)", options)
	case schemaDDLCreateIdxTasksTypeae99:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_type      ON tasks (type_id)", options)
	case schemaDDLCreateLabels4203:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS labels (\n\t\t\ttask_id TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tname    TEXT NOT NULL,\n\t\t\tPRIMARY KEY (task_id, name)\n\t\t) STRICT, WITHOUT ROWID", options)
	case schemaDDLCreateMlModelsb48d:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS ml_models (\n\t\t\tid          INTEGER PRIMARY KEY,\n\t\t\tprovider_id INTEGER NOT NULL REFERENCES providers(id),\n\t\t\tname        TEXT NOT NULL,\n\t\t\tUNIQUE (provider_id, name)\n\t\t) STRICT", options)
	case schemaDDLCreatePhases6e89:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS phases (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreatePriorities67d7:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS priorities (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateProviders8ba5:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS providers (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateRoles0ada:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS roles (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateStagesdffc:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateStatusesf4f1:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS statuses (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateTaskTypes4c16:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS task_types (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case schemaDDLCreateTaskscc07:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS tasks (\n\t\t\tid TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL,\n\t\t\tdescription TEXT NOT NULL DEFAULT '', status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),\n\t\t\tpriority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id), type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),\n\t\t\tphase_id INTEGER NOT NULL REFERENCES phases(id), owner_id TEXT REFERENCES agents(id), notes TEXT NOT NULL DEFAULT '',\n\t\t\tcreated_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, closed_at INTEGER, close_reason TEXT NOT NULL DEFAULT '',\n\t\t\tlast_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)\n\t\t) STRICT", options)
	case schemaDDLPragmaBusyTimeout44ea:
		return sqlitex.ExecuteTransient(conn, "PRAGMA busy_timeout=5000;", options)
	case schemaDDLPragmaForeignKeyscc13:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=OFF;", options)
	case schemaDDLPragmaJournalMode606c:
		return sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode=WAL", options)
	default:
		return unknownSQLStatementError("schemaDDL", uint16(statement))
	}
}
