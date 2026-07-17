package sqlite

import (
	"encoding/json"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// operations.go implements the mutation-time reducer for the operations,
// effects, results, and authority-lifecycle layer
// (docs/journal-relational-contract.md §2-§4, §9, §14). Apply commits one
// logical operation as an atomic append plus domain mutation in a single SQLite
// transaction (§9.5): it folds the operation's effects one at a time in caller
// list order (§9.3.1), authorizing each against the state produced by all
// earlier effects of the same operation (§9.3), and short-circuits an exact
// same-OperationID replay (§9.4). The per-effect validation is written as
// reusable *Locked reducer steps so the Open/replay reducer of a later slice
// folds onto them rather than duplicating a second switch (§9.2).

// Closed-lookup integer ids, matching the Go enum iota values so the SQL lookup
// and the typed enum cannot drift.
const (
	authKindBootstrapID  = int(journal.AuthorityKindBootstrap)
	authKindAssignmentID = int(journal.AuthorityKindAssignment)

	slotOwnerResponsibilityID = 0

	transitionStartedID = int(journal.TransitionStarted)
	transitionEndedID   = int(journal.TransitionEnded)
)

// slotDBID maps a typed AssignmentSlotID to its assignment_slots.id. Only
// owner-responsibility is seeded today (§4.5); an unknown slot is a typed error
// rather than a silent zero.
func slotDBID(slot journal.AssignmentSlotID) (int, error) {
	switch slot {
	case journal.SlotOwnerResponsibility, "":
		return slotOwnerResponsibilityID, nil
	default:
		return 0, fmt.Errorf("provenance: unknown assignment slot %q — only %q is registered (§4.5)",
			slot, journal.SlotOwnerResponsibility)
	}
}

// ---------------------------------------------------------------------------
// Schema (§3, §4) + the staged journal→journal_operations FK completion
// ---------------------------------------------------------------------------

// ensureOperationsSchema creates the operations/authority subtype relations and
// their closed lookups, then completes the deferred journal.produced_by FK the
// journal-base layer staged as NULL (§2.1 staging note, §10 rule 2). Idempotent.
func (db *DB) ensureOperationsSchema() error {
	ddl := []string{
		// Closed lookups (§4.1, §4.5), same shape/BCNF as journal_kinds (§2.2).
		`CREATE TABLE IF NOT EXISTS authority_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS assignment_slots (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS assignment_transitions (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		// Committed operations (§3.1). OperationID is the UNIQUE alternate key,
		// never the PK and never an ordering source (§11). The four-field replay
		// identity (§9.4) is deliberately NOT a UNIQUE constraint here.
		`CREATE TABLE IF NOT EXISTS journal_operations (
			JournalID            INTEGER PRIMARY KEY REFERENCES journal(JournalID),
			operation_id         TEXT NOT NULL UNIQUE,
			authority_journal_id INTEGER REFERENCES journal_authorities(JournalID),
			command_digest       BLOB NOT NULL,
			mutation_digest      BLOB NOT NULL
		) STRICT`,
		// Slot-keyed committed-result mapping (§3.2). rule-9 own-operation
		// integrity is reducer-enforced (the two FKs alone cannot express it).
		`CREATE TABLE IF NOT EXISTS journal_operation_result_slots (
			JournalID           INTEGER NOT NULL REFERENCES journal_operations(JournalID),
			result_slot_id      TEXT NOT NULL,
			produced_journal_id INTEGER NOT NULL REFERENCES journal(JournalID),
			PRIMARY KEY (JournalID, result_slot_id)
		) STRICT, WITHOUT ROWID`,
		// Authority supertype (§4.2) and its bootstrap detail (§4.3).
		`CREATE TABLE IF NOT EXISTS journal_authorities (
			JournalID              INTEGER PRIMARY KEY REFERENCES journal(JournalID),
			authority_kind_id      INTEGER NOT NULL REFERENCES authority_kinds(id),
			operation_authority_id TEXT NOT NULL UNIQUE
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS journal_authority_bootstraps (
			JournalID INTEGER PRIMARY KEY REFERENCES journal_authorities(JournalID),
			label     TEXT NOT NULL
		) STRICT`,
		// Assignment lifecycle BCNF decomposition (§4.4): whole-episode identity
		// separate from per-transition journal rows. PredecessorAssignmentID is
		// UNIQUE — single-consumption evidence (§14.2).
		`CREATE TABLE IF NOT EXISTS journal_authority_assignment_episodes (
			assignment_id             TEXT PRIMARY KEY,
			task_id                   TEXT NOT NULL REFERENCES tasks(id),
			slot_id                   INTEGER NOT NULL REFERENCES assignment_slots(id),
			actor_id                  TEXT NOT NULL REFERENCES agents(id),
			predecessor_assignment_id TEXT UNIQUE REFERENCES journal_authority_assignment_episodes(assignment_id)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS journal_authority_assignment_transitions (
			JournalID     INTEGER PRIMARY KEY REFERENCES journal_authorities(JournalID),
			assignment_id TEXT NOT NULL REFERENCES journal_authority_assignment_episodes(assignment_id),
			transition_id INTEGER NOT NULL REFERENCES assignment_transitions(id),
			UNIQUE (assignment_id, transition_id)
		) STRICT`,
		// Decisions (§6.1) and material-work evidence (§6.2).
		`CREATE TABLE IF NOT EXISTS journal_decisions (
			JournalID     INTEGER PRIMARY KEY REFERENCES journal(JournalID),
			decision_kind TEXT NOT NULL,
			task_id       TEXT REFERENCES tasks(id),
			payload       TEXT NOT NULL CHECK (json_valid(payload))
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS journal_evidence (
			JournalID      INTEGER PRIMARY KEY REFERENCES journal(JournalID),
			evidence_kind  TEXT NOT NULL,
			task_id        TEXT REFERENCES tasks(id),
			content_digest BLOB NOT NULL,
			payload        TEXT NOT NULL CHECK (json_valid(payload))
		) STRICT`,
		// idx_journal_pboj is owned solely by the journal rebuild in
		// completeJournalOperationFK (which drops and recreates the journal table),
		// so it is deliberately NOT created here — creating it in both places would
		// build it, drop it, then rebuild it on a first open (§ single-owner index).
		`CREATE INDEX IF NOT EXISTS idx_transitions_assignment ON journal_authority_assignment_transitions (assignment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_episodes_task ON journal_authority_assignment_episodes (task_id)`,
	}
	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureOperationsSchema: %w — statement: %s", err, stmt[:min(len(stmt), 80)])
		}
	}
	for id, name := range map[int]string{authKindBootstrapID: "bootstrap", authKindAssignmentID: "assignment"} {
		if err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO authority_kinds (id, name) VALUES (?1, ?2)`,
			&sqlitex.ExecOptions{Args: []any{id, name}}); err != nil {
			return fmt.Errorf("ensureOperationsSchema: seed authority_kinds: %w", err)
		}
	}
	if err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO assignment_slots (id, name) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{slotOwnerResponsibilityID, string(journal.SlotOwnerResponsibility)}}); err != nil {
		return fmt.Errorf("ensureOperationsSchema: seed assignment_slots: %w", err)
	}
	for id, name := range map[int]string{transitionStartedID: "started", transitionEndedID: "ended"} {
		if err := sqlitex.Execute(db.conn, `INSERT OR IGNORE INTO assignment_transitions (id, name) VALUES (?1, ?2)`,
			&sqlitex.ExecOptions{Args: []any{id, name}}); err != nil {
			return fmt.Errorf("ensureOperationsSchema: seed assignment_transitions: %w", err)
		}
	}
	return db.completeJournalOperationFK()
}

// completeJournalOperationFK completes the journal.produced_by_operation_journal_id
// foreign key the journal-base layer staged without an FK (§2.1 staging note).
// It rebuilds the journal table (the standard SQLite 12-step table rebuild)
// preserving every child FK that references journal(JournalID), so an
// operation-produced row can no longer name a producing operation that does not
// exist. Idempotent: it is a no-op once the FK is present.
func (db *DB) completeJournalOperationFK() error {
	present, err := db.journalProducedByFKPresent()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	// PRAGMA foreign_keys is a no-op inside a transaction, so toggle it around
	// an explicit rebuild transaction. The journal is empty at first Open, so
	// the row copy is trivial; child tables reference journal(JournalID) by name
	// and remain valid across the drop+rename.
	if err := sqlitex.ExecuteTransient(db.conn, `PRAGMA foreign_keys=OFF`, nil); err != nil {
		return fmt.Errorf("completeJournalOperationFK: disable FK enforcement: %w", err)
	}
	// Restore FK enforcement no matter how the rebuild ends (commit, rollback on a
	// step error, or rollback on a detected violation). PRAGMA foreign_keys=ON is
	// itself a no-op inside a transaction, so it runs here in autocommit after the
	// transaction has already ended.
	defer func() { _ = sqlitex.ExecuteTransient(db.conn, `PRAGMA foreign_keys=ON`, nil) }()
	// Steps 3-9 of the canonical SQLite 12-step table rebuild, up to but NOT
	// including COMMIT. idx_journal_pboj is created here (its single owner).
	rebuild := []string{
		`BEGIN IMMEDIATE`,
		// The journal_attributed view (§8.5) references journal; SQLite cannot rename
		// journal_new→journal while a view points at the (dropped) journal table, so
		// drop it before the rebuild and recreate it (below) once journal exists again.
		`DROP VIEW IF EXISTS journal_attributed`,
		// actor_id stays nullable with the anchor-only CHECK (§2.1, §10 rule 5): the
		// rebuild only completes the produced_by FK, it does not relax the actor
		// placement invariant the journal-base layer already established.
		`CREATE TABLE journal_new (
			JournalID   INTEGER PRIMARY KEY AUTOINCREMENT,
			kind_id     INTEGER NOT NULL REFERENCES journal_kinds(id),
			actor_id    TEXT REFERENCES agents(id),
			recorded_at INTEGER NOT NULL,
			produced_by_operation_journal_id INTEGER REFERENCES journal_operations(JournalID),
			CHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL))
		) STRICT`,
		`INSERT INTO journal_new (JournalID, kind_id, actor_id, recorded_at, produced_by_operation_journal_id)
			SELECT JournalID, kind_id, actor_id, recorded_at, produced_by_operation_journal_id FROM journal`,
		`DROP TABLE journal`,
		`ALTER TABLE journal_new RENAME TO journal`,
		`CREATE INDEX IF NOT EXISTS idx_journal_kind  ON journal (kind_id)`,
		`CREATE INDEX IF NOT EXISTS idx_journal_actor ON journal (actor_id)`,
		`CREATE INDEX IF NOT EXISTS idx_journal_pboj  ON journal (produced_by_operation_journal_id)`,
		// Recreate the §8.5 attribution view now that journal exists again.
		journalAttributedViewDDL,
	}
	for _, stmt := range rebuild {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			_ = sqlitex.ExecuteTransient(db.conn, `ROLLBACK`, nil)
			return fmt.Errorf("completeJournalOperationFK: rebuild step %q: %w", stmt[:min(len(stmt), 40)], err)
		}
	}
	// Step 10 of the canonical rebuild: foreign_key_check runs INSIDE the
	// transaction, before COMMIT, so a detected violation ROLLBACKs the whole
	// rebuild atomically rather than leaving a corrupt journal durably committed.
	// (foreign_key_check IS permitted inside a transaction; only the
	// foreign_keys=ON/OFF toggle is the no-op-inside-a-tx pragma handled above.)
	var violations int
	if err := sqlitex.ExecuteTransient(db.conn, `PRAGMA foreign_key_check`,
		&sqlitex.ExecOptions{ResultFunc: func(*zs.Stmt) error { violations++; return nil }}); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, `ROLLBACK`, nil)
		return fmt.Errorf("completeJournalOperationFK: foreign_key_check: %w", err)
	}
	if violations > 0 {
		_ = sqlitex.ExecuteTransient(db.conn, `ROLLBACK`, nil)
		return fmt.Errorf("completeJournalOperationFK: rebuild left %d foreign-key violations, rolled back — "+
			"where: journal FK completion; impact: the rebuild was reverted and the database left unchanged; "+
			"fix: this indicates a producing operation referenced by a journal row does not exist", violations)
	}
	if err := sqlitex.ExecuteTransient(db.conn, `COMMIT`, nil); err != nil {
		_ = sqlitex.ExecuteTransient(db.conn, `ROLLBACK`, nil)
		return fmt.Errorf("completeJournalOperationFK: commit rebuild: %w", err)
	}
	return nil
}

