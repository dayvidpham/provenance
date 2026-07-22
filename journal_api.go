package provenance

// journal_api.go re-exports the global-journal surface defined by
// docs/journal-relational-contract.md from
// internal/journal and exposes the JournalID-ordered journal API through the
// Tracker.

import (
	"github.com/dayvidpham/provenance/internal/journal"
)

// Journal identity and discriminator types.
type (
	JournalID                      = journal.JournalID
	JournalKind                    = journal.JournalKind
	EventKind                      = journal.EventKind
	OperationID                    = journal.OperationID
	EventContext                   = journal.EventContext
	EventContextKind               = journal.EventContextKind
	GitOID                         = journal.GitOID
	Row                            = journal.Row
	TaskEventRow                   = journal.TaskEventRow
	AppendTaskEventInput           = journal.AppendTaskEventInput
	JournalQueryV1                 = journal.JournalQueryV1
	JournalCursorV1                = journal.JournalCursorV1
	JournalTaskEventPageV1         = journal.JournalTaskEventPageV1
	OrderDimension                 = journal.OrderDimension
	TaskAttribution                = journal.TaskAttribution
	ActorNamespaceClaim            = journal.ActorNamespaceClaim
	FixedActorEntry                = journal.FixedActorEntry
	FixedSoftwareAgentRegistration = journal.FixedSoftwareAgentRegistration
	UUIDRange                      = journal.UUIDRange
	NamespaceCodec                 = journal.NamespaceCodec

	// Operations, effects, results, and authority-lifecycle surface (§2-§4, §9).
	OperationAuthorityID    = journal.OperationAuthorityID
	AssignmentID            = journal.AssignmentID
	ResultSlotID            = journal.ResultSlotID
	AuthorityKind           = journal.AuthorityKind
	AssignmentSlotID        = journal.AssignmentSlotID
	AssignmentTransition    = journal.AssignmentTransition
	EffectSort              = journal.EffectSort
	Effect                  = journal.Effect
	DecisionKind            = journal.DecisionKind
	EvidenceKind            = journal.EvidenceKind
	OperationInput          = journal.OperationInput
	StoredOperationIdentity = journal.StoredOperationIdentity
	CommittedResultKind     = journal.CommittedResultKind
	ResultSlotBinding       = journal.ResultSlotBinding
	CommittedResult         = journal.CommittedResult
	OperationConflict       = journal.OperationConflict
	CanonicalMutation       = journal.CanonicalMutation
	CanonicalMutationError  = journal.CanonicalMutationError
	MutationEncodingVersion = journal.MutationEncodingVersion

	// Shared-reducer replay, migration, and preflight surface (§9, §13, §15).
	TaskStatus                    = journal.TaskStatus
	TaskProjection                = journal.TaskProjection
	ReplayResult                  = journal.ReplayResult
	LegacyTaskRow                 = journal.LegacyTaskRow
	MigrationInput                = journal.MigrationInput
	MigrationResult               = journal.MigrationResult
	MigrationOwnerUnmappableError = journal.MigrationOwnerUnmappableError
	SchemaPreflightError          = journal.SchemaPreflightError
	ProjectionDivergenceError     = journal.ProjectionDivergenceError
)

