package sqlite

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// replay.go implements the shared-reducer projection step, Open-time full replay,
// and the projection-convergence check (docs/journal-relational-contract.md §9,
// §15). projectJournalRowLocked is the SINGLE per-row reducer step: Apply calls it
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

// projectJournalRowLocked advances the §8 projections (tasks.owner_id,
// tasks.status_id, tasks.last_journal_id, and task_attributions) for one already-
// committed journal row, derived solely from the persisted row and the ordered
// history before it. Apply invokes it incrementally after each effect; Open's full
// replay invokes it per row from JournalID 1. Both share this one fold (§9.2). It
// assumes db.mu is held and runs inside the caller's transaction.
func (db *DB) projectJournalRowLocked(jid int64) error {
	var (
		kind       = -1
		actorRaw   string
		recordedAt int64
	)
	found := false
	// Read the committing actor through journal_attributed (§8.5): a subordinate row
	// stores actor_id NULL, so effective_actor_id derives it from the row's anchor —
	// never a bare read of the NULL column. The single shared reducer step therefore
	// attributes the same committing actor whether Apply folds a just-produced
	// subordinate row or Open replays it (§9.2).
	if err := sqlitex.Execute(db.conn,
		`SELECT kind_id, effective_actor_id, recorded_at FROM journal_attributed WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			kind = stmt.ColumnInt(0)
			actorRaw = stmt.ColumnText(1)
			recordedAt = stmt.ColumnInt64(2)
			return nil
		}}); err != nil {
		return fmt.Errorf("project journal row %d: load supertype: %w", jid, err)
	}
	if !found {
		return fmt.Errorf("project journal row %d: no such journal row", jid)
	}
	committing, err := journalParseActor(actorRaw)
	if err != nil {
		return err
	}

	switch journal.JournalKind(kind) {
	case journal.JournalKindOperation:
		return nil // an operation anchor projects nothing
	case journal.JournalKindTaskEvent:
		return db.projectTaskEventRowLocked(jid, committing, recordedAt)
	case journal.JournalKindAuthority:
		return db.projectAuthorityRowLocked(jid)
	case journal.JournalKindDecision:
		return db.projectTaskScopedRowLocked(jid, committing, "journal_decisions")
	case journal.JournalKindEvidence:
		return db.projectTaskScopedRowLocked(jid, committing, "journal_evidence")
	default:
		return fmt.Errorf("project journal row %d: unknown journal kind %d", jid, kind)
	}
}

// projectTaskEventRowLocked attributes the committing (authoring) actor (§8.2),
// projects any lifecycle-status transition Provenance defines for its own
// namespaced kind (§8.1), and advances the watermark.
func (db *DB) projectTaskEventRowLocked(jid int64, committing journal.ActorID, recordedAt int64) error {
	var (
		taskRaw string
		kindStr string
		payload []byte
	)
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id, event_kind, payload FROM journal_task_events WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
			taskRaw = stmt.ColumnText(0)
			kindStr = stmt.ColumnText(1)
			payload = readBlob(stmt, 2)
			return nil
		}}); err != nil {
		return fmt.Errorf("project task_event %d: %w", jid, err)
	}
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if err := db.insertAttributionLocked(task, committing, jid); err != nil {
		return err
	}
	canonicalEffect, canonical, err := db.canonicalEffectForJournalRowLocked(jid)
	if err != nil {
		return err
	}
	if canonical {
		if canonicalEffect.TaskID != task || canonicalEffect.EventKind != "" && canonicalEffect.EventKind != journal.EventKind(kindStr) {
			return fmt.Errorf("provenance: canonical mutation row %d disagrees with journal_task_events task/event facts — where: startup canonical validation; impact: database open fails without projection writes; fix: restore the journal row and canonical bytes from the same committed operation", jid)
		}
		if db.projTasks() == shadowTasksTable {
			switch canonicalEffect.Sort {
			case journal.EffectTaskCreate:
				if err := db.insertCanonicalShadowTaskLocked(canonicalEffect, recordedAt, jid); err != nil {
					return err
				}
			case journal.EffectTaskEvent:
				if err := db.materializeCanonicalShadowTaskEventLocked(canonicalEffect, recordedAt); err != nil {
					return err
				}
			}
		}
	}
	// Journaled relationship/annotation mutation families (§6 amendment) fold into the
	// edges/labels/comments domain projection through the same shared reducer step Apply
	// and Open both run (§9.2), so those tables are re-derivable from ordered history and
	// covered by the §15 convergence check. They are non-lifecycle, so they never reach
	// the status branches below.
	if journal.IsMutationFamilyKind(journal.EventKind(kindStr)) {
		return db.projectMutationFamilyRowLocked(task, journal.EventKind(kindStr), payload, jid, recordedAt)
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
			return db.projectTaskStatusLocked(task, status, jid, recordedAt)
		}
		return db.advanceWatermarkLocked(task, jid)
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
				current, cerr := db.readProjTaskStatusLocked(task)
				if cerr != nil {
					return cerr
				}
				if verr := journal.ValidateStatusTransition(current, journal.EventKind(kindStr)); verr != nil {
					return fmt.Errorf("project task_event %d: %w", jid, verr)
				}
			}
		}
		return db.projectTaskStatusLocked(task, status, jid, recordedAt)
	}
	return db.advanceWatermarkLocked(task, jid)
}

// readProjTaskStatusLocked reads a task's current lifecycle status from the projection
// target (the real tasks table during a live Apply, the shadow table during a from-empty
// replay derivation), so the status FSM checks the transition against the status derived
// from history strictly before the row being folded (§8.1, §15).
func (db *DB) readProjTaskStatusLocked(task journal.TaskID) (journal.TaskStatus, error) {
	var status journal.TaskStatus
	found := false
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT status_id FROM %s WHERE id = ?1`, db.projTasks()),
		&sqlitex.ExecOptions{Args: []any{task.String()}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			status = journal.TaskStatus(stmt.ColumnInt(0))
			return nil
		}}); err != nil {
		return 0, fmt.Errorf("read current status for %q: %w", task, err)
	}
	if !found {
		return 0, fmt.Errorf(
			"provenance: status FSM cannot read current status of task %q — where: shared-reducer "+
				"status projection (§8.1); when: folding a transition lifecycle event; impact: nothing "+
				"committed; fix: the task row must exist (born via Session.Create) before a lifecycle "+
				"transition is folded against it", task)
	}
	return status, nil
}

