package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// RegisterHumanAgent registers a new human agent with a UUIDv7 ID.
// Acquires the DB mutex.
func (db *DB) RegisterHumanAgent(namespace, name, contact string) (ptypes.HumanAgent, error) {
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := executeStatement(db.conn,
		sqlStatement019,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf(
			"sqlite.RegisterHumanAgent: failed to insert agent row: %w", err,
		)
	}
	if err := executeStatement(db.conn,
		sqlStatement020,
		&sqlitex.ExecOptions{Args: []any{id.String(), name, contact}}); err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf(
			"sqlite.RegisterHumanAgent: failed to insert human row: %w", err,
		)
	}
	return ptypes.HumanAgent{
		Agent:   ptypes.Agent{ID: id, Kind: ptypes.AgentKindHuman},
		Name:    name,
		Contact: contact,
	}, nil
}

// RegisterMLAgent registers a new ML agent. The (provider, modelName) pair must
// exist in the ml_models seed table; returns ptypes.ErrNotFound if unknown.
// Acquires the DB mutex.
func (db *DB) RegisterMLAgent(namespace string, role ptypes.Role, provider ptypes.Provider, modelName ptypes.ModelID) (ptypes.MLAgent, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var modelID int
	var modelFound bool
	if err := executeStatement(db.conn,
		sqlStatement021,
		&sqlitex.ExecOptions{
			Args: []any{string(provider), string(modelName)},
			ResultFunc: func(stmt *zs.Stmt) error {
				modelID = stmt.ColumnInt(0)
				modelFound = true
				return nil
			},
		}); err != nil {
		return ptypes.MLAgent{}, fmt.Errorf(
			"sqlite.RegisterMLAgent: model lookup (%s, %q) failed: %w",
			provider.String(), modelName, err,
		)
	}
	if !modelFound {
		return ptypes.MLAgent{}, fmt.Errorf(
			"%w: RegisterMLAgent — model (%s, %q) not found in ml_models — "+
				"use a known (provider, name) combination seeded at database creation time",
			ptypes.ErrNotFound, provider.String(), modelName,
		)
	}

	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	if err := executeStatement(db.conn,
		sqlStatement022,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return ptypes.MLAgent{}, fmt.Errorf(
			"sqlite.RegisterMLAgent: failed to insert base agent row: %w", err,
		)
	}
	if err := executeStatement(db.conn,
		sqlStatement023,
		&sqlitex.ExecOptions{Args: []any{id.String(), int(role), modelID}}); err != nil {
		return ptypes.MLAgent{}, fmt.Errorf(
			"sqlite.RegisterMLAgent: failed to insert ml agent row: %w", err,
		)
	}
	return ptypes.MLAgent{
		Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindMachineLearning},
		Role:  role,
		Model: ptypes.MLModel{ID: modelID, Provider: provider, Name: modelName},
	}, nil
}

// RegisterSoftwareAgent registers a new software agent with a UUIDv7 ID.
// Acquires the DB mutex.
func (db *DB) RegisterSoftwareAgent(namespace, name, version, source string) (ptypes.SoftwareAgent, error) {
	id := ptypes.AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := executeStatement(db.conn,
		sqlStatement024,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"sqlite.RegisterSoftwareAgent: failed to insert base agent row: %w", err,
		)
	}
	if err := executeStatement(db.conn,
		sqlStatement011,
		&sqlitex.ExecOptions{Args: []any{id.String(), name, version, source}}); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"sqlite.RegisterSoftwareAgent: failed to insert software agent row: %w", err,
		)
	}
	return ptypes.SoftwareAgent{
		Agent:   ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware},
		Name:    name,
		Version: version,
		Source:  source,
	}, nil
}