// Closed enum values.
const (
	JournalKindOperation = journal.JournalKindOperation
	JournalKindTaskEvent = journal.JournalKindTaskEvent
	JournalKindAuthority = journal.JournalKindAuthority
	JournalKindDecision  = journal.JournalKindDecision
	JournalKindEvidence  = journal.JournalKindEvidence

	// OrderByRecordedAt is the non-causal readable-timeline display order and the
	// default for display-facing listings; OrderByJournalID is the canonical order.
	OrderByRecordedAt = journal.OrderByRecordedAt
	OrderByJournalID  = journal.OrderByJournalID

	EventContextKindTask     = journal.EventContextKindTask
	EventContextKindActivity = journal.EventContextKindActivity
	EventContextKindActor    = journal.EventContextKindActor
	EventContextKindGit      = journal.EventContextKindGit

	OrdinalV1CodecName            = journal.OrdinalV1CodecName
	MutationEncodingV1            = journal.MutationEncodingV1
	MaxCanonicalEffects           = journal.MaxCanonicalEffects
	MaxCanonicalContextsPerEffect = journal.MaxCanonicalContextsPerEffect
	MaxCanonicalFieldBytes        = journal.MaxCanonicalFieldBytes
	MaxCanonicalMutationBytes     = journal.MaxCanonicalMutationBytes

	// Authority-kind and assignment-lifecycle closed enums (§4).
	AuthorityKindBootstrap  = journal.AuthorityKindBootstrap
	AuthorityKindAssignment = journal.AuthorityKindAssignment
	SlotOwnerResponsibility = journal.SlotOwnerResponsibility
	TransitionStarted       = journal.TransitionStarted
	TransitionEnded         = journal.TransitionEnded

	// Effect sorts (§9.3).
	EffectTaskEvent           = journal.EffectTaskEvent
	EffectBootstrapAuthority  = journal.EffectBootstrapAuthority
	EffectAssignmentStart     = journal.EffectAssignmentStart
	EffectAssignmentEnd       = journal.EffectAssignmentEnd
	EffectDecision            = journal.EffectDecision
	EffectEvidence            = journal.EffectEvidence
	EffectTaskCreate          = journal.EffectTaskCreate
	EffectTaskCreateAllocated = journal.EffectTaskCreateAllocated

	// Journaled relationship / annotation mutation-family effect sorts (§6 amendment).
	EffectEdgeAdd     = journal.EffectEdgeAdd
	EffectEdgeRemove  = journal.EffectEdgeRemove
	EffectLabelAdd    = journal.EffectLabelAdd
	EffectLabelRemove = journal.EffectLabelRemove
	EffectCommentAdd  = journal.EffectCommentAdd

	// Committed-result variants (§3.2, §9.4).
	CommittedAbsent   = journal.CommittedAbsent
	CommittedExact    = journal.CommittedExact
	CommittedConflict = journal.CommittedConflict

	// Task-status projection (§8.1).
	TaskStatusOpen       = journal.TaskStatusOpen
	TaskStatusInProgress = journal.TaskStatusInProgress
	TaskStatusClosed     = journal.TaskStatusClosed

	// Provenance lifecycle task-event kinds the reducer projects (§8.1, §13).
	EventKindTaskCreated  = journal.EventKindTaskCreated
	EventKindTaskStarted  = journal.EventKindTaskStarted
	EventKindTaskStopped  = journal.EventKindTaskStopped
	EventKindTaskClosed   = journal.EventKindTaskClosed
	EventKindTaskReopened = journal.EventKindTaskReopened
	EventKindTaskMigrated = journal.EventKindTaskMigrated
	// EventKindTaskUpdated records a materialized-metadata mutation; it is NOT a
	// status-changing lifecycle kind (§8.1).
	EventKindTaskUpdated = journal.EventKindTaskUpdated

	// Journaled relationship / annotation mutation-family kinds (§6 amendment).
	EventKindEdgeAdded    = journal.EventKindEdgeAdded
	EventKindEdgeRemoved  = journal.EventKindEdgeRemoved
	EventKindLabelAdded   = journal.EventKindLabelAdded
	EventKindLabelRemoved = journal.EventKindLabelRemoved
	EventKindCommentAdded = journal.EventKindCommentAdded
)

