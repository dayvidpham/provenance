package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dayvidpham/provenance/internal/journal"
)

// replay.go implements the shared-reducer projection step, Open-time full replay,
// and the projection-convergence check (docs/journal-relational-contract.md §9,
// §15). projectJournalRow is the SINGLE per-row reducer step: Apply calls it
// after committing each effect, and ReplayProjections calls it for every persisted
// row in JournalID order. There is exactly one switch over JournalKind here and no
// second Open-time switch — the direct fix for the salvage regression where a live
// Apply reducer and a separate Open-time projection switch drifted out of sync
// (§9.2).

// Status ids match the seeded statuses lookup (open/in_progress/closed).
const (
	statusOpenID   = int(journal.TaskStatusOpen)
	statusClosedID = int(journal.TaskStatusClosed)
)

// ---------------------------------------------------------------------------
// The single shared reducer step (§9.2)
// ---------------------------------------------------------------------------

// projectJournalRow advances the §8 projections (tasks.owner_id,
// tasks.status_id, tasks.last_journal_id, and task_attributions) for one already-
// committed journal row, derived solely from the persisted row and the ordered
// history before it. Apply invokes it incrementally after each effect; Open's full
// replay invokes it per row from JournalID 1. Both share this one fold (§9.2). It
// assumes the caller owns scope.conn and runs inside the caller's transaction.
func (scope *connScope) projectJournalRow(jid int64) error {
	var (
		kind       = -1
		actorRaw   string
		recordedAt int64
	)
	// Read the committing actor through journal_attributed (§8.5): a subordinate row
	// stores actor_id NULL, so effective_actor_id derives it from the row's anchor —
	// never a bare read of the NULL column. The single shared reducer step therefore
	// attributes the same committing actor whether Apply folds a just-produced
	// subordinate row or Open replays it (§9.2).
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT kind_id, effective_actor_id, recorded_at FROM journal_attributed WHERE journal_id = ?1", jid).Scan(&kind, &actorRaw, &recordedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("project journal row %d: no such journal row", jid)
		}
		return fmt.Errorf("project journal row %d: load supertype: %w", jid, err)
	}
	committing, err := journalParseActor(actorRaw)
	if err != nil {
		return err
	}

	switch journal.JournalKind(kind) {
	case journal.JournalKindOperation:
		return nil // an operation anchor projects nothing
	case journal.JournalKindTaskEvent:
		return scope.projectTaskEventRow(jid, committing, recordedAt)
	case journal.JournalKindAuthority:
		return scope.projectAuthorityRow(jid)
	case journal.JournalKindDecision:
		return scope.projectTaskScopedRow(jid, committing, taskScopedDecision)
	case journal.JournalKindEvidence:
		return scope.projectTaskScopedRow(jid, committing, taskScopedEvidence)
	case journal.JournalKindActivity:
		// Activity birth rows have no task-scoped watermark or attribution to advance.
		// The journal_activity_creations row exists and the activities row was inserted
		// by the fold; no additional projection state changes here.
		return nil
	default:
		return fmt.Errorf("project journal row %d: unknown journal kind %d", jid, kind)
	}
}

// projectTaskEventRow attributes the committing (authoring) actor (§8.2),
// projects any lifecycle-status transition Provenance defines for its own
// namespaced kind (§8.1), and advances the watermark.
func (scope *connScope) projectTaskEventRow(jid int64, committing journal.ActorID, recordedAt int64) error {
	var (
		taskRaw string
		kindStr string
		payload []byte
	)
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT task_id, event_kind, payload FROM journal_task_events WHERE journal_id = ?1", jid).Scan(&taskRaw, &kindStr, &payload); err != nil {
		return fmt.Errorf("project task_event %d: %w", jid, err)
	}
	payload = append([]byte(nil), payload...)
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if err := scope.insertAttribution(task, committing, jid); err != nil {
		return err
	}
	canonicalEffect, canonical, err := scope.canonicalEffectForJournalRow(jid)
	if err != nil {
		return err
	}
	if canonical {
		if canonicalEffect.TaskID != task || canonicalEffect.EventKind != "" && canonicalEffect.EventKind != journal.EventKind(kindStr) {
			return fmt.Errorf("provenance: canonical mutation row %d disagrees with journal_task_events task/event facts — where: startup canonical validation; impact: database open fails without projection writes; fix: restore the journal row and canonical bytes from the same committed operation", jid)
		}
		if scope.projectionTarget == projectionTargetShadow {
			switch canonicalEffect.Sort {
			case journal.EffectTaskCreate, journal.EffectTaskCreateAllocated:
				if err := scope.insertCanonicalShadowTask(canonicalEffect, recordedAt, jid); err != nil {
					return err
				}
			case journal.EffectTaskEvent:
				if err := scope.materializeCanonicalShadowTaskEvent(canonicalEffect, recordedAt); err != nil {
					return err
				}
			}
		}
		if scope.projectionTarget == projectionTargetLive && canonicalEffect.Sort == journal.EffectTaskEvent && !journal.IsMutationFamilyKind(canonicalEffect.EventKind) && canonicalEffect.EventKind != journal.EventKindTaskMigrated {
			return projectV1TaskEvent(scope.ctx, allocationSQLTx{conn: scope.conn}, committing, jid, recordedAt, canonicalEffect)
		}
	}
	// Journaled relationship/annotation mutation families (§6 amendment) fold into the
	// edges/labels/comments domain projection through the same shared reducer step Apply
	// and Open both run (§9.2), so those tables are re-derivable from ordered history and
	// covered by the §15 convergence check. They are non-lifecycle, so they never reach
	// the status branches below.
	if journal.IsMutationFamilyKind(journal.EventKind(kindStr)) {
		return scope.projectMutationFamilyRow(task, journal.EventKind(kindStr), payload, jid, recordedAt)
	}
	// A migration marker seeds the status projection from the legacy status it
	// captured in its own payload (§13, §15) — read from the journal row, never from
	// the mutable tasks row — so both live migration and Open's from-empty replay
	// derive the identical status from the identical journal fact (§9.2).
	if journal.EventKind(kindStr) == journal.EventKindTaskMigrated {
		status, ok, derr := journal.DecodeLegacyStatus(payload)
		if derr != nil {
			return fmt.Errorf("project task_event %d: %w", jid, derr)
		}
		if ok {
			return scope.projectTaskStatus(task, status, jid, recordedAt)
		}
		return scope.advanceWatermark(task, jid)
	}
	if status, isLifecycle := journal.StatusForEventKind(journal.EventKind(kindStr)); isLifecycle {
		// The static status FSM (§8.1) governs the four TRANSITION lifecycle kinds
		// (started/stopped/closed/reopened). It is enforced HERE — in the single shared
		// reducer step Apply and Open both run (§9.2) — so a live Apply rejects an illegal
		// transition before commit and Open's from-empty replay applies the identical rule
		// to the identical row. The current status is read from the projection target
		// (real tasks during Apply, the shadow table during replay), which reflects the
		// history strictly before this row. A forced coercion carries a marker in this
		// row's payload and skips the FSM, so the coercion is reproducible either way.
		if journal.IsTransitionLifecycleKind(journal.EventKind(kindStr)) {
			forced, ferr := journal.DecodeForcedTransition(payload)
			if ferr != nil {
				return fmt.Errorf("project task_event %d: %w", jid, ferr)
			}
			if !forced {
				current, cerr := scope.readProjTaskStatus(task)
				if cerr != nil {
					return cerr
				}
				if verr := journal.ValidateStatusTransition(current, journal.EventKind(kindStr)); verr != nil {
					return fmt.Errorf("project task_event %d: %w", jid, verr)
				}
			}
		}
		return scope.projectTaskStatus(task, status, jid, recordedAt)
	}
	return scope.advanceWatermark(task, jid)
}

// readProjTaskStatus reads a task's current lifecycle status from the projection
// target (the real tasks table during a live Apply, the shadow table during a from-empty
// replay derivation), so the status FSM checks the transition against the status derived
// from history strictly before the row being folded (§8.1, §15).
func (scope *connScope) readProjTaskStatus(task journal.TaskID) (journal.TaskStatus, error) {
	var status journal.TaskStatus
	err := scope.conn.QueryRowContext(scope.ctx, scope.projectionTarget.readTaskStatusQuery(), task.String()).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read current status for %q: %w", task, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf(
			"provenance: status FSM cannot read current status of task %q — where: shared-reducer "+
				"status projection (§8.1); when: folding a transition lifecycle event; impact: nothing "+
				"committed; fix: the task row must exist (born via Session.Create) before a lifecycle "+
				"transition is folded against it", task)
	}
	return status, nil
}

