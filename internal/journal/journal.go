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

// OrderDimension is the closed set of orderable query dimensions. Two are
// exposed, and they serve deliberately different purposes (the display-vs-canonical
// firewall, §12):
//
//   - OrderByRecordedAt is a NON-CAUSAL display order — a readable timeline over
//     what happened, by wall-clock time. It NEVER establishes causality; replay,
//     authorization, and lifecycle decisions never consult it.
//   - OrderByJournalID is the sole CANONICAL order. Replay, authorization,
//     lifecycle, and convergence order by JournalID and nothing else.
//
// Any other dimension value is unexposed and rejected by Validate before a query
// runs, so a malformed order request fails loudly rather than being silently honored.
type OrderDimension int

const (
	// OrderByRecordedAt orders results by (RecordedAt, JournalID): the RecordedAt
	// wall-clock time with JournalID as the composite tiebreak, so equal timestamps
	// and backdated rows still yield a total, stable order. It is a readable
	// timeline over what happened and the DEFAULT for display-facing listing queries
	// (§12 "Readable timeline"); it is the zero value so an unqualified display query
	// gets the timeline order. It is explicitly NON-CAUSAL — it never reorders replay
	// or grants authority (the causal firewall stands).
	OrderByRecordedAt OrderDimension = iota
	// OrderByJournalID is the sole canonical ordering. Ascending JournalID. It stays
	// available explicitly and remains the ONLY order for replay/authorization/
	// lifecycle/convergence.
	OrderByJournalID
)

func (d OrderDimension) String() string {
	switch d {
	case OrderByRecordedAt:
		return "recorded_at"
	case OrderByJournalID:
		return "journal_id"
	default:
		return fmt.Sprintf("OrderDimension(%d)", int(d))
	}
}

// IsValid reports whether d is a supported ordering dimension.
func (d OrderDimension) IsValid() bool {
	return d == OrderByRecordedAt || d == OrderByJournalID
}

// JournalQueryV1 is the versioned journal query. Values within TaskIDs and
// EventKinds are ORed; non-empty dimensions combine with AND. OrderBy selects the
// display-vs-canonical firewall (§12): OrderByRecordedAt (the zero-value default)
// walks the readable timeline in (RecordedAt, JournalID) order; OrderByJournalID
// walks the canonical order. The first page leaves the cursor fields zero. Later
// pages repeat the filters and carry the page's SnapshotMaxJournalID watermark plus
// the exclusive cursor: AfterJournalID alone under OrderByJournalID, or the composite
// (AfterRecordedAt, AfterJournalID) under OrderByRecordedAt so a timeline walk never
// skips or duplicates a row across equal timestamps or backdated rows. The snapshot
// watermark is JournalID-bounded under both orders.
type JournalQueryV1 struct {
	OrderBy    OrderDimension `json:"orderBy"`
	TaskIDs    []TaskID       `json:"taskIds,omitempty"`
	EventKinds []EventKind    `json:"eventKinds,omitempty"`
	Contexts   []EventContext `json:"contexts,omitempty"`
	Limit      int            `json:"limit,omitempty"`
	// SnapshotMaxJournalID pins the page to a JournalID-bounded snapshot; repeated
	// on every later page of a walk (both orders).
	SnapshotMaxJournalID JournalID `json:"snapshotMaxJournalId,omitempty"`
	// AfterJournalID is the exclusive JournalID cursor: the whole cursor under
	// OrderByJournalID, and the composite tiebreak component under OrderByRecordedAt.
	AfterJournalID JournalID `json:"afterJournalId,omitempty"`
	// AfterRecordedAt is the RecordedAt component of the composite display cursor
	// (OrderByRecordedAt only): together with AfterJournalID it is the exclusive
	// lower bound (AfterRecordedAt, AfterJournalID). Ignored under OrderByJournalID.
	AfterRecordedAt time.Time `json:"afterRecordedAt,omitempty"`
}

// Validate rejects an unsupported ordering dimension before any query executes.
func (q JournalQueryV1) Validate() error {
	if !q.OrderBy.IsValid() {
		return fmt.Errorf("%w: requested order dimension %s is not exposed — "+
			"the journal exposes OrderByRecordedAt (a non-causal readable-timeline "+
			"display order, the default) and OrderByJournalID (the canonical order); "+
			"reissue the query with one of those",
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

// JournalCursorV1 is the exclusive cursor for the next page. Under
// OrderByJournalID only AfterJournalID is meaningful; under OrderByRecordedAt the
// composite (AfterRecordedAt, AfterJournalID) is the exclusive lower bound so the
// timeline walk is duplicate-free across ties and timestamp regressions.
type JournalCursorV1 struct {
	SnapshotMaxJournalID JournalID `json:"snapshotMaxJournalId"`
	AfterJournalID       JournalID `json:"afterJournalId"`
	// AfterRecordedAt is set only on a next-cursor produced by an OrderByRecordedAt
	// page: the RecordedAt of the page's last row.
	AfterRecordedAt time.Time `json:"afterRecordedAt,omitempty"`
}

// JournalTaskEventPageV1 is one page of task-event rows in the query's order
// (canonical JournalID or the readable-timeline (RecordedAt, JournalID)) plus the
// snapshot watermark and optional next cursor.
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
	// ordering dimension the contract does not expose — i.e. neither the
	// canonical OrderByJournalID nor the display OrderByRecordedAt (§1, §12).
	ErrUnsupportedOrderDimension = errors.New("provenance: unsupported order dimension")
	// ErrInvalidQuery is returned for otherwise malformed queries.
	ErrInvalidQuery = errors.New("provenance: invalid journal query")
	// ErrSubtypeIntegrity is returned when a journal row violates class-table
	// inheritance totality, exclusivity, or discriminator agreement (§10 rule 8).
	ErrSubtypeIntegrity = errors.New("provenance: journal subtype integrity violated")
	// ErrActorPlacement is returned when a journal row violates the anchor-only
	// actor-placement invariant (§2.1, §10 rule 5): a stored actor_id must be
	// present iff the row is an anchor (produced_by_operation_journal_id IS NULL).
	// A subordinate row carrying an actor, or an anchor row missing one, is rejected —
	// by Apply on the input, and by VerifyIntegrity over stored rows, backing the
	// CHECK constraint that also enforces it.
	ErrActorPlacement = errors.New("provenance: journal actor placement violated (actor present iff anchor row)")
	// ErrWatermarkMissing is returned when a tasks row carries no last_journal_id
	// watermark (§8.1): every task row must reflect a journal position, so an
	// un-anchored (NULL-watermark) row is an un-journaled task the tightening forbids.
	// A fresh native database enforces this at the schema level (NOT NULL); a legacy
	// database reaches it by anchoring every row through MigrateLegacyBaseline (§13).
	// VerifyIntegrity reports it over stored rows.
	ErrWatermarkMissing = errors.New("provenance: task row has no journal watermark (un-journaled task)")
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
