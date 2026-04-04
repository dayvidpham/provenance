package provenance

// reexports.go re-exports all public symbols from pkg/ptypes so that
// existing consumers of "github.com/dayvidpham/provenance" continue to work
// without any import changes.
//
// Type aliases are transparent: provenance.TaskID and ptypes.TaskID are the
// same type. Constants are re-declared with the same values. Sentinel errors
// and parse functions are re-exported as package-level vars/funcs.

import (
	"github.com/dayvidpham/provenance/pkg/namespace"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// ---------------------------------------------------------------------------
// Type aliases (transparent — identical to the ptypes originals)
// ---------------------------------------------------------------------------

// Enum types
type (
	Status    = ptypes.Status
	Priority  = ptypes.Priority
	TaskType  = ptypes.TaskType
	EdgeKind  = ptypes.EdgeKind
	AgentKind = ptypes.AgentKind
	Provider  = ptypes.Provider
	Role      = ptypes.Role
	Phase     = ptypes.Phase
	Stage     = ptypes.Stage
)

// ID types
type (
	TaskID     = ptypes.TaskID
	AgentID    = ptypes.AgentID
	ActivityID = ptypes.ActivityID
	CommentID  = ptypes.CommentID
)

// Entity types
type (
	Task          = ptypes.Task
	Agent         = ptypes.Agent
	HumanAgent    = ptypes.HumanAgent
	MLAgent       = ptypes.MLAgent
	SoftwareAgent = ptypes.SoftwareAgent
	MLModel       = ptypes.MLModel
	Activity      = ptypes.Activity
	Edge          = ptypes.Edge
	Label         = ptypes.Label
	Comment       = ptypes.Comment
)

// Model registry types
type (
	ModelEntry    = ptypes.ModelEntry
	ModelRegistry = ptypes.ModelRegistry
)

// Supporting types
type (
	UpdateFields = ptypes.UpdateFields
	ListFilter   = ptypes.ListFilter
)

// ---------------------------------------------------------------------------
// Constant re-exports
// ---------------------------------------------------------------------------

// Status constants
const (
	StatusOpen       = ptypes.StatusOpen
	StatusInProgress = ptypes.StatusInProgress
	StatusClosed     = ptypes.StatusClosed
)

// Priority constants
const (
	PriorityCritical = ptypes.PriorityCritical
	PriorityHigh     = ptypes.PriorityHigh
	PriorityMedium   = ptypes.PriorityMedium
	PriorityLow      = ptypes.PriorityLow
	PriorityBacklog  = ptypes.PriorityBacklog
)

// TaskType constants
const (
	TaskTypeBug     = ptypes.TaskTypeBug
	TaskTypeFeature = ptypes.TaskTypeFeature
	TaskTypeTask    = ptypes.TaskTypeTask
	TaskTypeEpic    = ptypes.TaskTypeEpic
	TaskTypeChore   = ptypes.TaskTypeChore
)

// EdgeKind constants
const (
	EdgeBlockedBy      = ptypes.EdgeBlockedBy
	EdgeDerivedFrom    = ptypes.EdgeDerivedFrom
	EdgeSupersedes     = ptypes.EdgeSupersedes
	EdgeDiscoveredFrom = ptypes.EdgeDiscoveredFrom
	EdgeGeneratedBy    = ptypes.EdgeGeneratedBy
	EdgeAttributedTo   = ptypes.EdgeAttributedTo
)

// AgentKind constants
const (
	AgentKindHuman           = ptypes.AgentKindHuman
	AgentKindMachineLearning = ptypes.AgentKindMachineLearning
	AgentKindSoftware        = ptypes.AgentKindSoftware
)

// Provider constants
const (
	ProviderAnthropic = ptypes.ProviderAnthropic
	ProviderGoogle    = ptypes.ProviderGoogle
	ProviderOpenAI    = ptypes.ProviderOpenAI
	ProviderLocal     = ptypes.ProviderLocal
)

// Role constants
const (
	RoleHuman      = ptypes.RoleHuman
	RoleArchitect  = ptypes.RoleArchitect
	RoleSupervisor = ptypes.RoleSupervisor
	RoleWorker     = ptypes.RoleWorker
	RoleReviewer   = ptypes.RoleReviewer
)

// Phase constants
const (
	PhaseRequest      = ptypes.PhaseRequest
	PhaseElicit       = ptypes.PhaseElicit
	PhasePropose      = ptypes.PhasePropose
	PhaseReview       = ptypes.PhaseReview
	PhasePlanUAT      = ptypes.PhasePlanUAT
	PhaseRatify       = ptypes.PhaseRatify
	PhaseHandoff      = ptypes.PhaseHandoff
	PhaseImplPlan     = ptypes.PhaseImplPlan
	PhaseWorkerSlices = ptypes.PhaseWorkerSlices
	PhaseCodeReview   = ptypes.PhaseCodeReview
	PhaseImplUAT      = ptypes.PhaseImplUAT
	PhaseLanding      = ptypes.PhaseLanding
	PhaseUnscoped     = ptypes.PhaseUnscoped
)

// Stage constants
const (
	StageNotStarted = ptypes.StageNotStarted
	StageInProgress = ptypes.StageInProgress
	StageBlocked    = ptypes.StageBlocked
	StageComplete   = ptypes.StageComplete
)

// ---------------------------------------------------------------------------
// Sentinel error re-exports
// ---------------------------------------------------------------------------

var (
	ErrNotFound          = ptypes.ErrNotFound
	ErrCycleDetected     = ptypes.ErrCycleDetected
	ErrAlreadyClosed     = ptypes.ErrAlreadyClosed
	ErrInvalidID         = ptypes.ErrInvalidID
	ErrAgentKindMismatch = ptypes.ErrAgentKindMismatch
)

// ---------------------------------------------------------------------------
// Parse function re-exports
// ---------------------------------------------------------------------------

// ParseTaskID parses "namespace--uuid" into a TaskID.
// See ptypes.ParseTaskID for full documentation.
func ParseTaskID(s string) (TaskID, error) {
	return ptypes.ParseTaskID(s)
}

// ParseAgentID parses "namespace--uuid" into an AgentID.
// See ptypes.ParseAgentID for full documentation.
func ParseAgentID(s string) (AgentID, error) {
	return ptypes.ParseAgentID(s)
}

// ParseActivityID parses "namespace--uuid" into an ActivityID.
// See ptypes.ParseActivityID for full documentation.
func ParseActivityID(s string) (ActivityID, error) {
	return ptypes.ParseActivityID(s)
}

// ParseCommentID parses "namespace--uuid" into a CommentID.
// See ptypes.ParseCommentID for full documentation.
func ParseCommentID(s string) (CommentID, error) {
	return ptypes.ParseCommentID(s)
}

// ---------------------------------------------------------------------------
// Namespace re-exports
// ---------------------------------------------------------------------------

// DefaultNamespace derives a namespace URI from the current git repo's
// remote URL, falling back to a file:// URI of the working directory.
// See namespace.DefaultNamespace for full documentation.
var DefaultNamespace = namespace.DefaultNamespace

// FromGitRemote normalizes a git remote URL to a canonical HTTPS URI.
// See namespace.FromGitRemote for full documentation.
var FromGitRemote = namespace.FromGitRemote

// FromDirectory returns a file:// URI for the given directory path.
// See namespace.FromDirectory for full documentation.
var FromDirectory = namespace.FromDirectory

// ErrNoRemote is returned by FromGitRemote when the remote URL is empty.
// See namespace.ErrNoRemote for full documentation.
var ErrNoRemote = namespace.ErrNoRemote