// projectAuthorityRow projects an authority row. A bootstrap authority
// touches no task and projects nothing; an assignment transition attributes the
// episode occupant on its started transition (§8.2), and recomputes the
// owner-responsibility projection on the owner slot (§8.1). The reducer reads the
// episode the transition belongs to rather than re-deriving from the caller.
func (scope *connScope) projectAuthorityRow(jid int64) error {
	var (
		assignment string
		transition = -1
		hasTrans   bool
	)
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT assignment_id, transition_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", jid).Scan(&assignment, &transition)
	if err == nil {
		hasTrans = true
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("project authority %d: load transition: %w", jid, err)
	}
	if !hasTrans {
		return nil // bootstrap authority: no task projection
	}
	var (
		taskRaw     string
		occupantRaw string
		slot        = -1
	)
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT task_id, actor_id, slot_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", assignment).Scan(&taskRaw, &occupantRaw, &slot); err != nil {
		return fmt.Errorf("project authority %d: load episode %q: %w", jid, assignment, err)
	}
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if transition == transitionStartedID {
		occupant, err := journalParseActor(occupantRaw)
		if err != nil {
			return err
		}
		if err := scope.insertAttribution(task, occupant, jid); err != nil {
			return err
		}
	}
	if slot == slotOwnerResponsibilityID {
		return scope.recomputeTaskOwner(task, jid)
	}
	return scope.advanceWatermark(task, jid)
}

// projectTaskScopedRow attributes the committing actor for a task-scoped
// decision/evidence row and advances the watermark (§8.2). An untasked row
// attributes nothing.
type taskScopedTable uint8

const (
	taskScopedDecision taskScopedTable = iota + 1
	taskScopedEvidence
)

func (table taskScopedTable) label() string {
	switch table {
	case taskScopedDecision:
		return "journal_decisions"
	case taskScopedEvidence:
		return "journal_evidence"
	default:
		panic("unknown task-scoped table")
	}
}

func (scope *connScope) projectTaskScopedRow(jid int64, committing journal.ActorID, table taskScopedTable) error {
	var taskRaw string
	hasTask := false
	var taskValue sql.NullString
	if err := scope.conn.QueryRowContext(scope.ctx, table.query(), jid).Scan(&taskValue); err != nil {
		return fmt.Errorf("project %s %d: %w", table.label(), jid, err)
	}
	if taskValue.Valid {
		taskRaw = taskValue.String
		hasTask = true
	}
	if !hasTask {
		return nil
	}
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if err := scope.insertAttribution(task, committing, jid); err != nil {
		return err
	}
	return scope.advanceWatermark(task, jid)
}

// projectTaskStatus materializes the lifecycle-status projection on tasks
// (§8.1): status_id, closed_at (set on close, cleared on re-open), and the
// watermark. Like owner_id, status here is written only by the shared reducer for
// journal-anchored lifecycle events.
func (scope *connScope) projectTaskStatus(task journal.TaskID, status journal.TaskStatus, jid, recordedAt int64) error {
	var closedAt any
	if status == journal.TaskStatusClosed {
		closedAt = recordedAt
	}
	// Targets the real tasks table during a live Apply and the shadow tasks table
	// during a from-empty replay derivation (§8.1, §15).
	if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.projectTaskStatusQuery(), int(status), closedAt, jid, task.String()); err != nil {
		return fmt.Errorf("project task status for %q: %w", task, err)
	}
	return nil
}

func (target projectionTarget) readTaskStatusQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT status_id FROM tasks WHERE id=?1"
	case projectionTargetShadow:
		return "SELECT status_id FROM shadow_tasks WHERE id=?1"
	default:
		panic("unknown projection target")
	}
}

func (table taskScopedTable) query() string {
	switch table {
	case taskScopedDecision:
		return "SELECT task_id FROM journal_decisions WHERE journal_id=?1"
	case taskScopedEvidence:
		return "SELECT task_id FROM journal_evidence WHERE journal_id=?1"
	default:
		panic("unknown task-scoped table")
	}
}

func (target projectionTarget) projectTaskStatusQuery() string {
	switch target {
	case projectionTargetLive:
		return "UPDATE tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4"
	case projectionTargetShadow:
		return "UPDATE shadow_tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4"
	default:
		panic("unknown projection target")
	}
}

// ---------------------------------------------------------------------------
// Open-time full replay (§9.2, §15)
// ---------------------------------------------------------------------------

// Shadow projection table names (§15 SHADOW DERIVATION). ReplayProjections folds
// the whole journal into these connection-scoped temporary tables — which carry the
// projection columns but NONE of the real tables' constraints (no NOT NULL on the
// watermark, no foreign keys) — and diffs them against the real tables. The real
// tasks / task_attributions rows are never mutated during the check, so the check is
// constraint-independent (the NOT NULL tasks.last_journal_id tightening cannot break
// a from-empty refold). The complete stored-and-derived comparison runs in a scoped
// savepoint, which holds one stable WAL snapshot while retaining this connection's
// TEMP-table affinity and composing with activation's outer transaction.
const (
	shadowTasksTable    = "shadow_tasks"
	shadowAttribTable   = "shadow_task_attributions"
	shadowEdgesTable    = "shadow_edges"
	shadowLabelsTable   = "shadow_labels"
	shadowCommentsTable = "shadow_comments"
)

// ReplayProjections re-derives EVERY projection from an EMPTY slate by folding the
// entire journal in JournalID order through the same projectJournalRow
// reducer step Apply uses (§9.2, §15), then verifies the stored projection equals
// that genuine from-empty re-derivation. Unlike an on-top re-fold (which only
// detects drift in a field some journal row actually writes, so an out-of-band
// corruption on a field no row revisits — e.g. a hand-corrupted tasks.status_id on
// a task with no status-changing lifecycle event — reads back unchanged and is
// falsely reported as converged), this re-derives the projection from a blank slate
// into connection-scoped SHADOW tables (§15 SHADOW DERIVATION), refolds, and compares
// the FULL projection set — owner, status, watermark, AND task_attributions — for
// every journal-anchored task. It first runs the external-schema preflight (§13). It
// is read-only and idempotent: the from-empty refold derives into throwaway shadow
// tables (which carry the projection columns but none of the real tables' constraints)
// while the real tasks / task_attributions rows stay read-only, so a converged
// database is left untouched and the check is constraint-independent (the NOT NULL
// tasks.last_journal_id tightening cannot be tripped by the refold). Its scoped
// savepoint keeps every stored read and every derived read in one WAL snapshot. On
// genuine divergence it returns a typed
// ProjectionDivergenceError naming the task, field, and stored-vs-derived values, and
// writes nothing (§13.1 six-field actionable shape).
//
// Scope during the direct-write staging window: convergence is
// asserted for journal-anchored tasks — tasks with at least one journal row
// (task_event, assignment episode, decision, or evidence) — so their owner, status,
// watermark, and attributions are all journal-reproducible (§15). A pure
// direct-write task with zero journal rows has no journal history to reduce over, so
// it is outside the checkable set until the direct-write path retires (the same
// honest-staging coupling already accepted for tasks.LastJournalID / the FK). A
// migrated task IS journal-anchored: its legacy status is captured in the migration
// marker's payload and re-derived from there, never trusted as pre-existing row
// state.
func (db *DB) ReplayProjections() (journal.ReplayResult, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.ReplayResult{}, fmt.Errorf("ReplayProjections: lease connection: %w", err)
	}
	defer scope.release()
	return scope.replayProjections()
}

// replayProjections is the scope-owned core of [DB.ReplayProjections]. Startup
// runs it on its single borrowed activation scope; runtime callers run it on a
// pooled lease. It opens its own savepoint so it composes inside an enclosing
// activation transaction.
func (scope *connScope) replayProjections() (result journal.ReplayResult, err error) {
	return scope.replayProjectionsMode(false)
}

// replayProjectionsReadOnlyLegacyCompatible is reserved for the pre-activation
// e66 compatibility pass. Activation creates and validates both context
// relations before the normal strict replay path is entered.
func (scope *connScope) replayProjectionsReadOnlyLegacyCompatible() (result journal.ReplayResult, err error) {
	return scope.replayProjectionsMode(true)
}

func (scope *connScope) replayProjectionsMode(allowLegacyFactContexts bool) (result journal.ReplayResult, err error) {
	return scope.replayProjectionsModeWithStoredSnapshotBarrier(allowLegacyFactContexts, nil)
}

