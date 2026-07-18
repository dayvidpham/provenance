package sqlite

import (
	"fmt"

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
// Scope during the direct-write staging window (pasture#14): convergence is
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

	// Snapshot the STORED projection before any change, keyed by task id.
	storedTasks, err := db.snapshotTaskProjectionsLocked("tasks")
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
	derivedTasks, derivedAttribs, derivedDomain, folded, err := db.rederiveProjectionsShadowLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}

	// Convergence over the FULL projection set, scoped to journal-anchored tasks.
	if err := diffTaskProjections(storedTasks, derivedTasks, anchored); err != nil {
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
	attribs map[string]map[string]int64,
	domain domainProjection,
	folded int,
	err error,
) {
	if err = db.createProjectionShadowLocked(); err != nil {
		return nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: stage shadow projection tables: %w", err)
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
		return nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: enumerate journal: %w", err)
	}
	for _, jid := range order {
		if err = db.projectJournalRowLocked(jid); err != nil {
			return nil, nil, domainProjection{}, 0, fmt.Errorf("ReplayProjections: fold row %d: %w", jid, err)
		}
		folded++
	}

	tasks, err = db.snapshotTaskProjectionsLocked(shadowTasksTable)
	if err != nil {
		return nil, nil, domainProjection{}, 0, err
	}
	attribs, err = db.snapshotAttributionsLocked(shadowAttribTable)
	if err != nil {
		return nil, nil, domainProjection{}, 0, err
	}
	domain, err = db.snapshotDomainProjectionsLocked(shadowEdgesTable, shadowLabelsTable, shadowCommentsTable)
	if err != nil {
		return nil, nil, domainProjection{}, 0, err
	}
	return tasks, attribs, domain, folded, nil
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
			owner_id        TEXT,
			status_id       INTEGER,
			closed_at       INTEGER,
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
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO shadow_tasks (id, owner_id, status_id, closed_at, last_journal_id)
		 SELECT id, NULL, ?1, NULL, NULL FROM tasks`,
		&sqlitex.ExecOptions{Args: []any{statusOpenID}}); err != nil {
		return fmt.Errorf("seed shadow_tasks empty slate: %w", err)
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

// domainProjection is a from-a-slate snapshot of the journaled relationship/annotation
// domain projections (§6 amendment): edges keyed by (source, target, kind), labels by
// (task, name), comments by id. Each value is the task the projected row reports on, so a
// divergence can name it. Because every edge/label/comment now flows through the journal,
// these sets are compared WHOLE (not scoped to anchored tasks): a from-empty refold
// reproduces exactly the journaled rows.
type domainProjection struct {
	edges    map[string]journal.TaskID
	labels   map[string]journal.TaskID
	comments map[string]journal.TaskID
}

// snapshotDomainProjectionsLocked reads the edge/label/comment key sets from the named
// tables (the real tables, or the shadow tables during a from-empty derivation) so a
// replay can diff the stored domain projection against the from-empty re-derivation (§15).
func (db *DB) snapshotDomainProjectionsLocked(edgesT, labelsT, commentsT string) (domainProjection, error) {
	dp := domainProjection{
		edges:    map[string]journal.TaskID{},
		labels:   map[string]journal.TaskID{},
		comments: map[string]journal.TaskID{},
	}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT source_id, target_id, kind_id FROM %s`, edgesT),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			src, tgt, kind := stmt.ColumnText(0), stmt.ColumnText(1), stmt.ColumnInt(2)
			task, err := journalParseTask(src)
			if err != nil {
				return err
			}
			dp.edges[fmt.Sprintf("%s\x00%s\x00%d", src, tgt, kind)] = task
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
			dp.labels[fmt.Sprintf("%s\x00%s", taskRaw, name)] = task
			return nil
		}}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot labels %s: %w", labelsT, err)
	}
	if err := sqlitex.Execute(db.conn,
		fmt.Sprintf(`SELECT id, task_id FROM %s`, commentsT),
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			id, taskRaw := stmt.ColumnText(0), stmt.ColumnText(1)
			task, err := journalParseTask(taskRaw)
			if err != nil {
				return err
			}
			dp.comments[id] = task
			return nil
		}}); err != nil {
		return domainProjection{}, fmt.Errorf("snapshot comments %s: %w", commentsT, err)
	}
	return dp, nil
}

// diffDomainProjections asserts the stored edge/label/comment sets equal the from-empty
// re-derivation (§6, §15): a stored row the fold does not reproduce, or a folded row the
// stored projection lacks, fails closed with a typed ProjectionDivergenceError.
func diffDomainProjections(stored, derived domainProjection) error {
	if err := diffDomainSet("edge", stored.edges, derived.edges); err != nil {
		return err
	}
	if err := diffDomainSet("label", stored.labels, derived.labels); err != nil {
		return err
	}
	return diffDomainSet("comment", stored.comments, derived.comments)
}

func diffDomainSet(field string, stored, derived map[string]journal.TaskID) error {
	for key, task := range stored {
		if _, ok := derived[key]; !ok {
			return divergence(task, field,
				fmt.Sprintf("%s %q present in the stored projection", field, key),
				"absent from the from-empty fold")
		}
	}
	for key, task := range derived {
		if _, ok := stored[key]; !ok {
			return divergence(task, field,
				fmt.Sprintf("%s %q absent from the stored projection", field, key),
				"derived by the from-empty fold")
		}
	}
	return nil
}
