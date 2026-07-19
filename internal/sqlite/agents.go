package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
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

	if err := sqlitex.Execute(db.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 0)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return ptypes.HumanAgent{}, fmt.Errorf(
			"sqlite.RegisterHumanAgent: failed to insert agent row: %w", err,
		)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO agents_human (agent_id, name, contact) VALUES (?1, ?2, ?3)`,
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
	if err := sqlitex.Execute(db.conn,
		`SELECT id FROM ml_models WHERE provider_id = (SELECT id FROM providers WHERE name = ?1) AND name = ?2`,
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
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 1)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return ptypes.MLAgent{}, fmt.Errorf(
			"sqlite.RegisterMLAgent: failed to insert base agent row: %w", err,
		)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO agents_ml (agent_id, role_id, model_id) VALUES (?1, ?2, ?3)`,
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

	if err := sqlitex.Execute(db.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 2)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"sqlite.RegisterSoftwareAgent: failed to insert base agent row: %w", err,
		)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)`,
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

// RegisterSoftwareAgentWithID registers a new software agent using a
// CALLER-SUPPLIED AgentID, instead of the random UUIDv7 RegisterSoftwareAgent
// mints. This is the fixed-ID software-agent registration seam of
// docs/journal-relational-contract.md §7: it exists so a caller can create the
// agents/agents_software row a fixed_actor_manifest_entries row's ActorID FK
// (§7.2) must reference — RegisterFixedActorEntry alone cannot satisfy that FK,
// since no other released path creates an agents row with a caller-chosen id.
//
// A dedicated method (rather than a WithAgentID functional option on
// RegisterSoftwareAgent) was chosen to keep the common random-UUIDv7 path's
// signature and behavior completely unchanged, and because the fixed-ID path
// has different failure semantics that would be surprising bolted onto the
// existing call: it requires a pre-existing actor_namespace_claims row and
// rejects out-of-range ids, neither of which the random-ID path has any
// reason to check. This mirrors the codebase's existing WithID-suffix idiom
// (StartActivityWithID) for "same verb, caller-supplied identity" — but the
// conflict behavior is deliberately the opposite: StartActivityWithID
// collapses a replay to an idempotent no-op, while a second call here with the
// same id is a typed conflict (ptypes.ErrAgentAlreadyExists), since an agent
// identity must be unique, never silently reused.
//
// Namespace-claim consistency (§7.1-7.2, §7.3 rule 2): id.Namespace MUST have
// a registered actor_namespace_claims row, and id.UUID MUST decode, under that
// claim's Codec, to an ordinal inside [RangeMin, RangeMax]. Fixed IDs OUTSIDE
// every claim are deliberately NOT permitted — the only released caller of a
// fixed ID is the fixed_actor_manifest_entries seam, and every manifest entry
// must already satisfy entry-in-range (§7.3 rule 2), so requiring the same
// containment at agent-registration time keeps the two tables consistent by
// construction: a fixed-ID agent can never be registered unless a manifest
// entry could legally reference it, and never left in a state that could not
// have come from a legitimate claimed range.
//
// Returns:
//   - ptypes.ErrInvalidID if id.Namespace is empty (malformed shape).
//   - journal.ErrNamespaceClaim if id.Namespace has no registered claim.
//   - journal.ErrEntryOutOfRange if id.UUID does not decode inside the claim's
//     range under its codec.
//   - ptypes.ErrAgentAlreadyExists if an agent with id already exists.
//
// Acquires the DB mutex.
func (db *DB) RegisterSoftwareAgentWithID(id ptypes.AgentID, name, version, source string) (ptypes.SoftwareAgent, error) {
	if id.Namespace == "" {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"%w: RegisterSoftwareAgentWithID — id %q has an empty namespace — "+
				"where: sqlite.RegisterSoftwareAgentWithID, fixed-ID validation; "+
				"when: before any row is written; impact: the agent cannot be "+
				"registered, since the wire format requires a non-empty namespace; "+
				"fix: supply an id of the form ptypes.AgentID{Namespace: \"pasture-system\", "+
				"UUID: ...} with a non-empty Namespace",
			ptypes.ErrInvalidID, id.String(),
		)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	claim, found, err := db.getNamespaceClaimLocked(id.Namespace)
	if err != nil {
		return ptypes.SoftwareAgent{}, err
	}
	if !found {
		return ptypes.SoftwareAgent{}, fmt.Errorf(
			"%w: RegisterSoftwareAgentWithID — namespace %q has no registered "+
				"actor_namespace_claims row — where: sqlite.RegisterSoftwareAgentWithID, "+
				"namespace-claim lookup; when: before the agent insert; impact: a "+
				"fixed-ID agent outside every claim is rejected, since no "+
				"fixed_actor_manifest_entries row could ever legally reference it "+
				"(§7.3 rule 2); fix: call RegisterNamespaceClaim for %q first, or use "+
				"RegisterSoftwareAgent instead if this agent does not need a fixed ID",
			journal.ErrNamespaceClaim, id.Namespace, id.Namespace,
		)
	}
	if err := journal.CheckEntryInRange(claim, id.Namespace, [16]byte(id.UUID)); err != nil {
		return ptypes.SoftwareAgent{}, fmt.Errorf("sqlite.RegisterSoftwareAgentWithID: %w", err)
	}

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 2)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); txErr != nil {
		if isUniqueViolation(txErr) {
			txErr = fmt.Errorf(
				"%w: RegisterSoftwareAgentWithID — agent %q is already registered — "+
					"where: sqlite.RegisterSoftwareAgentWithID, agents insert; when: "+
					"agents primary-key conflict; impact: the duplicate registration "+
					"is rejected rather than silently reusing or overwriting the "+
					"existing agent identity; fix: choose a distinct fixed id, or call "+
					"Agent/SoftwareAgent to fetch the existing row instead of "+
					"re-registering it",
				ptypes.ErrAgentAlreadyExists, id.String(),
			)
			return ptypes.SoftwareAgent{}, txErr
		}
		txErr = fmt.Errorf(
			"sqlite.RegisterSoftwareAgentWithID: failed to insert base agent row: %w", txErr,
		)
		return ptypes.SoftwareAgent{}, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{id.String(), name, version, source}}); txErr != nil {
		txErr = fmt.Errorf(
			"sqlite.RegisterSoftwareAgentWithID: failed to insert software agent row: %w", txErr,
		)
		return ptypes.SoftwareAgent{}, txErr
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
	err := sqlitex.Execute(db.conn,
		`SELECT id, kind_id FROM agents WHERE id = ?1`,
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
	err := sqlitex.Execute(db.conn,
		`SELECT a.kind_id, h.name, h.contact
		 FROM agents a JOIN agents_human h ON a.id = h.agent_id
		 WHERE a.id = ?1`,
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
	err := sqlitex.Execute(db.conn,
		`SELECT a.kind_id, m.role_id, ml.id, p.name, ml.name
		 FROM agents a
		 JOIN agents_ml m ON a.id = m.agent_id
		 JOIN ml_models ml ON m.model_id = ml.id
		 JOIN providers p ON ml.provider_id = p.id
		 WHERE a.id = ?1`,
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
	err := sqlitex.Execute(db.conn,
		`SELECT a.kind_id, s.name, s.version, s.source
		 FROM agents a JOIN agents_software s ON a.id = s.agent_id
		 WHERE a.id = ?1`,
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
