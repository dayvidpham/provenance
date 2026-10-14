package sqlite

import (
	"database/sql"
	"strings"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// activationSchemaObject is one sqlite_master entry a current store carries.
type activationSchemaObject struct {
	kind string
	name string
}

// expectedActivationObjects is the closed set of schema objects this build's
// activation creates. It is deliberately explicit rather than derived: the
// currency probe compares a live store against this list using reads only, and a
// store is treated as current only when every object activation would create is
// already present. TestActivationCurrencyMatchesFreshActivation asserts this set
// equals a freshly activated store's object set exactly, so activation's DDL
// cannot grow, shrink, or rename an object without failing that test and forcing
// this list to move with it. Extra unrelated objects a caller may add to a
// borrowed file are ignored: activation only ever runs CREATE ... IF NOT EXISTS
// for the objects below, so unrelated objects cannot make it change anything.
var expectedActivationObjects = []activationSchemaObject{
	// ensureSchema reference and domain relations.
	{"table", "statuses"},
	{"table", "priorities"},
	{"table", "task_types"},
	{"table", "edge_kinds"},
	{"table", "agent_kinds"},
	{"table", "providers"},
	{"table", "roles"},
	{"table", "phases"},
	{"table", "stages"},
	{"table", "ml_models"},
	{"table", "agents"},
	{"table", "agents_human"},
	{"table", "agents_ml"},
	{"table", "agents_software"},
	{"table", "tasks"},
	{"table", "edges"},
	{"table", "activities"},
	{"table", "labels"},
	{"table", "comments"},
	{"index", "idx_tasks_namespace"},
	{"index", "idx_tasks_status"},
	{"index", "idx_tasks_priority"},
	{"index", "idx_tasks_type"},
	{"index", "idx_tasks_phase"},
	{"index", "idx_tasks_owner"},
	{"index", "idx_edges_source"},
	{"index", "idx_edges_target"},
	{"index", "idx_edges_kind"},
	{"index", "idx_activities_agent"},
	{"index", "idx_activities_phase"},
	{"index", "idx_labels_name"},
	{"index", "idx_comments_task"},
	{"index", "idx_comments_author"},
	// ensureJournalSchema journal-base relations, index set, and view.
	{"table", "journal_kinds"},
	{"table", "journal"},
	{"table", "journal_task_events"},
	{"table", "journal_task_event_contexts"},
	{"table", "actor_namespace_claims"},
	{"table", "fixed_actor_manifest_entries"},
	{"table", "task_attributions"},
	{"index", "idx_journal_task_events_task"},
	{"index", "idx_journal_kind"},
	{"index", "idx_journal_actor"},
	{"index", "idx_journal_pboj"},
	{"index", "idx_journal_recorded_at"},
	{"view", "journal_attributed"},
	// ensureOperationsSchema operations, authority, decision, and evidence relations.
	{"table", "authority_kinds"},
	{"table", "assignment_slots"},
	{"table", "assignment_transitions"},
	{"table", "journal_operations"},
	{"table", "journal_operation_result_slots"},
	{"table", "journal_authorities"},
	{"table", "journal_authority_bootstraps"},
	{"table", "journal_authority_assignment_episodes"},
	{"table", "journal_authority_assignment_transitions"},
	{"table", "journal_decisions"},
	{"table", "journal_evidence"},
	{"trigger", "journal_operations_canonical_insert"},
	{"trigger", "journal_operations_canonical_update"},
	{"index", "idx_transitions_assignment"},
	{"index", "idx_episodes_task"},
	{"index", "idx_episodes_parent"},
	// ensureFactContextRelations immutable fact-context relations.
	{"table", "journal_decision_contexts"},
	{"table", "journal_evidence_contexts"},
	// ensureActivityCreationsSchema activity-projection relation.
	{"table", "journal_activity_creations"},
	{"index", "idx_journal_activity_creations_activity"},
	// allocation.EnsureSchema governed-allocation relations.
	{"table", "governed_allocation_operations"},
	{"table", "governed_operation_effect_rows"},
	{"table", "governed_allocation_genesis"},
	{"table", "governed_composed_supplement_owners"},
	// ensureVerifiedLedgerSchema verified-as-of ledger: the DML change counter, the
	// verified-as-of row, and (checked separately by activationLedgerCoverageCurrent,
	// because their names are derived rather than listed) one change trigger per
	// covered table. See open_verified.go.
	{"table", storeGenerationTable},
	{"table", verifiedStoreTable},
}

// obsoleteActivationObjects are objects activation drops on every open (with IF
// EXISTS). Their presence would make activation change the store, so their
// absence is part of currency.
var obsoleteActivationObjects = []string{"governed_operation_result_bindings"}

// seedRow is one (id, name) lookup row activation inserts with INSERT OR IGNORE.
type seedRow struct {
	id   int
	name string
}

// Read-only lookups the currency probe uses. They are compile-time constants so
// no SQL statement text is assembled at run time.
const (
	selectCanonicalInsertTriggerSQL = "SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'trigger' AND name = 'journal_operations_canonical_insert'"
	selectCanonicalUpdateTriggerSQL = "SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'trigger' AND name = 'journal_operations_canonical_update'"
	selectJournalKindsSQL           = "SELECT id, name FROM journal_kinds"
	selectAuthorityKindsSQL         = "SELECT id, name FROM authority_kinds"
	selectAssignmentSlotsSQL        = "SELECT id, name FROM assignment_slots"
	selectAssignmentTransitionsSQL  = "SELECT id, name FROM assignment_transitions"
)

// activationWouldChangeNothing reports whether this store is already exactly what
// this build's activation would produce, so activation would write nothing. Its
// structural checks use reads only: no DDL, no write pragmas, and no write
// transaction. It is deliberately conservative: any read failure, missing object,
// or unexpected state reports false, so the caller falls back to the full
// activation, which then writes what is missing or surfaces the canonical error.
//
// "Exactly what activation would produce" is NOT only structural: activation also
// verifies whole-journal integrity and refuses a store whose stored projections
// disagree with a from-empty re-derivation of the folded journal. The probe runs
// those same read-only checks — [connScope.activationConvergenceVerified] — so it
// is at least as strict as activation: a store activation would refuse is never
// served as "current" with the damage untouched.
//
// The convergence check itself is skipped only when the durable verified-as-of
// ledger proves the store has not changed since the last whole-store verification
// (open_verified.go), and that skip takes no write lock. The structural checks
// below always run, so DDL damage — a dropped or altered object, a spine table
// whose columns moved, a removed change trigger — is refused on the fast path
// exactly as it was before the ledger existed. When the checks DO have to run,
// the probe persists the ledger for the state they covered, which is this
// function's one write (a single row, inside its own bounded write transaction),
// and only on the path that had to do the whole-store work anyway.
func (scope *connScope) activationWouldChangeNothing(models []ptypes.ModelEntry) bool {
	checks := []func() (bool, error){
		scope.activationSchemaObjectsCurrent,
		scope.activationOperationsSchemaCurrent,
		scope.activationFactContextSchemaCurrent,
		func() (bool, error) { return scope.activationReferenceSeedsCurrent() },
		func() (bool, error) { return scope.activationMLModelsCurrent(models) },
		// Journal-spine column shapes: the one DDL change that reaches a covered
		// table's content without advancing the generation.
		scope.activationSpineShapeCurrent,
		// Every covered table must still carry its change trigger, or the
		// generation would stop tracking that table's writes.
		scope.activationLedgerCoverageCurrent,
		// Last, and only reached by a structurally-current store: this is the
		// most expensive probe and the one activation's own convergence
		// comparison lives in. It is skipped outright when the verified-as-of
		// ledger still describes the store as it stands.
		scope.activationConvergenceVerified,
	}
	for _, check := range checks {
		current, err := check()
		if err != nil || !current {
			return false
		}
	}
	return true
}

// activationConvergenceVerified reports whether the whole-store convergence
// checks activation would run can be skipped or have just passed.
//
// Fast path: the durable verified-as-of ledger still describes the store as it
// stands, so nothing VerifyIntegrity or ReplayProjections cover has moved since
// the last time both passed. That read is O(1) — one ledger row, one INTEGER
// PRIMARY KEY maximum, one row by primary key — and it takes no write lock.
//
// Slow path: run the same read-only pair activation runs, in the same order, and
// then persist the ledger for the state the checks covered. Persisting is a
// compare-and-set inside BEGIN IMMEDIATE: if any writer committed during the
// read-only checks the ledger is not written and this reports false, so the
// caller takes the full activation path instead of recording a verification that
// did not cover the committed state.
//
// Any failure — divergence, damaged journal, unreadable store, or a moved store —
// reports not-current. The activation path then re-runs the same checks inside its
// write transaction and surfaces the canonical error, so nothing is masked.
func (scope *connScope) activationConvergenceVerified() (bool, error) {
	if current, err := scope.verifiedLedgerCurrent(); err == nil && current {
		return true, nil
	}
	before, err := scope.readVerifiedStoreIdentity()
	if err != nil {
		return false, nil
	}
	if err := scope.verifyIntegrity(); err != nil {
		return false, nil
	}
	if _, err := scope.replayProjections(); err != nil {
		return false, nil
	}
	return scope.persistVerifiedLedgerIfUnchanged(before), nil
}

// activationSchemaObjectsCurrent verifies every object activation creates exists
// and none it drops is present.
func (scope *connScope) activationSchemaObjectsCurrent() (bool, error) {
	present := make(map[string]struct{}, len(expectedActivationObjects))
	err := scope.queryRows("SELECT type, name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'", nil, func(rows *sql.Rows) error {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return err
		}
		present[kind+"\x00"+name] = struct{}{}
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, want := range expectedActivationObjects {
		if _, ok := present[want.kind+"\x00"+want.name]; !ok {
			return false, nil
		}
	}
	for _, obsolete := range obsoleteActivationObjects {
		if _, ok := present["table\x00"+obsolete]; ok {
			return false, nil
		}
	}
	return true, nil
}

// activationOperationsSchemaCurrent verifies the operations relation shapes that
// ensureCanonicalMutationColumns and ensureGenericCanonicalConstraints read
// before they decide to migrate: both canonical columns present, no retained
// V1-specific constraint text on the table or its canonical-pair triggers.
func (scope *connScope) activationOperationsSchemaCurrent() (bool, error) {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return false, err
	}
	_, hasVersion := columns["mutation_encoding_version"]
	_, hasBytes := columns["canonical_mutation"]
	if !hasVersion || !hasBytes {
		return false, nil
	}
	var tableSQL string
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name = 'journal_operations'").Scan(&tableSQL); err != nil {
		return false, err
	}
	legacy := journal.MutationEncodingV1.String()
	if strings.Contains(tableSQL, legacy) {
		return false, nil
	}
	for _, statement := range []string{selectCanonicalInsertTriggerSQL, selectCanonicalUpdateTriggerSQL} {
		var triggerSQL string
		if err := scope.conn.QueryRowContext(scope.ctx, statement).Scan(&triggerSQL); err != nil {
			return false, err
		}
		if strings.Contains(triggerSQL, legacy) {
			return false, nil
		}
	}
	return true, nil
}

