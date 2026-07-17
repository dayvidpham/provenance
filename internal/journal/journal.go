package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Canonical order and identity domains
// ---------------------------------------------------------------------------

// JournalID is the database-generated INTEGER surrogate key of a journal row
// and the sole canonical order of the global journal
// (docs/journal-relational-contract.md §1). SQLite AUTOINCREMENT allocates
// strictly ascending positive values; zero is reserved for an unset value.
// Every query, authorization check, and replay decision orders by JournalID,
// never by RecordedAt.
type JournalID int64

// EventKind identifies a namespaced task/domain event definition, for example
// "provenance.task.created" or "pasture.review.recorded". It is an open,
// validated string (§5.1) — not a closed lookup — because callers define their
// own kinds Provenance never enumerates in advance.
type EventKind string

// OperationID is a caller-supplied, globally unique alternate idempotency key
// (§11). The operations slice (dayvidpham/provenance#5) owns replay semantics;
// the journal base stores and validates only its syntax.
type OperationID string

// ---------------------------------------------------------------------------
// JournalKind — the closed supertype discriminator (§2.1, §2.2)
// ---------------------------------------------------------------------------

// JournalKind is the closed discriminator selecting which typed subtype table
// extends a journal row. Backed by the journal_kinds integer lookup, seeded
// with exactly the five values below (§2.2).
type JournalKind int

const (
	JournalKindOperation JournalKind = iota // 0: journal_operations
	JournalKindTaskEvent                    // 1: journal_task_events
	JournalKindAuthority                    // 2: journal_authorities
	JournalKindDecision                     // 3: journal_decisions
	JournalKindEvidence                     // 4: journal_evidence
)

var journalKindStrings = [...]string{
	JournalKindOperation: "operation",
	JournalKindTaskEvent: "task_event",
	JournalKindAuthority: "authority",
	JournalKindDecision:  "decision",
	JournalKindEvidence:  "evidence",
}

// JournalKinds returns the closed set in declaration/id order. Used both to
// seed the journal_kinds lookup and to guard corpus enum freshness.
func JournalKinds() []JournalKind {
	return []JournalKind{
		JournalKindOperation, JournalKindTaskEvent, JournalKindAuthority,
		JournalKindDecision, JournalKindEvidence,
	}
}

func (k JournalKind) String() string {
	if int(k) >= 0 && int(k) < len(journalKindStrings) {
		return journalKindStrings[k]
	}
	return fmt.Sprintf("JournalKind(%d)", int(k))
}

// IsValid reports whether k is one of the five seeded kinds.
func (k JournalKind) IsValid() bool {
	return k >= JournalKindOperation && k <= JournalKindEvidence
}

// ParseJournalKind maps a seeded lookup name to its typed JournalKind.
func ParseJournalKind(name string) (JournalKind, error) {
	for i, s := range journalKindStrings {
		if s == name {
			return JournalKind(i), nil
		}
	}
	return 0, fmt.Errorf("provenance: unknown JournalKind %q — valid values: %v",
		name, journalKindStrings[:])
}

// SubtypeTable returns the class-table-inheritance subtype table name for a
// journal kind (§1 naming convention). Used by the subtype-integrity reducer.
func (k JournalKind) SubtypeTable() (string, error) {
	switch k {
	case JournalKindOperation:
		return "journal_operations", nil
	case JournalKindTaskEvent:
		return "journal_task_events", nil
	case JournalKindAuthority:
		return "journal_authorities", nil
	case JournalKindDecision:
		return "journal_decisions", nil
	case JournalKindEvidence:
		return "journal_evidence", nil
	default:
		return "", fmt.Errorf("provenance: no subtype table for %s", k)
	}
}

// ---------------------------------------------------------------------------
// Journal supertype and task_event subtype rows
// ---------------------------------------------------------------------------

// Row is one global journal supertype row (§2.1). Common fields live here
// exactly once; no subtype row repeats JournalKind, ActorID, or RecordedAt.
type Row struct {
	JournalID  JournalID   `json:"journalId"`
	Kind       JournalKind `json:"kind"`
	ActorID    ActorID     `json:"actorId"`
	RecordedAt time.Time   `json:"recordedAt"`
	// ProducedByOperationJournalID is the operation that produced this row.
	// NULL only on an operation anchor row (§2.1, §4.6). The foreign-key
	// constraint to journal_operations(JournalID) is added by the operations
	// slice (dayvidpham/provenance#5); at the journal-base layer the column
	// exists and stays NULL because no operation anchors are written yet.
	ProducedByOperationJournalID *JournalID `json:"producedByOperationJournalId,omitempty"`
}

// TaskEventRow is one journal_task_events subtype row (§5.1) joined to its
// supertype Row and canonical context set.
type TaskEventRow struct {
	Row
	TaskID    TaskID          `json:"taskId"`
	EventKind EventKind       `json:"eventKind"`
	Payload   json.RawMessage `json:"payload"`
	Contexts  []EventContext  `json:"contexts"`
}

// AppendTaskEventInput is the validated request to append one task-event
// journal row and its canonical context set as a single row of the global
// journal. The operations/reducer slices layer atomic multi-effect operations
// on top of this base append.
type AppendTaskEventInput struct {
	ActorID    ActorID
	TaskID     TaskID
	EventKind  EventKind
	RecordedAt time.Time
	Payload    json.RawMessage
	Contexts   []EventContext
}

