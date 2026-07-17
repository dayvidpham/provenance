package sqlite

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// journalParseActor / journalParseTask resolve stored wire-format IDs into the
// single typed identity domain, so a corrupt stored ID surfaces as a decode
// error rather than a silent zero value.
func journalParseActor(s string) (journal.ActorID, error) {
	id, err := ptypes.ParseActorID(s)
	if err != nil {
		return journal.ActorID{}, fmt.Errorf("decode stored actor id %q: %w", s, err)
	}
	return id, nil
}

func journalParseTask(s string) (journal.TaskID, error) {
	id, err := ptypes.ParseTaskID(s)
	if err != nil {
		return journal.TaskID{}, fmt.Errorf("decode stored task id %q: %w", s, err)
	}
	return id, nil
}

// This file implements the journal-base persistence surface (issue
// dayvidpham/provenance#4) described by docs/journal-relational-contract.md:
// the global journal supertype, the
// task_event subtype and its canonical context edges, the actor-namespace
// registry (§7), the current-task-state watermark and task_attributions
// projections (§8), the JournalID-ordered query surface (§8.3, §12), and the
// reducer-enforced subtype-integrity guard (§10 rule 8) for the kinds
// implementable at this layer.

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

// ensureJournalSchema creates the journal-base relations and seeds the closed
// journal_kinds lookup. Idempotent (CREATE TABLE IF NOT EXISTS / INSERT OR
// IGNORE), mirroring the existing reference-data discipline.
func (db *DB) ensureJournalSchema() error {
	ddl := []string{
		// Closed discriminator lookup (§2.2).
		`CREATE TABLE IF NOT EXISTS journal_kinds (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE
		) STRICT`,
		// Global journal supertype (§2.1). JournalID is the sole canonical order,
		// database-generated via AUTOINCREMENT so a strictly ascending, gap-stable
		// order survives concurrent same-RecordedAt commits.
		`CREATE TABLE IF NOT EXISTS journal (
			JournalID   INTEGER PRIMARY KEY AUTOINCREMENT,
			kind_id     INTEGER NOT NULL REFERENCES journal_kinds(id),
			actor_id    TEXT NOT NULL REFERENCES agents(id),
			recorded_at INTEGER NOT NULL,
			-- The producing operation (§2.1, §4.6). NULL at the journal-base layer;
			-- the operations slice (dayvidpham/provenance#5) adds the FK to
			-- journal_operations(JournalID) when that subtype table lands.
			produced_by_operation_journal_id INTEGER
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_journal_kind  ON journal (kind_id)`,
		`CREATE INDEX IF NOT EXISTS idx_journal_actor ON journal (actor_id)`,
		// task_event subtype (§5.1). PK == JournalID, class-table inheritance.
		`CREATE TABLE IF NOT EXISTS journal_task_events (
			JournalID  INTEGER PRIMARY KEY REFERENCES journal(JournalID),
			task_id    TEXT NOT NULL REFERENCES tasks(id),
			event_kind TEXT NOT NULL,
			payload    TEXT NOT NULL CHECK (json_valid(payload))
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_journal_task_events_task ON journal_task_events (task_id)`,
		// task-event context edges (§5.2). Written once; the first attach owns
		// AttachedByJournalID permanently, so a snapshot bounded by
		// attached_by_journal_id <= watermark is reproducible.
		`CREATE TABLE IF NOT EXISTS journal_task_event_contexts (
			event_journal_id       INTEGER NOT NULL REFERENCES journal_task_events(JournalID),
			context_kind           TEXT NOT NULL,
			context_identity       TEXT NOT NULL,
			attached_by_journal_id INTEGER NOT NULL REFERENCES journal_task_events(JournalID),
			PRIMARY KEY (event_journal_id, context_kind, context_identity)
		) STRICT, WITHOUT ROWID`,
		// Actor-namespace registry (§7.1, §7.2).
		`CREATE TABLE IF NOT EXISTS actor_namespace_claims (
			namespace   TEXT PRIMARY KEY,
			claimant_id TEXT NOT NULL,
			range_min   BLOB NOT NULL,
			range_max   BLOB NOT NULL,
			codec       TEXT NOT NULL
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS fixed_actor_manifest_entries (
			actor_id  TEXT PRIMARY KEY REFERENCES agents(id),
			namespace TEXT NOT NULL REFERENCES actor_namespace_claims(namespace),
			kind_id   INTEGER NOT NULL REFERENCES agent_kinds(id),
			name      TEXT NOT NULL,
			metadata  TEXT NOT NULL CHECK (json_valid(metadata)),
			UNIQUE (namespace, name)
		) STRICT`,
		// task_attributions projection (§8.2). Append-only cumulative edges.
		`CREATE TABLE IF NOT EXISTS task_attributions (
			task_id          TEXT NOT NULL REFERENCES tasks(id),
			actor_id         TEXT NOT NULL REFERENCES agents(id),
			first_journal_id INTEGER NOT NULL REFERENCES journal(JournalID),
			PRIMARY KEY (task_id, actor_id)
		) STRICT, WITHOUT ROWID`,
	}
	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureJournalSchema: %w — statement: %s", err, stmt[:min(len(stmt), 80)])
		}
	}

	// Seed journal_kinds from the single source of truth in the journal package,
	// so the SQL lookup and the Go enum can never drift.
	for _, k := range journal.JournalKinds() {
		if err := sqlitex.Execute(db.conn,
			`INSERT OR IGNORE INTO journal_kinds (id, name) VALUES (?1, ?2)`,
			&sqlitex.ExecOptions{Args: []any{int(k), k.String()}},
		); err != nil {
			return fmt.Errorf("ensureJournalSchema: seed journal_kinds %s: %w", k, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Append (journal-base single-row write)
// ---------------------------------------------------------------------------

// AppendTaskEvent writes one task-event row to the global journal as a single
// transaction — the supertype journal row, its journal_task_events subtype row,
// its canonical/deduplicated context edges, the reducer subtype-integrity gate
// (§10 rule 8), and the incremental projection updates (task_attributions and
// tasks.last_journal_id, §8). It returns the produced row with its assigned
// JournalID. Fail-closed: any error rolls back the whole write (§9.5).
func (db *DB) AppendTaskEvent(in journal.AppendTaskEventInput) (journal.TaskEventRow, error) {
	if err := journal.ValidateEventKind(in.EventKind); err != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: %w", err)
	}
	if in.ActorID.Namespace == "" {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: actor ID is required")
	}
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(in.Payload) {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: payload for %q is not valid JSON", in.EventKind)
	}
	contexts, err := journal.CanonicalEventContexts(in.Contexts)
	if err != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: %w", err)
	}
	recordedAt := in.RecordedAt.UTC()

	db.mu.Lock()
	defer db.mu.Unlock()

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)

	var journalID int64
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)
		 VALUES (?1, ?2, ?3, NULL)`,
		&sqlitex.ExecOptions{Args: []any{
			int(journal.JournalKindTaskEvent), in.ActorID.String(), recordedAt.UnixNano(),
		}},
	); txErr != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: insert journal row: %w", txErr)
	}
	journalID = db.conn.LastInsertRowID()

	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_task_events (JournalID, task_id, event_kind, payload)
		 VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{
			journalID, in.TaskID.String(), string(in.EventKind), string(in.Payload),
		}},
	); txErr != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: insert journal_task_events row: %w", txErr)
	}

	for _, ctx := range contexts {
		kind, identity, encErr := journal.EncodeStoredEventContext(ctx)
		if encErr != nil {
			txErr = encErr
			return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: encode context: %w", txErr)
		}
		if txErr = sqlitex.Execute(db.conn,
			`INSERT OR IGNORE INTO journal_task_event_contexts
				(event_journal_id, context_kind, context_identity, attached_by_journal_id)
			 VALUES (?1, ?2, ?3, ?4)`,
			&sqlitex.ExecOptions{Args: []any{journalID, string(kind), identity, journalID}},
		); txErr != nil {
			return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: insert context edge: %w", txErr)
		}
	}

	// Reducer subtype-integrity gate (§10 rule 8) before the projections.
	if txErr = db.verifySubtypeIntegrityLocked(); txErr != nil {
		return journal.TaskEventRow{}, txErr
	}

	// Projection: first-wins attribution edge for the authoring actor (§8.2).
	if txErr = sqlitex.Execute(db.conn,
		`INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id)
		 VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{in.TaskID.String(), in.ActorID.String(), journalID}},
	); txErr != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: update task_attributions: %w", txErr)
	}

	// Projection: advance the current-task-state watermark if the task exists.
	if txErr = sqlitex.Execute(db.conn,
		`UPDATE tasks SET last_journal_id = ?1 WHERE id = ?2`,
		&sqlitex.ExecOptions{Args: []any{journalID, in.TaskID.String()}},
	); txErr != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: advance tasks.last_journal_id: %w", txErr)
	}

	row := journal.TaskEventRow{
		Row: journal.Row{
			JournalID:  journal.JournalID(journalID),
			Kind:       journal.JournalKindTaskEvent,
			ActorID:    in.ActorID,
			RecordedAt: recordedAt,
		},
		TaskID:    in.TaskID,
		EventKind: in.EventKind,
		Payload:   in.Payload,
		Contexts:  contexts,
	}
	return row, nil
}

