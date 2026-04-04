package provenance

// Tracker is the central API for Provenance task management.
// All methods are safe for concurrent use.
// Use OpenSQLite or OpenMemory to obtain an implementation.
type Tracker interface {
	// Close releases all resources held by the tracker.
	// It is safe to call Close multiple times.
	Close() error

	// ---------------------------------------------------------------------------
	// Task CRUD
	// ---------------------------------------------------------------------------

	// Create creates a new task. A UUIDv7 TaskID with the given namespace is
	// assigned automatically. Returns ErrInvalidID if namespace is empty.
	Create(namespace, title, description string, taskType TaskType, priority Priority, phase Phase) (Task, error)

	// Show retrieves a task by ID.
	// Returns ErrNotFound if no task with that ID exists.
	Show(id TaskID) (Task, error)

	// Update applies partial updates to a task. Only non-nil fields in fields
	// are written. Returns ErrNotFound if the task does not exist.
	Update(id TaskID, fields UpdateFields) (Task, error)

	// CloseTask marks a task as closed with the given reason.
	// Returns ErrNotFound if the task does not exist.
	// Returns ErrAlreadyClosed if the task is already closed.
	CloseTask(id TaskID, reason string) (Task, error)

	// List returns tasks matching the filter. An empty ListFilter returns all
	// tasks ordered by creation time (ascending).
	List(filter ListFilter) ([]Task, error)

	// ---------------------------------------------------------------------------
	// Typed Dependency Edges
	// ---------------------------------------------------------------------------

	// AddEdge creates a typed edge from sourceID to targetID.
	// For EdgeBlockedBy: cycle detection is enforced; returns ErrCycleDetected
	// if the edge would introduce a cycle.
	// For other kinds: the edge is inserted directly without cycle checking.
	AddEdge(sourceID TaskID, targetID string, kind EdgeKind) error

	// RemoveEdge deletes the edge from sourceID to targetID with the given kind.
	// Returns nil if the edge did not exist.
	RemoveEdge(sourceID TaskID, targetID string, kind EdgeKind) error

	// Edges returns all edges originating from id.
	// If kind is non-nil, only edges of that kind are returned.
	Edges(id TaskID, kind *EdgeKind) ([]Edge, error)

	// ---------------------------------------------------------------------------
	// Readiness Queries (blocked-by subgraph only)
	// ---------------------------------------------------------------------------

	// Blocked returns tasks that are not closed and have at least one open blocker.
	Blocked() ([]Task, error)

	// Ready returns tasks that are not closed and have no open blockers.
	Ready() ([]Task, error)

	// DepTree returns all blocked-by edges reachable from id via depth-first
	// traversal. The result is in DFS order.
	DepTree(id TaskID) ([]Edge, error)

	// Ancestors returns all tasks that transitively block the given task.
	// In the blocked-by graph, A→B means "A is blocked by B". Ancestors of A
	// are B and everything B transitively waits for.
	// The given task itself is never included. Returns empty slice if none.
	Ancestors(id TaskID) ([]Task, error)

	// Descendants returns all tasks that are transitively waiting for the given
	// task to complete.
	// In the blocked-by graph, A→B means "A is blocked by B". Descendants of B
	// are A and everything that transitively depends on A.
	// The given task itself is never included. Returns empty slice if none.
	Descendants(id TaskID) ([]Task, error)

	// ---------------------------------------------------------------------------
	// Labels
	// ---------------------------------------------------------------------------

	// AddLabel attaches a label to a task. Idempotent.
	AddLabel(id TaskID, label string) error

	// RemoveLabel detaches a label from a task. Idempotent.
	RemoveLabel(id TaskID, label string) error

	// Labels returns all labels attached to a task.
	Labels(id TaskID) ([]string, error)

	// ---------------------------------------------------------------------------
	// Comments
	// ---------------------------------------------------------------------------

	// AddComment adds a comment to a task authored by authorID.
	// Returns ErrNotFound if the task or author agent does not exist.
	AddComment(id TaskID, authorID AgentID, body string) (Comment, error)

	// Comments returns all comments on a task in chronological order.
	Comments(id TaskID) ([]Comment, error)

	// ---------------------------------------------------------------------------
	// PROV-O Agents (table-per-type)
	// ---------------------------------------------------------------------------

	// RegisterHumanAgent registers a new human agent with a UUIDv7 ID.
	RegisterHumanAgent(namespace, name, contact string) (HumanAgent, error)

	// RegisterMLAgent registers a new ML agent. The (provider, modelName) pair
	// must exist in the ml_models seed table; returns ErrNotFound if unknown.
	RegisterMLAgent(namespace string, role Role, provider Provider, modelName string) (MLAgent, error)

	// RegisterSoftwareAgent registers a new software agent with a UUIDv7 ID.
	RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error)

	// Agent returns the base agent (kind only) by ID.
	// Returns ErrNotFound if the agent does not exist.
	Agent(id AgentID) (Agent, error)

	// HumanAgent returns the human agent by ID.
	// Returns ErrNotFound if not found; ErrAgentKindMismatch if the agent is
	// a different kind.
	HumanAgent(id AgentID) (HumanAgent, error)

	// MLAgent returns the ML agent by ID.
	// Returns ErrNotFound if not found; ErrAgentKindMismatch if the agent is
	// a different kind.
	MLAgent(id AgentID) (MLAgent, error)

	// SoftwareAgent returns the software agent by ID.
	// Returns ErrNotFound if not found; ErrAgentKindMismatch if the agent is
	// a different kind.
	SoftwareAgent(id AgentID) (SoftwareAgent, error)

	// ---------------------------------------------------------------------------
	// PROV-O Activities
	// ---------------------------------------------------------------------------

	// StartActivity records the start of an activity for the given agent.
	// A UUIDv7 ActivityID is assigned automatically.
	StartActivity(agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error)

	// EndActivity records the end time of an activity.
	// Returns ErrNotFound if the activity does not exist.
	EndActivity(id ActivityID) (Activity, error)

	// Activities returns all activities, optionally filtered by agent.
	// Pass nil to return activities for all agents.
	Activities(agentID *AgentID) ([]Activity, error)
}

// OpenSQLite creates a Tracker backed by a SQLite database at dbPath.
// The database file and parent directories are created if they do not exist.
// The schema is applied on every open (idempotent).
//
// Use WithModelRegistry to override the default model registry:
//
//	tr, err := provenance.OpenSQLite(path, provenance.WithModelRegistry(bestiary.Registry()))
func OpenSQLite(dbPath string, opts ...Option) (Tracker, error) {
	return openTracker(dbPath, opts...)
}

// OpenMemory creates a Tracker backed by an in-memory SQLite database.
// Useful for tests and ephemeral sessions. The database is destroyed when the
// Tracker is closed.
func OpenMemory(opts ...Option) (Tracker, error) {
	return openTracker(":memory:", opts...)
}
