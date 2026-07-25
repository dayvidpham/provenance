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
	// ConflictAxis is the broad discriminator on OperationConflict (five axes).
	ConflictAxis            = journal.ConflictAxis
	CanonicalMutation       = journal.CanonicalMutation
	CanonicalMutationError  = journal.CanonicalMutationError
	MutationEncodingVersion = journal.MutationEncodingVersion

	// Condition types (§9.5 pre-conditions).
	FactTaskScopeKind      = journal.FactTaskScopeKind
	FactTaskScope          = journal.FactTaskScope
	FactFilter             = journal.FactFilter
	FactKind               = journal.FactKind
	FactSelector           = journal.FactSelector
	ConditionKind          = journal.ConditionKind
	Condition              = journal.Condition
	ConditionFailureReason = journal.ConditionFailureReason
	ConditionFailure       = journal.ConditionFailure
	FactPageRequest        = journal.FactPageRequest
	DecisionQuery          = journal.DecisionQuery
	EvidenceQuery          = journal.EvidenceQuery
	FactCursor             = journal.FactCursor
	DecisionRow            = journal.DecisionRow
	EvidenceRow            = journal.EvidenceRow
	DecisionPage           = journal.DecisionPage
	EvidencePage           = journal.EvidencePage
	FactQueryAPI           = journal.FactQueryAPI
	// ActivityConflict is the typed conflict for ActivityCreate folds (later vertical).
	ActivityConflict = journal.ActivityConflict

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
	JournalKindActivity  = journal.JournalKindActivity

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
	MaxCanonicalConditions        = journal.MaxCanonicalConditions
	MaxFactFilterValues           = journal.MaxFactFilterValues
	MaxFactQueryKinds             = journal.MaxFactQueryKinds
	MaxFactPageSize               = journal.MaxFactPageSize
	MaxCanonicalResultSlots       = journal.MaxCanonicalResultSlots

	FactTaskAny      = journal.FactTaskAny
	FactTaskUnscoped = journal.FactTaskUnscoped
	FactTaskExact    = journal.FactTaskExact
	FactDecision     = journal.FactDecision
	FactEvidence     = journal.FactEvidence

	ConditionExactFact       = journal.ConditionExactFact
	ConditionCurrentFact     = journal.ConditionCurrentFact
	ConditionFactMissing     = journal.ConditionFactMissing
	ConditionFactMismatch    = journal.ConditionFactMismatch
	ConditionCurrentMismatch = journal.ConditionCurrentMismatch

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
	// EffectActivityCreate is the closed effect sort for activity birth (§Activity).
	// Codec and normalization are complete in this vertical; SQLite fold in .1.2.
	EffectActivityCreate = journal.EffectActivityCreate

	// Conflict axes: five broad discriminators on OperationConflict (§11).
	ConflictActor     = journal.ConflictActor
	ConflictAuthority = journal.ConflictAuthority
	ConflictCommand   = journal.ConflictCommand
	ConflictCondition = journal.ConflictCondition
	ConflictEffect    = journal.ConflictEffect

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

	// Static status FSM surface (§8.1, §16).
	ValidateStatusTransition      = journal.ValidateStatusTransition
	IsTransitionLifecycleKind     = journal.IsTransitionLifecycleKind
	TransitionLifecycleKinds      = journal.TransitionLifecycleKinds
	EncodeForcedTransitionPayload = journal.EncodeForcedTransitionPayload
	DecodeForcedTransition        = journal.DecodeForcedTransition

	// Journaled relationship / annotation mutation-family surface (§6 amendment).
	IsMutationFamilyKind         = journal.IsMutationFamilyKind
	MutationFamilyKinds          = journal.MutationFamilyKinds
	MutationFamilyKindForSort    = journal.MutationFamilyKindForSort
	DecodeEdgeMutationPayload    = journal.DecodeEdgeMutationPayload
	DecodeLabelMutationPayload   = journal.DecodeLabelMutationPayload
	DecodeCommentMutationPayload = journal.DecodeCommentMutationPayload
	// Canonicalize is the sole public preparation boundary.
	Canonicalize              = journal.Canonicalize
	DecodeCanonicalMutation   = journal.DecodeCanonicalMutation
	ValidateResultSlotBinding = journal.ValidateResultSlotBinding
	// ConflictAxes returns the closed five-axis set. All axes are nonzero.
	ConflictAxes            = journal.ConflictAxes
	FactTaskScopeKinds      = journal.FactTaskScopeKinds
	ConditionKinds          = journal.ConditionKinds
	ConditionFailureReasons = journal.ConditionFailureReasons
)

// Status-FSM typed error + sentinel (§8.1).
type InvalidStatusTransition = journal.InvalidStatusTransition

// Journal sentinel errors, re-exported for errors.Is at call sites.
var (
	ErrUnsupportedOrderDimension = journal.ErrUnsupportedOrderDimension
	ErrInvalidQuery              = journal.ErrInvalidQuery
	ErrSubtypeIntegrity          = journal.ErrSubtypeIntegrity
	ErrActorPlacement            = journal.ErrActorPlacement
	ErrNamespaceRange            = journal.ErrNamespaceRange
	ErrEntryOutOfRange           = journal.ErrEntryOutOfRange
	ErrNamespaceClaim            = journal.ErrNamespaceClaim

	// Operations/authority sentinel errors (§4, §9, §14).
	ErrOperationConflict   = journal.ErrOperationConflict
	ErrConditionFailed     = journal.ErrConditionFailed
	ErrActivityConflict    = journal.ErrActivityConflict
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

// Journal is the ordered global-journal surface.
type Journal interface {
	Facts() FactQueryAPI
	QueryTaskEvents(q JournalQueryV1) (JournalTaskEventPageV1, error)
	TaskAttributions(taskID TaskID) ([]TaskAttribution, error)
	VerifyIntegrity() error
	RegisterNamespaceClaim(claim ActorNamespaceClaim) error
	RegisterFixedActorEntry(entry FixedActorEntry) error
	NamespaceClaims() ([]ActorNamespaceClaim, error)
	Apply(in OperationInput) (CommittedResult, error)
	LookupCommitted(op OperationID) (CommittedResult, error)
	AuthorityGovernsTaskAt(authJID JournalID, task TaskID, beforeJID JournalID) (bool, error)
	PreflightSchema() error
	ReplayProjections() (ReplayResult, error)
	MigrateLegacyBaseline(in MigrationInput) (MigrationResult, error)
}

// Journal returns the ordered global-journal surface backed by the same SQLite
// connection as the task tracker.
func (t *sqliteTracker) Journal() Journal { return t.db }
