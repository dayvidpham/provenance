package ptypes

// errSentinel is an unexported error type used for sentinel errors.
// Because the type is unexported, consumers cannot construct values that
// would satisfy errors.Is — even if they reassign the package-level var,
// the original errSentinel value remains distinct via type+value identity.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// Sentinel errors returned by Tracker operations.
// Callers should use errors.Is() to detect these.
var (
	// ErrNotFound is returned when a requested entity does not exist.
	// This occurs in Show, Update, CloseTask, RemoveEdge, HumanAgent, MLAgent,
	// SoftwareAgent, and AddComment when the task/agent/activity ID is unknown.
	ErrNotFound = error(errSentinel("providence: entity not found"))

	// ErrCycleDetected is returned when adding a blocked-by edge would
	// create a cycle in the dependency graph.
	// This is returned by AddEdge with EdgeBlockedBy when the proposed
	// edge would form a cycle. To fix: recheck the dependency direction —
	// the target (child) must be work that finishes BEFORE the source (parent).
	ErrCycleDetected = error(errSentinel("providence: dependency cycle detected"))

	// ErrAlreadyClosed is returned when attempting to close an already-closed task.
	// To fix: check the task's Status before calling CloseTask, or use Update
	// to reopen the task first.
	ErrAlreadyClosed = error(errSentinel("providence: task is already closed"))

	// ErrInvalidID is returned when a string cannot be parsed as a valid ID.
	// The expected wire format is "namespace--uuidv7".
	// To fix: ensure the ID string was produced by TaskID.String(), AgentID.String(),
	// ActivityID.String(), or CommentID.String(), or that the namespace is non-empty
	// and the UUID portion is a valid v7 UUID.
	ErrInvalidID = error(errSentinel("providence: invalid ID format"))

	// ErrAgentKindMismatch is returned when querying a typed agent with the wrong kind.
	// For example, calling HumanAgent() on an ID that belongs to an MLAgent.
	// To fix: call Agent() first to inspect the Kind field, then call the
	// appropriate typed method (HumanAgent, MLAgent, or SoftwareAgent).
	ErrAgentKindMismatch = error(errSentinel("providence: agent kind mismatch"))
)
