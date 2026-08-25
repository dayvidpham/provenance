# PROPOSAL-1: Providence Architecture and Implementation Plan

> **Historical record — not current guidance.** This proposal is superseded and
> kept for the audit trail; its text is deliberately not rewritten. In
> particular, its SQLite driver decision (`zombiezen.com/go/sqlite`) no longer
> holds: persistence now uses `database/sql` over `modernc.org/sqlite`, and
> `internal/sqlite/sql_architecture_test.go` fails if zombiezen is reintroduced
> there. See `CLAUDE.md` and `docs/architecture.md` for the current rules.


**Beads ID:** aura-plugins-z24dr
**References:**
- REQUEST: aura-plugins-oviik
- URD: aura-plugins-f85gw (comments contain URE rounds 1-4)
- ELICIT: aura-plugins-4fejd
- EPIC: aura-plugins-t7pyf

---

## 1. Problem Space

Providence replaces Beads (bd) as the task dependency tracker for the Aura Protocol agent system. The fundamental problem is tracking work products, their dependencies, and their provenance across multi-agent planning and implementation workflows.

### Axes of the Problem

| Axis | Assessment |
|------|-----------|
| **Parallelism** | Low at storage layer (single SQLite writer), moderate at read layer (concurrent graph queries from multiple agents). WAL mode handles this. |
| **Distribution** | Single-process, single-machine. SQLite file on local disk. No network protocol needed. |
| **Scale** | < 10,000 tasks. In-memory graph operations are microsecond-scale at this size. |
| **Entity relationships** | Has-a: Task has Labels, has Comments, has Edges. Is-a: Agent, Activity, Entity are all PROV-O nodes with distinct schemas. |
| **Domain novelty** | Medium. W3C PROV-O is a well-defined ontology; mapping it to Go types and SQLite is straightforward but the 6-edge-kind model with selective readiness semantics requires careful design. |

### Key Design Constraint

Only `EdgeBlockedBy` affects task readiness. The other 5 edge kinds (`EdgeDerivedFrom`, `EdgeSupersedes`, `EdgeDiscoveredFrom`, `EdgeGeneratedBy`, `EdgeAttributedTo`) are provenance metadata that enriches the audit trail without affecting scheduling. This means the graph library's cycle prevention and topological sort operate only on the blocked-by subgraph, while all 6 edge kinds are persisted in the same SQLite table.

---

## 2. Module Layout

```
github.com/dayvidpham/provenance/
├── providence.go           # Package doc + public facade (Tracker type + constructors)
├── types.go                # All public types: TaskID, AgentID, ActivityID, Task, Agent, Activity, Edge, etc.
├── enums.go                # All iota enums: Status, Priority, TaskType, EdgeKind, Role, Model, Phase, Stage
├── errors.go               # Sentinel errors and error constructors
├── tracker.go              # Tracker methods: CRUD, edges, labels, comments, queries
├── tracker_test.go         # Integration tests for Tracker (black-box, package providence_test)
├── internal/
│   ├── graph/
│   │   ├── store.go        # dominikbraun/graph Store[TaskID, Task] implementation backed by SQLite
│   │   └── store_test.go   # Store implementation tests
│   ├── sqlite/
│   │   ├── db.go           # Database open/close, WAL config, schema migration
│   │   ├── schema.go       # CREATE TABLE statements, indexes, migrations
│   │   ├── tasks.go        # Task CRUD SQL operations
│   │   ├── edges.go        # Edge insert/delete/query SQL operations
│   │   ├── agents.go       # Agent CRUD SQL operations
│   │   ├── activities.go   # Activity CRUD SQL operations
│   │   ├── labels.go       # Label add/remove/query SQL operations
│   │   ├── comments.go     # Comment add/query SQL operations
│   │   └── db_test.go      # SQLite integration tests
│   └── helpers/
│       ├── ancestors.go    # Ancestors/Descendants composed from DFS + PredecessorMap
│       └── ancestors_test.go
├── docs/
│   └── PROPOSAL-1.md       # This document
├── go.mod
├── go.sum
├── LICENSE
├── .gitignore
└── Makefile                # fmt, lint, test, build targets
```

### Package Responsibilities

| Package | Role |
|---------|------|
| `providence` (root) | Public API surface. All exported types, the `Tracker` facade, and constructors. Consumers (e.g., pasture) import only this package. |
| `internal/sqlite` | All SQL operations. Encapsulates the zombiezen SQLite driver. No graph logic here — pure relational CRUD. |
| `internal/graph` | Implements `dominikbraun/graph.Store[TaskID, Task]` backed by `internal/sqlite`. Bridges graph library and persistence. |
| `internal/helpers` | Graph traversal utilities (Ancestors, Descendants) composed from dominikbraun/graph primitives. |

---

## 3. Type Definitions

### 3.1 ID Types

All three PROV-O entity types get distinct ID types for compile-time safety. Each follows the same structure: `{Namespace}--{UUIDv7}`.

```go
// TaskID uniquely identifies a task (PROV-O Entity).
// The Namespace scopes the ID to a project (e.g., "aura-plugins").
// The UUID is a UUIDv7 (time-sortable, globally unique).
type TaskID struct {
    Namespace string
    UUID      uuid.UUID
}

// String returns the wire format: "namespace--uuid".
func (id TaskID) String() string {
    return id.Namespace + "--" + id.UUID.String()
}

// ParseTaskID parses "namespace--uuid" into a TaskID.
// Returns an error if the format is invalid or the UUID is malformed.
func ParseTaskID(s string) (TaskID, error) { ... }

// AgentID uniquely identifies an agent (PROV-O Agent).
type AgentID struct {
    Namespace string
    UUID      uuid.UUID
}

// ActivityID uniquely identifies an activity (PROV-O Activity).
type ActivityID struct {
    Namespace string
    UUID      uuid.UUID
}

// CommentID uniquely identifies a comment.
type CommentID struct {
    Namespace string
    UUID      uuid.UUID
}
```