// AppendBareJournalRow inserts a supertype journal row of the given kind with
// NO matching subtype row, deliberately leaving the journal in a state that
// violates subtype totality (§10 rule 8). It exists so the adversarial proof
// corpus can drive the production VerifyIntegrity guard against a totality
// violation; production writers (AppendTaskEvent, and the operation writers of
// later slices) always write the subtype row atomically. The caller is expected
// to VerifyIntegrity and roll back.
func (db *DB) AppendBareJournalRow(kind journal.JournalKind, actorID journal.ActorID, recordedAt time.Time) (journal.JournalID, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)
		 VALUES (?1, ?2, ?3, NULL)`,
		&sqlitex.ExecOptions{Args: []any{int(kind), actorID.String(), recordedAt.UTC().UnixNano()}},
	); err != nil {
		return 0, fmt.Errorf("AppendBareJournalRow: %w", err)
	}
	return journal.JournalID(db.conn.LastInsertRowID()), nil
}

// ---------------------------------------------------------------------------
// Subtype-integrity guard (§10 rule 8)
// ---------------------------------------------------------------------------

// VerifyIntegrity checks the whole journal for class-table-inheritance
// violations (§10 rule 8): totality (every journal row has its kind's subtype
// row), exclusivity (a JournalID appears in at most one subtype table), and
// discriminator agreement (a subtype row's table matches journal.kind_id). It is
// the §15 convergence tool Open uses and the gate AppendTaskEvent runs before
// commit. Returns journal.ErrSubtypeIntegrity on any violation.
func (db *DB) VerifyIntegrity() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.verifySubtypeIntegrityLocked()
}

// knownSubtypeTables maps each JournalKind to its subtype table. Only tables
// that actually exist in the live schema are checked, so this guard extends
// automatically as later slices add operation/authority/decision/evidence
// tables without weakening the check for the kinds present today.
func (db *DB) subtypeTablesPresent() (map[journal.JournalKind]string, error) {
	all := map[journal.JournalKind]string{
		journal.JournalKindOperation: "journal_operations",
		journal.JournalKindTaskEvent: "journal_task_events",
		journal.JournalKindAuthority: "journal_authorities",
		journal.JournalKindDecision:  "journal_decisions",
		journal.JournalKindEvidence:  "journal_evidence",
	}
	present := make(map[journal.JournalKind]string, len(all))
	for kind, table := range all {
		var exists bool
		if err := sqlitex.Execute(db.conn,
			`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?1`,
			&sqlitex.ExecOptions{
				Args:       []any{table},
				ResultFunc: func(*zs.Stmt) error { exists = true; return nil },
			},
		); err != nil {
			return nil, fmt.Errorf("subtypeTablesPresent: probe %q: %w", table, err)
		}
		if exists {
			present[kind] = table
		}
	}
	return present, nil
}

func (db *DB) verifySubtypeIntegrityLocked() error {
	tables, err := db.subtypeTablesPresent()
	if err != nil {
		return err
	}
	for kind, table := range tables {
		// Totality: a journal row of this kind with no subtype row.
		var missing int64
		if err := sqlitex.Execute(db.conn,
			fmt.Sprintf(`SELECT j.JournalID FROM journal j
				LEFT JOIN %s s ON s.JournalID = j.JournalID
				WHERE j.kind_id = ?1 AND s.JournalID IS NULL LIMIT 1`, table),
			&sqlitex.ExecOptions{
				Args:       []any{int(kind)},
				ResultFunc: func(stmt *zs.Stmt) error { missing = stmt.ColumnInt64(0); return nil },
			},
		); err != nil {
			return fmt.Errorf("verifySubtypeIntegrity totality %s: %w", table, err)
		}
		if missing != 0 {
			return fmt.Errorf(
				"%w: journal row %d has kind %s but no matching %s subtype row — "+
					"where: subtype-integrity gate; when: before commit; impact: the "+
					"write is rolled back because class-table inheritance requires "+
					"exactly one subtype row per journal row; fix: write the %s row in "+
					"the same transaction as its journal row",
				journal.ErrSubtypeIntegrity, missing, kind, table, table)
		}
		// Discriminator agreement + exclusivity: a subtype row whose journal row
		// carries a different kind_id (or a JournalID present in a foreign
		// subtype table).
		var mismatch int64
		if err := sqlitex.Execute(db.conn,
			fmt.Sprintf(`SELECT s.JournalID FROM %s s
				JOIN journal j ON j.JournalID = s.JournalID
				WHERE j.kind_id <> ?1 LIMIT 1`, table),
			&sqlitex.ExecOptions{
				Args:       []any{int(kind)},
				ResultFunc: func(stmt *zs.Stmt) error { mismatch = stmt.ColumnInt64(0); return nil },
			},
		); err != nil {
			return fmt.Errorf("verifySubtypeIntegrity agreement %s: %w", table, err)
		}
		if mismatch != 0 {
			return fmt.Errorf(
				"%w: %s carries a row for journal %d whose supertype discriminator is "+
					"not %s — where: subtype-integrity gate; when: before commit; "+
					"impact: the write is rolled back; fix: the subtype table must "+
					"agree with journal.kind_id",
				journal.ErrSubtypeIntegrity, table, mismatch, kind)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ordered query surface (§8.3, §12)
// ---------------------------------------------------------------------------

// QueryTaskEvents returns one page of task-event rows in strictly ascending
// JournalID order (§12). It rejects any non-JournalID ordering before touching
// the database (§1). The first call (SnapshotMaxJournalID == 0) pins the
// snapshot to the current MAX(JournalID); later pages must repeat that
// watermark and pass the previous page's AfterJournalID cursor.
func (db *DB) QueryTaskEvents(q journal.JournalQueryV1) (journal.JournalTaskEventPageV1, error) {
	if err := q.Validate(); err != nil {
		return journal.JournalTaskEventPageV1{}, err
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	snapshot := int64(q.SnapshotMaxJournalID)
	if snapshot == 0 {
		if err := sqlitex.Execute(db.conn,
			`SELECT COALESCE(MAX(JournalID), 0) FROM journal`,
			&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
				snapshot = stmt.ColumnInt64(0)
				return nil
			}},
		); err != nil {
			return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: snapshot watermark: %w", err)
		}
	}

	args := []any{snapshot, int64(q.AfterJournalID)}
	sql := `SELECT j.JournalID, j.actor_id, j.recorded_at, te.task_id, te.event_kind, te.payload
		FROM journal j JOIN journal_task_events te ON te.JournalID = j.JournalID
		WHERE j.JournalID <= ?1 AND j.JournalID > ?2`
	next := 3
	if len(q.TaskIDs) > 0 {
		sql += " AND te.task_id IN ("
		for i, id := range q.TaskIDs {
			if i > 0 {
				sql += ","
			}
			sql += fmt.Sprintf("?%d", next)
			args = append(args, id.String())
			next++
		}
		sql += ")"
	}
	if len(q.EventKinds) > 0 {
		sql += " AND te.event_kind IN ("
		for i, k := range q.EventKinds {
			if i > 0 {
				sql += ","
			}
			sql += fmt.Sprintf("?%d", next)
			args = append(args, string(k))
			next++
		}
		sql += ")"
	}
	sql += " ORDER BY j.JournalID ASC"
	fetch := q.Limit
	if fetch > 0 {
		sql += fmt.Sprintf(" LIMIT %d", fetch+1) // +1 to detect a further page
	}

	var rows []journal.TaskEventRow
	if err := sqlitex.Execute(db.conn, sql, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			row, scanErr := scanTaskEventRow(stmt)
			if scanErr != nil {
				return scanErr
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: %w", err)
	}

	page := journal.JournalTaskEventPageV1{SnapshotMaxJournalID: journal.JournalID(snapshot)}
	if fetch > 0 && len(rows) > fetch {
		rows = rows[:fetch]
		last := rows[len(rows)-1].JournalID
		page.Next = &journal.JournalCursorV1{
			SnapshotMaxJournalID: journal.JournalID(snapshot),
			AfterJournalID:       last,
		}
	}
	// Attach each row's canonical context set.
	for i := range rows {
		ctxs, err := db.loadContextsLocked(int64(rows[i].JournalID))
		if err != nil {
			return journal.JournalTaskEventPageV1{}, err
		}
		rows[i].Contexts = ctxs
	}
	page.Events = rows
	return page, nil
}

func scanTaskEventRow(stmt *zs.Stmt) (journal.TaskEventRow, error) {
	actorID, err := journalParseActor(stmt.ColumnText(1))
	if err != nil {
		return journal.TaskEventRow{}, err
	}
	taskID, err := journalParseTask(stmt.ColumnText(3))
	if err != nil {
		return journal.TaskEventRow{}, err
	}
	return journal.TaskEventRow{
		Row: journal.Row{
			JournalID:  journal.JournalID(stmt.ColumnInt64(0)),
			Kind:       journal.JournalKindTaskEvent,
			ActorID:    actorID,
			RecordedAt: time.Unix(0, stmt.ColumnInt64(2)).UTC(),
		},
		TaskID:    taskID,
		EventKind: journal.EventKind(stmt.ColumnText(4)),
		Payload:   json.RawMessage(stmt.ColumnText(5)),
	}, nil
}

func (db *DB) loadContextsLocked(journalID int64) ([]journal.EventContext, error) {
	var ctxs []journal.EventContext
	if err := sqlitex.Execute(db.conn,
		`SELECT context_kind, context_identity FROM journal_task_event_contexts
		 WHERE event_journal_id = ?1 ORDER BY context_kind, context_identity`,
		&sqlitex.ExecOptions{
			Args: []any{journalID},
			ResultFunc: func(stmt *zs.Stmt) error {
				ctx, decErr := journal.DecodeStoredEventContext(
					journal.EventContextKind(stmt.ColumnText(0)), stmt.ColumnText(1))
				if decErr != nil {
					return decErr
				}
				ctxs = append(ctxs, ctx)
				return nil
			},
		},
	); err != nil {
		return nil, fmt.Errorf("loadContexts %d: %w", journalID, err)
	}
	return ctxs, nil
}

// ---------------------------------------------------------------------------
// Attribution projection read (§8.2)
// ---------------------------------------------------------------------------

// TaskAttributions returns the cumulative attribution edges for a task in
// ascending FirstJournalID order.
func (db *DB) TaskAttributions(taskID journal.TaskID) ([]journal.TaskAttribution, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []journal.TaskAttribution
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id, actor_id, first_journal_id FROM task_attributions
		 WHERE task_id = ?1 ORDER BY first_journal_id ASC`,
		&sqlitex.ExecOptions{
			Args: []any{taskID.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				actorID, err := journalParseActor(stmt.ColumnText(1))
				if err != nil {
					return err
				}
				tid, err := journalParseTask(stmt.ColumnText(0))
				if err != nil {
					return err
				}
				out = append(out, journal.TaskAttribution{
					TaskID:         tid,
					ActorID:        actorID,
					FirstJournalID: journal.JournalID(stmt.ColumnInt64(2)),
				})
				return nil
			},
		},
	); err != nil {
		return nil, fmt.Errorf("TaskAttributions %q: %w", taskID.String(), err)
	}
	return out, nil
}