// activationFactContextSchemaCurrent verifies the two immutable fact-context
// relations are in the canonical shape. A legacy store (neither relation) or a
// malformed pair reports false: activation then creates or rejects it exactly as
// it does today.
func (scope *connScope) activationFactContextSchemaCurrent() (bool, error) {
	state, err := scope.classifyFactContextSchema()
	if err != nil {
		// A shape error is itself "not provably current"; activation re-runs the
		// same classification and surfaces the canonical rejection.
		return false, nil
	}
	return state == factContextSchemaCanonical, nil
}

// activationReferenceSeedsCurrent verifies every closed-lookup row activation
// inserts with INSERT OR IGNORE is already present, and that the journal FK is
// complete, so seeding and the journal-table rebuild would both write nothing.
func (scope *connScope) activationReferenceSeedsCurrent() (bool, error) {
	for _, seed := range referenceSeeds {
		expected := make([]seedRow, 0, len(seed.names))
		for id, name := range seed.names {
			expected = append(expected, seedRow{id: id, name: name})
		}
		current, err := scope.seedLookupCurrent(seed.kind.selectQuery(), expected)
		if err != nil || !current {
			return false, err
		}
	}
	journalKinds := make([]seedRow, 0, len(journal.JournalKinds()))
	for _, kind := range journal.JournalKinds() {
		journalKinds = append(journalKinds, seedRow{id: int(kind), name: kind.String()})
	}
	if current, err := scope.seedLookupCurrent(selectJournalKindsSQL, journalKinds); err != nil || !current {
		return false, err
	}
	if current, err := scope.seedLookupCurrent(selectAuthorityKindsSQL, []seedRow{{authKindBootstrapID, "bootstrap"}, {authKindAssignmentID, "assignment"}}); err != nil || !current {
		return false, err
	}
	if current, err := scope.seedLookupCurrent(selectAssignmentSlotsSQL, []seedRow{{slotOwnerResponsibilityID, string(journal.SlotOwnerResponsibility)}}); err != nil || !current {
		return false, err
	}
	if current, err := scope.seedLookupCurrent(selectAssignmentTransitionsSQL, []seedRow{{transitionStartedID, "started"}, {transitionEndedID, "ended"}}); err != nil || !current {
		return false, err
	}
	// completeJournalOperationFK rebuilds the journal table when its producing-
	// operation foreign key is absent; the rebuild is a write.
	fkPresent, err := scope.journalProducedByFKPresent()
	if err != nil || !fkPresent {
		return false, err
	}
	return true, nil
}

// seedLookupCurrent reports whether every expected (id, name) row already exists.
// Extra rows are ignored because INSERT OR IGNORE would not touch them.
func (scope *connScope) seedLookupCurrent(query string, expected []seedRow) (bool, error) {
	present := make(map[int]string, len(expected))
	err := scope.queryRows(query, nil, func(rows *sql.Rows) error {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		present[id] = name
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, want := range expected {
		if present[want.id] != want.name {
			return false, nil
		}
	}
	return true, nil
}

// activationMLModelsCurrent mirrors seedMLModels' own short-circuit: it writes
// nothing when the stored model count already meets or exceeds the caller's
// registry, so currency holds exactly when that count is at least len(models).
func (scope *connScope) activationMLModelsCurrent(models []ptypes.ModelEntry) (bool, error) {
	var existing int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM ml_models").Scan(&existing); err != nil {
		return false, err
	}
	return existing >= len(models), nil
}
