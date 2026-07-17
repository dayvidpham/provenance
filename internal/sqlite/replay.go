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
	if err := sqlitex.Execute(db.conn,
		`SELECT kind_id, actor_id, recorded_at FROM journal WHERE JournalID = ?1`,
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
	)
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id, event_kind FROM journal_task_events WHERE JournalID = ?1`,
		&sqlitex.ExecOptions{Args: []any{jid}, ResultFunc: func(stmt *zs.Stmt) error {
			taskRaw = stmt.ColumnText(0)
			kindStr = stmt.ColumnText(1)
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
	if status, isLifecycle := journal.StatusForEventKind(journal.EventKind(kindStr)); isLifecycle {
		return db.projectTaskStatusLocked(task, status, jid, recordedAt)
	}
	return db.advanceWatermarkLocked(task, jid)
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
		`SELECT assignment_id, transition_id FROM journal_authority_assignment_transitions WHERE JournalID = ?1`,
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
		fmt.Sprintf(`SELECT task_id FROM %s WHERE JournalID = ?1`, table),
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
	if err := sqlitex.Execute(db.conn,
		`UPDATE tasks SET status_id = ?1, closed_at = ?2, last_journal_id = ?3 WHERE id = ?4`,
		&sqlitex.ExecOptions{Args: []any{int(status), closedAt, jid, task.String()}}); err != nil {
		return fmt.Errorf("project task status for %q: %w", task, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Open-time full replay (§9.2, §15)
// ---------------------------------------------------------------------------

// ReplayProjections folds the ENTIRE journal in JournalID order through the same
// projectJournalRowLocked reducer step Apply uses (§9.2), then verifies the
// recomputed projection converges with the stored incremental projection (§15). It
// first runs the external-schema preflight (§13) so a corrupted/partial topology
// fails closed before any fold. It is the Open/startup replay entry point and is
// idempotent: re-running it on a converged database is a no-op that returns the
// same per-task projection. On genuine divergence it returns a typed
// ProjectionDivergenceError and writes nothing further.
func (db *DB) ReplayProjections() (journal.ReplayResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.preflightSchemaLocked(); err != nil {
		return journal.ReplayResult{}, err
	}

	before, err := db.snapshotTaskProjectionsLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)

	folded := 0
	var order []int64
	if txErr = sqlitex.Execute(db.conn,
		`SELECT JournalID FROM journal ORDER BY JournalID ASC`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			order = append(order, stmt.ColumnInt64(0))
			return nil
		}}); txErr != nil {
		return journal.ReplayResult{}, fmt.Errorf("ReplayProjections: enumerate journal: %w", txErr)
	}
	for _, jid := range order {
		if txErr = db.projectJournalRowLocked(jid); txErr != nil {
			return journal.ReplayResult{}, fmt.Errorf("ReplayProjections: fold row %d: %w", jid, txErr)
		}
		folded++
	}

	after, err := db.snapshotTaskProjectionsLocked()
	if err != nil {
		txErr = err
		return journal.ReplayResult{}, err
	}
	// Convergence: the full replay must reproduce the stored incremental
	// projection exactly (§15). Any divergence is a fail-closed typed error.
	if txErr = diffTaskProjections(before, after); txErr != nil {
		return journal.ReplayResult{}, txErr
	}

	result := journal.ReplayResult{RowsFolded: folded}
	for _, p := range after {
		result.Tasks = append(result.Tasks, p)
	}
	return result, nil
}

// snapshotTaskProjectionsLocked reads every task's current projection (owner,
// status, watermark) keyed by task id, so a replay can compare its recomputed
// state against the stored incremental one (§15).
func (db *DB) snapshotTaskProjectionsLocked() (map[string]journal.TaskProjection, error) {
	out := map[string]journal.TaskProjection{}
	if err := sqlitex.Execute(db.conn,
		`SELECT id, owner_id, status_id, last_journal_id FROM tasks`,
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

func diffTaskProjections(before, after map[string]journal.TaskProjection) error {
	for id, a := range after {
		b := before[id]
		if ownerString(a.Owner) != ownerString(b.Owner) {
			return divergence(a.TaskID, "owner", ownerString(b.Owner), ownerString(a.Owner))
		}
		if a.Status != b.Status {
			return divergence(a.TaskID, "status", b.Status.String(), a.Status.String())
		}
		if a.LastJournalID != b.LastJournalID {
			return divergence(a.TaskID, "watermark",
				fmt.Sprintf("%d", int64(b.LastJournalID)), fmt.Sprintf("%d", int64(a.LastJournalID)))
		}
	}
	return nil
}

func divergence(task journal.TaskID, field, stored, replayed string) error {
	return &journal.ProjectionDivergenceError{
		Operation: "ReplayProjections",
		Task:      task,
		Field:     field,
		Stored:    stored,
		Replayed:  replayed,
		Why:       "the stored incremental projection does not equal the projection derived by folding ordered journal history",
		Impact:    "the database is not accepted as converged; no further projection write is applied",
		Fix:       "rebuild projections from the journal, or investigate the out-of-band write that corrupted the stored projection (§8.1: owner/status are reducer-exclusive)",
	}
}

func ownerString(o *journal.ActorID) string {
	if o == nil {
		return ""
	}
	return o.String()
}