All four ID types follow the same `{Namespace}--{UUIDv7}` wire format with `String()` and `Parse*()` methods.

**Graph hash function:** dominikbraun/graph requires a `Hash` function `func(TaskID) string`. This is simply `TaskID.String()`.

### 3.2 Enum Types

All enums use iota with the following methods: `String()`, `MarshalText()`, `UnmarshalText()`, `IsValid()`.

```go
// Status represents the lifecycle state of a task.
type Status int

const (
    StatusOpen       Status = iota // Task is created but not yet started
    StatusInProgress               // Work is actively happening
    StatusClosed                   // Work is complete
)

// Priority represents task urgency (0 = critical, 4 = backlog).
type Priority int

const (
    PriorityCritical Priority = iota // 0: security, data loss, broken builds
    PriorityHigh                     // 1: major features, important bugs
    PriorityMedium                   // 2: default
    PriorityLow                      // 3: polish, optimization
    PriorityBacklog                  // 4: future ideas
)

// TaskType classifies the kind of work.
type TaskType int

const (
    TaskTypeBug     TaskType = iota // Something broken
    TaskTypeFeature                 // New functionality
    TaskTypeTask                    // Work item (tests, docs, refactoring)
    TaskTypeEpic                    // Large feature with subtasks
    TaskTypeChore                   // Maintenance (dependencies, tooling)
)

// EdgeKind classifies the relationship between two tasks.
type EdgeKind int

const (
    EdgeBlockedBy      EdgeKind = iota // Affects task readiness
    EdgeDerivedFrom                    // PROPOSAL-2 derived from PROPOSAL-1
    EdgeSupersedes                     // PROPOSAL-3 supersedes PROPOSAL-2
    EdgeDiscoveredFrom                 // Found during work on parent
    EdgeGeneratedBy                    // Which activity produced this
    EdgeAttributedTo                   // Which agent owns this
)

// Role identifies an agent's role in the protocol.
type Role int

const (
    RoleHuman      Role = iota // Human user
    RoleArchitect              // Architect agent
    RoleSupervisor             // Supervisor agent
    RoleWorker                 // Worker agent
    RoleReviewer               // Reviewer agent
)

// Model identifies the AI model used by an agent.
type Model int

const (
    ModelNone          Model = iota // Human (no model)
    ModelClaudeOpus4                // Claude Opus 4
    ModelClaudeSonnet4              // Claude Sonnet 4
    ModelClaudeHaiku4               // Claude Haiku 4
    // Future models added here
)

// Phase identifies a phase in the epoch lifecycle.
type Phase int

const (
    PhaseRequest     Phase = iota // p1
    PhaseElicit                   // p2
    PhasePropose                  // p3
    PhaseReview                   // p4
    PhasePlanUAT                  // p5
    PhaseRatify                   // p6
    PhaseHandoff                  // p7
    PhaseImplPlan                 // p8
    PhaseWorkerSlices             // p9
    PhaseCodeReview               // p10
    PhaseImplUAT                  // p11
    PhaseLanding                  // p12
)

// Stage captures fine-grained progress within a phase.
type Stage int

const (
    StageNotStarted Stage = iota
    StageInProgress
    StageBlocked
    StageComplete
)
```

Each enum type implements the full interface:

```go
// Example for Status — same pattern applies to all enums.

var statusStrings = [...]string{
    StatusOpen:       "open",
    StatusInProgress: "in_progress",
    StatusClosed:     "closed",
}

func (s Status) String() string {
    if int(s) < len(statusStrings) {
        return statusStrings[s]
    }
    return fmt.Sprintf("Status(%d)", int(s))
}

func (s Status) MarshalText() ([]byte, error) {
    if !s.IsValid() {
        return nil, fmt.Errorf("providence: cannot marshal invalid Status(%d)", int(s))
    }
    return []byte(s.String()), nil
}

func (s *Status) UnmarshalText(b []byte) error {
    text := string(b)
    for i, name := range statusStrings {
        if name == text {
            *s = Status(i)
            return nil
        }
    }
    return fmt.Errorf(
        "providence: unknown Status %q — valid values: %v",
        text, statusStrings[:],
    )
}

func (s Status) IsValid() bool {
    return s >= StatusOpen && s <= StatusClosed
}
```

### 3.3 Entity Types

```go
// Task represents a work product (PROV-O Entity).
type Task struct {
    ID          TaskID    `json:"id"`
    Title       string    `json:"title"`
    Description string    `json:"description"`
    Status      Status    `json:"status"`
    Priority    Priority  `json:"priority"`
    Type        TaskType  `json:"type"`
    Owner       *AgentID  `json:"owner,omitempty"` // nil if unassigned
    Notes       string    `json:"notes,omitempty"`
    CreatedAt   time.Time `json:"createdAt"`
    UpdatedAt   time.Time `json:"updatedAt"`
    ClosedAt    *time.Time `json:"closedAt,omitempty"`
    CloseReason string    `json:"closeReason,omitempty"`
}

// Agent represents a human or AI participant (PROV-O Agent).
type Agent struct {
    ID    AgentID   `json:"id"`
    Name  string    `json:"name"`
    Role  Role      `json:"role"`
    Model Model     `json:"model"` // ModelNone for humans
}

// Activity represents a recorded action (PROV-O Activity).
type Activity struct {
    ID        ActivityID `json:"id"`
    AgentID   AgentID    `json:"agentId"`
    Phase     Phase      `json:"phase"`
    Stage     Stage      `json:"stage"`
    StartedAt time.Time  `json:"startedAt"`
    EndedAt   *time.Time `json:"endedAt,omitempty"`
    Notes     string     `json:"notes,omitempty"`
}

// Edge represents a typed relationship between two tasks.
type Edge struct {
    Source TaskID   `json:"source"`
    Target TaskID   `json:"target"`
    Kind   EdgeKind `json:"kind"`
}

// Label is a string tag attached to a task.
type Label struct {
    TaskID TaskID `json:"taskId"`
    Name   string `json:"name"`
}

// Comment is a timestamped note attached to a task.
type Comment struct {
    ID        CommentID `json:"id"`
    TaskID    TaskID    `json:"taskId"`
    AuthorID  AgentID   `json:"authorId"`
    Body      string    `json:"body"`
    CreatedAt time.Time `json:"createdAt"`
}
```