func (db *DB) journalProducedByFKPresent() (bool, error) {
	present := false
	if err := sqlitex.ExecuteTransient(db.conn, `PRAGMA foreign_key_list(journal)`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error {
			// columns: id, seq, table, from, to, on_update, on_delete, match
			if stmt.ColumnText(3) == "produced_by_operation_journal_id" {
				present = true
			}
			return nil
		}}); err != nil {
		return false, fmt.Errorf("journalProducedByFKPresent: %w", err)
	}
	return present, nil
}

// ---------------------------------------------------------------------------
// Apply — the atomic operation write path (§9)
// ---------------------------------------------------------------------------

// Apply commits one logical operation atomically (§9.5). It first evaluates the
// §9.4 idempotent-replay short-circuit; a new operation then validates genesis
// discipline (§4.6), inserts its anchor, folds its effects in order with
// per-effect authorization (§9.3), persists result slots (§3.2), and runs the
// subtype-integrity and close-ends-assignment gates before commit.
func (db *DB) Apply(in journal.OperationInput) (journal.CommittedResult, error) {
	if err := validateApplyInput(in); err != nil {
		return journal.CommittedResult{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.applyLocked(in, nil)
}

// validateApplyInput runs the pre-transaction input checks shared by the public
// Apply and the internal applyLocked callers (migration, replay).
func validateApplyInput(in journal.OperationInput) error {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return fmt.Errorf("Apply: %w", err)
	}
	if in.ActorID.Namespace == "" {
		return fmt.Errorf("Apply: operation %q: committing actor is required", in.OperationID)
	}
	if len(in.CommandDigest) == 0 || len(in.MutationDigest) == 0 {
		return fmt.Errorf(
			"Apply: operation %q: CommandDigest and MutationDigest are both required (§3.1) — "+
				"where: Apply input validation; impact: nothing committed; fix: supply both opaque digests",
			in.OperationID)
	}
	return nil
}

// applyLocked is the lock-free core of Apply: it assumes db.mu is held and folds
// one operation as a nested SQLite savepoint transaction (§9.5). It is reused by
// migration (which spans many per-task operations in one outer transaction) and
// exposes an optional faultHook so the adversarial corpus can inject a
// crash/cancellation between effects and observe the fail-closed rollback (§9.5) —
// production callers pass nil. faultHook, when non-nil, is invoked after each
// effect index is folded; a non-nil return aborts and rolls back the whole
// operation, committing nothing.
func (db *DB) applyLocked(in journal.OperationInput, faultHook func(effectIndex int) error) (journal.CommittedResult, error) {
	// SAVEPOINT (not BEGIN) so applyLocked composes as a nested transaction when
	// migration folds many per-task operations inside one outer savepoint (§9.5,
	// §13 whole-batch atomicity); standalone it behaves as an ordinary atomic
	// transaction.
	var txErr error
	endTx := sqlitex.Save(db.conn)
	defer endTx(&txErr)

	// §9.4: OperationID-presence short-circuit, evaluated before any
	// operation-kind-specific validity check (genesis rule 6 included, §4.6).
	existing, found, lookErr := db.lookupOperationLocked(in.OperationID)
	if lookErr != nil {
		txErr = lookErr
		return journal.CommittedResult{}, txErr
	}
	if found {
		// A committed row for this OperationID already exists: an exact four-field
		// identity match short-circuits (§9.4), any mismatch is the typed
		// CommittedConflict (§11). Either way no effect is folded and nothing is
		// written; on a conflict txErr is set so the transaction rolls back.
		res, err := db.committedOutcomeForExistingLocked(in, existing)
		if err != nil {
			txErr = err
		}
		return res, err
	}

	// New operation: genesis discipline (§4.6, §10 rules 6-7).
	genesis := in.AuthorityJournalID == nil
	if genesis {
		if err := db.validateGenesisLocked(in); err != nil {
			txErr = err
			return journal.CommittedResult{}, txErr
		}
	} else if err := db.requireAuthorityExistsLocked(*in.AuthorityJournalID); err != nil {
		txErr = err
		return journal.CommittedResult{}, txErr
	}

	// Anchor row (§10 rule 1): kind=operation, PBOJID=NULL.
	anchorJID, err := db.insertJournalRowLocked(journal.JournalKindOperation, in.ActorID, in.RecordedAt, nil)
	if err != nil {
		txErr = err
		return journal.CommittedResult{}, txErr
	}
	if err := db.insertOperationRowLocked(anchorJID, in); err != nil {
		if isUniqueViolation(err) {
			// §9.6 bullet 2 (defense-in-depth): a concurrent writer committed this
			// new OperationID first, so the anchor insert violates
			// journal_operations.OperationID UNIQUE. Translate the raw constraint
			// error into the typed idempotent/conflict outcome the caller is
			// promised — never a raw SQLite error. Unreachable under the in-process
			// db.mu (which serializes Apply end-to-end so the §9.4 lookup above
			// always observes a concurrent writer's committed row first), but
			// honoured for a future multi-connection/multi-process writer.
			res, rErr := db.resolveOperationIDInsertRaceLocked(in)
			txErr = rErr
			return res, rErr
		}
		txErr = err
		return journal.CommittedResult{}, txErr
	}

	// Fold effects in caller list order (§9.3.1), authorizing each against the
	// state produced by all earlier effects of this same operation (§9.3).
	for i := range in.Effects {
		eff := in.Effects[i]
		producedJID, err := db.foldEffectLocked(in, anchorJID, eff, i)
		if err != nil {
			txErr = err
			return journal.CommittedResult{}, txErr
		}
		// Advance the projections through the single shared reducer step Open's
		// full replay also uses (§9.2): one fold, no second switch. Projection is
		// derived from the just-committed row, exactly as replay derives it from a
		// persisted row.
		if err := db.projectJournalRowLocked(producedJID); err != nil {
			txErr = err
			return journal.CommittedResult{}, txErr
		}
		if eff.ResultSlot != "" {
			if err := db.insertResultSlotLocked(anchorJID, eff.ResultSlot, producedJID); err != nil {
				txErr = err
				return journal.CommittedResult{}, txErr
			}
		}
		// Fail-closed atomicity seam (§9.5): an injected fault/cancellation after
		// effect i rolls back every effect 1..i and the anchor as one transaction.
		if faultHook != nil {
			if err := faultHook(i); err != nil {
				txErr = err
				return journal.CommittedResult{}, txErr
			}
		}
	}

	// Post-fold gates: subtype integrity (§10 rule 8), anchor-only actor placement
	// (§2.1, §10 rule 5), and close-ends-assignment (§8.1 / owner_responsibility
	// regression c).
	if txErr = db.verifySubtypeIntegrityLocked(); txErr != nil {
		return journal.CommittedResult{}, txErr
	}
	if txErr = db.verifyActorPlacementLocked(); txErr != nil {
		return journal.CommittedResult{}, txErr
	}
	if txErr = db.validateClosesEndAssignmentsLocked(anchorJID, in.Effects); txErr != nil {
		return journal.CommittedResult{}, txErr
	}

	res, err := db.reconstructCommittedLocked(anchorJID)
	if err != nil {
		txErr = err
		return journal.CommittedResult{}, txErr
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Per-effect fold (§9.3) — the reusable reducer step
// ---------------------------------------------------------------------------

// foldEffectLocked validates and persists one effect, returning its produced
// journal row's JournalID. It enforces anchor-only actor placement (§10 rule 5)
// on the input and dispatches to the sort-specific reducer step, each of which
// authorizes against current transaction state (all earlier effects already
// inserted, §9.3).
func (db *DB) foldEffectLocked(in journal.OperationInput, anchorJID int64, eff journal.Effect, index int) (int64, error) {
	// A subordinate (operation-produced) row carries no stored actor: the committing
	// actor lives once on the anchor and is derived (§2.1, §8.5). Apply therefore
	// rejects any input effect that would stamp an actor on a produced row — the
	// input-side of §10 rule 5, backing the CHECK constraint and VerifyIntegrity guard.
	if eff.ActorID.Namespace != "" {
		return 0, fmt.Errorf(
			"%w: operation %q effect %d supplies a per-row actor %q, but a produced row must not "+
				"carry one — where: per-effect fold (§9.3); when: before commit; impact: nothing "+
				"committed; fix: leave the effect's actor unset; the committing actor is taken once "+
				"from the operation anchor and derived for produced rows (§2.1, §8.5)",
			journal.ErrActorPlacement, in.OperationID, index, eff.ActorID.String())
	}
	kind, err := eff.Sort.JournalKind()
	if err != nil {
		return 0, err
	}
	// RecordedAt is audit/display only (§12); a per-effect override carries an
	// honest legacy timestamp during migration (§13) without affecting order.
	recordedAt := in.RecordedAt
	if eff.RecordedAtOverride != nil {
		recordedAt = *eff.RecordedAtOverride
	}
	jid, err := db.insertJournalRowLocked(kind, in.ActorID, recordedAt, &anchorJID)
	if err != nil {
		return 0, err
	}
	switch eff.Sort {
	case journal.EffectTaskEvent:
		return jid, db.foldTaskEventLocked(in, jid, eff)
	case journal.EffectBootstrapAuthority:
		return jid, db.foldBootstrapAuthorityLocked(jid, eff)
	case journal.EffectAssignmentStart:
		return jid, db.foldAssignmentStartLocked(in, jid, eff)
	case journal.EffectAssignmentEnd:
		return jid, db.foldAssignmentEndLocked(in, jid, eff)
	case journal.EffectDecision:
		return jid, db.foldDecisionLocked(in, jid, eff)
	case journal.EffectEvidence:
		return jid, db.foldEvidenceLocked(in, jid, eff)
	default:
		return 0, fmt.Errorf("Apply: operation %q effect %d has unknown sort %s", in.OperationID, index, eff.Sort)
	}
}

func (db *DB) foldTaskEventLocked(in journal.OperationInput, jid int64, eff journal.Effect) error {
	if err := journal.ValidateEventKind(eff.EventKind); err != nil {
		return err
	}
	if err := db.requireAuthorityGovernsLocked(in, jid, eff.TaskID); err != nil {
		return err
	}
	payload := eff.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return fmt.Errorf("Apply: task_event payload for %q is not valid JSON", eff.EventKind)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_task_events (JournalID, task_id, event_kind, payload) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{jid, eff.TaskID.String(), string(eff.EventKind), string(payload)}}); err != nil {
		return fmt.Errorf("Apply: insert journal_task_events: %w", err)
	}
	contexts, err := journal.CanonicalEventContexts(eff.Contexts)
	if err != nil {
		return fmt.Errorf("Apply: canonical contexts: %w", err)
	}
	for _, ctx := range contexts {
		ck, identity, encErr := journal.EncodeStoredEventContext(ctx)
		if encErr != nil {
			return fmt.Errorf("Apply: encode context: %w", encErr)
		}
		if err := sqlitex.Execute(db.conn,
			`INSERT OR IGNORE INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id)
			 VALUES (?1, ?2, ?3, ?4)`,
			&sqlitex.ExecOptions{Args: []any{jid, string(ck), identity, jid}}); err != nil {
			return fmt.Errorf("Apply: insert context edge: %w", err)
		}
	}
	// Projections (attribution, watermark, and any lifecycle-status transition)
	// are advanced by the shared reducer step projectJournalRowLocked after this
	// row is inserted — the single fold Apply and Open both run (§9.2).
	return nil
}

func (db *DB) foldBootstrapAuthorityLocked(jid int64, eff journal.Effect) error {
	authorityID := string(eff.OperationAuthorityID)
	if authorityID == "" {
		authorityID = fmt.Sprintf("authority--bootstrap--%d", jid)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_authorities (JournalID, authority_kind_id, operation_authority_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{jid, authKindBootstrapID, authorityID}}); err != nil {
		return fmt.Errorf("Apply: insert journal_authorities (bootstrap): %w", err)
	}
	label := eff.BootstrapLabel
	if label == "" {
		label = "bootstrap"
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_authority_bootstraps (JournalID, label) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{jid, label}}); err != nil {
		return fmt.Errorf("Apply: insert journal_authority_bootstraps: %w", err)
	}
	return nil
}

