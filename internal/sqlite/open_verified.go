package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Verified-as-of ledger
// ---------------------------------------------------------------------------
//
// Whole-store verification (VerifyIntegrity plus ReplayProjections) is O(journal):
// reading and folding every journal row and diffing the full derived projection
// set. On a multi-hundred-megabyte store that costs seconds, which is longer
// than a host lifecycle hook's whole invocation deadline, so a store that has
// not changed since it was last verified must not pay for the verification
// again. This ledger is the durable "verified as of" marker that makes the open
// path O(1) in that case.
//
// It exists only where it can pay off: a durable store that is opened more than
// once. Activation of a process-unique in-memory database skips it entirely
// (connScope.ephemeralStore), because there is no earlier verification of a store
// that is re-created on every open and its trigger set could never be read.
//
// It records what the last verification covered, in one durable row:
//
//   - generation: a counter that a write trigger on every table this package's
//     schema owns bumps on any INSERT, UPDATE, or DELETE. It is the store's
//     DML change detector, and it is advanced by the same transaction as the
//     write that changed the store.
//   - journal_id and journal_digest: the journal tail identity at verification
//     time — the highest journal_id and a digest of that row.
//   - shape_id: a digest of this build's closed schema contract (the expected
//     activation objects, the covered-table trigger set, and the seed
//     definitions). A build that changes the shape refuses every ledger written
//     by an older build.
//   - tables_digest: the digest of the covered-table presence set the ledger was
//     written under. A covered table that appears or disappears afterwards
//     changes it, so the ledger stops matching even though DDL advances no
//     generation.
//
// The fast path reads all four in time bounded by the closed schema and not by
// the store — one ledger row, a sqlite_master enumeration, an INTEGER PRIMARY KEY
// maximum, and one row by primary key — and skips the whole-store checks only when
// they still match the store as it stands. The structural half of the currency
// probe (schema objects, closed-lookup seed rows, operations and fact-context
// shapes, the journal spine shape, and trigger coverage) always runs, so DDL damage
// is refused on the fast path exactly as before.
//
// Fail closed. An absent, unreadable, ambiguous, or mismatched ledger, and a
// store whose generation moved during the verification itself, all run the full
// checks exactly as the open path did before the ledger existed, and the ledger
// is re-persisted only once those checks pass. A crash between a write and its
// ledger update leaves the generation ahead of the ledger, which is a mismatch:
// the next open takes the full path, never a skip. The ledger is never a
// correctness shortcut — it is a change detector for a store that has provably
// not changed — and it never replaces VerifyIntegrity, ReplayProjections, or the
// convergence guard the currency probe runs; it only lets them be skipped when
// nothing they cover has moved.
//
// Boundary: the ledger detects *change*, not a deliberate adversary with DDL and
// DML rights, who could equally forge the ledger row or recreate a dropped
// trigger. The gates it protects against are partial writes, out-of-band edits
// by other tools, and damaged stores — the same class §10/§15 already fail
// closed on.

const (
	// storeGenerationTable is the singleton DML change counter.
	storeGenerationTable = "provenance_store_generation"
	// verifiedStoreTable is the singleton verified-as-of ledger row.
	verifiedStoreTable = "provenance_verified_store"
	// verifiedLedgerVersion is bumped whenever the ledger's meaning changes, so
	// a ledger written by an older build is never trusted. It participates in
	// shapeID. v2 adds the recorded covered-table presence set (tables_digest).
	verifiedLedgerVersion = "provenance.verified-store.v2"
	// verifiedTriggerPrefix names the per-table change triggers. Trigger names
	// are derived from the closed covered-table set, never from caller input.
	verifiedTriggerPrefix = "provenance_store_generation_on_"
)

// verifiedTriggerEvents are the SQL-based DML events one covered table needs.
// SQLite takes exactly ONE event per trigger, so a table that must observe every
// DML verb carries one trigger per event. The names are upper case, so the
// derived trigger names for one table stay distinct identifiers and a case-only
// difference cannot collide in SQLite's case-insensitive identifier space.
var verifiedTriggerEvents = []string{"INSERT", "UPDATE", "DELETE"}

