package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

type DuplicateMLAgentError struct{ ExistingID ptypes.AgentID }

func (e *DuplicateMLAgentError) Error() string {
	return fmt.Sprintf("register admitted ML agent: identical registration already exists as %q; reuse that identity", e.ExistingID)
}

// RegisterAdmittedMLAgentContext inserts just the admitted model and its agent.
// It does not seed the open-time registry or run activation.
func (db *DB) RegisterAdmittedMLAgentContext(ctx context.Context, namespace string, role ptypes.Role, provider ptypes.Provider, model ptypes.ModelID) (agent ptypes.MLAgent, err error) {
	if ctx == nil {
		return agent, errors.New("register admitted ML agent: nil context; supply a live context")
	}
	defer func() {
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
				err = errors.Join(err, cause)
			}
			if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
				err = errors.Join(err, ctx.Err())
			}
		}
	}()
	if namespace == "" || !utf8.ValidString(namespace) || strings.ContainsFunc(namespace, unicode.IsControl) ||
		!role.IsValid() || role == ptypes.RoleHuman ||
		!exactMLIdentity(string(provider)) || strings.ContainsAny(string(provider), "/#") ||
		!exactMLIdentity(string(model)) || strings.ContainsRune(string(model), '#') {
		return agent, errors.New("register admitted ML agent: invalid namespace, ML role, or exact provider/base-model pair; no identity created")
	}
	if err := ctx.Err(); err != nil {
		return agent, err
	}
	scope, err := db.bindScope(ctx, projectionTargetLive)
	if err != nil {
		return agent, fmt.Errorf("register admitted ML agent: lease connection: %w", err)
	}
	defer scope.release()
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	var modelID int
	err = runRegistrationTransaction(scope, func() error {
		var foreignKeys int
		if err := scope.conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			return err
		}
		if foreignKeys != 1 {
			return errors.New("foreign keys are disabled; enable foreign keys on the registration connection")
		}
		before, proofIDs, eligible := scope.admittedMLLedgerBaseline()
		var providerID int
		var providerAdded, modelAdded int64
		err := scope.conn.QueryRowContext(ctx, "SELECT id FROM providers WHERE name = ?1", string(provider)).Scan(&providerID)
		if errors.Is(err, sql.ErrNoRows) {
			err = scope.conn.QueryRowContext(ctx, "INSERT INTO providers (name) VALUES (?1) RETURNING id", string(provider)).Scan(&providerID)
			providerAdded = 1
		}
		if err != nil {
			return fmt.Errorf("resolve provider: %w", err)
		}
		err = scope.conn.QueryRowContext(ctx, "SELECT id FROM ml_models WHERE provider_id = ?1 AND name = ?2", providerID, string(model)).Scan(&modelID)
		if errors.Is(err, sql.ErrNoRows) {
			err = scope.conn.QueryRowContext(ctx, "INSERT INTO ml_models (provider_id, name) VALUES (?1, ?2) RETURNING id", providerID, string(model)).Scan(&modelID)
			modelAdded = 1
		}
		if err != nil {
			return fmt.Errorf("resolve model: %w", err)
		}
		if err := scope.rejectDuplicateMLAgent(namespace, role, modelID); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(ctx, insertAgentSQL, id.String(), int(ptypes.AgentKindMachineLearning)); err != nil {
			return fmt.Errorf("insert agent: %w", err)
		}
		if _, err := scope.conn.ExecContext(ctx, "INSERT INTO agents_ml (agent_id, role_id, model_id) VALUES (?1, ?2, ?3)", id.String(), int(role), modelID); err != nil {
			return fmt.Errorf("insert ML subtype: %w", err)
		}
		if !eligible {
			return nil
		}
		after, err := scope.readVerifiedStoreIdentity()
		if err != nil {
			return err
		}
		expected := map[int]int64{
			verifiedTableID("providers"): providerAdded, verifiedTableID("ml_models"): modelAdded,
			verifiedTableID("agents"): 1, verifiedTableID("agents_ml"): 1,
		}
		if !admittedMLDeltaMatches(before, after, expected) {
			return nil // Leave the prior proof stale; it cannot attest to this write.
		}
		if _, err := scope.conn.ExecContext(ctx, "INSERT INTO "+verifiedAgentProofTable+" (agent_id) VALUES (?1)", id.String()); err != nil {
			return err
		}
		digest := verifiedAgentSetDigest(append(proofIDs, id.String()))
		if _, err := scope.conn.ExecContext(ctx, "UPDATE "+verifiedAgentMetaTable+" SET agent_digest = ?1 WHERE id = 1", digest); err != nil {
			return err
		}
		return scope.advanceVerifiedLedger()
	})
	if err != nil {
		return agent, fmt.Errorf("register admitted ML agent: transaction: %w", err)
	}
	return ptypes.MLAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindMachineLearning}, Role: role,
		Model: ptypes.MLModel{ID: modelID, Provider: provider, Name: model}}, nil
}

