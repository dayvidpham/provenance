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
// again. This ledger is the durable "verified as of" marker that makes an
// unchanged store's open read time bounded by the closed schema instead of the
// journal.
//
// It exists only where it can pay off: a durable store that is opened more than
// once. Activation of a process-unique in-memory database skips it entirely
// (connScope.ephemeralStore), because there is no earlier verification of a store
// that is re-created on every open and its trigger set could never be read.
//
// It records what the last verification covered, in one durable row:
//
//   - generation: the sum of a baseline row and one counter per covered table
//     (provenance_table_generation). A write trigger on every covered table
//     bumps that table's counter on any INSERT, UPDATE, or DELETE, so the sum is
//     the store's DML change detector and advances in the same transaction as
//     the write. The baseline carries the previous build's single counter into
//     the first vector build, which keeps an existing ledger valid.
//   - journal_id and journal_digest: the journal tail identity at verification
//     time — the highest journal_id and a digest of that row.
//   - shape_id: a digest of this build's closed schema contract (the expected
//     activation objects, the covered-table trigger set, the seed definitions,
//     and the identity format). A build that changes the shape refuses every
//     ledger written by an older build.
//   - tables_digest: the digest of the covered-table presence set the ledger was
//     written under. A covered table that appears or disappears afterwards —
//     including one a schema-completion step creates later, such as
//     provenance_id_format — changes it, so the ledger stops matching even though
//     DDL advances no generation.
//
// The fast path reads all four in time bounded by the closed schema and not by
// the store — one ledger row, the closed vector, a sqlite_master enumeration, an
// INTEGER PRIMARY KEY maximum, and one row by primary key — and skips the
// whole-store checks only when they still match the store as it stands. The
// structural half of the currency probe (schema objects, closed-lookup seed
// rows, operations and fact-context shapes, the journal spine shape, and trigger
// coverage) always runs, so DDL damage is refused on the fast path exactly as
// before.
//
// Per-table recovery. When the ledger no longer matches but the journal tail
// still does, the per-table vector says which counters moved. If every moved
// table is one whose whole-store coverage is table-local — the agent registry,
// the closed seed lookups, the identity format, the model registry, the actor
// claim relations — the open verifies exactly those tables and records the same
// proof for the new identity. That is the common case after a direct mutator
// (for example RegisterSoftwareAgent, which writes agents and agents_software
// without touching the journal). Any journal- or projection-derived table in the
// moved set, any changed table presence set, and any torn vector fall through to
// the whole-store path, where the replay convergence guard still refuses damage.
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
// Sanctioned-write induction (v3). A write through applyPreparedOperation or
// ApplyBatchContext that finds the ledger matching its pre-write snapshot, then
// folds its delta through the same shared reducer step a full replay uses and
// runs the store-wide gates over the final state it commits, proves its own
// delta. Such a write advances the ledger to its post-write identity in its own
// commit — one BEGIN IMMEDIATE across the pre-check, the fold, the gates, and
// the advance, so no writer interleaves and a crash leaves both or neither.
// The advance moves the per-table mirror and rewrites the identity fields
// (generation, journal_id, journal_digest, tables_digest, shape_id) and leaves
// verified_count (full verifications) and verified_at (last full verification)
// alone. Every other writer — AppendTaskEvent, the direct mutators, migration,
// governed allocation, or any out-of-band DML/DDL — leaves the ledger stale: the
// next open takes the partial path when the moved tables are table-local and the
// whole-store path otherwise.
//
// Boundary: the ledger detects *change*, not a deliberate adversary with DDL and
// DML rights, who could equally forge the ledger row or recreate a dropped
// trigger. The gates it protects against are partial writes, out-of-band edits
// by other tools, and damaged stores — the same class §10/§15 already fail
// closed on.

