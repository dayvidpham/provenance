# PROPOSAL-2: Providence Architecture and Implementation Plan

**Beads ID:** (to be assigned)
**Supersedes:** PROPOSAL-1 (aura-plugins-z24dr)
**References:**
- REQUEST: aura-plugins-oviik
- URD: aura-plugins-f85gw (comments contain URE rounds 1-4 + UAT decisions)
- ELICIT: aura-plugins-4fejd
- UAT: aura-plugins-d5586
- EPIC: aura-plugins-t7pyf

**Changes from PROPOSAL-1:** Incorporates all 12 UAT decisions — exported Tracker interface, OpenSQLite/OpenMemory constructors, Agent TPT (Human/ML/Software), ml_models + providers lookup tables, Role on ML agent, Task phase_id required, TaskType generic + phase_id, single edges table with target_id FK dropped, ParseTaskID from right, git remote-derived default namespace, all types in root package confirmed.

---

## 1. Problem Space

Providence replaces Beads (bd) as the task dependency tracker for the Aura Protocol agent system. The fundamental problem is tracking work products, their dependencies, and their provenance across multi-agent planning and implementation workflows.

### Axes of the Problem

| Axis | Assessment |
|------|-----------|
| **Parallelism** | Low at storage layer (single SQLite writer), moderate at read layer (concurrent graph queries from multiple agents). WAL mode handles this. |
| **Distribution** | Single-process, single-machine. SQLite file on local disk. No network protocol needed. |
| **Scale** | < 10,000 tasks. In-memory graph operations are microsecond-scale at this size. |
| **Entity relationships** | Has-a: Task has Labels, has Comments, has Edges, has Phase. Is-a: Agent is-a HumanAgent, MLAgent, or SoftwareAgent (TPT inheritance). Agent, Activity, Entity are all PROV-O nodes. |
| **Domain novelty** | Medium. W3C PROV-O is a well-defined ontology; mapping it to Go types and SQLite is straightforward but the 6-edge-kind model with selective readiness semantics and agent TPT hierarchy requires careful design. |

### Key Design Constraints

1. **Readiness subgraph:** Only `EdgeBlockedBy` affects task readiness. The other 5 edge kinds (`EdgeDerivedFrom`, `EdgeSupersedes`, `EdgeDiscoveredFrom`, `EdgeGeneratedBy`, `EdgeAttributedTo`) are provenance metadata that enriches the audit trail without affecting scheduling. The graph library's cycle prevention and topological sort operate only on the blocked-by subgraph, while all 6 edge kinds are persisted in the same SQLite table.

2. **Cross-entity edges:** `EdgeGeneratedBy` (task → activity) and `EdgeAttributedTo` (task → agent) cross entity boundaries. The edges table uses a single `target_id TEXT` column without a foreign key constraint to accommodate all target types.

3. **Agent TPT:** Agents are modeled as a table-per-type hierarchy. The base `agents` table stores the common discriminator (`kind_id`), and three child tables (`agents_human`, `agents_ml`, `agents_software`) store kind-specific attributes.

---

## 2. Module Layout

```
github.com/dayvidpham/providence/
├── providence.go           # Package doc + public facade (Tracker interface + constructors)
├── types.go                # All public types: TaskID, AgentID, ActivityID, Task, Agent (TPT), Edge, etc.
├── enums.go                # All iota enums: Status, Priority, TaskType, EdgeKind, AgentKind, Provider, Role, Phase, Stage
├── errors.go               # Sentinel errors and error constructors
├── tracker.go              # sqliteTracker implementation of Tracker interface
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
│   │   ├── agents.go       # Agent TPT CRUD (base + 3 child tables)
│   │   ├── activities.go   # Activity CRUD SQL operations
│   │   ├── labels.go       # Label add/remove/query SQL operations
│   │   ├── comments.go     # Comment add/query SQL operations
│   │   └── db_test.go      # SQLite integration tests
│   └── helpers/
│       ├── ancestors.go    # Ancestors/Descendants composed from DFS + PredecessorMap
│       └── ancestors_test.go
├── docs/
│   ├── PROPOSAL-1.md       # Original proposal (superseded)
│   └── PROPOSAL-2.md       # This document
├── go.mod
├── go.sum
├── LICENSE
├── .gitignore
└── Makefile                # fmt, lint, test, build targets
```

### Package Responsibilities

| Package | Role |
|---------|------|
| `providence` (root) | Public API surface. All exported types, the `Tracker` interface, constructors (`OpenSQLite`, `OpenMemory`), and the internal `sqliteTracker` implementation. Consumers (e.g., pasture) import only this package. |
| `internal/sqlite` | All SQL operations. Encapsulates the zombiezen SQLite driver. No graph logic here — pure relational CRUD including agent TPT operations. |
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
// Uses strings.LastIndex to split on the rightmost "--" separator,
// which correctly handles namespaces that contain "--" themselves.
// Returns ErrInvalidID if the format is invalid or the UUID is malformed.
func ParseTaskID(s string) (TaskID, error) {
    idx := strings.LastIndex(s, "--")
    if idx < 0 {
        return TaskID{}, fmt.Errorf("%w: no '--' separator in %q", ErrInvalidID, s)
    }
    ns := s[:idx]
    if ns == "" {
        return TaskID{}, fmt.Errorf("%w: empty namespace in %q", ErrInvalidID, s)
    }
    u, err := uuid.Parse(s[idx+2:])
    if err != nil {
        return TaskID{}, fmt.Errorf("%w: invalid UUID in %q: %v", ErrInvalidID, s, err)
    }
    return TaskID{Namespace: ns, UUID: u}, nil
}

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

All four ID types follow the same `{Namespace}--{UUIDv7}` wire format with `String()` and `Parse*()` methods. All four `Parse*` functions use `strings.LastIndex(s, "--")` to split from the right.

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
// Protocol artifacts are distinguished by Phase, not by TaskType.
type TaskType int