func exactMLIdentity(value string) bool {
	return value != "" && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}

func (scope *connScope) rejectDuplicateMLAgent(namespace string, role ptypes.Role, modelID int) error {
	// The canonical UUID is a fixed-width suffix. Exact substr comparison
	// avoids LIKE wildcard interpretation in namespaces.
	var text string
	err := scope.conn.QueryRowContext(scope.ctx, `SELECT agent_id FROM agents_ml
		WHERE role_id = ?1 AND model_id = ?2 AND substr(agent_id, 1, length(agent_id)-37) = ?3
		ORDER BY agent_id LIMIT 1`, int(role), modelID, namespace).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check duplicate ML agent: %w", err)
	}
	existing, err := ptypes.ParseActorID(text)
	if err != nil {
		return fmt.Errorf("read existing ML agent identity: %w", err)
	}
	return &DuplicateMLAgentError{ExistingID: existing}
}

func (scope *connScope) admittedMLLedgerBaseline() (verifiedStoreIdentity, []string, bool) {
	var zero verifiedStoreIdentity
	if !scope.admittedMLDefinitionsCurrent() || !scope.exactReplayFactContextsVerified() {
		return zero, nil, false
	}
	ledger, ok := scope.readVerifiedLedger()
	if !ok {
		return zero, nil, false
	}
	proof, err := scope.verifiedAgentIDs()
	if err != nil {
		return zero, nil, false
	}
	digest, ok, err := scope.verifiedAgentDigest()
	if err != nil || !ok || digest != verifiedAgentSetDigest(proof) {
		return zero, nil, false
	}
	current, err := scope.currentAgentIDs()
	if err != nil || !slices.Equal(current, proof) {
		return zero, nil, false
	}
	return ledger.identity, proof, true
}

// Exact generation triggers alone do not rule out additional triggers that
// mutate the proof tables. Do not attest writes with an unknown side effect.
func (scope *connScope) admittedMLDefinitionsCurrent() bool {
	tables := map[string]string{
		"providers": providersDDL, "ml_models": mlModelsDDL, "agents": agentsDDL, "agents_ml": agentsMLDDL,
	}
	allowed := make(map[string]string)
	for table, ddl := range tables {
		allowed[table] = admittedMLDDLTokens(strings.Replace(ddl, "IF NOT EXISTS ", "", 1))
		for _, event := range verifiedTriggerEvents {
			allowed[verifiedTriggerName(table, event)] = verifiedTriggerDDL(table, event)
		}
	}
	current := true
	err := scope.queryRows(`SELECT name, sql FROM main.sqlite_schema
		WHERE (type = 'table' AND name IN ('providers', 'ml_models', 'agents', 'agents_ml'))
		OR (type = 'trigger' AND tbl_name IN ('providers', 'ml_models', 'agents', 'agents_ml',
			'provenance_table_generation', 'provenance_verified_table', 'provenance_verified_store',
			'provenance_verified_agent', 'provenance_verified_agent_meta'))
		UNION ALL SELECT name, sql FROM temp.sqlite_schema`, nil, func(rows *sql.Rows) error {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			return err
		}
		definition := ddl.String
		if _, table := tables[name]; table {
			definition = admittedMLDDLTokens(definition)
		}
		if !ddl.Valid || allowed[name] != definition {
			current = false
		}
		delete(allowed, name)
		return nil
	})
	return err == nil && current && len(allowed) == 0
}

var admittedMLDDLPunctuation = strings.NewReplacer("(", " ( ", ")", " ) ", ",", " , ")

// The expected table definitions have no quoted tokens. Permit formatting
// differences from a host migration without joining distinct SQL tokens.
func admittedMLDDLTokens(ddl string) string {
	return strings.Join(strings.Fields(admittedMLDDLPunctuation.Replace(ddl)), " ")
}

func admittedMLDeltaMatches(before, after verifiedStoreIdentity, expected map[int]int64) bool {
	if !journalIdentityEqual(before, after) || before.tablesDigest != after.tablesDigest ||
		len(before.generations) != len(after.generations) {
		return false
	}
	var total int64
	for i, generation := range before.generations {
		if after.generations[i]-generation != expected[i] {
			return false
		}
		total += expected[i]
	}
	return after.generation-before.generation == total
}