const (
	// storeGenerationTable is the previous build's single DML counter. The
	// per-table vector supersedes it at runtime; the row is kept as the seed for
	// the vector baseline so an existing ledger stays valid across the upgrade.
	storeGenerationTable = "provenance_store_generation"
	// verifiedStoreTable is the singleton verified-as-of ledger row.
	verifiedStoreTable = "provenance_verified_store"
	// verifiedLedgerVersion is bumped whenever the ledger's meaning changes, so
	// a ledger written by an older build is never trusted. It participates in
	// shapeID. v2 adds the recorded covered-table presence set (tables_digest).
	// v3 records the induction: a match now means "verified, and every change
	// since was itself verified" — a sanctioned write that proved its own delta
	// advanced the row in its own transaction — so a v2 row fails closed into
	// the full path once rather than being trusted. The per-table vector extends
	// v3 without changing its meaning: the recorded identity is still the same
	// global generation and journal tail, and the vector is ledger machinery
	// outside the shape contract, so a current v3 row is adopted rather than
	// rejected.
	verifiedLedgerVersion = "provenance.verified-store.v3"
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
	"provenance_id_format",
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
	// a ledger table created by the previous build. It is idempotent through the
	// column probe in ensureVerifiedLedgerColumns, and the empty default it leaves
	// on an existing row cannot match a live presence digest, so an upgraded store
	// fails closed to the full path rather than trusting the old row.
	verifiedLedgerTablesDigestColumnDDL = `ALTER TABLE provenance_verified_store ADD COLUMN tables_digest TEXT NOT NULL DEFAULT ''`
)

