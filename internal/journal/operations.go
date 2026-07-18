package journal

import (
	"encoding/json"
	"errors"
	"fmt"
)

// operations.go defines the mutation-time semantics types for the operations,
// effects, results, and authority-lifecycle layer
// (docs/journal-relational-contract.md §2, §3, §4, §9, §14). These are the
// typed shapes the Apply write path and LookupCommitted read path consume; the
// concrete SQLite reducer that folds them lives in internal/sqlite. The write
// path validation is structured as reusable reducer steps so the Open/replay
// reducer of a later slice folds onto them rather than duplicating a second
// switch (§9.2).

// ---------------------------------------------------------------------------
// Authority identity and closed lifecycle enums (§4)
// ---------------------------------------------------------------------------

// OperationAuthorityID is the opaque alternate key on journal_authorities,
// carried as MutationContext.Authority (§4.2). Distinct from OperationID.
type OperationAuthorityID string

// AssignmentID is the stable identity of one responsibility-occupancy episode
// (§4.4). It is invariant across an episode's started/ended transitions; a
// change of task/slot/actor is by definition a new episode (a transfer).
type AssignmentID string

// ResultSlotID is the caller's local handle name for a thing an operation
// allocated (e.g. "new-task-1"), persisted in journal_operation_result_slots
// so LookupCommitted can reconstruct the slot→id mapping (§3.2).
type ResultSlotID string

// AuthorityKind is the closed discriminator on journal_authorities: a
// bootstrap authority (the genesis/system root) or an assignment authority
// (one responsibility episode's transition). Backed by the authority_kinds
// integer lookup (§4.1).
type AuthorityKind int

const (
	AuthorityKindBootstrap  AuthorityKind = iota // 0: journal_authority_bootstraps
	AuthorityKindAssignment                      // 1: assignment episode/transition detail
)

var authorityKindStrings = [...]string{
	AuthorityKindBootstrap:  "bootstrap",
	AuthorityKindAssignment: "assignment",
}

// AuthorityKinds returns the closed set in id order, seeding authority_kinds
// and guarding corpus enum freshness.
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

// AssignmentSlotID names a responsibility slot on a task (§4.4). Backed by the
// assignment_slots lookup; extensible for future non-owner slots.
type AssignmentSlotID string

// SlotOwnerResponsibility is the one slot seeded today (§4.5).
const SlotOwnerResponsibility AssignmentSlotID = "owner-responsibility"

// AssignmentTransition is the closed two-value lifecycle transition on an
// episode (§4.4): started then, optionally, ended. Backed by the
// assignment_transitions lookup.
type AssignmentTransition int

const (
	TransitionStarted AssignmentTransition = iota // 0
	TransitionEnded                               // 1
)

var assignmentTransitionStrings = [...]string{
	TransitionStarted: "started",
	TransitionEnded:   "ended",
}

// AssignmentTransitions returns the closed set in id order, seeding the lookup
// and guarding corpus enum freshness.
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

// EffectSort is the closed set of effect shapes an operation can produce. It is
// finer-grained than JournalKind because an authority effect splits into a
// bootstrap-creation and the two assignment transitions, each of which folds
// through a distinct per-effect authorization and projection step (§9.3).
type EffectSort int

const (
	EffectTaskEvent          EffectSort = iota // JournalKindTaskEvent
	EffectBootstrapAuthority                   // JournalKindAuthority + AuthorityKindBootstrap
	EffectAssignmentStart                      // JournalKindAuthority + AuthorityKindAssignment, started
	EffectAssignmentEnd                        // JournalKindAuthority + AuthorityKindAssignment, ended
	EffectDecision                             // JournalKindDecision
	EffectEvidence                             // JournalKindEvidence
	// EffectTaskCreate journals the birth of a task: it INSERTs the tasks row and
	// emits a provenance.task.created task_event in one atomic fold, so a task's
	// existence — like every later mutation — flows through the journal rather than a
	// direct unjournaled write (§8.1, §9.3). Its produced journal row carries
	// JournalKindTaskEvent (the created event), and the projection seeds status=Open.
	// It must be ordered before any effect (or FK) that references the new task.
	EffectTaskCreate // JournalKindTaskEvent (provenance.task.created), also inserts the tasks row
)

