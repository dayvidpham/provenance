package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

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
}

// verifiedVectorLength is the baseline row plus one row per covered table.
func verifiedVectorLength() int { return len(verifiedCoveredTableNames) + 1 }

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

// verifiedTriggerDDL is the stored form of one change trigger. The trigger name
// keeps the v3 prefix so the ledger shape (which hashes names, not bodies) is
// unchanged; the body now advances the table's own counter.
func verifiedTriggerDDL(table, event string) string {
	return fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON %s BEGIN UPDATE %s SET generation = generation + 1 WHERE table_id = %d; END",
		verifiedTriggerName(table, event), event, table, verifiedTableGenerationTable, verifiedTableID(table))
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
			return fmt.Errorf("table generation row id %d is outside the closed vector", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate table generation row id %d", id)
		}
		want := verifiedBaselineName
		if id > 0 {
			want = verifiedTableName(int(id))
		}
		if name != want {
			return fmt.Errorf("table generation row id %d names %q, want %q", id, name, want)
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
			return nil, fmt.Errorf("table generation vector is missing row id %d", id)
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
			return fmt.Errorf("verified table row id %d is outside the closed vector", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate verified table row id %d", id)
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
			return nil, fmt.Errorf("verified table vector is missing row id %d", id)
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
		return fmt.Errorf("write verified table vector: got %d rows, want %d", len(vector), verifiedVectorLength())
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
		return fmt.Errorf("advance verified table vector: got %d current / %d verified rows, want %d", len(current), len(verified), verifiedVectorLength())
	}
	for id := range current {
		if current[id] < verified[id] {
			label := verifiedBaselineName
			if id > 0 {
				label = verifiedTableName(id)
			}
			return fmt.Errorf("advance verified table vector: %s counter moved backwards from %d to %d", label, verified[id], current[id])
		}
		if current[id] == verified[id] {
			continue
		}
		if _, err := scope.conn.ExecContext(scope.ctx,
			"UPDATE "+verifiedTableProofTable+" SET generation = ?1 WHERE table_id = ?2",
			current[id], int64(id)); err != nil {
			return fmt.Errorf("advance verified table row %d: %w", id, err)
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

// metadataChecksFor returns the table-local checks that cover every changed
// row, or false when any changed table needs the whole-store path. Only tables
// whose whole-store coverage is exactly a local check belong here; every
// journal- or projection-derived table deliberately falls through.
func (scope *connScope) metadataChecksFor(changed []int, models []ptypes.ModelEntry) ([]func() error, bool) {
	var needSeeds, needAgents, needClaims, needModels, needIDFormat bool
	for _, id := range changed {
		switch verifiedTableName(id) {
		case "statuses", "priorities", "task_types", "edge_kinds", "agent_kinds", "providers", "roles", "phases", "stages",
			"journal_kinds", "authority_kinds", "assignment_slots", "assignment_transitions":
			needSeeds = true
		case "agents", "agents_human", "agents_ml", "agents_software":
			needAgents = true
		case "actor_namespace_claims", "fixed_actor_manifest_entries":
			needClaims = true
		case "ml_models":
			needModels = true
		case "provenance_id_format":
			needIDFormat = true
		default:
			return nil, false
		}
	}
	checks := make([]func() error, 0, 5)
	if needSeeds {
		checks = append(checks, func() error {
			return scope.requireActivationCheck("reference seeds", scope.activationReferenceSeedsCurrent)
		})
	}
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

// verifyAgentRegistryIntegrity is the whole-store coverage for the agent
// registry: every agent has exactly one subtype row of its kind, every subtype
// row agrees with its supertype, no agent appears in two subtypes, and the
// foreign keys from the registry and its dependent tables hold.
func (scope *connScope) verifyAgentRegistryIntegrity() error {
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
//   - a complete mirror means the ordinary current comparison is authoritative;
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
		return identityEqual(ledger.identity, now), nil
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
	if ledgerRow.identity.generation != now.generation ||
		ledgerRow.identity.journalID != now.journalID ||
		ledgerRow.identity.digest != now.digest ||
		ledgerRow.identity.tablesDigest != now.tablesDigest {
		return false, nil
	}
	if err := scope.writeVerifiedTableVector(now.generations); err != nil {
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
	count, err := scope.verifiedTableVectorCount()
	if err != nil {
		// The vector tables do not exist yet: the v3 upgrade shape. Compare the
		// old global identity.
		legacy, err := scope.readVerifiedStoreIdentityLegacy()
		if err != nil {
			return false, err
		}
		return ledger.identity.generation == legacy.generation &&
			ledger.identity.journalID == legacy.journalID &&
			ledger.identity.digest == legacy.digest &&
			ledger.identity.tablesDigest == legacy.tablesDigest, nil
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
	return ledger.identity.generation == now.generation &&
		ledger.identity.journalID == now.journalID &&
		ledger.identity.digest == now.digest &&
		ledger.identity.tablesDigest == now.tablesDigest, nil
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
