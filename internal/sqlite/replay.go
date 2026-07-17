package sqlite

import (
	"errors"
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
		`SELECT kind_id, effective_actor_id, recorded_at FROM journal_attributed WHERE JournalID = ?1`,
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
		`SELECT task_id, event_kind, payload FROM journal_task_events WHERE JournalID = ?1`,
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

// errReplayScratchRollback forces the from-empty scratch re-derivation savepoint
// to roll back after its projections are captured, so ReplayProjections is a
// read-only convergence CHECK that never persists the rebuild (it fails closed on
// divergence rather than silently repairing). It never escapes ReplayProjections.
var errReplayScratchRollback = errors.New("provenance: internal replay-scratch rollback (never surfaced)")

// ReplayProjections re-derives EVERY projection from an EMPTY slate by folding the
// entire journal in JournalID order through the same projectJournalRowLocked
// reducer step Apply uses (§9.2, §15), then verifies the stored projection equals
// that genuine from-empty re-derivation. Unlike an on-top re-fold (which only
// detects drift in a field some journal row actually writes, so an out-of-band
// corruption on a field no row revisits — e.g. a hand-corrupted tasks.status_id on
// a task with no status-changing lifecycle event — reads back unchanged and is
// falsely reported as converged), this clears the projection to a blank slate in a
// scratch savepoint, refolds, and compares the FULL projection set — owner, status,
// watermark, AND task_attributions — for every journal-anchored task. It first runs
// the external-schema preflight (§13). It is read-only and idempotent: the scratch
// rebuild is always rolled back, so a converged database is left untouched and
// returns the from-empty per-task projection. On genuine divergence it returns a
// typed ProjectionDivergenceError naming the task, field, and stored-vs-derived
// values, and writes nothing (§13.1 six-field actionable shape).
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
	storedTasks, err := db.snapshotTaskProjectionsLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}
	storedAttribs, err := db.snapshotAttributionsLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}
	anchored, err := db.journalAnchoredTasksLocked()
	if err != nil {
		return journal.ReplayResult{}, err
	}

	// Re-derive every projection from empty in a scratch savepoint that is always
	// rolled back (read-only check).
	derivedTasks, derivedAttribs, folded, err := db.rederiveProjectionsScratchLocked()
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

	result := journal.ReplayResult{RowsFolded: folded}
	for id := range anchored {
		if p, ok := derivedTasks[id]; ok {
			result.Tasks = append(result.Tasks, p)
		}
	}
	return result, nil
}

// rederiveProjectionsScratchLocked clears the projection to an empty slate and
// refolds the whole journal through the single shared reducer step (§9.2), then
// captures the from-empty projection and attribution snapshots. The scratch
// rebuild runs inside a savepoint that is ALWAYS rolled back, so ReplayProjections
// never persists the rebuild — it returns the derived state purely for comparison.
func (db *DB) rederiveProjectionsScratchLocked() (
	tasks map[string]journal.TaskProjection,
	attribs map[string]map[string]int64,
	folded int,
	err error,
) {
	var txErr error
	endTx := sqlitex.Save(db.conn)
	defer func() {
		// Force the scratch rebuild to roll back even on the success path, so the
		// destructive clear+refold is never durable.
		if txErr == nil {
			txErr = errReplayScratchRollback
		}
		endTx(&txErr)
	}()

	if txErr = db.clearProjectionsLocked(); txErr != nil {
		return nil, nil, 0, fmt.Errorf("ReplayProjections: clear projections for from-empty replay: %w", txErr)
	}

	var order []int64
	if txErr = sqlitex.Execute(db.conn,
		`SELECT JournalID FROM journal ORDER BY JournalID ASC`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			order = append(order, stmt.ColumnInt64(0))
			return nil
		}}); txErr != nil {
		return nil, nil, 0, fmt.Errorf("ReplayProjections: enumerate journal: %w", txErr)
	}
	for _, jid := range order {
		if txErr = db.projectJournalRowLocked(jid); txErr != nil {
			return nil, nil, 0, fmt.Errorf("ReplayProjections: fold row %d: %w", jid, txErr)
		}
		folded++
	}

	tasks, txErr = db.snapshotTaskProjectionsLocked()
	if txErr != nil {
		return nil, nil, 0, txErr
	}
	attribs, txErr = db.snapshotAttributionsLocked()
	if txErr != nil {
		return nil, nil, 0, txErr
	}
	// txErr is nil here: the deferred rollback replaces it with the scratch
	// sentinel so the rebuild is discarded; the captured snapshots survive.
	return tasks, attribs, folded, nil
}

// clearProjectionsLocked resets every task's projection columns to the empty-slate
// values (owner NULL, status open, closed_at NULL, watermark NULL) and empties the
// task_attributions projection, so the subsequent refold rebuilds them SOLELY from
// journal history rather than reading any value back from the pre-existing row
// (§15: no projection is seeded/patched from a non-journal input). The journal-spine
// subtype tables — the source of truth the reducer reads — are untouched.
func (db *DB) clearProjectionsLocked() error {
	if err := sqlitex.Execute(db.conn,
		`UPDATE tasks SET owner_id = NULL, status_id = ?1, closed_at = NULL, last_journal_id = NULL`,
		&sqlitex.ExecOptions{Args: []any{statusOpenID}}); err != nil {
		return fmt.Errorf("clear tasks projection: %w", err)
	}
	if err := sqlitex.Execute(db.conn, `DELETE FROM task_attributions`, nil); err != nil {
		return fmt.Errorf("clear task_attributions projection: %w", err)
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
// status, watermark) keyed by task id, so a replay can compare its recomputed
// state against the stored one (§15).
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

// snapshotAttributionsLocked reads the task_attributions projection as a nested
// map task_id → (actor_id → first_journal_id), so a replay can compare the stored
// attribution edges against the from-empty re-derivation (§8.2, §15).
func (db *DB) snapshotAttributionsLocked() (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id, actor_id, first_journal_id FROM task_attributions`,
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