// projectAuthorityRowLocked projects an authority row. A bootstrap authority
// touches no task and projects nothing; an assignment transition attributes the
// episode occupant on its started transition (§8.2), and recomputes the
// owner-responsibility projection on the owner slot (§8.1). The reducer reads the
// episode the transition belongs to rather than re-deriving from the caller.
func (db *DB) projectAuthorityRowLocked(jid int64) error {
	var (
		assignment string
		transition = -1
		hasTrans   bool
	)
	if err := sqlitex.Execute(db.conn,
		`SELECT assignment_id, transition_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
			hasTrans = true
			assignment = stmt.ColumnText(0)
			transition = stmt.ColumnInt(1)
			return nil
		}}); err != nil {
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
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id, actor_id, slot_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{assignment}, ResultFunc: func(stmt *zs.Stmt) error {
			taskRaw = stmt.ColumnText(0)
			occupantRaw = stmt.ColumnText(1)
			slot = stmt.ColumnInt(2)
			return nil
		}}); err != nil {
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
		if err := db.insertAttributionLocked(task, occupant, jid); err != nil {
			return err
		}
	}
	if slot == slotOwnerResponsibilityID {
		return db.recomputeTaskOwnerLocked(task, jid)
	}
	return db.advanceWatermarkLocked(task, jid)
}

// projectTaskScopedRowLocked attributes the committing actor for a task-scoped
// decision/evidence row and advances the watermark (§8.2). An untasked row
// attributes nothing.
func (db *DB) projectTaskScopedRowLocked(jid int64, committing journal.ActorID, table string) error {
	var taskRaw string
	hasTask := false
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT task_id FROM %s WHERE journal_id = ?1`, table),
		&sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnType(0) != zs.TypeNull {
				taskRaw = stmt.ColumnText(0)
				hasTask = true
			}
			return nil
		}}); err != nil {
		return fmt.Errorf("project %s %d: %w", table, jid, err)
	}
	if !hasTask {
		return nil
	}
	task, err := journalParseTask(taskRaw)
	if err != nil {
		return err
	}
	if err := db.insertAttributionLocked(task, committing, jid); err != nil {
		return err
	}
	return db.advanceWatermarkLocked(task, jid)
}

// projectTaskStatusLocked materializes the lifecycle-status projection on tasks
// (§8.1): status_id, closed_at (set on close, cleared on re-open), and the
// watermark. Like owner_id, status here is written only by the shared reducer for
// journal-anchored lifecycle events.
func (db *DB) projectTaskStatusLocked(task journal.TaskID, status journal.TaskStatus, jid, recordedAt int64) error {
	var closedAt any
	if status == journal.TaskStatusClosed {
		closedAt = recordedAt
	}
	// Targets the real tasks table during a live Apply and the shadow tasks table
	// during a from-empty replay derivation (§8.1, §15).
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`UPDATE %s SET status_id = ?1, closed_at = ?2, last_journal_id = ?3 WHERE id = ?4`, db.projTasks()),
		&sqlitex.ExecOptions{Args: []any{int(status), closedAt, jid, task.String()}}); err != nil {
		return fmt.Errorf("project task status for %q: %w", task, err)
	}
	return nil
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
// a from-empty refold) and needs no scratch savepoint/rollback.
const (
	shadowTasksTable    = "shadow_tasks"
	shadowAttribTable   = "shadow_task_attributions"
	shadowEdgesTable    = "shadow_edges"
	shadowLabelsTable   = "shadow_labels"
	shadowCommentsTable = "shadow_comments"
)