// verifiedCoveredTableNames is the closed set of tables whose rows
// VerifyIntegrity or ReplayProjections read, in expectedActivationObjects order.
// It is the single source of the ledger's trigger set: it generates the
// per-table DDL, the coverage check, and the shape id. It is a package-level list
// of literals because every statement this package executes must be decidable
// from source (TestProductionSQLTextIsDecidableFromSource), and
// TestVerifiedLedgerCoveredTablesMatchActivationObjects pins it to the tables
// expectedActivationObjects declares so the two cannot drift. The two ledger
// tables themselves are excluded: this package writes the generation and the
// ledger deliberately, and a trigger on them would make the ledger's own write
// look like a store change.
var verifiedCoveredTableNames = []string{
	"statuses",
	"priorities",
	"task_types",
	"edge_kinds",
	"agent_kinds",
	"providers",
	"roles",
	"phases",
	"stages",
	"ml_models",
	"agents",
	"agents_human",
	"agents_ml",
	"agents_software",
	"tasks",
	"edges",
	"activities",
	"labels",
	"comments",
	"journal_kinds",
	"journal",
	"journal_task_events",
	"journal_task_event_contexts",
	"actor_namespace_claims",
	"fixed_actor_manifest_entries",
	"task_attributions",
	"authority_kinds",
	"assignment_slots",
	"assignment_transitions",
	"journal_operations",
	"journal_operation_result_slots",
	"journal_authorities",
	"journal_authority_bootstraps",
	"journal_authority_assignment_episodes",
	"journal_authority_assignment_transitions",
	"journal_decisions",
	"journal_evidence",
	"journal_decision_contexts",
	"journal_evidence_contexts",
	"journal_activity_creations",
	"governed_allocation_operations",
	"governed_operation_effect_rows",
	"governed_allocation_genesis",
	"governed_composed_supplement_owners",
}

// errVerifiedStoreMovedDuringVerification reports that the store changed while
// the whole-store verification was running, so the ledger it would record would
// describe a state no verification covered. The caller falls back to the full
// activation path, which re-runs the same checks inside its write transaction.
var errVerifiedStoreMovedDuringVerification = errors.New(
	"sqlite: the store changed while the whole-store verification was running; the verified-as-of ledger was not written " +
		"and the open falls back to the full activation path — why: another writer committed during VerifyIntegrity/ReplayProjections, " +
		"so the ledger would describe a state the verification never covered; where: internal/sqlite verified-as-of ledger commit; " +
		"impact: nothing was written; fix: none needed, this is the fail-closed path under concurrent writes")

var (
	// verifiedShape caches this build's shape digest; verifiedShapeOnce guards it.
	verifiedShape     string
	verifiedShapeOnce sync.Once
)

// verifiedGenerationTableDDL, verifiedGenerationSeedDDL, and
// verifiedLedgerTableDDL are the ledger's fixed statements. The per-table change
// triggers are generated in ensureVerifiedLedgerSchema from
// verifiedCoveredTableNames and verifiedTriggerEvents.
const (
	verifiedGenerationTableDDL = `CREATE TABLE IF NOT EXISTS provenance_store_generation (id INTEGER PRIMARY KEY CHECK (id = 1), generation INTEGER NOT NULL) STRICT`
	verifiedGenerationSeedDDL  = `INSERT OR IGNORE INTO provenance_store_generation (id, generation) VALUES (1, 0)`
	verifiedLedgerTableDDL     = `CREATE TABLE IF NOT EXISTS provenance_verified_store (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		generation INTEGER NOT NULL,
		journal_id INTEGER NOT NULL,
		journal_digest TEXT NOT NULL,
		shape_id TEXT NOT NULL,
		tables_digest TEXT NOT NULL,
		verified_count INTEGER NOT NULL,
		verified_at INTEGER NOT NULL) STRICT`
	// verifiedLedgerTablesDigestColumnDDL adds the covered-table presence digest to
	// a ledger table created by a build that predates it. It is idempotent through
	// the column probe in ensureVerifiedLedgerColumns, and the empty default it
	// leaves on an existing row cannot match a live presence digest, so such a
	// store fails closed to the full path rather than trusting the old row.
	verifiedLedgerTablesDigestColumnDDL = `ALTER TABLE provenance_verified_store ADD COLUMN tables_digest TEXT NOT NULL DEFAULT ''`
)