// replayProjectionsModeWithStoredSnapshotBarrier keeps the production replay path
// deterministic under concurrent writers. Production passes nil; the package test
// supplies a barrier after all stored snapshots to prove a real Modernc Apply cannot
// mix a newer derivation with older stored values.
func (scope *connScope) replayProjectionsModeWithStoredSnapshotBarrier(allowLegacyFactContexts bool, afterStoredSnapshot func()) (result journal.ReplayResult, err error) {
	err = scope.runScopedSavepoint(func() error {
		if err := scope.preflightSchema(); err != nil {
			return err
		}
		if allowLegacyFactContexts {
			if err := scope.verifyFactContextIntegrityReadOnlyLegacyCompatible(); err != nil {
				return err
			}
		} else if err := scope.verifyFactContextIntegrity(); err != nil {
			return err
		}
		if err := scope.validateCanonicalOperations(); err != nil {
			return err
		}

		// Snapshot the STORED projection before any change, keyed by task id.
		storedTasks, err := scope.snapshotTaskProjections(projectionTargetLive)
		if err != nil {
			return err
		}
		storedTaskState, err := scope.snapshotCompleteTaskState(projectionTargetLive)
		if err != nil {
			return err
		}
		storedAttribs, err := scope.snapshotAttributions(projectionTargetLive)
		if err != nil {
			return err
		}
		anchored, err := scope.journalAnchoredTasks()
		if err != nil {
			return err
		}
		// The relationship/annotation domain projections are journaled in full (§6 amendment),
		// so every edge/label/comment is journal-reproducible; the stored sets are snapshotted
		// whole (not scoped to anchored tasks) and diffed against the from-empty re-derivation.
		storedDomain, err := scope.snapshotDomainProjections(projectionTargetLive)
		if err != nil {
			return err
		}
		if afterStoredSnapshot != nil {
			afterStoredSnapshot()
		}

		// Re-derive every projection from empty into connection-scoped shadow tables;
		// the real tables stay read-only (SHADOW DERIVATION, §15).
		derivedTasks, derivedTaskState, derivedAttribs, derivedDomain, folded, err := scope.rederiveProjectionsShadow()
		if err != nil {
			return err
		}

		// Convergence over the FULL projection set, scoped to journal-anchored tasks.
		if err := diffTaskProjections(storedTasks, derivedTasks, anchored); err != nil {
			return err
		}
		if err := diffCompleteTaskState(storedTaskState, derivedTaskState, anchored); err != nil {
			return err
		}
		if err := diffAttributions(storedAttribs, derivedAttribs, anchored); err != nil {
			return err
		}
		// Convergence over the journaled edge/label/comment domain projections (§6, §15).
		if err := diffDomainProjections(storedDomain, derivedDomain); err != nil {
			return err
		}

		result = journal.ReplayResult{RowsFolded: folded}
		for id := range anchored {
			if p, ok := derivedTasks[id]; ok {
				result.Tasks = append(result.Tasks, p)
			}
		}
		return nil
	})
	if err != nil {
		return journal.ReplayResult{}, err
	}
	return result, nil
}

type canonicalStoredOperation struct {
	anchor       int64
	authority    *journal.JournalID
	version      string
	wire, digest []byte
	actor        journal.ActorID
	recordedAt   int64
	versionSet   bool
	wireSet      bool
}

type canonicalColumnShape uint8

const (
	canonicalColumnsPresent canonicalColumnShape = iota + 1
	canonicalColumnsLegacy
)

func (shape canonicalColumnShape) operationsQuery() string {
	switch shape {
	case canonicalColumnsPresent:
		return "SELECT o.journal_id,o.authority_journal_id,o.mutation_encoding_version,o.canonical_mutation,o.mutation_digest,j.actor_id,j.recorded_at FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id ORDER BY o.journal_id"
	case canonicalColumnsLegacy:
		return "SELECT o.journal_id,o.authority_journal_id,?1,?2,o.mutation_digest,j.actor_id,j.recorded_at FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id ORDER BY o.journal_id"
	default:
		panic("unknown canonical column shape")
	}
}

func (shape canonicalColumnShape) effectForRowQuery() string {
	switch shape {
	case canonicalColumnsPresent:
		return "SELECT o.journal_id,o.mutation_encoding_version,o.canonical_mutation FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.journal_id=?1"
	case canonicalColumnsLegacy:
		return "SELECT o.journal_id,?2,?3 FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.journal_id=?1"
	default:
		panic("unknown canonical column shape")
	}
}

func (shape canonicalColumnShape) seedLegacyTasksQuery() string {
	switch shape {
	case canonicalColumnsPresent:
		return "INSERT INTO shadow_tasks (id,namespace,title,description,owner_id,status_id,priority_id,type_id,phase_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT t.id,t.namespace,t.title,t.description,?4,?1,t.priority_id,t.type_id,t.phase_id,t.notes,t.created_at,t.updated_at,?5,t.close_reason,?6 FROM tasks t WHERE EXISTS (SELECT ?7 FROM journal_task_events e JOIN journal j ON j.journal_id=e.journal_id JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE e.task_id=t.id AND ((e.event_kind=?2 AND o.canonical_mutation IS ?8) OR e.event_kind=?3))"
	case canonicalColumnsLegacy:
		return "INSERT INTO shadow_tasks (id,namespace,title,description,owner_id,status_id,priority_id,type_id,phase_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT t.id,t.namespace,t.title,t.description,?4,?1,t.priority_id,t.type_id,t.phase_id,t.notes,t.created_at,t.updated_at,?5,t.close_reason,?6 FROM tasks t WHERE EXISTS (SELECT ?7 FROM journal_task_events e JOIN journal j ON j.journal_id=e.journal_id JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE e.task_id=t.id AND (e.event_kind=?2 OR e.event_kind=?3))"
	default:
		panic("unknown canonical column shape")
	}
}