// verifiedStoreIdentity is the closed-schema identity of the store as it stands:
// the global generation, the per-table counter vector, the journal tail id, a
// digest of that tail row, and the digest of the covered-table presence set. The
// vector distinguishes two tables moving in opposite directions; the presence
// digest is what makes the ledger self-contained about schema shape, so a
// covered table that appears or disappears after the row was written changes the
// identity even though DDL advances no generation.
type verifiedStoreIdentity struct {
	generation   int64
	generations  []int64
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

// ensureVerifiedLedgerSchema creates the ledger, its per-table change vector,
// and its change triggers on this connection. It runs as the last step of
// activation, after every table rebuild, so a rebuild that drops a covered table
// recreates that table's trigger in the same activation rather than leaving
// coverage incomplete. A trigger body advances its table's counter, so a covered
// table's write and its counter advance commit together.
//
// The generation baseline is seeded only into an empty vector. It carries the
// old singleton counter into the first vector build, which is what keeps an
// existing v3 ledger valid across the upgrade; a half-seeded vector must fail
// closed instead of jumping by the old value.
//
// A count-era proof table from this development branch is dropped here, before
// the digest DDL. CREATE TABLE IF NOT EXISTS would leave agent_count in place,
// and a later SELECT agent_digest would fail the open instead of falling back
// to the full probes. The count is not altered into a digest.
func (scope *connScope) ensureVerifiedLedgerSchema() error {
	if scope.ephemeralStore {
		return nil
	}
	if err := scope.dropCountEraVerifiedAgentProof(); err != nil {
		return err
	}
	for _, statement := range []string{
		verifiedGenerationTableDDL, verifiedGenerationSeedDDL, verifiedLedgerTableDDL,
		verifiedTableGenerationDDL, verifiedTableProofDDL, verifiedAgentProofDDL, verifiedAgentMetaDDL, verifiedPrefixDDL,
	} {
		if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
			return fmt.Errorf("ensureVerifiedLedgerSchema: %q: %w", statement, err)
		}
	}
	if err := scope.ensureVerifiedLedgerColumns(); err != nil {
		return err
	}
	// A layout drift — a removed covered table shifts the id-to-name map — is
	// repaired by clearing the whole vector and rebuilding it. Renaming rows in
	// place would collide with the UNIQUE table_name of a row that has not moved
	// yet, and renaming is not enough: the recorded sum must change so the next
	// open takes the full path exactly once.
	drift, err := scope.verifiedVectorLayoutDrifted()
	if err != nil {
		return err
	}
	if drift {
		for _, table := range []string{verifiedTableGenerationTable, verifiedTableProofTable} {
			if _, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM "+table); err != nil {
				return fmt.Errorf("ensureVerifiedLedgerSchema: clear drifted vector %s: %w", table, err)
			}
		}
	}
	// The baseline and table rows are reconciled by name, not only by id. A
	// covered table inserted or removed in the middle of the closed list shifts
	// the id-to-name mapping; the upsert then renames the row and resets its
	// counter, which makes the recorded sum disagree with the ledger and sends
	// the next open down the full path exactly once. Resetting is what lets
	// activation repair a drifted vector instead of failing every open.
	if _, err := scope.conn.ExecContext(scope.ctx,
		"INSERT INTO "+verifiedTableGenerationTable+" (table_id, table_name, generation) VALUES (?1, ?2, COALESCE((SELECT generation FROM "+storeGenerationTable+" WHERE id = 1), 0)) ON CONFLICT(table_id) DO UPDATE SET table_name = excluded.table_name, generation = CASE WHEN "+verifiedTableGenerationTable+".table_name = excluded.table_name THEN "+verifiedTableGenerationTable+".generation ELSE excluded.generation END",
		verifiedBaselineTableID, verifiedBaselineName); err != nil {
		return fmt.Errorf("ensureVerifiedLedgerSchema: seed generation baseline: %w", err)
	}
	for i, table := range verifiedCoveredTableNames {
		if _, err := scope.conn.ExecContext(scope.ctx,
			"INSERT INTO "+verifiedTableGenerationTable+" (table_id, table_name, generation) VALUES (?1, ?2, 0) ON CONFLICT(table_id) DO UPDATE SET table_name = excluded.table_name, generation = CASE WHEN "+verifiedTableGenerationTable+".table_name = excluded.table_name THEN "+verifiedTableGenerationTable+".generation ELSE 0 END",
			int64(i+1), table); err != nil {
			return fmt.Errorf("ensureVerifiedLedgerSchema: seed generation row for %s: %w", table, err)
		}
	}
	if _, err := scope.conn.ExecContext(scope.ctx,
		"DELETE FROM "+verifiedTableGenerationTable+" WHERE table_id < 0 OR table_id > ?1",
		len(verifiedCoveredTableNames)); err != nil {
		return fmt.Errorf("ensureVerifiedLedgerSchema: remove out-of-range generation rows: %w", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx,
		"DELETE FROM "+verifiedTableProofTable+" WHERE table_id < 0 OR table_id > ?1",
		len(verifiedCoveredTableNames)); err != nil {
		return fmt.Errorf("ensureVerifiedLedgerSchema: remove out-of-range verified rows: %w", err)
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
	// table may legitimately be absent: EnsureSchemaInTx completes the schema of a
	// pre-operations legacy store inside the caller's transaction, and
	// provenance_id_format is created later by whichever path owns that ordering
	// (activation's initializeEmptyIDFormat, or the cutover's PublishIDFormatInTx).
	// Completing the schema must not fail, and must not create a table this
	// package does not own at that point. The absent table cannot leave a stale
	// ledger either way: its absence fails the currency probe's expected-object
	// check, and the ledger row records the presence set it was written under
	// (see readVerifiedStoreIdentity), so a table that appears or disappears
	// afterwards stops the ledger matching on its own.
	//
	// A trigger whose stored body is not this build's body is dropped and
	// recreated. That is how a v3 store's singleton-advancing triggers become
	// per-table counters without invalidating the ledger: the shape hashes the
	// trigger names, and the names do not move.
	for tableIndex, table := range verifiedCoveredTableNames {
		if _, ok := presentSet[table]; !ok {
			continue
		}
		for _, event := range verifiedTriggerEvents {
			name := verifiedTriggerName(table, event)
			var stored string
			err := scope.conn.QueryRowContext(scope.ctx,
				"SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'trigger' AND name = ?1", name).Scan(&stored)
			if err == nil && stored == verifiedTriggerDDL(table, event) {
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("ensureVerifiedLedgerSchema: read change trigger %s: %w", name, err)
			}
			// The trigger names and bodies stay decidable from source: the drop
			// and create statements are formatted from the closed covered-table
			// and event lists, exactly as the previous body was.
			dropStatement := fmt.Sprintf("DROP TRIGGER IF EXISTS provenance_store_generation_on_%s_%s", table, event)
			if _, err := scope.conn.ExecContext(scope.ctx, dropStatement); err != nil {
				return fmt.Errorf("ensureVerifiedLedgerSchema: drop obsolete change trigger %s: %w", name, err)
			}
			tableID := tableIndex + 1
			statement := fmt.Sprintf(
				`CREATE TRIGGER provenance_store_generation_on_%s_%s AFTER %s ON %s BEGIN UPDATE %s SET generation = generation + 1 WHERE table_id = %d; END`,
				table, event, event, table, verifiedTableGenerationTable, tableID)
			if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
				return fmt.Errorf("ensureVerifiedLedgerSchema: create change trigger for %s on %s: %w", event, table, err)
			}
		}
	}
	return nil
}