// ReplayProjections re-derives EVERY projection from an EMPTY slate by folding the
// entire journal in JournalID order through the same projectJournalRowLocked
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
// database is left untouched, the check is constraint-independent (the NOT NULL
// tasks.last_journal_id tightening cannot be tripped by the refold), and no scratch
// savepoint/rollback is needed. On genuine divergence it returns a typed
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
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.preflightSchemaLocked(); err != nil {
		return journal.ReplayResult{}, err
	}
	if err := db.validateCanonicalOperationsLocked(); err != nil {
		return journal.ReplayResult{}, err
	}

	// Snapshot the STORED projection before any change, keyed by task id.
	storedTasks, err := db.snapshotTaskProjectionsLocked("tasks")
	if err != nil {
		return journal.ReplayResult{}, err
	}
	storedTaskState, err := db.snapshotCompleteTaskStateLocked("tasks")
	if err != nil {
		return journal.ReplayResult{}, err
	}
	storedAttribs, err := db.snapshotAttributionsLocked("task_attributions")
	if err != nil {
		return journal.ReplayResult{}, err
	}
	anchored, err := db.journalAnchoredTasksLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}
	// The relationship/annotation domain projections are journaled in full (§6 amendment),
	// so every edge/label/comment is journal-reproducible; the stored sets are snapshotted
	// whole (not scoped to anchored tasks) and diffed against the from-empty re-derivation.
	storedDomain, err := db.snapshotDomainProjectionsLocked("edges", "labels", "comments")
	if err != nil {
		return journal.ReplayResult{}, err
	}

	// Re-derive every projection from empty into connection-scoped shadow tables;
	// the real tables stay read-only (SHADOW DERIVATION, §15).
	derivedTasks, derivedTaskState, derivedAttribs, derivedDomain, folded, err := db.rederiveProjectionsShadowLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}

	// Convergence over the FULL projection set, scoped to journal-anchored tasks.
	if err := diffTaskProjections(storedTasks, derivedTasks, anchored); err != nil {
		return journal.ReplayResult{}, err
	}
	if err := diffCompleteTaskState(storedTaskState, derivedTaskState, anchored); err != nil {
		return journal.ReplayResult{}, err
	}
	if err := diffAttributions(storedAttribs, derivedAttribs, anchored); err != nil {
		return journal.ReplayResult{}, err
	}
	// Convergence over the journaled edge/label/comment domain projections (§6, §15).
	if err := diffDomainProjections(storedDomain, derivedDomain); err != nil {
		return journal.ReplayResult{}, err
	}

	result := journal.ReplayResult{RowsFolded: folded}
	for id := range anchored {
		if p, ok := derivedTasks[id]; ok {
			result.Tasks = append(result.Tasks, p)
		}
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

func (db *DB) validateCanonicalOperationsLocked() error {
	var operations []canonicalStoredOperation
	columns, err := db.tableColumnsLocked("journal_operations")
	if err != nil {
		return err
	}
	versionExpr, wireExpr := "o.mutation_encoding_version", "o.canonical_mutation"
	if isLegacyOperationsColumnSet(columns) {
		versionExpr, wireExpr = "NULL", "NULL"
	}
	query := fmt.Sprintf(`SELECT o.journal_id, o.authority_journal_id, %s, %s,
		        o.mutation_digest, j.actor_id, j.recorded_at
		 FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id
		 ORDER BY o.journal_id`, versionExpr, wireExpr)
	if err := sqlitex.Execute(db.conn,
		query,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			op := canonicalStoredOperation{anchor: stmt.ColumnInt64(0), digest: readBlob(stmt, 4), recordedAt: stmt.ColumnInt64(6)}
			if stmt.ColumnType(1) != zs.TypeNull {
				authority := journal.JournalID(stmt.ColumnInt64(1))
				op.authority = &authority
			}
			if stmt.ColumnType(2) != zs.TypeNull {
				op.versionSet = true
				op.version = stmt.ColumnText(2)
			}
			if stmt.ColumnType(3) != zs.TypeNull {
				op.wireSet = true
				op.wire = readBlob(stmt, 3)
			}
			actor, err := journalParseActor(stmt.ColumnText(5))
			if err != nil {
				return err
			}
			op.actor = actor
			operations = append(operations, op)
			return nil
		}}); err != nil {
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
		if op.version != prepared.EncodingVersion() {
			return canonicalCorruption(op.anchor, "encoding version", op.version, prepared.EncodingVersion())
		}
		if !bytes.Equal(op.digest, prepared.DerivedDigest()) {
			return canonicalCorruption(op.anchor, "mutation digest", fmt.Sprintf("%x", op.digest), fmt.Sprintf("%x", prepared.DerivedDigest()))
		}
		effects := prepared.NormalizedEffects()
		var rows []int64
		if err := sqlitex.Execute(db.conn, `SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id`, &sqlitex.ExecOptions{Args: []any{op.anchor}, ResultFunc: func(stmt *zs.Stmt) error { rows = append(rows, stmt.ColumnInt64(0)); return nil }}); err != nil {
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
			if err := db.requireAuthorityExistsLocked(*op.authority); err != nil {
				return fmt.Errorf("startup canonical operation %d authority: %w", op.anchor, err)
			}
			in := journal.OperationInput{ActorID: op.actor, AuthorityJournalID: op.authority}
			for i, effect := range effects {
				if effect.TaskID.Namespace != "" {
					if err := db.requireAuthorityGovernsLocked(in, rows[i], effect.TaskID); err != nil {
						return fmt.Errorf("startup canonical operation %d authority for effect %d: %w", op.anchor, i, err)
					}
				}
			}
		}
		if err := db.validateCanonicalResultSlotsLocked(op.anchor, rows, effects); err != nil {
			return err
		}
		for i, effect := range effects {
			if err := db.validateCanonicalEffectRowLocked(op, rows[i], effect); err != nil {
				return fmt.Errorf("operation %d effect %d: %w", op.anchor, i, err)
			}
		}
	}
	return nil
}