func (scope *connScope) validateCanonicalOperations() error {
	contextSchema, err := scope.classifyFactContextSchema()
	if err != nil {
		return err
	}
	var operations []canonicalStoredOperation
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return err
	}
	shape := canonicalColumnsPresent
	if isLegacyOperationsColumnSet(columns) {
		shape = canonicalColumnsLegacy
	}
	args := []any(nil)
	if shape == canonicalColumnsLegacy {
		args = []any{nil, nil}
	}
	if err := scope.queryRows(shape.operationsQuery(), args, func(rows *sql.Rows) error {
		var op canonicalStoredOperation
		var authority sql.NullInt64
		var version sql.NullString
		var wire []byte
		var actorRaw string
		if err := rows.Scan(&op.anchor, &authority, &version, &wire, &op.digest, &actorRaw, &op.recordedAt); err != nil {
			return err
		}
		op.digest = append([]byte(nil), op.digest...)
		if authority.Valid {
			value := journal.JournalID(authority.Int64)
			op.authority = &value
		}
		if version.Valid {
			op.versionSet = true
			op.version = version.String
		}
		if wire != nil {
			op.wireSet = true
			op.wire = append([]byte(nil), wire...)
		}
		actor, err := journalParseActor(actorRaw)
		if err != nil {
			return err
		}
		op.actor = actor
		operations = append(operations, op)
		return nil
	}); err != nil {
		return fmt.Errorf("startup canonical validation: enumerate operations: %w", err)
	}
	for _, op := range operations {
		if op.versionSet != op.wireSet {
			return canonicalCorruption(op.anchor, "encoding columns", op.version, fmt.Sprintf("%d bytes", len(op.wire)))
		}
		if !op.versionSet {
			continue
		}
		if op.version == "" || len(op.wire) == 0 {
			return canonicalCorruption(op.anchor, "encoding columns", op.version, fmt.Sprintf("%d bytes", len(op.wire)))
		}
		prepared, err := journal.DecodeCanonicalMutation(op.wire)
		if err != nil {
			return fmt.Errorf("startup canonical validation operation %d: %w", op.anchor, err)
		}
		if op.version != prepared.EncodingVersion().String() {
			return canonicalCorruption(op.anchor, "encoding version", op.version, prepared.EncodingVersion().String())
		}
		if !bytes.Equal(op.digest, prepared.DerivedDigest()) {
			return canonicalCorruption(op.anchor, "mutation digest", fmt.Sprintf("%x", op.digest), fmt.Sprintf("%x", prepared.DerivedDigest()))
		}
		effects := prepared.NormalizedEffects()
		var rows []int64
		if err := scope.queryRows("SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", []any{op.anchor}, func(sqlRows *sql.Rows) error {
			var journalID int64
			if err := sqlRows.Scan(&journalID); err != nil {
				return err
			}
			rows = append(rows, journalID)
			return nil
		}); err != nil {
			return err
		}
		if len(rows) != len(effects) {
			field := "effect row count"
			for _, effect := range effects {
				if effect.TaskID.Namespace != "" {
					field += " for task " + effect.TaskID.String()
					break
				}
			}
			return canonicalCorruption(op.anchor, field, strconv.Itoa(len(rows)), strconv.Itoa(len(effects)))
		}
		isGenesis := len(effects) == 1 && effects[0].Sort == journal.EffectBootstrapAuthority
		if isGenesis && op.authority != nil {
			return canonicalCorruption(op.anchor, "authority", fmt.Sprint(*op.authority), "NULL genesis authority")
		}
		if !isGenesis {
			if op.authority == nil {
				return canonicalCorruption(op.anchor, "authority", "NULL", "non-NULL governing authority")
			}
			if err := scope.requireAuthorityExists(*op.authority); err != nil {
				return fmt.Errorf("startup canonical operation %d authority: %w", op.anchor, err)
			}
			// A semantic self-transfer is the sole canonical history whose second
			// effect may outlive its operation authority. Reconstruct the same exact
			// shape and prove the predecessor was active immediately before effect 0;
			// ordinary bootstrap-authorized transfers continue through the generic
			// per-effect checks below.
			transferStartAuthorized := false
			if lease, transferErr := newAssignmentTransferLease(journal.OperationInput{
				AuthorityJournalID: op.authority,
				Conditions:         prepared.NormalizedConditions(),
				Effects:            effects,
			}); transferErr == nil {
				matches, err := scope.assignmentTransferAuthorityMatches(*op.authority, lease)
				if err != nil {
					return fmt.Errorf("startup canonical operation %d transfer authority: %w", op.anchor, err)
				}
				if matches {
					active, err := scope.episodeActiveAt(lease.previous, rows[0])
					if err != nil {
						return fmt.Errorf("startup canonical operation %d transfer predecessor liveness: %w", op.anchor, err)
					}
					if !active {
						return canonicalCorruption(op.anchor, "assignment transfer predecessor liveness", "inactive before predecessor end", "active immediately before effect 0")
					}
					transferStartAuthorized = true
				}
			}
			in := journal.OperationInput{ActorID: op.actor, AuthorityJournalID: op.authority}
			for i, effect := range effects {
				if transferStartAuthorized && i == 1 {
					continue
				}
				if effect.TaskID.Namespace != "" {
					if err := scope.requireAuthorityGoverns(in, rows[i], effect.TaskID); err != nil {
						return fmt.Errorf("startup canonical operation %d authority for effect %d: %w", op.anchor, i, err)
					}
				}
			}
		}
		if err := scope.validateCanonicalResultSlots(op.anchor, rows, effects); err != nil {
			return err
		}
		for i, effect := range effects {
			if err := scope.validateCanonicalEffectRow(op, rows[i], effect, contextSchema == factContextSchemaCanonical); err != nil {
				return fmt.Errorf("operation %d effect %d: %w", op.anchor, i, err)
			}
		}
	}
	return nil
}

func (scope *connScope) validateCanonicalResultSlots(anchor int64, rows []int64, effects []journal.Effect) error {
	expected := map[string]int64{}
	for i, effect := range effects {
		if effect.ResultSlot != "" {
			slot := string(effect.ResultSlot)
			if _, ok := expected[slot]; ok {
				return canonicalCorruption(anchor, "duplicate canonical result slot", slot, "unique")
			}
			expected[slot] = rows[i]
		}
	}
	actual := map[string]int64{}
	if err := scope.queryRows("SELECT result_slot_id,produced_journal_id FROM journal_operation_result_slots WHERE journal_id=?1", []any{anchor}, func(rows *sql.Rows) error {
		var slot string
		var journalID int64
		if err := rows.Scan(&slot, &journalID); err != nil {
			return err
		}
		actual[slot] = journalID
		return nil
	}); err != nil {
		return err
	}
	for slot, want := range expected {
		got, ok := actual[slot]
		if !ok {
			return canonicalCorruption(anchor, "result slot "+slot, "missing", strconv.FormatInt(want, 10))
		}
		if got != want {
			return canonicalCorruption(anchor, "result slot "+slot, strconv.FormatInt(got, 10), strconv.FormatInt(want, 10))
		}
	}
	for slot, got := range actual {
		if _, ok := expected[slot]; !ok {
			return canonicalCorruption(anchor, "result slot "+slot, strconv.FormatInt(got, 10), "absent")
		}
	}
	return nil
}

func canonicalCorruption(anchor int64, field, stored, canonical string) error {
	return fmt.Errorf("%w: provenance: canonical operation %d field %s diverged — stored=%q canonical=%q; where: startup canonical validation; when: before accepting the database; impact: Open fails closed and no projection row is mutated; fix: restore the operation, canonical bytes, and subtype rows from the same committed backup", journal.ErrProjectionDivergence, anchor, field, stored, canonical)
}

func (scope *connScope) validateCanonicalEffectRow(op canonicalStoredOperation, jid int64, effect journal.Effect, validateFactContexts bool) error {
	expectedKind, err := effect.Sort.JournalKind()
	if err != nil {
		return err
	}
	var kind int
	var recordedAt int64
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT kind_id, recorded_at FROM journal WHERE journal_id=?1", jid).Scan(&kind, &recordedAt); err != nil {
		return err
	}
	if kind != int(expectedKind) {
		return canonicalCorruption(op.anchor, fmt.Sprintf("row %d kind", jid), strconv.Itoa(kind), expectedKind.String())
	}
	expectedRecordedAt := op.recordedAt
	if effect.RecordedAtOverride != nil {
		expectedRecordedAt = *effect.RecordedAtOverride
	}
	if recordedAt != expectedRecordedAt {
		return canonicalCorruption(op.anchor, fmt.Sprintf("row %d recorded_at", jid), strconv.FormatInt(recordedAt, 10), strconv.FormatInt(expectedRecordedAt, 10))
	}
	switch effect.Sort {
	case journal.EffectTaskCreate, journal.EffectTaskCreateAllocated, journal.EffectTaskEvent, journal.EffectEdgeAdd, journal.EffectEdgeRemove, journal.EffectLabelAdd, journal.EffectLabelRemove, journal.EffectCommentAdd:
		return scope.validateCanonicalTaskEvent(op.anchor, jid, effect)
	case journal.EffectBootstrapAuthority:
		label := effect.BootstrapLabel
		if label == "" {
			label = "bootstrap"
		}
		auth := string(effect.OperationAuthorityID)
		if auth == "" {
			auth = fmt.Sprintf("authority--bootstrap--%d", jid)
		}
		return scope.compareSingleRow(op.anchor, jid, canonicalBootstrapRow, []string{auth, label})
	case journal.EffectAssignmentStart:
		occupant := effect.Occupant
		if occupant.Namespace == "" {
			occupant = op.actor
		}
		slot, _ := slotDBID(effect.SlotID)
		return scope.compareSingleRow(op.anchor, jid, canonicalAssignmentStartRow, []string{string(effect.AssignmentID), effect.TaskID.String(), strconv.Itoa(slot), occupant.String(), string(effect.Predecessor), string(effect.Parent), strconv.Itoa(transitionStartedID)})
	case journal.EffectAssignmentEnd:
		return scope.compareSingleRow(op.anchor, jid, canonicalAssignmentEndRow, []string{string(effect.AssignmentID), strconv.Itoa(transitionEndedID)})
	case journal.EffectDecision:
		payload := effect.Payload
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}
		if err := scope.compareSingleRow(op.anchor, jid, canonicalDecisionRow, []string{string(effect.DecisionKind), optionalTaskString(effect.TaskID), string(payload)}); err != nil {
			return err
		}
		if validateFactContexts {
			return scope.validateCanonicalFactContextSet(op.anchor, jid, factContextDecision, effect.Contexts)
		}
		return nil
	case journal.EffectEvidence:
		payload := effect.Payload
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}
		if err := scope.compareSingleRow(op.anchor, jid, canonicalEvidenceRow, []string{string(effect.EvidenceKind), optionalTaskString(effect.TaskID), strings.ToUpper(fmt.Sprintf("%x", effect.ContentDigest)), string(payload)}); err != nil {
			return err
		}
		if validateFactContexts {
			return scope.validateCanonicalFactContextSet(op.anchor, jid, factContextEvidence, effect.Contexts)
		}
		return nil
	}
	return nil
}

func optionalTaskString(id journal.TaskID) string {
	if id.Namespace == "" {
		return ""
	}
	return id.String()
}

