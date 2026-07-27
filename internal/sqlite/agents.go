package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

const insertAgentSQL = "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)"

// RegisterHumanAgent registers a new human agent with a UUIDv7 ID.
func (db *DB) RegisterHumanAgent(namespace, name, contact string) (ptypes.HumanAgent, error) {
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf("sqlite.RegisterHumanAgent: lease connection: %w", err)
	}
	defer scope.release()

	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, insertAgentSQL, id.String(), int(ptypes.AgentKindHuman)); err != nil {
			return fmt.Errorf("failed to insert agent row: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_human (agent_id, name, contact) VALUES (?1, ?2, ?3)", id.String(), name, contact); err != nil {
			return fmt.Errorf("failed to insert human row: %w", err)
		}
		return nil
	}); err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf("sqlite.RegisterHumanAgent: transactional registration failed: %w", err)
	}
	return ptypes.HumanAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindHuman}, Name: name, Contact: contact}, nil
}

// RegisterMLAgent registers a new ML agent. The (provider, modelName) pair must
// exist in the ml_models seed table; returns ptypes.ErrNotFound if unknown.
func (db *DB) RegisterMLAgent(namespace string, role ptypes.Role, provider ptypes.Provider, modelName ptypes.ModelID) (ptypes.MLAgent, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.MLAgent{}, fmt.Errorf("sqlite.RegisterMLAgent: lease connection: %w", err)
	}
	defer scope.release()

	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	var modelID int
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		err := scope.conn.QueryRowContext(scope.ctx, "SELECT id FROM ml_models WHERE provider_id = (SELECT id FROM providers WHERE name = ?1) AND name = ?2", string(provider), string(modelName)).Scan(&modelID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: RegisterMLAgent — model (%s, %q) not found in ml_models — use a known (provider, name) combination seeded at database creation time", ptypes.ErrNotFound, provider.String(), modelName)
		}
		if err != nil {
			return fmt.Errorf("model lookup (%s, %q) failed: %w", provider.String(), modelName, err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, insertAgentSQL, id.String(), int(ptypes.AgentKindMachineLearning)); err != nil {
			return fmt.Errorf("failed to insert base agent row: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_ml (agent_id, role_id, model_id) VALUES (?1, ?2, ?3)", id.String(), int(role), modelID); err != nil {
			return fmt.Errorf("failed to insert ml agent row: %w", err)
		}
		return nil
	}); err != nil {
		return ptypes.MLAgent{}, fmt.Errorf("sqlite.RegisterMLAgent: transactional registration failed: %w", err)
	}
	return ptypes.MLAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindMachineLearning}, Role: role, Model: ptypes.MLModel{ID: modelID, Provider: provider, Name: modelName}}, nil
}

// RegisterSoftwareAgent registers a new software agent with a UUIDv7 ID.
func (db *DB) RegisterSoftwareAgent(namespace, name, version, source string) (ptypes.SoftwareAgent, error) {
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("sqlite.RegisterSoftwareAgent: lease connection: %w", err)
	}
	defer scope.release()

	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, insertAgentSQL, id.String(), int(ptypes.AgentKindSoftware)); err != nil {
			return fmt.Errorf("failed to insert base agent row: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)", id.String(), name, version, source); err != nil {
			return fmt.Errorf("failed to insert software agent row: %w", err)
		}
		return nil
	}); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("sqlite.RegisterSoftwareAgent: transactional registration failed: %w", err)
	}
	return ptypes.SoftwareAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware}, Name: name, Version: version, Source: source}, nil
}

// GetAgent returns the base agent (kind only) by ID.
// Returns ptypes.ErrNotFound if the agent does not exist.
func (db *DB) GetAgent(id ptypes.AgentID) (ptypes.Agent, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Agent{}, fmt.Errorf("sqlite.GetAgent: lease connection: %w", err)
	}
	defer scope.release()
	var kind int
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT kind_id FROM agents WHERE id = ?1", id.String()).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ptypes.Agent{}, fmt.Errorf("%w: Agent — agent %q does not exist — use RegisterHumanAgent, RegisterMLAgent, or RegisterSoftwareAgent to create agents", ptypes.ErrNotFound, id.String())
	}
	if err != nil {
		return ptypes.Agent{}, fmt.Errorf("sqlite.GetAgent: %w", err)
	}
	return ptypes.Agent{ID: id, Kind: ptypes.AgentKind(kind)}, nil
}

// GetHumanAgent returns the human agent by ID.
// Returns ptypes.ErrNotFound if not found or if the agent is a different kind.
func (db *DB) GetHumanAgent(id ptypes.AgentID) (ptypes.HumanAgent, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf("sqlite.GetHumanAgent: lease connection: %w", err)
	}
	defer scope.release()
	var name, contact string
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT h.name, h.contact FROM agents a JOIN agents_human h ON a.id = h.agent_id WHERE a.id = ?1", id.String()).Scan(&name, &contact)
	if errors.Is(err, sql.ErrNoRows) {
		return ptypes.HumanAgent{}, fmt.Errorf("%w: HumanAgent — agent %q not found or is not a human agent — call Agent() first to inspect the Kind field", ptypes.ErrNotFound, id.String())
	}
	if err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf("sqlite.GetHumanAgent: %w", err)
	}
	return ptypes.HumanAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindHuman}, Name: name, Contact: contact}, nil
}

// GetMLAgent returns the ML agent by ID.
// Returns ptypes.ErrNotFound if not found or if the agent is a different kind.
func (db *DB) GetMLAgent(id ptypes.AgentID) (ptypes.MLAgent, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.MLAgent{}, fmt.Errorf("sqlite.GetMLAgent: lease connection: %w", err)
	}
	defer scope.release()
	var role, modelID int
	var provider, modelName string
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT m.role_id, ml.id, p.name, ml.name FROM agents a JOIN agents_ml m ON a.id = m.agent_id JOIN ml_models ml ON m.model_id = ml.id JOIN providers p ON ml.provider_id = p.id WHERE a.id = ?1", id.String()).Scan(&role, &modelID, &provider, &modelName)
	if errors.Is(err, sql.ErrNoRows) {
		return ptypes.MLAgent{}, fmt.Errorf("%w: MLAgent — agent %q not found or is not an ML agent — call Agent() first to inspect the Kind field", ptypes.ErrNotFound, id.String())
	}
	if err != nil {
		return ptypes.MLAgent{}, fmt.Errorf("sqlite.GetMLAgent: %w", err)
	}
	return ptypes.MLAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindMachineLearning}, Role: ptypes.Role(role), Model: ptypes.MLModel{ID: modelID, Provider: ptypes.Provider(provider), Name: ptypes.ModelID(modelName)}}, nil
}

// GetSoftwareAgent returns the software agent by ID.
// Returns ptypes.ErrNotFound if not found or if the agent is a different kind.
func (db *DB) GetSoftwareAgent(id ptypes.AgentID) (ptypes.SoftwareAgent, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("sqlite.GetSoftwareAgent: lease connection: %w", err)
	}
	defer scope.release()
	var name, version, source string
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT s.name, s.version, s.source FROM agents a JOIN agents_software s ON a.id = s.agent_id WHERE a.id = ?1", id.String()).Scan(&name, &version, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return ptypes.SoftwareAgent{}, fmt.Errorf("%w: SoftwareAgent — agent %q not found or is not a software agent — call Agent() first to inspect the Kind field", ptypes.ErrNotFound, id.String())
	}
	if err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("sqlite.GetSoftwareAgent: %w", err)
	}
	return ptypes.SoftwareAgent{Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware}, Name: name, Version: version, Source: source}, nil
}