// dropCountEraVerifiedAgentProof removes the development-branch proof tables
// that stored agent_count instead of agent_digest. Both tables go: the set and
// its cardinality were one proof, and a surviving set must not be paired with
// a digest it was not written under. The DDL loop recreates both. Adoption may
// then re-seed them from the current registry; an empty set forces the full
// orphan probes. A released store never has this shape.
func (scope *connScope) dropCountEraVerifiedAgentProof() error {
	legacy, err := scope.countEraVerifiedAgentProof()
	if err != nil {
		return fmt.Errorf("ensureVerifiedLedgerSchema: inspect verified agent proof: %w", err)
	}
	if !legacy {
		return nil
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "DROP TABLE "+verifiedAgentProofTable); err != nil {
		return fmt.Errorf("ensureVerifiedLedgerSchema: drop count-era %s: %w", verifiedAgentProofTable, err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "DROP TABLE "+verifiedAgentMetaTable); err != nil {
		return fmt.Errorf("ensureVerifiedLedgerSchema: drop count-era %s: %w", verifiedAgentMetaTable, err)
	}
	return nil
}

// countEraVerifiedAgentProof reports whether provenance_verified_agent_meta
// exists without an agent_digest column. A missing table is not this shape:
// activationSchemaObjectsCurrent already refuses it.
func (scope *connScope) countEraVerifiedAgentProof() (bool, error) {
	exists, err := scope.tableExists(verifiedAgentMetaTable)
	if err != nil || !exists {
		return false, err
	}
	columns, err := scope.tableColumns(verifiedAgentMetaTable)
	if err != nil {
		return false, err
	}
	_, digest := columns["agent_digest"]
	return !digest, nil
}

// activationVerifiedAgentProofCurrent reports whether the verified-agent proof
// is this build's digest shape. It returns false for the count-era development
// table so activation runs and dropCountEraVerifiedAgentProof can replace it.
// A missing table is already not current through verifiedLedgerObjects.
func (scope *connScope) activationVerifiedAgentProofCurrent() (bool, error) {
	legacy, err := scope.countEraVerifiedAgentProof()
	if err != nil {
		return false, fmt.Errorf("activationVerifiedAgentProofCurrent: %w", err)
	}
	return !legacy, nil
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
// appearing or disappearing — including one a schema-completion step creates
// later, such as provenance_id_format — changes the store's identity and stops the
// ledger matching. An absent covered table is not an error here: see
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
// currency probe use, so a build whose expected objects, seed set, identity
// format, or ledger trigger set changes refuses every older ledger.
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
		fmt.Fprintf(&b, "\nidformat\x00%s", IDFormatSlash)
		sum := sha256.Sum256([]byte(b.String()))
		verifiedShape = hex.EncodeToString(sum[:])
	})
	return verifiedShape
}

// readVerifiedStoreIdentity reads the store's current identity in time bounded
// by the closed covered-table set: the per-table change vector (one baseline row
// plus one row per covered table), the journal tail id (an INTEGER PRIMARY KEY
// maximum, so a b-tree lookup rather than a scan), and one digest of that tail
// row.
func (scope *connScope) readVerifiedStoreIdentity() (verifiedStoreIdentity, error) {
	var identity verifiedStoreIdentity
	vector, err := scope.readTableGenerationVector()
	if err != nil {
		return verifiedStoreIdentity{}, err
	}
	identity.generations = vector
	identity.generation = verifiedGenerationSum(vector)
	present, err := scope.verifiedCoveredTablesPresent()
	if err != nil {
		return verifiedStoreIdentity{}, err
	}
	identity.tablesDigest = verifiedTablesDigest(present)
	journalID, digest, err := scope.readJournalTailIdentity()
	if err != nil {
		return verifiedStoreIdentity{}, err
	}
	identity.journalID = journalID
	identity.digest = digest
	return identity, nil
}

