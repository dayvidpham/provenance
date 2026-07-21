package provenance

// tracker.go contains the sqliteTracker implementation of the Tracker interface.
//
// Architecture: Types live in pkg/ptypes, which imports bestiary for
// Provider.IsValid() catalog validation. No cyclic import exists because bestiary
// imports neither provenance nor pkg/ptypes. SQL persistence lives in
// internal/sqlite. The graph store adapter lives in internal/graph. Graph traversal
// helpers live in internal/helpers. This root package imports all of them and wires
// them together.

import (
	"fmt"

	intgraph "github.com/dayvidpham/provenance/internal/graph"
	"github.com/dayvidpham/provenance/internal/helpers"
	dbsqlite "github.com/dayvidpham/provenance/internal/sqlite"
	dgraph "github.com/dominikbraun/graph"
)

// ---------------------------------------------------------------------------
// sqliteTracker — implements Tracker
// ---------------------------------------------------------------------------

// sqliteTracker is the canonical implementation of Tracker.
// It delegates SQL to internal/sqlite, graph operations to internal/graph,
// and traversal to internal/helpers.
type sqliteTracker struct {
	db       *dbsqlite.DB
	graph    dgraph.Graph[string, Task]
	registry ModelRegistry
}

// openTracker opens (or creates) a SQLite database at dbPath and returns
// an initialised Tracker. Pass ":memory:" for an in-memory database.
func openTracker(dbPath string, opts ...Option) (Tracker, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	db, err := dbsqlite.Open(dbPath, o.registry.Models())
	if err != nil {
		return nil, fmt.Errorf("provenance.openTracker: %w", err)
	}

	return &sqliteTracker{
		db:       db,
		graph:    intgraph.NewGraph(db),
		registry: o.registry,
	}, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (t *sqliteTracker) Close() error {
	if err := t.db.Close(); err != nil {
		return fmt.Errorf("provenance.Tracker.Close: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task reads
// ---------------------------------------------------------------------------
//
// Task mutations (create/update/close) are journaled and live on the Session SDK
// (Tracker.As → Session.Create/Update/CloseTask), which commits them through
// Apply so every lifecycle change is authorized and reproducible from journal
// history (§8.1, §9). There is no direct-write task-mutation path on the Tracker.

func (t *sqliteTracker) Show(id TaskID) (Task, error) {
	task, found, err := t.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Tracker.Show: %w", err)
	}
	if !found {
		return Task{}, fmt.Errorf(
			"%w: Show — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			ErrNotFound, id.String(),
		)
	}
	return task, nil
}

func (t *sqliteTracker) List(filter ListFilter) ([]Task, error) {
	tasks, err := t.db.ListTasks(filter)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.List: %w", err)
	}
	return tasks, nil
}

// ---------------------------------------------------------------------------
// Typed Dependency Edges (journaled §6 relationship writes)
// ---------------------------------------------------------------------------
//
// Edge MUTATIONS (AddEdge/RemoveEdge) are journaled and live on the Session SDK
// (Tracker.As → Session.AddEdge/RemoveEdge): each commits one typed edge mutation-family
// effect through Apply, and the shared reducer folds it into the edges projection the
// graph store reads (§6). Cycle detection for blocked_by edges is
// enforced in the reducer fold. There is no direct-write edge-mutation path on the
// Tracker; the Edges read stays on the Tracker interface, backed by the same projection.

func (t *sqliteTracker) Edges(id TaskID, kind *EdgeKind) ([]Edge, error) {
	edges, err := t.db.GetEdges(id, kind)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Edges: %w", err)
	}
	return edges, nil
}

// ---------------------------------------------------------------------------
// Readiness Queries
// ---------------------------------------------------------------------------

func (t *sqliteTracker) Blocked() ([]Task, error) {
	tasks, err := t.db.BlockedTasks()
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Blocked: %w", err)
	}
	return tasks, nil
}

func (t *sqliteTracker) Ready() ([]Task, error) {
	tasks, err := t.db.ReadyTasks()
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Ready: %w", err)
	}
	return tasks, nil
}

func (t *sqliteTracker) DepTree(id TaskID) ([]Edge, error) {
	edges, err := t.db.GetDepTree(id)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.DepTree: %w", err)
	}
	return edges, nil
}

func (t *sqliteTracker) Ancestors(id TaskID) ([]Task, error) {
	return helpers.Ancestors(t.graph, t.db, id)
}

func (t *sqliteTracker) Descendants(id TaskID) ([]Task, error) {
	return helpers.Descendants(t.graph, t.db, id)
}

// ---------------------------------------------------------------------------
// Labels (journaled §6 annotation writes)
// ---------------------------------------------------------------------------
//
// Label MUTATIONS (AddLabel/RemoveLabel) are journaled on the Session SDK (Tracker.As);
// each commits one label mutation-family effect and the shared reducer folds it into the
// labels projection (§6). Labels reads stay on the Tracker interface.