func (db *DB) validateCanonicalResultSlotsLocked(anchor int64, rows []int64, effects []journal.Effect) error {
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
	if err := sqlitex.Execute(db.conn, `SELECT result_slot_id,produced_journal_id FROM journal_operation_result_slots WHERE journal_id=?1`, &sqlitex.ExecOptions{Args: []any{anchor}, ResultFunc: func(stmt *zs.Stmt) error { slot := stmt.ColumnText(0); actual[slot] = stmt.ColumnInt64(1); return nil }}); err != nil {
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

func (db *DB) validateCanonicalEffectRowLocked(op canonicalStoredOperation, jid int64, effect journal.Effect) error {
	expectedKind, err := effect.Sort.JournalKind()
	if err != nil {
		return err
	}
	var kind int
	var recordedAt int64
	if err := sqlitex.Execute(db.conn, `SELECT kind_id, recorded_at FROM journal WHERE journal_id=?1`, &sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error { kind = stmt.ColumnInt(0); recordedAt = stmt.ColumnInt64(1); return nil }}); err != nil {
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
	case journal.EffectTaskCreate, journal.EffectTaskEvent, journal.EffectEdgeAdd, journal.EffectEdgeRemove, journal.EffectLabelAdd, journal.EffectLabelRemove, journal.EffectCommentAdd:
		return db.validateCanonicalTaskEventLocked(op.anchor, jid, effect)
	case journal.EffectBootstrapAuthority:
		label := effect.BootstrapLabel
		if label == "" {
			label = "bootstrap"
		}
		auth := string(effect.OperationAuthorityID)
		if auth == "" {
			auth = fmt.Sprintf("authority--bootstrap--%d", jid)
		}
		return db.compareSingleRowLocked(op.anchor, jid, `SELECT a.operation_authority_id, b.label FROM journal_authorities a JOIN journal_authority_bootstraps b ON b.journal_id=a.journal_id WHERE a.journal_id=?1`, []string{auth, label})
	case journal.EffectAssignmentStart:
		occupant := effect.Occupant
		if occupant.Namespace == "" {
			occupant = op.actor
		}
		slot, _ := slotDBID(effect.SlotID)
		return db.compareSingleRowLocked(op.anchor, jid, `SELECT e.assignment_id,e.task_id,e.slot_id,e.actor_id,COALESCE(e.predecessor_assignment_id,''),COALESCE(e.parent_assignment_id,''),t.transition_id FROM journal_authority_assignment_transitions t JOIN journal_authority_assignment_episodes e ON e.assignment_id=t.assignment_id WHERE t.journal_id=?1`, []string{string(effect.AssignmentID), effect.TaskID.String(), strconv.Itoa(slot), occupant.String(), string(effect.Predecessor), string(effect.Parent), strconv.Itoa(transitionStartedID)})
	case journal.EffectAssignmentEnd:
		return db.compareSingleRowLocked(op.anchor, jid, `SELECT assignment_id,transition_id FROM journal_authority_assignment_transitions WHERE journal_id=?1`, []string{string(effect.AssignmentID), strconv.Itoa(transitionEndedID)})
	case journal.EffectDecision:
		return db.compareSingleRowLocked(op.anchor, jid, `SELECT decision_kind,COALESCE(task_id,''),payload FROM journal_decisions WHERE journal_id=?1`, []string{string(effect.DecisionKind), optionalTaskString(effect.TaskID), string(effect.Payload)})
	case journal.EffectEvidence:
		return db.compareSingleRowLocked(op.anchor, jid, `SELECT evidence_kind,COALESCE(task_id,''),hex(content_digest),payload FROM journal_evidence WHERE journal_id=?1`, []string{string(effect.EvidenceKind), optionalTaskString(effect.TaskID), strings.ToUpper(fmt.Sprintf("%x", effect.ContentDigest)), string(effect.Payload)})
	}
	return nil
}

func optionalTaskString(id journal.TaskID) string {
	if id.Namespace == "" {
		return ""
	}
	return id.String()
}

func (db *DB) validateCanonicalTaskEventLocked(anchor, jid int64, effect journal.Effect) error {
	kind := effect.EventKind
	payload := effect.Payload
	if effect.Sort == journal.EffectTaskCreate {
		kind = journal.EventKindTaskCreated
	}
	if effect.Sort == journal.EffectTaskEvent && effect.Forced && journal.IsTransitionLifecycleKind(kind) {
		payload = journal.EncodeForcedTransitionPayload()
	}
	if journalKind, ok := journal.MutationFamilyKindForSort(effect.Sort); ok {
		kind = journalKind
		var err error
		payload, err = db.encodeMutationFamilyPayload(effect)
		if err != nil {
			return err
		}
	}
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := db.compareSingleRowLocked(anchor, jid, `SELECT task_id,event_kind,payload FROM journal_task_events WHERE journal_id=?1`, []string{effect.TaskID.String(), string(kind), string(payload)}); err != nil {
		return err
	}
	expected, err := journal.CanonicalEventContexts(effect.Contexts)
	if err != nil {
		return err
	}
	actual, err := db.loadContextsLocked(jid)
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
	if err := sqlitex.Execute(db.conn, `SELECT attached_by_journal_id FROM journal_task_event_contexts WHERE event_journal_id=?1 ORDER BY context_kind,context_identity`, &sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error { attached = append(attached, stmt.ColumnInt64(0)); return nil }}); err != nil {
		return err
	}
	for i, got := range attached {
		if got != jid {
			return canonicalCorruption(anchor, fmt.Sprintf("row %d context %d attached_by_journal_id", jid, i), strconv.FormatInt(got, 10), strconv.FormatInt(jid, 10))
		}
	}
	return nil
}

