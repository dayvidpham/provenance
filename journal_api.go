package provenance

// journal_api.go re-exports the global-journal surface (issue
// dayvidpham/provenance#4, docs/journal-relational-contract.md) from
// internal/journal and exposes the JournalID-ordered journal API through the
// Tracker.

import (
	"github.com/dayvidpham/provenance/internal/journal"
)

// Journal identity and discriminator types.
type (
	JournalID              = journal.JournalID
	JournalKind            = journal.JournalKind
	EventKind              = journal.EventKind
	OperationID            = journal.OperationID
	EventContext           = journal.EventContext
	EventContextKind       = journal.EventContextKind
	GitOID                 = journal.GitOID
	Row                    = journal.Row
	TaskEventRow           = journal.TaskEventRow
	AppendTaskEventInput   = journal.AppendTaskEventInput
	JournalQueryV1         = journal.JournalQueryV1
	JournalCursorV1        = journal.JournalCursorV1
	JournalTaskEventPageV1 = journal.JournalTaskEventPageV1
	OrderDimension         = journal.OrderDimension
	TaskAttribution        = journal.TaskAttribution
	ActorNamespaceClaim    = journal.ActorNamespaceClaim
	FixedActorEntry        = journal.FixedActorEntry
	UUIDRange              = journal.UUIDRange
	NamespaceCodec         = journal.NamespaceCodec
)

// Closed enum values.
const (
	JournalKindOperation = journal.JournalKindOperation
	JournalKindTaskEvent = journal.JournalKindTaskEvent
	JournalKindAuthority = journal.JournalKindAuthority
	JournalKindDecision  = journal.JournalKindDecision
	JournalKindEvidence  = journal.JournalKindEvidence

	OrderByJournalID = journal.OrderByJournalID

	EventContextKindTask     = journal.EventContextKindTask
	EventContextKindActivity = journal.EventContextKindActivity
	EventContextKindActor    = journal.EventContextKindActor
	EventContextKindGit      = journal.EventContextKindGit

	OrdinalV1CodecName = journal.OrdinalV1CodecName
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
)

// Journal sentinel errors, re-exported for errors.Is at call sites.
var (
	ErrUnsupportedOrderDimension = journal.ErrUnsupportedOrderDimension
	ErrSubtypeIntegrity          = journal.ErrSubtypeIntegrity
	ErrNamespaceRange            = journal.ErrNamespaceRange
	ErrEntryOutOfRange           = journal.ErrEntryOutOfRange
	ErrNamespaceClaim            = journal.ErrNamespaceClaim
)

// JournalAPI is the ordered global-journal surface: append task-event rows,
// query them in strictly ascending JournalID order with a snapshot watermark
// and exclusive JournalID cursor, read the cumulative attribution projection,
// verify subtype integrity (§10 rule 8 / §15), and register the actor-namespace
// reservation registry (§7).
type JournalAPI interface {
	// AppendTaskEvent appends one task-event row to the global journal and
	// advances its projections in a single fail-closed transaction.
	AppendTaskEvent(in AppendTaskEventInput) (TaskEventRow, error)
	// QueryTaskEvents returns one JournalID-ordered page. A non-JournalID order
	// request is rejected with ErrUnsupportedOrderDimension.
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
	// (§7.3 rule 2). fixedUUID is the entry's 16-byte fixed-UUID form.
	RegisterFixedActorEntry(entry FixedActorEntry, fixedUUID [16]byte) error
	// NamespaceClaims returns every registered claim.
	NamespaceClaims() ([]ActorNamespaceClaim, error)
}

// Journal returns the ordered global-journal surface backed by the same SQLite
// connection as the task tracker.
func (t *sqliteTracker) Journal() JournalAPI { return t.db }