const (
    TaskTypeBug     TaskType = iota // Something broken
    TaskTypeFeature                 // New functionality
    TaskTypeTask                    // Work item (tests, docs, refactoring)
    TaskTypeEpic                    // Large feature with subtasks
    TaskTypeChore                   // Maintenance (dependencies, tooling)
)

// EdgeKind classifies the relationship between entities.
type EdgeKind int

const (
    EdgeBlockedBy      EdgeKind = iota // Task → Task: affects task readiness
    EdgeDerivedFrom                    // Task → Task: PROPOSAL-2 derived from PROPOSAL-1
    EdgeSupersedes                     // Task → Task: PROPOSAL-3 supersedes PROPOSAL-2
    EdgeDiscoveredFrom                 // Task → Task: found during work on parent
    EdgeGeneratedBy                    // Task → Activity: which activity produced this
    EdgeAttributedTo                   // Task → Agent: which agent owns this
)

// AgentKind discriminates the agent TPT hierarchy.
type AgentKind int

const (
    AgentKindHuman           AgentKind = iota // 0: Human user
    AgentKindMachineLearning                  // 1: AI/ML model agent
    AgentKindSoftware                         // 2: Software tool or script
)

// Provider identifies the organization behind an ML model.
type Provider int

const (
    ProviderAnthropic Provider = iota // 0
    ProviderGoogle                    // 1
    ProviderOpenAI                    // 2
    ProviderLocal                     // 3
)

// Role identifies an agent's role in the protocol.
// Only used by ML agents (agents_ml.role_id).
type Role int

const (
    RoleHuman      Role = iota // Human user (conceptual; not stored on agents_ml)
    RoleArchitect              // Architect agent
    RoleSupervisor             // Supervisor agent
    RoleWorker                 // Worker agent
    RoleReviewer               // Reviewer agent
)

// Phase identifies a phase in the epoch lifecycle.
// Every task has a required phase. Use PhaseUnscoped for generic
// tasks that are not specific to a protocol phase.
type Phase int