// GetAgent returns the base agent (kind only) by ID.
// Returns ptypes.ErrNotFound if the agent does not exist. Acquires the DB mutex.
func (db *DB) GetAgent(id ptypes.AgentID) (ptypes.Agent, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var agent ptypes.Agent
	var found bool
	err := executeStatement(db.conn,
		sqlStatement025,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				agent = ptypes.Agent{ID: id, Kind: ptypes.AgentKind(stmt.ColumnInt(1))}
				found = true
				return nil
			},
		})
	if err != nil {
		return ptypes.Agent{}, fmt.Errorf("sqlite.GetAgent: %w", err)
	}
	if !found {
		return ptypes.Agent{}, fmt.Errorf(
			"%w: Agent — agent %q does not exist — "+
				"use RegisterHumanAgent, RegisterMLAgent, or RegisterSoftwareAgent to create agents",
			ptypes.ErrNotFound, id.String(),
		)
	}
	return agent, nil
}

// GetHumanAgent returns the human agent by ID.
// Returns ptypes.ErrNotFound if not found or if the agent is a different kind.
// Acquires the DB mutex.
func (db *DB) GetHumanAgent(id ptypes.AgentID) (ptypes.HumanAgent, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var ha ptypes.HumanAgent
	var found bool
	err := executeStatement(db.conn,
		sqlStatement026,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				ha = ptypes.HumanAgent{
					Agent:   ptypes.Agent{ID: id, Kind: ptypes.AgentKindHuman},
					Name:    stmt.ColumnText(1),
					Contact: stmt.ColumnText(2),
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf("sqlite.GetHumanAgent: %w", err)
	}
	if !found {
		return ptypes.HumanAgent{}, fmt.Errorf(
			"%w: HumanAgent — agent %q not found or is not a human agent — "+
				"call Agent() first to inspect the Kind field",
			ptypes.ErrNotFound, id.String(),
		)
	}
	return ha, nil
}

// GetMLAgent returns the ML agent by ID.
// Returns ptypes.ErrNotFound if not found or if the agent is a different kind.
// Acquires the DB mutex.
func (db *DB) GetMLAgent(id ptypes.AgentID) (ptypes.MLAgent, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var mla ptypes.MLAgent
	var found bool
	err := executeStatement(db.conn,
		sqlStatement027,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				mla = ptypes.MLAgent{
					Agent: ptypes.Agent{ID: id, Kind: ptypes.AgentKindMachineLearning},
					Role:  ptypes.Role(stmt.ColumnInt(1)),
					Model: ptypes.MLModel{
						ID:       stmt.ColumnInt(2),
						Provider: ptypes.Provider(stmt.ColumnText(3)),
						Name:     ptypes.ModelID(stmt.ColumnText(4)),
					},
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return ptypes.MLAgent{}, fmt.Errorf("sqlite.GetMLAgent: %w", err)
	}
	if !found {
		return ptypes.MLAgent{}, fmt.Errorf(
			"%w: MLAgent — agent %q not found or is not an ML agent — "+
				"call Agent() first to inspect the Kind field",
			ptypes.ErrNotFound, id.String(),
		)
	}
	return mla, nil
}

// GetSoftwareAgent returns the software agent by ID.
// Returns ptypes.ErrNotFound if not found or if the agent is a different kind.
// Acquires the DB mutex.
func (db *DB) GetSoftwareAgent(id ptypes.AgentID) (ptypes.SoftwareAgent, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var sa ptypes.SoftwareAgent
	var found bool
	err := executeStatement(db.conn,
		sqlStatement028,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				sa = ptypes.SoftwareAgent{
					Agent:   ptypes.Agent{ID: id, Kind: ptypes.AgentKindSoftware},
					Name:    stmt.ColumnText(1),
					Version: stmt.ColumnText(2),
					Source:  stmt.ColumnText(3),
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("sqlite.GetSoftwareAgent: %w", err)
	}
	if !found {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"%w: SoftwareAgent — agent %q not found or is not a software agent — "+
				"call Agent() first to inspect the Kind field",
			ptypes.ErrNotFound, id.String(),
		)
	}
	return sa, nil
}