// readVerifiedStoreIdentityLegacy reads the old single-counter identity. It is
// used only by migration adoption, before the per-table vector exists, and by
// the read-only preflight that decides whether activation may adopt instead of
// re-verifying.
func (scope *connScope) readVerifiedStoreIdentityLegacy() (verifiedStoreIdentity, error) {
	var identity verifiedStoreIdentity
	if err := scope.conn.QueryRowContext(scope.ctx,
		"SELECT generation FROM "+storeGenerationTable+" WHERE id = 1").Scan(&identity.generation); err != nil {
		return verifiedStoreIdentity{}, fmt.Errorf("read legacy store generation: %w", err)
	}
	present, err := scope.verifiedCoveredTablesPresent()
	if err != nil {
		return verifiedStoreIdentity{}, err
	}
	identity.tablesDigest = verifiedTablesDigest(present)
	journalID, digest, err := scope.readJournalTailIdentity()
	if err != nil {
		return verifiedStoreIdentity{}, err
	}
	identity.journalID = journalID
	identity.digest = digest
	return identity, nil
}

// readJournalTailIdentity reads the highest journal id and the digest of that
// row. The id is an INTEGER PRIMARY KEY maximum, so this is a b-tree lookup, not
// a scan.
func (scope *connScope) readJournalTailIdentity() (int64, string, error) {
	var journalID int64
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(MAX(journal_id), 0) FROM journal").Scan(&journalID); err != nil {
		return 0, "", fmt.Errorf("read journal tail id: %w", err)
	}
	if journalID == 0 {
		return 0, "", nil
	}
	var (
		kindID     int64
		actor      sql.NullString
		recordedAt int64
		producedBy sql.NullInt64
	)
	if err := scope.conn.QueryRowContext(scope.ctx,
		"SELECT kind_id, actor_id, recorded_at, produced_by_operation_journal_id FROM journal WHERE journal_id = ?1",
		journalID).Scan(&kindID, &actor, &recordedAt, &producedBy); err != nil {
		return 0, "", fmt.Errorf("read journal tail row %d: %w", journalID, err)
	}
	return journalID, journalTailDigest(journalID, kindID, actor.String, recordedAt, producedBy.Int64), nil
}

// journalTailDigest is the canonical digest of one journal tail row.
func journalTailDigest(journalID, kindID int64, actor string, recordedAt, producedBy int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%d\x00%s\x00%d\x00%d", journalID, kindID, actor, recordedAt, producedBy))
	return hex.EncodeToString(sum[:])
}

// readVerifiedLedgerRowOnly reads the persisted ledger row without its
// per-table mirror. Migration adoption uses it before the mirror exists; every
// current-path caller uses readVerifiedLedger.
func (scope *connScope) readVerifiedLedgerRowOnly() (verifiedLedger, bool) {
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

// readVerifiedLedger reads the persisted ledger row and its per-table mirror.
// An absent row, a missing mirror row, or a mirror whose sum disagrees with the
// recorded generation all report false: the fail-closed direction.
func (scope *connScope) readVerifiedLedger() (verifiedLedger, bool) {
	ledger, ok := scope.readVerifiedLedgerRowOnly()
	if !ok {
		return verifiedLedger{}, false
	}
	vector, err := scope.readVerifiedTableVector()
	if err != nil {
		return verifiedLedger{}, false
	}
	if verifiedGenerationSum(vector) != ledger.identity.generation {
		return verifiedLedger{}, false
	}
	ledger.identity.generations = vector
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
	if !ok || !scope.verifiedPrefixUsableForLedger(ledger.identity.journalID) {
		return false
	}
	if ledger.shapeID != verifiedShapeID() {
		return false
	}
	now, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return false
	}
	return identityEqual(ledger.identity, now)
}

// writeVerifiedLedger persists the ledger from inside an enclosing write
// transaction, reading the identity it records from that same transaction so the
// row describes exactly the state the transaction will commit. A ledger that
// already records exactly that state is left alone: an open that changes nothing
// must not start rewriting the database it used to leave untouched. The verified
// count is advanced only by a genuinely new record, which is how a caller (and a
// test) can see that a whole-store verification ran.
func (scope *connScope) writeVerifiedLedger() error {
	return scope.writeVerifiedLedgerWithPrefix(nil)
}