// verifiedStoreIdentity is the O(1) identity of the store as it stands: the DML
// generation, the journal tail id, a digest of that tail row, and the digest of
// the covered-table presence set. The presence digest is what makes the ledger
// self-contained about schema shape: a covered table that appears or disappears
// after the row was written changes this identity, even though DDL advances no
// generation.
type verifiedStoreIdentity struct {
	generation   int64
	journalID    int64
	digest       string
	tablesDigest string
}

// verifiedLedger is one persisted verified-as-of row.
type verifiedLedger struct {
	identity   verifiedStoreIdentity
	shapeID    string
	count      int64
	verifiedAt int64
}

// verifiedTriggerName is the derived name of one covered table's change trigger.
func verifiedTriggerName(table, event string) string {
	return verifiedTriggerPrefix + table + "_" + event
}

// verifiedTriggerNamesFor returns the derived change triggers for one table, in
// verifiedTriggerEvents order. Callers that need to recognise the ledger's own
// triggers on a table (rather than count every trigger on it) use this rather
// than restating the naming scheme.
func verifiedTriggerNamesFor(table string) []string {
	out := make([]string, 0, len(verifiedTriggerEvents))
	for _, event := range verifiedTriggerEvents {
		out = append(out, verifiedTriggerName(table, event))
	}
	return out
}

// ensureVerifiedLedgerSchema creates the ledger and its change triggers on this
// connection. It runs as the last step of activation, after every table rebuild,
// so a rebuild that drops a covered table recreates that table's trigger in the
// same activation rather than leaving coverage incomplete. Trigger bodies advance
// only the generation row, so a covered table's write and its generation advance
// commit together.
func (scope *connScope) ensureVerifiedLedgerSchema() error {
	if scope.ephemeralStore {
		return nil
	}
	for _, statement := range []string{verifiedGenerationTableDDL, verifiedGenerationSeedDDL, verifiedLedgerTableDDL} {
		if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
			return fmt.Errorf("ensureVerifiedLedgerSchema: %q: %w", statement, err)
		}
	}
	if err := scope.ensureVerifiedLedgerColumns(); err != nil {
		return err
	}
	present, err := scope.verifiedCoveredTablesPresent()
	if err != nil {
		return err
	}
	presentSet := make(map[string]struct{}, len(present))
	for _, table := range present {
		presentSet[table] = struct{}{}
	}
	// One change trigger per DML verb, and only for tables that exist. A covered
	// table may be legitimately absent while a caller completes the schema of a
	// store it does not yet own: this step must tolerate that, and must not invent
	// a table it does not own to satisfy its own triggers. The absent table cannot
	// leave a stale ledger either way: its absence fails the currency probe's
	// expected-object check, and the ledger row records the presence set it was
	// written under (see readVerifiedStoreIdentity), so a table that appears or
	// disappears afterwards stops the ledger matching on its own.
	for _, table := range verifiedCoveredTableNames {
		if _, ok := presentSet[table]; !ok {
			continue
		}
		for _, event := range verifiedTriggerEvents {
			statement := fmt.Sprintf(
				`CREATE TRIGGER IF NOT EXISTS provenance_store_generation_on_%s_%s AFTER %s ON %s BEGIN UPDATE provenance_store_generation SET generation = generation + 1 WHERE id = 1; END`,
				table, event, event, table)
			if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
				return fmt.Errorf("ensureVerifiedLedgerSchema: create change trigger for %s on %s: %w", event, table, err)
			}
		}
	}
	return nil
}

// ensureVerifiedLedgerColumns adds the presence-digest column to a ledger table
// created before it existed. It is idempotent through the column probe, and it is
// the only migration this package performs on its own table.
func (scope *connScope) ensureVerifiedLedgerColumns() error {
	columns, err := scope.tableColumns(verifiedStoreTable)
	if err != nil {
		return fmt.Errorf("ensureVerifiedLedgerColumns: read %s columns: %w", verifiedStoreTable, err)
	}
	if _, ok := columns["tables_digest"]; ok {
		return nil
	}
	if _, err := scope.conn.ExecContext(scope.ctx, verifiedLedgerTablesDigestColumnDDL); err != nil {
		return fmt.Errorf("ensureVerifiedLedgerColumns: add tables_digest to %s: %w", verifiedStoreTable, err)
	}
	return nil
}

