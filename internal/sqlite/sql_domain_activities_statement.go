package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type activitiesStatement uint16

const (
	activitiesInsertActivities10d4 activitiesStatement = iota + 1
	activitiesInsertActivities148f
	activitiesSelectActivities629c
	activitiesSelectActivitiesc7f6
	activitiesUpdateActivities487b
)

func (activitiesStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement activitiesStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case activitiesInsertActivities10d4:
		return sqlitex.Execute(conn, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)\n\t\t ON CONFLICT(id) DO NOTHING", options)
	case activitiesInsertActivities148f:
		return sqlitex.Execute(conn, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", options)
	case activitiesSelectActivities629c:
		return sqlitex.Execute(conn, "SELECT id,agent_id,phase_id,stage_id,started_at,ended_at,notes FROM activities WHERE (NOT ?1 OR agent_id=?2) ORDER BY started_at ASC", options)
	case activitiesSelectActivitiesc7f6:
		return sqlitex.Execute(conn, "SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes\n\t\t FROM activities WHERE id = ?1", options)
	case activitiesUpdateActivities487b:
		return sqlitex.Execute(conn, "UPDATE activities SET ended_at = ?2 WHERE id = ?1", options)
	default:
		return unknownSQLStatementError("activitiesStatement", uint16(statement))
	}
}
