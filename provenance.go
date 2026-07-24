package provenance

// Tracker is the central API for Provenance task management.
// All methods are safe for concurrent use.
// Use OpenSQLite or OpenMemory to obtain an implementation.
type Tracker interface {
	// Close releases all resources held by the tracker.
	// It is safe to call Close multiple times.
	Close() error

	// Journal returns the ordered global-journal surface
	// (docs/journal-relational-contract.md): task-event append, JournalID-ordered
	// queries, the cumulative attribution projection, subtype-integrity
	// verification, and the actor-namespace reservation registry.
	Journal() JournalAPI

	// As binds a committing actor and a governing authority (a bootstrap or
	// assignment authority's JournalID, obtained from a genesis operation or a
	// started assignment episode) and returns a Session: the mutation SDK over the
	// journal. Every Session mutation is a journaled operation: task lifecycle,
	// relationship, and annotation verbs all commit typed effects through Apply
	// (docs/journal-relational-contract.md §6 and §9). See Session.
	As(actor ActorID, authority JournalID) *Session

	// ---------------------------------------------------------------------------
	// Task reads
	// ---------------------------------------------------------------------------
	//
	// Task MUTATIONS (create, update, close) live on the journaled Session SDK
	// (Tracker.As), not on Tracker: every task-lifecycle change flows through the
	// ordered journal so it is authorized and reproducible from history (§8.1, §9).

	// Show retrieves a task by ID.
	// Returns ErrNotFound if no task with that ID exists.
	Show(id TaskID) (Task, error)

	// List returns tasks matching the filter. An empty ListFilter returns all
	// tasks ordered by creation time (ascending).
	List(filter ListFilter) ([]Task, error)

	// ---------------------------------------------------------------------------
	// Typed Dependency Edges
	// ---------------------------------------------------------------------------
	//
	// Edge MUTATIONS (AddEdge/RemoveEdge) live on the Session SDK (Tracker.As) as
	// journaled §6 relationship writes; Edges reads are on Tracker.

	// Edges returns all edges originating from id.
	// If kind is non-nil, only edges of that kind are returned.
	Edges(id TaskID, kind *EdgeKind) ([]Edge, error)

	// AllEdges returns every edge in the graph (all kinds, including
	// EdgeBlockedBy), each with Edge.CreatedAt populated from the edges.created_at
	// column. Rows are ordered deterministically by (source, kind, target). This is
	// a pure read intended for whole-graph consumers such as the PROV-O exporter,
	// which cannot be written against the per-task Edges surface alone.
	AllEdges() ([]Edge, error)

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
	//
	// Label MUTATIONS (AddLabel/RemoveLabel) live on the Session SDK (Tracker.As)
	// as journaled §6 annotation writes; Labels reads are on Tracker.

	// Labels returns all labels attached to a task.
	Labels(id TaskID) ([]string, error)

	// ---------------------------------------------------------------------------
	// Comments
	// ---------------------------------------------------------------------------
	//
	// Comment MUTATION (AddComment) lives on the Session SDK (Tracker.As) as a
	// journaled §6 annotation write; Comments reads are on Tracker.

	// Comments returns all comments on a task in chronological order.
	Comments(id TaskID) ([]Comment, error)

	// ---------------------------------------------------------------------------
	// PROV-O Agents (table-per-type)
	// ---------------------------------------------------------------------------

	// RegisterHumanAgent registers a new human agent with a UUIDv7 ID.
	RegisterHumanAgent(namespace, name, contact string) (HumanAgent, error)

	// RegisterMLAgent registers a new ML agent. The (provider, modelName) pair
	// must exist in the ml_models seed table; returns ErrNotFound if unknown.
	RegisterMLAgent(namespace string, role Role, provider Provider, modelName ModelID) (MLAgent, error)

	// RegisterSoftwareAgent registers a new software agent with a UUIDv7 ID.
	RegisterSoftwareAgent(namespace, name, version, source string) (SoftwareAgent, error)

	// RegisterFixedSoftwareAgent atomically registers a namespace claim, fixed
	// software agent, and manifest entry under one SQLite transaction and lock.
	// Exact retries are inert; missing rows under an exact claim are repaired;
	// conflicting existing rows fail before mutation.
	RegisterFixedSoftwareAgent(reg FixedSoftwareAgentRegistration) (SoftwareAgent, error)

	// AllActors returns every registered actor as a base row (ID + Kind only);
	// dereference detail with the kind-specific getters (HumanAgent/MLAgent/
	// SoftwareAgent). Rows are ordered deterministically by ID. This is a pure read
	// intended for whole-graph consumers such as the PROV-O exporter, which cannot be
	// written against the per-ID Agent surface alone.
	AllActors() ([]Agent, error)

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

	// StartActivityWithID records the start of an activity using a
	// caller-supplied ActivityID, idempotently: a second call with the same id
	// is a no-op (INSERT ... ON CONFLICT(id) DO NOTHING) returning the existing
	// row. Use a deterministic id (e.g. a name-based UUIDv5 over the caller's
	// logical identity) to make activity emission safe to replay, e.g. across
	// durable-workflow recovery. Returns the canonical persisted activity.
	StartActivityWithID(id ActivityID, agentID AgentID, phase Phase, stage Stage, notes string) (Activity, error)

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
//	tr, err := provenance.OpenSQLite(path,
//		provenance.WithModelRegistry(provenance.RegistryFromBestiary(bestiary.Models())))
func OpenSQLite(dbPath string, opts ...Option) (Tracker, error) {
	return openTracker(dbPath, opts...)
}

// OpenMemory creates a Tracker backed by an in-memory SQLite database.
// Useful for tests and ephemeral sessions. The database is destroyed when the
// Tracker is closed.
func OpenMemory(opts ...Option) (Tracker, error) {
	return openTracker(":memory:", opts...)
}