---

## 4. Public API Surface

The public API is exposed through a single `Tracker` type that owns the graph and the database connection. All methods are on `*Tracker`.

```go
// Tracker is the central API for providence task management.
// It owns a dominikbraun/graph instance backed by SQLite persistence
// and provides all CRUD, dependency, label, comment, and query operations.
//
// A Tracker must be created via Open and closed via Close.
type Tracker struct {
    db    *internal/sqlite.DB
    graph graph.Graph[TaskID, Task]
}

// --- Constructors ---

// Open creates a new Tracker backed by a SQLite database at dbPath.
// Parent directories are created if they do not exist.
// The database schema is applied via migrations on first open.
//
// The default dbPath follows XDG: ~/.local/share/pasture/providence.db
func Open(dbPath string) (*Tracker, error)

// Close releases the database connection and all resources.
func (t *Tracker) Close() error

// --- Task CRUD ---

// Create creates a new task with the given fields.
// Assigns a UUIDv7 and sets CreatedAt/UpdatedAt to now.
// Returns the created Task (with ID populated).
func (t *Tracker) Create(namespace, title, description string, taskType TaskType, priority Priority) (Task, error)

// Show returns a task by ID.
// Returns ErrNotFound if the task does not exist.
func (t *Tracker) Show(id TaskID) (Task, error)

// Update modifies mutable fields of an existing task.
// Only non-nil fields in the UpdateFields struct are applied.
// Returns the updated Task.
func (t *Tracker) Update(id TaskID, fields UpdateFields) (Task, error)

// CloseTask marks a task as closed with the given reason.
// Sets ClosedAt to now and Status to StatusClosed.
// Returns ErrNotFound if the task does not exist.
// Returns ErrAlreadyClosed if the task is already closed.
func (t *Tracker) CloseTask(id TaskID, reason string) (Task, error)

// List returns all tasks matching the given filter.
// An empty filter returns all tasks.
func (t *Tracker) List(filter ListFilter) ([]Task, error)

// --- UpdateFields ---

// UpdateFields specifies which task fields to modify.
// Nil pointer fields are not modified.
type UpdateFields struct {
    Title       *string
    Description *string
    Status      *Status
    Priority    *Priority
    Owner       *AgentID
    Notes       *string
}

// --- ListFilter ---

// ListFilter specifies criteria for listing tasks.
// Zero-value fields are ignored (no filter on that field).
type ListFilter struct {
    Status    *Status
    Priority  *Priority
    Type      *TaskType
    Label     string // empty means no label filter
    Namespace string // empty means all namespaces
}

// --- Typed Dependency Edges ---

// AddEdge creates a typed relationship between two tasks.
// For EdgeBlockedBy, returns ErrCycleDetected if the edge would create a cycle
// in the blocked-by subgraph.
// Returns ErrNotFound if either task does not exist.
func (t *Tracker) AddEdge(source, target TaskID, kind EdgeKind) error

// RemoveEdge removes a typed relationship between two tasks.
// Returns ErrNotFound if the edge does not exist.
func (t *Tracker) RemoveEdge(source, target TaskID, kind EdgeKind) error

// Edges returns all edges of the given kind involving the task.
// If kind is nil, returns edges of all kinds.
func (t *Tracker) Edges(id TaskID, kind *EdgeKind) ([]Edge, error)

// --- Readiness Queries (blocked-by subgraph only) ---

// Blocked returns tasks that have at least one open blocked-by dependency.
func (t *Tracker) Blocked() ([]Task, error)

// Ready returns tasks that are open and have no open blocked-by dependencies.
func (t *Tracker) Ready() ([]Task, error)

// DepTree returns the blocked-by dependency tree rooted at the given task.
// Returns a list of edges in the tree (depth-first order).
func (t *Tracker) DepTree(id TaskID) ([]Edge, error)

// Ancestors returns all tasks that transitively block the given task
// (following blocked-by edges backward).
func (t *Tracker) Ancestors(id TaskID) ([]Task, error)

// Descendants returns all tasks that are transitively blocked by the given task
// (following blocked-by edges forward).
func (t *Tracker) Descendants(id TaskID) ([]Task, error)

// --- Labels ---

// AddLabel attaches a label to a task. Idempotent.
func (t *Tracker) AddLabel(id TaskID, label string) error

// RemoveLabel detaches a label from a task. Idempotent.
func (t *Tracker) RemoveLabel(id TaskID, label string) error

// Labels returns all labels attached to a task.
func (t *Tracker) Labels(id TaskID) ([]string, error)

// --- Comments ---

// AddComment adds a timestamped comment to a task.
// Assigns a UUIDv7 CommentID and sets CreatedAt to now.
// Returns the created Comment (with ID populated).
// Returns ErrNotFound if the task or author agent does not exist.
func (t *Tracker) AddComment(id TaskID, authorID AgentID, body string) (Comment, error)

// Comments returns all comments on a task in chronological order.
func (t *Tracker) Comments(id TaskID) ([]Comment, error)

// --- PROV-O Agents ---

// RegisterAgent creates or updates an agent record.
func (t *Tracker) RegisterAgent(namespace, name string, role Role, model Model) (Agent, error)

// Agent returns an agent by ID.
func (t *Tracker) Agent(id AgentID) (Agent, error)

// --- PROV-O Activities ---

// StartActivity records the beginning of an activity.
func (t *Tracker) StartActivity(agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error)

// EndActivity records the completion of an activity.
func (t *Tracker) EndActivity(id ActivityID) (Activity, error)

// Activities returns all activities, optionally filtered by agent.
func (t *Tracker) Activities(agentID *AgentID) ([]Activity, error)
```