func (scope *connScope) validateCanonicalTaskEvent(anchor, jid int64, effect journal.Effect) error {
	kind := effect.EventKind
	payload := effect.Payload
	if effect.Sort == journal.EffectTaskCreate || effect.Sort == journal.EffectTaskCreateAllocated {
		kind = journal.EventKindTaskCreated
	}
	if effect.Sort == journal.EffectTaskEvent && effect.Forced && journal.IsTransitionLifecycleKind(kind) {
		payload = journal.EncodeForcedTransitionPayload()
	}
	if journalKind, ok := journal.MutationFamilyKindForSort(effect.Sort); ok {
		kind = journalKind
		var err error
		payload, err = encodeMutationFamilyPayload(effect)
		if err != nil {
			return err
		}
	}
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := scope.compareSingleRow(anchor, jid, canonicalTaskEventRow, []string{effect.TaskID.String(), string(kind), string(payload)}); err != nil {
		return err
	}
	expected, err := journal.CanonicalEventContexts(effect.Contexts)
	if err != nil {
		return err
	}
	actual, err := scope.loadContexts(jid)
	if err != nil {
		return err
	}
	if len(expected) != len(actual) {
		return canonicalCorruption(anchor, fmt.Sprintf("row %d contexts", jid), strconv.Itoa(len(actual)), strconv.Itoa(len(expected)))
	}
	for i := range expected {
		ek, ei, _ := journal.EncodeStoredEventContext(expected[i])
		ak, ai, _ := journal.EncodeStoredEventContext(actual[i])
		if ek != ak || ei != ai {
			return canonicalCorruption(anchor, fmt.Sprintf("row %d context %d", jid, i), string(ak)+":"+ai, string(ek)+":"+ei)
		}
	}
	attached := []int64{}
	if err := scope.queryRows("SELECT attached_by_journal_id FROM journal_task_event_contexts WHERE event_journal_id=?1 ORDER BY context_kind,context_identity", []any{jid}, func(rows *sql.Rows) error {
		var attachedBy int64
		if err := rows.Scan(&attachedBy); err != nil {
			return err
		}
		attached = append(attached, attachedBy)
		return nil
	}); err != nil {
		return err
	}
	for i, got := range attached {
		if got != jid {
			return canonicalCorruption(anchor, fmt.Sprintf("row %d context %d attached_by_journal_id", jid, i), strconv.FormatInt(got, 10), strconv.FormatInt(jid, 10))
		}
	}
	return nil
}

type canonicalSubtypeQuery uint8

const (
	canonicalBootstrapRow canonicalSubtypeQuery = iota + 1
	canonicalAssignmentStartRow
	canonicalAssignmentEndRow
	canonicalDecisionRow
	canonicalEvidenceRow
	canonicalTaskEventRow
)

func (query canonicalSubtypeQuery) sql() string {
	switch query {
	case canonicalBootstrapRow:
		return "SELECT a.operation_authority_id, b.label FROM journal_authorities a JOIN journal_authority_bootstraps b ON b.journal_id=a.journal_id WHERE a.journal_id=?1"
	case canonicalAssignmentStartRow:
		return "SELECT e.assignment_id,e.task_id,e.slot_id,e.actor_id,e.predecessor_assignment_id,e.parent_assignment_id,t.transition_id FROM journal_authority_assignment_transitions t JOIN journal_authority_assignment_episodes e ON e.assignment_id=t.assignment_id WHERE t.journal_id=?1"
	case canonicalAssignmentEndRow:
		return "SELECT assignment_id,transition_id FROM journal_authority_assignment_transitions WHERE journal_id=?1"
	case canonicalDecisionRow:
		return "SELECT decision_kind,task_id,payload FROM journal_decisions WHERE journal_id=?1"
	case canonicalEvidenceRow:
		return "SELECT evidence_kind,task_id,hex(content_digest),payload FROM journal_evidence WHERE journal_id=?1"
	case canonicalTaskEventRow:
		return "SELECT task_id,event_kind,payload FROM journal_task_events WHERE journal_id=?1"
	default:
		panic("unknown canonical subtype query")
	}
}

func (scope *connScope) compareSingleRow(anchor, jid int64, query canonicalSubtypeQuery, expected []string) error {
	found := false
	var actual []string
	if err := scope.queryRows(query.sql(), []any{jid}, func(rows *sql.Rows) error {
		values := make([]any, len(expected))
		pointers := make([]any, len(expected))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		found = true
		for _, value := range values {
			switch value := value.(type) {
			case nil:
				actual = append(actual, "")
			case []byte:
				actual = append(actual, string(value))
			default:
				actual = append(actual, fmt.Sprint(value))
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return canonicalCorruption(anchor, fmt.Sprintf("row %d subtype", jid), "missing", "present")
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return canonicalCorruption(anchor, fmt.Sprintf("row %d subtype column %d", jid, i), actual[i], expected[i])
		}
	}
	return nil
}

// rederiveProjectionsShadow re-derives every projection from an empty slate
// into connection-scoped shadow tables and returns the derived projection and
// attribution snapshots for comparison, WITHOUT mutating the real tables (§15 SHADOW
// DERIVATION). It creates empty-slate shadow tables (one row per real task id, with
// blank projection columns and NO real-table constraints), repoints the shared
// reducer's projection-write target at them, folds the whole journal in JournalID
// order through the same projectJournalRow step Apply uses (§9.2), then captures
// the from-empty snapshots from the shadow tables. The real tasks / task_attributions
// rows are read-only throughout, so the check is constraint-independent (the NOT NULL
// tasks.last_journal_id tightening is never tripped) and needs no savepoint/rollback.
// The projection-write target and the shadow tables are always restored/dropped
// before return, on every path.
func (scope *connScope) rederiveProjectionsShadow() (
	tasks map[string]journal.TaskProjection,
	taskState map[string]completeTaskState,
	attribs map[string]map[string]int64,
	domain domainProjection,
	folded int,
	err error,
) {
	if err = scope.createProjectionShadow(); err != nil {
		return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: stage shadow projection tables: %w", err)
	}
	// Repoint the shared reducer's projection-write steps at the shadow tables, and
	// unconditionally restore the real targets + drop the shadow tables on return. The
	// relationship/annotation domain projections (§6 amendment) are repointed alongside
	// the task/attribution projections so their from-empty refold lands in the shadow
	// tables too.
	scope.projectionTarget = projectionTargetShadow
	defer func() {
		scope.projectionTarget = projectionTargetLive
		if derr := scope.dropProjectionShadow(); derr != nil && err == nil {
			err = fmt.Errorf("ReplayProjections: drop shadow projection tables: %w", derr)
		}
	}()

	var order []int64
	if err = scope.queryRows("SELECT journal_id FROM journal ORDER BY journal_id ASC", nil, func(rows *sql.Rows) error {
		var journalID int64
		if err := rows.Scan(&journalID); err != nil {
			return err
		}
		order = append(order, journalID)
		return nil
	}); err != nil {
		return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: enumerate journal: %w", err)
	}
	for _, jid := range order {
		if err = scope.projectJournalRow(jid); err != nil {
			return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: fold row %d: %w", jid, err)
		}
		folded++
	}

	tasks, err = scope.snapshotTaskProjections(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	taskState, err = scope.snapshotCompleteTaskState(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	attribs, err = scope.snapshotAttributions(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	domain, err = scope.snapshotDomainProjections(projectionTargetShadow)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	return tasks, taskState, attribs, domain, folded, nil
}

// createProjectionShadow builds the connection-scoped empty-slate shadow
// projection tables the from-empty refold derives into (§15). The shadow tasks table
// is seeded with one blank-projection row per real task id (owner NULL, status open,
// closed_at NULL, watermark NULL) so a lifecycle/watermark UPDATE folded during the
// refold has a row to hit — exactly the empty-slate the retired clear-in-place scratch
// produced, but on a throwaway table with NO constraints (no NOT NULL watermark, no
// foreign keys), so the real tables are never touched. The shadow attribution table
// starts empty. The journal-spine subtype tables the reducer READS are untouched.
func (scope *connScope) createProjectionShadow() error {
	// Drop first in case a prior aborted run left them on the connection.
	if err := scope.dropProjectionShadow(); err != nil {
		return err
	}
	ddl := []string{
		"CREATE TEMP TABLE shadow_tasks (id TEXT PRIMARY KEY, namespace TEXT, title TEXT, description TEXT, owner_id TEXT, status_id INTEGER, priority_id INTEGER, type_id INTEGER, phase_id INTEGER, notes TEXT, created_at INTEGER, updated_at INTEGER, closed_at INTEGER, close_reason TEXT, last_journal_id INTEGER)",
		"CREATE TEMP TABLE shadow_task_attributions (task_id TEXT NOT NULL, actor_id TEXT NOT NULL, first_journal_id INTEGER NOT NULL, PRIMARY KEY (task_id, actor_id))",
		"CREATE TEMP TABLE shadow_edges (source_id TEXT NOT NULL, target_id TEXT NOT NULL, kind_id INTEGER NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (source_id, target_id, kind_id))",
		"CREATE TEMP TABLE shadow_labels (task_id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY (task_id, name))",
		"CREATE TEMP TABLE shadow_comments (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, author_id TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL)",
	}
	for _, stmt := range ddl {
		if _, err := scope.conn.ExecContext(scope.ctx, stmt); err != nil {
			return fmt.Errorf("create shadow projection table: %w", err)
		}
	}
	// Only opaque legacy task births are seeded from their materialized row. New
	// canonical births must be reconstructed from canonical effects, making missing
	// and spurious task rows observable while retaining mixed legacy/new databases.
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return err
	}
	shape := canonicalColumnsPresent
	if isLegacyOperationsColumnSet(columns) {
		shape = canonicalColumnsLegacy
	}
	args := []any{statusOpenID, string(journal.EventKindTaskCreated), string(journal.EventKindTaskMigrated), nil, nil, nil, 1}
	if shape == canonicalColumnsPresent {
		args = append(args, nil)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, shape.seedLegacyTasksQuery(), args...); err != nil {
		return fmt.Errorf("seed shadow_tasks legacy slate: %w", err)
	}
	return nil
}

func (scope *connScope) canonicalEffectForJournalRow(jid int64) (journal.Effect, bool, error) {
	var version string
	var wire []byte
	var anchor int64
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return journal.Effect{}, false, err
	}
	shape := canonicalColumnsPresent
	if isLegacyOperationsColumnSet(columns) {
		shape = canonicalColumnsLegacy
	}
	args := []any{jid}
	if shape == canonicalColumnsLegacy {
		args = append(args, nil, nil)
	}
	var storedVersion sql.NullString
	var storedWire []byte
	err = scope.conn.QueryRowContext(scope.ctx, shape.effectForRowQuery(), args...).Scan(&anchor, &storedVersion, &storedWire)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return journal.Effect{}, false, nil
		}
		return journal.Effect{}, false, fmt.Errorf("load canonical mutation for row %d: %w", jid, err)
	}
	if storedVersion.Valid {
		version = storedVersion.String
	}
	if storedWire != nil {
		wire = append([]byte(nil), storedWire...)
	}
	if (version == "") != (len(wire) == 0) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d has mixed canonical encoding facts (version=%q bytes=%d) — where: startup canonical validation; impact: database open fails without mutation; fix: restore both canonical columns together, or NULL both only for a genuine legacy operation", anchor, version, len(wire))
	}
	if version == "" {
		return journal.Effect{}, false, nil
	}
	prepared, err := journal.DecodeCanonicalMutation(wire)
	if err != nil {
		return journal.Effect{}, false, fmt.Errorf("operation %d canonical mutation: %w", anchor, err)
	}
	if prepared.EncodingVersion().String() != version {
		return journal.Effect{}, false, fmt.Errorf("operation %d canonical column version %q differs from registered wire version %q", anchor, version, prepared.EncodingVersion())
	}
	ordinal := 0
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id<=?2", anchor, jid).Scan(&ordinal); err != nil {
		return journal.Effect{}, false, fmt.Errorf("resolve canonical effect ordinal for row %d: %w", jid, err)
	}
	ordinal--
	effects := prepared.NormalizedEffects()
	if ordinal < 0 || ordinal >= len(effects) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d row %d has effect ordinal %d outside canonical effect count %d", anchor, jid, ordinal, len(effects))
	}
	return effects[ordinal], true, nil
}