func (db *DB) foldAssignmentStartLocked(in journal.OperationInput, jid int64, eff journal.Effect) error {
	if err := db.requireAuthorityGovernsLocked(in, jid, eff.TaskID); err != nil {
		return err
	}
	occupant := eff.Occupant
	if occupant.Namespace == "" {
		occupant = in.ActorID
	}
	slot, err := slotDBID(eff.SlotID)
	if err != nil {
		return err
	}
	// Orphaned/multiply-consumed predecessor evidence (§14.2, §14.3).
	var predecessor any
	if eff.Predecessor != "" {
		ended, exists, err := db.episodeEndedLocked(eff.Predecessor)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: predecessor episode %q does not exist (§14.3)", journal.ErrOrphanedEvidence, eff.Predecessor)
		}
		if !ended {
			return fmt.Errorf(
				"%w: transfer start names predecessor %q which has no ended transition — "+
					"where: assignment-start fold (§14.3); when: before commit; impact: nothing committed; "+
					"fix: end the predecessor episode in this or an earlier operation before succeeding it",
				journal.ErrOrphanedEvidence, eff.Predecessor)
		}
		predecessor = string(eff.Predecessor)
	}
	// Episode identity row (append-only; created once per AssignmentID). The
	// UNIQUE(predecessor_assignment_id) constraint is the single-consumption
	// backstop (§14.2); a second successor of the same predecessor fails here.
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id)
		 VALUES (?1, ?2, ?3, ?4, ?5)`,
		&sqlitex.ExecOptions{Args: []any{string(eff.AssignmentID), eff.TaskID.String(), slot, occupant.String(), predecessor}}); err != nil {
		if eff.Predecessor != "" && isUniqueViolation(err) {
			return fmt.Errorf("%w: predecessor episode %q is already consumed by another successor (§14.2)",
				journal.ErrOrphanedEvidence, eff.Predecessor)
		}
		return fmt.Errorf("Apply: insert assignment episode %q: %w", eff.AssignmentID, err)
	}
	// The transition row's occupant/owner projection (attribution to the episode
	// occupant, current owner-responsibility recompute) is advanced by the shared
	// reducer step projectJournalRowLocked after this row is inserted (§8.2, §9.2).
	return db.insertAuthorityAssignmentTransitionLocked(jid, eff.AssignmentID, transitionStartedID)
}

func (db *DB) foldAssignmentEndLocked(in journal.OperationInput, jid int64, eff journal.Effect) error {
	// Lifecycle order (§14.4): a started transition must precede the ended one.
	started, err := db.episodeStartedLocked(eff.AssignmentID)
	if err != nil {
		return err
	}
	if !started {
		return fmt.Errorf(
			"%w: episode %q has no started transition, so it cannot be ended — "+
				"where: assignment-end fold (§14.4); when: before commit; impact: nothing committed; "+
				"fix: a started transition must be committed with a strictly smaller JournalID first",
			journal.ErrAssignmentLifecycle, eff.AssignmentID)
	}
	ended, _, err := db.episodeEndedLocked(eff.AssignmentID)
	if err != nil {
		return err
	}
	if ended {
		// A concurrent transfer CAS loser observes the winner's committed ended
		// transition here and is rejected with a typed stale-episode conflict
		// (§9.6), writing nothing.
		return fmt.Errorf(
			"%w: episode %q is already ended — where: assignment-end fold (§9.6 CAS); "+
				"when: before commit; impact: nothing committed for this operation; "+
				"fix: the episode was ended by a concurrent winning transfer; re-read current state and retry",
			journal.ErrStaleEpisode, eff.AssignmentID)
	}
	task, err := db.episodeTaskLocked(eff.AssignmentID)
	if err != nil {
		return err
	}
	if err := db.requireAuthorityGovernsLocked(in, jid, task); err != nil {
		return err
	}
	// The owner-responsibility recompute (cleared when this ends the active owner
	// episode, §8.1) is advanced by the shared reducer step projectJournalRowLocked
	// after this row is inserted (§9.2).
	return db.insertAuthorityAssignmentTransitionLocked(jid, eff.AssignmentID, transitionEndedID)
}

func (db *DB) foldDecisionLocked(in journal.OperationInput, jid int64, eff journal.Effect) error {
	// §9.3 names journal_decisions as a consuming effect: a task-scoped decision is
	// authorized against the operation's authority at this effect's own JournalID,
	// exactly like a task_event. An untasked decision (§6.1 permits a NULL task_id)
	// legitimately skips the governance check.
	var taskID any
	if eff.TaskID.Namespace != "" {
		if err := db.requireAuthorityGovernsLocked(in, jid, eff.TaskID); err != nil {
			return err
		}
		taskID = eff.TaskID.String()
	}
	payload := eff.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_decisions (JournalID, decision_kind, task_id, payload) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{jid, string(eff.DecisionKind), taskID, string(payload)}}); err != nil {
		return fmt.Errorf("Apply: insert journal_decisions: %w", err)
	}
	return nil
}

func (db *DB) foldEvidenceLocked(in journal.OperationInput, jid int64, eff journal.Effect) error {
	// §9.3 names journal_evidence as a consuming effect: a task-scoped evidence row
	// is authorized against the operation's authority at this effect's own
	// JournalID. An untasked evidence row (§6.2 permits a NULL task_id) skips it.
	var taskID any
	if eff.TaskID.Namespace != "" {
		if err := db.requireAuthorityGovernsLocked(in, jid, eff.TaskID); err != nil {
			return err
		}
		taskID = eff.TaskID.String()
	}
	payload := eff.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_evidence (JournalID, evidence_kind, task_id, content_digest, payload) VALUES (?1, ?2, ?3, ?4, ?5)`,
		&sqlitex.ExecOptions{Args: []any{jid, string(eff.EvidenceKind), taskID, eff.ContentDigest, string(payload)}}); err != nil {
		return fmt.Errorf("Apply: insert journal_evidence: %w", err)
	}
	return nil
}
