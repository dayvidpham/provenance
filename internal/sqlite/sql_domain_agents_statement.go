package sqlite

import (
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type agentsStatement uint16

const (
	agentsInsertActorNamespaceClaims7b3c agentsStatement = iota + 1
	agentsInsertActorNamespaceClaims9230
	agentsInsertAgentsHuman8029
	agentsInsertAgentsMl1f20
	agentsInsertAgentsSoftwaref75f
	agentsInsertAgentse4db
	agentsInsertFixedActorManifestEntries73e8
	agentsInsertFixedActorManifestEntries823e
	agentsSelectActorNamespaceClaims091c
	agentsSelectActorNamespaceClaims769b
	agentsSelectAgents480a
	agentsSelectAgentsb65a
	agentsSelectAgentsc468
	agentsSelectAgentsca17
	agentsSelectAgentsdb52
	agentsSelectFixedActorManifestEntriesa609
	agentsSelectMlModels2532
)

func (agentsStatement) statementClass() sqlStatementClass { return sqlDMLStatement }

func (statement agentsStatement) execute(conn *zs.Conn, options *sqlitex.ExecOptions) error {
	switch statement {
	case agentsInsertActorNamespaceClaims7b3c:
		return sqlitex.Execute(conn, "INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case agentsInsertActorNamespaceClaims9230:
		return sqlitex.Execute(conn, "INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case agentsInsertAgentsHuman8029:
		return sqlitex.Execute(conn, "INSERT INTO agents_human (agent_id, name, contact) VALUES (?1, ?2, ?3)", options)
	case agentsInsertAgentsMl1f20:
		return sqlitex.Execute(conn, "INSERT INTO agents_ml (agent_id, role_id, model_id) VALUES (?1, ?2, ?3)", options)
	case agentsInsertAgentsSoftwaref75f:
		return sqlitex.Execute(conn, "INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)", options)
	case agentsInsertAgentse4db:
		return sqlitex.Execute(conn, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", options)
	case agentsInsertFixedActorManifestEntries73e8:
		return sqlitex.Execute(conn, "INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case agentsInsertFixedActorManifestEntries823e:
		return sqlitex.Execute(conn, "INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case agentsSelectActorNamespaceClaims091c:
		return sqlitex.Execute(conn, "SELECT namespace, claimant_id, range_min, range_max, codec\n\t\t FROM actor_namespace_claims WHERE namespace = ?1", options)
	case agentsSelectActorNamespaceClaims769b:
		return sqlitex.Execute(conn, "SELECT namespace, claimant_id, range_min, range_max, codec\n\t\t FROM actor_namespace_claims ORDER BY namespace ASC", options)
	case agentsSelectAgents480a:
		return sqlitex.Execute(conn, "SELECT id, kind_id FROM agents WHERE id = ?1", options)
	case agentsSelectAgentsb65a:
		return sqlitex.Execute(conn, "SELECT a.kind_id, h.name, h.contact\n\t\t FROM agents a JOIN agents_human h ON a.id = h.agent_id\n\t\t WHERE a.id = ?1", options)
	case agentsSelectAgentsc468:
		return sqlitex.Execute(conn, "SELECT a.kind_id, s.name, s.version, s.source\n\t\t FROM agents a JOIN agents_software s ON a.id = s.agent_id\n\t\t WHERE a.id = ?1", options)
	case agentsSelectAgentsca17:
		return sqlitex.Execute(conn, "SELECT a.kind_id, s.name, s.version, s.source FROM agents a LEFT JOIN agents_software s ON s.agent_id = a.id WHERE a.id = ?1", options)
	case agentsSelectAgentsdb52:
		return sqlitex.Execute(conn, "SELECT a.kind_id, m.role_id, ml.id, p.name, ml.name\n\t\t FROM agents a\n\t\t JOIN agents_ml m ON a.id = m.agent_id\n\t\t JOIN ml_models ml ON m.model_id = ml.id\n\t\t JOIN providers p ON ml.provider_id = p.id\n\t\t WHERE a.id = ?1", options)
	case agentsSelectFixedActorManifestEntriesa609:
		return sqlitex.Execute(conn, "SELECT actor_id, namespace, kind_id, name, metadata FROM fixed_actor_manifest_entries WHERE actor_id = ?1 OR (namespace = ?2 AND name = ?3)", options)
	case agentsSelectMlModels2532:
		return sqlitex.Execute(conn, "SELECT id FROM ml_models WHERE provider_id = (SELECT id FROM providers WHERE name = ?1) AND name = ?2", options)
	default:
		return unknownSQLStatementError("agentsStatement", uint16(statement))
	}
}
