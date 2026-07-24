package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// bindOperationDB leases one pool connection for a complete public operation and
// binds the existing connection-oriented internals to it. Startup uses a pool-less
// DB on its exclusive activation connection, so it remains the sole activation
// owner without taking a runtime lease. P2 removes this adapter with db.conn.
func (db *DB) bindOperationDB(ctx context.Context) (*DB, func(), error) {
	if db.pool == nil {
		return db, func() {}, nil
	}
	scope, err := db.bindConn(ctx)
	if err != nil {
		return nil, nil, err
	}
	bound := &DB{
		pool:             db.pool,
		conn:             scope.conn,
		projectionTarget: db.projectionTarget,
	}
	return bound, scope.release, nil
}

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

// journalAttributedViewDDL creates the read-only denormalized attribution surface
// (§8.5). effective_actor_id is a row's own stored actor on an anchor, or its
// anchor's actor on a subordinate row, resolved once via the anchor self-join so no
// consumer hand-writes the COALESCE. LEFT JOIN keeps anchor rows (whose
// produced_by_operation_journal_id is NULL) in the result. It is defined as a shared
// const because the operations slice's journal-table rebuild
// (completeJournalOperationFK) must drop and recreate the view around the rebuild —
// a view that references the table being renamed cannot dangle across the rename.
const journalAttributedViewDDL = `CREATE VIEW IF NOT EXISTS journal_attributed AS
	SELECT j.journal_id                           AS journal_id,
	       j.kind_id                              AS kind_id,
	       COALESCE(j.actor_id, anchor.actor_id)  AS effective_actor_id,
	       j.recorded_at                          AS recorded_at,
	       j.produced_by_operation_journal_id     AS produced_by_operation_journal_id
	FROM journal j
	LEFT JOIN journal anchor
	  ON anchor.journal_id = j.produced_by_operation_journal_id`

const insertJournalTaskEventSQL = "INSERT INTO journal_task_events (journal_id, task_id, event_kind, payload) VALUES (?1, ?2, ?3, ?4)"

const insertJournalAuthoritySQL = "INSERT INTO journal_authorities (journal_id, authority_kind_id, operation_authority_id) VALUES (?1, ?2, ?3)"