// verifiedCoveredTablesPresent returns the covered tables that exist right now, in
// a stable order. The ledger records the digest of this set, so a covered table
// appearing or disappearing changes the store's identity and stops the ledger
// matching. An absent covered table is not an error here: see
// ensureVerifiedLedgerSchema.
func (scope *connScope) verifiedCoveredTablesPresent() ([]string, error) {
	covered := make(map[string]struct{}, len(verifiedCoveredTableNames))
	for _, table := range verifiedCoveredTableNames {
		covered[table] = struct{}{}
	}
	present := make([]string, 0, len(verifiedCoveredTableNames))
	err := scope.queryRows("SELECT name FROM sqlite_master WHERE type = 'table'", nil, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, ok := covered[name]; ok {
			present = append(present, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate covered tables: %w", err)
	}
	sort.Strings(present)
	return present, nil
}

// verifiedTablesDigest is the digest of one covered-table presence set. It is
// order-insensitive over the set, so the value depends only on which covered
// tables exist, and a set that gains or loses a table always digests differently.
func verifiedTablesDigest(present []string) string {
	names := append([]string(nil), present...)
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])
}

// verifiedShapeID is the digest of this build's closed schema contract. It is
// computed once per process from the same definitions activation and the
// currency probe use, so a build whose expected objects, seed set, or ledger
// trigger set changes refuses every older ledger.
func verifiedShapeID() string {
	verifiedShapeOnce.Do(func() {
		var b strings.Builder
		b.WriteString(verifiedLedgerVersion)
		b.WriteString("\nobjects")
		for _, object := range expectedActivationObjects {
			fmt.Fprintf(&b, "\n%s\x00%s", object.kind, object.name)
		}
		b.WriteString("\nobsolete")
		for _, name := range obsoleteActivationObjects {
			fmt.Fprintf(&b, "\n%s", name)
		}
		b.WriteString("\ntriggers")
		for _, table := range verifiedCoveredTableNames {
			for _, trigger := range verifiedTriggerNamesFor(table) {
				fmt.Fprintf(&b, "\n%s", trigger)
			}
		}
		b.WriteString("\nseeds")
		for _, seed := range referenceSeeds {
			fmt.Fprintf(&b, "\nkind=%d", seed.kind)
			for id, name := range seed.names {
				fmt.Fprintf(&b, "\n%d\x00%s", id, name)
			}
		}
		sum := sha256.Sum256([]byte(b.String()))
		verifiedShape = hex.EncodeToString(sum[:])
	})
	return verifiedShape
}

// readVerifiedStoreIdentity reads the store's current identity in O(1): the
// generation row, the journal tail id (an INTEGER PRIMARY KEY maximum, so a
// b-tree lookup rather than a scan), and one digest of that tail row.
func (scope *connScope) readVerifiedStoreIdentity() (verifiedStoreIdentity, error) {
	var identity verifiedStoreIdentity
	if err := scope.conn.QueryRowContext(scope.ctx,
		fmt.Sprintf("SELECT generation FROM %s WHERE id = 1", storeGenerationTable)).Scan(&identity.generation); err != nil {
		return verifiedStoreIdentity{}, fmt.Errorf("read store generation: %w", err)
	}
	present, err := scope.verifiedCoveredTablesPresent()
	if err != nil {
		return verifiedStoreIdentity{}, err
	}
	identity.tablesDigest = verifiedTablesDigest(present)
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(MAX(journal_id), 0) FROM journal").Scan(&identity.journalID); err != nil {
		return verifiedStoreIdentity{}, fmt.Errorf("read journal tail id: %w", err)
	}
	if identity.journalID == 0 {
		return identity, nil
	}
	var (
		kindID     int64
		actor      sql.NullString
		recordedAt int64
		producedBy sql.NullInt64
	)
	if err := scope.conn.QueryRowContext(scope.ctx,
		"SELECT kind_id, actor_id, recorded_at, produced_by_operation_journal_id FROM journal WHERE journal_id = ?1",
		identity.journalID).Scan(&kindID, &actor, &recordedAt, &producedBy); err != nil {
		return verifiedStoreIdentity{}, fmt.Errorf("read journal tail row %d: %w", identity.journalID, err)
	}
	identity.digest = journalTailDigest(identity.journalID, kindID, actor.String, recordedAt, producedBy.Int64)
	return identity, nil
}

