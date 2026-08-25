package journal

import (
	"encoding/json"
	"errors"
	"fmt"
)

// operations.go defines the mutation-time semantics types for the operations,
// effects, results, and authority-lifecycle layer.

// ---------------------------------------------------------------------------
// Authority identity and closed lifecycle enums (§4)
// ---------------------------------------------------------------------------

// OperationAuthorityID is the opaque alternate key on journal_authorities.
type OperationAuthorityID string

// AssignmentID is the stable identity of one responsibility-occupancy episode.
type AssignmentID string

// ResultSlotID is the caller's local handle name for an operation-produced row.
type ResultSlotID string

// AuthorityKind is the closed discriminator on journal_authorities.
type AuthorityKind int

const (
	AuthorityKindBootstrap  AuthorityKind = iota // 0: journal_authority_bootstraps
	AuthorityKindAssignment                      // 1: assignment episode/transition detail
)

var authorityKindStrings = [...]string{
	AuthorityKindBootstrap:  "bootstrap",
	AuthorityKindAssignment: "assignment",
}

// AuthorityKinds returns the closed set in id order.
func AuthorityKinds() []AuthorityKind {
	return []AuthorityKind{AuthorityKindBootstrap, AuthorityKindAssignment}
}

func (k AuthorityKind) String() string {
	if int(k) >= 0 && int(k) < len(authorityKindStrings) {
		return authorityKindStrings[k]
	}
	return fmt.Sprintf("AuthorityKind(%d)", int(k))
}

// IsValid reports whether k is one of the two seeded authority kinds.
func (k AuthorityKind) IsValid() bool {
	return k == AuthorityKindBootstrap || k == AuthorityKindAssignment
}

// AssignmentSlotID names a responsibility slot on a task.
type AssignmentSlotID string

// SlotOwnerResponsibility is the one slot seeded today.
const SlotOwnerResponsibility AssignmentSlotID = "owner-responsibility"

// AssignmentTransition is the closed two-value lifecycle transition on an episode.
type AssignmentTransition int

const (
	TransitionStarted AssignmentTransition = iota // 0
	TransitionEnded                               // 1
)

var assignmentTransitionStrings = [...]string{
	TransitionStarted: "started",
	TransitionEnded:   "ended",
}

// AssignmentTransitions returns the closed set in id order.
func AssignmentTransitions() []AssignmentTransition {
	return []AssignmentTransition{TransitionStarted, TransitionEnded}
}

func (t AssignmentTransition) String() string {
	if int(t) >= 0 && int(t) < len(assignmentTransitionStrings) {
		return assignmentTransitionStrings[t]
	}
	return fmt.Sprintf("AssignmentTransition(%d)", int(t))
}

// IsValid reports whether t is one of the two seeded transitions.
func (t AssignmentTransition) IsValid() bool {
	return t == TransitionStarted || t == TransitionEnded
}

// ---------------------------------------------------------------------------
// Effects (§9.3) — the ordered list of journal rows one operation produces
// ---------------------------------------------------------------------------

// EffectSort is the closed set of effect shapes an operation can produce.
type EffectSort int

const (
	EffectTaskEvent          EffectSort = iota // JournalKindTaskEvent
	EffectBootstrapAuthority                   // JournalKindAuthority + AuthorityKindBootstrap
	EffectAssignmentStart                      // JournalKindAuthority + AuthorityKindAssignment, started
	EffectAssignmentEnd                        // JournalKindAuthority + AuthorityKindAssignment, ended
	EffectDecision                             // JournalKindDecision
	EffectEvidence                             // JournalKindEvidence
	// EffectTaskCreate journals the birth of a task.
	EffectTaskCreate // JournalKindTaskEvent (provenance.task.created), also inserts tasks row

	// Journaled relationship / annotation mutation-family effect sorts (§6).
	EffectEdgeAdd     // JournalKindTaskEvent (provenance.edge.added)
	EffectEdgeRemove  // JournalKindTaskEvent (provenance.edge.removed)
	EffectLabelAdd    // JournalKindTaskEvent (provenance.label.added)
	EffectLabelRemove // JournalKindTaskEvent (provenance.label.removed)
	EffectCommentAdd  // JournalKindTaskEvent (provenance.comment.added)
	// EffectTaskCreateAllocated folds like EffectTaskCreate on first execution
	// but marks TaskID.UUID as a provisional Session allocation.
	EffectTaskCreateAllocated

	// EffectActivityCreate journals the immutable birth of one Activity row.
	// ResultSlot is required: it is the proof-of-allocation handle callers use
	// to recover the committed ActivityID via LookupCommitted. The SQLite fold
	// (writing the row, collision detection, and result reconstruction) is
	// implemented in .1.2; this vertical defines the canonical DTO and codec.
	EffectActivityCreate
)

