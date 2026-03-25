// Package tracker provides the sqliteTracker implementation of the
// providence.Tracker interface. It lives in internal/tracker to avoid
// the import cycle that would occur if tracker.go lived in the root
// package (root imports internal/graph, internal/graph imports root).
package tracker

import (
	"fmt"
	"time"

	"github.com/dayvidpham/providence"
	"github.com/dayvidpham/providence/internal/graph"
	"github.com/dayvidpham/providence/internal/helpers"
	"github.com/dayvidpham/providence/internal/sqlite"
	dgraph "github.com/dominikbraun/graph"
	"github.com/google/uuid"
)

// SQLiteTracker is the canonical implementation of providence.Tracker.
// It holds a *sqlite.DB for persistence and a dgraph.Graph[string, Task]
// for blocked-by cycle prevention and traversal.
type SQLiteTracker struct {
	db    *sqlite.DB
	graph dgraph.Graph[string, providence.Task]
}

// Open creates a SQLiteTracker backed by a SQLite database at dbPath.
// Pass ":memory:" for an in-memory database.
func Open(dbPath string) (*SQLiteTracker, error) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf(
			"tracker.Open: failed to open SQLite database at %q: %w — "+
				"ensure the path is writable or use ':memory:' for in-memory usage",
			dbPath, err,
		)
	}

	g := graph.NewBlockedByGraph(db)
	return &SQLiteTracker{db: db, graph: g}, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) Close() error {
	if err := t.db.Close(); err != nil {
		return fmt.Errorf("tracker.SQLiteTracker.Close: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task CRUD
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) Create(namespace, title, description string, taskType providence.TaskType, priority providence.Priority, phase providence.Phase) (providence.Task, error) {
	if namespace == "" {
		return providence.Task{}, fmt.Errorf(
			"%w: Create — namespace is empty — "+
				"provide a non-empty namespace string such as 'aura-plugins' or 'my-project'",
			providence.ErrInvalidID,
		)
	}

	now := time.Now().UTC()
	task := providence.Task{
		ID: providence.TaskID{
			Namespace: namespace,
			UUID:      uuid.Must(uuid.NewV7()),
		},
		Title:       title,
		Description: description,
		Status:      providence.StatusOpen,
		Priority:    priority,
		Type:        taskType,
		Phase:       phase,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Register the task as a vertex in the blocked-by graph.
	// The graph's sqliteStore.AddVertex calls sqlite.InsertTask internally,
	// so no separate InsertTask call is needed.
	if err := t.graph.AddVertex(task); err != nil {
		return providence.Task{}, fmt.Errorf(
			"tracker.SQLiteTracker.Create: failed to insert task %q: %w — "+
				"check that the database is writable and the namespace is valid",
			task.ID.String(), err,
		)
	}

	return task, nil
}

func (t *SQLiteTracker) Show(id providence.TaskID) (providence.Task, error) {
	task, found, err := sqlite.GetTask(t.db, id)
	if err != nil {
		return providence.Task{}, fmt.Errorf("tracker.SQLiteTracker.Show: %w", err)
	}
	if !found {
		return providence.Task{}, fmt.Errorf(
			"%w: Show — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			providence.ErrNotFound, id.String(),
		)
	}
	return task, nil
}

func (t *SQLiteTracker) Update(id providence.TaskID, fields providence.UpdateFields) (providence.Task, error) {
	task, err := sqlite.UpdateTask(t.db, id, fields, time.Now().UTC())
	if err != nil {
		return providence.Task{}, fmt.Errorf("tracker.SQLiteTracker.Update: %w", err)
	}
	return task, nil
}

func (t *SQLiteTracker) CloseTask(id providence.TaskID, reason string) (providence.Task, error) {
	// Check current status first to return ErrAlreadyClosed instead of silently re-closing.
	current, found, err := sqlite.GetTask(t.db, id)
	if err != nil {
		return providence.Task{}, fmt.Errorf("tracker.SQLiteTracker.CloseTask: failed to fetch task %q: %w", id.String(), err)
	}
	if !found {
		return providence.Task{}, fmt.Errorf(
			"%w: CloseTask — task %q does not exist — "+
				"verify the TaskID was obtained from Create or a previous List/Show call",
			providence.ErrNotFound, id.String(),
		)
	}
	if current.Status == providence.StatusClosed {
		return providence.Task{}, fmt.Errorf(
			"%w: CloseTask — task %q is already closed (reason: %q) — "+
				"use Update to reopen the task (set status to open or in_progress) before closing again",
			providence.ErrAlreadyClosed, id.String(), current.CloseReason,
		)
	}

	task, err := sqlite.CloseTask(t.db, id, reason, time.Now().UTC())
	if err != nil {
		return providence.Task{}, fmt.Errorf("tracker.SQLiteTracker.CloseTask: %w", err)
	}
	return task, nil
}

func (t *SQLiteTracker) List(filter providence.ListFilter) ([]providence.Task, error) {
	tasks, err := sqlite.ListTasks(t.db, filter)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.List: %w", err)
	}
	return tasks, nil
}

// ---------------------------------------------------------------------------
// Typed Dependency Edges
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) AddEdge(sourceID providence.TaskID, targetID string, kind providence.EdgeKind) error {
	if kind == providence.EdgeBlockedBy {
		// Use the graph for cycle detection. The graph's sqliteStore.AddEdge
		// persists the edge to SQLite; dgraph enforces PreventCycles.
		if err := t.graph.AddEdge(sourceID.String(), targetID); err != nil {
			if dgraph.ErrEdgeCreatesCycle == err {
				return fmt.Errorf(
					"%w: AddEdge — adding blocked-by edge from %q to %q would create a cycle — "+
						"check the dependency direction: the target must be work that finishes BEFORE the source; "+
						"use DepTree or Ancestors to inspect the current dependency graph",
					providence.ErrCycleDetected, sourceID.String(), targetID,
				)
			}
			return fmt.Errorf(
				"tracker.SQLiteTracker.AddEdge: failed to add blocked-by edge %q->%q: %w",
				sourceID.String(), targetID, err,
			)
		}
		return nil
	}

	// Non-blocked-by edges go directly to sqlite without cycle checking.
	if err := sqlite.InsertEdge(t.db, sourceID, targetID, kind, time.Now().UTC()); err != nil {
		return fmt.Errorf(
			"tracker.SQLiteTracker.AddEdge: failed to insert edge %q->%q kind=%s: %w",
			sourceID.String(), targetID, kind.String(), err,
		)
	}
	return nil
}

func (t *SQLiteTracker) RemoveEdge(sourceID providence.TaskID, targetID string, kind providence.EdgeKind) error {
	if kind == providence.EdgeBlockedBy {
		if err := t.graph.RemoveEdge(sourceID.String(), targetID); err != nil {
			// dgraph.ErrEdgeNotFound means the edge didn't exist — treat as no-op.
			if dgraph.ErrEdgeNotFound == err {
				return nil
			}
			return fmt.Errorf(
				"tracker.SQLiteTracker.RemoveEdge: failed to remove blocked-by edge %q->%q: %w",
				sourceID.String(), targetID, err,
			)
		}
		return nil
	}

	if err := sqlite.DeleteEdge(t.db, sourceID, targetID, kind); err != nil {
		return fmt.Errorf(
			"tracker.SQLiteTracker.RemoveEdge: failed to delete edge %q->%q kind=%s: %w",
			sourceID.String(), targetID, kind.String(), err,
		)
	}
	return nil
}

func (t *SQLiteTracker) Edges(id providence.TaskID, kind *providence.EdgeKind) ([]providence.Edge, error) {
	edges, err := sqlite.GetEdges(t.db, id, kind)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Edges: %w", err)
	}
	return edges, nil
}