// writeVerifiedLedgerWithPrefix is writeVerifiedLedger for a caller that already
// hashed the verified prefix outside the write lock. prefix must describe the
// rows at or below the journal id this transaction records; nil hashes them here.
// The prefix row commits with the ledger row, so the digests describe exactly
// the state the ledger claims.
func (scope *connScope) writeVerifiedLedgerWithPrefix(prefix *verifiedPrefixDigests) error {
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
	if recorded, ok := scope.readVerifiedLedger(); ok && recorded.shapeID == shape && identityEqual(recorded.identity, identity) {
		// The verification that just passed also proves the prefix at this
		// journal id. A sanctioned write moves the ledger past the recorded
		// prefix without rehashing it, so refresh the cache here rather than
		// leave it behind the ledger; a prefix already at this id is untouched.
		if !scope.verifiedPrefixRowCurrent(identity.journalID) {
			if err := scope.writeVerifiedPrefix(identity.journalID, prefix); err != nil {
				return fmt.Errorf("writeVerifiedLedger: %w", err)
			}
		}
		return nil
	}
	if err := scope.writeVerifiedTableVector(identity.generations); err != nil {
		return fmt.Errorf("writeVerifiedLedger: %w", err)
	}
	if err := scope.writeVerifiedAgentSet(); err != nil {
		return fmt.Errorf("writeVerifiedLedger: %w", err)
	}
	if err := scope.writeVerifiedPrefix(identity.journalID, prefix); err != nil {
		return fmt.Errorf("writeVerifiedLedger: %w", err)
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

// verifiedLedgerEligibleForAdvance reports whether the enclosing write
// transaction may advance the ledger on commit. The caller must invoke it
// inside its own BEGIN IMMEDIATE before the first fold, so the comparison is
// over the transaction's pre-write snapshot: a ledger that matches the store
// as it stands right now, written by this build, over the same covered-table
// presence set and journal tail. Any read error, absent row, or mismatch —
// including a store one out-of-band write has poisoned — reports false, which
// is the fail-closed direction: the write still commits, but the ledger stays
// stale and the next open takes the full path. An ephemeral store is never
// eligible: activation skips the ledger there entirely.
func (scope *connScope) verifiedLedgerEligibleForAdvance() bool {
	if scope.ephemeralStore {
		return false
	}
	return scope.verifiedLedgerMatches()
}

// advanceVerifiedLedger records the post-write identity of a sanctioned
// transaction whose caller proved its own delta. The fold materialised every
// effect through the shared reducer step a full replay uses, and the caller
// verified that delta before asking for the advance.
// The caller must invoke it inside the same BEGIN IMMEDIATE after the fold and
// gates succeed and before COMMIT, so the advance and the delta commit or roll
// back together. It moves the per-table mirror and rewrites the identity fields
// — generation, journal_id, journal_digest, shape_id, tables_digest — and leaves
// verified_count (which counts only whole-store verifications) and verified_at
// (the last full verification) alone. It never inserts: an absent ledger stays
// absent until a full path heals it. It is best-effort by contract: the caller
// ignores its error and commits anyway, leaving the ledger stale (the
// fail-closed state) rather than letting an optimisation veto a durable write.
func (scope *connScope) advanceVerifiedLedger() error {
	if scope.ephemeralStore {
		return nil
	}
	ledger, ok := scope.readVerifiedLedger()
	if !ok {
		// No usable ledger means there is nothing this write can advance; the
		// next open runs the full path and establishes one.
		return nil
	}
	identity, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return fmt.Errorf("advanceVerifiedLedger: %w", err)
	}
	// The vector moves first, the ledger row last. A failure between the two
	// leaves the recorded generation behind the vector, which the next open
	// reads as stale and heals with the full path.
	if err := scope.advanceVerifiedTableVector(identity.generations, ledger.identity.generations); err != nil {
		return fmt.Errorf("advanceVerifiedLedger: %w", err)
	}
	shape := verifiedShapeID()
	if _, err := scope.conn.ExecContext(scope.ctx, fmt.Sprintf(`UPDATE %s SET
		generation = ?1,
		journal_id = ?2,
		journal_digest = ?3,
		shape_id = ?4,
		tables_digest = ?5
		WHERE id = 1`, verifiedStoreTable),
		identity.generation, identity.journalID, identity.digest, shape, identity.tablesDigest); err != nil {
		return fmt.Errorf("advanceVerifiedLedger: %w", err)
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
	// Hash the verified prefix before taking the write lock. The compare-and-set
	// below proves the store still stands at `before`, so digests computed for
	// before.journalID describe the state the ledger records; a store that moved
	// is refused and the digests are discarded. A prefix that cannot be hashed is
	// left to the write, which then records no cache.
	var prefix *verifiedPrefixDigests
	if !scope.ephemeralStore {
		if digests, err := scope.computeVerifiedPrefixDigests(before.journalID); err == nil {
			prefix = &digests
		}
	}
	err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		now, err := scope.readVerifiedStoreIdentity()
		if err != nil {
			return err
		}
		if !identityEqual(now, before) {
			return errVerifiedStoreMovedDuringVerification
		}
		return scope.writeVerifiedLedgerWithPrefix(prefix)
	})
	return err == nil
}