// journalTailDigest is the canonical digest of one journal tail row.
func journalTailDigest(journalID, kindID int64, actor string, recordedAt, producedBy int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%d\x00%s\x00%d\x00%d", journalID, kindID, actor, recordedAt, producedBy))
	return hex.EncodeToString(sum[:])
}

// readVerifiedLedger reads the persisted ledger row. An absent row reports false.
func (scope *connScope) readVerifiedLedger() (verifiedLedger, bool) {
	var (
		ledger verifiedLedger
		shape  string
	)
	err := scope.conn.QueryRowContext(scope.ctx,
		fmt.Sprintf("SELECT generation, journal_id, journal_digest, shape_id, tables_digest, verified_count, verified_at FROM %s WHERE id = 1", verifiedStoreTable)).
		Scan(&ledger.identity.generation, &ledger.identity.journalID, &ledger.identity.digest, &shape, &ledger.identity.tablesDigest, &ledger.count, &ledger.verifiedAt)
	if err != nil {
		return verifiedLedger{}, false
	}
	ledger.shapeID = shape
	return ledger, true
}

// verifiedLedgerCurrent reports whether the ledger still describes the store as
// it stands right now. Both the ledger row and the store identity are read in
// one transaction, so the comparison is over a single snapshot and cannot mix a
// ledger with a newer (or older) store. Any read error, absent row, or mismatch
// reports not-current, which is the fail-closed direction.
func (scope *connScope) verifiedLedgerCurrent() (bool, error) {
	var current bool
	err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		current = scope.verifiedLedgerMatches()
		return nil
	})
	if err != nil {
		return false, err
	}
	return current, nil
}

// verifiedLedgerMatches is the read-only ledger comparison; the caller owns the
// snapshot (see [connScope.verifiedLedgerCurrent]).
func (scope *connScope) verifiedLedgerMatches() bool {
	ledger, ok := scope.readVerifiedLedger()
	if !ok {
		return false
	}
	if ledger.shapeID != verifiedShapeID() {
		return false
	}
	now, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return false
	}
	return ledger.identity == now
}

// writeVerifiedLedger persists the ledger from inside an enclosing write
// transaction, reading the identity it records from that same transaction so the
// row describes exactly the state the transaction will commit. A ledger that
// already records exactly that state is left alone: an open that changes nothing
// must not start rewriting the database it used to leave untouched. The verified
// count is advanced only by a genuinely new record, which is how a caller (and a
// test) can see that a whole-store verification ran.
func (scope *connScope) writeVerifiedLedger() error {
	if scope.ephemeralStore {
		return nil
	}
	identity, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return fmt.Errorf("writeVerifiedLedger: %w", err)
	}
	shape := verifiedShapeID()
	// A ledger that already records exactly this state needs no write. That is not
	// only an optimisation: activation runs on some stores without changing
	// anything (a structural probe that cannot be satisfied, such as an ml_models
	// count below the caller's registry, keeps sending them down the activation
	// path), and an open of such a store must not start rewriting the database
	// bytes it used to leave untouched.
	if recorded, ok := scope.readVerifiedLedger(); ok && recorded.shapeID == shape && recorded.identity == identity {
		return nil
	}
	if _, err := scope.conn.ExecContext(scope.ctx, fmt.Sprintf(`INSERT INTO %s
		(id, generation, journal_id, journal_digest, shape_id, tables_digest, verified_count, verified_at)
		VALUES (1, ?1, ?2, ?3, ?4, ?5, 1, ?6)
		ON CONFLICT(id) DO UPDATE SET
			generation = excluded.generation,
			journal_id = excluded.journal_id,
			journal_digest = excluded.journal_digest,
			shape_id = excluded.shape_id,
			tables_digest = excluded.tables_digest,
			verified_count = %s.verified_count + 1,
			verified_at = excluded.verified_at`,
		verifiedStoreTable, verifiedStoreTable),
		identity.generation, identity.journalID, identity.digest, shape, identity.tablesDigest, time.Now().UTC().UnixNano()); err != nil {
		return fmt.Errorf("writeVerifiedLedger: %w", err)
	}
	return nil
}

