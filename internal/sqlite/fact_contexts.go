package sqlite

import (
	"fmt"
	"strconv"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
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
		return 0, factContextIntegrityError(
			"only one subtype-owned fact-context relation is present",
			"read-only fact-context schema classification",
			"restore both journal_decision_contexts and journal_evidence_contexts from the same schema generation")
	}
}

func (scope *connScope) validateFactContextTableShape(relation factContextRelation) error {
	type column struct {
		name     string
		typeName string
		pk       int
	}
	expected := []column{
		{name: relation.parentColumn(), typeName: "INTEGER", pk: 1},
		{name: "context_kind", typeName: "TEXT", pk: 2},
		{name: "context_identity", typeName: "TEXT", pk: 3},
	}
	var actual []column
	var nullable string
	if err := sqlitex.Execute(scope.conn, "SELECT * FROM pragma_table_info(?1)", &sqlitex.ExecOptions{
		Args: []any{relation.tableName()},
		ResultFunc: func(stmt *zs.Stmt) error {
			actual = append(actual, column{name: stmt.ColumnText(1), typeName: stmt.ColumnText(2), pk: stmt.ColumnInt(5)})
			if stmt.ColumnInt(3) == 0 {
				nullable = stmt.ColumnText(1)
			}
			return nil
		},
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

	strict, withoutRowID := false, false
	if err := sqlitex.Execute(scope.conn, "SELECT wr,strict FROM pragma_table_list WHERE schema=?1 AND name=?2", &sqlitex.ExecOptions{
		Args: []any{"main", relation.tableName()},
		ResultFunc: func(stmt *zs.Stmt) error {
			withoutRowID = stmt.ColumnInt(0) == 1
			strict = stmt.ColumnInt(1) == 1
			return nil
		},
	}); err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" table options", "fact-context schema classification", "restore the relation as STRICT, WITHOUT ROWID: "+err.Error())
	}
	if !strict || !withoutRowID {
		return factContextIntegrityError("malformed "+relation.tableName()+" storage mode", "fact-context schema classification", "restore the relation as STRICT, WITHOUT ROWID")
	}

	fkCount := 0
	fkValid := false
	if err := sqlitex.Execute(scope.conn, "SELECT * FROM pragma_foreign_key_list(?1)", &sqlitex.ExecOptions{
		Args: []any{relation.tableName()},
		ResultFunc: func(stmt *zs.Stmt) error {
			fkCount++
			fkValid = stmt.ColumnText(2) == relation.parentTable() && stmt.ColumnText(3) == relation.parentColumn() && stmt.ColumnText(4) == "journal_id"
			return nil
		},
	}); err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" foreign key", "fact-context schema classification", "restore the subtype-owned parent foreign key: "+err.Error())
	}
	if fkCount != 1 || !fkValid {
		return factContextIntegrityError("malformed "+relation.tableName()+" parent foreign key", "fact-context schema classification", "reference only "+relation.parentTable()+"(journal_id) from "+relation.parentColumn())
	}

	var triggers int
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM sqlite_master WHERE type=?1 AND tbl_name=?2", &sqlitex.ExecOptions{
		Args:       []any{"trigger", relation.tableName()},
		ResultFunc: func(stmt *zs.Stmt) error { triggers = stmt.ColumnInt(0); return nil },
	}); err != nil {
		return factContextIntegrityError("could not inspect "+relation.tableName()+" triggers", "fact-context schema classification", "restore the relation without triggers: "+err.Error())
	}
	if triggers != 0 {
		return factContextIntegrityError("malformed trigger on "+relation.tableName(), "fact-context schema classification", "remove the trigger and restore the static subtype foreign key relation")
	}
	return nil
}

// ensureFactContextRelations activates the two relations together. Legacy files
// have neither relation; canonical files already have both and are validated,
// never repaired. The caller owns Open's surrounding savepoint.
func (scope *connScope) ensureFactContextRelations() error {
	state, err := scope.classifyFactContextSchema()
	if err != nil {
		return err
	}
	if state == factContextSchemaCanonical {
		return nil
	}
	for _, relation := range []factContextRelation{factContextDecision, factContextEvidence} {
		if err := sqlitex.ExecuteTransient(scope.conn, relation.createDDL(), nil); err != nil {
			return fmt.Errorf("create %s: %w", relation.tableName(), err)
		}
	}
	if err := scope.backfillLegacyFactContexts(); err != nil {
		return err
	}
	return nil
}