### 4.1 Sentinel Errors

```go
var (
    // ErrNotFound is returned when a requested entity does not exist.
    ErrNotFound = errors.New("providence: entity not found")

    // ErrCycleDetected is returned when adding a blocked-by edge would
    // create a cycle in the dependency graph.
    ErrCycleDetected = errors.New("providence: dependency cycle detected")

    // ErrAlreadyClosed is returned when attempting to close an already-closed task.
    ErrAlreadyClosed = errors.New("providence: task is already closed")

    // ErrInvalidID is returned when a string cannot be parsed as a valid ID.
    ErrInvalidID = errors.New("providence: invalid ID format")
)
```

---

## 5. Graph Integration

### 5.1 dominikbraun/graph Configuration

```go
// In internal/graph/store.go

// NewBlockedByGraph creates a directed, cycle-preventing graph for blocked-by
// edges. The graph is keyed by TaskID and stores Task values.
//
// Cycle prevention is applied at the graph library level via PreventCycles().
// This means AddEdge returns graph.ErrEdgeCreatesCycle immediately if the
// proposed edge would introduce a cycle — no separate cycle check needed.
func NewBlockedByGraph(store graph.Store[TaskID, Task]) graph.Graph[TaskID, Task] {
    return graph.NewWithStore(
        func(id TaskID) string { return id.String() },
        store,
        graph.Directed(),
        graph.PreventCycles(),
    )
}
```

### 5.2 Store Implementation

The `graph.Store[K, T]` interface from dominikbraun/graph provides these methods that must be implemented:

```go
type Store[K comparable, T any] interface {
    AddVertex(hash K, value T, properties VertexProperties) error
    Vertex(hash K) (T, VertexProperties, error)
    RemoveVertex(hash K) error
    ListVertices() ([]K, error)
    VertexCount() (int, error)

    AddEdge(sourceHash, targetHash K, edge Edge[K]) error
    Edge(sourceHash, targetHash K) (Edge[K], error)
    RemoveEdge(sourceHash, targetHash K) error

    // For PredecessorMap/AdjacencyMap traversal:
    // These return the full adjacency and predecessor maps.
}
```

Our `internal/graph/store.go` implements this interface backed by `internal/sqlite`. The Store delegates to SQLite queries for all operations. The dominikbraun/graph library calls these methods transparently — it never directly touches SQLite.

### 5.3 Blocked-By Subgraph vs. Full Edge Set

The dominikbraun/graph instance manages **only blocked-by edges** for cycle prevention and graph queries. The other 5 edge kinds are stored directly in SQLite (same `edges` table) but are NOT loaded into the graph library. This is because:

1. Cycle prevention only matters for blocked-by (you cannot have a circular dependency).
2. Topological sort only makes sense for blocked-by (execution ordering).
3. The other edge kinds are metadata queries ("what was this derived from?") that are simple SQL lookups.

### 5.4 Ancestors and Descendants

```go
// In internal/helpers/ancestors.go

// Ancestors returns all tasks that transitively block the given task by
// following blocked-by edges backward (via PredecessorMap).
func Ancestors(g graph.Graph[TaskID, Task], id TaskID) ([]TaskID, error) {
    predecessors, err := g.PredecessorMap()
    if err != nil {
        return nil, err
    }
    var result []TaskID
    visited := make(map[string]bool)
    var dfs func(current string)
    dfs = func(current string) {
        for pred := range predecessors[current] {
            if !visited[pred] {
                visited[pred] = true
                // Parse pred back to TaskID
                tid, _ := ParseTaskID(pred)
                result = append(result, tid)
                dfs(pred)
            }
        }
    }
    dfs(id.String())
    return result, nil
}

// Descendants follows the AdjacencyMap forward in the same pattern.
func Descendants(g graph.Graph[TaskID, Task], id TaskID) ([]TaskID, error) { ... }
```

---

## 6. SQLite Schema

### 6.1 Driver

Providence uses `zombiezen.com/go/sqlite` (GitHub: `github.com/zombiezen/go-sqlite`), a pure-Go SQLite implementation. This is different from pasture's `modernc.org/sqlite` — the choice was made in URE round 4 because zombiezen's API is lower-level and more suitable for the Store interface pattern where we want explicit connection management rather than `database/sql` pooling.

**Important:** zombiezen SQLite does NOT use `database/sql`. It has its own connection API:

```go
conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenWAL)
```

### 6.2 BCNF Design

The schema is in Boyce-Codd Normal Form: every non-trivial functional dependency has a superkey as its determinant. Enum types are first-class reference tables with foreign key constraints enforced at the database level.

**Redundancy note:** `tasks.namespace` is derivable from `tasks.id` (the prefix before `--`). It is stored as a denormalized column for efficient `WHERE namespace = ?` queries. Since `id → namespace` and `id` is a superkey, this does not violate BCNF — it is a materialized deterministic function of the key.

### 6.3 Reference Tables (Enum Types)

Each iota enum has a corresponding lookup table following the pattern from agent-data-leverage: `(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`. Seed data is inserted on every `Open()` via `INSERT OR IGNORE`.

```sql
-- Enum lookup tables (same pattern as agent-data-leverage V16)
CREATE TABLE statuses (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE priorities (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE task_types (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE edge_kinds (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE roles (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE models (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE phases (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE stages (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;
```

#### Seed Data

