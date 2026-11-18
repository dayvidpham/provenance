package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// Per-table change vector (verified-as-of ledger v3, vector extension).
//
// The ledger's durable identity is still one global generation plus the journal
// tail. The global generation is now the sum of a baseline row and one counter
// per covered table, so an existing v3 ledger stays valid across the upgrade:
// the baseline is seeded from the old singleton counter and every covered-table
// counter starts at zero.
//
// The vector exists to answer the one question the global counter cannot: which
// covered tables moved since the last proof. When the journal tail is unchanged,
// a table-local change (a direct agent registration, a seed insert, an id-format
// repair) can be verified on its own instead of re-running the whole-store
// checks. Journal- or projection-table changes still take the full path.
const (
	verifiedTableGenerationTable = "provenance_table_generation"
	verifiedTableProofTable      = "provenance_verified_table"
	verifiedAgentProofTable      = "provenance_verified_agent"
	verifiedAgentMetaTable       = "provenance_verified_agent_meta"
	verifiedBaselineTableID      = 0
	verifiedBaselineName         = "__verified_baseline__"
)

const (
	verifiedTableGenerationDDL = `CREATE TABLE IF NOT EXISTS provenance_table_generation (
		table_id INTEGER PRIMARY KEY,
		table_name TEXT NOT NULL UNIQUE,
		generation INTEGER NOT NULL) STRICT`
	verifiedTableProofDDL = `CREATE TABLE IF NOT EXISTS provenance_verified_table (
		table_id INTEGER PRIMARY KEY,
		generation INTEGER NOT NULL) STRICT`
	verifiedAgentProofDDL = `CREATE TABLE IF NOT EXISTS provenance_verified_agent (
		agent_id TEXT PRIMARY KEY) STRICT, WITHOUT ROWID`
	// verifiedAgentMetaDDL is the singleton digest of the verified agent set.
	// The set alone cannot tell a partial out-of-band delete, or a
	// same-cardinality substitution, of proof rows from a different registry,
	// so the digest is written in the same transaction and a missing or
	// mismatched row refuses the diff. What that digest covers, and what it
	// does not, is verifiedAgentSetDigest.
	verifiedAgentMetaDDL = `CREATE TABLE IF NOT EXISTS provenance_verified_agent_meta (id INTEGER PRIMARY KEY CHECK (id = 1), agent_digest TEXT NOT NULL) STRICT`
)

// verifiedLedgerObjects are the schema objects the verified-as-of ledger owns
// beyond the change triggers. They are activation-created and checked by the
// currency probe, but they are deliberately excluded from
// expectedActivationObjects and verifiedShapeID: they are ledger machinery, not
// part of the data contract, so adding them must not invalidate a ledger an
// older build wrote.
var verifiedLedgerObjects = []activationSchemaObject{
	{"table", verifiedTableGenerationTable},
	{"table", verifiedTableProofTable},
	{"table", verifiedAgentProofTable},
	{"table", verifiedAgentMetaTable},
	{"table", verifiedPrefixTable},
}

// verifiedVectorLength is the baseline row plus one row per covered table.
func verifiedVectorLength() int { return len(verifiedCoveredTableNames) + 1 }

