package journal

import (
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

// Lifecycle task-event kinds the reducer projects to a status transition (§8.1).
// This is a small closed set of Provenance-owned namespaced kinds — not a
// sprawling caller-domain switch — folded identically by Apply and Open.
const (
	EventKindTaskCreated  EventKind = "provenance.task.created"
	EventKindTaskClosed   EventKind = "provenance.task.closed"
	EventKindTaskReopened EventKind = "provenance.task.reopened"
	EventKindTaskMigrated EventKind = "provenance.task.migrated"
)

// StatusForEventKind reports the status a lifecycle task-event kind projects to,
// and whether the kind is a status-changing lifecycle kind at all. A non-lifecycle
// kind (e.g. provenance.task.updated or any caller kind) returns ok=false and does
// not move the status projection. This is the single source of truth the shared
// reducer step consults for both Apply and Open (§9.2).
// The migration marker (EventKindTaskMigrated) is deliberately NOT a
// status-changing kind: migration preserves each legacy task's own status verbatim
// (§13) and must never overwrite a closed legacy task's status to open. It records
// the fact of migration, not a lifecycle transition.
func StatusForEventKind(kind EventKind) (TaskStatus, bool) {
	switch kind {
	case EventKindTaskCreated, EventKindTaskReopened:
		return TaskStatusOpen, true
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
	Field     string // what: the diverging projection field (owner/status/watermark)
	Stored    string // what: the stored incremental value
	Replayed  string // what: the value the shared fold derived
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