```sql
INSERT OR IGNORE INTO statuses (id, name) VALUES
    (0, 'open'), (1, 'in_progress'), (2, 'closed');

INSERT OR IGNORE INTO priorities (id, name) VALUES
    (0, 'critical'), (1, 'high'), (2, 'medium'), (3, 'low'), (4, 'backlog');

INSERT OR IGNORE INTO task_types (id, name) VALUES
    (0, 'bug'), (1, 'feature'), (2, 'task'), (3, 'epic'), (4, 'chore');

INSERT OR IGNORE INTO edge_kinds (id, name) VALUES
    (0, 'blocked_by'), (1, 'derived_from'), (2, 'supersedes'),
    (3, 'discovered_from'), (4, 'generated_by'), (5, 'attributed_to');

INSERT OR IGNORE INTO roles (id, name) VALUES
    (0, 'human'), (1, 'architect'), (2, 'supervisor'), (3, 'worker'), (4, 'reviewer');

INSERT OR IGNORE INTO models (id, name) VALUES
    (0, 'none'), (1, 'claude_opus_4'), (2, 'claude_sonnet_4'), (3, 'claude_haiku_4');

INSERT OR IGNORE INTO phases (id, name) VALUES
    (0, 'request'), (1, 'elicit'), (2, 'propose'), (3, 'review'),
    (4, 'plan_uat'), (5, 'ratify'), (6, 'handoff'), (7, 'impl_plan'),
    (8, 'worker_slices'), (9, 'code_review'), (10, 'impl_uat'), (11, 'landing');

INSERT OR IGNORE INTO stages (id, name) VALUES
    (0, 'not_started'), (1, 'in_progress'), (2, 'blocked'), (3, 'complete');
```

### 6.4 Entity Tables

All entity tables use `STRICT` mode and UUIDv7 TEXT primary keys (no autoincrement integers). FD annotations inline.

```sql
-- FD: id → all columns (id is PK)
-- Agents (PROV-O Agent) — must be created before tasks (owner FK)
CREATE TABLE agents (
    id        TEXT PRIMARY KEY,  -- AgentID.String() ("namespace--uuidv7")
    namespace TEXT NOT NULL,
    name      TEXT NOT NULL,
    role_id   INTEGER NOT NULL REFERENCES roles(id),
    model_id  INTEGER NOT NULL DEFAULT 0 REFERENCES models(id)
) STRICT;

-- FD: id → all columns (id is PK)
-- Tasks (PROV-O Entity)
CREATE TABLE tasks (
    id           TEXT PRIMARY KEY,  -- TaskID.String() ("namespace--uuidv7")
    namespace    TEXT NOT NULL,     -- materialized from id for query efficiency
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    status_id    INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),
    priority_id  INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),
    type_id      INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),
    owner_id     TEXT REFERENCES agents(id),  -- NULL if unassigned
    notes        TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,  -- Unix nanoseconds UTC
    updated_at   INTEGER NOT NULL,  -- Unix nanoseconds UTC
    closed_at    INTEGER,           -- NULL if open
    close_reason TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX idx_tasks_namespace ON tasks (namespace);
CREATE INDEX idx_tasks_status ON tasks (status_id);
CREATE INDEX idx_tasks_priority ON tasks (priority_id);
CREATE INDEX idx_tasks_type ON tasks (type_id);
CREATE INDEX idx_tasks_owner ON tasks (owner_id);

-- FD: (source_id, target_id, kind_id) → created_at (composite PK)
-- Edges (all 6 kinds in one table)
CREATE TABLE edges (
    source_id  TEXT NOT NULL REFERENCES tasks(id),
    target_id  TEXT NOT NULL REFERENCES tasks(id),
    kind_id    INTEGER NOT NULL REFERENCES edge_kinds(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (source_id, target_id, kind_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_edges_source ON edges (source_id);
CREATE INDEX idx_edges_target ON edges (target_id);
CREATE INDEX idx_edges_kind ON edges (kind_id);

-- FD: id → all columns (id is PK)
-- Activities (PROV-O Activity)
CREATE TABLE activities (
    id         TEXT PRIMARY KEY,  -- ActivityID.String() ("namespace--uuidv7")
    agent_id   TEXT NOT NULL REFERENCES agents(id),
    phase_id   INTEGER NOT NULL REFERENCES phases(id),
    stage_id   INTEGER NOT NULL REFERENCES stages(id),
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    notes      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX idx_activities_agent ON activities (agent_id);
CREATE INDEX idx_activities_phase ON activities (phase_id);

-- FD: (task_id, name) → ∅ (composite PK, no non-key attributes)
-- Labels
CREATE TABLE labels (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    name    TEXT NOT NULL,
    PRIMARY KEY (task_id, name)
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_labels_name ON labels (name);

-- FD: id → all columns (id is PK)
-- Comments — UUIDv7 PK (no autoincrement)
CREATE TABLE comments (
    id         TEXT PRIMARY KEY,  -- CommentID.String() ("namespace--uuidv7")
    task_id    TEXT NOT NULL REFERENCES tasks(id),
    author_id  TEXT NOT NULL REFERENCES agents(id),
    body       TEXT NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_comments_task ON comments (task_id);
CREATE INDEX idx_comments_author ON comments (author_id);
```

### 6.5 Schema Migration Strategy

For MVP, use a single `ensureSchema()` function that runs `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` statements. This is idempotent and safe to run on every `Open()`.

Post-MVP, if schema changes are needed, add a `schema_version` pragma and numbered migration functions. But do not build migration infrastructure until it is needed.

### 6.6 WAL Mode and Concurrency

```go
// Applied on every Open():
PRAGMA journal_mode=WAL;    // concurrent readers, single writer
PRAGMA busy_timeout=5000;   // retry on SQLITE_BUSY for up to 5 seconds
PRAGMA foreign_keys=ON;     // enforce referential integrity on edges
```

---

## 7. Dependency Resolution

### 7.1 Readiness Algorithm

A task is "ready" if:
1. Its status is `StatusOpen` (not closed or in-progress — though in-progress tasks may also be considered "claimed but active"; the key filter is "not closed").
2. It has **zero open blocked-by predecessors**. That is: for every `EdgeBlockedBy` edge pointing to this task, the source task must be `StatusClosed`.

