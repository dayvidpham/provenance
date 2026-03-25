package ptypes

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// ID Types
// ---------------------------------------------------------------------------

// namespacedID is a type constraint for all ID types that have
// Namespace and UUID fields. Used by parseID to deduplicate parse logic.
type namespacedID interface {
	TaskID | AgentID | ActivityID | CommentID
}

// parseID is a generic helper that parses "namespace--uuid" into any ID type
// matching the namespacedID constraint.
//
// It splits on the rightmost "--" separator (handles namespaces containing "--"),
// validates that the namespace is non-empty, and parses the UUID portion.
// The caller parameter is used in error messages (e.g. "ParseTaskID").
// Returns ErrInvalidID on any parse failure.
func parseID[T namespacedID](raw, caller string) (T, error) {
	var zero T
	idx := strings.LastIndex(raw, "--")
	if idx < 0 {
		return zero, fmt.Errorf("%w: %s — no '--' separator found in %q — "+
			"expected format is 'namespace--uuidv7'", ErrInvalidID, caller, raw)
	}
	ns := raw[:idx]
	if ns == "" {
		return zero, fmt.Errorf("%w: %s — empty namespace in %q — "+
			"namespace must be non-empty, e.g. 'aura-plugins--<uuid>'", ErrInvalidID, caller, raw)
	}
	u, err := uuid.Parse(raw[idx+2:])
	if err != nil {
		return zero, fmt.Errorf("%w: %s — invalid UUID in %q: %v — "+
			"the part after the last '--' must be a valid UUID", ErrInvalidID, caller, raw, err)
	}
	// Type switch to construct the correct concrete type.
	// This is safe because T is constrained to our known ID types.
	var result any
	switch any(zero).(type) {
	case TaskID:
		result = TaskID{Namespace: ns, UUID: u}
	case AgentID:
		result = AgentID{Namespace: ns, UUID: u}
	case ActivityID:
		result = ActivityID{Namespace: ns, UUID: u}
	case CommentID:
		result = CommentID{Namespace: ns, UUID: u}
	}
	return result.(T), nil
}

// TaskID uniquely identifies a task (PROV-O Entity).
// The Namespace scopes the ID to a project (e.g., "aura-plugins").
// The UUID is a UUIDv7 (time-sortable, globally unique).
// Wire format: "namespace--uuid".
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
	return parseID[TaskID](s, "ParseTaskID")
}

// AgentID uniquely identifies an agent (PROV-O Agent).
// Wire format: "namespace--uuid".
type AgentID struct {
	Namespace string
	UUID      uuid.UUID
}

// String returns the wire format: "namespace--uuid".
func (id AgentID) String() string {
	return id.Namespace + "--" + id.UUID.String()
}

// ParseAgentID parses "namespace--uuid" into an AgentID.
// Uses strings.LastIndex to split on the rightmost "--" separator.
// Returns ErrInvalidID if the format is invalid or the UUID is malformed.
func ParseAgentID(s string) (AgentID, error) {
	return parseID[AgentID](s, "ParseAgentID")
}

// ActivityID uniquely identifies an activity (PROV-O Activity).
// Wire format: "namespace--uuid".
type ActivityID struct {
	Namespace string
	UUID      uuid.UUID
}

// String returns the wire format: "namespace--uuid".
func (id ActivityID) String() string {
	return id.Namespace + "--" + id.UUID.String()
}

// ParseActivityID parses "namespace--uuid" into an ActivityID.
// Uses strings.LastIndex to split on the rightmost "--" separator.
// Returns ErrInvalidID if the format is invalid or the UUID is malformed.
func ParseActivityID(s string) (ActivityID, error) {
	return parseID[ActivityID](s, "ParseActivityID")
}

// CommentID uniquely identifies a comment.
// Wire format: "namespace--uuid".
type CommentID struct {
	Namespace string
	UUID      uuid.UUID
}

// String returns the wire format: "namespace--uuid".
func (id CommentID) String() string {
	return id.Namespace + "--" + id.UUID.String()
}

// ParseCommentID parses "namespace--uuid" into a CommentID.
// Uses strings.LastIndex to split on the rightmost "--" separator.
// Returns ErrInvalidID if the format is invalid or the UUID is malformed.
func ParseCommentID(s string) (CommentID, error) {
	return parseID[CommentID](s, "ParseCommentID")
}

// ---------------------------------------------------------------------------
// Entity Types
// ---------------------------------------------------------------------------

// Task represents a work product (PROV-O Entity).
// Every task has a required Phase — use PhaseUnscoped for generic tasks.
type Task struct {
	ID          TaskID     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      Status     `json:"status"`
	Priority    Priority   `json:"priority"`
	Type        TaskType   `json:"type"`
	Phase       Phase      `json:"phase"`           // Required — protocol artifacts distinguished by phase
	Owner       *AgentID   `json:"owner,omitempty"` // nil if unassigned
	Notes       string     `json:"notes,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ClosedAt    *time.Time `json:"closedAt,omitempty"`
	CloseReason string     `json:"closeReason,omitempty"`
}

// Agent is the base type for all agents (PROV-O Agent).
// Use Kind to determine which typed agent to query.
//
// Agents use table-per-type (TPT) inheritance in SQLite:
//   - Base: agents table (id, kind_id)
//   - Human: agents_human (agent_id, name, contact)
//   - ML:    agents_ml (agent_id, role_id, model_id)
//   - Software: agents_software (agent_id, name, version, source)
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

// ---------------------------------------------------------------------------
// Supporting Types for Tracker API
// ---------------------------------------------------------------------------

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
	Phase     *Phase // Filter by protocol phase
	Label     string // empty means no label filter
	Namespace string // empty means all namespaces
}