// ---------------------------------------------------------------------------
// Readiness Queries
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) Blocked() ([]providence.Task, error) {
	tasks, err := sqlite.BlockedTasks(t.db)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Blocked: %w", err)
	}
	return tasks, nil
}

func (t *SQLiteTracker) Ready() ([]providence.Task, error) {
	tasks, err := sqlite.ReadyTasks(t.db)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Ready: %w", err)
	}
	return tasks, nil
}

func (t *SQLiteTracker) DepTree(id providence.TaskID) ([]providence.Edge, error) {
	edges, err := sqlite.GetDepTree(t.db, id)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.DepTree: %w", err)
	}
	return edges, nil
}

func (t *SQLiteTracker) Ancestors(id providence.TaskID) ([]providence.Task, error) {
	ids, err := helpers.Ancestors(t.graph, id)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Ancestors: %w", err)
	}

	tasks := make([]providence.Task, 0, len(ids))
	for _, tid := range ids {
		task, found, err := sqlite.GetTask(t.db, tid)
		if err != nil {
			return nil, fmt.Errorf(
				"tracker.SQLiteTracker.Ancestors: failed to resolve task %q: %w",
				tid.String(), err,
			)
		}
		if found {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (t *SQLiteTracker) Descendants(id providence.TaskID) ([]providence.Task, error) {
	ids, err := helpers.Descendants(t.graph, id)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Descendants: %w", err)
	}

	tasks := make([]providence.Task, 0, len(ids))
	for _, tid := range ids {
		task, found, err := sqlite.GetTask(t.db, tid)
		if err != nil {
			return nil, fmt.Errorf(
				"tracker.SQLiteTracker.Descendants: failed to resolve task %q: %w",
				tid.String(), err,
			)
		}
		if found {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) AddLabel(id providence.TaskID, label string) error {
	if err := sqlite.AddLabel(t.db, id, label); err != nil {
		return fmt.Errorf("tracker.SQLiteTracker.AddLabel: %w", err)
	}
	return nil
}

func (t *SQLiteTracker) RemoveLabel(id providence.TaskID, label string) error {
	if err := sqlite.RemoveLabel(t.db, id, label); err != nil {
		return fmt.Errorf("tracker.SQLiteTracker.RemoveLabel: %w", err)
	}
	return nil
}

func (t *SQLiteTracker) Labels(id providence.TaskID) ([]string, error) {
	labels, err := sqlite.GetLabels(t.db, id)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Labels: %w", err)
	}
	return labels, nil
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) AddComment(id providence.TaskID, authorID providence.AgentID, body string) (providence.Comment, error) {
	now := time.Now().UTC()
	comment := providence.Comment{
		ID: providence.CommentID{
			Namespace: id.Namespace,
			UUID:      uuid.Must(uuid.NewV7()),
		},
		TaskID:    id,
		AuthorID:  authorID,
		Body:      body,
		CreatedAt: now,
	}

	if err := sqlite.InsertComment(t.db, comment); err != nil {
		return providence.Comment{}, fmt.Errorf("tracker.SQLiteTracker.AddComment: %w", err)
	}
	return comment, nil
}

func (t *SQLiteTracker) Comments(id providence.TaskID) ([]providence.Comment, error) {
	comments, err := sqlite.GetComments(t.db, id)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Comments: %w", err)
	}
	return comments, nil
}

// ---------------------------------------------------------------------------
// PROV-O Agents
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) RegisterHumanAgent(namespace, name, contact string) (providence.HumanAgent, error) {
	agent, err := sqlite.RegisterHumanAgent(t.db, namespace, name, contact, time.Now().UTC())
	if err != nil {
		return providence.HumanAgent{}, fmt.Errorf("tracker.SQLiteTracker.RegisterHumanAgent: %w", err)
	}
	return agent, nil
}

