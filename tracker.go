package providence

// tracker.go contains the sqliteTracker implementation of the Tracker interface.
//
// Import constraint: ALL packages under internal/ (sqlite, graph, helpers)
// import the root providence package for its types (Task, TaskID, etc.).
// To avoid the resulting import cycle, tracker.go CANNOT import any internal/*
// package. Instead, it uses zombiezen.com/go/sqlite and dominikbraun/graph
// directly, repeating the SQLite operations that internal/sqlite provides.
//
// This is the canonical Go solution when a package provides both the types
// used by its internal packages AND the implementation that needs those
// packages. The alternative (splitting types into pkg/types) would change
// the public API and is a larger refactor.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	dgraph "github.com/dominikbraun/graph"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// sqliteTracker — implements Tracker
// ---------------------------------------------------------------------------

// sqliteTracker is the canonical implementation of Tracker.
// The conn field holds the single SQLite connection guarded by mu.
// The graph field is a blocked-by graph used for cycle prevention and
// traversal; its store reads/writes the same SQLite connection.
type sqliteTracker struct {
	mu   sync.Mutex
	conn *zs.Conn
	// graph is a directed, cycle-preventing dgraph over task ID strings.
	// Vertices are task ID wire strings; the vertex value type is Task.
	// The store delegates to the same SQLite connection.
	graph dgraph.Graph[string, Task]
}

// openTracker opens (or creates) a SQLite database at dbPath and returns
// an initialised Tracker. Pass ":memory:" for an in-memory database.
func openTracker(dbPath string) (Tracker, error) {
	if dbPath != ":memory:" {
		// Parent directories are created by sqlite.Open in internal/sqlite;
		// we replicate that here for the root-level constructor.
		// sqlite.OpenConn handles creating the file itself.
	}

	conn, err := zs.OpenConn(dbPath, zs.OpenReadWrite|zs.OpenCreate|zs.OpenWAL|zs.OpenURI)
	if err != nil {
		return nil, fmt.Errorf(
			"providence.openTracker: failed to open SQLite at %q: %w — "+
				"ensure the path is writable, the parent directory exists, "+
				"and no other process holds an exclusive lock",
			dbPath, err,
		)
	}

	t := &sqliteTracker{conn: conn}

	if err := t.applyPragmas(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("providence.openTracker: failed to apply pragmas on %q: %w", dbPath, err)
	}

	if err := t.ensureSchema(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("providence.openTracker: failed to apply schema on %q: %w", dbPath, err)
	}

	// Construct the blocked-by graph backed by the same connection.
	store := &graphStore{t: t}
	t.graph = dgraph.NewWithStore(
		func(task Task) string { return task.ID.String() },
		store,
		dgraph.Directed(),
		dgraph.PreventCycles(),
	)

	return t, nil
}