func (db *DB) compareSingleRowLocked(anchor, jid int64, query string, expected []string) error {
	found := false
	var actual []string
	if err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
		found = true
		for i := range expected {
			actual = append(actual, stmt.ColumnText(i))
		}
		return nil
	}}); err != nil {
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

// rederiveProjectionsShadowLocked re-derives every projection from an empty slate
// into connection-scoped shadow tables and returns the derived projection and
// attribution snapshots for comparison, WITHOUT mutating the real tables (§15 SHADOW
// DERIVATION). It creates empty-slate shadow tables (one row per real task id, with
// blank projection columns and NO real-table constraints), repoints the shared
// reducer's projection-write target at them, folds the whole journal in JournalID
// order through the same projectJournalRowLocked step Apply uses (§9.2), then captures
// the from-empty snapshots from the shadow tables. The real tasks / task_attributions
// rows are read-only throughout, so the check is constraint-independent (the NOT NULL
// tasks.last_journal_id tightening is never tripped) and needs no savepoint/rollback.
// The projection-write target and the shadow tables are always restored/dropped
// before return, on every path.
func (db *DB) rederiveProjectionsShadowLocked() (
	tasks map[string]journal.TaskProjection,
	taskState map[string]completeTaskState,
	attribs map[string]map[string]int64,
	domain domainProjection,
	folded int,
	err error,
) {
	if err = db.createProjectionShadowLocked(); err != nil {
		return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: stage shadow projection tables: %w", err)
	}
	// Repoint the shared reducer's projection-write steps at the shadow tables, and
	// unconditionally restore the real targets + drop the shadow tables on return. The
	// relationship/annotation domain projections (§6 amendment) are repointed alongside
	// the task/attribution projections so their from-empty refold lands in the shadow
	// tables too.
	db.projTasksTable = shadowTasksTable
	db.projAttribTable = shadowAttribTable
	db.projEdgesTable = shadowEdgesTable
	db.projLabelsTable = shadowLabelsTable
	db.projCommentsTable = shadowCommentsTable
	defer func() {
		db.projTasksTable = ""
		db.projAttribTable = ""
		db.projEdgesTable = ""
		db.projLabelsTable = ""
		db.projCommentsTable = ""
		if derr := db.dropProjectionShadowLocked(); derr != nil && err == nil {
			err = fmt.Errorf("ReplayProjections: drop shadow projection tables: %w", derr)
		}
	}()

	var order []int64
	if err = sqlitex.Execute(db.conn,
		`SELECT journal_id FROM journal ORDER BY journal_id ASC`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			order = append(order, stmt.ColumnInt64(0))
			return nil
		}}); err != nil {
		return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: enumerate journal: %w", err)
	}
	for _, jid := range order {
		if err = db.projectJournalRowLocked(jid); err != nil {
			return nil, nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: fold row %d: %w", jid, err)
		}
		folded++
	}

	tasks, err = db.snapshotTaskProjectionsLocked(shadowTasksTable)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	taskState, err = db.snapshotCompleteTaskStateLocked(shadowTasksTable)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	attribs, err = db.snapshotAttributionsLocked(shadowAttribTable)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	domain, err = db.snapshotDomainProjectionsLocked(shadowEdgesTable, shadowLabelsTable, shadowCommentsTable)
	if err != nil {
		return nil, nil, nil, domainProjection{}, 0, err
	}
	return tasks, taskState, attribs, domain, folded, nil
}