func (t *SQLiteTracker) RegisterMLAgent(namespace string, role providence.Role, provider providence.Provider, modelName string) (providence.MLAgent, error) {
	agent, err := sqlite.RegisterMLAgent(t.db, namespace, role, provider, modelName)
	if err != nil {
		return providence.MLAgent{}, fmt.Errorf("tracker.SQLiteTracker.RegisterMLAgent: %w", err)
	}
	return agent, nil
}

func (t *SQLiteTracker) RegisterSoftwareAgent(namespace, name, version, source string) (providence.SoftwareAgent, error) {
	agent, err := sqlite.RegisterSoftwareAgent(t.db, namespace, name, version, source)
	if err != nil {
		return providence.SoftwareAgent{}, fmt.Errorf("tracker.SQLiteTracker.RegisterSoftwareAgent: %w", err)
	}
	return agent, nil
}

func (t *SQLiteTracker) Agent(id providence.AgentID) (providence.Agent, error) {
	agent, found, err := sqlite.GetAgent(t.db, id)
	if err != nil {
		return providence.Agent{}, fmt.Errorf("tracker.SQLiteTracker.Agent: %w", err)
	}
	if !found {
		return providence.Agent{}, fmt.Errorf(
			"%w: Agent — agent %q does not exist — "+
				"use RegisterHumanAgent, RegisterMLAgent, or RegisterSoftwareAgent to create agents",
			providence.ErrNotFound, id.String(),
		)
	}
	return agent, nil
}