const (
    PhaseRequest      Phase = iota // p1
    PhaseElicit                    // p2
    PhasePropose                   // p3
    PhaseReview                    // p4
    PhasePlanUAT                   // p5
    PhaseRatify                    // p6
    PhaseHandoff                   // p7
    PhaseImplPlan                  // p8
    PhaseWorkerSlices              // p9
    PhaseCodeReview                // p10
    PhaseImplUAT                   // p11
    PhaseLanding                   // p12
    PhaseUnscoped                  // Generic tasks not tied to a specific protocol phase
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
// Every task has a required Phase — use PhaseUnscoped for generic tasks.
type Task struct {
    ID          TaskID     `json:"id"`
    Title       string     `json:"title"`
    Description string     `json:"description"`
    Status      Status     `json:"status"`
    Priority    Priority   `json:"priority"`
    Type        TaskType   `json:"type"`
    Phase       Phase      `json:"phase"`              // Required — protocol artifacts distinguished by phase
    Owner       *AgentID   `json:"owner,omitempty"`     // nil if unassigned
    Notes       string     `json:"notes,omitempty"`
    CreatedAt   time.Time  `json:"createdAt"`
    UpdatedAt   time.Time  `json:"updatedAt"`
    ClosedAt    *time.Time `json:"closedAt,omitempty"`
    CloseReason string     `json:"closeReason,omitempty"`
}

// --- Agent TPT Hierarchy ---
//
// Agents use table-per-type (TPT) inheritance:
//   Base agents table: (id, kind_id)
//   agents_human:    (agent_id PK/FK, name, contact)
//   agents_ml:       (agent_id PK/FK, role_id, model_id)
//   agents_software: (agent_id PK/FK, name, version, source)
//
// The Go types mirror this: Agent is the base, with HumanAgent,
// MLAgent, and SoftwareAgent embedding it.

// Agent is the base type for all agents (PROV-O Agent).
// Use Kind to determine which typed agent to query.
type Agent struct {
    ID   AgentID   `json:"id"`
    Kind AgentKind `json:"kind"`
}

// HumanAgent represents a human user.
type HumanAgent struct {
    Agent
    Name    string `json:"name"`
    Contact string `json:"contact,omitempty"` // email, slack handle, etc.
}

// MLAgent represents a machine learning model acting as an agent.
// Role stays on the agent: same model with different roles = different registrations.
type MLAgent struct {
    Agent
    Role  Role    `json:"role"`
    Model MLModel `json:"model"`
}

// SoftwareAgent represents a software tool or script.
type SoftwareAgent struct {
    Agent
    Name    string `json:"name"`
    Version string `json:"version"`
    Source  string `json:"source"` // git remote URL or filesystem path
}

// MLModel represents a row in the ml_models lookup table.
// The combination (Provider, Name) is unique.
type MLModel struct {
    ID       int      `json:"id"`
    Provider Provider `json:"provider"`
    Name     string   `json:"name"`
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

// Edge represents a typed relationship originating from a task.
// Source is always a TaskID. Target may be a TaskID, AgentID, or
// ActivityID depending on the EdgeKind:
//   - EdgeBlockedBy, EdgeDerivedFrom, EdgeSupersedes, EdgeDiscoveredFrom: target is TaskID
//   - EdgeGeneratedBy: target is ActivityID
//   - EdgeAttributedTo: target is AgentID
type Edge struct {
    SourceID string   `json:"sourceId"` // Task ID (always)
    TargetID string   `json:"targetId"` // Task, Agent, or Activity ID
    Kind     EdgeKind `json:"kind"`
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

The public API is exposed through an exported `Tracker` interface. The internal `sqliteTracker` struct implements it. Two constructors return the interface.

```go
// Tracker is the central API for providence task management.
// It manages a dominikbraun/graph instance backed by SQLite persistence
// and provides all CRUD, dependency, label, comment, and query operations.
type Tracker interface {
    // Close releases the database connection and all resources.
    Close() error

    // --- Task CRUD ---

    // Create creates a new task with the given fields.
    // Assigns a UUIDv7 and sets CreatedAt/UpdatedAt to now.
    // Returns the created Task (with ID populated).
    Create(namespace, title, description string, taskType TaskType, priority Priority, phase Phase) (Task, error)

    // Show returns a task by ID.
    // Returns ErrNotFound if the task does not exist.
    Show(id TaskID) (Task, error)

    // Update modifies mutable fields of an existing task.
    // Only non-nil fields in the UpdateFields struct are applied.
    // Returns the updated Task.
    Update(id TaskID, fields UpdateFields) (Task, error)

    // CloseTask marks a task as closed with the given reason.
    // Sets ClosedAt to now and Status to StatusClosed.
    // Returns ErrNotFound if the task does not exist.
    // Returns ErrAlreadyClosed if the task is already closed.
    CloseTask(id TaskID, reason string) (Task, error)

    // List returns all tasks matching the given filter.
    // An empty filter returns all tasks.
    List(filter ListFilter) ([]Task, error)

    // --- Typed Dependency Edges ---

    // AddEdge creates a typed relationship from a task to a target entity.
    // For EdgeBlockedBy, returns ErrCycleDetected if the edge would create a cycle
    // in the blocked-by subgraph.
    // For task-to-task edge kinds, returns ErrNotFound if either task does not exist.
    // Target is a string ID; its type depends on the EdgeKind (see Edge docs).
    AddEdge(sourceID TaskID, targetID string, kind EdgeKind) error

    // RemoveEdge removes a typed relationship.
    // Returns ErrNotFound if the edge does not exist.
    RemoveEdge(sourceID TaskID, targetID string, kind EdgeKind) error

    // Edges returns all edges of the given kind involving the task (as source).
    // If kind is nil, returns edges of all kinds.
    Edges(id TaskID, kind *EdgeKind) ([]Edge, error)

    // --- Readiness Queries (blocked-by subgraph only) ---

    // Blocked returns tasks that have at least one open blocked-by dependency.
    Blocked() ([]Task, error)

    // Ready returns tasks that are open and have no open blocked-by dependencies.
    Ready() ([]Task, error)

    // DepTree returns the blocked-by dependency tree rooted at the given task.
    // Returns a list of edges in the tree (depth-first order).
    DepTree(id TaskID) ([]Edge, error)

    // Ancestors returns all tasks that transitively block the given task
    // (following blocked-by edges backward).
    Ancestors(id TaskID) ([]Task, error)

    // Descendants returns all tasks that are transitively blocked by the given task
    // (following blocked-by edges forward).
    Descendants(id TaskID) ([]Task, error)

    // --- Labels ---

    // AddLabel attaches a label to a task. Idempotent.
    AddLabel(id TaskID, label string) error

    // RemoveLabel detaches a label from a task. Idempotent.
    RemoveLabel(id TaskID, label string) error

    // Labels returns all labels attached to a task.
    Labels(id TaskID) ([]string, error)

    // --- Comments ---

    // AddComment adds a timestamped comment to a task.
    // Assigns a UUIDv7 CommentID and sets CreatedAt to now.
    // Returns the created Comment (with ID populated).
    // Returns ErrNotFound if the task or author agent does not exist.
    AddComment(id TaskID, authorID AgentID, body string) (Comment, error)

    // Comments returns all comments on a task in chronological order.
    Comments(id TaskID) ([]Comment, error)

    // --- PROV-O Agents (TPT) ---

    // RegisterHumanAgent creates or updates a human agent.
    RegisterHumanAgent(namespace, name, contact string) (HumanAgent, error)

    // RegisterMLAgent creates or updates an ML agent.
    // Looks up the model by (provider, modelName) in the ml_models table.
    // Returns ErrNotFound if the model combination does not exist in the lookup table.
    // Same model with different roles = different agent registrations.
    RegisterMLAgent(namespace string, role Role, provider Provider, modelName string) (MLAgent, error)

    // RegisterSoftwareAgent creates or updates a software agent.
    RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error)

    // Agent returns the base agent by ID. Use Kind to determine which typed method to call.
    Agent(id AgentID) (Agent, error)

    // HumanAgent returns a human agent by ID.
    // Returns ErrNotFound if the agent does not exist or is not a human agent.
    HumanAgent(id AgentID) (HumanAgent, error)

    // MLAgent returns an ML agent by ID.
    // Returns ErrNotFound if the agent does not exist or is not an ML agent.
    MLAgent(id AgentID) (MLAgent, error)

    // SoftwareAgent returns a software agent by ID.
    // Returns ErrNotFound if the agent does not exist or is not a software agent.
    SoftwareAgent(id AgentID) (SoftwareAgent, error)

    // --- PROV-O Activities ---

    // StartActivity records the beginning of an activity.
    StartActivity(agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error)

    // EndActivity records the completion of an activity.
    EndActivity(id ActivityID) (Activity, error)

    // Activities returns all activities, optionally filtered by agent.
    Activities(agentID *AgentID) ([]Activity, error)
}

// --- Constructors ---

// OpenSQLite creates a new Tracker backed by a SQLite database at dbPath.
// Parent directories are created if they do not exist.
// The database schema is applied via migrations on first open.
//
// The default dbPath follows XDG: ~/.local/share/pasture/providence.db
func OpenSQLite(dbPath string) (Tracker, error)

// OpenMemory creates a new Tracker backed by an in-memory SQLite database.
// This is equivalent to OpenSQLite(":memory:") — same code path, same implementation.
// Useful for tests and ephemeral sessions.
func OpenMemory() (Tracker, error)
```

### 4.1 Supporting Types

```go
// UpdateFields specifies which task fields to modify.
// Nil pointer fields are not modified.
type UpdateFields struct {
    Title       *string
    Description *string
    Status      *Status
    Priority    *Priority
    Phase       *Phase
    Owner       *AgentID
    Notes       *string
}

// ListFilter specifies criteria for listing tasks.
// Zero-value fields are ignored (no filter on that field).
type ListFilter struct {
    Status    *Status
    Priority  *Priority
    Type      *TaskType
    Phase     *Phase     // Filter by protocol phase
    Label     string     // empty means no label filter
    Namespace string     // empty means all namespaces
}
```

### 4.2 Sentinel Errors

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

    // ErrAgentKindMismatch is returned when querying a typed agent with the wrong kind.
    ErrAgentKindMismatch = errors.New("providence: agent kind mismatch")
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

**Redundancy note:** `tasks.namespace` is derivable from `tasks.id` (the prefix before `--`). It is stored as a denormalized column for efficient `WHERE namespace = ?` queries. Since `id -> namespace` and `id` is a superkey, this does not violate BCNF — it is a materialized deterministic function of the key.

### 6.3 Reference Tables (Enum Lookup Tables)

Each iota enum has a corresponding lookup table following the pattern from agent-data-leverage: `(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`. Seed data is inserted on every `Open()` via `INSERT OR IGNORE`.

```sql
-- Simple enum lookup tables
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

CREATE TABLE agent_kinds (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE providers (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE roles (
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

### 6.4 Composite Lookup Table: ml_models

The `ml_models` table is not a simple enum — it has a composite unique key on `(provider_id, name)`. This is a closed lookup table: seed data is defined at schema creation time.

```sql
-- FD: id -> (provider_id, name); (provider_id, name) -> id
-- ml_models lookup table — closed set of known models
CREATE TABLE ml_models (
    id          INTEGER PRIMARY KEY,
    provider_id INTEGER NOT NULL REFERENCES providers(id),
    name        TEXT NOT NULL,
    UNIQUE (provider_id, name)
) STRICT;
```

### 6.5 Seed Data

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

INSERT OR IGNORE INTO agent_kinds (id, name) VALUES
    (0, 'human'), (1, 'machine_learning'), (2, 'software');

INSERT OR IGNORE INTO providers (id, name) VALUES
    (0, 'anthropic'), (1, 'google'), (2, 'openai'), (3, 'local');

INSERT OR IGNORE INTO roles (id, name) VALUES
    (0, 'human'), (1, 'architect'), (2, 'supervisor'), (3, 'worker'), (4, 'reviewer');

INSERT OR IGNORE INTO phases (id, name) VALUES
    (0, 'request'), (1, 'elicit'), (2, 'propose'), (3, 'review'),
    (4, 'plan_uat'), (5, 'ratify'), (6, 'handoff'), (7, 'impl_plan'),
    (8, 'worker_slices'), (9, 'code_review'), (10, 'impl_uat'), (11, 'landing'),
    (12, 'unscoped');

INSERT OR IGNORE INTO stages (id, name) VALUES
    (0, 'not_started'), (1, 'in_progress'), (2, 'blocked'), (3, 'complete');

-- ML models — closed lookup table
INSERT OR IGNORE INTO ml_models (id, provider_id, name) VALUES
    (0, 0, 'claude_opus_4'),
    (1, 0, 'claude_sonnet_4'),
    (2, 0, 'claude_haiku_4');
```

### 6.6 Entity Tables

All entity tables use `STRICT` mode and UUIDv7 TEXT primary keys. FD annotations inline.

```sql
-- ============================================================
-- Agent TPT Hierarchy
-- ============================================================

-- FD: id -> kind_id (id is PK)
-- Base agents table — discriminator only
CREATE TABLE agents (
    id      TEXT PRIMARY KEY,  -- AgentID.String() ("namespace--uuidv7")
    kind_id INTEGER NOT NULL REFERENCES agent_kinds(id)
) STRICT;

-- FD: agent_id -> (name, contact) (agent_id is PK)
-- Human agent child table
CREATE TABLE agents_human (
    agent_id TEXT PRIMARY KEY REFERENCES agents(id),
    name     TEXT NOT NULL,
    contact  TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

-- FD: agent_id -> (role_id, model_id) (agent_id is PK)
-- ML agent child table — role stays on agent, not activity
CREATE TABLE agents_ml (
    agent_id TEXT PRIMARY KEY REFERENCES agents(id),
    role_id  INTEGER NOT NULL REFERENCES roles(id),
    model_id INTEGER NOT NULL REFERENCES ml_models(id)
) STRICT, WITHOUT ROWID;

-- FD: agent_id -> (name, version, source) (agent_id is PK)
-- Software agent child table
CREATE TABLE agents_software (
    agent_id TEXT PRIMARY KEY REFERENCES agents(id),
    name     TEXT NOT NULL,
    version  TEXT NOT NULL DEFAULT '',
    source   TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

-- ============================================================
-- Tasks
-- ============================================================

-- FD: id -> all columns (id is PK)
-- Tasks (PROV-O Entity) — phase_id is REQUIRED
CREATE TABLE tasks (
    id           TEXT PRIMARY KEY,  -- TaskID.String() ("namespace--uuidv7")
    namespace    TEXT NOT NULL,     -- materialized from id for query efficiency
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    status_id    INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),
    priority_id  INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),
    type_id      INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),
    phase_id     INTEGER NOT NULL REFERENCES phases(id),
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
CREATE INDEX idx_tasks_phase ON tasks (phase_id);
CREATE INDEX idx_tasks_owner ON tasks (owner_id);

-- ============================================================
-- Edges (all 6 kinds, cross-entity capable)
-- ============================================================

-- FD: (source_id, target_id, kind_id) -> created_at (composite PK)
-- target_id has NO FK — it may reference tasks, agents, or activities
CREATE TABLE edges (
    source_id  TEXT NOT NULL REFERENCES tasks(id),
    target_id  TEXT NOT NULL,  -- Task, Agent, or Activity ID (no FK)
    kind_id    INTEGER NOT NULL REFERENCES edge_kinds(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (source_id, target_id, kind_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_edges_source ON edges (source_id);
CREATE INDEX idx_edges_target ON edges (target_id);
CREATE INDEX idx_edges_kind ON edges (kind_id);

-- ============================================================
-- Activities
-- ============================================================

-- FD: id -> all columns (id is PK)
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

-- ============================================================
-- Labels and Comments
-- ============================================================

-- FD: (task_id, name) -> {} (composite PK, no non-key attributes)
CREATE TABLE labels (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    name    TEXT NOT NULL,
    PRIMARY KEY (task_id, name)
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_labels_name ON labels (name);

-- FD: id -> all columns (id is PK)
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

### 6.7 Schema Migration Strategy

For MVP, use a single `ensureSchema()` function that runs `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` statements. This is idempotent and safe to run on every `Open()`.

Post-MVP, if schema changes are needed, add a `schema_version` pragma and numbered migration functions. But do not build migration infrastructure until it is needed.

### 6.8 WAL Mode and Concurrency

```go
// Applied on every Open():
PRAGMA journal_mode=WAL;    // concurrent readers, single writer
PRAGMA busy_timeout=5000;   // retry on SQLITE_BUSY for up to 5 seconds
PRAGMA foreign_keys=ON;     // enforce referential integrity
```

---

## 7. Dependency Resolution

### 7.1 Readiness Algorithm

A task is "ready" if:
1. Its status is `StatusOpen` (not closed or in-progress — though in-progress tasks may also be considered "claimed but active"; the key filter is "not closed").
2. It has **zero open blocked-by predecessors**. That is: for every `EdgeBlockedBy` edge pointing from this task, the target task must be `StatusClosed`.

```go
// Ready returns all tasks that are open and have no open blockers.
func (t *sqliteTracker) Ready() ([]Task, error) {
    // Optimization: single SQL query with NOT EXISTS:
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

Conversely, `Blocked()` returns tasks where at least one `EdgeBlockedBy` target is not closed:

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
- `AddEdge(parent, child.String(), EdgeBlockedBy)` means "parent is blocked by child" — child must finish first.
- The `source_id` in the edges table is the parent (the thing that stays open).
- The `target_id` is the child (the thing that must finish first).

This matches `bd dep add parent --blocked-by child`.

---

## 8. Integration with Pasture

### 8.1 Import Path

Pasture imports providence as a Go module:

```go
import "github.com/dayvidpham/providence"
```

In pasture's `go.mod`:

```
require github.com/dayvidpham/providence v0.1.0
```

During development, use `replace` directive for local development:

```
replace github.com/dayvidpham/providence => ../../providence
```

### 8.2 CLI Surface: `pasture task ...`

The CLI lives in pasture, not in providence. Providence is a library. The pasture CLI commands use the Cobra + handler pattern from pasture's existing conventions:

```
pasture task create "Title" --description="..." --type=feature --priority=1 --phase=propose
pasture task show <id>
pasture task update <id> --status=in_progress
pasture task close <id> --reason="Done"
pasture task list [--status=open] [--label=aura:epic] [--phase=request]
pasture task ready
pasture task blocked

pasture task dep add <parent-id> --blocked-by <child-id>
pasture task dep tree <id>

pasture task label add <id> <label>
pasture task label remove <id> <label>

pasture task comment add <id> "Comment text"
pasture task comments <id>
```

### 8.3 Default Namespace

The CLI derives the default namespace from the git remote URL of the current repository, falling back to the directory name if no git remote is configured.

```go
// DefaultNamespace returns a namespace derived from the git remote.
// Example: "git@github.com:dayvidpham/providence.git" -> "providence"
// Fallback: current directory name.
func DefaultNamespace() string {
    // 1. Try: git remote get-url origin
    // 2. Parse repo name from URL (strip .git suffix, take last path component)
    // 3. Fallback: filepath.Base(os.Getwd())
}
```

This is a pasture concern, not a providence library concern. Providence's `Create()` method requires an explicit namespace parameter.

### 8.4 Storage Location

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

### 8.5 Pasture Handler Pattern

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
    tracker, err := providence.OpenSQLite(dbPath)
    if err != nil { return err }
    defer tracker.Close()

    // Parse flags, call tracker.Create, format output
    task, err := tracker.Create(namespace, args[0], description, taskType, priority, phase)
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
- `enums.go` — all 9 enum types (Status, Priority, TaskType, EdgeKind, AgentKind, Provider, Role, Phase, Stage) with iota, String(), MarshalText/UnmarshalText, IsValid()
- `types.go` — TaskID, AgentID, ActivityID, CommentID (with Parse/String using LastIndex), Task, Agent (TPT: Agent/HumanAgent/MLAgent/SoftwareAgent), MLModel, Activity, Edge, Label, Comment, UpdateFields, ListFilter
- `errors.go` — ErrNotFound, ErrCycleDetected, ErrAlreadyClosed, ErrInvalidID, ErrAgentKindMismatch
- `enums_test.go` — round-trip marshal/unmarshal, IsValid boundaries, String coverage for all 9 enums
- `types_test.go` — Parse/String round-trip for all ID types, edge cases (empty namespace, malformed UUID, namespace containing "--")

**Exit criteria:** `go test -race ./...` passes. All enum values round-trip through MarshalText/UnmarshalText. All ID types round-trip through String/Parse. ParseTaskID correctly handles namespaces with "--" via LastIndex.

### Slice 2: SQLite Persistence Layer

**Scope:** Database open/close, schema creation, raw CRUD operations for all tables including agent TPT. No graph library yet.

**Deliverables:**
- `internal/sqlite/db.go` — Open, Close, WAL config, foreign keys
- `internal/sqlite/schema.go` — ensureSchema with all CREATE TABLE/INDEX statements (9 enum lookup tables + ml_models + agents TPT + tasks + edges + activities + labels + comments)
- `internal/sqlite/tasks.go` — InsertTask, GetTask, UpdateTask, ListTasks, CloseTask
- `internal/sqlite/edges.go` — InsertEdge, DeleteEdge, EdgesByTask, EdgesByKind
- `internal/sqlite/agents.go` — Agent TPT operations: InsertHumanAgent, InsertMLAgent, InsertSoftwareAgent, GetAgent, GetHumanAgent, GetMLAgent, GetSoftwareAgent
- `internal/sqlite/activities.go` — InsertActivity, UpdateActivity, ListActivities
- `internal/sqlite/labels.go` — AddLabel, RemoveLabel, LabelsByTask, TasksByLabel
- `internal/sqlite/comments.go` — AddComment, CommentsByTask
- `internal/sqlite/db_test.go` — integration tests: CRUD round-trips, agent TPT insert/query, foreign key enforcement, WAL mode verification, ml_models lookup

**Exit criteria:** All CRUD operations work against a real SQLite database (in-memory `:memory:` for tests). Agent TPT correctly inserts into base + child table in a transaction. Foreign keys enforced. Schema idempotent (run twice without error). `phase_id NOT NULL` enforced on tasks.

### Slice 3: Graph Store Implementation

**Scope:** Implement `dominikbraun/graph.Store[TaskID, Task]` backed by Slice 2's SQLite layer. Wire up cycle prevention.

**Deliverables:**
- `internal/graph/store.go` — Store implementation: AddVertex, Vertex, RemoveVertex, ListVertices, VertexCount, AddEdge, Edge, RemoveEdge + adjacency/predecessor map methods
- `internal/helpers/ancestors.go` — Ancestors, Descendants functions
- `internal/graph/store_test.go` — Store contract tests: add/remove vertices and edges, cycle detection, vertex count
- `internal/helpers/ancestors_test.go` — ancestor/descendant traversal tests with known graph topologies

**Exit criteria:** Graph operations work through SQLite. `PreventCycles()` rejects cyclic blocked-by edges. Ancestors/Descendants return correct transitive closures.

### Slice 4: Tracker Facade + Dependency Queries

**Scope:** The public `Tracker` interface and `sqliteTracker` implementation that wires together graph + SQLite. All CRUD, edge management, readiness queries, agent TPT operations.

**Deliverables:**
- `providence.go` — Package doc, Tracker interface, OpenSQLite, OpenMemory constructors
- `tracker.go` — `sqliteTracker` struct implementing Tracker: Create, Show, Update, CloseTask, List, AddEdge, RemoveEdge, Edges, Blocked, Ready, DepTree, Ancestors, Descendants, AddLabel, RemoveLabel, Labels, AddComment, Comments, RegisterHumanAgent, RegisterMLAgent, RegisterSoftwareAgent, Agent, HumanAgent, MLAgent, SoftwareAgent, StartActivity, EndActivity, Activities
- `tracker_test.go` — Integration tests exercising the full stack: create tasks with phases, add edges, verify readiness, close blockers, verify readiness changes, agent TPT registration and retrieval, OpenMemory convenience

**Exit criteria:** Full public API works end-to-end against a real SQLite database. OpenMemory() works identically to OpenSQLite(":memory:"). Readiness correctly reflects blocked-by edge state. Cycle detection works through the public API. Agent TPT registration and typed retrieval work correctly. Phase filtering works in List().

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

1. **Integration over unit:** Test through the public `Tracker` interface whenever possible. Internal packages have their own tests only where the public API does not exercise a code path.
2. **Real SQLite:** Use `OpenMemory()` (`:memory:` SQLite databases) in tests — never mock the database. The database IS the system under test. Same code path as production.
3. **No mocking of providence internals:** The `Tracker` is not mocked. Tests exercise the real code path from public API through graph library through SQLite.
4. **Race detection:** All tests run with `-race`.

### 10.2 Test Categories

| Category | Package | What |
|----------|---------|------|
| Enum round-trip | `providence_test` | MarshalText/UnmarshalText for all 9 enum types, IsValid boundaries, unknown values |
| ID round-trip | `providence_test` | Parse/String for TaskID, AgentID, ActivityID, CommentID; edge cases (empty namespace, bad UUID, wrong separator, namespace with "--") |
| SQLite CRUD | `internal/sqlite` (white-box) | Insert/Get/Update/Delete for every table; agent TPT operations; foreign key enforcement; schema idempotency |
| Agent TPT | `internal/sqlite` (white-box) | Insert base + child in transaction; type-specific retrieval; kind mismatch errors |
| Graph Store | `internal/graph` (black-box) | Store interface contract: add/remove vertices, add/remove edges, cycle rejection, vertex/edge count |
| Traversal | `internal/helpers` (black-box) | Ancestors/Descendants on known topologies (linear chain, diamond, disconnected components) |
| Tracker integration | `providence_test` | Full lifecycle: create tasks with phases, add edges, verify readiness, close tasks, verify readiness changes, labels, comments, agent TPT, cross-entity edges |
| Readiness | `providence_test` | Edge cases: task with no edges is ready, task with all-closed blockers is ready, task with one open blocker is blocked, closing last blocker makes task ready |
| Cycle detection | `providence_test` | Direct cycle (A blocks B blocks A), indirect cycle (A->B->C->A), self-loop |
| Phase filtering | `providence_test` | List with Phase filter returns only matching tasks; PhaseUnscoped for generic tasks |

### 10.3 BDD Acceptance Criteria

**Scenario: Task readiness reflects blocked-by state**

- **Given** task A is open with PhaseRequest and task B is open with PhaseElicit
- **When** AddEdge(A, B.String(), EdgeBlockedBy) is called (A is blocked by B)
- **Then** Ready() returns B but not A, and Blocked() returns A but not B
- **Should not** include A in Ready() results while B is open

**Scenario: Closing a blocker unblocks the dependent**

- **Given** task A is blocked by task B (EdgeBlockedBy)
- **When** CloseTask(B, "done") is called
- **Then** Ready() returns A, and Blocked() returns neither A nor B
- **Should not** leave A in Blocked() after its only blocker is closed

**Scenario: Cycle prevention on blocked-by edges**

- **Given** task A is blocked by task B
- **When** AddEdge(B, A.String(), EdgeBlockedBy) is called (B is blocked by A — creating a cycle)
- **Then** AddEdge returns ErrCycleDetected
- **Should not** persist the cyclic edge in the database

**Scenario: Non-blocked-by edges do not affect readiness**

- **Given** task A has EdgeDerivedFrom pointing to task B, and no blocked-by edges
- **When** Ready() is called
- **Then** both A and B appear in the Ready() results
- **Should not** treat EdgeDerivedFrom as a blocking dependency

**Scenario: Cross-entity edges (attributed-to)**

- **Given** task A exists and ML agent X is registered
- **When** AddEdge(A, X.Agent.ID.String(), EdgeAttributedTo) is called
- **Then** Edges(A, &EdgeAttributedTo) returns an edge with TargetID == X.Agent.ID.String()
- **Should not** reject the edge due to missing task FK on target_id

**Scenario: TaskID round-trip with "--" in namespace**

- **Given** a TaskID with namespace "my--project" and a valid UUIDv7
- **When** String() is called and the result is passed to ParseTaskID()
- **Then** the parsed TaskID has Namespace "my--project" and the correct UUID
- **Should not** split on the first "--" and corrupt the namespace

**Scenario: Enum marshal round-trip**

- **Given** every valid value for each of the 9 enum types
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

**Scenario: Agent TPT registration and retrieval**

- **Given** an empty database with schema applied
- **When** RegisterMLAgent is called with namespace "test", RoleWorker, ProviderAnthropic, "claude_opus_4"
- **Then** MLAgent(id) returns an MLAgent with the correct Role, Model.Provider, and Model.Name
- **Should not** allow HumanAgent(id) to succeed for an ML agent ID (returns ErrAgentKindMismatch)

**Scenario: Task phase_id is required**

- **Given** an empty database with schema applied
- **When** Create is called with PhasePropose
- **Then** the returned Task has Phase == PhasePropose
- **Should not** allow a task to be created without a valid phase

**Scenario: OpenMemory same code path**

- **Given** OpenMemory() is called
- **When** tasks are created and queried
- **Then** all operations behave identically to OpenSQLite(":memory:")
- **Should not** use a different code path or backend

---

## 11. Dependencies

### go.mod

```
module github.com/dayvidpham/providence

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
| ID parsing | Split on first "--" vs. split on last "--" | Last "--" (strings.LastIndex) | **UAT decision.** Namespaces may contain "--" themselves. Parsing from the right ensures the UUID suffix (fixed format) is correctly separated. |
| Enum pattern | String constants (pasture style) vs. iota | iota | URD decision. Iota provides integer efficiency for in-memory operations and database storage while MarshalText/UnmarshalText handles human-readable serialization. |
| Edge storage | Separate table per edge kind vs. single table | Single table with kind column | Simpler schema, easier queries ("all edges for task X"). The kind column with an index handles filtering efficiently at <10K scale. |
| Edge target FK | FK to tasks(id) on target_id vs. no FK | No FK on target_id | **UAT decision.** EdgeGeneratedBy targets activities, EdgeAttributedTo targets agents. A single FK to tasks would reject valid cross-entity edges. Source always references tasks. |
| Blocked-by subgraph | Load all edges into graph vs. only blocked-by | Only blocked-by | Cycle prevention and topo sort are meaningless for metadata edges. Loading only blocked-by edges keeps the graph small and semantically correct. |
| Public API shape | Concrete struct vs. exported interface | Exported Tracker interface | **UAT decision.** Interface + `OpenSQLite()`/`OpenMemory()` constructors. Internal `sqliteTracker` implements it. Enables consumers to define test doubles and future backends. |
| In-memory backend | Separate map-based store vs. SQLite :memory: | SQLite :memory: (same code path) | **UAT decision.** OpenMemory() = OpenSQLite(":memory:"). One code path eliminates divergence between test and production behavior. |
| Agent model | Single agents table vs. TPT hierarchy | TPT: base agents + 3 child tables | **UAT decision.** Human/ML/Software agents have fundamentally different attributes. TPT avoids nullable columns and maintains type safety at the schema level. |
| ML model tracking | Iota enum (Model) vs. lookup table (ml_models) | Composite lookup table (provider_id, name) | **UAT decision.** Models have two axes: provider and name. A flat iota can't capture this. The closed lookup table keeps referential integrity while supporting the composite key. |
| Role placement | Role on Activity vs. Role on Agent | Role on ML Agent | **UAT decision.** Same model with different roles = different agent registrations. This is cleaner than attaching role to every activity, and matches how agents actually work in the protocol. |
| Task phase | Optional phase vs. required phase_id | Required phase_id (NOT NULL) | **UAT decision.** Every task exists within a phase context. PhaseUnscoped (value 12) serves as the phase for generic tasks not tied to a specific protocol phase. |
| Protocol artifacts | Distinct TaskType per artifact vs. generic TaskType + phase_id | Generic TaskType + phase_id | **UAT decision.** Bug/Feature/Task/Epic/Chore are orthogonal to protocol phase. A REQUEST is a Task of type Feature in PhaseRequest. Phase_id distinguishes protocol artifacts without polluting TaskType. |
| Default namespace | Explicit always vs. derived from context | Git remote-derived default (in CLI) | **UAT decision.** The CLI derives namespace from git remote URL, falling back to directory name. Providence library requires explicit namespace — the default logic lives in pasture. |
| PROV-O scope | Tasks only (add Agent/Activity later) vs. full PROV-O | Full PROV-O from day 1 | URD decision. Agent TPT and Activity are essential from the start. Adding them later would require schema migration and API changes. |

---

## 13. Validation Checklist

- [ ] All 9 enum types implement String(), MarshalText(), UnmarshalText(), IsValid()
- [ ] TaskID, AgentID, ActivityID, CommentID are distinct types (compile-time safety)
- [ ] All Parse*() functions use strings.LastIndex for right-to-left "--" splitting
- [ ] ParseTaskID correctly handles namespaces containing "--"
- [ ] SQLite schema creates all entity tables (agents TPT, tasks, edges, activities, labels, comments)
- [ ] SQLite schema creates all 9 enum lookup tables + ml_models composite lookup table
- [ ] All enum columns use INTEGER with FK references to lookup tables (no string literals in queries)
- [ ] Lookup tables seeded via INSERT OR IGNORE on every Open()
- [ ] Schema is BCNF: every non-trivial FD has a superkey determinant (FD annotations inline)
- [ ] Agent TPT: base agents table has kind_id discriminator; 3 child tables with PK=FK to agents(id)
- [ ] agents_human, agents_ml, agents_software use WITHOUT ROWID
- [ ] agents_ml references roles(id) and ml_models(id)
- [ ] ml_models references providers(id) with UNIQUE(provider_id, name)
- [ ] tasks.phase_id is NOT NULL with FK to phases(id)
- [ ] PhaseUnscoped (value 12) exists in phases seed data
- [ ] edges.target_id has NO FK constraint (supports cross-entity edges)
- [ ] edges.source_id has FK to tasks(id) (source is always a task)
- [ ] WAL mode and busy_timeout configured on every Open()
- [ ] dominikbraun/graph Store implementation passes all Store contract tests
- [ ] PreventCycles rejects cyclic blocked-by edges
- [ ] Only EdgeBlockedBy edges are loaded into the graph (other kinds bypass it)
- [ ] Ready() returns tasks with no open blocked-by predecessors
- [ ] Blocked() returns tasks with at least one open blocked-by predecessor
- [ ] Ancestors/Descendants compute correct transitive closures
- [ ] Tracker is an exported interface, not a concrete struct
- [ ] OpenSQLite(dbPath) and OpenMemory() both return Tracker interface
- [ ] OpenMemory() delegates to OpenSQLite(":memory:") — same code path
- [ ] RegisterMLAgent looks up model by (provider, name), not raw integer ID
- [ ] Agent(), HumanAgent(), MLAgent(), SoftwareAgent() retrieve typed agents correctly
- [ ] ErrAgentKindMismatch returned when querying wrong agent kind
- [ ] `CGO_ENABLED=0 go build ./...` succeeds
- [ ] `go test -race ./...` passes
- [ ] All errors are actionable (what, why, where, when, fix)
- [ ] go.mod contains only approved dependencies (dominikbraun/graph, google/uuid, zombiezen SQLite)

---

## 14. Open Questions

1. **zombiezen SQLite Store compatibility:** The dominikbraun/graph Store interface expects synchronous method calls. zombiezen's API uses explicit connection objects rather than database/sql's pool. The Store implementation will need to manage a dedicated connection or use zombiezen's connection pool. This is a detailed design decision for Slice 3.

2. **JSON export/import:** Beads uses JSONL for git-friendly export. Providence will need a similar mechanism eventually, but it is not in MVP scope. The SQLite database is the source of truth; JSONL export is a follow-up feature.

3. **ML model seed extensibility:** The ml_models lookup table ships with 3 Anthropic models. Adding models from other providers (Google, OpenAI) can be done by extending the seed data in future versions without schema changes.

---

## Appendix A: Dependency Chain (Beads)

```
REQUEST (aura-plugins-oviik)
  └── blocked by ELICIT (aura-plugins-4fejd)
        └── blocked by PROPOSAL-2 (to be assigned)  <-- this document
              └── blocked by IMPL_PLAN (to be created after ratification)
                    ├── blocked by Slice 1: Type Foundation
                    ├── blocked by Slice 2: SQLite Persistence
                    ├── blocked by Slice 3: Graph Store
                    ├── blocked by Slice 4: Tracker Facade
                    └── blocked by Slice 5: Makefile + CI
```

## Appendix B: UAT Decision Traceability

| # | UAT Decision | Sections Affected |
|---|-------------|-------------------|
| 1 | Exported Tracker interface | 4.0 (API), 9 Slice 4, 12 Tradeoffs, 13 Checklist |
| 2 | OpenMemory() = OpenSQLite(":memory:") | 4.0 (constructors), 10.1 (test principles), 10.3 (BDD), 12 Tradeoffs |
| 3 | Agent TPT (Human/ML/Software) | 3.3 (entity types), 4.0 (Register*Agent), 6.6 (schema), 9 Slices 1-2-4, 10.2-10.3 |
| 4 | ml_models lookup table | 3.3 (MLModel type), 6.4 (composite lookup), 6.5 (seed), 12 Tradeoffs |
| 5 | providers lookup table | 3.2 (Provider enum), 6.3 (lookup table), 6.5 (seed) |
| 6 | Role on Agent (not Activity) | 3.3 (MLAgent.Role), 6.6 (agents_ml.role_id), 12 Tradeoffs |
| 7 | Task phase_id REQUIRED | 3.3 (Task.Phase), 4.0 (Create signature), 6.6 (tasks.phase_id NOT NULL), 12 Tradeoffs |
| 8 | TaskType stays generic | 3.2 (TaskType unchanged), 12 Tradeoffs |
| 9 | Single edges table, no target FK | 3.3 (Edge type), 6.6 (edges DDL), 10.3 (cross-entity BDD), 12 Tradeoffs |
| 10 | ParseTaskID from right | 3.1 (ParseTaskID impl), 10.3 (BDD "--" in namespace), 12 Tradeoffs |
| 11 | Git remote-derived default namespace | 8.3 (DefaultNamespace), 12 Tradeoffs |
| 12 | All types in root package | 2 (module layout), confirmed unchanged |