var effectSortStrings = [...]string{
	EffectTaskEvent:          "task_event",
	EffectBootstrapAuthority: "bootstrap_authority",
	EffectAssignmentStart:    "assignment_start",
	EffectAssignmentEnd:      "assignment_end",
	EffectDecision:           "decision",
	EffectEvidence:           "evidence",
	EffectTaskCreate:         "task_create",
}

func (s EffectSort) String() string {
	if int(s) >= 0 && int(s) < len(effectSortStrings) {
		return effectSortStrings[s]
	}
	return fmt.Sprintf("EffectSort(%d)", int(s))
}

// JournalKind maps an effect sort to the supertype discriminator its produced
// journal row carries (§2.1). This is the single source of truth binding the
// finer effect taxonomy to the closed JournalKind enum.
func (s EffectSort) JournalKind() (JournalKind, error) {
	switch s {
	case EffectTaskEvent, EffectTaskCreate:
		return JournalKindTaskEvent, nil
	case EffectBootstrapAuthority, EffectAssignmentStart, EffectAssignmentEnd:
		return JournalKindAuthority, nil
	case EffectDecision:
		return JournalKindDecision, nil
	case EffectEvidence:
		return JournalKindEvidence, nil
	default:
		return 0, fmt.Errorf("provenance: no JournalKind for effect sort %s", s)
	}
}

// Effect is one journal row an operation produces, in caller list order
// (§9.3.1). It is a discriminated record keyed by Sort: exactly the fields for
// that sort are read. A flat record (not an interface) keeps intra-operation
// order an ordinal slice position, never a map iteration order (§9.3.1), and
// keeps each effect statically inspectable by the reducer.
type Effect struct {
	Sort EffectSort

	// ResultSlot, when non-empty, is the caller's local handle for this
	// produced row, persisted in journal_operation_result_slots (§3.2).
	ResultSlot ResultSlotID

	// ActorID must be left zero: a produced (subordinate) row carries no stored
	// actor. The committing actor is recorded once on the operation anchor and
	// derived for produced rows (§2.1, §8.5, §10 rule 5). Apply rejects any effect
	// that sets it (anchor-only actor placement). The field is retained only so a
	// caller attempting the retired per-row-actor pattern gets an actionable
	// rejection rather than silently mis-shaped input.
	ActorID ActorID

	// RecordedAtOverride, when non-nil, is a general-purpose per-effect audit/display
	// RecordedAt stamped on this effect's journal row instead of the operation's
	// single RecordedAt. Its scope is exactly §12's caller-trust doctrine for
	// RecordedAt generally: it is audit/display only and NEVER establishes causality,
	// order, or authority (JournalID still totally orders, §1, §12), so Apply applies
	// it unconditionally with no honesty verification — a live caller setting it is
	// exactly as trusted as the same caller supplying an unusual operation-level
	// RecordedAt, which the system already permits by design. It is settable by any
	// Apply caller, not migration-restricted. Honest legacy-baseline migration (§13)
	// is its primary/motivating use (the marker/started rows carry the legacy
	// updated_at and an ended row the legacy closed_at — two honest legacy timestamps
	// within one operation), and migration adds its OWN self-consistency guard
	// (assertHonestBaselineTimestamps) that runs ONLY on the migration path and only
	// over values migration itself derived — that guard is not a general invariant on
	// this field for non-migration callers (§13 regression g is enforced on the
	// migration path alone).
	RecordedAtOverride *RecordedTime

	// task_event (EffectTaskEvent)
	TaskID    TaskID
	EventKind EventKind
	Payload   json.RawMessage
	Contexts  []EventContext

	// task_create (EffectTaskCreate): the immutable birth metadata of the new task
	// whose row this effect inserts (§8.1). TaskID (above) is the new task's id;
	// the reducer forces EventKind to provenance.task.created, so the created event
	// and its status=Open projection are canonical. Title and Description are free
	// text; Type/Priority/Phase are the closed classification enums the tasks row
	// stores.
	Title       string
	Description string
	Type        TaskType
	Priority    Priority
	Phase       Phase

	// task update/close materialization (EffectTaskEvent). These are materialized-only
	// projections of the tasks row (§8.1), written directly in the fold exactly as
	// EffectTaskCreate writes Title — safe because the reducer never re-derives or
	// compares them during §15 convergence (which covers only owner/status/watermark/
	// attribution). They let the Session.Update decomposition carry the mutated columns
	// alongside the provenance.task.updated event, and CloseReason alongside the
	// provenance.task.closed lifecycle event, within one journaled operation. Each is
	// applied only when non-nil (or, for CloseReason, only on a close event), so a
	// plain caller-domain task_event is unaffected.
	CloseReason       string  // materialized when EventKind == provenance.task.closed
	UpdateTitle       *string // materialized when non-nil (provenance.task.updated)
	UpdateDescription *string
	UpdatePriority    *Priority
	UpdatePhase       *Phase
	UpdateNotes       *string

	// bootstrap authority (EffectBootstrapAuthority)
	BootstrapLabel       string
	OperationAuthorityID OperationAuthorityID

	// assignment start/end (EffectAssignmentStart / EffectAssignmentEnd)
	AssignmentID AssignmentID
	SlotID       AssignmentSlotID
	Occupant     ActorID      // episode occupant (start); attributed actor (§8.2)
	Predecessor  AssignmentID // optional predecessor episode on a transfer start
	// Parent, on an EffectAssignmentStart, optionally cites the episode this one
	// is deliberately rooted under for delegated governance (§14.5): the cited
	// parent must be an episode that is ACTIVE at this start's own journal
	// position, and the citation must not create a cycle. It is the deliberate
	// ownership-citation edge and is DISTINCT from Predecessor — Predecessor is
	// succession in time on one slot (a transfer), Parent is governance lineage
	// across tasks. An episode may carry both, one, or neither.
	Parent AssignmentID

	// decision (EffectDecision) / evidence (EffectEvidence)
	DecisionKind  DecisionKind
	EvidenceKind  EvidenceKind
	ContentDigest []byte
}

