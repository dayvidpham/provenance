package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// replay.go defines the shared-reducer replay, legacy-baseline migration, and
// external-schema preflight semantics types for the shared-reducer layer
// (docs/journal-relational-contract.md §9, §13, §15). The concrete SQLite
// reducer that folds them lives in internal/sqlite. Open/startup replay folds
// the entire journal through the same per-effect reducer steps Apply uses (§9.2,
// one fold, no second switch); migration installs deterministic legacy baselines
// under the genesis bootstrap authority (§13); preflight verifies the external
// pre-journal schema shape in both directions before any write (§13 preflight).

// ---------------------------------------------------------------------------
// Task-status projection (§8.1)
// ---------------------------------------------------------------------------

// TaskStatus is the closed lifecycle-status projection derived by the shared
// reducer from Provenance's own namespaced lifecycle task-event kinds. It is
// backed by the existing statuses lookup (open/in_progress/closed) whose ids the
// tasks projection stores; the reducer never interprets a caller-domain kind,
// only the provenance.task.* lifecycle kinds Provenance itself defines (§5.1,
// §8.1).
type TaskStatus int

const (
	TaskStatusOpen       TaskStatus = 0 // statuses.id 0
	TaskStatusInProgress TaskStatus = 1 // statuses.id 1
	TaskStatusClosed     TaskStatus = 2 // statuses.id 2
)

func (s TaskStatus) String() string {
	switch s {
	case TaskStatusOpen:
		return "open"
	case TaskStatusInProgress:
		return "in_progress"
	case TaskStatusClosed:
		return "closed"
	default:
		return fmt.Sprintf("TaskStatus(%d)", int(s))
	}
}

// TaskStatusFromString is the inverse of String for the three seeded statuses,
// reporting ok=false for any other token. It is the single decoder the migration
// marker's captured legacy status (EncodeMigrationMarkerPayload) round-trips
// through so the status projection stays reproducible solely from journal history
// (§13, §15).
func TaskStatusFromString(s string) (TaskStatus, bool) {
	switch s {
	case "open":
		return TaskStatusOpen, true
	case "in_progress":
		return TaskStatusInProgress, true
	case "closed":
		return TaskStatusClosed, true
	default:
		return 0, false
	}
}

// LegacyStatusPayloadKey is the JSON object key the migration-marker task_event
// (EventKindTaskMigrated) carries so the migrated task's preserved legacy status
// is recorded IN the journal — not read from the mutable tasks row — and is thus
// reproducible solely from ordered journal history when Open re-derives the status
// projection from empty (§13, §15). Without it, a migrated-but-never-relifecycled
// task's status would not be journal-derivable and a from-empty convergence check
// could neither reproduce nor verify it.
const LegacyStatusPayloadKey = "legacy_status"

// EncodeMigrationMarkerPayload builds the migration-marker task_event payload that
// captures a migrated task's legacy status verbatim (§13 item 1). The shared
// reducer decodes it via DecodeLegacyStatus to seed the status projection during
// both live migration and Open-time from-empty replay, so both derive the same
// value from the same journal fact (§9.2).
func EncodeMigrationMarkerPayload(status TaskStatus) json.RawMessage {
	// Encoded by hand from a closed value set so the marker payload is a stable,
	// canonical object rather than depending on struct field/tag ordering.
	return json.RawMessage(fmt.Sprintf(`{%q:%q}`, LegacyStatusPayloadKey, status.String()))
}

// DecodeLegacyStatus recovers the legacy status a migration marker captured
// (EncodeMigrationMarkerPayload). ok is false when the payload carries no
// legacy-status key (e.g. a non-marker task_event payload); an unrecognized status
// token is a typed error rather than a silent default, so a corrupted marker fails
// closed rather than seeding an arbitrary status.
func DecodeLegacyStatus(payload []byte) (TaskStatus, bool, error) {
	if len(payload) == 0 {
		return 0, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return 0, false, fmt.Errorf("provenance: decode migration-marker payload: %w", err)
	}
	raw, ok := fields[LegacyStatusPayloadKey]
	if !ok {
		return 0, false, nil
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
		return 0, false, fmt.Errorf("provenance: decode migration-marker %s: %w", LegacyStatusPayloadKey, err)
	}
	status, valid := TaskStatusFromString(token)
	if !valid {
		return 0, false, fmt.Errorf(
			"provenance: migration marker carries unrecognized legacy status %q — where: status "+
				"projection seed (§13, §15); impact: the status projection cannot be reproduced from journal "+
				"history; fix: the marker payload must record one of open/in_progress/closed", token)
	}
	return status, true, nil
}