func (t *sqliteTracker) applyPragmas() error {
	for _, p := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	} {
		if err := sqlitex.ExecuteTransient(t.conn, p, nil); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Schema + seed data (mirrors internal/sqlite/schema.go)
// ---------------------------------------------------------------------------

func (t *sqliteTracker) ensureSchema() error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS statuses (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS priorities (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS task_types (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS edge_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS agent_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS providers (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS roles (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS phases (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS ml_models (
			id          INTEGER PRIMARY KEY,
			provider_id INTEGER NOT NULL REFERENCES providers(id),
			name        TEXT NOT NULL,
			UNIQUE (provider_id, name)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS agents (
			id      TEXT PRIMARY KEY,
			kind_id INTEGER NOT NULL REFERENCES agent_kinds(id)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS agents_human (
			agent_id TEXT PRIMARY KEY REFERENCES agents(id),
			name     TEXT NOT NULL,
			contact  TEXT NOT NULL DEFAULT ''
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS agents_ml (
			agent_id TEXT PRIMARY KEY REFERENCES agents(id),
			role_id  INTEGER NOT NULL REFERENCES roles(id),
			model_id INTEGER NOT NULL REFERENCES ml_models(id)
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS agents_software (
			agent_id TEXT PRIMARY KEY REFERENCES agents(id),
			name     TEXT NOT NULL,
			version  TEXT NOT NULL DEFAULT '',
			source   TEXT NOT NULL DEFAULT ''
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id           TEXT PRIMARY KEY,
			namespace    TEXT NOT NULL,
			title        TEXT NOT NULL,
			description  TEXT NOT NULL DEFAULT '',
			status_id    INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),
			priority_id  INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),
			type_id      INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),
			phase_id     INTEGER NOT NULL REFERENCES phases(id),
			owner_id     TEXT REFERENCES agents(id),
			notes        TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			closed_at    INTEGER,
			close_reason TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status    ON tasks (status_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_priority  ON tasks (priority_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_type      ON tasks (type_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_phase     ON tasks (phase_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_owner     ON tasks (owner_id)`,
		`CREATE TABLE IF NOT EXISTS edges (
			source_id  TEXT NOT NULL REFERENCES tasks(id),
			target_id  TEXT NOT NULL,
			kind_id    INTEGER NOT NULL REFERENCES edge_kinds(id),
			created_at INTEGER NOT NULL,
			PRIMARY KEY (source_id, target_id, kind_id)
		) STRICT, WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_edges_source ON edges (source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_target ON edges (target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_kind   ON edges (kind_id)`,
		`CREATE TABLE IF NOT EXISTS activities (
			id         TEXT PRIMARY KEY,
			agent_id   TEXT NOT NULL REFERENCES agents(id),
			phase_id   INTEGER NOT NULL REFERENCES phases(id),
			stage_id   INTEGER NOT NULL REFERENCES stages(id),
			started_at INTEGER NOT NULL,
			ended_at   INTEGER,
			notes      TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_activities_agent ON activities (agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_activities_phase ON activities (phase_id)`,
		`CREATE TABLE IF NOT EXISTS labels (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			name    TEXT NOT NULL,
			PRIMARY KEY (task_id, name)
		) STRICT, WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_labels_name ON labels (name)`,
		`CREATE TABLE IF NOT EXISTS comments (
			id         TEXT PRIMARY KEY,
			task_id    TEXT NOT NULL REFERENCES tasks(id),
			author_id  TEXT NOT NULL REFERENCES agents(id),
			body       TEXT NOT NULL,
			created_at INTEGER NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_comments_task   ON comments (task_id)`,
		`CREATE INDEX IF NOT EXISTS idx_comments_author ON comments (author_id)`,
	}

	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(t.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureSchema: %w — statement: %s", err, stmt[:min(len(stmt), 80)])
		}
	}
	return t.seedReferenceData()
}

func (t *sqliteTracker) seedReferenceData() error {
	seeds := []string{
		`INSERT OR IGNORE INTO statuses (id, name) VALUES (0,'open'),(1,'in_progress'),(2,'closed')`,
		`INSERT OR IGNORE INTO priorities (id, name) VALUES (0,'critical'),(1,'high'),(2,'medium'),(3,'low'),(4,'backlog')`,
		`INSERT OR IGNORE INTO task_types (id, name) VALUES (0,'bug'),(1,'feature'),(2,'task'),(3,'epic'),(4,'chore')`,
		`INSERT OR IGNORE INTO edge_kinds (id, name) VALUES (0,'blocked_by'),(1,'derived_from'),(2,'supersedes'),(3,'discovered_from'),(4,'generated_by'),(5,'attributed_to')`,
		`INSERT OR IGNORE INTO agent_kinds (id, name) VALUES (0,'human'),(1,'machine_learning'),(2,'software')`,
		`INSERT OR IGNORE INTO providers (id, name) VALUES (0,'anthropic'),(1,'google'),(2,'openai'),(3,'local')`,
		`INSERT OR IGNORE INTO roles (id, name) VALUES (0,'human'),(1,'architect'),(2,'supervisor'),(3,'worker'),(4,'reviewer')`,
		`INSERT OR IGNORE INTO phases (id, name) VALUES (0,'request'),(1,'elicit'),(2,'propose'),(3,'review'),(4,'plan_uat'),(5,'ratify'),(6,'handoff'),(7,'impl_plan'),(8,'worker_slices'),(9,'code_review'),(10,'impl_uat'),(11,'landing'),(12,'unscoped')`,
		`INSERT OR IGNORE INTO stages (id, name) VALUES (0,'not_started'),(1,'in_progress'),(2,'blocked'),(3,'complete')`,
		`INSERT OR IGNORE INTO ml_models (id, provider_id, name) VALUES (0,0,'claude_opus_4'),(1,0,'claude_sonnet_4'),(2,0,'claude_haiku_4')`,
	}
	for _, seed := range seeds {
		if err := sqlitex.ExecuteTransient(t.conn, seed, nil); err != nil {
			return fmt.Errorf("seedReferenceData: %w — seed: %s", err, seed[:min(len(seed), 80)])
		}
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// graphStore — implements dgraph.Store[string, Task]
// ---------------------------------------------------------------------------

// graphStore implements dgraph.Store[string, Task] for the blocked-by subgraph.
// It delegates all persistence to the sqliteTracker's SQLite connection.
// Defined here (root package) to avoid the import cycle that would result
// from importing internal/graph (which imports the root package).
type graphStore struct {
	t *sqliteTracker
}

var _ dgraph.Store[string, Task] = (*graphStore)(nil)

func (s *graphStore) AddVertex(hash string, value Task, _ dgraph.VertexProperties) error {
	if hash != value.ID.String() {
		return fmt.Errorf(
			"graphStore.AddVertex: hash %q does not match task ID %q — "+
				"the hash function must return task.ID.String()",
			hash, value.ID.String(),
		)
	}
	return s.t.insertTask(value)
}

func (s *graphStore) Vertex(hash string) (Task, dgraph.VertexProperties, error) {
	id, err := ParseTaskID(hash)
	if err != nil {
		return Task{}, dgraph.VertexProperties{}, fmt.Errorf(
			"graphStore.Vertex: cannot parse hash %q as TaskID: %w", hash, err,
		)
	}
	task, found, err := s.t.getTask(id)
	if err != nil {
		return Task{}, dgraph.VertexProperties{}, fmt.Errorf(
			"graphStore.Vertex: failed to get task %q: %w", hash, err,
		)
	}
	if !found {
		return Task{}, dgraph.VertexProperties{}, dgraph.ErrVertexNotFound
	}
	return task, dgraph.VertexProperties{}, nil
}

func (s *graphStore) RemoveVertex(_ string) error {
	return fmt.Errorf(
		"graphStore.RemoveVertex: not implemented — " +
			"close the task via CloseTask instead of deleting it",
	)
}

func (s *graphStore) ListVertices() ([]string, error) {
	tasks, err := s.t.listTasks(ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("graphStore.ListVertices: %w", err)
	}
	hashes := make([]string, len(tasks))
	for i, task := range tasks {
		hashes[i] = task.ID.String()
	}
	return hashes, nil
}

func (s *graphStore) VertexCount() (int, error) {
	hashes, err := s.ListVertices()
	if err != nil {
		return 0, fmt.Errorf("graphStore.VertexCount: %w", err)
	}
	return len(hashes), nil
}

func (s *graphStore) AddEdge(sourceHash, targetHash string, _ dgraph.Edge[string]) error {
	srcID, err := ParseTaskID(sourceHash)
	if err != nil {
		return fmt.Errorf("graphStore.AddEdge: invalid source hash %q: %w", sourceHash, err)
	}
	return s.t.insertEdge(srcID, targetHash, EdgeBlockedBy, time.Now().UTC())
}

func (s *graphStore) UpdateEdge(sourceHash, targetHash string, _ dgraph.Edge[string]) error {
	_, err := s.Edge(sourceHash, targetHash)
	return err
}

func (s *graphStore) RemoveEdge(sourceHash, targetHash string) error {
	srcID, err := ParseTaskID(sourceHash)
	if err != nil {
		return fmt.Errorf("graphStore.RemoveEdge: invalid source hash %q: %w", sourceHash, err)
	}
	return s.t.deleteEdge(srcID, targetHash, EdgeBlockedBy)
}

func (s *graphStore) Edge(sourceHash, targetHash string) (dgraph.Edge[string], error) {
	srcID, err := ParseTaskID(sourceHash)
	if err != nil {
		return dgraph.Edge[string]{}, fmt.Errorf("graphStore.Edge: invalid source hash %q: %w", sourceHash, err)
	}
	kind := EdgeBlockedBy
	edges, err := s.t.getEdges(srcID, &kind)
	if err != nil {
		return dgraph.Edge[string]{}, fmt.Errorf("graphStore.Edge: %w", err)
	}
	for _, e := range edges {
		if e.TargetID == targetHash {
			return dgraph.Edge[string]{
				Source:     sourceHash,
				Target:     targetHash,
				Properties: dgraph.EdgeProperties{Attributes: map[string]string{}},
			}, nil
		}
	}
	return dgraph.Edge[string]{}, dgraph.ErrEdgeNotFound
}

func (s *graphStore) ListEdges() ([]dgraph.Edge[string], error) {
	edges, err := s.t.getBlockedByEdges()
	if err != nil {
		return nil, fmt.Errorf("graphStore.ListEdges: %w", err)
	}
	result := make([]dgraph.Edge[string], len(edges))
	for i, e := range edges {
		result[i] = dgraph.Edge[string]{
			Source:     e.SourceID,
			Target:     e.TargetID,
			Properties: dgraph.EdgeProperties{Attributes: map[string]string{}},
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (t *sqliteTracker) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	err := t.conn.Close()
	t.conn = nil
	if err != nil {
		return fmt.Errorf(
			"providence.Tracker.Close: failed to close SQLite connection: %w — "+
				"this may indicate uncommitted transactions",
			err,
		)
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

	// AddVertex calls graphStore.AddVertex → insertTask.
	if err := t.graph.AddVertex(task); err != nil {
		return Task{}, fmt.Errorf(
			"providence.Tracker.Create: failed to insert task %q: %w — "+
				"check that the database is writable and the namespace is valid",
			task.ID.String(), err,
		)
	}
	return task, nil
}

func (t *sqliteTracker) Show(id TaskID) (Task, error) {
	task, found, err := t.getTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("providence.Tracker.Show: %w", err)
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
	task, err := t.updateTask(id, fields, time.Now().UTC())
	if err != nil {
		return Task{}, fmt.Errorf("providence.Tracker.Update: %w", err)
	}
	return task, nil
}

func (t *sqliteTracker) CloseTask(id TaskID, reason string) (Task, error) {
	current, found, err := t.getTask(id)
	if err != nil {
		return Task{}, fmt.Errorf("providence.Tracker.CloseTask: failed to fetch task %q: %w", id.String(), err)
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

	task, err := t.closeTask(id, reason, time.Now().UTC())
	if err != nil {
		return Task{}, fmt.Errorf("providence.Tracker.CloseTask: %w", err)
	}
	return task, nil
}

func (t *sqliteTracker) List(filter ListFilter) ([]Task, error) {
	tasks, err := t.listTasks(filter)
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.List: %w", err)
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
				"providence.Tracker.AddEdge: failed to add blocked-by edge %q->%q: %w",
				sourceID.String(), targetID, err,
			)
		}
		return nil
	}

	if err := t.insertEdge(sourceID, targetID, kind, time.Now().UTC()); err != nil {
		return fmt.Errorf(
			"providence.Tracker.AddEdge: failed to insert edge %q->%q kind=%s: %w",
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
				"providence.Tracker.RemoveEdge: failed to remove blocked-by edge %q->%q: %w",
				sourceID.String(), targetID, err,
			)
		}
		return nil
	}

	if err := t.deleteEdge(sourceID, targetID, kind); err != nil {
		return fmt.Errorf(
			"providence.Tracker.RemoveEdge: failed to delete edge %q->%q kind=%s: %w",
			sourceID.String(), targetID, kind.String(), err,
		)
	}
	return nil
}

func (t *sqliteTracker) Edges(id TaskID, kind *EdgeKind) ([]Edge, error) {
	edges, err := t.getEdges(id, kind)
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.Edges: %w", err)
	}
	return edges, nil
}

// ---------------------------------------------------------------------------
// Readiness Queries
// ---------------------------------------------------------------------------

func (t *sqliteTracker) Blocked() ([]Task, error) {
	tasks, err := t.blockedTasks()
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.Blocked: %w", err)
	}
	return tasks, nil
}

func (t *sqliteTracker) Ready() ([]Task, error) {
	tasks, err := t.readyTasks()
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.Ready: %w", err)
	}
	return tasks, nil
}

func (t *sqliteTracker) DepTree(id TaskID) ([]Edge, error) {
	edges, err := t.getDepTree(id)
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.DepTree: %w", err)
	}
	return edges, nil
}

// Ancestors returns all tasks that transitively block the given task.
// In the blocked-by graph, A→B means "A is blocked by B". Ancestors of A
// are B and everything B transitively waits for (outgoing adjacency DFS).
func (t *sqliteTracker) Ancestors(id TaskID) ([]Task, error) {
	adjacency, err := t.graph.AdjacencyMap()
	if err != nil {
		return nil, fmt.Errorf(
			"providence.Tracker.Ancestors: failed to compute adjacency map for task %q: %w",
			id.String(), err,
		)
	}

	var ids []TaskID
	visited := make(map[string]bool)
	var dfs func(cur string)
	dfs = func(cur string) {
		for adj := range adjacency[cur] {
			if !visited[adj] {
				visited[adj] = true
				if tid, err := ParseTaskID(adj); err == nil {
					ids = append(ids, tid)
				}
				dfs(adj)
			}
		}
	}
	dfs(id.String())

	tasks := make([]Task, 0, len(ids))
	for _, tid := range ids {
		task, found, err := t.getTask(tid)
		if err != nil {
			return nil, fmt.Errorf("providence.Tracker.Ancestors: failed to resolve task %q: %w", tid.String(), err)
		}
		if found {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// Descendants returns all tasks that are transitively waiting for the given
// task. In the blocked-by graph, A→B means "A is blocked by B". Descendants
// of B are A and everything that transitively depends on A (predecessor DFS).
func (t *sqliteTracker) Descendants(id TaskID) ([]Task, error) {
	predecessors, err := t.graph.PredecessorMap()
	if err != nil {
		return nil, fmt.Errorf(
			"providence.Tracker.Descendants: failed to compute predecessor map for task %q: %w",
			id.String(), err,
		)
	}

	var ids []TaskID
	visited := make(map[string]bool)
	var dfs func(cur string)
	dfs = func(cur string) {
		for pred := range predecessors[cur] {
			if !visited[pred] {
				visited[pred] = true
				if tid, err := ParseTaskID(pred); err == nil {
					ids = append(ids, tid)
				}
				dfs(pred)
			}
		}
	}
	dfs(id.String())

	tasks := make([]Task, 0, len(ids))
	for _, tid := range ids {
		task, found, err := t.getTask(tid)
		if err != nil {
			return nil, fmt.Errorf("providence.Tracker.Descendants: failed to resolve task %q: %w", tid.String(), err)
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

func (t *sqliteTracker) AddLabel(id TaskID, label string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return sqlitex.Execute(t.conn,
		`INSERT OR IGNORE INTO labels (task_id, name) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{id.String(), label}})
}

func (t *sqliteTracker) RemoveLabel(id TaskID, label string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return sqlitex.Execute(t.conn,
		`DELETE FROM labels WHERE task_id = ?1 AND name = ?2`,
		&sqlitex.ExecOptions{Args: []any{id.String(), label}})
}

func (t *sqliteTracker) Labels(id TaskID) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var labels []string
	err := sqlitex.Execute(t.conn,
		`SELECT name FROM labels WHERE task_id = ?1 ORDER BY name ASC`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				labels = append(labels, stmt.ColumnText(0))
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.Labels: %w", err)
	}
	return labels, nil
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func (t *sqliteTracker) AddComment(id TaskID, authorID AgentID, body string) (Comment, error) {
	now := time.Now().UTC()
	comment := Comment{
		ID:        CommentID{Namespace: id.Namespace, UUID: uuid.Must(uuid.NewV7())},
		TaskID:    id,
		AuthorID:  authorID,
		Body:      body,
		CreatedAt: now,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := sqlitex.Execute(t.conn,
		`INSERT INTO comments (id, task_id, author_id, body, created_at) VALUES (?1, ?2, ?3, ?4, ?5)`,
		&sqlitex.ExecOptions{Args: []any{
			comment.ID.String(), comment.TaskID.String(),
			comment.AuthorID.String(), comment.Body, comment.CreatedAt.UnixNano(),
		}}); err != nil {
		return Comment{}, fmt.Errorf(
			"providence.Tracker.AddComment: failed to insert comment on task %q: %w — "+
				"check that the task and author agent both exist",
			id.String(), err,
		)
	}
	return comment, nil
}

func (t *sqliteTracker) Comments(id TaskID) ([]Comment, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var comments []Comment
	err := sqlitex.Execute(t.conn,
		`SELECT id, task_id, author_id, body, created_at
		 FROM comments WHERE task_id = ?1 ORDER BY created_at ASC`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				c, err := t.scanComment(stmt)
				if err != nil {
					return err
				}
				comments = append(comments, c)
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.Comments: %w", err)
	}
	return comments, nil
}

// ---------------------------------------------------------------------------
// PROV-O Agents
// ---------------------------------------------------------------------------

func (t *sqliteTracker) RegisterHumanAgent(namespace, name, contact string) (HumanAgent, error) {
	id := AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := sqlitex.Execute(t.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 0)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return HumanAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterHumanAgent: failed to insert agent row: %w", err,
		)
	}
	if err := sqlitex.Execute(t.conn,
		`INSERT INTO agents_human (agent_id, name, contact) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{id.String(), name, contact}}); err != nil {
		return HumanAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterHumanAgent: failed to insert human row: %w", err,
		)
	}
	return HumanAgent{
		Agent:   Agent{ID: id, Kind: AgentKindHuman},
		Name:    name,
		Contact: contact,
	}, nil
}

func (t *sqliteTracker) RegisterMLAgent(namespace string, role Role, provider Provider, modelName string) (MLAgent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var modelID int
	var modelFound bool
	if err := sqlitex.Execute(t.conn,
		`SELECT id FROM ml_models WHERE provider_id = ?1 AND name = ?2`,
		&sqlitex.ExecOptions{
			Args: []any{int(provider), modelName},
			ResultFunc: func(stmt *zs.Stmt) error {
				modelID = stmt.ColumnInt(0)
				modelFound = true
				return nil
			},
		}); err != nil {
		return MLAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterMLAgent: model lookup (%s, %q) failed: %w",
			provider.String(), modelName, err,
		)
	}
	if !modelFound {
		return MLAgent{}, fmt.Errorf(
			"%w: RegisterMLAgent — model (%s, %q) not found in ml_models — "+
				"use a known (provider, name) combination seeded at database creation time",
			ErrNotFound, provider.String(), modelName,
		)
	}

	id := AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	if err := sqlitex.Execute(t.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 1)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return MLAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterMLAgent: failed to insert base agent row: %w", err,
		)
	}
	if err := sqlitex.Execute(t.conn,
		`INSERT INTO agents_ml (agent_id, role_id, model_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{id.String(), int(role), modelID}}); err != nil {
		return MLAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterMLAgent: failed to insert ml agent row: %w", err,
		)
	}
	return MLAgent{
		Agent: Agent{ID: id, Kind: AgentKindMachineLearning},
		Role:  role,
		Model: MLModel{ID: modelID, Provider: provider, Name: modelName},
	}, nil
}

func (t *sqliteTracker) RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error) {
	id := AgentID{Namespace: namespace, UUID: uuid.Must(uuid.NewV7())}
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := sqlitex.Execute(t.conn,
		`INSERT INTO agents (id, kind_id) VALUES (?1, 2)`,
		&sqlitex.ExecOptions{Args: []any{id.String()}}); err != nil {
		return SoftwareAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterSoftwareAgent: failed to insert base agent row: %w", err,
		)
	}
	if err := sqlitex.Execute(t.conn,
		`INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{id.String(), name, version, source}}); err != nil {
		return SoftwareAgent{}, fmt.Errorf(
			"providence.Tracker.RegisterSoftwareAgent: failed to insert software agent row: %w", err,
		)
	}
	return SoftwareAgent{
		Agent:   Agent{ID: id, Kind: AgentKindSoftware},
		Name:    name,
		Version: version,
		Source:  source,
	}, nil
}

func (t *sqliteTracker) Agent(id AgentID) (Agent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var agent Agent
	var found bool
	err := sqlitex.Execute(t.conn,
		`SELECT id, kind_id FROM agents WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				agent = Agent{ID: id, Kind: AgentKind(stmt.ColumnInt(1))}
				found = true
				return nil
			},
		})
	if err != nil {
		return Agent{}, fmt.Errorf("providence.Tracker.Agent: %w", err)
	}
	if !found {
		return Agent{}, fmt.Errorf(
			"%w: Agent — agent %q does not exist — "+
				"use RegisterHumanAgent, RegisterMLAgent, or RegisterSoftwareAgent to create agents",
			ErrNotFound, id.String(),
		)
	}
	return agent, nil
}

func (t *sqliteTracker) HumanAgent(id AgentID) (HumanAgent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ha HumanAgent
	var found bool
	err := sqlitex.Execute(t.conn,
		`SELECT a.kind_id, h.name, h.contact
		 FROM agents a JOIN agents_human h ON a.id = h.agent_id
		 WHERE a.id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				ha = HumanAgent{
					Agent:   Agent{ID: id, Kind: AgentKindHuman},
					Name:    stmt.ColumnText(1),
					Contact: stmt.ColumnText(2),
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return HumanAgent{}, fmt.Errorf("providence.Tracker.HumanAgent: %w", err)
	}
	if !found {
		return HumanAgent{}, fmt.Errorf(
			"%w: HumanAgent — agent %q not found or is not a human agent — "+
				"call Agent() first to inspect the Kind field",
			ErrNotFound, id.String(),
		)
	}
	return ha, nil
}

func (t *sqliteTracker) MLAgent(id AgentID) (MLAgent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var mla MLAgent
	var found bool
	err := sqlitex.Execute(t.conn,
		`SELECT a.kind_id, m.role_id, ml.id, ml.provider_id, ml.name
		 FROM agents a
		 JOIN agents_ml m ON a.id = m.agent_id
		 JOIN ml_models ml ON m.model_id = ml.id
		 WHERE a.id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				mla = MLAgent{
					Agent: Agent{ID: id, Kind: AgentKindMachineLearning},
					Role:  Role(stmt.ColumnInt(1)),
					Model: MLModel{
						ID:       stmt.ColumnInt(2),
						Provider: Provider(stmt.ColumnInt(3)),
						Name:     stmt.ColumnText(4),
					},
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return MLAgent{}, fmt.Errorf("providence.Tracker.MLAgent: %w", err)
	}
	if !found {
		return MLAgent{}, fmt.Errorf(
			"%w: MLAgent — agent %q not found or is not an ML agent — "+
				"call Agent() first to inspect the Kind field",
			ErrNotFound, id.String(),
		)
	}
	return mla, nil
}

func (t *sqliteTracker) SoftwareAgent(id AgentID) (SoftwareAgent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sa SoftwareAgent
	var found bool
	err := sqlitex.Execute(t.conn,
		`SELECT a.kind_id, s.name, s.version, s.source
		 FROM agents a JOIN agents_software s ON a.id = s.agent_id
		 WHERE a.id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				sa = SoftwareAgent{
					Agent:   Agent{ID: id, Kind: AgentKindSoftware},
					Name:    stmt.ColumnText(1),
					Version: stmt.ColumnText(2),
					Source:  stmt.ColumnText(3),
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return SoftwareAgent{}, fmt.Errorf("providence.Tracker.SoftwareAgent: %w", err)
	}
	if !found {
		return SoftwareAgent{}, fmt.Errorf(
			"%w: SoftwareAgent — agent %q not found or is not a software agent — "+
				"call Agent() first to inspect the Kind field",
			ErrNotFound, id.String(),
		)
	}
	return sa, nil
}

// ---------------------------------------------------------------------------
// PROV-O Activities
// ---------------------------------------------------------------------------

func (t *sqliteTracker) StartActivity(agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error) {
	now := time.Now().UTC()
	activity := Activity{
		ID:        ActivityID{Namespace: agentID.Namespace, UUID: uuid.Must(uuid.NewV7())},
		AgentID:   agentID,
		Phase:     phase,
		Stage:     stage,
		StartedAt: now,
		Notes:     notes,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := sqlitex.Execute(t.conn,
		`INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
		&sqlitex.ExecOptions{Args: []any{
			activity.ID.String(), activity.AgentID.String(),
			int(activity.Phase), int(activity.Stage),
			activity.StartedAt.UnixNano(), nil, activity.Notes,
		}}); err != nil {
		return Activity{}, fmt.Errorf(
			"providence.Tracker.StartActivity: failed to insert activity for agent %q: %w — "+
				"ensure the agent is registered before starting an activity",
			agentID.String(), err,
		)
	}
	return activity, nil
}

func (t *sqliteTracker) EndActivity(id ActivityID) (Activity, error) {
	endTime := time.Now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := sqlitex.Execute(t.conn,
		`UPDATE activities SET ended_at = ?2 WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{id.String(), endTime.UnixNano()}}); err != nil {
		return Activity{}, fmt.Errorf("providence.Tracker.EndActivity: %w", err)
	}

	var act Activity
	var found bool
	if err := sqlitex.Execute(t.conn,
		`SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes
		 FROM activities WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				act, err = t.scanActivity(stmt)
				if err != nil {
					return err
				}
				found = true
				return nil
			},
		}); err != nil {
		return Activity{}, fmt.Errorf("providence.Tracker.EndActivity: re-fetch: %w", err)
	}
	if !found {
		return Activity{}, fmt.Errorf(
			"%w: EndActivity — activity %q not found — "+
				"verify the ActivityID was obtained from StartActivity",
			ErrNotFound, id.String(),
		)
	}
	return act, nil
}

func (t *sqliteTracker) Activities(agentID *AgentID) ([]Activity, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	query := `SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes FROM activities`
	var args []any
	if agentID != nil {
		query += ` WHERE agent_id = ?1`
		args = append(args, agentID.String())
	}
	query += ` ORDER BY started_at ASC`

	var activities []Activity
	err := sqlitex.Execute(t.conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			act, err := t.scanActivity(stmt)
			if err != nil {
				return err
			}
			activities = append(activities, act)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("providence.Tracker.Activities: %w", err)
	}
	return activities, nil
}

// ---------------------------------------------------------------------------
// Internal SQL helpers (called by both sqliteTracker methods and graphStore)
// The caller is responsible for acquiring t.mu before calling these.
// Exception: methods that acquire the lock themselves are documented as such.
// ---------------------------------------------------------------------------

// insertTask inserts a task row. Acquires t.mu.
// The graphStore.AddVertex calls this, so it must not re-acquire the lock.
// IMPORTANT: this is called from graphStore.AddVertex which is called from
// t.graph.AddVertex. The dgraph library does not hold t.mu, so we must
// acquire it here.
func (t *sqliteTracker) insertTask(task Task) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var ownerVal any
	if task.Owner != nil {
		ownerVal = task.Owner.String()
	}

	return sqlitex.Execute(t.conn,
		`INSERT INTO tasks
			(id, namespace, title, description, status_id, priority_id, type_id,
			 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)`,
		&sqlitex.ExecOptions{Args: []any{
			task.ID.String(), task.ID.Namespace, task.Title, task.Description,
			int(task.Status), int(task.Priority), int(task.Type), int(task.Phase),
			ownerVal, task.Notes,
			task.CreatedAt.UnixNano(), task.UpdatedAt.UnixNano(),
			timeToNullInt(task.ClosedAt), task.CloseReason,
		}})
}

// getTask retrieves a task by ID. Acquires t.mu.
func (t *sqliteTracker) getTask(id TaskID) (Task, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var task Task
	var found bool
	err := sqlitex.Execute(t.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = t.scanTask(stmt)
				if err != nil {
					return err
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return Task{}, false, fmt.Errorf("getTask %q: %w", id.String(), err)
	}
	return task, found, nil
}

// updateTask applies partial updates to a task. Acquires t.mu.
func (t *sqliteTracker) updateTask(id TaskID, fields UpdateFields, now time.Time) (Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	setClauses := []string{"updated_at = ?1"}
	args := []any{now.UnixNano()}
	idx := 2

	if fields.Title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = ?%d", idx))
		args = append(args, *fields.Title)
		idx++
	}
	if fields.Description != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = ?%d", idx))
		args = append(args, *fields.Description)
		idx++
	}
	if fields.Status != nil {
		setClauses = append(setClauses, fmt.Sprintf("status_id = ?%d", idx))
		args = append(args, int(*fields.Status))
		idx++
	}
	if fields.Priority != nil {
		setClauses = append(setClauses, fmt.Sprintf("priority_id = ?%d", idx))
		args = append(args, int(*fields.Priority))
		idx++
	}
	if fields.Phase != nil {
		setClauses = append(setClauses, fmt.Sprintf("phase_id = ?%d", idx))
		args = append(args, int(*fields.Phase))
		idx++
	}
	if fields.Notes != nil {
		setClauses = append(setClauses, fmt.Sprintf("notes = ?%d", idx))
		args = append(args, *fields.Notes)
		idx++
	}
	if fields.Owner != nil {
		setClauses = append(setClauses, fmt.Sprintf("owner_id = ?%d", idx))
		args = append(args, fields.Owner.String())
		idx++
	}

	args = append(args, id.String())
	whereIdx := idx
	query := fmt.Sprintf(`UPDATE tasks SET %s WHERE id = ?%d`,
		strings.Join(setClauses, ", "), whereIdx)

	if err := sqlitex.Execute(t.conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		return Task{}, fmt.Errorf("updateTask %q: %w", id.String(), err)
	}

	var task Task
	var found bool
	if err := sqlitex.Execute(t.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = t.scanTask(stmt)
				found = true
				return err
			},
		}); err != nil {
		return Task{}, fmt.Errorf("updateTask re-fetch %q: %w", id.String(), err)
	}
	if !found {
		return Task{}, fmt.Errorf("%w: task %q not found after update", ErrNotFound, id.String())
	}
	return task, nil
}

// closeTask closes a task. Acquires t.mu.
func (t *sqliteTracker) closeTask(id TaskID, reason string, now time.Time) (Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := sqlitex.Execute(t.conn,
		`UPDATE tasks SET status_id = 2, close_reason = ?2, closed_at = ?3, updated_at = ?4 WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{id.String(), reason, now.UnixNano(), now.UnixNano()}}); err != nil {
		return Task{}, fmt.Errorf("closeTask %q: %w", id.String(), err)
	}

	var task Task
	var found bool
	if err := sqlitex.Execute(t.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = t.scanTask(stmt)
				found = true
				return err
			},
		}); err != nil {
		return Task{}, fmt.Errorf("closeTask re-fetch %q: %w", id.String(), err)
	}
	if !found {
		return Task{}, fmt.Errorf("%w: task %q not found after close", ErrNotFound, id.String())
	}
	return task, nil
}

// listTasks returns tasks matching filter. Acquires t.mu.
func (t *sqliteTracker) listTasks(filter ListFilter) ([]Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	query := `SELECT id, namespace, title, description, status_id, priority_id, type_id,
	                 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
	          FROM tasks WHERE 1=1`
	var args []any
	idx := 1

	if filter.Status != nil {
		query += fmt.Sprintf(" AND status_id = ?%d", idx)
		args = append(args, int(*filter.Status))
		idx++
	}
	if filter.Priority != nil {
		query += fmt.Sprintf(" AND priority_id = ?%d", idx)
		args = append(args, int(*filter.Priority))
		idx++
	}
	if filter.Type != nil {
		query += fmt.Sprintf(" AND type_id = ?%d", idx)
		args = append(args, int(*filter.Type))
		idx++
	}
	if filter.Phase != nil {
		query += fmt.Sprintf(" AND phase_id = ?%d", idx)
		args = append(args, int(*filter.Phase))
		idx++
	}
	if filter.Namespace != "" {
		query += fmt.Sprintf(" AND namespace = ?%d", idx)
		args = append(args, filter.Namespace)
		idx++
	}
	if filter.Label != "" {
		query += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM labels l WHERE l.task_id = tasks.id AND l.name = ?%d)", idx,
		)
		args = append(args, filter.Label)
		idx++
	}
	_ = idx
	query += " ORDER BY created_at ASC"

	var tasks []Task
	err := sqlitex.Execute(t.conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			task, err := t.scanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("listTasks: %w", err)
	}
	return tasks, nil
}

// readyTasks returns tasks with no open blockers. Acquires t.mu.
func (t *sqliteTracker) readyTasks() ([]Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	const query = `
		SELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,
		       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,
		       t.closed_at, t.close_reason
		FROM tasks t
		WHERE t.status_id != 2
		AND NOT EXISTS (
			SELECT 1 FROM edges e
			JOIN tasks blocker ON e.target_id = blocker.id
			WHERE e.source_id = t.id AND e.kind_id = 0 AND blocker.status_id != 2
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`

	var tasks []Task
	err := sqlitex.Execute(t.conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			task, err := t.scanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("readyTasks: %w", err)
	}
	return tasks, nil
}

// blockedTasks returns tasks with at least one open blocker. Acquires t.mu.
func (t *sqliteTracker) blockedTasks() ([]Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	const query = `
		SELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,
		       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,
		       t.closed_at, t.close_reason
		FROM tasks t
		WHERE t.status_id != 2
		AND EXISTS (
			SELECT 1 FROM edges e
			JOIN tasks blocker ON e.target_id = blocker.id
			WHERE e.source_id = t.id AND e.kind_id = 0 AND blocker.status_id != 2
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`

	var tasks []Task
	err := sqlitex.Execute(t.conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			task, err := t.scanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("blockedTasks: %w", err)
	}
	return tasks, nil
}

// insertEdge inserts an edge. Acquires t.mu.
func (t *sqliteTracker) insertEdge(sourceID TaskID, targetID string, kind EdgeKind, now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return sqlitex.Execute(t.conn,
		`INSERT OR IGNORE INTO edges (source_id, target_id, kind_id, created_at) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{sourceID.String(), targetID, int(kind), now.UnixNano()}})
}

// deleteEdge deletes an edge. Acquires t.mu.
func (t *sqliteTracker) deleteEdge(sourceID TaskID, targetID string, kind EdgeKind) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return sqlitex.Execute(t.conn,
		`DELETE FROM edges WHERE source_id = ?1 AND target_id = ?2 AND kind_id = ?3`,
		&sqlitex.ExecOptions{Args: []any{sourceID.String(), targetID, int(kind)}})
}

// getEdges returns edges from sourceID, optionally filtered by kind. Acquires t.mu.
func (t *sqliteTracker) getEdges(sourceID TaskID, kind *EdgeKind) ([]Edge, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	query := `SELECT source_id, target_id, kind_id FROM edges WHERE source_id = ?1`
	args := []any{sourceID.String()}
	if kind != nil {
		query += " AND kind_id = ?2"
		args = append(args, int(*kind))
	}
	query += " ORDER BY created_at ASC"

	var edges []Edge
	err := sqlitex.Execute(t.conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			edges = append(edges, Edge{
				SourceID: stmt.ColumnText(0),
				TargetID: stmt.ColumnText(1),
				Kind:     EdgeKind(stmt.ColumnInt(2)),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getEdges %q: %w", sourceID.String(), err)
	}
	return edges, nil
}

// getBlockedByEdges returns all EdgeBlockedBy edges. Acquires t.mu.
func (t *sqliteTracker) getBlockedByEdges() ([]Edge, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var edges []Edge
	err := sqlitex.Execute(t.conn,
		`SELECT source_id, target_id, kind_id FROM edges WHERE kind_id = 0 ORDER BY created_at ASC`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *zs.Stmt) error {
				edges = append(edges, Edge{
					SourceID: stmt.ColumnText(0),
					TargetID: stmt.ColumnText(1),
					Kind:     EdgeBlockedBy,
				})
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("getBlockedByEdges: %w", err)
	}
	return edges, nil
}

// getDepTree returns all blocked-by edges reachable from rootID via DFS.
// Acquires t.mu.
func (t *sqliteTracker) getDepTree(rootID TaskID) ([]Edge, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	adj := make(map[string][]string)
	if err := sqlitex.Execute(t.conn,
		`SELECT source_id, target_id FROM edges WHERE kind_id = 0`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *zs.Stmt) error {
				src := stmt.ColumnText(0)
				tgt := stmt.ColumnText(1)
				adj[src] = append(adj[src], tgt)
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("getDepTree: %w", err)
	}

	var result []Edge
	visited := make(map[string]bool)
	var dfs func(srcID string)
	dfs = func(srcID string) {
		for _, tgtID := range adj[srcID] {
			result = append(result, Edge{SourceID: srcID, TargetID: tgtID, Kind: EdgeBlockedBy})
			if !visited[tgtID] {
				visited[tgtID] = true
				dfs(tgtID)
			}
		}
	}
	visited[rootID.String()] = true
	dfs(rootID.String())
	return result, nil
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

func (t *sqliteTracker) scanTask(stmt *zs.Stmt) (Task, error) {
	idStr := stmt.ColumnText(0)
	id, err := ParseTaskID(idStr)
	if err != nil {
		return Task{}, fmt.Errorf("scanTask: invalid task ID %q: %w", idStr, err)
	}

	var ownerID *AgentID
	if !stmt.ColumnIsNull(8) {
		aid, err := ParseAgentID(stmt.ColumnText(8))
		if err != nil {
			return Task{}, fmt.Errorf("scanTask: invalid owner_id %q: %w", stmt.ColumnText(8), err)
		}
		ownerID = &aid
	}

	createdAt := time.Unix(0, stmt.ColumnInt64(10)).UTC()
	updatedAt := time.Unix(0, stmt.ColumnInt64(11)).UTC()

	var closedAt *time.Time
	if !stmt.ColumnIsNull(12) {
		ct := time.Unix(0, stmt.ColumnInt64(12)).UTC()
		closedAt = &ct
	}

	return Task{
		ID:          id,
		Title:       stmt.ColumnText(2),
		Description: stmt.ColumnText(3),
		Status:      Status(stmt.ColumnInt(4)),
		Priority:    Priority(stmt.ColumnInt(5)),
		Type:        TaskType(stmt.ColumnInt(6)),
		Phase:       Phase(stmt.ColumnInt(7)),
		Owner:       ownerID,
		Notes:       stmt.ColumnText(9),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ClosedAt:    closedAt,
		CloseReason: stmt.ColumnText(13),
	}, nil
}

func (t *sqliteTracker) scanActivity(stmt *zs.Stmt) (Activity, error) {
	idStr := stmt.ColumnText(0)
	id, err := ParseActivityID(idStr)
	if err != nil {
		return Activity{}, fmt.Errorf("scanActivity: invalid activity ID %q: %w", idStr, err)
	}

	agentIDStr := stmt.ColumnText(1)
	agentID, err := ParseAgentID(agentIDStr)
	if err != nil {
		return Activity{}, fmt.Errorf("scanActivity: invalid agent_id %q: %w", agentIDStr, err)
	}

	startedAt := time.Unix(0, stmt.ColumnInt64(4)).UTC()
	var endedAt *time.Time
	if !stmt.ColumnIsNull(5) {
		et := time.Unix(0, stmt.ColumnInt64(5)).UTC()
		endedAt = &et
	}

	return Activity{
		ID:        id,
		AgentID:   agentID,
		Phase:     Phase(stmt.ColumnInt(2)),
		Stage:     Stage(stmt.ColumnInt(3)),
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Notes:     stmt.ColumnText(6),
	}, nil
}

func (t *sqliteTracker) scanComment(stmt *zs.Stmt) (Comment, error) {
	idStr := stmt.ColumnText(0)
	id, err := ParseCommentID(idStr)
	if err != nil {
		return Comment{}, fmt.Errorf("scanComment: invalid comment ID %q: %w", idStr, err)
	}
	taskIDStr := stmt.ColumnText(1)
	taskID, err := ParseTaskID(taskIDStr)
	if err != nil {
		return Comment{}, fmt.Errorf("scanComment: invalid task_id %q: %w", taskIDStr, err)
	}
	authorIDStr := stmt.ColumnText(2)
	authorID, err := ParseAgentID(authorIDStr)
	if err != nil {
		return Comment{}, fmt.Errorf("scanComment: invalid author_id %q: %w", authorIDStr, err)
	}
	return Comment{
		ID:        id,
		TaskID:    taskID,
		AuthorID:  authorID,
		Body:      stmt.ColumnText(3),
		CreatedAt: time.Unix(0, stmt.ColumnInt64(4)).UTC(),
	}, nil
}

// timeToNullInt converts *time.Time to a nullable int64 value for SQLite.
func timeToNullInt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}