func (s EffectSort) String() string {
	if tag, ok := semanticEffectTag(s); ok {
		return tag
	}
	return fmt.Sprintf("EffectSort(%d)", int(s))
}

// JournalKind maps an effect sort to the supertype discriminator its produced
// journal row carries (§2.1).
func (s EffectSort) JournalKind() (JournalKind, error) {
	if kind, ok := semanticEffectJournalKind(s); ok {
		return kind, nil
	}
	return 0, fmt.Errorf("provenance: no JournalKind for effect sort %s", s)
}

// Effect is one journal row an operation produces, in caller list order (§9.3.1).
type Effect struct {
	Sort EffectSort

	// ResultSlot, when non-empty, is the caller's local handle for this produced row.
	ResultSlot ResultSlotID

	// ActorID must be left zero; the committing actor comes from the operation anchor.
	ActorID ActorID

	// RecordedAtOverride, when non-nil, is a per-effect audit/display timestamp.
	RecordedAtOverride *RecordedTime

	// task_event (EffectTaskEvent)
	TaskID    TaskID
	EventKind EventKind
	Payload   json.RawMessage
	Contexts  []EventContext

	// task_create (EffectTaskCreate / EffectTaskCreateAllocated)
	Title       string
	Description string
	Type        TaskType
	Priority    Priority
	Phase       Phase

	// task update/close materialization (EffectTaskEvent)
	CloseReason       string
	UpdateTitle       *string
	UpdateDescription *string
	UpdatePriority    *Priority
	UpdatePhase       *Phase
	UpdateNotes       *string

	// Forced on a TRANSITION lifecycle event requests the FSM escape hatch.
	Forced bool

	// bootstrap authority (EffectBootstrapAuthority)
	BootstrapLabel       string
	OperationAuthorityID OperationAuthorityID

	// assignment start/end (EffectAssignmentStart / EffectAssignmentEnd)
	AssignmentID AssignmentID
	SlotID       AssignmentSlotID
	Occupant     ActorID
	Predecessor  AssignmentID
	Parent       AssignmentID

	// decision (EffectDecision) / evidence (EffectEvidence)
	DecisionKind  DecisionKind
	EvidenceKind  EvidenceKind
	ContentDigest []byte

	// activity_create (EffectActivityCreate): immutable birth of one Activity row.
	// ResultSlot (above) is mandatory — it is the caller's proof-of-allocation handle.
	// Fields match StartActivityWithID(id ActivityID, agentID AgentID, phase Phase, stage Stage, notes string).
	ActivityID      ActivityID // stable identity of the new activity
	ActivityAgentID AgentID    // agent responsible for this activity episode (alias for ActorID; same wire format)
	ActivityPhase   Phase      // initial phase (e.g. PhaseWorkerSlices)
	ActivityStage   Stage      // initial lifecycle stage (e.g. StageInProgress)
	ActivityNotes   string     // free-text birth note (optional)

	// Relationship / annotation mutation families (§6 amendment).
	EdgeTargetID    string
	EdgeRelKind     EdgeKind
	Label           string
	CommentIdentity CommentID
	CommentAuthor   ActorID
	CommentBody     string
}

// DecisionKind / EvidenceKind are open, validated namespaced strings (§6).
type (
	DecisionKind string
	EvidenceKind string
)

// ---------------------------------------------------------------------------
// Operation input and replay identity (§3.1, §9.4, §11)
// ---------------------------------------------------------------------------

// OperationInput is the validated request to commit one logical operation.
type OperationInput struct {
	OperationID OperationID
	ActorID     ActorID
	// AuthorityJournalID is NULL only on a genesis operation.
	AuthorityJournalID *JournalID
	CommandDigest      []byte
	// MutationDigest is the SHA-256 digest of CanonicalBytes(). Canonicalize
	// derives and sets this from the encoded canonical bytes; do not set it directly.
	MutationDigest []byte
	RecordedAt     RecordedTime // audit/display only (§12)
	Conditions     []Condition
	Effects        []Effect
}