```go
// Ready returns all tasks that are open and have no open blockers.
func (t *Tracker) Ready() ([]Task, error) {
    // 1. Get all open tasks from SQLite.
    // 2. For each open task, query blocked-by edges where this task is the target.
    // 3. For each blocker, check if the source task is closed.
    // 4. If all blockers are closed (or there are none), the task is ready.
    //
    // Optimization: single SQL query with LEFT JOIN:
    //
    // SELECT t.* FROM tasks t
    // WHERE t.status_id != 2  -- StatusClosed
    // AND NOT EXISTS (
    //     SELECT 1 FROM edges e
    //     JOIN tasks blocker ON e.target_id = blocker.id
    //     WHERE e.source_id = t.id
    //     AND e.kind_id = 0    -- EdgeBlockedBy
    //     AND blocker.status_id != 2  -- StatusClosed
    // )
}
```

### 7.2 Blocked Tasks

Conversely, `Blocked()` returns tasks where at least one `EdgeBlockedBy` source is not closed:

```sql
SELECT t.* FROM tasks t
WHERE t.status_id != 2  -- StatusClosed
AND EXISTS (
    SELECT 1 FROM edges e
    JOIN tasks blocker ON e.target_id = blocker.id
    WHERE e.source_id = t.id
    AND e.kind_id = 0    -- EdgeBlockedBy
    AND blocker.status_id != 2  -- StatusClosed
)
```

### 7.3 Edge Direction Convention

Providence follows the same direction convention as Beads:
- `AddEdge(parent, child, EdgeBlockedBy)` means "parent is blocked by child" — child must finish first.
- The `source` in the edges table is the parent (the thing that stays open).
- The `target` is the child (the thing that must finish first).

This matches `bd dep add parent --blocked-by child`.

---

## 8. Integration with Pasture

### 8.1 Import Path

Pasture imports providence as a Go module:

```go
import "github.com/dayvidpham/provenance"
```

In pasture's `go.mod`:

```
require github.com/dayvidpham/provenance v0.1.0
```

During development, use `replace` directive for local development:

```
replace github.com/dayvidpham/provenance => ../../providence
```

### 8.2 CLI Surface: `pasture task ...`

The CLI lives in pasture, not in providence. Providence is a library. The pasture CLI commands use the Cobra + handler pattern from pasture's existing conventions:

```
pasture task create "Title" --description="..." --type=feature --priority=1
pasture task show <id>
pasture task update <id> --status=in_progress
pasture task close <id> --reason="Done"
pasture task list [--status=open] [--label=aura:epic]
pasture task ready
pasture task blocked

pasture task dep add <parent-id> --blocked-by <child-id>
pasture task dep tree <id>

pasture task label add <id> <label>
pasture task label remove <id> <label>

pasture task comment add <id> "Comment text"
pasture task comments <id>
```

### 8.3 Storage Location

```go
// Default database path follows XDG:
// ~/.local/share/pasture/providence.db
//
// Overridable via:
// - PROVIDENCE_DB_PATH environment variable
// - pasture config file (pasture.toml: [providence] db_path = "...")
func DefaultDBPath() string {
    dataDir := os.Getenv("XDG_DATA_HOME")
    if dataDir == "" {
        home, _ := os.UserHomeDir()
        dataDir = filepath.Join(home, ".local", "share")
    }
    return filepath.Join(dataDir, "pasture", "providence.db")
}
```

### 8.4 Pasture Handler Pattern

Each `pasture task` subcommand follows the existing Cobra + handler pattern:

```go
// cmd/pasture-msg/task_create.go
var taskCreateCmd = &cobra.Command{
    Use:   "create TITLE",
    Short: "Create a new task",
    RunE:  runTaskCreate,
}

func runTaskCreate(cmd *cobra.Command, args []string) error {
    dbPath := viper.GetString("providence.db_path")
    tracker, err := providence.Open(dbPath)
    if err != nil { return err }
    defer tracker.Close()

    // Parse flags, call tracker.Create, format output
    task, err := tracker.Create(namespace, args[0], description, taskType, priority)
    if err != nil { return err }

    // Output formatting (JSON or text, based on --format flag)
    return formatters.PrintTask(cmd.OutOrStdout(), task, outputFormat)
}
```

---

## 9. Implementation Slices

These slices are ordered by dependency. Each slice is independently testable and produces a working subset of the system.

### Slice 1: Type Foundation

**Scope:** All types, enums, IDs, sentinel errors. No persistence, no graph.

**Deliverables:**
- `enums.go` — all 8 enum types with iota, String(), MarshalText/UnmarshalText, IsValid()
- `types.go` — TaskID, AgentID, ActivityID, CommentID (with Parse/String), Task, Agent, Activity, Edge, Label, Comment, UpdateFields, ListFilter
- `errors.go` — ErrNotFound, ErrCycleDetected, ErrAlreadyClosed, ErrInvalidID
- `enums_test.go` — round-trip marshal/unmarshal, IsValid boundaries, String coverage
- `types_test.go` — Parse/String round-trip for all ID types, edge cases (empty namespace, malformed UUID)

**Exit criteria:** `go test -race ./...` passes. All enum values round-trip through MarshalText/UnmarshalText. All ID types round-trip through String/Parse.

### Slice 2: SQLite Persistence Layer

**Scope:** Database open/close, schema creation, raw CRUD operations for all tables. No graph library yet.

**Deliverables:**
- `internal/sqlite/db.go` — Open, Close, WAL config, foreign keys
- `internal/sqlite/schema.go` — ensureSchema with all CREATE TABLE/INDEX statements
- `internal/sqlite/tasks.go` — InsertTask, GetTask, UpdateTask, ListTasks, CloseTask
- `internal/sqlite/edges.go` — InsertEdge, DeleteEdge, EdgesByTask, EdgesByKind
- `internal/sqlite/agents.go` — UpsertAgent, GetAgent
- `internal/sqlite/activities.go` — InsertActivity, UpdateActivity, ListActivities
- `internal/sqlite/labels.go` — AddLabel, RemoveLabel, LabelsByTask, TasksByLabel
- `internal/sqlite/comments.go` — AddComment, CommentsByTask
- `internal/sqlite/db_test.go` — integration tests: CRUD round-trips, foreign key enforcement, WAL mode verification