func (t *sqliteTracker) Labels(id TaskID) ([]string, error) {
	labels, err := t.db.GetLabels(id)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Labels: %w", err)
	}
	return labels, nil
}

// ---------------------------------------------------------------------------
// Comments (journaled §6 annotation write)
// ---------------------------------------------------------------------------
//
// Comment MUTATION (AddComment) is journaled on the Session SDK (Tracker.As); it commits
// one comment mutation-family effect and the shared reducer folds it into the comments
// projection (§6). Comments reads stay on the Tracker interface.

func (t *sqliteTracker) Comments(id TaskID) ([]Comment, error) {
	comments, err := t.db.GetComments(id)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Comments: %w", err)
	}
	return comments, nil
}

// ---------------------------------------------------------------------------
// PROV-O Agents
// ---------------------------------------------------------------------------

func (t *sqliteTracker) RegisterHumanAgent(namespace, name, contact string) (HumanAgent, error) {
	ha, err := t.db.RegisterHumanAgent(namespace, name, contact)
	if err != nil {
		return HumanAgent{}, fmt.Errorf("provenance.Tracker.RegisterHumanAgent: %w", err)
	}
	return ha, nil
}

func (t *sqliteTracker) RegisterMLAgent(namespace string, role Role, provider Provider, modelName ModelID) (MLAgent, error) {
	if _, ok := t.registry.Lookup(provider, string(modelName)); !ok {
		return MLAgent{}, fmt.Errorf(
			"%w: RegisterMLAgent — model (%s, %q) not found in registry — "+
				"use a known (provider, name) combination from the model registry",
			ErrNotFound, provider.String(), modelName,
		)
	}
	mla, err := t.db.RegisterMLAgent(namespace, role, provider, modelName)
	if err != nil {
		return MLAgent{}, fmt.Errorf("provenance.Tracker.RegisterMLAgent: %w", err)
	}
	return mla, nil
}

func (t *sqliteTracker) RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error) {
	sa, err := t.db.RegisterSoftwareAgent(namespace, name, version, source)
	if err != nil {
		return SoftwareAgent{}, fmt.Errorf("provenance.Tracker.RegisterSoftwareAgent: %w", err)
	}
	return sa, nil
}

func (t *sqliteTracker) RegisterFixedSoftwareAgent(reg FixedSoftwareAgentRegistration) (SoftwareAgent, error) {
	sa, err := t.db.RegisterFixedSoftwareAgent(reg)
	if err != nil {
		return SoftwareAgent{}, fmt.Errorf("provenance.Tracker.RegisterFixedSoftwareAgent: %w", err)
	}
	return sa, nil
}

func (t *sqliteTracker) Agent(id AgentID) (Agent, error) {
	agent, err := t.db.GetAgent(id)
	if err != nil {
		return Agent{}, fmt.Errorf("provenance.Tracker.Agent: %w", err)
	}
	return agent, nil
}

func (t *sqliteTracker) HumanAgent(id AgentID) (HumanAgent, error) {
	ha, err := t.db.GetHumanAgent(id)
	if err != nil {
		return HumanAgent{}, fmt.Errorf("provenance.Tracker.HumanAgent: %w", err)
	}
	return ha, nil
}

func (t *sqliteTracker) MLAgent(id AgentID) (MLAgent, error) {
	mla, err := t.db.GetMLAgent(id)
	if err != nil {
		return MLAgent{}, fmt.Errorf("provenance.Tracker.MLAgent: %w", err)
	}
	return mla, nil
}

func (t *sqliteTracker) SoftwareAgent(id AgentID) (SoftwareAgent, error) {
	sa, err := t.db.GetSoftwareAgent(id)
	if err != nil {
		return SoftwareAgent{}, fmt.Errorf("provenance.Tracker.SoftwareAgent: %w", err)
	}
	return sa, nil
}

// ---------------------------------------------------------------------------
// PROV-O Activities
// ---------------------------------------------------------------------------

func (t *sqliteTracker) StartActivity(agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error) {
	act, err := t.db.StartActivity(agentID, phase, stage, notes)
	if err != nil {
		return Activity{}, fmt.Errorf("provenance.Tracker.StartActivity: %w", err)
	}
	return act, nil
}

func (t *sqliteTracker) StartActivityWithID(id ActivityID, agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error) {
	act, err := t.db.StartActivityWithID(id, agentID, phase, stage, notes)
	if err != nil {
		return Activity{}, fmt.Errorf("provenance.Tracker.StartActivityWithID: %w", err)
	}
	return act, nil
}

func (t *sqliteTracker) EndActivity(id ActivityID) (Activity, error) {
	act, err := t.db.EndActivity(id)
	if err != nil {
		return Activity{}, fmt.Errorf("provenance.Tracker.EndActivity: %w", err)
	}
	return act, nil
}

func (t *sqliteTracker) Activities(agentID *AgentID) ([]Activity, error) {
	activities, err := t.db.GetActivities(agentID)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Activities: %w", err)
	}
	return activities, nil
}