// backfillLegacyFactContexts derives rows only from canonical operation bytes.
// Opaque legacy operations have no reconstructable context set, so they produce
// no synthetic context rows.
func (scope *connScope) backfillLegacyFactContexts() error {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return err
	}
	if isLegacyOperationsColumnSet(columns) {
		return nil
	}
	var operations []struct {
		anchor int64
		wire   []byte
	}
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT ?1 ORDER BY journal_id", &sqlitex.ExecOptions{
		Args: []any{nil},
		ResultFunc: func(stmt *zs.Stmt) error {
			operations = append(operations, struct {
				anchor int64
				wire   []byte
			}{anchor: stmt.ColumnInt64(0), wire: readBlob(stmt, 1)})
			return nil
		},
	}); err != nil {
		return fmt.Errorf("enumerate canonical operations for fact-context backfill: %w", err)
	}
	for _, operation := range operations {
		prepared, err := journal.DecodeCanonicalMutation(operation.wire)
		if err != nil {
			return fmt.Errorf("decode canonical operation %d for fact-context backfill: %w", operation.anchor, err)
		}
		effects := prepared.NormalizedEffects()
		var rows []int64
		if err := sqlitex.Execute(scope.conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", &sqlitex.ExecOptions{
			Args:       []any{operation.anchor},
			ResultFunc: func(stmt *zs.Stmt) error { rows = append(rows, stmt.ColumnInt64(0)); return nil },
		}); err != nil {
			return fmt.Errorf("enumerate operation %d rows for fact-context backfill: %w", operation.anchor, err)
		}
		if len(rows) != len(effects) {
			return factContextIntegrityError("canonical operation "+strconv.FormatInt(operation.anchor, 10)+" has "+strconv.Itoa(len(rows))+" produced rows for "+strconv.Itoa(len(effects))+" effects", "legacy fact-context backfill", "restore the operation closure and canonical mutation from the same committed backup")
		}
		for i, effect := range effects {
			relation, ok := factContextRelationForEffect(effect)
			if !ok {
				continue
			}
			if err := scope.persistFactContexts(relation, rows[i], effect.Contexts); err != nil {
				return fmt.Errorf("backfill canonical operation %d effect %d: %w", operation.anchor, i, err)
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
		if err := sqlitex.Execute(scope.conn, relation.insertSQL(), &sqlitex.ExecOptions{Args: []any{journalID, string(kind), identity}}); err != nil {
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
	contexts, err := scope.verifySelectedFactContext(relation, journalID)
	if err != nil {
		return nil, err
	}
	return contexts, nil
}

// requireCanonicalFactContextSchema is the runtime guard. The legacy state is
// only accepted by the read-only startup compatibility path before activation;
// a live store must always have both subtype-owned context relations.
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
// fact's immutable producing operation and effect, compares the complete
// canonical context set (including the empty set), and returns the decoded
// stored set only after that comparison succeeds. It intentionally does not
// enumerate unrelated journal history; startup, VerifyIntegrity, and replay
// retain those whole-database checks.
func (scope *connScope) verifySelectedFactContext(relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	var oppositeParent, oppositeContext bool
	if err := sqlitex.Execute(scope.conn, relation.oppositeParentSQL(), &sqlitex.ExecOptions{
		Args:       []any{journalID, 1},
		ResultFunc: func(*zs.Stmt) error { oppositeParent = true; return nil },
	}); err != nil {
		return nil, factContextIntegrityError("could not inspect the opposite fact subtype for journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore the fact to exactly one subtype table: "+err.Error())
	}
	if err := sqlitex.Execute(scope.conn, relation.oppositeContextSQL(), &sqlitex.ExecOptions{
		Args:       []any{journalID, 1},
		ResultFunc: func(*zs.Stmt) error { oppositeContext = true; return nil },
	}); err != nil {
		return nil, factContextIntegrityError("could not inspect opposite context rows for journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore contexts only in the selected subtype relation: "+err.Error())
	}
	if oppositeParent || oppositeContext {
		return nil, factContextIntegrityError("cross-subtype fact or context row for journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "remove the opposite subtype row and retain only the selected fact relation")
	}

	var (
		found               bool
		kindID              int
		operationRecordedAt int64
		producer            int64
		operationID         int64
		version             string
		wire                []byte
	)
	if err := sqlitex.Execute(scope.conn, relation.selectedParentSQL(), &sqlitex.ExecOptions{
		Args: []any{journalID},
		ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			kindID = stmt.ColumnInt(1)
			if stmt.ColumnType(3) == zs.TypeNull || stmt.ColumnType(4) == zs.TypeNull {
				return factContextIntegrityError("fact journal "+strconv.FormatInt(journalID, 10)+" has no producing operation", "selected fact-context validation", "restore the fact's producing operation and canonical mutation")
			}
			producer = stmt.ColumnInt64(3)
			operationID = stmt.ColumnInt64(4)
			if stmt.ColumnType(5) == zs.TypeNull || stmt.ColumnType(6) == zs.TypeNull {
				return factContextIntegrityError("opaque legacy operation owns selected fact journal "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore canonical operation bytes or remove the legacy fact before querying it")
			}
			version = stmt.ColumnText(5)
			wire = readBlob(stmt, 6)
			if stmt.ColumnType(7) == zs.TypeNull {
				return factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has no canonical operation timestamp", "selected fact-context validation", "restore the producing operation journal anchor")
			}
			operationRecordedAt = stmt.ColumnInt64(7)
			return nil
		},
	}); err != nil {
		return nil, err
	}
	if !found {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has no "+relation.parentTable()+" parent", "selected fact-context validation", "restore the subtype parent row before querying its contexts")
	}
	if kindID != int(relation.journalKind()) {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has kind "+strconv.Itoa(kindID)+" but belongs to "+relation.parentTable(), "selected fact-context validation", "restore the journal discriminator and matching subtype row")
	}
	if version == "" || len(wire) == 0 {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has an empty canonical operation", "selected fact-context validation", "restore both canonical mutation columns from the same committed operation")
	}
	prepared, err := journal.DecodeCanonicalMutation(wire)
	if err != nil {
		return nil, factContextIntegrityError("cannot decode producing operation "+strconv.FormatInt(operationID, 10)+" for selected fact "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore the operation's canonical mutation bytes: "+err.Error())
	}
	if version != prepared.EncodingVersion().String() {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has canonical encoding version "+version+" but decoded operation requires "+prepared.EncodingVersion().String(), "selected fact-context validation", "restore mutation_encoding_version together with canonical_mutation")
	}
	effects := prepared.NormalizedEffects()
	var rows []int64
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", &sqlitex.ExecOptions{
		Args:       []any{producer},
		ResultFunc: func(stmt *zs.Stmt) error { rows = append(rows, stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		return nil, factContextIntegrityError("could not load effect rows for producing operation "+strconv.FormatInt(producer, 10), "selected fact-context validation", "restore the operation's journal row closure: "+err.Error())
	}
	if len(rows) != len(effects) {
		return nil, factContextIntegrityError("producing operation "+strconv.FormatInt(producer, 10)+" has "+strconv.Itoa(len(rows))+" effect rows for "+strconv.Itoa(len(effects))+" canonical effects, selected fact "+strconv.FormatInt(journalID, 10), "selected fact-context validation", "restore the operation and all of its produced rows from one committed backup")
	}
	effectIndex := -1
	for i, rowID := range rows {
		if rowID == journalID {
			effectIndex = i
			break
		}
	}
	if effectIndex < 0 {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" is not produced by operation "+strconv.FormatInt(producer, 10), "selected fact-context validation", "restore the producing-operation pointer and effect-row closure")
	}
	effect := effects[effectIndex]
	expectedRelation, ok := factContextRelationForEffect(effect)
	if !ok || expectedRelation != relation {
		return nil, factContextIntegrityError("selected fact journal "+strconv.FormatInt(journalID, 10)+" has canonical effect subtype "+effect.Sort.String()+" but was loaded as "+relation.parentTable(), "selected fact-context validation", "restore the canonical effect and matching subtype relation")
	}
	if err := scope.validateCanonicalEffectRow(canonicalStoredOperation{anchor: producer, recordedAt: operationRecordedAt}, journalID, effect, false); err != nil {
		return nil, factContextIntegrityError("selected fact parent does not match its canonical effect", "selected fact-context validation", "restore the subtype parent from the producing operation: "+err.Error())
	}
	canonical, err := journal.CanonicalEventContexts(effect.Contexts)
	if err != nil {
		return nil, factContextIntegrityError("canonical contexts for selected fact journal "+strconv.FormatInt(journalID, 10)+" are invalid", "selected fact-context validation", "restore the producing operation's canonical mutation: "+err.Error())
	}
	actual, err := scope.loadFactContextsFromRelation(relation, journalID)
	if err != nil {
		return nil, err
	}
	if err := compareFactContextSets(producer, journalID, canonical, actual); err != nil {
		return nil, err
	}
	return actual, nil
}

func (scope *connScope) loadFactContextsFromRelation(relation factContextRelation, journalID int64) ([]journal.EventContext, error) {
	var contexts []journal.EventContext
	if err := sqlitex.Execute(scope.conn, relation.loadSQL(), &sqlitex.ExecOptions{
		Args: []any{journalID},
		ResultFunc: func(stmt *zs.Stmt) error {
			context, err := journal.DecodeStoredEventContext(journal.EventContextKind(stmt.ColumnText(0)), stmt.ColumnText(1))
			if err != nil {
				return factContextIntegrityError("malformed "+relation.tableName()+" row for journal "+strconv.FormatInt(journalID, 10), "verified fact-context load", "restore a canonical context kind and identity: "+err.Error())
			}
			contexts = append(contexts, context)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("load %s contexts for journal %d: %w", relation.tableName(), journalID, err)
	}
	return contexts, nil
}

func (scope *connScope) verifyFactContextIntegrity() error {
	return scope.verifyFactContextIntegrityMode(false)
}

// verifyFactContextIntegrityReadOnlyLegacyCompatible is used only by the
// pre-activation, read-only compatibility pass for e66 files. Once activation
// has created the subtype relations, all callers use the strict method above.
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
	result := func(stmt *zs.Stmt) error {
		journalID := stmt.ColumnInt64(0)
		if stmt.ColumnType(3) == zs.TypeNull {
			return factContextIntegrityError("context row for journal "+strconv.FormatInt(journalID, 10)+" has no "+relation.parentTable()+" parent", "fact-context topology verification", "restore the subtype parent row or remove the corrupt context row")
		}
		if stmt.ColumnType(4) == zs.TypeNull || stmt.ColumnInt(4) != int(relation.journalKind()) {
			return factContextIntegrityError("cross-subtype context row for journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "attach the context only to its matching "+relation.parentTable()+" parent")
		}
		if stmt.ColumnType(5) == zs.TypeNull || stmt.ColumnType(6) == zs.TypeNull {
			return factContextIntegrityError("context row for journal "+strconv.FormatInt(journalID, 10)+" has no producing operation", "fact-context topology verification", "restore the fact's producing operation and canonical mutation together")
		}
		versionNull := stmt.ColumnType(7) == zs.TypeNull
		wireNull := stmt.ColumnType(8) == zs.TypeNull
		if versionNull && wireNull {
			return factContextIntegrityError("opaque legacy operation owns a context row for journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "remove the synthetic context row; opaque legacy operations have no reconstructable contexts")
		}
		if versionNull != wireNull {
			return factContextIntegrityError("canonical operation has malformed version/bytes pair for context journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "restore both canonical columns from the same committed operation")
		}
		if _, err := journal.DecodeStoredEventContext(journal.EventContextKind(stmt.ColumnText(1)), stmt.ColumnText(2)); err != nil {
			return factContextIntegrityError("malformed context row for journal "+strconv.FormatInt(journalID, 10), "fact-context topology verification", "restore a canonical context kind and identity: "+err.Error())
		}
		return nil
	}
	if canonicalColumns {
		return sqlitex.Execute(scope.conn, relation.parentIntegritySQL(), &sqlitex.ExecOptions{ResultFunc: result})
	}
	return sqlitex.Execute(scope.conn, relation.legacyParentIntegritySQL(), &sqlitex.ExecOptions{Args: []any{nil, nil}, ResultFunc: result})
}

func (scope *connScope) validateCanonicalFactContextSets() error {
	columns, err := scope.tableColumns("journal_operations")
	if err != nil {
		return err
	}
	if isLegacyOperationsColumnSet(columns) {
		return nil
	}
	var operations []struct {
		anchor int64
		wire   []byte
	}
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT ?1 ORDER BY journal_id", &sqlitex.ExecOptions{
		Args: []any{nil},
		ResultFunc: func(stmt *zs.Stmt) error {
			operations = append(operations, struct {
				anchor int64
				wire   []byte
			}{anchor: stmt.ColumnInt64(0), wire: readBlob(stmt, 1)})
			return nil
		},
	}); err != nil {
		return fmt.Errorf("enumerate canonical operations for fact-context validation: %w", err)
	}
	for _, operation := range operations {
		prepared, err := journal.DecodeCanonicalMutation(operation.wire)
		if err != nil {
			return fmt.Errorf("decode canonical operation %d for fact-context validation: %w", operation.anchor, err)
		}
		effects := prepared.NormalizedEffects()
		var rows []int64
		if err := sqlitex.Execute(scope.conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", &sqlitex.ExecOptions{
			Args:       []any{operation.anchor},
			ResultFunc: func(stmt *zs.Stmt) error { rows = append(rows, stmt.ColumnInt64(0)); return nil },
		}); err != nil {
			return err
		}
		if len(rows) != len(effects) {
			// validateCanonicalOperations reports the primary row-closure corruption.
			continue
		}
		for i, effect := range effects {
			relation, ok := factContextRelationForEffect(effect)
			if !ok {
				continue
			}
			if err := scope.validateCanonicalFactContextSet(operation.anchor, rows[i], relation, effect.Contexts); err != nil {
				return err
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
