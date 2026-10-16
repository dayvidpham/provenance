package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
)

// exactReplayFactContextsVerified reuses the durable whole-store proof only at
// the exact-replay boundary. The caller owns the enclosing transaction snapshot
// (Apply's BEGIN IMMEDIATE, or the enclosing fold transaction). Nothing is cached
// on a scope: a preceding new fold in a mixed batch changes the generation, so
// its subsequent replay runs the original full gate. No ledger is published here.
//
// DML on every input of fact-context verification advances the generation in the
// same transaction. DDL does not: always recheck the activation structural probes,
// exact context-table definitions, and actual generation-trigger definitions.
// Names alone cannot prove coverage after a same-name no-op replacement. Extra
// host tables are permitted; activationSchemaIsPristine is deliberately not used.
// Any uncertainty retains the original full check and its canonical errors.
func (scope *connScope) exactReplayFactContextsVerified() bool {
	if scope.ephemeralStore || !scope.verifiedLedgerMatches() {
		return false
	}
	for _, check := range []func() (bool, error){
		scope.activationSchemaObjectsCurrent,
		scope.activationIDFormatCurrent,
		scope.activationOperationsSchemaCurrent,
		scope.activationFactContextSchemaCurrent,
		scope.activationReferenceSeedsCurrent,
		scope.activationSpineShapeCurrent,
		scope.exactReplayLedgerDefinitionsCurrent,
	} {
		if current, err := check(); err != nil || !current {
			return false
		}
	}
	return true
}

func (scope *connScope) exactReplayLedgerDefinitionsCurrent() (bool, error) {
	definitions := make(map[string]string)
	err := scope.queryRows("SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type IN ('trigger', 'table')", nil, func(rows *sql.Rows) error {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			return err
		}
		definitions[name] = definition
		return nil
	})
	if err != nil {
		return false, err
	}
	for table, ddl := range map[string]string{storeGenerationTable: verifiedGenerationTableDDL, verifiedStoreTable: verifiedLedgerTableDDL} {
		if definitions[table] != strings.Replace(ddl, "IF NOT EXISTS ", "", 1) {
			return false, nil
		}
	}
	for _, table := range verifiedCoveredTableNames {
		for _, event := range verifiedTriggerEvents {
			expected := fmt.Sprintf("CREATE TRIGGER provenance_store_generation_on_%s_%s AFTER %s ON %s BEGIN UPDATE provenance_store_generation SET generation = generation + 1 WHERE id = 1; END", table, event, event, table)
			if definitions[verifiedTriggerName(table, event)] != expected {
				return false, nil
			}
		}
	}
	for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
		// SQLite omits IF NOT EXISTS from its stored definition. Exact comparison
		// intentionally falls back on equivalent but noncanonical DDL as well.
		expected := strings.Replace(relation.createDDL(), "IF NOT EXISTS ", "", 1)
		if definitions[relation.tableName()] != expected {
			return false, nil
		}
	}
	return true, nil
}