// Lifecycle task-event kinds the reducer projects to a status transition (§8.1).
// This is a small closed set of Provenance-owned namespaced kinds — not a
// sprawling caller-domain switch — folded identically by Apply and Open.
const (
	EventKindTaskCreated  EventKind = "provenance.task.created"
	EventKindTaskStarted  EventKind = "provenance.task.started"
	EventKindTaskStopped  EventKind = "provenance.task.stopped"
	EventKindTaskClosed   EventKind = "provenance.task.closed"
	EventKindTaskReopened EventKind = "provenance.task.reopened"
	EventKindTaskMigrated EventKind = "provenance.task.migrated"

	// EventKindTaskUpdated records a materialized-metadata mutation (title,
	// description, priority, phase, notes) of an existing task. It is a NON-lifecycle
	// kind — StatusForEventKind returns ok=false for it, so it only attributes the
	// committing actor and advances the watermark (§8.1, §8.2). The mutated columns it
	// carries are materialized-only projections of the tasks row, written directly in
	// the fold like the birth metadata EventKindTaskCreated writes, and are NOT part of
	// the §15 owner/status/watermark convergence set.
	EventKindTaskUpdated EventKind = "provenance.task.updated"

	// Journaled relationship / annotation mutation-family kinds (§6, as amended by #5).
	// Each is a fixed per-family kind — never a payload-generalized dispatch — carried on
	// a journal_task_events row, so who added/removed an edge/label/comment, under which
	// authority, at which journal position is queryable from the journal (who-provenance).
	// The operands (edge target/kind, label text, comment id/author/body) live in the
	// row payload; the shared reducer folds them into the edges/labels/comments domain
	// projections (§6, §15 convergence), never into status. They are NON-lifecycle, so
	// StatusForEventKind returns ok=false for them.
	EventKindEdgeAdded    EventKind = "provenance.edge.added"
	EventKindEdgeRemoved  EventKind = "provenance.edge.removed"
	EventKindLabelAdded   EventKind = "provenance.label.added"
	EventKindLabelRemoved EventKind = "provenance.label.removed"
	EventKindCommentAdded EventKind = "provenance.comment.added"
)