func (t *SQLiteTracker) HumanAgent(id providence.AgentID) (providence.HumanAgent, error) {
	agent, err := sqlite.GetHumanAgent(t.db, id)
	if err != nil {
		return providence.HumanAgent{}, fmt.Errorf("tracker.SQLiteTracker.HumanAgent: %w", err)
	}
	return agent, nil
}

func (t *SQLiteTracker) MLAgent(id providence.AgentID) (providence.MLAgent, error) {
	agent, err := sqlite.GetMLAgent(t.db, id)
	if err != nil {
		return providence.MLAgent{}, fmt.Errorf("tracker.SQLiteTracker.MLAgent: %w", err)
	}
	return agent, nil
}

func (t *SQLiteTracker) SoftwareAgent(id providence.AgentID) (providence.SoftwareAgent, error) {
	agent, err := sqlite.GetSoftwareAgent(t.db, id)
	if err != nil {
		return providence.SoftwareAgent{}, fmt.Errorf("tracker.SQLiteTracker.SoftwareAgent: %w", err)
	}
	return agent, nil
}

// ---------------------------------------------------------------------------
// PROV-O Activities
// ---------------------------------------------------------------------------

func (t *SQLiteTracker) StartActivity(agentID providence.AgentID, phase providence.Phase, stage providence.Stage, notes string) (providence.Activity, error) {
	now := time.Now().UTC()
	activity := providence.Activity{
		ID: providence.ActivityID{
			Namespace: agentID.Namespace,
			UUID:      uuid.Must(uuid.NewV7()),
		},
		AgentID:   agentID,
		Phase:     phase,
		Stage:     stage,
		StartedAt: now,
		Notes:     notes,
	}

	if err := sqlite.InsertActivity(t.db, activity); err != nil {
		return providence.Activity{}, fmt.Errorf(
			"tracker.SQLiteTracker.StartActivity: failed to insert activity for agent %q: %w — "+
				"ensure the agent is registered before starting an activity",
			agentID.String(), err,
		)
	}
	return activity, nil
}

func (t *SQLiteTracker) EndActivity(id providence.ActivityID) (providence.Activity, error) {
	activity, err := sqlite.EndActivity(t.db, id, time.Now().UTC())
	if err != nil {
		return providence.Activity{}, fmt.Errorf("tracker.SQLiteTracker.EndActivity: %w", err)
	}
	return activity, nil
}

func (t *SQLiteTracker) Activities(agentID *providence.AgentID) ([]providence.Activity, error) {
	activities, err := sqlite.GetActivities(t.db, agentID)
	if err != nil {
		return nil, fmt.Errorf("tracker.SQLiteTracker.Activities: %w", err)
	}
	return activities, nil
}