// DecisionKind / EvidenceKind are open, validated namespaced strings (§6),
// recorded opaquely like EventKind.
type (
	DecisionKind string
	EvidenceKind string
)

// ---------------------------------------------------------------------------
// Operation input and replay identity (§3.1, §9.4, §11)
// ---------------------------------------------------------------------------

// OperationInput is the validated request to commit one logical operation as an
// atomic append plus domain mutation (§9.5). Its effects fold in slice order
// (§9.3.1); each is authorized against state folded through all earlier effects
// of the same operation (§9.3).
type OperationInput struct {
	OperationID OperationID
	ActorID     ActorID // committing actor of the whole operation (§2.1)
	// AuthorityJournalID is the authority this operation executes under. NULL
	// only on a genesis operation whose sole effect is one bootstrap authority
	// (§4.6, §10 rule 6).
	AuthorityJournalID *JournalID
	CommandDigest      []byte
	MutationDigest     []byte
	RecordedAt         RecordedTime // audit/display only (§12)
	Effects            []Effect
}

// RecordedTime is the caller-supplied wall-clock stamp copied into
// journal.RecordedAt for audit/display only (§12). Aliased so the operations
// surface does not import time here; the sqlite layer converts to UnixNano.
type RecordedTime = int64

// StoredOperationIdentity is the four-field exact replay identity compared at
// §9.4 short-circuit time: a same-OperationID retry whose identity matches
// exactly returns the original result; any mismatch is a typed conflict (§11).
// It is deliberately NOT a schema uniqueness constraint (§3.1).
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
	// CommittedAbsent: no journal_operations row exists for the OperationID
	// (§9.4). LookupCommitted returns this with a nil error and no side effects.
	CommittedAbsent CommittedResultKind = iota
	// CommittedExact: a committed operation matching the requested identity.
	CommittedExact
	// CommittedConflict: an OperationID reused with a differing four-field
	// identity (§11) — a typed conflict, never a re-execution.
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

// ResultSlotBinding is one reconstructed ResultSlotID→produced-row mapping
// (§3.2), bucketed by the produced row's JournalKind. For a task_event slot the
// produced row's TaskID is resolved for the caller.
type ResultSlotBinding struct {
	Slot              ResultSlotID
	ProducedJournalID JournalID
	Kind              JournalKind
	TaskID            *TaskID
}