// Typed context and validation constructors.
var (
	TaskContext            = journal.TaskContext
	ActivityContext        = journal.ActivityContext
	ActorContext           = journal.ActorContext
	GitContext             = journal.GitContext
	CanonicalEventContexts = journal.CanonicalEventContexts
	ValidateEventKind      = journal.ValidateEventKind
	ValidateOperationID    = journal.ValidateOperationID
	OrdinalUUID            = journal.OrdinalUUID
	BigEndianUUID          = journal.BigEndianUUID
	LookupCodec            = journal.LookupCodec

	// Deterministic migration identity + lifecycle-status projection (§8.1, §13).
	MigrationBaselineOperationID  = journal.MigrationBaselineOperationID
	MigrationBaselineAssignmentID = journal.MigrationBaselineAssignmentID
	StatusForEventKind            = journal.StatusForEventKind

	// Static status FSM surface (§8.1, §16): the transition table and its forced escape
	// hatch, re-exported so callers can validate/inspect transitions and recover the
	// typed rejection.
	ValidateStatusTransition      = journal.ValidateStatusTransition
	IsTransitionLifecycleKind     = journal.IsTransitionLifecycleKind
	TransitionLifecycleKinds      = journal.TransitionLifecycleKinds
	EncodeForcedTransitionPayload = journal.EncodeForcedTransitionPayload
	DecodeForcedTransition        = journal.DecodeForcedTransition

	// Journaled relationship / annotation mutation-family surface (§6 amendment):
	// classification + payload codecs, re-exported so who-provenance queries can decode
	// an edge/label/comment row's operands straight from the journal.
	IsMutationFamilyKind         = journal.IsMutationFamilyKind
	MutationFamilyKinds          = journal.MutationFamilyKinds
	MutationFamilyKindForSort    = journal.MutationFamilyKindForSort
	DecodeEdgeMutationPayload    = journal.DecodeEdgeMutationPayload
	DecodeLabelMutationPayload   = journal.DecodeLabelMutationPayload
	DecodeCommentMutationPayload = journal.DecodeCommentMutationPayload
	PrepareMutationV1            = journal.PrepareMutationV1
	DecodeCanonicalMutation      = journal.DecodeCanonicalMutation
)

// Status-FSM typed error + sentinel (§8.1).
type InvalidStatusTransition = journal.InvalidStatusTransition

// Journal sentinel errors, re-exported for errors.Is at call sites.
var (
	ErrUnsupportedOrderDimension = journal.ErrUnsupportedOrderDimension
	ErrSubtypeIntegrity          = journal.ErrSubtypeIntegrity
	ErrActorPlacement            = journal.ErrActorPlacement
	ErrNamespaceRange            = journal.ErrNamespaceRange
	ErrEntryOutOfRange           = journal.ErrEntryOutOfRange
	ErrNamespaceClaim            = journal.ErrNamespaceClaim

	// Operations/authority sentinel errors (§4, §9, §14).
	ErrOperationConflict   = journal.ErrOperationConflict
	ErrGenesis             = journal.ErrGenesis
	ErrAuthorityScope      = journal.ErrAuthorityScope
	ErrAssignmentLifecycle = journal.ErrAssignmentLifecycle
	ErrOrphanedEvidence    = journal.ErrOrphanedEvidence
	ErrStaleEpisode        = journal.ErrStaleEpisode
	ErrResultSlotIntegrity = journal.ErrResultSlotIntegrity
	ErrCloseWithoutEnding  = journal.ErrCloseWithoutEnding
	ErrParentCitation      = journal.ErrParentCitation
	ErrCorruptParentChain  = journal.ErrCorruptParentChain

	// Shared-reducer replay, migration, and preflight sentinels (§9, §13, §15).
	ErrMigrationOwnerUnmappable    = journal.ErrMigrationOwnerUnmappable
	ErrSchemaPreflight             = journal.ErrSchemaPreflight
	ErrProjectionDivergence        = journal.ErrProjectionDivergence
	ErrStatusTransition            = journal.ErrStatusTransition
	ErrMigrationFault              = journal.ErrMigrationFault
	ErrInjectedFault               = journal.ErrInjectedFault
	ErrDishonestMigrationTimestamp = journal.ErrDishonestMigrationTimestamp
	ErrCanonicalMutation           = journal.ErrCanonicalMutation
)