// ---------------------------------------------------------------------------
// Ordered query surface (§8.3, §12)
// ---------------------------------------------------------------------------

// OrderDimension is the closed set of orderable query dimensions. Only
// JournalID is exposed: RecordedAt is deliberately absent so a request to
// order by wall-clock time is rejected rather than silently honored
// (§1, §12, ordering.yaml/query-ordering-ignores-caller-supplied-order-hint).
type OrderDimension int

const (
	// OrderByJournalID is the sole canonical ordering. Ascending JournalID.
	OrderByJournalID OrderDimension = iota
)

func (d OrderDimension) String() string {
	if d == OrderByJournalID {
		return "journal_id"
	}
	return fmt.Sprintf("OrderDimension(%d)", int(d))
}

// IsValid reports whether d is a supported ordering dimension.
func (d OrderDimension) IsValid() bool { return d == OrderByJournalID }

// JournalQueryV1 is the versioned, JournalID-ordered journal query. Values
// within TaskIDs and EventKinds are ORed; non-empty dimensions combine with
// AND. The first page leaves the cursor fields zero. Later pages repeat the
// filters and carry the page's SnapshotMaxJournalID and exclusive AfterJournalID
// cursor (§12 re-expresses the salvage RecordedAt cursor as a pure JournalID
// cursor — RecordedAt is never an ordering or cursor dimension).
type JournalQueryV1 struct {
	OrderBy              OrderDimension `json:"orderBy"`
	TaskIDs              []TaskID       `json:"taskIds,omitempty"`
	EventKinds           []EventKind    `json:"eventKinds,omitempty"`
	Contexts             []EventContext `json:"contexts,omitempty"`
	Limit                int            `json:"limit,omitempty"`
	SnapshotMaxJournalID JournalID      `json:"snapshotMaxJournalId,omitempty"`
	AfterJournalID       JournalID      `json:"afterJournalId,omitempty"`
}

// Validate rejects an unsupported ordering dimension before any query executes.
func (q JournalQueryV1) Validate() error {
	if !q.OrderBy.IsValid() {
		return fmt.Errorf("%w: requested order dimension %s is not exposed — "+
			"the journal is ordered exclusively by JournalID (RecordedAt is "+
			"audit metadata only); reissue the query with OrderByJournalID",
			ErrUnsupportedOrderDimension, q.OrderBy)
	}
	if q.Limit < 0 {
		return fmt.Errorf("%w: negative limit %d — pass 0 for unbounded or a positive page size",
			ErrInvalidQuery, q.Limit)
	}
	for i, ctx := range q.Contexts {
		if err := validateEventContext(ctx); err != nil {
			return fmt.Errorf("%w: Contexts[%d] is malformed: %v — each context filter value must be "+
				"constructed via TaskContext/ActivityContext/ActorContext/GitContext/ExtensionContext, "+
				"never assembled by hand", ErrInvalidQuery, i, err)
		}
	}
	return nil
}

// JournalCursorV1 is the exclusive JournalID cursor for the next page.
type JournalCursorV1 struct {
	SnapshotMaxJournalID JournalID `json:"snapshotMaxJournalId"`
	AfterJournalID       JournalID `json:"afterJournalId"`
}

// JournalTaskEventPageV1 is one page of task-event rows in ascending JournalID
// order plus the snapshot watermark and optional next cursor.
type JournalTaskEventPageV1 struct {
	Events               []TaskEventRow   `json:"events"`
	SnapshotMaxJournalID JournalID        `json:"snapshotMaxJournalId"`
	Next                 *JournalCursorV1 `json:"next,omitempty"`
}

// ---------------------------------------------------------------------------
// Projections (§8) — pure functions of ordered journal history
// ---------------------------------------------------------------------------

// TaskAttribution is one append-only cumulative attribution edge (§8.2): the
// earliest journal row establishing an actor's material contribution to a task.
type TaskAttribution struct {
	TaskID         TaskID    `json:"taskId"`
	ActorID        ActorID   `json:"actorId"`
	FirstJournalID JournalID `json:"firstJournalId"`
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	// ErrUnsupportedOrderDimension is returned when a query requests an
	// ordering dimension the contract does not expose (§1, §12).
	ErrUnsupportedOrderDimension = errors.New("provenance: unsupported order dimension")
	// ErrInvalidQuery is returned for otherwise malformed queries.
	ErrInvalidQuery = errors.New("provenance: invalid journal query")
	// ErrSubtypeIntegrity is returned when a journal row violates class-table
	// inheritance totality, exclusivity, or discriminator agreement (§10 rule 8).
	ErrSubtypeIntegrity = errors.New("provenance: journal subtype integrity violated")
	// ErrInvalidEventKind is returned for a malformed namespaced event kind.
	ErrInvalidEventKind = errors.New("provenance: invalid event kind")
)

// ValidateEventKind validates the common namespaced event-kind syntax (§5.1).
func ValidateEventKind(kind EventKind) error {
	if err := validateNamespacedName(string(kind)); err != nil {
		return fmt.Errorf("%w %q: %v", ErrInvalidEventKind, kind, err)
	}
	return nil
}

// ValidateOperationID rejects empty or control-bearing correlation identities
// without imposing the operations slice's replay semantics.
func ValidateOperationID(id OperationID) error {
	s := string(id)
	if s == "" {
		return fmt.Errorf("invalid operation ID: must be non-empty")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid operation ID: contains control character")
		}
	}
	return nil
}