// CommittedResult is the closed result of Apply and LookupCommitted. For
// CommittedExact it carries the operation's anchor, the flat EmittedEvents list
// (the §2.1 ProducedByOperationJournalID closure over task_event rows in
// JournalID order — no slot row needed, §3.2), and the slot-keyed ResultSlots.
type CommittedResult struct {
	Kind            CommittedResultKind
	AnchorJournalID JournalID
	EmittedEvents   []JournalID // task_event closure, JournalID order (§2.1)
	ResultSlots     []ResultSlotBinding
	// ShortCircuited is true when a §9.4 idempotent-replay retry returned this
	// already-committed result without folding effects (never re-executed).
	ShortCircuited bool
	Conflict       *OperationConflict
}

// OperationConflict is the typed conflict returned when an OperationID is reused
// with a differing four-field identity (§11), or when a concurrent racer loses
// the OperationID insert with a non-matching identity (§9.6). It is the payload
// of the CommittedConflict result variant.
type OperationConflict struct {
	OperationID OperationID
	Field       string // the first identity field that differed
}

func (c OperationConflict) Error() string {
	return fmt.Sprintf(
		"provenance: OperationID %q reused with a different %s than the committed "+
			"operation — where: Apply replay short-circuit (§9.4/§11); when: before "+
			"any write; impact: nothing was committed; fix: retry with the identical "+
			"actor, authority, command digest, and mutation digest, or issue a new "+
			"OperationID for a genuinely different operation",
		string(c.OperationID), c.Field)
}

// ---------------------------------------------------------------------------
// Errors (§13.1 actionable shape where the contract requires)
// ---------------------------------------------------------------------------

var (
	// ErrOperationConflict wraps a typed OperationID-reuse conflict (§11, §9.6).
	ErrOperationConflict = errors.New("provenance: operation identity conflict")
	// ErrGenesis is returned for genesis-discipline violations (§4.6, §10
	// rules 6-7): a NULL authority off the first operation, a second genesis
	// against a non-empty journal, or a genesis producing a non-bootstrap effect.
	ErrGenesis = errors.New("provenance: genesis authority discipline violated")
	// ErrAuthorityScope is returned when an operation's authority does not reach
	// (govern) the task an effect mutates (§9.3, §14.1).
	ErrAuthorityScope = errors.New("provenance: authority does not govern the effect's task")
	// ErrAssignmentLifecycle is returned for assignment-transition lifecycle
	// order violations (§14.4): ended-without-started, ended-before-started.
	ErrAssignmentLifecycle = errors.New("provenance: assignment transition lifecycle order violated")
	// ErrOrphanedEvidence is returned when a transfer names a predecessor that
	// has not ended (§14.3), or one already consumed (§14.2).
	ErrOrphanedEvidence = errors.New("provenance: predecessor assignment evidence invalid")
	// ErrStaleEpisode is returned to the losing racer of a concurrent transfer
	// CAS whose precondition (the target episode still active) no longer holds
	// (§9.6).
	ErrStaleEpisode = errors.New("provenance: assignment episode is no longer active")
	// ErrResultSlotIntegrity is returned when a result slot references a row
	// produced by a different operation (§3.2, §10 rule 9).
	ErrResultSlotIntegrity = errors.New("provenance: result slot references a foreign operation's produced row")
	// ErrCloseWithoutEnding is returned when a task is closed while an active
	// owner-responsibility episode is not ended in the same operation (§8.1,
	// owner_responsibility.yaml regression c).
	ErrCloseWithoutEnding = errors.New("provenance: task closed without ending its active owner-responsibility episode")
	// ErrParentCitation is returned when an assignment-start's ParentAssignmentID
	// citation is invalid (§14.5): a cited parent that does not exist, is not
	// active at the citation's journal position, or whose citation would create a
	// cycle in the parent chain. Distinct from ErrOrphanedEvidence, which guards
	// the predecessor (succession) edge.
	ErrParentCitation = errors.New("provenance: assignment parent citation invalid")
	// ErrCorruptParentChain is returned when the governance walk over
	// ParentAssignmentID citations detects a cycle in the STORED chain (§14.5) —
	// a corruption reachable only by bypassing the start-effect citation guard
	// (e.g. direct schema corruption). The bounded, visited-tracked walk fails
	// closed with this typed error rather than looping.
	ErrCorruptParentChain = errors.New("provenance: corrupt cyclic assignment parent-citation chain")
)