**Exit criteria:** All CRUD operations work against a real SQLite database (in-memory `:memory:` for tests). Foreign keys enforced. Schema idempotent (run twice without error).

### Slice 3: Graph Store Implementation

**Scope:** Implement `dominikbraun/graph.Store[TaskID, Task]` backed by Slice 2's SQLite layer. Wire up cycle prevention.

**Deliverables:**
- `internal/graph/store.go` — Store implementation: AddVertex, Vertex, RemoveVertex, ListVertices, VertexCount, AddEdge, Edge, RemoveEdge + adjacency/predecessor map methods
- `internal/helpers/ancestors.go` — Ancestors, Descendants functions
- `internal/graph/store_test.go` — Store contract tests: add/remove vertices and edges, cycle detection, vertex count
- `internal/helpers/ancestors_test.go` — ancestor/descendant traversal tests with known graph topologies

**Exit criteria:** Graph operations work through SQLite. `PreventCycles()` rejects cyclic blocked-by edges. Ancestors/Descendants return correct transitive closures.

### Slice 4: Tracker Facade + Dependency Queries

**Scope:** The public `Tracker` type that wires together graph + SQLite. Task CRUD, edge management, readiness queries.

**Deliverables:**
- `providence.go` — Package doc, Open, Close constructors
- `tracker.go` — All Tracker methods: Create, Show, Update, CloseTask, List, AddEdge, RemoveEdge, Edges, Blocked, Ready, DepTree, Ancestors, Descendants, AddLabel, RemoveLabel, Labels, AddComment, Comments, RegisterAgent, Agent, StartActivity, EndActivity, Activities
- `tracker_test.go` — Integration tests exercising the full stack: create tasks, add edges, verify readiness, close blockers, verify readiness changes

**Exit criteria:** Full public API works end-to-end against a real SQLite database. Readiness correctly reflects blocked-by edge state. Cycle detection works through the public API.

### Slice 5: Makefile + CI Foundation

**Scope:** Build tooling, quality gates, go.mod dependencies.

**Deliverables:**
- `Makefile` — fmt, lint, test, build targets (matching pasture conventions)
- `go.mod` / `go.sum` — dependencies resolved: `github.com/dominikbraun/graph`, `zombiezen.com/go/sqlite`, `github.com/google/uuid`
- `CLAUDE.md` — agent coding standards for the providence repo
- `CONTRIBUTING.md` — development workflow guide

**Exit criteria:** `make fmt && make lint && make test && make build` all pass. `CGO_ENABLED=0 go build ./...` succeeds.

---

## 10. Test Strategy

### 10.1 Test Principles

1. **Integration over unit:** Test through the public `Tracker` API whenever possible. Internal packages have their own tests only where the public API does not exercise a code path.
2. **Real SQLite:** Use `:memory:` SQLite databases in tests — never mock the database. The database IS the system under test.
3. **No mocking of providence internals:** The `Tracker` is not mocked. Tests exercise the real code path from public API through graph library through SQLite.
4. **Race detection:** All tests run with `-race`.

### 10.2 Test Categories

| Category | Package | What |
|----------|---------|------|
| Enum round-trip | `providence_test` | MarshalText/UnmarshalText for all 8 enum types, IsValid boundaries, unknown values |
| ID round-trip | `providence_test` | Parse/String for TaskID, AgentID, ActivityID, CommentID; edge cases (empty namespace, bad UUID, wrong separator) |
| SQLite CRUD | `internal/sqlite` (white-box) | Insert/Get/Update/Delete for every table; foreign key enforcement; schema idempotency |
| Graph Store | `internal/graph` (black-box) | Store interface contract: add/remove vertices, add/remove edges, cycle rejection, vertex/edge count |
| Traversal | `internal/helpers` (black-box) | Ancestors/Descendants on known topologies (linear chain, diamond, disconnected components) |
| Tracker integration | `providence_test` | Full lifecycle: create tasks, add edges, verify readiness, close tasks, verify readiness changes, labels, comments, PROV-O agents/activities |
| Readiness | `providence_test` | Edge cases: task with no edges is ready, task with all-closed blockers is ready, task with one open blocker is blocked, closing last blocker makes task ready |
| Cycle detection | `providence_test` | Direct cycle (A blocks B blocks A), indirect cycle (A->B->C->A), self-loop |

### 10.3 BDD Acceptance Criteria

**Scenario: Task readiness reflects blocked-by state**

- **Given** task A is open and task B is open
- **When** AddEdge(A, B, EdgeBlockedBy) is called (A is blocked by B)
- **Then** Ready() returns B but not A, and Blocked() returns A but not B
- **Should not** include A in Ready() results while B is open

**Scenario: Closing a blocker unblocks the dependent**

- **Given** task A is blocked by task B (EdgeBlockedBy)
- **When** CloseTask(B, "done") is called
- **Then** Ready() returns A, and Blocked() returns neither A nor B
- **Should not** leave A in Blocked() after its only blocker is closed

**Scenario: Cycle prevention on blocked-by edges**

- **Given** task A is blocked by task B
- **When** AddEdge(B, A, EdgeBlockedBy) is called (B is blocked by A — creating a cycle)
- **Then** AddEdge returns ErrCycleDetected
- **Should not** persist the cyclic edge in the database

**Scenario: Non-blocked-by edges do not affect readiness**

- **Given** task A has EdgeDerivedFrom pointing to task B, and no blocked-by edges
- **When** Ready() is called
- **Then** both A and B appear in the Ready() results
- **Should not** treat EdgeDerivedFrom as a blocking dependency

**Scenario: TaskID round-trip**

- **Given** a TaskID with namespace "aura-plugins" and a valid UUIDv7
- **When** String() is called and the result is passed to ParseTaskID()
- **Then** the parsed TaskID equals the original
- **Should not** lose the namespace or corrupt the UUID

**Scenario: Enum marshal round-trip**