// RecordedTime is the caller-supplied wall-clock stamp (audit/display only §12).
type RecordedTime = int64

// StoredOperationIdentity contains the scalar replay identity.
type StoredOperationIdentity struct {
	ActorID            ActorID
	AuthorityJournalID *JournalID
	CommandDigest      []byte
	MutationDigest     []byte
}

// ---------------------------------------------------------------------------
// LookupCommitted result variants (§3.2, §9.4)
// ---------------------------------------------------------------------------

// CommittedResultKind is the closed variant of a LookupCommitted / Apply result.
type CommittedResultKind int

const (
	CommittedAbsent CommittedResultKind = iota
	CommittedExact
	CommittedConflict
)

func (k CommittedResultKind) String() string {
	switch k {
	case CommittedAbsent:
		return "CommittedAbsent"
	case CommittedExact:
		return "CommittedExact"
	case CommittedConflict:
		return "CommittedConflict"
	default:
		return fmt.Sprintf("CommittedResultKind(%d)", int(k))
	}
}

// ResultSlotBinding is one reconstructed ResultSlotID→produced-row mapping.
// For task_event slots TaskID is resolved; for activity_create slots ActivityID is resolved.
type ResultSlotBinding struct {
	Slot              ResultSlotID
	ProducedJournalID JournalID
	Kind              JournalKind
	TaskID            *TaskID     // non-nil for JournalKindTaskEvent
	ActivityID        *ActivityID // non-nil for JournalKindActivity
}

// CommittedResult is the closed result of Apply and LookupCommitted.
type CommittedResult struct {
	Kind            CommittedResultKind
	AnchorJournalID JournalID
	EmittedEvents   []JournalID // task_event closure, JournalID order (§2.1)
	ResultSlots     []ResultSlotBinding
	// ShortCircuited is true when a §9.4 idempotent-replay retry returned this
	// already-committed result without folding effects.
	ShortCircuited bool
	Conflict       *OperationConflict
}

// ---------------------------------------------------------------------------
// Errors (§13.1 actionable shape where the contract requires)
// ---------------------------------------------------------------------------

var (
	// ErrOperationConflict classifies operation-identity admission conflicts. A
	// committed OperationID-reuse conflict also carries *OperationConflict details
	// (§11, §9.6); rejection of a reserved reducer-owned identity wraps this
	// sentinel only because no conflicting committed identity is being compared.
	ErrOperationConflict = errors.New("provenance: operation identity conflict")
	// ErrConditionFailed wraps a typed pre-condition failure (§9.5).
	ErrConditionFailed = errors.New("provenance: journal condition failed")
	// ErrActivityConflict wraps a typed ActivityID collision in an ActivityCreate fold.
	ErrActivityConflict = errors.New("provenance: activity identity conflict")
	// ErrGenesis is returned for genesis-discipline violations (§4.6).
	ErrGenesis = errors.New("provenance: genesis authority discipline violated")
	// ErrAuthorityScope is returned when an authority does not govern the effect's task.
	ErrAuthorityScope = errors.New("provenance: authority does not govern the effect's task")
	// ErrAssignmentLifecycle is returned for assignment-transition lifecycle order violations.
	ErrAssignmentLifecycle = errors.New("provenance: assignment transition lifecycle order violated")
	// ErrOrphanedEvidence is returned for invalid predecessor evidence.
	ErrOrphanedEvidence = errors.New("provenance: predecessor assignment evidence invalid")
	// ErrStaleEpisode is returned to the losing racer of a concurrent transfer CAS.
	ErrStaleEpisode = errors.New("provenance: assignment episode is no longer active")
	// ErrResultSlotIntegrity is returned when a result slot references a foreign row.
	ErrResultSlotIntegrity = errors.New("provenance: result slot references a foreign operation's produced row")
	// ErrCloseWithoutEnding is returned when a task is closed without ending its episode.
	ErrCloseWithoutEnding = errors.New("provenance: task closed without ending its active owner-responsibility episode")
	// ErrParentCitation is returned for invalid assignment parent citations.
	ErrParentCitation = errors.New("provenance: assignment parent citation invalid")
	// ErrCorruptParentChain is returned when a cyclic parent chain is detected in stored data.
	ErrCorruptParentChain = errors.New("provenance: corrupt cyclic assignment parent-citation chain")
)