func (scope *connScope) insertCanonicalShadowTask(e journal.Effect, recordedAt, jid int64) error {
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO shadow_tasks\n\t\t (id, namespace, title, description, owner_id, status_id, priority_id, type_id,\n\t\t  phase_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?12, ?5, ?6, ?7, ?8, ?9, ?10, ?10, ?13, ?9, ?11)", e.TaskID.String(), e.TaskID.Namespace, e.Title, e.Description,
		statusOpenID, int(e.Priority), int(e.Type), int(e.Phase), "", recordedAt, jid, nil, nil); err != nil {
		return fmt.Errorf("replay canonical task create %q: %w", e.TaskID, err)
	}
	return nil
}

func (scope *connScope) materializeCanonicalShadowTaskEvent(e journal.Effect, recordedAt int64) error {
	closeReasonSet := e.EventKind == journal.EventKindTaskClosed && e.CloseReason != ""
	if e.UpdateTitle == nil && e.UpdateDescription == nil && e.UpdatePriority == nil && e.UpdatePhase == nil && e.UpdateNotes == nil && !closeReasonSet {
		return nil
	}
	value := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	flag := func(set bool) int {
		if set {
			return 1
		}
		return 0
	}
	var priority, phase any
	if e.UpdatePriority != nil {
		priority = int(*e.UpdatePriority)
	}
	if e.UpdatePhase != nil {
		phase = int(*e.UpdatePhase)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE shadow_tasks SET\n\t\tupdated_at=?1,\n\t\ttitle=CASE WHEN ?2 THEN ?3 ELSE title END,\n\t\tdescription=CASE WHEN ?4 THEN ?5 ELSE description END,\n\t\tpriority_id=CASE WHEN ?6 THEN ?7 ELSE priority_id END,\n\t\tphase_id=CASE WHEN ?8 THEN ?9 ELSE phase_id END,\n\t\tnotes=CASE WHEN ?10 THEN ?11 ELSE notes END,\n\t\tclose_reason=CASE WHEN ?12 THEN ?13 ELSE close_reason END\n\t\tWHERE id=?14",
		recordedAt,
		flag(e.UpdateTitle != nil), value(e.UpdateTitle),
		flag(e.UpdateDescription != nil), value(e.UpdateDescription),
		flag(e.UpdatePriority != nil), priority,
		flag(e.UpdatePhase != nil), phase,
		flag(e.UpdateNotes != nil), value(e.UpdateNotes),
		flag(closeReasonSet), e.CloseReason,
		e.TaskID.String(),
	); err != nil {
		return fmt.Errorf("replay canonical task event %q: %w", e.TaskID, err)
	}
	return nil
}

// dropProjectionShadow removes the connection-scoped shadow projection tables.
func (scope *connScope) dropProjectionShadow() error {
	for _, stmt := range []string{"DROP TABLE IF EXISTS shadow_tasks", "DROP TABLE IF EXISTS shadow_task_attributions", "DROP TABLE IF EXISTS shadow_edges", "DROP TABLE IF EXISTS shadow_labels", "DROP TABLE IF EXISTS shadow_comments"} {
		if _, err := scope.conn.ExecContext(scope.ctx, stmt); err != nil {
			return fmt.Errorf("drop shadow projection table: %w", err)
		}
	}
	return nil
}