// StatusForEventKind reports the status a lifecycle task-event kind projects to,
// and whether the kind is a status-changing lifecycle kind at all. A non-lifecycle
// kind (e.g. provenance.task.updated or any caller kind) returns ok=false and does
// not move the status projection. This is the single source of truth the shared
// reducer step consults for both Apply and Open (§9.2).
// The migration marker (EventKindTaskMigrated) is deliberately NOT a fixed
// kind→status mapping here: migration preserves each legacy task's own status
// verbatim (§13), so the marker instead CAPTURES that status in its payload
// (EncodeMigrationMarkerPayload) and the shared reducer seeds the projection from
// the captured value (DecodeLegacyStatus), never overwriting a closed legacy task's
// status to open. Because the captured status lives in the journal row, it remains
// reproducible solely from journal history when Open re-derives from empty (§15);
// StatusForEventKind stays the source of truth only for the fixed-mapping lifecycle
// kinds (created/started/reopened/closed).
//
// EventKindTaskStarted → in_progress is a fixed-mapping lifecycle kind added under
// §8.1's own forward pointer ("route that status change through a journal lifecycle
// event once the direct-write path retires"): the tightening package IS that
// retirement, and in_progress (statuses.id 1) is first-class in the native status
// domain, so a native task reaches it through this journaled event rather than an
// un-journaled direct write. It is a fixed kind→status mapping deliberately — NOT a
// generalized status-from-payload — so the closed lifecycle vocabulary stays
// strongly typed (the migration marker's payload-captured status stays the sole
// special case, §13).
//
// EventKindTaskStopped → open is the fixed-mapping lifecycle kind for the
// in_progress → open transition (Session.Stop): a task that was started can be
// halted back to open without closing it. Like reopened it projects to open; the
// static FSM (ValidateStatusTransition) distinguishes the two by their legal source
// status — stopped only from in_progress, reopened only from closed — so the target
// status alone never has to disambiguate the two kinds.
func StatusForEventKind(kind EventKind) (TaskStatus, bool) {
	switch kind {
	case EventKindTaskCreated, EventKindTaskReopened, EventKindTaskStopped:
		return TaskStatusOpen, true
	case EventKindTaskStarted:
		return TaskStatusInProgress, true
	case EventKindTaskClosed:
		return TaskStatusClosed, true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Replay result (§9, §15)
// ---------------------------------------------------------------------------

// TaskProjection is one task's reducer-derived current state as of the replayed
// watermark (§8.1): its owner-responsibility occupant (nil when none is active),
// its lifecycle status, and the JournalID whose ordered history the state reflects.
type TaskProjection struct {
	TaskID        TaskID
	Owner         *ActorID
	Status        TaskStatus
	LastJournalID JournalID
}

// ReplayResult is the outcome of a full Open-time replay (§9.2): the recomputed
// per-task projections and how many journal rows were folded, so a caller can
// assert convergence against the live projection Apply produced incrementally.
type ReplayResult struct {
	Tasks      []TaskProjection
	RowsFolded int
}

// ProjectionForTask returns the replayed projection for a task, or ok=false when
// the replay folded no rows touching it.
func (r ReplayResult) ProjectionForTask(id TaskID) (TaskProjection, bool) {
	for _, p := range r.Tasks {
		if p.TaskID.String() == id.String() {
			return p, true
		}
	}
	return TaskProjection{}, false
}

// ProjectionDivergenceError is the typed, actionable fail-closed error returned
// when the stored incremental projection does not equal the projection Open's
// full replay derives from ordered journal history (§9.2, §15). It carries every
// component of the repo's six-part actionable-error contract.
type ProjectionDivergenceError struct {
	Operation string // where: the replay routine
	Task      TaskID // what: the task whose projection diverged
	Field     string // what: the diverging projection field (owner/status/watermark/attribution)
	Stored    string // what: the stored incremental value
	Replayed  string // what: the value the shared from-empty fold derived
	Why       string // why
	Impact    string // caller impact
	Fix       string // how to fix
}

func (e *ProjectionDivergenceError) Error() string {
	return fmt.Sprintf(
		"provenance: projection divergence on task %s field %s — stored=%q replayed=%q; why: %s; "+
			"where: %s; when: Open-time full replay convergence check (§9.2, §15); impact: %s; fix: %s",
		e.Task.String(), e.Field, e.Stored, e.Replayed, e.Why, e.Operation, e.Impact, e.Fix)
}

// Is lets errors.Is recover the sentinel for the typed divergence error.
func (e *ProjectionDivergenceError) Is(target error) bool { return target == ErrProjectionDivergence }

// ---------------------------------------------------------------------------
// Legacy-baseline migration (§13)
// ---------------------------------------------------------------------------

// LegacyTaskRow is one pre-journal task presented to migration (§13). The
// deterministic pre-migration sort is (CreatedAt ascending, then ID ascending);
// RawOwner is the legacy owner string (possibly empty, possibly unmappable);
// RecordedAt provenance is taken honestly from UpdatedAt (marker/started) and
// ClosedAt (ended), never from the migration's own wall-clock time.
type LegacyTaskRow struct {
	ID        TaskID
	RawOwner  string // legacy owner string; "" means no owner
	Status    TaskStatus
	CreatedAt time.Time  // deterministic sort key (secondary: ID)
	UpdatedAt time.Time  // honest RecordedAt for the marker + started transition
	ClosedAt  *time.Time // honest RecordedAt for the ended transition; nil falls back to UpdatedAt
}

// MigrationInput is the whole-batch migration request (§13). Owners resolves a
// legacy owner string to a registered ActorID; a non-empty RawOwner absent from
// Owners fails the WHOLE batch with MigrationOwnerUnmappableError before any row
// is written (§13 item 4). System is the migration's committing actor (the
// pasture-system actor, §2.1 committing-actor model), BootstrapAuthority the
// genesis-established bootstrap the per-task anchors execute under (§4.6, §13).
type MigrationInput struct {
	System             ActorID
	BootstrapAuthority JournalID
	Owners             map[string]ActorID
	Legacy             []LegacyTaskRow
}

// MigrationResult reports how a migration run resolved (§13). BaselineAnchorsCreated
// counts freshly written per-task anchors; ShortCircuited counts anchors an
// idempotent re-run returned unchanged via the §9.4 short-circuit; TasksMigrated is
// the total legacy tasks processed.
type MigrationResult struct {
	BaselineAnchorsCreated int
	ShortCircuited         int
	TasksMigrated          int
}

// MigrationOwnerUnmappableError is the typed, six-field fail-closed error for a
// legacy owner string that resolves to no registered ActorID (§13 item 4, §13.1).
// Every field is non-empty on return; the whole migration transaction rolls back.
type MigrationOwnerUnmappableError struct {
	Operation string // where: the migration routine
	Task      TaskID // what: the offending legacy task id
	RawOwner  string // what: the raw unmappable owner string
	Stage     string // where/when: the owner-resolution stage, before any baseline row is committed
	Why       string // why
	Impact    string // caller impact: whole transaction rolled back, zero baselines
	Fix       string // how to fix
	Cause     error  // underlying cause, if any
}

func (e *MigrationOwnerUnmappableError) Error() string {
	return fmt.Sprintf(
		"provenance: migration owner %q on legacy task %s is unmappable — why: %s; where: %s; "+
			"when: %s; impact: %s; fix: %s",
		e.RawOwner, e.Task.String(), e.Why, e.Operation, e.Stage, e.Impact, e.Fix)
}

func (e *MigrationOwnerUnmappableError) Unwrap() error { return e.Cause }

// Is lets errors.Is recover the sentinel for the typed unmappable-owner error.
func (e *MigrationOwnerUnmappableError) Is(target error) bool {
	return target == ErrMigrationOwnerUnmappable
}

// SchemaPreflightError is the typed, six-field fail-closed error for an external
// pre-journal schema that does not match the exact shape this build understands —
// a missing expected table, a missing expected column, or an unexpected extra
// column (§13 preflight, §13.1). It is raised strictly before any transaction
// opens; every field is non-empty on return.
type SchemaPreflightError struct {
	Operation     string // where: the preflight routine
	ExpectedShape string // what: the table/column the preflight expected
	FoundShape    string // what: what it actually found
	Stage         string // where/when: the specific table/column check, before any transaction opens
	Why           string // why
	Impact        string // caller impact: no row written, activation halts
	Fix           string // how to fix
	Cause         error  // underlying cause, if any
}

func (e *SchemaPreflightError) Error() string {
	return fmt.Sprintf(
		"provenance: schema preflight failed — expected %s but found %s; why: %s; where: %s; "+
			"when: %s; impact: %s; fix: %s",
		e.ExpectedShape, e.FoundShape, e.Why, e.Operation, e.Stage, e.Impact, e.Fix)
}

func (e *SchemaPreflightError) Unwrap() error { return e.Cause }

// Is lets errors.Is recover the sentinel for the typed preflight error.
func (e *SchemaPreflightError) Is(target error) bool { return target == ErrSchemaPreflight }

// MigrationBaselineOperationID returns the deterministic per-task baseline anchor
// OperationID (§13): provenance.migration.baseline--<legacy tasks.id>. Because it
// is a pure function of the legacy id, a re-run presents the identical OperationID
// per task and hits §9.4's idempotent short-circuit, never a duplicate anchor.
func MigrationBaselineOperationID(id TaskID) OperationID {
	return OperationID("provenance.migration.baseline--" + id.String())
}

// MigrationBaselineAssignmentID returns the deterministic per-task
// owner-responsibility episode identity a legacy-assigned baseline installs (§13
// item 2), so a re-run references the same episode rather than minting a new one.
func MigrationBaselineAssignmentID(id TaskID) AssignmentID {
	return AssignmentID("provenance.migration.episode--" + id.String())
}

var (
	// ErrMigrationOwnerUnmappable wraps a typed MigrationOwnerUnmappableError (§13).
	ErrMigrationOwnerUnmappable = errors.New("provenance: migration owner is unmappable")
	// ErrSchemaPreflight wraps a typed SchemaPreflightError (§13 preflight).
	ErrSchemaPreflight = errors.New("provenance: external schema preflight failed")
	// ErrProjectionDivergence wraps a typed ProjectionDivergenceError (§9.2, §15).
	ErrProjectionDivergence = errors.New("provenance: replay projection diverged from stored projection")
	// ErrMigrationFault is the sentinel a migration fault/cancellation injection
	// surfaces so the whole-batch fail-closed rollback (§9.5, §13) is observable as
	// a typed outcome rather than a raw driver error.
	ErrMigrationFault = errors.New("provenance: migration aborted before completion")
	// ErrInjectedFault is the sentinel an intra-operation fault/cancellation
	// injection surfaces so a corpus history can distinguish a deliberately injected
	// abort (proving §9.5 fail-closed atomicity) from an incidental error.
	ErrInjectedFault = errors.New("provenance: operation aborted before commit")
	// ErrDishonestMigrationTimestamp guards regression (g): a migration-sourced
	// row's RecordedAt MUST trace to a legacy column value (updated_at/closed_at),
	// never to a wall-clock read taken during migration (§13 items 1-3, §12).
	ErrDishonestMigrationTimestamp = errors.New("provenance: migration RecordedAt does not trace to a legacy source column")
)