// persistVerifiedLedgerIfUnchanged commits the ledger for a verification that
// just passed outside any transaction, but only when the store still stands
// exactly where it stood when that verification started. The compare-and-set
// runs inside BEGIN IMMEDIATE, so no writer can commit between the comparison
// and the ledger write: a store that moved during the read-only verification is
// never recorded as verified, and the caller falls back to the full activation
// path instead.
func (scope *connScope) persistVerifiedLedgerIfUnchanged(before verifiedStoreIdentity) bool {
	err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		now, err := scope.readVerifiedStoreIdentity()
		if err != nil {
			return err
		}
		if now != before {
			return errVerifiedStoreMovedDuringVerification
		}
		return scope.writeVerifiedLedger()
	})
	return err == nil
}

// activationSchemaIsPristine reports whether the live schema carries exactly the
// objects activation creates: none missing, none extra apart from the ledger's own
// change triggers. The currency probe itself tolerates extra objects, because a
// borrowed file may carry tables the host added. The file-backed read-only
// preflight needs the stricter form: the isolated activation clone it may skip is
// what catches a schema whose NAMES collide (an expected index name re-used by a
// table, an expected index replaced by a view), and a presence-only comparison of
// the expected set cannot see that.
func (scope *connScope) activationSchemaIsPristine() (bool, error) {
	live := make(map[string]struct{}, len(expectedActivationObjects))
	err := scope.queryRows("SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'", nil, func(rows *sql.Rows) error {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return err
		}
		live[kind+"\x00"+name] = struct{}{}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("enumerate schema objects: %w", err)
	}
	ledger := make(map[string]struct{}, len(verifiedCoveredTableNames)*len(verifiedTriggerEvents))
	for _, table := range verifiedCoveredTableNames {
		for _, trigger := range verifiedTriggerNamesFor(table) {
			ledger["trigger\x00"+trigger] = struct{}{}
		}
	}
	for _, object := range expectedActivationObjects {
		key := object.kind + "\x00" + object.name
		if _, ok := live[key]; !ok {
			return false, nil
		}
		delete(live, key)
	}
	for key := range live {
		if _, ok := ledger[key]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// activationLedgerCoverageCurrent reports whether every covered table still
// carries its change trigger. A dropped trigger, or a table rebuilt without
// recreating one, reports not-current so activation reinstalls coverage; a
// trigger on a table this build does not cover is ignored.
//
// Coverage is required for the WHOLE covered set, not only for the tables that
// exist. Creation is conditional (ensureVerifiedLedgerSchema) because a covered
// table may legitimately be absent while a caller completes a store's schema,
// but currency is not: a store missing a covered table answers "not current"
// here as well as at the expected-object check, so it can never take the fast
// path until its schema is complete.
func (scope *connScope) activationLedgerCoverageCurrent() (bool, error) {
	present := make(map[string]struct{}, len(expectedActivationObjects))
	err := scope.queryRows("SELECT name FROM sqlite_master WHERE type = 'trigger'", nil, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		present[name] = struct{}{}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("enumerate triggers: %w", err)
	}
	for _, table := range verifiedCoveredTableNames {
		for _, trigger := range verifiedTriggerNamesFor(table) {
			if _, ok := present[trigger]; !ok {
				return false, nil
			}
		}
	}
	return true, nil
}

// activationSpineShapeCurrent runs the same journal-spine shape preflight
// ReplayProjections runs, which is the only check that refuses a covered table
// whose column set was altered out of band. DDL does not advance the generation,
// so the fast path runs this cheap O(schema) check itself rather than skipping
// it with the O(journal) work.
func (scope *connScope) activationSpineShapeCurrent() (bool, error) {
	if err := scope.preflightSchema(); err != nil {
		return false, nil
	}
	return true, nil
}