// createProjectionShadowLocked builds the connection-scoped empty-slate shadow
// projection tables the from-empty refold derives into (§15). The shadow tasks table
// is seeded with one blank-projection row per real task id (owner NULL, status open,
// closed_at NULL, watermark NULL) so a lifecycle/watermark UPDATE folded during the
// refold has a row to hit — exactly the empty-slate the retired clear-in-place scratch
// produced, but on a throwaway table with NO constraints (no NOT NULL watermark, no
// foreign keys), so the real tables are never touched. The shadow attribution table
// starts empty. The journal-spine subtype tables the reducer READS are untouched.
func (db *DB) createProjectionShadowLocked() error {
	// Drop first in case a prior aborted run left them on the connection.
	if err := db.dropProjectionShadowLocked(); err != nil {
		return err
	}
	ddl := []string{
		`CREATE TEMP TABLE shadow_tasks (
			id              TEXT PRIMARY KEY,
			namespace       TEXT,
			title           TEXT,
			description     TEXT,
			owner_id        TEXT,
			status_id       INTEGER,
			priority_id     INTEGER,
			type_id         INTEGER,
			phase_id        INTEGER,
			notes           TEXT,
			created_at      INTEGER,
			updated_at      INTEGER,
			closed_at       INTEGER,
			close_reason    TEXT,
			last_journal_id INTEGER
		)`,
		`CREATE TEMP TABLE shadow_task_attributions (
			task_id          TEXT NOT NULL,
			actor_id         TEXT NOT NULL,
			first_journal_id INTEGER NOT NULL,
			PRIMARY KEY (task_id, actor_id)
		)`,
		// Journaled relationship/annotation domain projections (§6 amendment): shadow
		// mirrors of the real edges/labels/comments tables, carrying the same projection
		// columns but NONE of the real constraints (no FKs), started EMPTY so the
		// from-empty refold re-derives them purely from journal history (§15).
		`CREATE TEMP TABLE shadow_edges (
			source_id  TEXT NOT NULL,
			target_id  TEXT NOT NULL,
			kind_id    INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (source_id, target_id, kind_id)
		)`,
		`CREATE TEMP TABLE shadow_labels (
			task_id TEXT NOT NULL,
			name    TEXT NOT NULL,
			PRIMARY KEY (task_id, name)
		)`,
		`CREATE TEMP TABLE shadow_comments (
			id         TEXT PRIMARY KEY,
			task_id    TEXT NOT NULL,
			author_id  TEXT NOT NULL,
			body       TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
	}
	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("create shadow projection table: %w", err)
		}
	}
	// Only opaque legacy task births are seeded from their materialized row. New
	// canonical births must be reconstructed from canonical effects, making missing
	// and spurious task rows observable while retaining mixed legacy/new databases.
	columns, err := db.tableColumnsLocked("journal_operations")
	if err != nil {
		return err
	}
	legacyBirth := "o.canonical_mutation IS NULL"
	if isLegacyOperationsColumnSet(columns) {
		legacyBirth = "1"
	}
	seedLegacy := fmt.Sprintf(`INSERT INTO shadow_tasks
		 (id, namespace, title, description, owner_id, status_id, priority_id, type_id,
		  phase_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)
		 SELECT t.id, t.namespace, t.title, t.description, NULL, ?1, t.priority_id, t.type_id,
		        t.phase_id, t.notes, t.created_at, t.updated_at, NULL, t.close_reason, NULL
		 FROM tasks t
		 WHERE EXISTS (
		   SELECT 1 FROM journal_task_events e
		   JOIN journal j ON j.journal_id=e.journal_id
		   JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id
		   WHERE e.task_id=t.id AND (
		     (e.event_kind=?2 AND %s) OR e.event_kind=?3
		   )
		 )`, legacyBirth)
	if err := sqlitex.Execute(db.conn, seedLegacy,
		&sqlitex.ExecOptions{Args: []any{statusOpenID, string(journal.EventKindTaskCreated), string(journal.EventKindTaskMigrated)}}); err != nil {
		return fmt.Errorf("seed shadow_tasks legacy slate: %w", err)
	}
	return nil
}

func (db *DB) canonicalEffectForJournalRowLocked(jid int64) (journal.Effect, bool, error) {
	var version string
	var wire []byte
	var anchor int64
	found := false
	columns, err := db.tableColumnsLocked("journal_operations")
	if err != nil {
		return journal.Effect{}, false, err
	}
	versionExpr, wireExpr := "o.mutation_encoding_version", "o.canonical_mutation"
	if isLegacyOperationsColumnSet(columns) {
		versionExpr, wireExpr = "NULL", "NULL"
	}
	query := fmt.Sprintf(`SELECT o.journal_id, %s, %s
		 FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id
		 WHERE j.journal_id=?1`, versionExpr, wireExpr)
	if err := sqlitex.Execute(db.conn, query,
		&sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			anchor = stmt.ColumnInt64(0)
			if stmt.ColumnType(1) != zs.TypeNull {
				version = stmt.ColumnText(1)
			}
			if stmt.ColumnType(2) != zs.TypeNull {
				wire = readBlob(stmt, 2)
			}
			return nil
		}}); err != nil {
		return journal.Effect{}, false, fmt.Errorf("load canonical mutation for row %d: %w", jid, err)
	}
	if !found {
		return journal.Effect{}, false, nil
	}
	if (version == "") != (len(wire) == 0) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d has mixed canonical encoding facts (version=%q bytes=%d) — where: startup canonical validation; impact: database open fails without mutation; fix: restore both canonical columns together, or NULL both only for a genuine legacy operation", anchor, version, len(wire))
	}
	if version == "" {
		return journal.Effect{}, false, nil
	}
	if version != journal.MutationEncodingV1 {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d uses unsupported canonical mutation version %q", anchor, version)
	}
	prepared, err := journal.DecodeCanonicalMutation(wire)
	if err != nil {
		return journal.Effect{}, false, fmt.Errorf("operation %d canonical mutation: %w", anchor, err)
	}
	ordinal := 0
	if err := sqlitex.Execute(db.conn,
		`SELECT COUNT(*) FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id<=?2`,
		&sqlitex.ExecOptions{Args: []any{anchor, jid}, ResultFunc: func(stmt *zs.Stmt) error { ordinal = stmt.ColumnInt(0) - 1; return nil }}); err != nil {
		return journal.Effect{}, false, fmt.Errorf("resolve canonical effect ordinal for row %d: %w", jid, err)
	}
	effects := prepared.NormalizedEffects()
	if ordinal < 0 || ordinal >= len(effects) {
		return journal.Effect{}, false, fmt.Errorf("provenance: operation %d row %d has effect ordinal %d outside canonical effect count %d", anchor, jid, ordinal, len(effects))
	}
	return effects[ordinal], true, nil
}

func (db *DB) insertCanonicalShadowTaskLocked(e journal.Effect, recordedAt, jid int64) error {
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO shadow_tasks
		 (id, namespace, title, description, owner_id, status_id, priority_id, type_id,
		  phase_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)
		 VALUES (?1, ?2, ?3, ?4, NULL, ?5, ?6, ?7, ?8, '', ?9, ?9, NULL, '', ?10)`,
		&sqlitex.ExecOptions{Args: []any{e.TaskID.String(), e.TaskID.Namespace, e.Title, e.Description,
			statusOpenID, int(e.Priority), int(e.Type), int(e.Phase), recordedAt, jid}}); err != nil {
		return fmt.Errorf("replay canonical task create %q: %w", e.TaskID, err)
	}
	return nil
}

