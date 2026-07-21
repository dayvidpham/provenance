package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type schemaStatement uint16

const (
	schemaInsertAgentKinds1cc0 schemaStatement = iota + 1
	schemaInsertEdgeKinds6327
	schemaInsertMlModels20ed
	schemaInsertPhasesea1e
	schemaInsertPriorities8039
	schemaInsertProviders3199
	schemaInsertRolese069
	schemaInsertStagesa935
	schemaInsertStatusese359
	schemaInsertTaskTypes54b6
	schemaSelectMlModels27ce
)

func (schemaStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement schemaStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case schemaInsertAgentKinds1cc0:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO agent_kinds (id,name) VALUES (?1,?2)", options)
	case schemaInsertEdgeKinds6327:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO edge_kinds (id,name) VALUES (?1,?2)", options)
	case schemaInsertMlModels20ed:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO ml_models (provider_id, name) VALUES ((SELECT id FROM providers WHERE name = ?1), ?2)", options)
	case schemaInsertPhasesea1e:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO phases (id,name) VALUES (?1,?2)", options)
	case schemaInsertPriorities8039:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO priorities (id,name) VALUES (?1,?2)", options)
	case schemaInsertProviders3199:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO providers (id,name) VALUES (?1,?2)", options)
	case schemaInsertRolese069:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO roles (id,name) VALUES (?1,?2)", options)
	case schemaInsertStagesa935:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO stages (id,name) VALUES (?1,?2)", options)
	case schemaInsertStatusese359:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO statuses (id,name) VALUES (?1,?2)", options)
	case schemaInsertTaskTypes54b6:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO task_types (id,name) VALUES (?1,?2)", options)
	case schemaSelectMlModels27ce:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM ml_models", options)
	default:
		return unknownSQLStatementError("schemaStatement", uint16(statement))
	}
}