// journalAnchoredTasks returns the set of task ids referenced by at least one
// journal-spine subtype row (task_event, assignment episode, task-scoped decision
// or evidence). These are the tasks whose projections are journal-reproducible and
// therefore in scope for the §15 convergence assertion.
func (scope *connScope) journalAnchoredTasks() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if err := scope.queryRows("SELECT task_id FROM journal_task_events\n\t\t UNION SELECT task_id FROM journal_authority_assignment_episodes\n\t\t UNION SELECT task_id FROM journal_decisions WHERE task_id IS NOT ?1\n\t\t UNION SELECT task_id FROM journal_evidence WHERE task_id IS NOT ?2", []any{nil, nil}, func(rows *sql.Rows) error {
		var task sql.NullString
		if err := rows.Scan(&task); err != nil {
			return err
		}
		if task.Valid {
			out[task.String] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("enumerate journal-anchored tasks: %w", err)
	}
	return out, nil
}

// snapshotTaskProjections reads every task's current projection (owner,
// status, watermark) keyed by task id from the named table, so a replay can compare
// the stored projection ("tasks") against the from-empty re-derivation
// ("shadow_tasks") (§15).
func (scope *connScope) snapshotTaskProjections(target projectionTarget) (map[string]journal.TaskProjection, error) {
	out := map[string]journal.TaskProjection{}
	if err := scope.queryRows(target.snapshotTaskProjectionsQuery(), nil, func(rows *sql.Rows) error {
		var taskRaw string
		var owner sql.NullString
		var status int
		var lastJournalID sql.NullInt64
		if err := rows.Scan(&taskRaw, &owner, &status, &lastJournalID); err != nil {
			return err
		}
		task, err := journalParseTask(taskRaw)
		if err != nil {
			return err
		}
		p := journal.TaskProjection{TaskID: task, Status: journal.TaskStatus(status)}
		if owner.Valid {
			ownerID, err := journalParseActor(owner.String)
			if err != nil {
				return err
			}
			p.Owner = &ownerID
		}
		if lastJournalID.Valid {
			p.LastJournalID = journal.JournalID(lastJournalID.Int64)
		}
		out[taskRaw] = p
		return nil
	}); err != nil {
		return nil, fmt.Errorf("snapshot task projections: %w", err)
	}
	return out, nil
}

func (target projectionTarget) snapshotTaskProjectionsQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT id,owner_id,status_id,last_journal_id FROM tasks"
	case projectionTargetShadow:
		return "SELECT id,owner_id,status_id,last_journal_id FROM shadow_tasks"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) snapshotAttributionsQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT task_id,actor_id,first_journal_id FROM task_attributions"
	case projectionTargetShadow:
		return "SELECT task_id,actor_id,first_journal_id FROM shadow_task_attributions"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) snapshotCompleteTaskStateQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks"
	case projectionTargetShadow:
		return "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM shadow_tasks"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) snapshotEdgesQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT source_id,target_id,kind_id,created_at FROM edges"
	case projectionTargetShadow:
		return "SELECT source_id,target_id,kind_id,created_at FROM shadow_edges"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) snapshotLabelsQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT task_id,name FROM labels"
	case projectionTargetShadow:
		return "SELECT task_id,name FROM shadow_labels"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) snapshotCommentsQuery() string {
	switch target {
	case projectionTargetLive:
		return "SELECT id,task_id,author_id,body,created_at FROM comments"
	case projectionTargetShadow:
		return "SELECT id,task_id,author_id,body,created_at FROM shadow_comments"
	default:
		panic("unknown projection target")
	}
}

// snapshotAttributions reads the attribution projection from the named table
// as a nested map task_id → (actor_id → first_journal_id), so a replay can compare
// the stored edges ("task_attributions") against the from-empty re-derivation
// ("shadow_task_attributions") (§8.2, §15).
func (scope *connScope) snapshotAttributions(target projectionTarget) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if err := scope.queryRows(target.snapshotAttributionsQuery(), nil, func(rows *sql.Rows) error {
		var task, actor string
		var journalID int64
		if err := rows.Scan(&task, &actor, &journalID); err != nil {
			return err
		}
		if out[task] == nil {
			out[task] = map[string]int64{}
		}
		out[task][actor] = journalID
		return nil
	}); err != nil {
		return nil, fmt.Errorf("snapshot task_attributions: %w", err)
	}
	return out, nil
}

// diffTaskProjections asserts the stored projection equals the from-empty
// re-derivation for every journal-anchored task, across owner, status, and
// watermark. A stored value present on a fold-unvisited field (an out-of-band
// corruption) diverges here because the derived value is the reducer's ground truth
// from an empty slate, not the pre-existing row.
func diffTaskProjections(stored, derived map[string]journal.TaskProjection, anchored map[string]struct{}) error {
	for id := range anchored {
		s := stored[id]
		d := derived[id]
		if ownerString(s.Owner) != ownerString(d.Owner) {
			return divergence(d.TaskID, "owner", ownerString(s.Owner), ownerString(d.Owner))
		}
		if s.Status != d.Status {
			return divergence(d.TaskID, "status", s.Status.String(), d.Status.String())
		}
		if s.LastJournalID != d.LastJournalID {
			return divergence(d.TaskID, "watermark",
				fmt.Sprintf("%d", int64(s.LastJournalID)), fmt.Sprintf("%d", int64(d.LastJournalID)))
		}
	}
	return nil
}

// diffAttributions asserts the stored task_attributions edges equal the from-empty
// re-derivation for every journal-anchored task: a spurious stored edge, a missing
// edge, or a wrong first_journal_id all diverge (§8.2, §15).
func diffAttributions(stored, derived map[string]map[string]int64, anchored map[string]struct{}) error {
	for id := range anchored {
		task, err := journalParseTask(id)
		if err != nil {
			return err
		}
		s := stored[id]
		d := derived[id]
		// Every stored edge must be reproduced by the from-empty fold.
		for actor, sJID := range s {
			dJID, ok := d[actor]
			if !ok {
				return divergence(task, "attribution",
					fmt.Sprintf("actor %s attributed at %d", actor, sJID),
					fmt.Sprintf("actor %s not attributed by the from-empty fold", actor))
			}
			if dJID != sJID {
				return divergence(task, "attribution",
					fmt.Sprintf("actor %s first_journal_id %d", actor, sJID),
					fmt.Sprintf("actor %s first_journal_id %d", actor, dJID))
			}
		}
		// The from-empty fold must not derive an edge the stored projection lacks.
		for actor, dJID := range d {
			if _, ok := s[actor]; !ok {
				return divergence(task, "attribution",
					fmt.Sprintf("actor %s not attributed in the stored projection", actor),
					fmt.Sprintf("actor %s attributed at %d by the from-empty fold", actor, dJID))
			}
		}
	}
	return nil
}

func divergence(task journal.TaskID, field, stored, derived string) error {
	return &journal.ProjectionDivergenceError{
		Operation: "ReplayProjections",
		Task:      task,
		Field:     field,
		Stored:    stored,
		Replayed:  derived,
		Why:       "the stored projection does not equal the projection derived by folding ordered journal history from an empty slate",
		Impact:    "the database is not accepted as converged; no further projection write is applied",
		Fix:       "rebuild projections from the journal, or investigate the out-of-band write that corrupted the stored projection (§8.1: owner/status/attribution are reducer-exclusive)",
	}
}

func ownerString(o *journal.ActorID) string {
	if o == nil {
		return ""
	}
	return o.String()
}

type completeTaskState struct {
	task   journal.TaskID
	fields map[string]string
}

