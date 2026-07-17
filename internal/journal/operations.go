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
)

var effectSortStrings = [...]string{
	EffectTaskEvent:          "task_event",
	EffectBootstrapAuthority: "bootstrap_authority",
	EffectAssignmentStart:    "assignment_start",
	EffectAssignmentEnd:      "assignment_end",
	EffectDecision:           "decision",
	EffectEvidence:           "evidence",
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
	case EffectTaskEvent:
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

	// ActorID, when set (non-zero namespace), is the committing actor stamped
	// on this effect's journal row. It MUST equal the operation's anchor actor
	// (§10 rule 5); a differing value is rejected. Left zero, the anchor actor
	// is used.
	ActorID ActorID

	// RecordedAtOverride, when non-nil, is the audit/display RecordedAt stamped on
	// this effect's journal row instead of the operation's single RecordedAt (§12).
	// It exists for honest legacy-baseline migration (§13), where the marker/started
	// rows carry the legacy updated_at and an ended row carries the legacy closed_at —
	// two different honest legacy timestamps within one operation. It never
	// establishes causality or order (JournalID still totally orders, §1, §12).
	RecordedAtOverride *RecordedTime

	// task_event (EffectTaskEvent)
	TaskID    TaskID
	EventKind EventKind
	Payload   json.RawMessage
	Contexts  []EventContext

	// bootstrap authority (EffectBootstrapAuthority)
	BootstrapLabel       string
	OperationAuthorityID OperationAuthorityID

	// assignment start/end (EffectAssignmentStart / EffectAssignmentEnd)
	AssignmentID AssignmentID
	SlotID       AssignmentSlotID
	Occupant     ActorID      // episode occupant (start); attributed actor (§8.2)
	Predecessor  AssignmentID // optional predecessor episode on a transfer start

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
	// ErrEffectActorMismatch is returned when an effect row's committing actor
	// differs from its operation anchor's actor (§10 rule 5).
	ErrEffectActorMismatch = errors.New("provenance: effect actor differs from operation anchor")
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
)