func (db *DB) materializeCanonicalShadowTaskEventLocked(e journal.Effect, recordedAt int64) error {
	set := []string{"updated_at=?1"}
	args := []any{recordedAt}
	add := func(column string, value any) {
		args = append(args, value)
		set = append(set, column+"=?"+strconv.Itoa(len(args)))
	}
	if e.UpdateTitle != nil {
		add("title", *e.UpdateTitle)
	}
	if e.UpdateDescription != nil {
		add("description", *e.UpdateDescription)
	}
	if e.UpdatePriority != nil {
		add("priority_id", int(*e.UpdatePriority))
	}
	if e.UpdatePhase != nil {
		add("phase_id", int(*e.UpdatePhase))
	}
	if e.UpdateNotes != nil {
		add("notes", *e.UpdateNotes)
	}
	if e.EventKind == journal.EventKindTaskClosed && e.CloseReason != "" {
		add("close_reason", e.CloseReason)
	}
	if len(set) == 1 {
		return nil
	}
	args = append(args, e.TaskID.String())
	query := fmt.Sprintf("UPDATE shadow_tasks SET %s WHERE id=?%d", strings.Join(set, ","), len(args))
	if err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		return fmt.Errorf("replay canonical task event %q: %w", e.TaskID, err)
	}
	return nil
}

// dropProjectionShadowLocked removes the connection-scoped shadow projection tables.
func (db *DB) dropProjectionShadowLocked() error {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS shadow_tasks`, `DROP TABLE IF EXISTS shadow_task_attributions`,
		`DROP TABLE IF EXISTS shadow_edges`, `DROP TABLE IF EXISTS shadow_labels`,
		`DROP TABLE IF EXISTS shadow_comments`,
	} {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("drop shadow projection table: %w", err)
		}
	}
	return nil
}

// journalAnchoredTasksLocked returns the set of task ids referenced by at least one
// journal-spine subtype row (task_event, assignment episode, task-scoped decision
// or evidence). These are the tasks whose projections are journal-reproducible and
// therefore in scope for the §15 convergence assertion.
func (db *DB) journalAnchoredTasksLocked() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id FROM journal_task_events
		 UNION SELECT task_id FROM journal_authority_assignment_episodes
		 UNION SELECT task_id FROM journal_decisions WHERE task_id IS NOT NULL
		 UNION SELECT task_id FROM journal_evidence WHERE task_id IS NOT NULL`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnType(0) != zs.TypeNull {
				out[stmt.ColumnText(0)] = struct{}{}
			}
			return nil
		}}); err != nil {
		return nil, fmt.Errorf("enumerate journal-anchored tasks: %w", err)
	}
	return out, nil
}

