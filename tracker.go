package provenance

// tracker.go contains the sqliteTracker implementation of the Tracker interface.
//
// Architecture: Types live in pkg/ptypes (no dependencies). The SQL persistence
// layer lives in internal/sqlite. The graph store adapter lives in
// internal/graph. Graph traversal helpers live in internal/helpers.
// This root package imports all of them and wires them together.

import (
	"fmt"
	"time"

	intgraph "github.com/dayvidpham/provenance/internal/graph"
	"github.com/dayvidpham/provenance/internal/helpers"
	dbsqlite "github.com/dayvidpham/provenance/internal/sqlite"
	dgraph "github.com/dominikbraun/graph"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// sqliteTracker — implements Tracker
// ---------------------------------------------------------------------------

// sqliteTracker is the canonical implementation of Tracker.
// It delegates SQL to internal/sqlite, graph operations to internal/graph,
// and traversal to internal/helpers.
type sqliteTracker struct {
	db    *dbsqlite.DB
	graph dgraph.Graph[string, Task]
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
		db:    db,
		graph: intgraph.NewGraph(db),
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
// Task CRUD
// ---------------------------------------------------------------------------

func (t *sqliteTracker) Create(namespace, title, description string, taskType TaskType, priority Priority, phase Phase) (Task, error) {
	if namespace == "" {
		return Task{}, fmt.Errorf(
			"%w: Create — namespace is empty — "+
				"provide a non-empty namespace string such as 'aura-plugins' or 'my-project'",
			ErrInvalidID,
		)
	}

	now := time.Now().UTC()
	task := Task{
		ID:          TaskID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())},
		Title:       title,
		Description: description,
		Status:      StatusOpen,
		Priority:    priority,
		Type:        taskType,
		Phase:       phase,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := t.graph.AddVertex(task); err != nil {
		return Task{}, fmt.Errorf(
			"provenance.Tracker.Create: failed to insert task %q: %w — "+
				"check that the database is writable and the namespace is valid",
			task.ID.String(), err,
		)
	}
	return task, nil
}

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

func (t *sqliteTracker) Update(id TaskID, fields UpdateFields) (Task, error) {
	task, err := t.db.UpdateTask(id, fields, time.Now().UTC())
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Tracker.Update: %w", err)
	}
	return task, nil
}

func (t *sqliteTracker) CloseTask(id TaskID, reason string) (Task, error) {
	current, found, err := t.db.GetTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Tracker.CloseTask: failed to fetch task %q: %w", id.String(), err)
	}
	if !found {
		return Task{}, fmt.Errorf(
			"%w: CloseTask — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			ErrNotFound, id.String(),
		)
	}
	if current.Status == StatusClosed {
		return Task{}, fmt.Errorf(
			"%w: CloseTask — task %q is already closed (reason: %q) — "+
				"use Update to reopen the task before closing again",
			ErrAlreadyClosed, id.String(), current.CloseReason,
		)
	}

	task, err := t.db.CloseTask(id, reason, time.Now().UTC())
	if err != nil {
		return Task{}, fmt.Errorf("provenance.Tracker.CloseTask: %w", err)
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
// Typed Dependency Edges
// ---------------------------------------------------------------------------

func (t *sqliteTracker) AddEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	if kind == EdgeBlockedBy {
		if err := t.graph.AddEdge(sourceID.String(), targetID); err != nil {
			if dgraph.ErrEdgeCreatesCycle == err {
				return fmt.Errorf(
					"%w: AddEdge — adding blocked-by edge from %q to %q would create a cycle — "+
						"the target must be work that finishes BEFORE the source; "+
						"use DepTree or Ancestors to inspect the current dependency graph",
					ErrCycleDetected, sourceID.String(), targetID,
				)
			}
			return fmt.Errorf(
				"provenance.Tracker.AddEdge: failed to add blocked-by edge %q->%q: %w",
				sourceID.String(), targetID, err,
			)
		}
		return nil
	}

	if err := t.db.InsertEdge(sourceID, targetID, kind, time.Now().UTC()); err != nil {
		return fmt.Errorf(
			"provenance.Tracker.AddEdge: failed to insert edge %q->%q kind=%s: %w",
			sourceID.String(), targetID, kind.String(), err,
		)
	}
	return nil
}

func (t *sqliteTracker) RemoveEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	if kind == EdgeBlockedBy {
		if err := t.graph.RemoveEdge(sourceID.String(), targetID); err != nil {
			if dgraph.ErrEdgeNotFound == err {
				return nil
			}
			return fmt.Errorf(
				"provenance.Tracker.RemoveEdge: failed to remove blocked-by edge %q->%q: %w",
				sourceID.String(), targetID, err,
			)
		}
		return nil
	}

	if err := t.db.DeleteEdge(sourceID, targetID, kind); err != nil {
		return fmt.Errorf(
			"provenance.Tracker.RemoveEdge: failed to delete edge %q->%q kind=%s: %w",
			sourceID.String(), targetID, kind.String(), err,
		)
	}
	return nil
}

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
// Labels
// ---------------------------------------------------------------------------

func (t *sqliteTracker) AddLabel(id TaskID, label string) error {
	return t.db.AddLabel(id, label)
}

func (t *sqliteTracker) RemoveLabel(id TaskID, label string) error {
	return t.db.RemoveLabel(id, label)
}

func (t *sqliteTracker) Labels(id TaskID) ([]string, error) {
	labels, err := t.db.GetLabels(id)
	if err != nil {
		return nil, fmt.Errorf("provenance.Tracker.Labels: %w", err)
	}
	return labels, nil
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func (t *sqliteTracker) AddComment(id TaskID, authorID AgentID, body string) (Comment, error) {
	comment, err := t.db.AddComment(id, authorID, body)
	if err != nil {
		return Comment{}, fmt.Errorf("provenance.Tracker.AddComment: %w", err)
	}
	return comment, nil
}

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

func (t *sqliteTracker) RegisterMLAgent(namespace string, role Role, provider Provider, modelName string) (MLAgent, error) {
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