// JournalAPI is the ordered global-journal surface: commit operations (§9), query
// task-event rows in strictly ascending JournalID order with a snapshot watermark
// and exclusive JournalID cursor, read the cumulative attribution projection,
// verify subtype integrity (§10 rule 8 / §15), and register the actor-namespace
// reservation registry (§7).
//
// The bare AppendTaskEvent primitive is retired from this surface: every task event is
// now produced by an operation (Session.Create/Update/CloseTask, an Atomic op, or a
// migration baseline). The producer constraint enforces
// produced_by_operation_journal_id NOT NULL for task events. A lower-level
// append primitive exists only for controlled legacy migration and is not part
// of this public API.
type JournalAPI interface {
	// QueryTaskEvents returns one page in the query's order: the readable-timeline
	// (RecordedAt, JournalID) display order (the default) or the canonical JournalID
	// order. An unexposed order dimension is rejected with ErrUnsupportedOrderDimension.
	QueryTaskEvents(q JournalQueryV1) (JournalTaskEventPageV1, error)
	// TaskAttributions returns a task's cumulative attribution edges (§8.2).
	TaskAttributions(taskID TaskID) ([]TaskAttribution, error)
	// VerifyIntegrity checks class-table-inheritance integrity across the whole
	// journal (§10 rule 8), the convergence tool Open uses (§15).
	VerifyIntegrity() error
	// RegisterNamespaceClaim registers a reserved actor-namespace range (§7.1),
	// rejecting range overlaps with ErrNamespaceRange (§7.3 rule 1).
	RegisterNamespaceClaim(claim ActorNamespaceClaim) error
	// RegisterFixedActorEntry registers a fixed system actor within a claimed
	// range (§7.2), rejecting out-of-range entries with ErrEntryOutOfRange
	// (§7.3 rule 2). The fixed UUID is derived from entry.ActorID.
	RegisterFixedActorEntry(entry FixedActorEntry) error
	// NamespaceClaims returns every registered claim.
	NamespaceClaims() ([]ActorNamespaceClaim, error)

	// Apply commits one logical operation atomically (§9.5): an atomic append
	// plus domain mutation folding the operation's effects in caller list order
	// with per-effect authorization (§9.3), the §9.4 idempotent-replay
	// short-circuit, genesis discipline (§4.6), and the subtype-integrity gate.
	Apply(in OperationInput) (CommittedResult, error)
	// LookupCommitted returns the committed result for an OperationID: the closed
	// Absent variant (no side effects) for a never-applied operation, or the
	// Exact variant with the reconstructed EmittedEvents closure and slot map
	// (§3.2, §9.4).
	LookupCommitted(op OperationID) (CommittedResult, error)
	// AuthorityGovernsTaskAt reports whether the authority at authJID governs
	// task for an effect at beforeJID, ordering strictly by JournalID (§9.3, §12).
	AuthorityGovernsTaskAt(authJID JournalID, task TaskID, beforeJID JournalID) (bool, error)

	// PreflightSchema verifies the external pre-journal schema's exact expected
	// shape in both directions before any transaction opens (§13), failing closed
	// with a typed *SchemaPreflightError on a missing table, missing expected
	// column, or unexpected extra column.
	PreflightSchema() error
	// ReplayProjections folds the entire journal in JournalID order through the
	// same reducer step Apply uses (§9.2) and verifies the recomputed projection
	// converges with the stored incremental one (§15). It runs the schema preflight
	// first, so a corrupted topology fails closed before any fold. It returns the
	// converged per-task projection, or a typed *ProjectionDivergenceError.
	ReplayProjections() (ReplayResult, error)
	// MigrateLegacyBaseline migrates pre-journal tasks into deterministic baseline
	// journal entries under the genesis bootstrap authority (§13): honest legacy
	// RecordedAt, whole-batch fail-closed atomicity, and per-task idempotent anchors.
	// An unmappable owner fails the whole batch with a typed
	// *MigrationOwnerUnmappableError; a schema mismatch fails with a typed
	// *SchemaPreflightError; nothing is committed in either case.
	MigrateLegacyBaseline(in MigrationInput) (MigrationResult, error)
}

// Journal returns the ordered global-journal surface backed by the same SQLite
// connection as the task tracker.
func (t *sqliteTracker) Journal() JournalAPI { return t.db }