// ensureJournalSchema creates the journal-base relations and seeds the closed
// journal_kinds lookup. Idempotent (CREATE TABLE IF NOT EXISTS / INSERT OR
// IGNORE), mirroring the existing reference-data discipline.
func (db *DB) ensureJournalSchema() error {
	ddl := []string{
		"CREATE TABLE IF NOT EXISTS journal_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		`CREATE TABLE IF NOT EXISTS journal (
			journal_id INTEGER PRIMARY KEY AUTOINCREMENT, kind_id INTEGER NOT NULL REFERENCES journal_kinds(id),
			actor_id TEXT REFERENCES agents(id), recorded_at INTEGER NOT NULL, produced_by_operation_journal_id INTEGER,
			CHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL))) STRICT`,
		"CREATE INDEX IF NOT EXISTS idx_journal_kind ON journal (kind_id)",
		"CREATE INDEX IF NOT EXISTS idx_journal_actor ON journal (actor_id)",
		"CREATE INDEX IF NOT EXISTS idx_journal_recorded_at ON journal (recorded_at, journal_id)",
		"CREATE TABLE IF NOT EXISTS journal_task_events (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), task_id TEXT NOT NULL REFERENCES tasks(id), event_kind TEXT NOT NULL, payload TEXT NOT NULL CHECK (json_valid(payload))) STRICT",
		"CREATE INDEX IF NOT EXISTS idx_journal_task_events_task ON journal_task_events (task_id)",
		"CREATE TABLE IF NOT EXISTS journal_task_event_contexts (event_journal_id INTEGER NOT NULL REFERENCES journal_task_events(journal_id), context_kind TEXT NOT NULL, context_identity TEXT NOT NULL, attached_by_journal_id INTEGER NOT NULL REFERENCES journal_task_events(journal_id), PRIMARY KEY (event_journal_id, context_kind, context_identity)) STRICT, WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS actor_namespace_claims (namespace TEXT PRIMARY KEY, claimant_id TEXT NOT NULL, range_min BLOB NOT NULL, range_max BLOB NOT NULL, codec TEXT NOT NULL) STRICT",
		"CREATE TABLE IF NOT EXISTS fixed_actor_manifest_entries (actor_id TEXT PRIMARY KEY REFERENCES agents(id), namespace TEXT NOT NULL REFERENCES actor_namespace_claims(namespace), kind_id INTEGER NOT NULL REFERENCES agent_kinds(id), name TEXT NOT NULL, metadata TEXT NOT NULL CHECK (json_valid(metadata)), UNIQUE (namespace, name)) STRICT",
		"CREATE TABLE IF NOT EXISTS task_attributions (task_id TEXT NOT NULL REFERENCES tasks(id), actor_id TEXT NOT NULL REFERENCES agents(id), first_journal_id INTEGER NOT NULL REFERENCES journal(journal_id), PRIMARY KEY (task_id, actor_id)) STRICT, WITHOUT ROWID",
		journalAttributedViewDDL,
	}
	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureJournalSchema: statement %q: %w", stmt, err)
		}
	}

	// Seed journal_kinds from the single source of truth in the journal package,
	// so the SQL lookup and the Go enum can never drift.
	for _, k := range journal.JournalKinds() {
		if err := sqlitex.Execute(db.conn, "INSERT OR IGNORE INTO journal_kinds (id, name) VALUES (?1, ?2)", &sqlitex.ExecOptions{Args: []any{int(k), k.String()}}); err != nil {
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

	bound, release, err := db.bindOperationDB(context.Background())
	if err != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: lease connection: %w", err)
	}
	defer release()
	db = bound

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)

	var journalID int64
	if txErr = sqlitex.Execute(db.conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4)", &sqlitex.ExecOptions{Args: []any{
		int(journal.JournalKindTaskEvent), in.ActorID.String(), recordedAt.UnixNano(), nil,
	}}); txErr != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: insert journal row: %w", txErr)
	}
	journalID = db.conn.LastInsertRowID()

	if txErr = sqlitex.Execute(db.conn,
		insertJournalTaskEventSQL,
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
		if txErr = sqlitex.Execute(db.conn, "INSERT OR IGNORE INTO journal_task_event_contexts\n\t\t\t\t(event_journal_id, context_kind, context_identity, attached_by_journal_id)\n\t\t\t VALUES (?1, ?2, ?3, ?4)", &sqlitex.ExecOptions{Args: []any{journalID, string(kind), identity, journalID}}); txErr != nil {
			return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: insert context edge: %w", txErr)
		}
	}

	// Reducer subtype-integrity gate (§10 rule 8) before the projections.
	if txErr = db.verifySubtypeIntegrityLocked(); txErr != nil {
		return journal.TaskEventRow{}, txErr
	}

	// Projection: first-wins attribution edge for the authoring actor (§8.2).
	if txErr = sqlitex.Execute(db.conn, "INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id)\n\t\t VALUES (?1, ?2, ?3)", &sqlitex.ExecOptions{Args: []any{in.TaskID.String(), in.ActorID.String(), journalID}}); txErr != nil {
		return journal.TaskEventRow{}, fmt.Errorf("AppendTaskEvent: update task_attributions: %w", txErr)
	}

	// Projection: advance the current-task-state watermark if the task exists.
	if txErr = sqlitex.Execute(db.conn, "UPDATE tasks SET last_journal_id = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{journalID, in.TaskID.String()}}); txErr != nil {
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
	bound, release, err := db.bindOperationDB(context.Background())
	if err != nil {
		return 0, fmt.Errorf("AppendBareJournalRow: lease connection: %w", err)
	}
	defer release()
	var txErr error
	endTx := sqlitex.Save(bound.conn)
	defer endTx(&txErr)
	if txErr = sqlitex.Execute(bound.conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4)", &sqlitex.ExecOptions{Args: []any{int(kind), actorID.String(), recordedAt.UTC().UnixNano(), nil}}); txErr != nil {
		return 0, fmt.Errorf("AppendBareJournalRow: %w", txErr)
	}
	return journal.JournalID(bound.conn.LastInsertRowID()), nil
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
func (db *DB) VerifyIntegrity() (err error) {
	bound, release, err := db.bindOperationDB(context.Background())
	if err != nil {
		return fmt.Errorf("VerifyIntegrity: lease connection: %w", err)
	}
	defer release()
	db = bound
	endTx := sqlitex.Save(db.conn)
	defer endTx(&err)
	if err := db.verifyForeignKeyTopologyLocked(); err != nil {
		return err
	}
	if err := db.verifySubtypeIntegrityLocked(); err != nil {
		return err
	}
	if err := db.verifyActorPlacementLocked(); err != nil {
		return err
	}
	// Watermark presence is checked LAST: it is the §8.1 tightening (no un-journaled
	// task row), a whole-database invariant a converged/native database satisfies,
	// whereas the subtype and placement checks above localise a specific injected
	// journal violation. Ordering it last lets the adversarial corpus assert those
	// journal-row violations by their own sentinel without a coexisting legacy task
	// row masking them.
	return db.verifyWatermarkPresenceLocked()
}

func (db *DB) verifyForeignKeyTopologyLocked() error {
	var table, parent string
	var rowID int64
	if err := sqlitex.Execute(db.conn, "PRAGMA foreign_key_check", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
		if table == "" {
			table, rowID, parent = stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnText(2)
		}
		return nil
	}}); err != nil {
		return fmt.Errorf("verify foreign-key topology: %w", err)
	}
	if table != "" {
		return fmt.Errorf("%w: table %s row %d references missing parent %s — where: read-only startup topology preflight; impact: activation stopped before persistent pragmas or writes; fix: restore the missing canonical support/supertype row", journal.ErrSubtypeIntegrity, table, rowID, parent)
	}
	var produced int64
	if err := sqlitex.Execute(db.conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.produced_by_operation_journal_id IS NOT ?1 AND o.journal_id IS ?2 LIMIT ?3", &sqlitex.ExecOptions{Args: []any{nil, nil, 1}, ResultFunc: func(stmt *zs.Stmt) error { produced = stmt.ColumnInt64(0); return nil }}); err != nil {
		return fmt.Errorf("verify operation producer topology: %w", err)
	}
	if produced != 0 {
		return fmt.Errorf("%w: journal row %d references a missing producing operation — where: read-only startup topology preflight; impact: activation stopped before writes; fix: restore its journal_operations anchor", journal.ErrSubtypeIntegrity, produced)
	}
	var orphanEpisode string
	if err := sqlitex.Execute(db.conn, "SELECT e.assignment_id FROM journal_authority_assignment_episodes e LEFT JOIN journal_authority_assignment_transitions t ON t.assignment_id=e.assignment_id WHERE t.journal_id IS ?1 LIMIT ?2", &sqlitex.ExecOptions{Args: []any{nil, 1}, ResultFunc: func(stmt *zs.Stmt) error { orphanEpisode = stmt.ColumnText(0); return nil }}); err != nil {
		return fmt.Errorf("verify assignment episode topology: %w", err)
	}
	if orphanEpisode != "" {
		return fmt.Errorf("%w: assignment episode %q has no transition row — where: read-only startup topology preflight; impact: activation stopped before writes; fix: restore its canonical start transition or remove the spurious episode", journal.ErrSubtypeIntegrity, orphanEpisode)
	}
	return nil
}

// verifyWatermarkPresenceLocked enforces the §8.1 watermark tightening over stored
// rows: every tasks row must carry a last_journal_id (no un-journaled task). It is a
// no-op on an even-older legacy database whose tasks table predates the column entirely
// (that database is not yet migrated; migration adds the column and anchors its rows,
// §13). Returns journal.ErrWatermarkMissing on the first un-anchored row.
func (db *DB) verifyWatermarkPresenceLocked() error {
	present, _, err := db.tasksWatermarkColumnInfoLocked()
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	var badID string
	if err := sqlitex.Execute(db.conn, "SELECT id FROM tasks WHERE last_journal_id IS ?1 LIMIT ?2", &sqlitex.ExecOptions{Args: []any{nil, 1}, ResultFunc: func(stmt *zs.Stmt) error { badID = stmt.ColumnText(0); return nil }}); err != nil {
		return fmt.Errorf("verifyWatermarkPresence: %w", err)
	}
	if badID != "" {
		return fmt.Errorf(
			"%w: task %q has a NULL last_journal_id — where: watermark-presence gate; when: at "+
				"VerifyIntegrity / §15 convergence; impact: the database is not accepted as converged; "+
				"fix: a task is born through the journal fold (Session.Create) carrying its watermark, "+
				"and a legacy row is anchored by MigrateLegacyBaseline — no task row may exist un-journaled",
			journal.ErrWatermarkMissing, badID)
	}
	return nil
}

// verifyActorPlacementLocked enforces the anchor-only actor-placement invariant
// (§2.1, §10 rule 5): a stored actor_id is present iff the row is an anchor
// (produced_by_operation_journal_id IS NULL). It rejects a subordinate row that
// carries an actor (the new-model violation the retired committing-actor-agreement
// rule made structurally impossible on the input path) and, symmetrically, an
// anchor row missing one. It backs the journal CHECK constraint that also enforces
// this, and is the §15 convergence tool's placement guard. Returns
// journal.ErrActorPlacement on any violation.
func (db *DB) verifyActorPlacementLocked() error {
	var (
		badJID   int64
		subord   bool
		violated bool
	)
	if err := sqlitex.Execute(db.conn, "SELECT journal_id, produced_by_operation_journal_id IS NOT ?1\n\t\t FROM journal\n\t\t WHERE (produced_by_operation_journal_id IS NOT ?2 AND actor_id IS NOT ?3)\n\t\t    OR (produced_by_operation_journal_id IS ?4     AND actor_id IS ?5)\n\t\t LIMIT ?6", &sqlitex.ExecOptions{Args: []any{nil, nil, nil, nil, nil, 1}, ResultFunc: func(stmt *zs.Stmt) error {
		violated = true
		badJID = stmt.ColumnInt64(0)
		subord = stmt.ColumnInt64(1) == 1
		return nil
	}}); err != nil {
		return fmt.Errorf("verifyActorPlacement: %w", err)
	}
	if !violated {
		return nil
	}
	if subord {
		return fmt.Errorf(
			"%w: subordinate journal row %d (produced by an operation) carries a stored "+
				"actor_id — where: actor-placement gate; when: before commit / at Open "+
				"convergence; impact: the write is rolled back / the database is not accepted "+
				"as converged; fix: a subordinate row must leave actor_id NULL and derive its "+
				"committing actor from its anchor (§2.1, §8.5)",
			journal.ErrActorPlacement, badJID)
	}
	return fmt.Errorf(
		"%w: anchor journal row %d (produced_by_operation_journal_id NULL) is missing its "+
			"actor_id — where: actor-placement gate; when: before commit / at Open "+
			"convergence; impact: the write is rolled back / the database is not accepted as "+
			"converged; fix: an anchor row must carry the committing actor",
		journal.ErrActorPlacement, badJID)
}

// subtypeAllTables is the closed class-table-inheritance map (§10 rule 8): every
// JournalKind to its subtype table. It is the SINGLE source of truth — the live-present
// probe (subtypeTablesPresent) and every pre-built integrity query below derive from it, so
// a new subtype kind is added in exactly one place. Statically defined (over the former
// per-call literal) per the statically-defined-over-runtime preference; identifiers are
// compile-time literals, never caller input.
type subtypeTable uint8

const (
	subtypeOperations subtypeTable = iota + 1
	subtypeTaskEvents
	subtypeAuthorities
	subtypeDecisions
	subtypeEvidence
)

type authorityIntegrityQuery uint8

const (
	authorityBootstrapMissing authorityIntegrityQuery = iota + 1
	authorityAssignmentMissing
	authorityBootstrapMismatch
	authorityAssignmentMismatch
)

func (query authorityIntegrityQuery) query() string {
	switch query {
	case authorityBootstrapMissing:
		return "SELECT a.journal_id FROM journal_authorities a LEFT JOIN journal_authority_bootstraps d ON d.journal_id=a.journal_id WHERE a.authority_kind_id=?1 AND d.journal_id IS ?2 LIMIT ?3"
	case authorityAssignmentMissing:
		return "SELECT a.journal_id FROM journal_authorities a LEFT JOIN journal_authority_assignment_transitions d ON d.journal_id=a.journal_id WHERE a.authority_kind_id=?1 AND d.journal_id IS ?2 LIMIT ?3"
	case authorityBootstrapMismatch:
		return "SELECT d.journal_id FROM journal_authority_bootstraps d JOIN journal_authorities a ON a.journal_id=d.journal_id WHERE a.authority_kind_id<>?1 LIMIT ?2"
	case authorityAssignmentMismatch:
		return "SELECT d.journal_id FROM journal_authority_assignment_transitions d JOIN journal_authorities a ON a.journal_id=d.journal_id WHERE a.authority_kind_id<>?1 LIMIT ?2"
	default:
		panic("unknown authority integrity query")
	}
}

var subtypeAllTables = map[journal.JournalKind]subtypeTable{
	journal.JournalKindOperation: subtypeOperations,
	journal.JournalKindTaskEvent: subtypeTaskEvents,
	journal.JournalKindAuthority: subtypeAuthorities,
	journal.JournalKindDecision:  subtypeDecisions,
	journal.JournalKindEvidence:  subtypeEvidence,
}

func (table subtypeTable) label() string {
	switch table {
	case subtypeOperations:
		return "journal_operations"
	case subtypeTaskEvents:
		return "journal_task_events"
	case subtypeAuthorities:
		return "journal_authorities"
	case subtypeDecisions:
		return "journal_decisions"
	case subtypeEvidence:
		return "journal_evidence"
	default:
		panic("unknown subtype table")
	}
}

func (table subtypeTable) totalityQuery() string {
	switch table {
	case subtypeOperations:
		return "SELECT j.journal_id FROM journal j LEFT JOIN journal_operations s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3"
	case subtypeTaskEvents:
		return "SELECT j.journal_id FROM journal j LEFT JOIN journal_task_events s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3"
	case subtypeAuthorities:
		return "SELECT j.journal_id FROM journal j LEFT JOIN journal_authorities s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3"
	case subtypeDecisions:
		return "SELECT j.journal_id FROM journal j LEFT JOIN journal_decisions s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3"
	case subtypeEvidence:
		return "SELECT j.journal_id FROM journal j LEFT JOIN journal_evidence s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3"
	default:
		panic("unknown subtype table")
	}
}

func (table subtypeTable) discriminatorQuery() string {
	switch table {
	case subtypeOperations:
		return "SELECT s.journal_id FROM journal_operations s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2"
	case subtypeTaskEvents:
		return "SELECT s.journal_id FROM journal_task_events s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2"
	case subtypeAuthorities:
		return "SELECT s.journal_id FROM journal_authorities s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2"
	case subtypeDecisions:
		return "SELECT s.journal_id FROM journal_decisions s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2"
	case subtypeEvidence:
		return "SELECT s.journal_id FROM journal_evidence s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2"
	default:
		panic("unknown subtype table")
	}
}

// subtypeExclusivityPair is one pre-built cross-subtype exclusivity probe (§10 rule 8):
// a JournalID may appear in at most one subtype table. All C(5,2) pairs over the closed
// subtypeAllTables set are built once at package init, in deterministic kind order.
type subtypeExclusivityPair struct {
	a, b  journal.JournalKind
	query subtypeExclusivityQuery
}

type subtypeExclusivityQuery uint8

const (
	exclusivityOperationTaskEvent subtypeExclusivityQuery = iota + 1
	exclusivityOperationAuthority
	exclusivityOperationDecision
	exclusivityOperationEvidence
	exclusivityTaskEventAuthority
	exclusivityTaskEventDecision
	exclusivityTaskEventEvidence
	exclusivityAuthorityDecision
	exclusivityAuthorityEvidence
	exclusivityDecisionEvidence
)

func (query subtypeExclusivityQuery) query() string {
	switch query {
	case exclusivityOperationTaskEvent:
		return "SELECT a.journal_id FROM journal_operations a JOIN journal_task_events b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityOperationAuthority:
		return "SELECT a.journal_id FROM journal_operations a JOIN journal_authorities b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityOperationDecision:
		return "SELECT a.journal_id FROM journal_operations a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityOperationEvidence:
		return "SELECT a.journal_id FROM journal_operations a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityTaskEventAuthority:
		return "SELECT a.journal_id FROM journal_task_events a JOIN journal_authorities b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityTaskEventDecision:
		return "SELECT a.journal_id FROM journal_task_events a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityTaskEventEvidence:
		return "SELECT a.journal_id FROM journal_task_events a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityAuthorityDecision:
		return "SELECT a.journal_id FROM journal_authorities a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityAuthorityEvidence:
		return "SELECT a.journal_id FROM journal_authorities a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1"
	case exclusivityDecisionEvidence:
		return "SELECT a.journal_id FROM journal_decisions a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1"
	default:
		panic("unknown subtype exclusivity query")
	}
}

var subtypeExclusivityPairs = []subtypeExclusivityPair{
	{journal.JournalKindOperation, journal.JournalKindTaskEvent, exclusivityOperationTaskEvent},
	{journal.JournalKindOperation, journal.JournalKindAuthority, exclusivityOperationAuthority},
	{journal.JournalKindOperation, journal.JournalKindDecision, exclusivityOperationDecision},
	{journal.JournalKindOperation, journal.JournalKindEvidence, exclusivityOperationEvidence},
	{journal.JournalKindTaskEvent, journal.JournalKindAuthority, exclusivityTaskEventAuthority},
	{journal.JournalKindTaskEvent, journal.JournalKindDecision, exclusivityTaskEventDecision},
	{journal.JournalKindTaskEvent, journal.JournalKindEvidence, exclusivityTaskEventEvidence},
	{journal.JournalKindAuthority, journal.JournalKindDecision, exclusivityAuthorityDecision},
	{journal.JournalKindAuthority, journal.JournalKindEvidence, exclusivityAuthorityEvidence},
	{journal.JournalKindDecision, journal.JournalKindEvidence, exclusivityDecisionEvidence},
}

// subtypeTablesPresent narrows the closed subtypeAllTables map to the tables that actually
// exist in the live schema, so this guard extends automatically as later slices add
// operation/authority/decision/evidence tables without weakening the check for the kinds
// present today.
func (db *DB) subtypeTablesPresent() (map[journal.JournalKind]subtypeTable, error) {
	present := make(map[journal.JournalKind]subtypeTable, len(subtypeAllTables))
	for kind, table := range subtypeAllTables {
		var exists bool
		if err := sqlitex.Execute(db.conn,
			"SELECT ?3 FROM sqlite_master WHERE type=?1 AND name=?2",
			&sqlitex.ExecOptions{
				Args:       []any{"table", table.label(), 1},
				ResultFunc: func(*zs.Stmt) error { exists = true; return nil },
			},
		); err != nil {
			return nil, fmt.Errorf("subtypeTablesPresent: probe %q: %w", table.label(), err)
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
		if err := sqlitex.Execute(db.conn, table.totalityQuery(),
			&sqlitex.ExecOptions{
				Args:       []any{int(kind), nil, 1},
				ResultFunc: func(stmt *zs.Stmt) error { missing = stmt.ColumnInt64(0); return nil },
			},
		); err != nil {
			return fmt.Errorf("verifySubtypeIntegrity totality %s: %w", table.label(), err)
		}
		if missing != 0 {
			return fmt.Errorf(
				"%w: journal row %d has kind %s but no matching %s subtype row — "+
					"where: subtype-integrity gate; when: before commit; impact: the "+
					"write is rolled back because class-table inheritance requires "+
					"exactly one subtype row per journal row; fix: write the %s row in "+
					"the same transaction as its journal row",
				journal.ErrSubtypeIntegrity, missing, kind, table.label(), table.label())
		}
		// Discriminator agreement + exclusivity: a subtype row whose journal row
		// carries a different kind_id (or a JournalID present in a foreign
		// subtype table).
		var mismatch int64
		if err := sqlitex.Execute(db.conn, table.discriminatorQuery(),
			&sqlitex.ExecOptions{
				Args:       []any{int(kind), 1},
				ResultFunc: func(stmt *zs.Stmt) error { mismatch = stmt.ColumnInt64(0); return nil },
			},
		); err != nil {
			return fmt.Errorf("verifySubtypeIntegrity agreement %s: %w", table.label(), err)
		}
		if mismatch != 0 {
			return fmt.Errorf(
				"%w: %s carries a row for journal %d whose supertype discriminator is "+
					"not %s — where: subtype-integrity gate; when: before commit; "+
					"impact: the write is rolled back; fix: the subtype table must "+
					"agree with journal.kind_id",
				journal.ErrSubtypeIntegrity, table.label(), mismatch, kind)
		}
	}
	// Exclusivity across subtype tables (§10 rule 8): a JournalID may appear in at
	// most one subtype table. Checked explicitly so a second subtype row whose own
	// discriminator happens to agree is still rejected (the per-table discriminator
	// pass above only rejects a row disagreeing with its supertype).
	if err := db.verifySubtypeExclusivityLocked(tables); err != nil {
		return err
	}
	// Authority-level class-table inheritance (§10 rule 8, second level):
	// journal_authorities → its bootstrap/assignment detail rows.
	return db.verifyAuthorityDetailIntegrityLocked()
}

// verifySubtypeExclusivityLocked rejects a JournalID present in two subtype
// tables at once (§10 rule 8 exclusivity). The subtype PKs are all JournalID, so
// a pairwise existence probe over the present tables is exact.
func (db *DB) verifySubtypeExclusivityLocked(tables map[journal.JournalKind]subtypeTable) error {
	// Walk the pre-built closed pair set, probing only pairs whose BOTH tables are present
	// in the live schema — so the check is the exact subset of pairs the former dynamic
	// double loop covered, with no per-call SQL construction.
	for _, p := range subtypeExclusivityPairs {
		ta, okA := tables[p.a]
		tb, okB := tables[p.b]
		if !okA || !okB {
			continue
		}
		var dup int64
		if err := sqlitex.Execute(db.conn, p.query.query(),
			&sqlitex.ExecOptions{Args: []any{1}, ResultFunc: func(stmt *zs.Stmt) error { dup = stmt.ColumnInt64(0); return nil }},
		); err != nil {
			return fmt.Errorf("verifySubtypeExclusivity %s/%s: %w", ta.label(), tb.label(), err)
		}
		if dup != 0 {
			return fmt.Errorf(
				"%w: journal %d appears in both %s and %s subtype tables — where: subtype-integrity "+
					"gate; when: before commit; impact: the write is rolled back; fix: a journal row "+
					"must have exactly one subtype row selected by its JournalKind",
				journal.ErrSubtypeIntegrity, dup, ta.label(), tb.label())
		}
	}
	return nil
}

// verifyAuthorityDetailIntegrityLocked enforces authority-level discriminator
// agreement (§10 rule 8): a bootstrap authority carries a bootstrap detail row
// and no assignment transition; an assignment authority carries a transition and
// no bootstrap detail.
func (db *DB) verifyAuthorityDetailIntegrityLocked() error {
	var present bool
	if err := sqlitex.Execute(db.conn,
		"SELECT ?3 FROM sqlite_master WHERE type=?1 AND name=?2",
		&sqlitex.ExecOptions{Args: []any{"table", "journal_authorities", 1}, ResultFunc: func(*zs.Stmt) error { present = true; return nil }}); err != nil {
		return fmt.Errorf("verifyAuthorityDetailIntegrity: probe journal_authorities: %w", err)
	}
	if !present {
		return nil
	}
	checks := []struct {
		query authorityIntegrityQuery
		want  int
		label string
	}{
		{authorityBootstrapMissing, 0, "bootstrap authority missing its bootstrap detail"},
		{authorityAssignmentMissing, 1, "assignment authority missing its assignment transition"},
		{authorityBootstrapMismatch, 0, "bootstrap detail on a non-bootstrap authority"},
		{authorityAssignmentMismatch, 1, "assignment transition on a non-assignment authority"},
	}
	for _, c := range checks {
		var bad int64
		args := []any{c.want, 1}
		if c.query == authorityBootstrapMissing || c.query == authorityAssignmentMissing {
			args = []any{c.want, nil, 1}
		}
		if err := sqlitex.Execute(db.conn, c.query.query(),
			&sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *zs.Stmt) error { bad = stmt.ColumnInt64(0); return nil }},
		); err != nil {
			return fmt.Errorf("verifyAuthorityDetailIntegrity %s: %w", c.label, err)
		}
		if bad != 0 {
			return fmt.Errorf(
				"%w: authority %d has a %s — where: subtype-integrity gate (authority level); when: "+
					"before commit; impact: the write is rolled back; fix: an authority's detail row must "+
					"agree with its AuthorityKind",
				journal.ErrSubtypeIntegrity, bad, c.label)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ordered query surface (§8.3, §12)
// ---------------------------------------------------------------------------

// QueryTaskEvents returns one page of task-event rows in the query's order (§12):
// the canonical ascending journal_id (OrderByJournalID) or the readable-timeline
// (recorded_at, journal_id) display order (OrderByRecordedAt, the default for
// display-facing listings). It rejects an unexposed ordering before touching the
// database (§1). The first call (SnapshotMaxJournalID == 0) pins the snapshot to
// the current MAX(journal_id) under BOTH orders; later pages must repeat that
// watermark and pass the previous page's cursor — AfterJournalID under the
// canonical order, or the composite (AfterRecordedAt, AfterJournalID) under the
// timeline order so the walk never skips or duplicates a row across equal
// timestamps or backdated rows.
func (db *DB) QueryTaskEvents(q journal.JournalQueryV1) (page journal.JournalTaskEventPageV1, err error) {
	if err := q.Validate(); err != nil {
		return journal.JournalTaskEventPageV1{}, err
	}

	bound, release, err := db.bindOperationDB(context.Background())
	if err != nil {
		return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: lease connection: %w", err)
	}
	defer release()
	db = bound
	endTx := sqlitex.Save(db.conn)
	defer endTx(&err)

	snapshot := int64(q.SnapshotMaxJournalID)
	if snapshot == 0 {
		if err := sqlitex.Execute(db.conn,
			"SELECT COALESCE(MAX(journal_id), ?1) FROM journal",
			&sqlitex.ExecOptions{Args: []any{0}, ResultFunc: func(stmt *zs.Stmt) error {
				snapshot = stmt.ColumnInt64(0)
				return nil
			}},
		); err != nil {
			return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: snapshot watermark: %w", err)
		}
	}

	timeline := q.OrderBy == journal.OrderByRecordedAt
	queryOrder, afterRecordedAt := taskEventQueryJournalOrder, int64(0)
	if timeline {
		queryOrder = taskEventQueryTimelineOrder
		afterRecordedAt = q.AfterRecordedAt.UTC().UnixNano()
	}
	taskIDs := make([]string, len(q.TaskIDs))
	for i, id := range q.TaskIDs {
		taskIDs[i] = id.String()
	}
	eventKinds := make([]string, len(q.EventKinds))
	for i, kind := range q.EventKinds {
		eventKinds[i] = string(kind)
	}
	contextFilters := make([][2]string, len(q.Contexts))
	for i, context := range q.Contexts {
		kind, identity, err := journal.EncodeStoredEventContext(context)
		if err != nil {
			return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: encode context filter: %w", err)
		}
		contextFilters[i] = [2]string{string(kind), identity}
	}
	taskJSON, err := json.Marshal(taskIDs)
	if err != nil {
		return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: encode task filters: %w", err)
	}
	eventJSON, err := json.Marshal(eventKinds)
	if err != nil {
		return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: encode event filters: %w", err)
	}
	contextJSON, err := json.Marshal(contextFilters)
	if err != nil {
		return journal.JournalTaskEventPageV1{}, fmt.Errorf("QueryTaskEvents: encode context filters: %w", err)
	}
	flag := func(enabled bool) int {
		if enabled {
			return 1
		}
		return 0
	}
	fetch, boundLimit := q.Limit, -1
	if fetch > 0 {
		boundLimit = fetch + 1
	}

	var rows []journal.TaskEventRow
	if err := sqlitex.Execute(db.conn, queryOrder.query(), &sqlitex.ExecOptions{
		Args: []any{snapshot, afterRecordedAt, int64(q.AfterJournalID),
			flag(len(taskIDs) > 0), string(taskJSON), flag(len(eventKinds) > 0), string(eventJSON),
			flag(len(contextFilters) > 0), string(contextJSON), "$[0]", "$[1]", boundLimit, 1},
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

	page = journal.JournalTaskEventPageV1{SnapshotMaxJournalID: journal.JournalID(snapshot)}
	if fetch > 0 && len(rows) > fetch {
		rows = rows[:fetch]
		last := rows[len(rows)-1]
		next := &journal.JournalCursorV1{
			SnapshotMaxJournalID: journal.JournalID(snapshot),
			AfterJournalID:       last.JournalID,
		}
		if timeline {
			// Composite cursor: carry the last row's RecordedAt so the next page
			// resumes at (recorded_at, journal_id) exactly after this row.
			next.AfterRecordedAt = last.RecordedAt
		}
		page.Next = next
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
	if err := sqlitex.Execute(db.conn, "SELECT context_kind, context_identity FROM journal_task_event_contexts\n\t\t WHERE event_journal_id = ?1 ORDER BY context_kind, context_identity", &sqlitex.ExecOptions{
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
	}); err != nil {
		return nil, fmt.Errorf("loadContexts %d: %w", journalID, err)
	}
	return ctxs, nil
}

// ---------------------------------------------------------------------------
// Attribution projection read (§8.2)
// ---------------------------------------------------------------------------

// TaskAttributions returns the cumulative attribution edges for a task in
// ascending FirstJournalID order.
func (db *DB) TaskAttributions(taskID journal.TaskID) (out []journal.TaskAttribution, err error) {
	bound, release, err := db.bindOperationDB(context.Background())
	if err != nil {
		return nil, fmt.Errorf("TaskAttributions: lease connection: %w", err)
	}
	defer release()
	db = bound
	endTx := sqlitex.Save(db.conn)
	defer endTx(&err)
	if err := sqlitex.Execute(db.conn, "SELECT task_id, actor_id, first_journal_id FROM task_attributions\n\t\t WHERE task_id = ?1 ORDER BY first_journal_id ASC", &sqlitex.ExecOptions{
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
	}); err != nil {
		return nil, fmt.Errorf("TaskAttributions %q: %w", taskID.String(), err)
	}
	return out, nil
}