func (scope *connScope) snapshotCompleteTaskState(target projectionTarget) (map[string]completeTaskState, error) {
	out := map[string]completeTaskState{}
	if err := scope.queryRows(target.snapshotCompleteTaskStateQuery(), nil, func(rows *sql.Rows) error {
		var idRaw string
		var namespace, title, description, notes, closeReason sql.NullString
		var status, priority, taskType, phase, createdAt, updatedAt sql.NullInt64
		var owner sql.NullString
		var closedAt, watermark sql.NullInt64
		if err := rows.Scan(&idRaw, &namespace, &title, &description, &status, &priority, &taskType, &phase, &owner, &notes, &createdAt, &updatedAt, &closedAt, &closeReason, &watermark); err != nil {
			return err
		}
		id, err := journalParseTask(idRaw)
		if err != nil {
			return err
		}
		text := func(value sql.NullString) string {
			if !value.Valid {
				return "<null>"
			}
			return value.String
		}
		integer := func(value sql.NullInt64) string {
			if !value.Valid {
				return "<null>"
			}
			return strconv.FormatInt(value.Int64, 10)
		}
		out[id.String()] = completeTaskState{task: id, fields: map[string]string{
			"namespace": text(namespace), "title": text(title), "description": text(description),
			"status": integer(status), "priority": integer(priority), "type": integer(taskType),
			"phase": integer(phase), "owner": text(owner), "notes": text(notes),
			"created_at": integer(createdAt), "updated_at": integer(updatedAt), "closed_at": integer(closedAt),
			"close_reason": text(closeReason), "watermark": integer(watermark),
		}}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("snapshot complete task state from %s: %w", target.label(), err)
	}
	return out, nil
}

func diffCompleteTaskState(stored, derived map[string]completeTaskState, anchored map[string]struct{}) error {
	fields := []string{"namespace", "title", "description", "status", "priority", "type", "phase", "owner", "notes", "created_at", "updated_at", "closed_at", "close_reason", "watermark"}
	for id := range anchored {
		s, storedOK := stored[id]
		d, derivedOK := derived[id]
		task := d.task
		if !derivedOK {
			task = s.task
		}
		if !storedOK {
			return divergence(task, "task row", "missing", "present in canonical replay")
		}
		if !derivedOK {
			return divergence(task, "task row", "present", "missing from canonical replay")
		}
		for _, field := range fields {
			if s.fields[field] != d.fields[field] {
				return divergence(task, field, s.fields[field], d.fields[field])
			}
		}
	}
	for id, s := range stored {
		if _, ok := derived[id]; !ok {
			if s.fields["watermark"] == "<null>" {
				continue // explicit pre-journal legacy row awaiting baseline migration
			}
			return divergence(s.task, "task row", "spurious stored row", "absent from journal replay")
		}
	}
	for id, d := range derived {
		if _, ok := stored[id]; !ok {
			return divergence(d.task, "task row", "missing stored row", "present in journal replay")
		}
	}
	return nil
}

// domainProjection is a from-a-slate snapshot of the journaled relationship/annotation
// domain projections (§6 amendment): edges keyed by (source, target, kind), labels by
// (task, name), comments by id. Each family maps its natural key to the FULL projected
// tuple — the key AND every non-key content column — so the §15 convergence check compares
// the COMPLETE row against the from-empty re-derivation, matching the full-tuple discipline
// of diffTaskProjections/diffAttributions. A key-only comparison would let a comment's
// task_id/author_id/body/created_at (or an edge's created_at) drift out-of-band undetected
// — the exact corruption class §15's from-empty SHADOW derivation exists to catch; carrying
// the whole tuple here closes that hole. Because every edge/label/comment now flows through
// the journal, these sets are compared WHOLE (not scoped to anchored tasks): a from-empty
// refold reproduces exactly the journaled rows.
type domainProjection struct {
	edges    map[string]domainEdge
	labels   map[string]domainLabel
	comments map[string]domainComment
}

// domainEdge is one projected edge keyed by (source, target, kind); created_at is its only
// non-key content column. task names the reporting (source) task for a divergence message.
type domainEdge struct {
	task      journal.TaskID
	createdAt int64
}

// domainLabel is one projected label keyed by (task, name). Labels are KEY-ONLY BY NATURE:
// (task_id, name) are the projection's only columns, so key presence already IS the full
// tuple and there is no non-key content column to diverge — hence domainLabel carries only
// the task it names.
type domainLabel struct {
	task journal.TaskID
}

// domainComment is one projected comment keyed by its id; task_id, author_id, body, and
// created_at are ALL non-key content a full-tuple compare must verify (the comment PK is id
// alone, so its task_id is not part of the key).
type domainComment struct {
	task      journal.TaskID
	author    string
	body      string
	createdAt int64
}

// snapshotDomainProjections reads the FULL edge/label/comment tuples from the named
// tables (the real tables, or the shadow tables during a from-empty derivation) so a replay
// can diff the stored domain projection against the from-empty re-derivation across every
// column, not merely the keys (§15). It selects the non-key content columns
// (edges.created_at; comments.author_id/body/created_at) alongside the keys; labels have no
// non-key column.
func (scope *connScope) snapshotDomainProjections(target projectionTarget) (domainProjection, error) {
	dp := domainProjection{
		edges:    map[string]domainEdge{},
		labels:   map[string]domainLabel{},
		comments: map[string]domainComment{},
	}
	if err := scope.queryRows(target.snapshotEdgesQuery(), nil, func(rows *sql.Rows) error {
		var src, tgt string
		var kind int
		var createdAt int64
		if err := rows.Scan(&src, &tgt, &kind, &createdAt); err != nil {
			return err
		}
		task, err := journalParseTask(src)
		if err != nil {
			return err
		}
		dp.edges[fmt.Sprintf("%s\x00%s\x00%d", src, tgt, kind)] = domainEdge{task: task, createdAt: createdAt}
		return nil
	}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot %s edges: %w", target.label(), err)
	}
	if err := scope.queryRows(target.snapshotLabelsQuery(), nil, func(rows *sql.Rows) error {
		var taskRaw, name string
		if err := rows.Scan(&taskRaw, &name); err != nil {
			return err
		}
		task, err := journalParseTask(taskRaw)
		if err != nil {
			return err
		}
		dp.labels[fmt.Sprintf("%s\x00%s", taskRaw, name)] = domainLabel{task: task}
		return nil
	}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot %s labels: %w", target.label(), err)
	}
	if err := scope.queryRows(target.snapshotCommentsQuery(), nil, func(rows *sql.Rows) error {
		var id, taskRaw, author, body string
		var createdAt int64
		if err := rows.Scan(&id, &taskRaw, &author, &body, &createdAt); err != nil {
			return err
		}
		task, err := journalParseTask(taskRaw)
		if err != nil {
			return err
		}
		dp.comments[id] = domainComment{
			task:      task,
			author:    author,
			body:      body,
			createdAt: createdAt,
		}
		return nil
	}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot %s comments: %w", target.label(), err)
	}
	return dp, nil
}

// diffDomainProjections asserts the stored edge/label/comment projections equal the
// from-empty re-derivation ACROSS THE FULL TUPLE (§6, §15): a stored row the fold does not
// reproduce, a folded row the stored projection lacks, OR a row present in both whose
// non-key content differs (an edge's created_at; a comment's task/author/body/created_at),
// all fail closed with a typed ProjectionDivergenceError naming the divergent field. This
// mirrors diffTaskProjections/diffAttributions, closing the key-only hole that let a
// comment's body/author/task drift out-of-band undetected.
func diffDomainProjections(stored, derived domainProjection) error {
	if err := diffDomainEdges(stored.edges, derived.edges); err != nil {
		return err
	}
	if err := diffDomainLabels(stored.labels, derived.labels); err != nil {
		return err
	}
	return diffDomainComments(stored.comments, derived.comments)
}

// diffDomainEdges compares the full edge tuple: key presence in both directions, then the
// created_at content column for a key present in both.
func diffDomainEdges(stored, derived map[string]domainEdge) error {
	for key, s := range stored {
		d, ok := derived[key]
		if !ok {
			return divergence(s.task, "edge",
				fmt.Sprintf("edge %q present in the stored projection", key),
				"absent from the from-empty fold")
		}
		if s.createdAt != d.createdAt {
			return divergence(d.task, "edge created_at",
				fmt.Sprintf("edge %q created_at %d", key, s.createdAt),
				fmt.Sprintf("created_at %d", d.createdAt))
		}
	}
	for key, d := range derived {
		if _, ok := stored[key]; !ok {
			return divergence(d.task, "edge",
				fmt.Sprintf("edge %q absent from the stored projection", key),
				"derived by the from-empty fold")
		}
	}
	return nil
}

// diffDomainLabels compares labels by key presence only: (task_id, name) are the
// projection's ONLY columns, so key presence IS the full tuple — there is no content column
// to diverge (documented on domainLabel).
func diffDomainLabels(stored, derived map[string]domainLabel) error {
	for key, s := range stored {
		if _, ok := derived[key]; !ok {
			return divergence(s.task, "label",
				fmt.Sprintf("label %q present in the stored projection", key),
				"absent from the from-empty fold")
		}
	}
	for key, d := range derived {
		if _, ok := stored[key]; !ok {
			return divergence(d.task, "label",
				fmt.Sprintf("label %q absent from the stored projection", key),
				"derived by the from-empty fold")
		}
	}
	return nil
}

// diffDomainComments compares the full comment tuple: key (id) presence in both directions,
// then each non-key content column (task_id, author_id, body, created_at). A comment whose
// stored body/author/task drifts from the from-empty re-derivation diverges here, naming the
// specific field — the completeness gap this fix closes.
func diffDomainComments(stored, derived map[string]domainComment) error {
	for id, s := range stored {
		d, ok := derived[id]
		if !ok {
			return divergence(s.task, "comment",
				fmt.Sprintf("comment %q present in the stored projection", id),
				"absent from the from-empty fold")
		}
		if s.task.String() != d.task.String() {
			return divergence(d.task, "comment task",
				fmt.Sprintf("comment %q task %s", id, s.task.String()),
				fmt.Sprintf("task %s", d.task.String()))
		}
		if s.author != d.author {
			return divergence(d.task, "comment author",
				fmt.Sprintf("comment %q author %s", id, s.author),
				fmt.Sprintf("author %s", d.author))
		}
		if s.body != d.body {
			return divergence(d.task, "comment body",
				fmt.Sprintf("comment %q body %q", id, s.body),
				fmt.Sprintf("body %q", d.body))
		}
		if s.createdAt != d.createdAt {
			return divergence(d.task, "comment created_at",
				fmt.Sprintf("comment %q created_at %d", id, s.createdAt),
				fmt.Sprintf("created_at %d", d.createdAt))
		}
	}
	for id, d := range derived {
		if _, ok := stored[id]; !ok {
			return divergence(d.task, "comment",
				fmt.Sprintf("comment %q absent from the stored projection", id),
				"derived by the from-empty fold")
		}
	}
	return nil
}