- **Given** every valid value for each of the 8 enum types
- **When** MarshalText is called and the result is passed to UnmarshalText
- **Then** the unmarshaled value equals the original
- **Should not** accept unknown string values (UnmarshalText returns error)

**Scenario: Schema idempotency**

- **Given** a SQLite database with the schema already applied
- **When** ensureSchema() is called again
- **Then** no error is returned and no data is lost
- **Should not** drop existing tables or indexes

**Scenario: Foreign key enforcement on edges**

- **Given** an empty database with schema applied
- **When** InsertEdge is called with a source_id that does not exist in tasks
- **Then** the operation returns an error (foreign key violation)
- **Should not** silently insert an edge referencing a nonexistent task

---

## 11. Dependencies

### go.mod

```
module github.com/dayvidpham/provenance

go 1.24

require (
    github.com/dominikbraun/graph v0.23.0
    github.com/google/uuid v1.6.0
    zombiezen.com/go/sqlite v1.6.0
)
```

All three dependencies are pure Go (`CGO_ENABLED=0` compatible):
- **dominikbraun/graph** — zero transitive dependencies
- **google/uuid** — zero transitive dependencies
- **zombiezen.com/go/sqlite** — pure Go SQLite implementation (uses modernc.org/libc internally but is CGO-free)

---

## 12. Engineering Tradeoffs

| Decision | Options Considered | Choice | Rationale |
|----------|--------------------|--------|-----------|
| Graph library | Hand-roll (~130 LOC) vs. dominikbraun/graph | dominikbraun/graph | Store interface enables SQLite persistence; PreventCycles is built-in; zero deps. Hand-roll would need equivalent cycle detection and would not support the Store pattern. |
| SQLite driver | modernc.org/sqlite vs. zombiezen.com/go/sqlite | zombiezen | Lower-level API suitable for Store interface; direct connection management without database/sql pooling overhead. Both are CGO-free. |
| ID format | UUID only vs. Namespace+UUID | Namespace+UUID | Namespace enables multi-project usage (aura-plugins, other projects). Double-dash separator avoids ambiguity with UUID's internal dashes. |
| Enum pattern | String constants (pasture style) vs. iota | iota | URD decision. Iota provides integer efficiency for in-memory operations and database storage while MarshalText/UnmarshalText handles human-readable serialization. |
| Edge storage | Separate table per edge kind vs. single table | Single table with kind column | Simpler schema, easier queries ("all edges for task X"). The kind column with an index handles filtering efficiently at <10K scale. |
| Blocked-by subgraph | Load all edges into graph vs. only blocked-by | Only blocked-by | Cycle prevention and topo sort are meaningless for metadata edges. Loading only blocked-by edges keeps the graph small and semantically correct. |
| Public API shape | Interface vs. concrete type | Concrete Tracker type | Providence is the only implementation. An interface would be premature abstraction. Pasture can define its own interface if it needs to mock providence in tests. |
| PROV-O scope | Tasks only (add Agent/Activity later) vs. full PROV-O | Full PROV-O from day 1 | URD decision. Agent and Activity are small additions (1 table + 1 struct each). Adding them later would require schema migration and API changes. |

---

## 13. Validation Checklist

- [ ] All 8 enum types implement String(), MarshalText(), UnmarshalText(), IsValid()
- [ ] TaskID, AgentID, ActivityID, CommentID are distinct types (compile-time safety)
- [ ] ParseTaskID/ParseAgentID/ParseActivityID/ParseCommentID String round-trip for all ID types
- [ ] SQLite schema creates all 6 entity tables + 8 enum lookup tables with proper indexes and foreign keys
- [ ] All enum columns use INTEGER with FK references to lookup tables (no string literals in queries)
- [ ] Lookup tables seeded via INSERT OR IGNORE on every Open()
- [ ] Schema is BCNF: every non-trivial FD has a superkey determinant (FD annotations inline)
- [ ] WAL mode and busy_timeout configured on every Open()
- [ ] dominikbraun/graph Store implementation passes all Store contract tests
- [ ] PreventCycles rejects cyclic blocked-by edges
- [ ] Only EdgeBlockedBy edges are loaded into the graph (other kinds bypass it)
- [ ] Ready() returns tasks with no open blocked-by predecessors
- [ ] Blocked() returns tasks with at least one open blocked-by predecessor
- [ ] Ancestors/Descendants compute correct transitive closures
- [ ] `CGO_ENABLED=0 go build ./...` succeeds
- [ ] `go test -race ./...` passes
- [ ] All errors are actionable (what, why, where, when, fix)
- [ ] go.mod contains only approved dependencies (dominikbraun/graph, google/uuid, zombiezen SQLite)

---

## 14. Open Questions

1. **zombiezen SQLite Store compatibility:** The dominikbraun/graph Store interface expects synchronous method calls. zombiezen's API uses explicit connection objects rather than database/sql's pool. The Store implementation will need to manage a dedicated connection or use zombiezen's connection pool. This is a detailed design decision for Slice 3.

2. **Namespace default:** When creating tasks without an explicit namespace, should there be a default namespace (e.g., derived from the current git repo name)? The current design requires explicit namespace. This can be decided during pasture CLI integration.

3. **JSON export/import:** Beads uses JSONL for git-friendly export. Providence will need a similar mechanism eventually, but it is not in MVP scope. The SQLite database is the source of truth; JSONL export is a follow-up feature.

---

## Appendix A: Dependency Chain (Beads)

```
REQUEST (aura-plugins-oviik)
  └── blocked by ELICIT (aura-plugins-4fejd)
        └── blocked by PROPOSAL-1 (aura-plugins-z24dr)  <-- this document
              └── blocked by IMPL_PLAN (to be created after ratification)
                    ├── blocked by Slice 1: Type Foundation
                    ├── blocked by Slice 2: SQLite Persistence
                    ├── blocked by Slice 3: Graph Store
                    ├── blocked by Slice 4: Tracker Facade
                    └── blocked by Slice 5: Makefile + CI
```