// snapshotTaskProjectionsLocked reads every task's current projection (owner,
// status, watermark) keyed by task id from the named table, so a replay can compare
// the stored projection ("tasks") against the from-empty re-derivation
// ("shadow_tasks") (§15).
func (db *DB) snapshotTaskProjectionsLocked(table string) (map[string]journal.TaskProjection, error) {
	out := map[string]journal.TaskProjection{}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT id, owner_id, status_id, last_journal_id FROM %s`, table),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			taskRaw := stmt.ColumnText(0)
			task, err := journalParseTask(taskRaw)
			if err != nil {
				return err
			}
			p := journal.TaskProjection{TaskID: task, Status: journal.TaskStatus(stmt.ColumnInt(2))}
			if stmt.ColumnType(1) != zs.TypeNull {
				owner, err := journalParseActor(stmt.ColumnText(1))
				if err != nil {
					return err
				}
				p.Owner = &owner
			}
			if stmt.ColumnType(3) != zs.TypeNull {
				p.LastJournalID = journal.JournalID(stmt.ColumnInt64(3))
			}
			out[taskRaw] = p
			return nil
		}}); err != nil {
		return nil, fmt.Errorf("snapshot task projections: %w", err)
	}
	return out, nil
}

// snapshotAttributionsLocked reads the attribution projection from the named table
// as a nested map task_id → (actor_id → first_journal_id), so a replay can compare
// the stored edges ("task_attributions") against the from-empty re-derivation
// ("shadow_task_attributions") (§8.2, §15).
func (db *DB) snapshotAttributionsLocked(table string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT task_id, actor_id, first_journal_id FROM %s`, table),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			task := stmt.ColumnText(0)
			if out[task] == nil {
				out[task] = map[string]int64{}
			}
			out[task][stmt.ColumnText(1)] = stmt.ColumnInt64(2)
			return nil
		}}); err != nil {
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

func (db *DB) snapshotCompleteTaskStateLocked(table string) (map[string]completeTaskState, error) {
	out := map[string]completeTaskState{}
	query := fmt.Sprintf(`SELECT id, namespace, title, description, status_id, priority_id,
		type_id, phase_id, owner_id, notes, created_at, updated_at, closed_at,
		close_reason, last_journal_id FROM %s`, table)
	if err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		id, err := journalParseTask(stmt.ColumnText(0))
		if err != nil {
			return err
		}
		text := func(column int) string {
			if stmt.ColumnType(column) == zs.TypeNull {
				return "<null>"
			}
			return stmt.ColumnText(column)
		}
		integer := func(column int) string {
			if stmt.ColumnType(column) == zs.TypeNull {
				return "<null>"
			}
			return strconv.FormatInt(stmt.ColumnInt64(column), 10)
		}
		out[id.String()] = completeTaskState{task: id, fields: map[string]string{
			"namespace": text(1), "title": text(2), "description": text(3),
			"status": integer(4), "priority": integer(5), "type": integer(6),
			"phase": integer(7), "owner": text(8), "notes": text(9),
			"created_at": integer(10), "updated_at": integer(11), "closed_at": integer(12),
			"close_reason": text(13), "watermark": integer(14),
		}}
		return nil
	}}); err != nil {
		return nil, fmt.Errorf("snapshot complete task state from %s: %w", table, err)
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

// snapshotDomainProjectionsLocked reads the FULL edge/label/comment tuples from the named
// tables (the real tables, or the shadow tables during a from-empty derivation) so a replay
// can diff the stored domain projection against the from-empty re-derivation across every
// column, not merely the keys (§15). It selects the non-key content columns
// (edges.created_at; comments.author_id/body/created_at) alongside the keys; labels have no
// non-key column.
func (db *DB) snapshotDomainProjectionsLocked(edgesT, labelsT, commentsT string) (domainProjection, error) {
	dp := domainProjection{
		edges:    map[string]domainEdge{},
		labels:   map[string]domainLabel{},
		comments: map[string]domainComment{},
	}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT source_id, target_id, kind_id, created_at FROM %s`, edgesT),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			src, tgt, kind := stmt.ColumnText(0), stmt.ColumnText(1), stmt.ColumnInt(2)
			task, err := journalParseTask(src)
			if err != nil {
				return err
			}
			dp.edges[fmt.Sprintf("%s\x00%s\x00%d", src, tgt, kind)] = domainEdge{task: task, createdAt: stmt.ColumnInt64(3)}
			return nil
		}}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot edges %s: %w", edgesT, err)
	}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT task_id, name FROM %s`, labelsT),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			taskRaw, name := stmt.ColumnText(0), stmt.ColumnText(1)
			task, err := journalParseTask(taskRaw)
			if err != nil {
				return err
			}
			dp.labels[fmt.Sprintf("%s\x00%s", taskRaw, name)] = domainLabel{task: task}
			return nil
		}}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot labels %s: %w", labelsT, err)
	}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT id, task_id, author_id, body, created_at FROM %s`, commentsT),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			id, taskRaw := stmt.ColumnText(0), stmt.ColumnText(1)
			task, err := journalParseTask(taskRaw)
			if err != nil {
				return err
			}
			dp.comments[id] = domainComment{
				task:      task,
				author:    stmt.ColumnText(2),
				body:      stmt.ColumnText(3),
				createdAt: stmt.ColumnInt64(4),
			}
			return nil
		}}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot comments %s: %w", commentsT, err)
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
