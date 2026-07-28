package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dayvidpham/provenance/internal/allocation"
	"github.com/dayvidpham/provenance/internal/journal"
)

// operations.go implements the mutation-time reducer for the operations,
// effects, results, and authority-lifecycle layer
// (docs/journal-relational-contract.md §2-§4, §9, §14). Apply commits one
// logical operation as an atomic append plus domain mutation in a single SQLite
// transaction (§9.5): it folds the operation's effects one at a time in caller
// list order (§9.3.1), authorizing each against the state produced by all
// earlier effects of the same operation (§9.3), and short-circuits an exact
// same-OperationID replay (§9.4). The per-effect validation is expressed as
// reusable fold/reducer steps, so the Open/replay reducer folds onto the same
// steps rather than duplicating a second switch (§9.2).

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
func (scope *connScope) ensureOperationsSchema() error {
	ddl := []string{
		"CREATE TABLE IF NOT EXISTS authority_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS assignment_slots (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS assignment_transitions (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		`CREATE TABLE IF NOT EXISTS journal_operations (
			journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), operation_id TEXT NOT NULL UNIQUE,
			authority_journal_id INTEGER REFERENCES journal_authorities(journal_id), command_digest BLOB NOT NULL,
			mutation_digest BLOB NOT NULL, mutation_encoding_version TEXT, canonical_mutation BLOB,
			CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (length(mutation_encoding_version) > 0 AND length(canonical_mutation) > 0))) STRICT`,
		"CREATE TRIGGER IF NOT EXISTS journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version) > 0 AND length(NEW.canonical_mutation) > 0)) BEGIN SELECT RAISE(ABORT, 'invalid canonical mutation version/bytes pair'); END",
		"CREATE TRIGGER IF NOT EXISTS journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version, canonical_mutation ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version) > 0 AND length(NEW.canonical_mutation) > 0)) BEGIN SELECT RAISE(ABORT, 'invalid canonical mutation version/bytes pair'); END",
		"CREATE TABLE IF NOT EXISTS journal_operation_result_slots (journal_id INTEGER NOT NULL REFERENCES journal_operations(journal_id), result_slot_id TEXT NOT NULL, produced_journal_id INTEGER NOT NULL REFERENCES journal(journal_id), PRIMARY KEY (journal_id, result_slot_id)) STRICT, WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS journal_authorities (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), authority_kind_id INTEGER NOT NULL REFERENCES authority_kinds(id), operation_authority_id TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS journal_authority_bootstraps (journal_id INTEGER PRIMARY KEY REFERENCES journal_authorities(journal_id), label TEXT NOT NULL) STRICT",
		"CREATE TABLE IF NOT EXISTS journal_authority_assignment_episodes (assignment_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), slot_id INTEGER NOT NULL REFERENCES assignment_slots(id), actor_id TEXT NOT NULL REFERENCES agents(id), predecessor_assignment_id TEXT UNIQUE REFERENCES journal_authority_assignment_episodes(assignment_id), parent_assignment_id TEXT REFERENCES journal_authority_assignment_episodes(assignment_id)) STRICT",
		"CREATE TABLE IF NOT EXISTS journal_authority_assignment_transitions (journal_id INTEGER PRIMARY KEY REFERENCES journal_authorities(journal_id), assignment_id TEXT NOT NULL REFERENCES journal_authority_assignment_episodes(assignment_id), transition_id INTEGER NOT NULL REFERENCES assignment_transitions(id), UNIQUE (assignment_id, transition_id)) STRICT",
		"CREATE TABLE IF NOT EXISTS journal_decisions (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), decision_kind TEXT NOT NULL, task_id TEXT REFERENCES tasks(id), payload TEXT NOT NULL CHECK (json_valid(payload))) STRICT",
		"CREATE TABLE IF NOT EXISTS journal_evidence (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), evidence_kind TEXT NOT NULL, task_id TEXT REFERENCES tasks(id), content_digest BLOB NOT NULL, payload TEXT NOT NULL CHECK (json_valid(payload))) STRICT",
		"CREATE INDEX IF NOT EXISTS idx_transitions_assignment ON journal_authority_assignment_transitions (assignment_id)",
		"CREATE INDEX IF NOT EXISTS idx_episodes_task ON journal_authority_assignment_episodes (task_id)",
		"CREATE INDEX IF NOT EXISTS idx_episodes_parent ON journal_authority_assignment_episodes (parent_assignment_id)",
	}
	for _, stmt := range ddl {
		if _, err := scope.conn.ExecContext(scope.ctx, stmt); err != nil {
			return fmt.Errorf("ensureOperationsSchema: statement %q: %w", stmt, err)
		}
	}
	for id, name := range map[int]string{authKindBootstrapID: "bootstrap", authKindAssignmentID: "assignment"} {
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO authority_kinds (id, name) VALUES (?1, ?2)", id, name); err != nil {
			return fmt.Errorf("ensureOperationsSchema: seed authority_kinds: %w", err)
		}
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO assignment_slots (id, name) VALUES (?1, ?2)", slotOwnerResponsibilityID, string(journal.SlotOwnerResponsibility)); err != nil {
		return fmt.Errorf("ensureOperationsSchema: seed assignment_slots: %w", err)
	}
	for id, name := range map[int]string{transitionStartedID: "started", transitionEndedID: "ended"} {
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO assignment_transitions (id, name) VALUES (?1, ?2)", id, name); err != nil {
			return fmt.Errorf("ensureOperationsSchema: seed assignment_transitions: %w", err)
		}
	}
	if err := scope.ensureCanonicalMutationColumns(); err != nil {
		return err
	}
	if err := scope.ensureGenericCanonicalConstraints(); err != nil {
		return err
	}
	if err := scope.ensureFactContextRelations(); err != nil {
		return err
	}
	if err := scope.ensureActivityCreationsSchema(); err != nil {
		return err
	}
	if err := scope.completeJournalOperationFK(); err != nil {
		return err
	}
	if err := allocation.EnsureSchema(scope.ctx, allocationSQLTx{conn: scope.conn}); err != nil {
		return fmt.Errorf("ensure governed allocation schema: %w", err)
	}
	return nil
}

// ensureCanonicalMutationColumns upgrades operation rows without rewriting them.
// Existing operations remain explicit legacy opaque records (both columns NULL);
// every operation committed after this migration stores both columns.
func (scope *connScope) ensureCanonicalMutationColumns() error {
	for _, column := range []canonicalOperationColumn{
		canonicalOperationVersionColumn, canonicalOperationBytesColumn,
	} {
		present := false
		if err := scope.queryRows("PRAGMA table_info(journal_operations)", nil, func(rows *sql.Rows) error {
			var cid int
			var name, columnType string
			var notNull, pk int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
				return err
			}
			present = present || name == column.name()
			return nil
		}); err != nil {
			return fmt.Errorf("ensure canonical mutation schema: inspect %s: %w", column.name(), err)
		}
		if !present {
			if _, err := scope.conn.ExecContext(scope.ctx, column.addQuery()); err != nil {
				return fmt.Errorf("ensure canonical mutation schema: add %s: %w", column.name(), err)
			}
		}
	}
	return nil
}

type canonicalOperationColumn uint8

const (
	canonicalOperationVersionColumn canonicalOperationColumn = iota + 1
	canonicalOperationBytesColumn
)

func (column canonicalOperationColumn) name() string {
	switch column {
	case canonicalOperationVersionColumn:
		return "mutation_encoding_version"
	case canonicalOperationBytesColumn:
		return "canonical_mutation"
	default:
		panic("unknown canonical operation column")
	}
}
func (column canonicalOperationColumn) addQuery() string {
	switch column {
	case canonicalOperationVersionColumn:
		return "ALTER TABLE journal_operations ADD COLUMN mutation_encoding_version TEXT"
	case canonicalOperationBytesColumn:
		return "ALTER TABLE journal_operations ADD COLUMN canonical_mutation BLOB"
	default:
		panic("unknown canonical operation column")
	}
}

func (scope *connScope) ensureGenericCanonicalConstraints() error {
	var tableSQL string
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT sql FROM sqlite_master WHERE type=?1 AND name=?2", "table", "journal_operations").Scan(&tableSQL); err != nil {
		return fmt.Errorf("inspect journal_operations constraint: %w", err)
	}
	needsTriggers := false
	if strings.Contains(tableSQL, journal.MutationEncodingV1.String()) {
		needsTriggers = true
		if _, err := scope.conn.ExecContext(scope.ctx, "DROP TRIGGER IF EXISTS journal_operations_canonical_insert"); err != nil {
			return fmt.Errorf("replace V1-specific journal_operations constraint: drop insert trigger: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "DROP TRIGGER IF EXISTS journal_operations_canonical_update"); err != nil {
			return fmt.Errorf("replace V1-specific journal_operations constraint: drop update trigger: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "CREATE TABLE journal_operations_generic (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL,mutation_encoding_version TEXT,canonical_mutation BLOB,CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (length(mutation_encoding_version)>0 AND length(canonical_mutation)>0))) STRICT"); err != nil {
			return fmt.Errorf("replace V1-specific journal_operations constraint: create generic table: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operations_generic SELECT * FROM journal_operations"); err != nil {
			return fmt.Errorf("replace V1-specific journal_operations constraint: copy rows: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "DROP TABLE journal_operations"); err != nil {
			return fmt.Errorf("replace V1-specific journal_operations constraint: drop old table: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "ALTER TABLE journal_operations_generic RENAME TO journal_operations"); err != nil {
			return fmt.Errorf("replace V1-specific journal_operations constraint: rename table: %w", err)
		}
	}
	if !needsTriggers {
		for _, name := range []string{"journal_operations_canonical_insert", "journal_operations_canonical_update"} {
			triggerSQL := ""
			err := scope.conn.QueryRowContext(scope.ctx, "SELECT sql FROM sqlite_master WHERE type=?1 AND name=?2", "trigger", name).Scan(&triggerSQL)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if triggerSQL == "" || strings.Contains(triggerSQL, journal.MutationEncodingV1.String()) {
				needsTriggers = true
				break
			}
		}
	}
	if !needsTriggers {
		return nil
	}
	for _, trigger := range []string{"DROP TRIGGER IF EXISTS journal_operations_canonical_insert", "DROP TRIGGER IF EXISTS journal_operations_canonical_update", "CREATE TRIGGER journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version)>0 AND length(NEW.canonical_mutation)>0)) BEGIN SELECT RAISE(ABORT,'invalid canonical mutation version/bytes pair'); END", "CREATE TRIGGER journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version,canonical_mutation ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version)>0 AND length(NEW.canonical_mutation)>0)) BEGIN SELECT RAISE(ABORT,'invalid canonical mutation version/bytes pair'); END"} {
		if _, err := scope.conn.ExecContext(scope.ctx, trigger); err != nil {
			return fmt.Errorf("install generic canonical constraint trigger: %w", err)
		}
	}
	return nil
}

func (scope *connScope) preflightCanonicalColumnsReadOnly() error {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return canonicalStartupPreflightError(fmt.Errorf("%w: %v", journal.ErrProjectionDivergence, err), "journal_operations canonical column shape could not be read", "SQLite rejected the read-only table-info query: "+err.Error(), "repair the journal_operations schema from a known-good backup, then retry Open")
	}
	_, hasVersion := columns["mutation_encoding_version"]
	_, hasBytes := columns["canonical_mutation"]
	if hasVersion != hasBytes {
		found := "canonical_mutation only"
		if hasVersion {
			found = "mutation_encoding_version only"
		}
		return canonicalStartupPreflightError(journal.ErrProjectionDivergence, "journal_operations has a one-column canonical shape: "+found, "canonical version and bytes are an atomic pair, but exactly one column exists", "restore both mutation_encoding_version and canonical_mutation columns together, or remove both only for a genuine legacy schema")
	}
	if !hasVersion {
		return nil
	}
	malformed, versionState, bytesState := "", "", ""
	var version sql.NullString
	var wire []byte
	var wireIsNull bool
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT operation_id,mutation_encoding_version,canonical_mutation,canonical_mutation IS NULL FROM journal_operations WHERE (mutation_encoding_version IS ?1) != (canonical_mutation IS ?2) OR (mutation_encoding_version IS NOT ?3 AND (NOT length(mutation_encoding_version) OR NOT length(canonical_mutation))) LIMIT ?4", nil, nil, nil, 1).Scan(&malformed, &version, &wire, &wireIsNull)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return canonicalStartupPreflightError(fmt.Errorf("%w: %v", journal.ErrProjectionDivergence, err), "canonical version/bytes pairing could not be inspected", "SQLite rejected the read-only canonical-pair query: "+err.Error(), "repair the journal_operations canonical columns from a known-good backup, then retry Open")
	}
	if err == nil {
		versionState = canonicalColumnState(version.Valid, len(version.String))
		bytesState = canonicalColumnState(!wireIsNull, len(wire))
	}
	if malformed != "" {
		return canonicalStartupPreflightError(journal.ErrProjectionDivergence, fmt.Sprintf("operation %q has malformed canonical pairing (version=%s, bytes=%s)", malformed, versionState, bytesState), "version and bytes must either both be NULL for a legacy row or both be nonempty for a canonical row", "restore both canonical columns from the same committed operation, or set both NULL only if the row is genuinely legacy")
	}
	oversized := ""
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT operation_id FROM journal_operations WHERE canonical_mutation IS NOT ?2 AND length(canonical_mutation)>?1 LIMIT ?3", journal.MaxCanonicalMutationBytes, nil, 1).Scan(&oversized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return canonicalStartupPreflightError(fmt.Errorf("%w: %v", journal.ErrCanonicalMutation, err), "canonical mutation size could not be inspected", "SQLite rejected the read-only canonical-size query: "+err.Error(), "repair journal_operations from a known-good backup, then retry Open")
	}
	if oversized != "" {
		cause := &journal.CanonicalMutationError{Field: "mutation", Reason: fmt.Sprintf("operation %q exceeds maximum %d bytes", oversized, journal.MaxCanonicalMutationBytes), Fix: "restore bounded canonical bytes"}
		return canonicalStartupPreflightError(cause, fmt.Sprintf("operation %q has an oversized canonical mutation", oversized), "the stored wire exceeds the allocation-safe canonical mutation limit", "restore canonical bytes and their digest from a bounded known-good committed backup")
	}
	if err := scope.queryRows("SELECT operation_id,mutation_encoding_version,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT ?1", []any{nil}, func(rows *sql.Rows) error {
		var opID, storedVersion string
		var storedWire []byte
		if err := rows.Scan(&opID, &storedVersion, &storedWire); err != nil {
			return err
		}
		wire := append([]byte(nil), storedWire...)
		wireVersion, err := journal.InspectCanonicalMutationEncodingVersion(wire)
		if err != nil {
			return canonicalStartupPreflightError(err, fmt.Sprintf("operation %q has a malformed canonical wire-version frame", opID), "the canonical bytes do not begin with one valid framed version field", "restore canonical bytes and mutation digest from the same committed backup")
		}
		if !wireVersion.MatchesStoredText(storedVersion) {
			return canonicalStartupPreflightError(journal.ErrProjectionDivergence, fmt.Sprintf("operation %q column version %q differs from wire version (opaque inspected tag)", opID, storedVersion), "the redundant column and framed wire version identify different codecs", "restore mutation_encoding_version, canonical_mutation, and mutation_digest from the same committed operation")
		}
		registeredVersion, supported := wireVersion.RegisteredVersion()
		if !supported || !journal.IsSupportedMutationEncoding(registeredVersion) {
			cause := &journal.CanonicalMutationError{Field: "version", Reason: fmt.Sprintf("unsupported canonical codec version %q for operation %q", storedVersion, opID), Fix: "open with a build that supports this codec or restore bytes written by a supported codec"}
			return canonicalStartupPreflightError(cause, fmt.Sprintf("operation %q uses unsupported canonical codec version %q", opID, storedVersion), "the column and wire agree, but this build has no registered decoder for that version", "upgrade to a codec-capable build, or restore the operation's version, bytes, and digest from a supported backup")
		}
		if _, err := journal.DecodeCanonicalMutation(wire); err != nil {
			return canonicalStartupPreflightError(err, fmt.Sprintf("operation %q has malformed canonical wire for supported version %q", opID, storedVersion), "the full canonical frame is invalid, incomplete, duplicated, or has trailing data", "restore canonical bytes and mutation digest from the same committed operation")
		}
		return nil
	}); err != nil {
		if errors.Is(err, journal.ErrCanonicalMutation) || errors.Is(err, journal.ErrProjectionDivergence) {
			return err
		}
		return canonicalStartupPreflightError(fmt.Errorf("%w: %v", journal.ErrProjectionDivergence, err), "canonical operation rows could not be inspected", "SQLite rejected the read-only canonical operation scan: "+err.Error(), "repair journal_operations from a known-good backup, then retry Open")
	}
	return nil
}

func canonicalColumnState(valid bool, n int) string {
	if !valid {
		return "NULL"
	}
	if n == 0 {
		return "empty"
	}
	return fmt.Sprintf("nonempty(%d bytes)", n)
}

func canonicalStartupPreflightError(cause error, what, why, fix string) error {
	return fmt.Errorf("%w: canonical startup preflight failed — what: %s; why: %s; where: preflightCanonicalColumnsReadOnly on journal_operations; when: read-only startup before schema migration, activation transaction, or WAL enablement; impact: Open fails closed and the caller receives no tracker; no schema, journal, projection, or mode change is written; fix: %s", cause, what, why, fix)
}

// completeJournalOperationFK completes the journal.produced_by_operation_journal_id
// foreign key the journal-base layer staged without an FK (§2.1 staging note).
// It rebuilds the journal table inside the startup transaction owned by Open
// preserving every child FK that references journal(journal_id), so an
// operation-produced row can no longer name a producing operation that does not
// exist. Idempotent: it is a no-op once the FK is present.
func (scope *connScope) completeJournalOperationFK() error {
	present, err := scope.journalProducedByFKPresent()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	// Open disables FK enforcement before beginning its one activation transaction
	// and restores it after commit/rollback. This function must remain composable:
	// it neither starts a nested transaction nor changes connection pragmas.
	if _, err := scope.conn.ExecContext(scope.ctx, "DROP VIEW IF EXISTS journal_attributed"); err != nil {
		return fmt.Errorf("completeJournalOperationFK: drop view: %w", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "CREATE TABLE journal_new (journal_id INTEGER PRIMARY KEY AUTOINCREMENT,kind_id INTEGER NOT NULL REFERENCES journal_kinds(id),actor_id TEXT REFERENCES agents(id),recorded_at INTEGER NOT NULL,produced_by_operation_journal_id INTEGER REFERENCES journal_operations(journal_id),CHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL)),CHECK (kind_id <> 1 OR produced_by_operation_journal_id IS NOT NULL)) STRICT"); err != nil {
		return fmt.Errorf("completeJournalOperationFK: create table: %w", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_new (journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id) SELECT journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id FROM journal"); err != nil {
		return fmt.Errorf("completeJournalOperationFK: copy rows: %w", err)
	}
	for _, stmt := range []string{"DROP TABLE journal", "ALTER TABLE journal_new RENAME TO journal", "CREATE INDEX IF NOT EXISTS idx_journal_kind ON journal (kind_id)", "CREATE INDEX IF NOT EXISTS idx_journal_actor ON journal (actor_id)", "CREATE INDEX IF NOT EXISTS idx_journal_pboj ON journal (produced_by_operation_journal_id)", "CREATE INDEX IF NOT EXISTS idx_journal_recorded_at ON journal (recorded_at, journal_id)", journalAttributedViewDDL} {
		if _, err := scope.conn.ExecContext(scope.ctx, stmt); err != nil {
			return fmt.Errorf("completeJournalOperationFK: rebuild DDL: %w", err)
		}
	}
	// Step 10 of the canonical rebuild: foreign_key_check runs INSIDE the
	// transaction, before COMMIT, so a detected violation ROLLBACKs the whole
	// rebuild atomically rather than leaving a corrupt journal durably committed.
	// (foreign_key_check IS permitted inside a transaction; only the
	// foreign_keys=ON/OFF toggle is the no-op-inside-a-tx pragma handled above.)
	var violations int
	if err := scope.queryRows("PRAGMA foreign_key_check", nil, func(rows *sql.Rows) error {
		violations++
		return nil
	}); err != nil {
		return fmt.Errorf("completeJournalOperationFK: foreign_key_check: %w", err)
	}
	if violations > 0 {
		return fmt.Errorf("completeJournalOperationFK: rebuild left %d foreign-key violations, rolled back — "+
			"where: journal FK completion; impact: the rebuild was reverted and the database left unchanged; "+
			"fix: this indicates a producing operation referenced by a journal row does not exist", violations)
	}
	return nil
}

func (scope *connScope) journalProducedByFKPresent() (bool, error) {
	present := false
	if err := scope.queryRows("PRAGMA foreign_key_list(journal)", nil, func(rows *sql.Rows) error {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return err
		}
		// columns: id, seq, table, from, to, on_update, on_delete, match
		if from == "produced_by_operation_journal_id" {
			present = true
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("journalProducedByFKPresent: %w", err)
	}
	return present, nil
}

// JournalIsEmpty reports whether the global journal holds no rows at all — i.e. no
// genesis bootstrap authority has been established and no legacy baseline migrated
// (§4.6, §13). The Session SDK consults it to turn a journaled mutation against a
// never-initialized journal into an actionable genesis/migration-required error
// rather than a raw authority-not-found rejection deep in Apply. Leases once.
func (db *DB) JournalIsEmpty() (bool, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return false, fmt.Errorf("JournalIsEmpty: lease pooled connection: %w", err)
	}
	defer scope.release()
	var present int
	err = scope.conn.QueryRowContext(scope.ctx, "SELECT ?1 FROM journal LIMIT ?2", 1, 1).Scan(&present)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("JournalIsEmpty: %w", err)
	}
	return errors.Is(err, sql.ErrNoRows), nil
}

// ---------------------------------------------------------------------------
// Apply — the atomic operation write path (§9)
// ---------------------------------------------------------------------------

// Apply commits one logical operation atomically (§9.5). It first evaluates the
// §9.4 idempotent-replay short-circuit; a new operation then validates genesis
// discipline (§4.6), inserts its anchor, folds its effects in order with
// per-effect authorization (§9.3), persists result slots (§3.2), and runs the
// subtype-integrity and close-ends-assignment gates before commit.
func (db *DB) Apply(in journal.OperationInput) (res journal.CommittedResult, err error) {
	in, prepared, callerMutationDigest, err := prepareApplyInput(in)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	return db.applyPreparedOperation(in, prepared, callerMutationDigest, foldOptions{})
}

// prepareApplyInput is the one canonical operation preparation path shared by
// generic Apply and the distinct assignment-transfer entrypoint.
func prepareApplyInput(in journal.OperationInput) (journal.OperationInput, journal.CanonicalMutation, []byte, error) {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return journal.OperationInput{}, journal.CanonicalMutation{}, nil, err
	}
	// Canonicalize before acquiring write ownership or allocating (§9.1).
	// This validates and normalizes conditions and effects; a failure here is a
	// pure input error and nothing has been written.
	callerMutationDigest := append([]byte(nil), in.MutationDigest...)
	prepared, err := journal.Canonicalize(in)
	if err != nil {
		return journal.OperationInput{}, journal.CanonicalMutation{}, nil, fmt.Errorf("Apply: prepare canonical mutation before any write: %w", err)
	}
	// Canonical bytes, not caller assertions, define mutation identity. Execute the
	// decoded normalized values so identity and behavior cannot drift.
	in.Conditions = prepared.NormalizedConditions()
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	if err := validateApplyInput(in); err != nil {
		return journal.OperationInput{}, journal.CanonicalMutation{}, nil, err
	}
	return in, prepared, callerMutationDigest, nil
}

// applyPreparedOperation owns the BEGIN IMMEDIATE transaction shared by all
// canonical operation entrypoints. foldOptions is package-private; public Apply
// always supplies the zero value.
func (db *DB) applyPreparedOperation(in journal.OperationInput, prepared journal.CanonicalMutation, callerMutationDigest []byte, options foldOptions) (res journal.CommittedResult, err error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("Apply: lease pooled connection before write transaction: %w", err)
	}
	defer scope.release()
	// BEGIN IMMEDIATE acquires SQLite write ownership before the OperationID
	// lookup and condition reads. This prevents check-then-act races: two
	// concurrent contenders on a CurrentFact condition both serialize here;
	// the loser observes the winner's committed fact and receives ConditionFailure
	// (not BUSY_SNAPSHOT). Initial BUSY/LOCKED while acquiring this lock is
	// transient infrastructure — the 5 s busy_timeout (set in Open) retries it.
	if transactionErr := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		var foldErr error
		res, foldErr = scope.foldPreparedOperation(in, prepared, callerMutationDigest, options)
		return foldErr
	}); transactionErr != nil {
		// A replay conflict intentionally returns both its closed result variant and
		// its typed error. The transaction rolls back because no new durable state
		// may accompany the conflict, but callers still need its classified axis.
		return res, fmt.Errorf("Apply: acquire, fold, and commit pinned SQLite write transaction: %w", transactionErr)
	}
	return res, nil
}

// validateApplyInput runs the pre-transaction input checks shared by the public
// Apply and the internal foldOperation callers (migration, replay).
func validateApplyInput(in journal.OperationInput) error {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return err
	}
	if in.ActorID.Namespace == "" {
		return fmt.Errorf("Apply: operation %q: committing actor is required", in.OperationID)
	}
	if len(in.CommandDigest) == 0 {
		return fmt.Errorf(
			"Apply: operation %q: CommandDigest is required — where: Apply input validation; "+
				"impact: nothing committed; fix: supply the command provenance digest; the mutation digest is derived from canonical effects",
			in.OperationID)
	}
	return nil
}

func validateExternalApplyOperationID(operationID journal.OperationID) error {
	if err := journal.ValidateExternalOperationID(operationID); err != nil {
		return fmt.Errorf("%w: reserved OperationID %q belongs to the governed-allocation supplemental namespace and cannot enter the generic reducer — why: this identity is reducer-owned, not caller-owned (%v); where: foldPreparedOperation reserved-identity admission; when: before anchor, effect, or DBOS work; impact: zero durable writes and no committed result; fix: supply a fresh caller-owned OperationID and let the composed governed-allocation operation mint and own its supplemental identity", journal.ErrOperationConflict, operationID, err)
	}
	return nil
}

// foldOperation canonicalizes raw operation input and delegates to
// foldPreparedOperation. It assumes the caller exclusively owns scope.conn for
// the whole operation. Migration reuses it for many per-task operations in one
// outer transaction, and it exposes an optional faultHook so the adversarial
// corpus can inject a crash/cancellation between effects and observe the
// fail-closed rollback (§9.5) — production callers pass nil. Public DB.Apply
// canonicalizes before BEGIN IMMEDIATE and calls foldPreparedOperation directly
// after acquiring write ownership. faultHook, when non-nil, is invoked after
// each effect index is folded; a non-nil return aborts and rolls back the whole
// operation, committing nothing.
func (scope *connScope) foldOperation(in journal.OperationInput, faultHook func(effectIndex int) error) (journal.CommittedResult, error) {
	if err := journal.ValidateOperationID(in.OperationID); err != nil {
		return journal.CommittedResult{}, err
	}
	callerMutationDigest := append([]byte(nil), in.MutationDigest...)
	prepared, err := journal.Canonicalize(in)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("Apply: prepare canonical mutation before any write: %w", err)
	}
	in.Conditions = prepared.NormalizedConditions()
	in.Effects = prepared.NormalizedEffects()
	in.MutationDigest = prepared.DerivedDigest()
	return scope.foldPreparedOperation(in, prepared, callerMutationDigest, foldOptions{faultHook: faultHook})
}

// foldPreparedOperation owns the savepoint-based fold. Public DB.Apply calls it
// after BEGIN IMMEDIATE acquires write ownership; foldOperation calls it after
// canonicalizing raw input.
func (scope *connScope) foldPreparedOperation(in journal.OperationInput, prepared journal.CanonicalMutation, callerMutationDigest []byte, options foldOptions) (journal.CommittedResult, error) {
	// SAVEPOINT (not BEGIN) so foldOperation composes as a nested transaction when
	// migration folds many per-task operations inside one outer savepoint (§9.5,
	// §13 whole-batch atomicity); standalone it behaves as an ordinary atomic
	// transaction.
	var txErr error
	if _, txErr = scope.conn.ExecContext(scope.ctx, "SAVEPOINT provenance_fold"); txErr != nil {
		return journal.CommittedResult{}, fmt.Errorf("Apply: start reducer savepoint: %w", txErr)
	}
	defer func() {
		if txErr != nil {
			_, _ = scope.conn.ExecContext(scope.ctx, "ROLLBACK TO SAVEPOINT provenance_fold")
			_, _ = scope.conn.ExecContext(scope.ctx, "RELEASE SAVEPOINT provenance_fold")
			return
		}
		_, _ = scope.conn.ExecContext(scope.ctx, "RELEASE SAVEPOINT provenance_fold")
	}()

	// §9.4: OperationID-presence short-circuit, evaluated before any
	// operation-kind-specific validity check (genesis rule 6 included, §4.6).
	existing, found, lookErr := scope.lookupOperation(in.OperationID)
	if lookErr != nil {
		txErr = lookErr
		return journal.CommittedResult{}, txErr
	}
	if journal.IsReservedInternalOperationID(in.OperationID) {
		var owner string
		ownerErr := scope.conn.QueryRowContext(scope.ctx, `SELECT governed_operation_id FROM governed_composed_supplement_owners WHERE supplement_operation_id=?1`, string(in.OperationID)).Scan(&owner)
		if ownerErr == nil {
			txErr = fmt.Errorf("%w: reserved OperationID %q is already owned by composed governed allocation %q and cannot enter the generic reducer — why: replay authority belongs to its owning composed operation; where: foldPreparedOperation reserved-identity admission; when: before anchor, effect, or DBOS work; impact: zero durable writes and no committed result; fix: retry or inspect the owning composed operation %q instead of submitting its supplemental OperationID directly", journal.ErrOperationConflict, in.OperationID, owner, owner)
			return journal.CommittedResult{}, txErr
		}
		if !errors.Is(ownerErr, sql.ErrNoRows) {
			txErr = fmt.Errorf("Apply: classify reserved operation %q ownership before replay: %w", in.OperationID, ownerErr)
			return journal.CommittedResult{}, txErr
		}
		if !found {
			txErr = validateExternalApplyOperationID(in.OperationID)
			return journal.CommittedResult{}, txErr
		}
	}
	if found {
		// A committed row for this OperationID already exists: an exact four-field
		// identity match short-circuits (§9.4), any mismatch is the typed
		// CommittedConflict (§11). Either way no effect is folded and nothing is
		// written; on a conflict txErr is set so the transaction rolls back.
		res, err := scope.committedOutcomeForExisting(in, existing, callerMutationDigest)
		if err != nil {
			txErr = err
		}
		return res, err
	}
	// The distinct transfer path authenticates predecessor liveness only after
	// replay/conflict admission. Generic Apply always has a nil lease here.
	if options.assignmentTransfer != nil {
		if authErr := scope.authenticateAssignmentTransfer(in, options.assignmentTransfer); authErr != nil {
			txErr = authErr
			return journal.CommittedResult{}, txErr
		}
	}

	// Evaluate pre-conditions inside the write transaction (§9.5).
	// Conditions run after exact-replay lookup (above) and before genesis/effects,
	// so they observe the same transaction snapshot as the effects they gate.
	if condErr := checkConditions(scope, in); condErr != nil {
		txErr = condErr
		return journal.CommittedResult{}, txErr
	}

	// New operation: genesis discipline (§4.6, §10 rules 6-7).
	genesis := in.AuthorityJournalID == nil
	if genesis {
		if err := scope.validateGenesis(in); err != nil {
			txErr = err
			return journal.CommittedResult{}, txErr
		}
	} else if err := scope.requireAuthorityExists(*in.AuthorityJournalID); err != nil {
		txErr = err
		return journal.CommittedResult{}, txErr
	}

	// Anchor row (§10 rule 1): kind=operation, PBOJID=NULL.
	anchorJID, err := scope.insertJournalRow(journal.JournalKindOperation, in.ActorID, in.RecordedAt, nil)
	if err != nil {
		txErr = err
		return journal.CommittedResult{}, txErr
	}
	if err := scope.insertOperationRow(anchorJID, in, prepared); err != nil {
		if isUniqueViolation(err) {
			// §9.6 bullet 2 (defense-in-depth): a concurrent writer committed this
			// new OperationID first, so the anchor insert violates
			// journal_operations.OperationID UNIQUE. Translate the raw constraint
			// error into the typed idempotent/conflict outcome the caller is
			// promised — never a raw SQLite error. BEGIN IMMEDIATE serializes SQLite
			// writers before the §9.4 lookup, while each caller retains its own pool
			// lease for the complete transaction.
			res, rErr := scope.resolveOperationIDInsertRace(in, callerMutationDigest)
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
		producedJID, err := scope.foldEffect(in, anchorJID, eff, i, options.assignmentTransfer)
		if err != nil {
			txErr = err
			return journal.CommittedResult{}, txErr
		}
		// Advance the projections through the single shared reducer step Open's
		// full replay also uses (§9.2): one fold, no second switch. Projection is
		// derived from the just-committed row, exactly as replay derives it from a
		// persisted row.
		if err := scope.projectJournalRow(producedJID); err != nil {
			txErr = err
			return journal.CommittedResult{}, txErr
		}
		if eff.ResultSlot != "" {
			if err := scope.insertResultSlot(anchorJID, eff.ResultSlot, producedJID); err != nil {
				txErr = err
				return journal.CommittedResult{}, txErr
			}
		}
		// Fail-closed atomicity seam (§9.5): an injected fault/cancellation after
		// effect i rolls back every effect 1..i and the anchor as one transaction.
		if options.assignmentTransfer != nil {
			if err := options.assignmentTransfer.recordFoldedEffect(eff, i); err != nil {
				txErr = err
				return journal.CommittedResult{}, txErr
			}
		}
		if options.faultHook != nil {
			if err := options.faultHook(i); err != nil {
				txErr = err
				return journal.CommittedResult{}, txErr
			}
		}
	}
	if options.assignmentTransfer != nil && !options.assignmentTransfer.complete() {
		txErr = assignmentTransferValidationError("lease", "the exact predecessor-end/successor-start closure did not consume its transaction-local lease", "retry through Session.TransferAssignment without altering the canonical operation")
		return journal.CommittedResult{}, txErr
	}

	// Post-fold gates: subtype integrity (§10 rule 8), anchor-only actor placement
	// (§2.1, §10 rule 5), and close-ends-assignment (§8.1 / owner_responsibility
	// regression c).
	if txErr = scope.verifySubtypeIntegrity(); txErr != nil {
		return journal.CommittedResult{}, txErr
	}
	if txErr = scope.verifyActorPlacement(); txErr != nil {
		return journal.CommittedResult{}, txErr
	}
	if txErr = scope.validateClosesEndAssignments(anchorJID, in.Effects); txErr != nil {
		return journal.CommittedResult{}, txErr
	}

	res, err := scope.reconstructAndValidateCommitted(anchorJID)
	if err != nil {
		txErr = err
		return journal.CommittedResult{}, txErr
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Per-effect fold (§9.3) — the reusable reducer step
// ---------------------------------------------------------------------------

// foldEffect validates and persists one effect, returning its produced
// journal row's JournalID. It enforces anchor-only actor placement (§10 rule 5)
// on the input and dispatches to the sort-specific reducer step, each of which
// authorizes against current transaction state (all earlier effects already
// inserted, §9.3).
func (scope *connScope) foldEffect(in journal.OperationInput, anchorJID int64, eff journal.Effect, index int, transferLease *assignmentTransferLease) (int64, error) {
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
	jid, err := scope.insertJournalRow(kind, in.ActorID, recordedAt, &anchorJID)
	if err != nil {
		return 0, err
	}
	switch eff.Sort {
	case journal.EffectTaskCreate, journal.EffectTaskCreateAllocated:
		return jid, scope.foldTaskCreate(in, jid, eff)
	case journal.EffectTaskEvent:
		return jid, scope.foldTaskEvent(in, jid, eff)
	case journal.EffectBootstrapAuthority:
		return jid, scope.foldBootstrapAuthority(jid, eff)
	case journal.EffectAssignmentStart:
		return jid, scope.foldAssignmentStart(in, jid, eff, index, transferLease)
	case journal.EffectAssignmentEnd:
		return jid, scope.foldAssignmentEnd(in, jid, eff)
	case journal.EffectDecision:
		return jid, scope.foldDecision(in, jid, eff)
	case journal.EffectEvidence:
		return jid, scope.foldEvidence(in, jid, eff)
	case journal.EffectEdgeAdd, journal.EffectEdgeRemove,
		journal.EffectLabelAdd, journal.EffectLabelRemove, journal.EffectCommentAdd:
		return jid, scope.foldMutationFamily(in, jid, eff)
	case journal.EffectActivityCreate:
		return jid, scope.foldActivityCreate(in, jid, eff)
	default:
		return 0, fmt.Errorf("Apply: operation %q effect %d has unknown sort %s", in.OperationID, index, eff.Sort)
	}
}

// foldTaskCreate journals the birth of a task (§8.1, §9.3): it authorizes the
// creation against the operation's authority at this effect's own JournalID, INSERTs
// the tasks row (status Open, watermark = this effect's journal id, so the row is
// born with a non-NULL last_journal_id), then writes the provenance.task.created
// journal_task_events row. The tasks INSERT precedes the journal_task_events INSERT
// because journal_task_events.task_id references tasks(id). The shared reducer's
// projectJournalRow (run after the fold) seeds the status projection and the
// creator attribution from this same created event, so Open's from-empty replay
// re-derives the identical projection (§9.2).
func (scope *connScope) foldTaskCreate(in journal.OperationInput, jid int64, eff journal.Effect) error {
	if eff.TaskID.Namespace == "" {
		return fmt.Errorf(
			"%w: operation %q task-create effect has an empty task id/namespace — where: task-create "+
				"fold (§8.1); when: before commit; impact: nothing committed; fix: supply a namespaced TaskID",
			journal.ErrActorPlacement, in.OperationID)
	}
	if !eff.Type.IsValid() || !eff.Priority.IsValid() || !eff.Phase.IsValid() {
		return fmt.Errorf(
			"provenance: operation %q task-create effect for %q has an invalid classification "+
				"(type=%d priority=%d phase=%d) — where: task-create fold (§8.1); when: before commit; "+
				"impact: nothing committed; fix: supply valid TaskType/Priority/Phase enum values",
			in.OperationID, eff.TaskID, int(eff.Type), int(eff.Priority), int(eff.Phase))
	}
	// Authorize the creation against the operation's authority, exactly like a
	// task_event (§9.3): a brand-new task is reached only by a system-root bootstrap
	// authority (an assignment authority governs no task without an episode), so a
	// create under a non-governing authority fails closed with ErrAuthorityScope.
	if err := scope.requireAuthorityGoverns(in, jid, eff.TaskID); err != nil {
		return err
	}
	// Existence guard: creating a task id that already has a row is a typed conflict,
	// not a silent duplicate (the UNIQUE PK would otherwise surface a raw driver error).
	var exists int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT ?2 FROM tasks WHERE id = ?1", eff.TaskID.String(), 1).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("Apply: task-create existence check for %q: %w", eff.TaskID, err)
	}
	if err == nil {
		return fmt.Errorf(
			"provenance: operation %q task-create effect targets task %q which already exists — where: "+
				"task-create fold (§8.1); when: before commit; impact: nothing committed; fix: create a task "+
				"with a fresh id, or mutate the existing task via an update effect",
			in.OperationID, eff.TaskID)
	}
	recordedAt := in.RecordedAt
	if eff.RecordedAtOverride != nil {
		recordedAt = *eff.RecordedAtOverride
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO tasks\n\t\t\t(id, namespace, title, description, status_id, priority_id, type_id,\n\t\t\t phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?13, ?9, ?10, ?11, ?14, ?9, ?12)",
		eff.TaskID.String(), eff.TaskID.Namespace, eff.Title, eff.Description,
		statusOpenID, int(eff.Priority), int(eff.Type), int(eff.Phase),
		"", recordedAt, recordedAt, jid, nil, nil,
	); err != nil {
		return fmt.Errorf("Apply: insert task row for %q: %w", eff.TaskID, err)
	}
	// The created event itself (provenance.task.created), forced to the canonical kind
	// so the status=Open projection is derived from a fixed lifecycle mapping (§8.1).
	payload := eff.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return fmt.Errorf("Apply: task-create payload for %q is not valid JSON", eff.TaskID)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, insertJournalTaskEventSQL, jid, eff.TaskID.String(), string(journal.EventKindTaskCreated), string(payload)); err != nil {
		return fmt.Errorf("Apply: insert journal_task_events (task-create): %w", err)
	}
	contexts, err := journal.CanonicalEventContexts(eff.Contexts)
	if err != nil {
		return fmt.Errorf("Apply: canonical contexts (task-create): %w", err)
	}
	for _, ctx := range contexts {
		ck, identity, encErr := journal.EncodeStoredEventContext(ctx)
		if encErr != nil {
			return fmt.Errorf("Apply: encode context (task-create): %w", encErr)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id)\n\t\t\t VALUES (?1, ?2, ?3, ?4)", jid, string(ck), identity, jid); err != nil {
			return fmt.Errorf("Apply: insert context edge (task-create): %w", err)
		}
	}
	return nil
}

func (scope *connScope) foldTaskEvent(in journal.OperationInput, jid int64, eff journal.Effect) error {
	recordedAt := in.RecordedAt
	if eff.RecordedAtOverride != nil {
		recordedAt = *eff.RecordedAtOverride
	}
	return foldV1TaskEvent(scope.ctx, allocationSQLTx{conn: scope.conn}, in, jid, recordedAt, eff)
}

func (scope *connScope) foldBootstrapAuthority(jid int64, eff journal.Effect) error {
	authorityID := string(eff.OperationAuthorityID)
	if authorityID == "" {
		authorityID = fmt.Sprintf("authority--bootstrap--%d", jid)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, insertJournalAuthoritySQL, jid, authKindBootstrapID, authorityID); err != nil {
		return fmt.Errorf("Apply: insert journal_authorities (bootstrap): %w", err)
	}
	label := eff.BootstrapLabel
	if label == "" {
		label = "bootstrap"
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_bootstraps (journal_id, label) VALUES (?1, ?2)", jid, label); err != nil {
		return fmt.Errorf("Apply: insert journal_authority_bootstraps: %w", err)
	}
	return nil
}

func (scope *connScope) foldAssignmentStart(in journal.OperationInput, jid int64, eff journal.Effect, index int, transferLease *assignmentTransferLease) error {
	if transferLease == nil || !transferLease.permitsSuccessorStart(in, eff, index) {
		if err := scope.requireAuthorityGoverns(in, jid, eff.TaskID); err != nil {
			return err
		}
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
		ended, exists, err := scope.episodeEnded(eff.Predecessor)
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
	// Parent citation (§14.5): the deliberate governance-lineage edge. The cited
	// parent must exist and be ACTIVE at this start transition's own journal
	// position, and the citation must not create a cycle. Validated per-effect at
	// jid, distinct from the predecessor (succession) checks above.
	var parent any
	if eff.Parent != "" {
		if err := scope.requireParentCitationValid(eff.AssignmentID, eff.Parent, jid); err != nil {
			return err
		}
		parent = string(eff.Parent)
	}
	// Episode identity row (append-only; created once per AssignmentID). The
	// UNIQUE(predecessor_assignment_id) constraint is the single-consumption
	// backstop (§14.2); a second successor of the same predecessor fails here.
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6)", string(eff.AssignmentID), eff.TaskID.String(), slot, occupant.String(), predecessor, parent); err != nil {
		if eff.Predecessor != "" && isUniqueViolation(err) {
			return fmt.Errorf("%w: predecessor episode %q is already consumed by another successor (§14.2)",
				journal.ErrOrphanedEvidence, eff.Predecessor)
		}
		return fmt.Errorf("Apply: insert assignment episode %q: %w", eff.AssignmentID, err)
	}
	// The transition row's occupant/owner projection (attribution to the episode
	// occupant, current owner-responsibility recompute) is advanced by the shared
	// reducer step projectJournalRow after this row is inserted (§8.2, §9.2).
	return scope.insertAuthorityAssignmentTransition(jid, eff.AssignmentID, transitionStartedID)
}

func (scope *connScope) foldAssignmentEnd(in journal.OperationInput, jid int64, eff journal.Effect) error {
	// Lifecycle order (§14.4): a started transition must precede the ended one.
	started, err := scope.episodeStarted(eff.AssignmentID)
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
	ended, _, err := scope.episodeEnded(eff.AssignmentID)
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
	task, err := scope.episodeTask(eff.AssignmentID)
	if err != nil {
		return err
	}
	if eff.TaskID.Namespace != "" && eff.TaskID != task {
		return fmt.Errorf("%w: assignment end %q names task %q but the episode belongs to %q — where: assignment-end fold; impact: nothing committed; fix: use the episode's task", journal.ErrAssignmentLifecycle, eff.AssignmentID, eff.TaskID, task)
	}
	if _, err := slotDBID(eff.SlotID); err != nil {
		return err
	}
	if err := scope.requireAuthorityGoverns(in, jid, task); err != nil {
		return err
	}
	// The owner-responsibility recompute (cleared when this ends the active owner
	// episode, §8.1) is advanced by the shared reducer step projectJournalRow
	// after this row is inserted (§9.2).
	return scope.insertAuthorityAssignmentTransition(jid, eff.AssignmentID, transitionEndedID)
}

func (scope *connScope) foldDecision(in journal.OperationInput, jid int64, eff journal.Effect) error {
	// §9.3 names journal_decisions as a consuming effect: a task-scoped decision is
	// authorized against the operation's authority at this effect's own JournalID,
	// exactly like a task_event. An untasked decision (§6.1 permits a NULL task_id)
	// legitimately skips the governance check.
	var taskID any
	if eff.TaskID.Namespace != "" {
		if err := scope.requireAuthorityGoverns(in, jid, eff.TaskID); err != nil {
			return err
		}
		taskID = eff.TaskID.String()
	}
	payload := eff.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?3, ?4)", jid, string(eff.DecisionKind), taskID, string(payload)); err != nil {
		return fmt.Errorf("Apply: insert journal_decisions: %w", err)
	}
	if err := scope.persistFactContexts(factContextDecision, jid, eff.Contexts); err != nil {
		return fmt.Errorf("Apply: persist decision contexts: %w", err)
	}
	return nil
}

func (scope *connScope) foldEvidence(in journal.OperationInput, jid int64, eff journal.Effect) error {
	return foldV1Evidence(scope.ctx, allocationSQLTx{conn: scope.conn}, in, jid, eff)
}