// verifiedVectorLayoutDrifted reports whether the generation table's rows no
// longer line up with this build's closed covered-table list. An empty table is
// a first build, not drift. Any other count, an out-of-range id, or a name that
// belongs to a different id is drift. Activation deletes the whole vector and
// reseeds it on drift, which is collision-free even when a covered table was
// removed and the ids shifted left; the changed sum then sends the next open
// down the full path exactly once.
func (scope *connScope) verifiedVectorLayoutDrifted() (bool, error) {
	drift := false
	seen := 0
	err := scope.queryRows("SELECT table_id, table_name FROM "+verifiedTableGenerationTable+" ORDER BY table_id", nil, func(rows *sql.Rows) error {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		seen++
		if id < 0 || id > int64(len(verifiedCoveredTableNames)) {
			drift = true
			return nil
		}
		want := verifiedBaselineName
		if id > 0 {
			want = verifiedTableName(int(id))
		}
		if name != want {
			drift = true
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("inspect generation vector layout: %w", err)
	}
	if seen != 0 && seen != verifiedVectorLength() {
		drift = true
	}
	return drift, nil
}

// verifiedTableID maps a covered table to its vector row. It returns 0 for an
// unknown name; callers derive names only from verifiedCoveredTableNames.
func verifiedTableID(table string) int {
	for i, name := range verifiedCoveredTableNames {
		if name == table {
			return i + 1
		}
	}
	return 0
}

// verifiedTableName maps a vector row back to its covered table.
func verifiedTableName(id int) string {
	if id <= 0 || id > len(verifiedCoveredTableNames) {
		return ""
	}
	return verifiedCoveredTableNames[id-1]
}

// verifiedTriggerSQLFormat is the stored form verifiedTriggerDDL compares
// against. Activation's own create statement must stay an inline literal: the
// SQL architecture guard only decides a format string written at the sink, so
// the two sites are pinned to each other by
// TestVerifiedTriggerActivationMatchesHelper.
const verifiedTriggerSQLFormat = "CREATE TRIGGER %s AFTER %s ON %s BEGIN UPDATE %s SET generation = generation + 1 WHERE table_id = %d; END"

// verifiedTriggerDDL is the stored form of one change trigger. The trigger name
// keeps the v3 prefix so the ledger shape (which hashes names, not bodies) is
// unchanged; the body now advances the table's own counter.
func verifiedTriggerDDL(table, event string) string {
	return fmt.Sprintf(verifiedTriggerSQLFormat, verifiedTriggerName(table, event), event, table, verifiedTableGenerationTable, verifiedTableID(table))
}

// readTableGenerationVector reads the current per-table counters, baseline row
// included. A missing, duplicate, out-of-range, or misnamed row is an error: the
// vector is the change detector, so an incomplete vector must fail closed.
func (scope *connScope) readTableGenerationVector() ([]int64, error) {
	vector := make([]int64, verifiedVectorLength())
	seen := make([]bool, len(vector))
	err := scope.queryRows("SELECT table_id, table_name, generation FROM "+verifiedTableGenerationTable+" ORDER BY table_id", nil, func(rows *sql.Rows) error {
		var id, generation int64
		var name string
		if err := rows.Scan(&id, &name, &generation); err != nil {
			return err
		}
		if id < 0 || id >= int64(len(vector)) {
			return fmt.Errorf("table generation row id %d is outside the closed vector — where: verified-as-of vector; when: reading the store identity; impact: the ledger is not trusted and the full verification runs; fix: let activation reconcile the vector", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate table generation row id %d — where: verified-as-of vector; when: reading the store identity; impact: the ledger is not trusted and the full verification runs; fix: let activation reconcile the vector", id)
		}
		want := verifiedBaselineName
		if id > 0 {
			want = verifiedTableName(int(id))
		}
		if name != want {
			return fmt.Errorf("table generation row id %d names %q, want %q — where: verified-as-of vector; when: reading the store identity; impact: the ledger is not trusted and the full verification runs; fix: let activation reconcile the row name and counter", id, name, want)
		}
		vector[id] = generation
		seen[id] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read table generation vector: %w", err)
	}
	for id, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("table generation vector is missing row id %d — where: verified-as-of vector; when: reading the store identity; impact: the ledger is not trusted and the full verification runs; fix: let activation reseed the missing row", id)
		}
	}
	return vector, nil
}

// readVerifiedTableVector reads the ledger's verified-as-of mirror. Every row
// must be present; a torn or partially written vector fails closed.
func (scope *connScope) readVerifiedTableVector() ([]int64, error) {
	vector := make([]int64, verifiedVectorLength())
	seen := make([]bool, len(vector))
	err := scope.queryRows("SELECT table_id, generation FROM "+verifiedTableProofTable+" ORDER BY table_id", nil, func(rows *sql.Rows) error {
		var id int64
		var generation int64
		if err := rows.Scan(&id, &generation); err != nil {
			return err
		}
		if id < 0 || id >= int64(len(vector)) {
			return fmt.Errorf("verified table row id %d is outside the closed vector — where: verified-as-of mirror; when: reading the ledger; impact: the ledger is not trusted and the full verification runs; fix: the full path rewrites the mirror", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate verified table row id %d — where: verified-as-of mirror; when: reading the ledger; impact: the ledger is not trusted and the full verification runs; fix: the full path rewrites the mirror", id)
		}
		vector[id] = generation
		seen[id] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read verified table vector: %w", err)
	}
	for id, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("verified table vector is missing row id %d — where: verified-as-of mirror; when: reading the ledger; impact: the ledger is not trusted and the full verification runs; fix: the full path rewrites the mirror", id)
		}
	}
	return vector, nil
}

// verifiedTableVectorCount is the row count of the verified-as-of mirror. It is
// how a migration tells "never seeded" (0) from "torn" (1..len-1).
func (scope *connScope) verifiedTableVectorCount() (int, error) {
	var count int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM "+verifiedTableProofTable).Scan(&count); err != nil {
		return 0, fmt.Errorf("count verified table rows: %w", err)
	}
	return count, nil
}

// verifiedGenerationSum is the global generation the ledger records: the
// baseline plus every covered table's counter.
func verifiedGenerationSum(vector []int64) int64 {
	var sum int64
	for _, generation := range vector {
		sum += generation
	}
	return sum
}

// writeVerifiedTableVector writes every as-of row. The full path and the
// migration adoption use it; the row and the per-table vector commit in the
// caller's transaction.
func (scope *connScope) writeVerifiedTableVector(vector []int64) error {
	if len(vector) != verifiedVectorLength() {
		return fmt.Errorf("write verified table vector: got %d rows, want %d — where: verified-as-of mirror; when: recording a verification; impact: nothing is written and the store stays stale; fix: rebuild the vector from the closed covered-table set", len(vector), verifiedVectorLength())
	}
	for id, generation := range vector {
		if _, err := scope.conn.ExecContext(scope.ctx,
			"INSERT INTO "+verifiedTableProofTable+" (table_id, generation) VALUES (?1, ?2) ON CONFLICT(table_id) DO UPDATE SET generation = excluded.generation",
			int64(id), generation); err != nil {
			return fmt.Errorf("write verified table row %d: %w", id, err)
		}
	}
	return nil
}

// advanceVerifiedTableVector moves the as-of rows forward to the current
// vector. The caller has already proven the delta through its own gates; a
// counter that moved backwards is refused so a reset cannot masquerade as a
// proven change.
func (scope *connScope) advanceVerifiedTableVector(current, verified []int64) error {
	if len(current) != verifiedVectorLength() || len(verified) != verifiedVectorLength() {
		return fmt.Errorf("advance verified table vector: got %d current / %d verified rows, want %d — where: verified-as-of mirror; when: advancing a sanctioned write; impact: the ledger is not advanced and the next open verifies fully; fix: restore the vector to the closed covered-table set", len(current), len(verified), verifiedVectorLength())
	}
	for id := range current {
		if current[id] < verified[id] {
			label := verifiedBaselineName
			if id > 0 {
				label = verifiedTableName(id)
			}
			return fmt.Errorf("advance verified table vector: %s counter moved backwards from %d to %d — where: verified-as-of vector; when: advancing a sanctioned write; impact: the ledger is not advanced and the next open verifies fully; fix: restore the counter or let the full path rebuild the vector", label, verified[id], current[id])
		}
		if current[id] == verified[id] {
			continue
		}
		result, err := scope.conn.ExecContext(scope.ctx,
			"UPDATE "+verifiedTableProofTable+" SET generation = ?1 WHERE table_id = ?2",
			current[id], int64(id))
		if err != nil {
			return fmt.Errorf("advance verified table row %d: %w", id, err)
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("advance verified table row %d: read affected rows: %w — where: verified-as-of mirror; when: advancing a sanctioned write; impact: the ledger is not advanced and the next open verifies fully; fix: retry the open", id, rowsErr)
		}
		if rows != 1 {
			return fmt.Errorf("advance verified table row %d affected %d rows, want exactly one — where: verified-as-of mirror; when: advancing a sanctioned write; impact: the mirror is torn and the ledger is not advanced; fix: the next open runs the full path and rewrites the mirror", id, rows)
		}
	}
	return nil
}

// changedTableIDs returns the vector rows that differ, baseline row included.
func changedTableIDs(verified, current []int64) []int {
	if len(verified) != len(current) {
		return nil
	}
	changed := make([]int, 0, len(verified))
	for id := range verified {
		if verified[id] != current[id] {
			changed = append(changed, id)
		}
	}
	return changed
}

// identityEqual is the complete verified-as-of comparison. The vector is part
// of it: the same global sum can hide two tables moving in opposite directions.
func identityEqual(a, b verifiedStoreIdentity) bool {
	return a.generation == b.generation &&
		a.journalID == b.journalID &&
		a.digest == b.digest &&
		a.tablesDigest == b.tablesDigest &&
		slices.Equal(a.generations, b.generations)
}

// journalIdentityEqual compares only the journal tail, which is the immutable
// prefix's identity.
func journalIdentityEqual(a, b verifiedStoreIdentity) bool {
	return a.journalID == b.journalID && a.digest == b.digest
}

// migrationIdentityMatches is the pre-vector identity comparison: the ledger's
// four persisted identity fields against the store as it stands. The per-table
// vector is compared separately by identityEqual. Both adoption paths call this
// one predicate so a future identity field cannot be added to one and missed in
// the other.
func migrationIdentityMatches(ledger, now verifiedStoreIdentity) bool {
	return ledger.generation == now.generation &&
		ledger.journalID == now.journalID &&
		ledger.digest == now.digest &&
		ledger.tablesDigest == now.tablesDigest
}

// metadataChecksFor returns the table-local checks that cover every changed
// row, or false when any changed table needs the whole-store path. Only tables
// whose whole-store coverage is exactly a local check belong here; every
// journal- or projection-derived table deliberately falls through. Closed seed
// lookup changes also fall through: the seed-presence check alone does not cover
// the foreign keys from their dependent tables.
func (scope *connScope) metadataChecksFor(changed []int, models []ptypes.ModelEntry) ([]func() error, bool) {
	var needAgents, needClaims, needModels, needIDFormat bool
	for _, id := range changed {
		switch verifiedTableName(id) {
		case "agents", "agents_human", "agents_ml", "agents_software":
			needAgents = true
		case "actor_namespace_claims", "fixed_actor_manifest_entries":
			needClaims = true
		case "ml_models":
			needModels = true
		case "provenance_id_format":
			needIDFormat = true
		default:
			// Closed seed lookups fall through deliberately: their dependent
			// tables need a foreign-key check the seed-presence test does not
			// provide, and seed changes are rare enough that the whole-store path
			// is the right authority.
			return nil, false
		}
	}
	checks := make([]func() error, 0, 4)
	if needAgents {
		checks = append(checks, scope.verifyAgentRegistryIntegrity)
	}
	if needClaims {
		checks = append(checks, scope.verifyActorClaimForeignKeys)
	}
	if needModels {
		checks = append(checks, func() error { return scope.verifyModelRegistryIntegrity(models) })
	}
	if needIDFormat {
		checks = append(checks, func() error {
			return scope.requireActivationCheck("identity format", scope.activationIDFormatCurrent)
		})
	}
	return checks, true
}

// requireActivationCheck adapts a currency predicates pair to an error.
func (scope *connScope) requireActivationCheck(label string, check func() (bool, error)) error {
	current, err := check()
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !current {
		return fmt.Errorf("%s is not current", label)
	}
	return nil
}

// currentAgentIDs reads every registered agent id. The agents table is the
// registry; order is only for deterministic writes and tests.
func (scope *connScope) currentAgentIDs() ([]string, error) {
	ids := make([]string, 0)
	err := scope.queryRows("SELECT id FROM agents ORDER BY id", nil, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate current agents: %w", err)
	}
	return ids, nil
}

// verifiedAgentIDs reads the ledger's verified agent set.
func (scope *connScope) verifiedAgentIDs() ([]string, error) {
	ids := make([]string, 0)
	err := scope.queryRows("SELECT agent_id FROM "+verifiedAgentProofTable+" ORDER BY agent_id", nil, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate verified agents: %w", err)
	}
	return ids, nil
}

// verifiedAgentSetDigest is the content digest of one verified agent set. The
// IDs are copied and sorted, then each is written as a length-prefixed record,
// so the value depends only on the set and two different sets cannot collide
// by concatenation. A count would miss a same-cardinality substitution. The
// digest is not tamper-proof against a writer who rewrites both the set and
// the digest.
func verifiedAgentSetDigest(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.New()
	for _, id := range sorted {
		fmt.Fprintf(sum, "%d\x00%s\n", len(id), id)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// writeVerifiedAgentSet records the current registry and its digest as
// verified. It runs in the caller's transaction alongside the ledger identity,
// so the set and the digest commit or roll back together. A later open trusts
// the set only when the surviving proof rows still digest to that value.
func (scope *connScope) writeVerifiedAgentSet() error {
	ids, err := scope.currentAgentIDs()
	if err != nil {
		return err
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM "+verifiedAgentProofTable); err != nil {
		return fmt.Errorf("clear verified agent set: %w", err)
	}
	for _, id := range ids {
		if _, err := scope.conn.ExecContext(scope.ctx,
			"INSERT INTO "+verifiedAgentProofTable+" (agent_id) VALUES (?1)", id); err != nil {
			return fmt.Errorf("record verified agent %q: %w", id, err)
		}
	}
	if _, err := scope.conn.ExecContext(scope.ctx,
		"INSERT INTO "+verifiedAgentMetaTable+" (id, agent_digest) VALUES (1, ?1) ON CONFLICT(id) DO UPDATE SET agent_digest = excluded.agent_digest",
		verifiedAgentSetDigest(ids)); err != nil {
		return fmt.Errorf("record verified agent digest: %w", err)
	}
	return nil
}

// verifiedAgentDigest reads the singleton digest written with the verified
// set. ok is false when the row is missing: a set without its digest is not a
// proof, and the caller must run the full orphan probes.
func (scope *connScope) verifiedAgentDigest() (digest string, ok bool, err error) {
	err = scope.conn.QueryRowContext(scope.ctx,
		"SELECT agent_digest FROM "+verifiedAgentMetaTable+" WHERE id = 1").Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read verified agent digest: %w", err)
	}
	return digest, true, nil
}

// removedVerifiedAgents diffs the verified set against the current registry.
// known is false when the set is empty, the digest row is missing, or the
// surviving proof rows do not digest to the recorded value. An empty set is
// indistinguishable from a set whose rows were deleted along with the agents
// they named, and a partial delete or same-cardinality substitution of proof
// rows would otherwise look like a different verified registry. Either case
// must run the full orphan probes rather than assume no agent was removed. The
// probes are cheap on a store that never had agents and are the only
// fail-safe on one whose registry was emptied, or whose proof was torn, out
// of band. What the digest covers, and what it does not, is
// verifiedAgentSetDigest.
func (scope *connScope) removedVerifiedAgents() (removed []string, known bool, err error) {
	current, err := scope.currentAgentIDs()
	if err != nil {
		return nil, false, err
	}
	verified, err := scope.verifiedAgentIDs()
	if err != nil {
		return nil, false, err
	}
	digest, digestOK, err := scope.verifiedAgentDigest()
	if err != nil {
		return nil, false, err
	}
	if len(verified) == 0 || !digestOK || verifiedAgentSetDigest(verified) != digest {
		return nil, false, nil
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, id := range current {
		currentSet[id] = struct{}{}
	}
	for _, id := range verified {
		if _, ok := currentSet[id]; !ok {
			removed = append(removed, id)
		}
	}
	return removed, true, nil
}

// verifyAgentRegistryIntegrity is the table-local coverage for the agent
// registry: every agent has exactly one subtype row of its kind, every subtype
// row agrees with its supertype, no agent appears in two subtypes, and every
// relation that references an agent still finds it. The whole-store path covers
// the references with its global foreign-key check but deliberately does not run
// the subtype probes: adding them there would tighten the proof behind an
// existing v3 ledger without a version bump. The partial path is therefore
// stricter for a changed agent registry, which is the fail-closed direction.
func (scope *connScope) verifyAgentRegistryIntegrity() error {
	if err := scope.verifyAgentRegistryForeignKeys(); err != nil {
		return err
	}
	return scope.verifyAgentRegistryProbes()
}

// verifyAgentRegistryForeignKeys covers the relations that reference agents.
// The small relations use a plain foreign-key check. The three large ones use
// an indexed orphan probe instead of a full foreign-key scan: journal.actor_id
// and task_attributions.actor_id are indexed, and the orphan probe asks only
// whether any non-null reference has no parent.
func (scope *connScope) verifyAgentRegistryForeignKeys() error {
	for _, statement := range []string{
		"PRAGMA foreign_key_check(agents)",
		"PRAGMA foreign_key_check(agents_human)",
		"PRAGMA foreign_key_check(agents_ml)",
		"PRAGMA foreign_key_check(agents_software)",
		"PRAGMA foreign_key_check(actor_namespace_claims)",
		"PRAGMA foreign_key_check(fixed_actor_manifest_entries)",
		"PRAGMA foreign_key_check(tasks)",
		"PRAGMA foreign_key_check(activities)",
		"PRAGMA foreign_key_check(comments)",
	} {
		if err := scope.requireNoForeignKeyViolation(statement); err != nil {
			return err
		}
	}
	removed, known, err := scope.removedVerifiedAgents()
	if err != nil {
		return err
	}
	if !known {
		// No trusted set (empty, missing digest, or a digest that does not
		// match the surviving proof rows): run the full probes rather than
		// assume an insert-only change.
		for _, probe := range agentOrphanProbes {
			if err := scope.scanAgentOrphan(probe.query(), probe.label()); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range removed {
		for _, probe := range agentOrphanProbes {
			if err := scope.scanAgentOrphan(probe.removedQuery(), probe.label(), id); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanAgentOrphan runs one orphan probe. A returned row is a labeled integrity
// failure; ErrNoRows is clean; any other error fails closed with the same label.
func (scope *connScope) scanAgentOrphan(query, label string, args ...any) error {
	var bad string
	err := scope.conn.QueryRowContext(scope.ctx, query, args...).Scan(&bad)
	if err == nil {
		return fmt.Errorf("agent registry %s: %q", label, bad)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("agent registry %s: %w", label, err)
	}
	return nil
}

// verifyAgentRegistryProbes covers the subtype invariants that no foreign key
// expresses.
func (scope *connScope) verifyAgentRegistryProbes() error {
	for _, probe := range agentRegistryProbes {
		var bad string
		err := scope.conn.QueryRowContext(scope.ctx, probe.query()).Scan(&bad)
		if err == nil {
			return fmt.Errorf("agent registry %s: %q", probe.label(), bad)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("agent registry %s: %w", probe.label(), err)
		}
	}
	return nil
}

// agentOrphanProbe is the closed set of indexed references to agents that are
// too large for a plain per-table foreign-key scan in the partial path.
type agentOrphanProbe uint8

const (
	agentOrphanJournalAnchor agentOrphanProbe = iota + 1
	agentOrphanTaskAttribution
	agentOrphanAssignmentEpisode
)

var agentOrphanProbes = []agentOrphanProbe{
	agentOrphanJournalAnchor,
	agentOrphanTaskAttribution,
	agentOrphanAssignmentEpisode,
}

func (probe agentOrphanProbe) label() string {
	switch probe {
	case agentOrphanJournalAnchor:
		return "journal anchor actor"
	case agentOrphanTaskAttribution:
		return "task attribution actor"
	case agentOrphanAssignmentEpisode:
		return "assignment episode actor"
	default:
		panic("unknown agent orphan probe")
	}
}

func (probe agentOrphanProbe) query() string {
	switch probe {
	case agentOrphanJournalAnchor:
		return "SELECT CAST(journal_id AS TEXT) FROM journal WHERE actor_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM agents WHERE agents.id = journal.actor_id) LIMIT 1"
	case agentOrphanTaskAttribution:
		return "SELECT task_id FROM task_attributions WHERE NOT EXISTS (SELECT 1 FROM agents WHERE agents.id = task_attributions.actor_id) LIMIT 1"
	case agentOrphanAssignmentEpisode:
		return "SELECT assignment_id FROM journal_authority_assignment_episodes WHERE NOT EXISTS (SELECT 1 FROM agents WHERE agents.id = journal_authority_assignment_episodes.actor_id) LIMIT 1"
	default:
		panic("unknown agent orphan probe")
	}
}

// removedQuery is the indexed per-removed-agent form. The journal probe uses
// idx_journal_actor; the other two relations are small.
func (probe agentOrphanProbe) removedQuery() string {
	switch probe {
	case agentOrphanJournalAnchor:
		return "SELECT CAST(journal_id AS TEXT) FROM journal WHERE actor_id = ?1 LIMIT 1"
	case agentOrphanTaskAttribution:
		return "SELECT task_id FROM task_attributions WHERE actor_id = ?1 LIMIT 1"
	case agentOrphanAssignmentEpisode:
		return "SELECT assignment_id FROM journal_authority_assignment_episodes WHERE actor_id = ?1 LIMIT 1"
	default:
		panic("unknown agent orphan probe")
	}
}

// agentRegistryProbe is the closed set of table-local agent-registry checks.
// A selector method (not a struct field) is what keeps every statement text
// decidable from source for the SQL architecture guard.
type agentRegistryProbe uint8

const (
	agentRegistryTotality agentRegistryProbe = iota + 1
	agentRegistryHumanDiscriminator
	agentRegistryMLDiscriminator
	agentRegistrySoftwareDiscriminator
	agentRegistryHumanMLExclusivity
	agentRegistryHumanSoftwareExclusivity
	agentRegistryMLSoftwareExclusivity
)

var agentRegistryProbes = []agentRegistryProbe{
	agentRegistryTotality,
	agentRegistryHumanDiscriminator,
	agentRegistryMLDiscriminator,
	agentRegistrySoftwareDiscriminator,
	agentRegistryHumanMLExclusivity,
	agentRegistryHumanSoftwareExclusivity,
	agentRegistryMLSoftwareExclusivity,
}

func (probe agentRegistryProbe) label() string {
	switch probe {
	case agentRegistryTotality:
		return "agents totality"
	case agentRegistryHumanDiscriminator:
		return "agents human discriminator"
	case agentRegistryMLDiscriminator:
		return "agents ml discriminator"
	case agentRegistrySoftwareDiscriminator:
		return "agents software discriminator"
	case agentRegistryHumanMLExclusivity:
		return "agents human/ml exclusivity"
	case agentRegistryHumanSoftwareExclusivity:
		return "agents human/software exclusivity"
	case agentRegistryMLSoftwareExclusivity:
		return "agents ml/software exclusivity"
	default:
		panic("unknown agent registry probe")
	}
}

func (probe agentRegistryProbe) query() string {
	switch probe {
	case agentRegistryTotality:
		return "SELECT a.id FROM agents a LEFT JOIN agents_human h ON h.agent_id=a.id LEFT JOIN agents_ml m ON m.agent_id=a.id LEFT JOIN agents_software s ON s.agent_id=a.id WHERE (a.kind_id=0 AND h.agent_id IS NULL) OR (a.kind_id=1 AND m.agent_id IS NULL) OR (a.kind_id=2 AND s.agent_id IS NULL) LIMIT 1"
	case agentRegistryHumanDiscriminator:
		return "SELECT h.agent_id FROM agents_human h JOIN agents a ON a.id=h.agent_id WHERE a.kind_id<>0 LIMIT 1"
	case agentRegistryMLDiscriminator:
		return "SELECT m.agent_id FROM agents_ml m JOIN agents a ON a.id=m.agent_id WHERE a.kind_id<>1 LIMIT 1"
	case agentRegistrySoftwareDiscriminator:
		return "SELECT s.agent_id FROM agents_software s JOIN agents a ON a.id=s.agent_id WHERE a.kind_id<>2 LIMIT 1"
	case agentRegistryHumanMLExclusivity:
		return "SELECT h.agent_id FROM agents_human h JOIN agents_ml m ON m.agent_id=h.agent_id LIMIT 1"
	case agentRegistryHumanSoftwareExclusivity:
		return "SELECT h.agent_id FROM agents_human h JOIN agents_software s ON s.agent_id=h.agent_id LIMIT 1"
	case agentRegistryMLSoftwareExclusivity:
		return "SELECT m.agent_id FROM agents_ml m JOIN agents_software s ON s.agent_id=m.agent_id LIMIT 1"
	default:
		panic("unknown agent registry probe")
	}
}

// adoptCurrentVerifiedLedger upgrades a current v3 ledger to the per-table
// vector without re-running the whole-store checks. It is called from
// activation after ensureSchema installed the vector tables and this build's
// trigger bodies, and it acts only when the ledger row still proves the store as
// it stands:
//
//   - a complete mirror means the ordinary current comparison is authoritative,
//     and a match also seeds the verified agent set and its digest from
//     the current registry; the branch is not only an identity comparison;
//   - an empty mirror on a store whose covered-table counters are all still zero
//     is the first vector build. The baseline row was seeded from the old
//     singleton counter, so the ledger's global generation must match it, and
//     the journal tail and table presence set must match too;
//   - any other state is torn and falls back to the full path.
//
// Adoption never advances verified_count or verified_at: it is not a new
// verification, only the same proof recorded in the new shape.
func (scope *connScope) adoptCurrentVerifiedLedger() (bool, error) {
	if scope.ephemeralStore {
		return false, nil
	}
	ledgerRow, ok := scope.readVerifiedLedgerRowOnly()
	if !ok || ledgerRow.shapeID != verifiedShapeID() {
		return false, nil
	}
	count, err := scope.verifiedTableVectorCount()
	if err != nil {
		return false, err
	}
	if count == verifiedVectorLength() {
		ledger, ok := scope.readVerifiedLedger()
		if !ok {
			return false, nil
		}
		now, err := scope.readVerifiedStoreIdentity()
		if err != nil {
			return false, nil
		}
		if !identityEqual(ledger.identity, now) {
			return false, nil
		}
		if err := scope.writeVerifiedAgentSet(); err != nil {
			return false, err
		}
		// The ledger proves the store at its journal id, so the verified prefix
		// is seeded by hashing the stored rows. No operation is decoded.
		if err := scope.seedVerifiedPrefixIfAbsent(ledger.identity.journalID); err != nil {
			return false, err
		}
		return true, nil
	}
	if count != 0 {
		return false, nil
	}
	now, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return false, nil
	}
	for id := 1; id < len(now.generations); id++ {
		if now.generations[id] != 0 {
			return false, nil
		}
	}
	if !migrationIdentityMatches(ledgerRow.identity, now) {
		return false, nil
	}
	if err := scope.writeVerifiedTableVector(now.generations); err != nil {
		return false, err
	}
	if err := scope.writeVerifiedAgentSet(); err != nil {
		return false, err
	}
	if err := scope.seedVerifiedPrefixIfAbsent(ledgerRow.identity.journalID); err != nil {
		return false, err
	}
	return true, nil
}

// adoptableVerifiedLedger is the read-only preflight counterpart of
// adoptCurrentVerifiedLedger. A current v3 ledger lets the preflight skip the
// whole-store checks and the isolated activation clone: activation will create
// the vector tables and adopt, and if adoption fails there it still runs the
// full path inside its own transaction. Anything torn reports false.
func (scope *connScope) adoptableVerifiedLedger() (bool, error) {
	ledger, ok := scope.readVerifiedLedgerRowOnly()
	if !ok || ledger.shapeID != verifiedShapeID() {
		return false, nil
	}
	exists, err := scope.tableExists(verifiedTableProofTable)
	if err != nil {
		return false, err
	}
	if !exists {
		// The vector tables do not exist yet: the v3 upgrade shape. Compare the
		// old global identity.
		legacy, err := scope.readVerifiedStoreIdentityLegacy()
		if err != nil {
			return false, err
		}
		return migrationIdentityMatches(ledger.identity, legacy), nil
	}
	count, err := scope.verifiedTableVectorCount()
	if err != nil {
		return false, err
	}
	if count != 0 {
		// A complete mirror is the normal verifiedLedgerCurrent check's job; a
		// partial mirror is torn.
		return false, nil
	}
	now, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return false, nil
	}
	for id := 1; id < len(now.generations); id++ {
		if now.generations[id] != 0 {
			return false, nil
		}
	}
	return migrationIdentityMatches(ledger.identity, now), nil
}

// verifyModelRegistryIntegrity is the whole-store coverage for ml_models: the
// caller's registry still describes the seeded set, and both sides of the
// model-to-agent foreign key hold.
func (scope *connScope) verifyModelRegistryIntegrity(models []ptypes.ModelEntry) error {
	for _, statement := range []string{
		"PRAGMA foreign_key_check(ml_models)",
		"PRAGMA foreign_key_check(agents_ml)",
	} {
		if err := scope.requireNoForeignKeyViolation(statement); err != nil {
			return err
		}
	}
	return scope.requireActivationCheck("ml models", func() (bool, error) { return scope.activationMLModelsCurrent(models) })
}

// verifyActorClaimForeignKeys is the whole-store coverage for the two direct
// actor-registry relations: their foreign keys are the only whole-store rule
// that reads them.
func (scope *connScope) verifyActorClaimForeignKeys() error {
	for _, statement := range []string{
		"PRAGMA foreign_key_check(actor_namespace_claims)",
		"PRAGMA foreign_key_check(fixed_actor_manifest_entries)",
	} {
		if err := scope.requireNoForeignKeyViolation(statement); err != nil {
			return err
		}
	}
	return nil
}

// requireNoForeignKeyViolation reports the first row a foreign-key check
// returns. The PRAGMA text is a compile-time literal at every call site.
func (scope *connScope) requireNoForeignKeyViolation(statement string) error {
	violated := false
	if err := scope.queryRows(statement, nil, func(rows *sql.Rows) error {
		violated = true
		return nil
	}); err != nil {
		return fmt.Errorf("foreign-key check %q: %w", statement, err)
	}
	if violated {
		return fmt.Errorf("foreign-key check %q reported a violation", statement)
	}
	return nil
}