// persistVerifiedChangedTablesIfUnchanged records a partial verification: the
// journal tail still matched the ledger, and the only tables that moved were
// table-local ones whose checks the caller ran. It advances the as-of mirror for
// exactly those rows and rewrites the identity, preserving verified_count and
// verified_at (a partial check is not a whole-store verification). The
// compare-and-set is the same one the full path uses: a store that moved during
// the checks is never recorded.
func (scope *connScope) persistVerifiedChangedTablesIfUnchanged(before verifiedStoreIdentity) bool {
	err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		now, err := scope.readVerifiedStoreIdentity()
		if err != nil {
			return err
		}
		if !identityEqual(now, before) {
			return errVerifiedStoreMovedDuringVerification
		}
		ledger, ok := scope.readVerifiedLedger()
		if !ok || !scope.verifiedPrefixUsableForLedger(ledger.identity.journalID) {
			return errVerifiedStoreMovedDuringVerification
		}
		if err := scope.advanceVerifiedTableVector(now.generations, ledger.identity.generations); err != nil {
			return fmt.Errorf("persist partial verification: %w", err)
		}
		if err := scope.writeVerifiedAgentSet(); err != nil {
			return fmt.Errorf("persist partial verification: %w", err)
		}
		shape := verifiedShapeID()
		if _, err := scope.conn.ExecContext(scope.ctx, fmt.Sprintf(`UPDATE %s SET
			generation = ?1,
			journal_id = ?2,
			journal_digest = ?3,
			shape_id = ?4,
			tables_digest = ?5
			WHERE id = 1`, verifiedStoreTable),
			now.generation, now.journalID, now.digest, shape, now.tablesDigest); err != nil {
			return fmt.Errorf("persist partial verification: %w", err)
		}
		return nil
	})
	return err == nil
}

// activationSchemaIsPristine reports whether the live schema carries exactly the
// objects activation creates: none missing, none extra apart from the ledger's own
// vector tables and change triggers. The currency probe itself tolerates extra objects, because a
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
	ledger := make(map[string]struct{}, len(verifiedLedgerObjects)+len(verifiedCoveredTableNames)*len(verifiedTriggerEvents))
	for _, object := range verifiedLedgerObjects {
		ledger[object.kind+"\x00"+object.name] = struct{}{}
	}
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
// carries this build's change trigger, body included. A dropped trigger, a
// trigger whose body was altered out of band, or a table rebuilt without
// recreating one reports not-current so activation reinstalls coverage; a
// trigger on a table this build does not cover is ignored.
//
// The body check matters as much as the name check: a no-op body would stop the
// counter that the fast path's change detection relies on, exactly the damage a
// missing trigger would cause, without changing any name.
//
// Coverage is required for the WHOLE covered set, not only for the tables that
// exist. Creation is conditional (ensureVerifiedLedgerSchema) because a covered
// table may legitimately be absent while a caller completes a legacy store's
// schema, but currency is not: a store missing a covered table answers "not
// current" here as well as at the expected-object check, so it can never take the
// fast path until its schema is complete.
func (scope *connScope) activationLedgerCoverageCurrent() (bool, error) {
	present := make(map[string]string, len(expectedActivationObjects))
	err := scope.queryRows("SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'trigger'", nil, func(rows *sql.Rows) error {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			return err
		}
		present[name] = statement
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("enumerate triggers: %w", err)
	}
	for _, table := range verifiedCoveredTableNames {
		for _, event := range verifiedTriggerEvents {
			if present[verifiedTriggerName(table, event)] != verifiedTriggerDDL(table, event) {
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
