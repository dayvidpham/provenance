package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
)

// factContextRelation is the closed subtype dispatch for immutable fact
// contexts. A context belongs to the decision or evidence row it describes;
// it is never attached through a task-event row.
type factContextRelation uint8

const (
	factContextDecision factContextRelation = iota + 1
	factContextEvidence
)

type factContextSchemaState uint8

const (
	factContextSchemaLegacy factContextSchemaState = iota + 1
	factContextSchemaCanonical
)

func (relation factContextRelation) tableName() string {
	switch relation {
	case factContextDecision:
		return "journal_decision_contexts"
	case factContextEvidence:
		return "journal_evidence_contexts"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) parentTable() string {
	switch relation {
	case factContextDecision:
		return "journal_decisions"
	case factContextEvidence:
		return "journal_evidence"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) parentColumn() string {
	switch relation {
	case factContextDecision:
		return "decision_journal_id"
	case factContextEvidence:
		return "evidence_journal_id"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) journalKind() journal.JournalKind {
	switch relation {
	case factContextDecision:
		return journal.JournalKindDecision
	case factContextEvidence:
		return journal.JournalKindEvidence
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) selectedParentSQL() string {
	switch relation {
	case factContextDecision:
		return `SELECT j.journal_id,j.kind_id,j.recorded_at,j.produced_by_operation_journal_id,
			o.journal_id,o.mutation_encoding_version,o.canonical_mutation,opj.recorded_at
			FROM journal_decisions d
			JOIN journal j ON j.journal_id=d.journal_id
			LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id
			LEFT JOIN journal opj ON opj.journal_id=j.produced_by_operation_journal_id
			WHERE d.journal_id=?1`
	case factContextEvidence:
		return `SELECT j.journal_id,j.kind_id,j.recorded_at,j.produced_by_operation_journal_id,
			o.journal_id,o.mutation_encoding_version,o.canonical_mutation,opj.recorded_at
			FROM journal_evidence e
			JOIN journal j ON j.journal_id=e.journal_id
			LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id
			LEFT JOIN journal opj ON opj.journal_id=j.produced_by_operation_journal_id
			WHERE e.journal_id=?1`
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) oppositeParentSQL() string {
	switch relation {
	case factContextDecision:
		return "SELECT ?2 FROM journal_evidence WHERE journal_id=?1 LIMIT ?2"
	case factContextEvidence:
		return "SELECT ?2 FROM journal_decisions WHERE journal_id=?1 LIMIT ?2"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) oppositeContextSQL() string {
	switch relation {
	case factContextDecision:
		return "SELECT ?2 FROM journal_evidence_contexts WHERE evidence_journal_id=?1 LIMIT ?2"
	case factContextEvidence:
		return "SELECT ?2 FROM journal_decision_contexts WHERE decision_journal_id=?1 LIMIT ?2"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) createDDL() string {
	switch relation {
	case factContextDecision:
		return "CREATE TABLE IF NOT EXISTS journal_decision_contexts (decision_journal_id INTEGER NOT NULL REFERENCES journal_decisions(journal_id), context_kind TEXT NOT NULL, context_identity TEXT NOT NULL, PRIMARY KEY (decision_journal_id, context_kind, context_identity)) STRICT, WITHOUT ROWID"
	case factContextEvidence:
		return "CREATE TABLE IF NOT EXISTS journal_evidence_contexts (evidence_journal_id INTEGER NOT NULL REFERENCES journal_evidence(journal_id), context_kind TEXT NOT NULL, context_identity TEXT NOT NULL, PRIMARY KEY (evidence_journal_id, context_kind, context_identity)) STRICT, WITHOUT ROWID"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) insertSQL() string {
	switch relation {
	case factContextDecision:
		return "INSERT INTO journal_decision_contexts (decision_journal_id, context_kind, context_identity) VALUES (?1, ?2, ?3)"
	case factContextEvidence:
		return "INSERT INTO journal_evidence_contexts (evidence_journal_id, context_kind, context_identity) VALUES (?1, ?2, ?3)"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) loadSQL() string {
	switch relation {
	case factContextDecision:
		return "SELECT context_kind,context_identity FROM journal_decision_contexts WHERE decision_journal_id=?1 ORDER BY context_kind,context_identity"
	case factContextEvidence:
		return "SELECT context_kind,context_identity FROM journal_evidence_contexts WHERE evidence_journal_id=?1 ORDER BY context_kind,context_identity"
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) parentIntegritySQL() string {
	switch relation {
	case factContextDecision:
		return `SELECT c.decision_journal_id,c.context_kind,c.context_identity,d.journal_id,j.kind_id,
			j.produced_by_operation_journal_id,o.journal_id,o.mutation_encoding_version,o.canonical_mutation
			FROM journal_decision_contexts c
			LEFT JOIN journal_decisions d ON d.journal_id=c.decision_journal_id
			LEFT JOIN journal j ON j.journal_id=d.journal_id
			LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id`
	case factContextEvidence:
		return `SELECT c.evidence_journal_id,c.context_kind,c.context_identity,e.journal_id,j.kind_id,
			j.produced_by_operation_journal_id,o.journal_id,o.mutation_encoding_version,o.canonical_mutation
			FROM journal_evidence_contexts c
			LEFT JOIN journal_evidence e ON e.journal_id=c.evidence_journal_id
			LEFT JOIN journal j ON j.journal_id=e.journal_id
			LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id`
	default:
		panic("unknown factContextRelation")
	}
}

func (relation factContextRelation) legacyParentIntegritySQL() string {
	switch relation {
	case factContextDecision:
		return `SELECT c.decision_journal_id,c.context_kind,c.context_identity,d.journal_id,j.kind_id,
			j.produced_by_operation_journal_id,o.journal_id,?1,?2
			FROM journal_decision_contexts c
			LEFT JOIN journal_decisions d ON d.journal_id=c.decision_journal_id
			LEFT JOIN journal j ON j.journal_id=d.journal_id
			LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id`
	case factContextEvidence:
		return `SELECT c.evidence_journal_id,c.context_kind,c.context_identity,e.journal_id,j.kind_id,
			j.produced_by_operation_journal_id,o.journal_id,?1,?2
			FROM journal_evidence_contexts c
			LEFT JOIN journal_evidence e ON e.journal_id=c.evidence_journal_id
			LEFT JOIN journal j ON j.journal_id=e.journal_id
			LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id`
	default:
		panic("unknown factContextRelation")
	}
}

func factContextRelationForEffect(effect journal.Effect) (factContextRelation, bool) {
	switch effect.Sort {
	case journal.EffectDecision:
		return factContextDecision, true
	case journal.EffectEvidence:
		return factContextEvidence, true
	default:
		return 0, false
	}
}

func factContextIntegrityError(problem, where, fix string) error {
	return fmt.Errorf("%w: %s — where: %s; when: fact-context validation; impact: the database is rejected rather than returning or replaying an incomplete fact; fix: %s", journal.ErrFactContextIntegrity, problem, where, fix)
}

func forEachFactContextRow(scope *connScope, query string, args []any, visit func(*sql.Rows) error) error {
	rows, err := scope.conn.QueryContext(scope.ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := visit(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func factContextExists(scope *connScope, query string, args ...any) (bool, error) {
	var present int
	err := scope.conn.QueryRowContext(scope.ctx, query, args...).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// classifyFactContextSchema is deliberately read-only. Open calls it before
// ensureSchema so a partial or malformed deployment never reaches CREATE TABLE.
func (scope *connScope) classifyFactContextSchema() (factContextSchemaState, error) {
	decisionPresent, err := scope.tableExists(factContextDecision.tableName())
	if err != nil {
		return 0, err
	}
	evidencePresent, err := scope.tableExists(factContextEvidence.tableName())
	if err != nil {
		return 0, err
	}
	switch {
	case !decisionPresent && !evidencePresent:
		return factContextSchemaLegacy, nil
	case decisionPresent && evidencePresent:
		for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
			if err := scope.validateFactContextTableShape(relation); err != nil {
				return 0, err
			}
		}
		return factContextSchemaCanonical, nil
	default:
		return 0, factContextIntegrityError("only one subtype-owned fact-context relation is present", "read-only fact-context schema classification", "restore both journal_decision_contexts and journal_evidence_contexts from the same schema generation")
	}
}

func (scope *connScope) validateFactContextTableShape(relation factContextRelation) error {
	type column struct {
		name, typeName string
		pk             int
	}
	expected := []column{{relation.parentColumn(), "INTEGER", 1}, {"context_kind", "TEXT", 2}, {"context_identity", "TEXT", 3}}
	var actual []column
	var nullable string
	if err := forEachFactContextRow(scope, "SELECT * FROM pragma_table_info(?1)", []any{relation.tableName()}, func(row *sql.Rows) error {
		var cid, notNull, pk int
		var name, typeName string
		var defaultValue any
		if err := row.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		actual = append(actual, column{name, typeName, pk})
		if notNull == 0 {
			nullable = name
		}
		return nil
	}); err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" columns", "fact-context schema classification", "restore the relation from a known-good schema: "+err.Error())
	}
	if len(actual) != len(expected) {
		return factContextIntegrityError("malformed "+relation.tableName()+" column count "+strconv.Itoa(len(actual)), "fact-context schema classification", "restore exactly the parent JournalID, context_kind, and context_identity columns")
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return factContextIntegrityError("malformed "+relation.tableName()+" column "+strconv.Itoa(i+1), "fact-context schema classification", "restore the exact ordered compound primary-key schema")
		}
	}
	if nullable != "" {
		return factContextIntegrityError("malformed nullable "+relation.tableName()+"."+nullable, "fact-context schema classification", "restore NOT NULL columns in the immutable context relation")
	}
	var withoutRowID, strict int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT wr,strict FROM pragma_table_list WHERE schema=?1 AND name=?2", "main", relation.tableName()).Scan(&withoutRowID, &strict)
	if err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" table options", "fact-context schema classification", "restore the relation as STRICT, WITHOUT ROWID: "+err.Error())
	}
	if strict != 1 || withoutRowID != 1 {
		return factContextIntegrityError("malformed "+relation.tableName()+" storage mode", "fact-context schema classification", "restore the relation as STRICT, WITHOUT ROWID")
	}
	fkCount, fkValid := 0, false
	if err := forEachFactContextRow(scope, "SELECT * FROM pragma_foreign_key_list(?1)", []any{relation.tableName()}, func(row *sql.Rows) error {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := row.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return err
		}
		fkCount++
		fkValid = table == relation.parentTable() && from == relation.parentColumn() && to == "journal_id"
		return nil
	}); err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" foreign key", "fact-context schema classification", "restore the subtype-owned parent foreign key: "+err.Error())
	}
	if fkCount != 1 || !fkValid {
		return factContextIntegrityError("malformed "+relation.tableName()+" parent foreign key", "fact-context schema classification", "reference only "+relation.parentTable()+"(journal_id) from "+relation.parentColumn())
	}
	var triggers int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type=?1 AND tbl_name=?2", "trigger", relation.tableName()).Scan(&triggers); err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" triggers", "fact-context schema classification", "restore the relation without triggers: "+err.Error())
	}
	if triggers != 0 {
		return factContextIntegrityError("malformed trigger on "+relation.tableName(), "fact-context schema classification", "remove the trigger and restore the static subtype foreign key relation")
	}
	return nil
}

// ensureFactContextRelations activates the two relations together. Legacy files
// have neither relation; canonical files already have both and are validated,
// never repaired. The caller owns Open's surrounding transaction.
func (scope *connScope) ensureFactContextRelations() error {
	state, err := scope.classifyFactContextSchema()
	if err != nil || state == factContextSchemaCanonical {
		return err
	}
	for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
		if _, err := scope.conn.ExecContext(scope.ctx, relation.createDDL()); err != nil {
			return fmt.Errorf("create %s: %w", relation.tableName(), err)
		}
	}
	return scope.backfillLegacyFactContexts()
}

type canonicalFactOperation struct {
	anchor int64
	wire   []byte
}

func (scope *connScope) canonicalFactOperations(where string) ([]canonicalFactOperation, error) {
	operations := make([]canonicalFactOperation, 0)
	err := forEachFactContextRow(scope, "SELECT journal_id,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT ?1 ORDER BY journal_id", []any{nil}, func(row *sql.Rows) error {
		var operation canonicalFactOperation
		if err := row.Scan(&operation.anchor, &operation.wire); err != nil {
			return err
		}
		operation.wire = append([]byte(nil), operation.wire...)
		operations = append(operations, operation)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate canonical operations for %s: %w", where, err)
	}
	return operations, nil
}

func (scope *connScope) producedFactRows(anchor int64) ([]int64, error) {
	rows := make([]int64, 0)
	err := forEachFactContextRow(scope, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", []any{anchor}, func(row *sql.Rows) error {
		var journalID int64
		if err := row.Scan(&journalID); err != nil {
			return err
		}
		rows = append(rows, journalID)
		return nil
	})
	return rows, err
}

// backfillLegacyFactContexts derives rows only from canonical operation bytes.
// Opaque legacy operations have no reconstructable context set, so they produce
// no synthetic context rows.
func (scope *connScope) backfillLegacyFactContexts() error {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil || isLegacyOperationsColumnSet(columns) {
		return err
	}
	operations, err := scope.canonicalFactOperations("fact-context backfill")
	if err != nil {
		return err
	}
	for _, operation := range operations {
		prepared, err := journal.DecodeCanonicalMutation(operation.wire)
		if err != nil {
			return fmt.Errorf("decode canonical operation %d for fact-context backfill: %w — where: backfillLegacyFactContexts; when: startup, while deriving fact contexts from stored canonical operations; impact: the open fails closed and no synthetic context row is written; fix: %s", operation.anchor, err, unsupportedPreV004DatabaseFix)
		}
		effects := prepared.NormalizedEffects()
		rows, err := scope.producedFactRows(operation.anchor)
		if err != nil {
			return fmt.Errorf("enumerate operation %d rows for fact-context backfill: %w", operation.anchor, err)
		}
		if len(rows) != len(effects) {
			return factContextIntegrityError("canonical operation "+strconv.FormatInt(operation.anchor, 10)+" has "+strconv.Itoa(len(rows))+" produced rows for "+strconv.Itoa(len(effects))+" effects", "legacy fact-context backfill", "restore the operation closure and canonical mutation from the same committed backup")
		}
		for i, effect := range effects {
			relation, ok := factContextRelationForEffect(effect)
			if ok {
				if err := scope.persistFactContexts(relation, rows[i], effect.Contexts); err != nil {
					return fmt.Errorf("backfill canonical operation %d effect %d: %w", operation.anchor, i, err)
				}
			}
		}
	}
	return nil
}

func (scope *connScope) persistFactContexts(relation factContextRelation, journalID int64, contexts []journal.EventContext) error {
	canonical, err := journal.CanonicalEventContexts(contexts)
	if err != nil {
		return fmt.Errorf("canonical %s contexts: %w", relation.tableName(), err)
	}
	for _, context := range canonical {
		kind, identity, err := journal.EncodeStoredEventContext(context)
		if err != nil {
			return fmt.Errorf("encode %s context: %w", relation.tableName(), err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, relation.insertSQL(), journalID, string(kind), identity); err != nil {
			return fmt.Errorf("insert %s context for journal %d: %w", relation.tableName(), journalID, err)
		}
	}
	return nil
}

// loadVerifiedFactContexts is the storage contract consumed by the fact-query
// vertical. It decodes only the selected subtype relation and rejects malformed
// stored values instead of omitting them.
func (scope *connScope) loadVerifiedFactContexts(relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	if err := scope.requireCanonicalFactContextSchema("verified fact-context load"); err != nil {
		return nil, err
	}
	return scope.verifySelectedFactContext(relation, journalID)
}

func (scope *connScope) requireCanonicalFactContextSchema(where string) error {
	state, err := scope.classifyFactContextSchema()
	if err != nil {
		return err
	}
	if state != factContextSchemaCanonical {
		return factContextIntegrityError("both subtype-owned fact-context relations are missing", where, "restore journal_decision_contexts and journal_evidence_contexts, then reopen the store")
	}
	return nil
}

// verifySelectedFactContext validates only one candidate fact. It resolves the
// immutable producing operation and effect, compares its complete canonical
// context set, and returns the stored set only after that comparison succeeds.
func (scope *connScope) verifySelectedFactContext(relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	return verifySelectedFactContextInTransaction(scope.ctx, allocationSQLTx{conn: scope.conn}, relation, journalID)
}

// verifySelectedFactContextInTransaction is the complete transaction-neutral
// selected-fact proof shared by ordinary Apply and governed composition. It
// authenticates the subtype row, producing operation, canonical effect order,
// exact subtype payload, and complete context set before a condition may win.
func verifySelectedFactContextInTransaction(ctx context.Context, reader fusedtx.SQLReader, relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	exists := func(query string, args ...any) (bool, error) {
		var present int
		err := reader.QueryRow(ctx, query, args...).Scan(&present)
		if fusedtx.IsNoRows(err) {
			return false, nil
		}
		return err == nil, err
	}
	oppositeParent, err := exists(relation.oppositeParentSQL(), journalID, 1)
	if err != nil {
		return nil, factContextIntegrityError("could not inspect the opposite fact subtype for journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore the fact to exactly one subtype table: "+err.Error())
	}
	oppositeContext, err := exists(relation.oppositeContextSQL(), journalID, 1)
	if err != nil {
		return nil, factContextIntegrityError("could not inspect opposite context rows for journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore contexts only in the selected subtype relation: "+err.Error())
	}
	if oppositeParent || oppositeContext {
		return nil, factContextIntegrityError("cross-subtype fact or context row for journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "remove the opposite subtype row and retain only the selected fact relation")
	}

	var kindID, ignoredJournalID, ignoredRecordedAt sql.NullInt64
	var producer, operationID, operationRecordedAt sql.NullInt64
	var version sql.NullString
	var wire []byte
	err = reader.QueryRow(ctx, relation.selectedParentSQL(), journalID).Scan(&ignoredJournalID, &kindID, &ignoredRecordedAt, &producer, &operationID, &version, &wire, &operationRecordedAt)
	if fusedtx.IsNoRows(err) {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has no "+relation.parentTable()+" parent", "selected fact-context validation", "restore the subtype parent row before querying its contexts")
	}
	if err != nil {
		return nil, err
	}
	if !producer.Valid || !operationID.Valid || producer.Int64 != operationID.Int64 {
		return nil, factContextIntegrityError("fact journal "+strconv.FormatInt(journalID, 10)+" has no producing operation", "selected fact-context validation", "restore the fact's producing operation and canonical mutation")
	}
	if !version.Valid || wire == nil {
		return nil, factContextIntegrityError("opaque legacy operation owns selected fact journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore canonical operation bytes or remove the legacy fact before querying it")
	}
	if !operationRecordedAt.Valid {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has no canonical operation timestamp", "selected fact-context validation", "restore the producing operation journal anchor")
	}
	if !kindID.Valid || kindID.Int64 != int64(relation.journalKind()) {
		kindValue := "NULL"
		if kindID.Valid {
			kindValue = strconv.FormatInt(kindID.Int64, 10)
		}
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has kind "+kindValue+" but belongs to "+relation.parentTable(), "selected fact-context validation", "restore the journal discriminator and matching subtype row")
	}
	if version.String == "" || len(wire) == 0 {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has an empty canonical operation", "selected fact-context validation", "restore both canonical mutation columns from the same committed operation")
	}
	prepared, err := journal.DecodeCanonicalMutation(wire)
	if err != nil {
		return nil, factContextIntegrityError("cannot decode producing operation "+strconv.FormatInt(operationID.Int64, 10)+" for selected fact "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore the operation's canonical mutation bytes: "+err.Error())
	}
	if version.String != prepared.EncodingVersion().String() {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has canonical encoding version "+version.String+" but decoded operation requires "+prepared.EncodingVersion().String(), "selected fact-context validation", "restore mutation_encoding_version together with canonical_mutation")
	}
	effects := prepared.NormalizedEffects()
	producedRows, err := reader.Query(ctx, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", producer.Int64)
	if err != nil {
		return nil, factContextIntegrityError("could not load effect rows for producing operation "+strconv.FormatInt(producer.Int64, 10), "selected fact-context validation", "restore the operation's journal row closure: "+err.Error())
	}
	rows := make([]int64, 0, len(effects))
	for producedRows.Next() {
		var row int64
		if err := producedRows.Scan(&row); err != nil {
			_ = producedRows.Close()
			return nil, factContextIntegrityError("could not decode effect rows for producing operation "+strconv.FormatInt(producer.Int64, 10), "selected fact-context validation", "restore the operation's journal row closure: "+err.Error())
		}
		rows = append(rows, row)
	}
	if err := producedRows.Err(); err != nil {
		_ = producedRows.Close()
		return nil, factContextIntegrityError("could not iterate effect rows for producing operation "+strconv.FormatInt(producer.Int64, 10), "selected fact-context validation", "restore the operation's journal row closure: "+err.Error())
	}
	if err := producedRows.Close(); err != nil {
		return nil, factContextIntegrityError("could not close effect rows for producing operation "+strconv.FormatInt(producer.Int64, 10), "selected fact-context validation", "retry after repairing the database reader: "+err.Error())
	}
	if len(rows) != len(effects) {
		return nil, factContextIntegrityError("producing operation "+strconv.FormatInt(producer.Int64, 10)+" has "+strconv.Itoa(len(rows))+" effect rows for "+strconv.Itoa(len(effects))+" canonical effects, selected fact "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore the operation and all of its produced rows from one committed backup")
	}
	effectIndex := -1
	for i, rowID := range rows {
		if rowID == journalID {
			effectIndex = i
			break
		}
	}
	if effectIndex < 0 {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" is not produced by operation "+strconv.FormatInt(producer.Int64, 10), "selected fact-context validation", "restore the producing-operation pointer and effect-row closure")
	}
	effect := effects[effectIndex]
	expectedRelation, ok := factContextRelationForEffect(effect)
	if !ok || expectedRelation != relation {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has canonical effect subtype "+effect.Sort.String()+" but was loaded as "+relation.parentTable(), "selected fact-context validation", "restore the canonical effect and matching subtype relation")
	}
	if err := validateSelectedCanonicalFactRow(ctx, reader, producer.Int64, operationRecordedAt.Int64, journalID, relation, effect); err != nil {
		return nil, factContextIntegrityError("selected fact parent does not match its canonical effect", "selected fact-context validation", "restore the subtype parent from the producing operation: "+err.Error())
	}
	canonical, err := journal.CanonicalEventContexts(effect.Contexts)
	if err != nil {
		return nil, factContextIntegrityError("canonical contexts for selected fact journal "+strconv.FormatInt(journalID, 10)+" are invalid", "selected fact-context validation", "restore the producing operation's canonical mutation: "+err.Error())
	}
	actual, err := loadFactContextsFromReader(ctx, reader, relation, journalID)
	if err != nil {
		return nil, err
	}
	if err := compareFactContextSets(producer.Int64, journalID, canonical, actual); err != nil {
		return nil, err
	}
	return actual, nil
}

func validateSelectedCanonicalFactRow(ctx context.Context, reader fusedtx.SQLReader, anchor, operationRecordedAt, journalID int64, relation factContextRelation, effect journal.Effect) error {
	expectedRecordedAt := operationRecordedAt
	if effect.RecordedAtOverride != nil {
		expectedRecordedAt = *effect.RecordedAtOverride
	}
	var kind int
	var recordedAt int64
	if err := reader.QueryRow(ctx, "SELECT kind_id,recorded_at FROM journal WHERE journal_id=?1", journalID).Scan(&kind, &recordedAt); err != nil {
		return err
	}
	if kind != int(relation.journalKind()) || recordedAt != expectedRecordedAt {
		return fmt.Errorf("operation %d fact row %d has non-canonical kind or ordering timestamp", anchor, journalID)
	}
	payload := effect.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	var storedKind, storedPayload string
	var storedTask sql.NullString
	switch relation {
	case factContextDecision:
		if err := reader.QueryRow(ctx, "SELECT decision_kind,task_id,payload FROM journal_decisions WHERE journal_id=?1", journalID).Scan(&storedKind, &storedTask, &storedPayload); err != nil {
			return err
		}
		if storedKind != string(effect.DecisionKind) {
			return fmt.Errorf("decision kind differs from canonical effect")
		}
	case factContextEvidence:
		var digest []byte
		if err := reader.QueryRow(ctx, "SELECT evidence_kind,task_id,content_digest,payload FROM journal_evidence WHERE journal_id=?1", journalID).Scan(&storedKind, &storedTask, &digest, &storedPayload); err != nil {
			return err
		}
		if storedKind != string(effect.EvidenceKind) || !bytes.Equal(digest, effect.ContentDigest) {
			return fmt.Errorf("evidence kind or digest differs from canonical effect")
		}
	}
	expectedTask := optionalTaskString(effect.TaskID)
	if storedTask.Valid != (expectedTask != "") || (storedTask.Valid && storedTask.String != expectedTask) || storedPayload != string(payload) {
		return fmt.Errorf("fact task or payload differs from canonical effect")
	}
	return nil
}

func loadFactContextsFromReader(ctx context.Context, reader fusedtx.SQLReader, relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	rows, err := reader.Query(ctx, relation.loadSQL(), journalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contexts := make([]journal.EventContext, 0)
	for rows.Next() {
		var kind, identity string
		if err := rows.Scan(&kind, &identity); err != nil {
			return nil, err
		}
		value, err := journal.DecodeStoredEventContext(journal.EventContextKind(kind), identity)
		if err != nil {
			return nil, factContextIntegrityError("malformed "+relation.tableName()+" row for journal "+strconv.FormatInt(journalID, 10), "verified fact-context load", "restore a canonical context kind and identity: "+err.Error())
		}
		contexts = append(contexts, value)
	}
	return contexts, rows.Err()
}

func (scope *connScope) loadFactContextsFromRelation(relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	contexts := make([]journal.EventContext, 0)
	err := forEachFactContextRow(scope, relation.loadSQL(), []any{journalID}, func(row *sql.Rows) error {
		var kind, identity string
		if err := row.Scan(&kind, &identity); err != nil {
			return err
		}
		context, err := journal.DecodeStoredEventContext(journal.EventContextKind(kind), identity)
		if err != nil {
			return factContextIntegrityError("malformed "+relation.tableName()+" row for journal "+strconv.FormatInt(journalID, 10), "verified fact-context load", "restore a canonical context kind and identity: "+err.Error())
		}
		contexts = append(contexts, context)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load %s contexts for journal %d: %w", relation.tableName(), journalID, err)
	}
	return contexts, nil
}

func (scope *connScope) verifyFactContextIntegrity() error {
	return scope.verifyFactContextIntegrityMode(false)
}

// verifyFactContextIntegrityReadOnlyLegacyCompatible is used only by the
// pre-activation, read-only compatibility pass for e66 files.
func (scope *connScope) verifyFactContextIntegrityReadOnlyLegacyCompatible() error {
	return scope.verifyFactContextIntegrityMode(true)
}

func (scope *connScope) verifyFactContextIntegrityMode(allowLegacy bool) error {
	state, err := scope.classifyFactContextSchema()
	if err != nil {
		return err
	}
	if state == factContextSchemaLegacy {
		if allowLegacy {
			return nil
		}
		return factContextIntegrityError("both subtype-owned fact-context relations are missing", "fact-context topology verification", "restore both relations or reopen this file only as an unactivated e66 legacy database")
	}
	if err := scope.requireCanonicalFactContextSchema("fact-context topology verification"); err != nil {
		return err
	}
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return err
	}
	_, hasVersion := columns["mutation_encoding_version"]
	_, hasMutation := columns["canonical_mutation"]
	if hasVersion != hasMutation {
		return factContextIntegrityError("journal_operations has a one-column canonical shape", "fact-context topology verification", "restore mutation_encoding_version and canonical_mutation as an atomic column pair")
	}
	for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
		if err := scope.verifyFactContextParents(relation, hasVersion); err != nil {
			return err
		}
	}
	return scope.validateCanonicalFactContextSets()
}

func (scope *connScope) verifyFactContextParents(relation factContextRelation, canonicalColumns bool) error {
	query, args := relation.parentIntegritySQL(), []any(nil)
	if !canonicalColumns {
		query, args = relation.legacyParentIntegritySQL(), []any{nil, nil}
	}
	return forEachFactContextRow(scope, query, args, func(row *sql.Rows) error {
		var journalID int64
		var contextKind, contextIdentity string
		var parent, kindID, producer, operationID sql.NullInt64
		var version sql.NullString
		var wire []byte
		if err := row.Scan(&journalID, &contextKind, &contextIdentity, &parent, &kindID, &producer, &operationID, &version, &wire); err != nil {
			return err
		}
		if !parent.Valid {
			return factContextIntegrityError("context row for journal "+strconv.FormatInt(journalID, 10)+" has no "+relation.parentTable()+" parent", "fact-context topology verification", "restore the subtype parent row or remove the corrupt context row")
		}
		if !kindID.Valid || kindID.Int64 != int64(relation.journalKind()) {
			return factContextIntegrityError("cross-subtype context row for journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "attach the context only to its matching "+relation.parentTable()+" parent")
		}
		if !producer.Valid || !operationID.Valid {
			return factContextIntegrityError("context row for journal "+strconv.FormatInt(journalID, 10)+" has no producing operation", "fact-context topology verification", "restore the fact's producing operation and canonical mutation together")
		}
		if !version.Valid && wire == nil {
			return factContextIntegrityError("opaque legacy operation owns a context row for journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "remove the synthetic context row; opaque legacy operations have no reconstructable contexts")
		}
		if version.Valid != (wire != nil) {
			return factContextIntegrityError("canonical operation has malformed version/bytes pair for context journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "restore both canonical columns from the same committed operation")
		}
		if _, err := journal.DecodeStoredEventContext(journal.EventContextKind(contextKind), contextIdentity); err != nil {
			return factContextIntegrityError("malformed context row for journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "restore a canonical context kind and identity: "+err.Error())
		}
		return nil
	})
}

func (scope *connScope) validateCanonicalFactContextSets() error {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil || isLegacyOperationsColumnSet(columns) {
		return err
	}
	operations, err := scope.canonicalFactOperations("fact-context validation")
	if err != nil {
		return err
	}
	for _, operation := range operations {
		prepared, err := journal.DecodeCanonicalMutation(operation.wire)
		if err != nil {
			return fmt.Errorf("decode canonical operation %d for fact-context validation: %w — where: validateCanonicalFactContextSets; when: startup fact-context validation over stored canonical operations; impact: the open fails closed and no fact-context set is accepted; fix: %s", operation.anchor, err, unsupportedPreV004DatabaseFix)
		}
		effects := prepared.NormalizedEffects()
		rows, err := scope.producedFactRows(operation.anchor)
		if err != nil {
			return err
		}
		if len(rows) != len(effects) {
			continue
		}
		for i, effect := range effects {
			relation, ok := factContextRelationForEffect(effect)
			if ok {
				if err := scope.validateCanonicalFactContextSet(operation.anchor, rows[i], relation, effect.Contexts); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (scope *connScope) validateCanonicalFactContextSet(anchor, journalID int64, relation factContextRelation, expectedInput []journal.EventContext) error {
	expected, err := journal.CanonicalEventContexts(expectedInput)
	if err != nil {
		return factContextIntegrityError("canonical contexts for journal "+strconv.FormatInt(journalID, 10)+" are invalid", "canonical fact-context comparison", "restore the operation's canonical mutation: "+err.Error())
	}
	actual, err := scope.loadFactContextsFromRelation(relation, journalID)
	if err != nil {
		return err
	}
	return compareFactContextSets(anchor, journalID, expected, actual)
}

func compareFactContextSets(anchor, journalID int64, expected, actual []journal.EventContext) error {
	if len(actual) != len(expected) {
		return factContextIntegrityError("context set mismatch for journal "+strconv.FormatInt(journalID, 10)+" (stored="+strconv.Itoa(len(actual))+", canonical="+strconv.Itoa(len(expected))+")", "canonical fact-context comparison for operation "+strconv.FormatInt(anchor, 10), "restore exactly the canonical context rows for this fact")
	}
	for i := range expected {
		expectedKind, expectedIdentity, _ := journal.EncodeStoredEventContext(expected[i])
		actualKind, actualIdentity, _ := journal.EncodeStoredEventContext(actual[i])
		if expectedKind != actualKind || expectedIdentity != actualIdentity {
			return factContextIntegrityError("context set mismatch for journal "+strconv.FormatInt(journalID, 10), "canonical fact-context comparison for operation "+strconv.FormatInt(anchor, 10), "restore the exact canonical context kind and identity set")
		}
	}
	return nil
}
