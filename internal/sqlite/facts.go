package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// factQuerySnapshotBarrierHook is nil in production. Package tests install it
// around one deterministic reader/writer barrier to prove that the transaction
// snapshot is acquired before the bounded page SQL runs.
var factQuerySnapshotBarrierHook func(factSelectorKind, int64)

type factPageRow struct {
	journalID                   journal.JournalID
	recordedAt                  time.Time
	taskID                      *journal.TaskID
	kind                        string
	digest                      []byte
	payload                     []byte
	contexts                    []journal.EventContext
	effectiveActorID            journal.ActorID
	producingOperationID        journal.OperationID
	producingOperationJournalID journal.JournalID
}

// Facts exposes the read-only fact query surface on the same opened store as
// the mutation journal.
func (db *DB) Facts() journal.FactQueryAPI { return db }

var _ journal.FactQueryAPI = (*DB)(nil)

func (db *DB) QueryDecisions(q journal.DecisionQuery) (journal.DecisionPage, error) {
	if err := q.Validate(); err != nil {
		return journal.DecisionPage{}, fmt.Errorf("QueryDecisions: %w", err)
	}
	kinds := make([]string, len(q.Kinds))
	for i, kind := range q.Kinds {
		kinds[i] = string(kind)
	}
	rows, snapshot, next, err := db.queryFacts(factSelectorDecision, q.Page, q.Filter, kinds)
	if err != nil {
		return journal.DecisionPage{}, fmt.Errorf("QueryDecisions: %w", err)
	}
	page := journal.DecisionPage{SnapshotMaxJournalID: snapshot, Next: next}
	page.Rows = make([]journal.DecisionRow, 0, len(rows))
	for _, row := range rows {
		page.Rows = append(page.Rows, journal.DecisionRow{
			JournalID:                   row.journalID,
			RecordedAt:                  row.recordedAt,
			TaskID:                      row.taskID,
			DecisionKind:                journal.DecisionKind(row.kind),
			Payload:                     append([]byte(nil), row.payload...),
			Contexts:                    append([]journal.EventContext(nil), row.contexts...),
			EffectiveActorID:            row.effectiveActorID,
			ProducingOperationID:        row.producingOperationID,
			ProducingOperationJournalID: row.producingOperationJournalID,
		})
	}
	return page, nil
}

func (db *DB) QueryEvidence(q journal.EvidenceQuery) (journal.EvidencePage, error) {
	if err := q.Validate(); err != nil {
		return journal.EvidencePage{}, fmt.Errorf("QueryEvidence: %w", err)
	}
	kinds := make([]string, len(q.Kinds))
	for i, kind := range q.Kinds {
		kinds[i] = string(kind)
	}
	rows, snapshot, next, err := db.queryFacts(factSelectorEvidence, q.Page, q.Filter, kinds)
	if err != nil {
		return journal.EvidencePage{}, fmt.Errorf("QueryEvidence: %w", err)
	}
	page := journal.EvidencePage{SnapshotMaxJournalID: snapshot, Next: next}
	page.Rows = make([]journal.EvidenceRow, 0, len(rows))
	for _, row := range rows {
		page.Rows = append(page.Rows, journal.EvidenceRow{
			JournalID:                   row.journalID,
			RecordedAt:                  row.recordedAt,
			TaskID:                      row.taskID,
			EvidenceKind:                journal.EvidenceKind(row.kind),
			ContentDigest:               append([]byte(nil), row.digest...),
			Payload:                     append([]byte(nil), row.payload...),
			Contexts:                    append([]journal.EventContext(nil), row.contexts...),
			EffectiveActorID:            row.effectiveActorID,
			ProducingOperationID:        row.producingOperationID,
			ProducingOperationJournalID: row.producingOperationJournalID,
		})
	}
	return page, nil
}

// normalizeFactQueryFilter is retained as the matcher boundary used by
// buildFactPageMatchBinding and condition selectors. Query execution itself
// never binds filter dimensions independently of that shared binding.
func normalizeFactQueryFilter(in journal.FactFilter) (journal.FactFilter, error) {
	contexts, err := journal.CanonicalEventContexts(in.RequiredContexts)
	if err != nil {
		return journal.FactFilter{}, fmt.Errorf("%w: normalize required contexts: %v", journal.ErrInvalidQuery, err)
	}
	actors := append([]journal.ActorID(nil), in.EffectiveActorIDs...)
	sort.Slice(actors, func(i, j int) bool { return actors[i].String() < actors[j].String() })
	actors = deduplicateFactActors(actors)
	operations := append([]journal.OperationID(nil), in.OperationIDs...)
	sort.Slice(operations, func(i, j int) bool { return operations[i] < operations[j] })
	operations = deduplicateFactOperations(operations)
	return journal.FactFilter{
		TaskScope:         in.TaskScope,
		RequiredContexts:  contexts,
		EffectiveActorIDs: actors,
		OperationIDs:      operations,
	}, nil
}

func deduplicateFactActors(values []journal.ActorID) []journal.ActorID {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func deduplicateFactOperations(values []journal.OperationID) []journal.OperationID {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func (db *DB) queryFacts(kind factSelectorKind, page journal.FactPageRequest, filter journal.FactFilter, kinds []string) (rows []factPageRow, snapshot journal.JournalID, next *journal.FactCursor, err error) {
	binding, err := buildFactPageMatchBinding(kind, page, filter, kinds)
	if err != nil {
		return nil, 0, nil, err
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("lease connection for read-only fact page: %w", err)
	}
	defer scope.release()

	// Keep the snapshot lookup, topology validation, page fetch, and verified
	// context loads on one read transaction. SQLite's WAL snapshot is therefore
	// fixed at the connection-lease boundary instead of being inferred from
	// separate pooled reads. The transaction is read-only and cannot change the
	// database or its WAL/SHM sidecars.
	endTx := sqlitex.Transaction(scope.conn)
	defer endTx(&err)

	if err = scope.resolveFactSnapshot(&page); err != nil {
		return nil, 0, nil, err
	}
	snapshot = page.SnapshotMaxJournalID
	if page.SnapshotMaxJournalID != binding.snapshotMax || page.AfterJournalID != binding.afterJournal {
		if binding, err = buildFactPageMatchBinding(kind, page, filter, kinds); err != nil {
			return nil, 0, nil, err
		}
	}
	if barrier := factQuerySnapshotBarrierHook; barrier != nil {
		barrier(kind, int64(snapshot))
	}
	if err = scope.requireCanonicalFactContextSchema("bounded fact query"); err != nil {
		return nil, 0, nil, err
	}

	rows = make([]factPageRow, 0, page.Limit+1)
	if err = sqlitex.Execute(scope.conn, binding.kind.pageMatchSQL(), &sqlitex.ExecOptions{
		Args: binding.args,
		ResultFunc: func(stmt *zs.Stmt) error {
			row, scanErr := scanFactPageRow(stmt, kind)
			if scanErr != nil {
				return scanErr
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, 0, nil, fmt.Errorf("bounded fact page SQL: %w", err)
	}
	for i := range rows {
		contexts, contextErr := scope.loadVerifiedFactContexts(binding.contexts, int64(rows[i].journalID))
		if contextErr != nil {
			return nil, 0, nil, contextErr
		}
		rows[i].contexts = contexts
	}

	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		last := rows[len(rows)-1]
		next = &journal.FactCursor{SnapshotMaxJournalID: snapshot, AfterJournalID: last.journalID}
	}
	return rows, snapshot, next, nil
}

// resolveFactSnapshot validates a caller-supplied watermark against a journal
// row in the same read transaction used for the page. A nonzero watermark is
// a positional boundary, not an unchecked integer supplied to page SQL.
func (scope *connScope) resolveFactSnapshot(page *journal.FactPageRequest) error {
	if page.SnapshotMaxJournalID == 0 {
		var maxID int64
		if err := sqlitex.Execute(scope.conn, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal", &sqlitex.ExecOptions{
			Args: []any{0},
			ResultFunc: func(stmt *zs.Stmt) error {
				maxID = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			return fmt.Errorf("resolve fact snapshot: read committed journal maximum: %w", err)
		}
		page.SnapshotMaxJournalID = journal.JournalID(maxID)
		return nil
	}
	var committed bool
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id FROM journal WHERE journal_id=?1", &sqlitex.ExecOptions{
		Args: []any{int64(page.SnapshotMaxJournalID)},
		ResultFunc: func(*zs.Stmt) error {
			committed = true
			return nil
		},
	}); err != nil {
		return fmt.Errorf("resolve fact snapshot %d: validate committed journal boundary: %w", page.SnapshotMaxJournalID, err)
	}
	if !committed {
		return fmt.Errorf("%w: snapshot watermark %d is not an existing committed journal boundary — where: fact query snapshot validation; when: before page SQL; impact: pagination could include facts appended after a forged boundary; fix: use the SnapshotMaxJournalID returned by an earlier page or leave it zero for a new positional query", journal.ErrInvalidQuery, page.SnapshotMaxJournalID)
	}
	return nil
}

func factQueryIntegrityError(journalID journal.JournalID, problem, fix string, cause ...error) error {
	where := "bounded fact query topology"
	if journalID > 0 {
		where = fmt.Sprintf("bounded fact query topology for journal %d", journalID)
	}
	message := fmt.Sprintf("%s — where: %s; when: read-only fact query; impact: the fact is rejected rather than omitted or returned partially; fix: %s", problem, where, fix)
	if len(cause) > 0 && cause[0] != nil {
		return fmt.Errorf("%w: %s: %w", journal.ErrSubtypeIntegrity, message, cause[0])
	}
	return fmt.Errorf("%w: %s", journal.ErrSubtypeIntegrity, message)
}

func scanFactPageRow(stmt *zs.Stmt, kind factSelectorKind) (factPageRow, error) {
	rowID := journal.JournalID(stmt.ColumnInt64(0))
	topologyStart := 8
	if kind == factSelectorEvidence {
		topologyStart = 9
	}
	if err := verifyFactPageTopologyRow(stmt, kind, rowID, topologyStart); err != nil {
		return factPageRow{}, err
	}
	row := factPageRow{
		journalID:  rowID,
		recordedAt: time.Unix(0, stmt.ColumnInt64(1)).UTC(),
		kind:       stmt.ColumnText(3),
	}
	if row.journalID <= 0 {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "journal id is not positive", "restore the fact and journal primary-key rows")
	}
	if stmt.ColumnType(2) != zs.TypeNull {
		taskID, err := journalParseTask(stmt.ColumnText(2))
		if err != nil {
			return factPageRow{}, factQueryIntegrityError(row.journalID, "task ID cannot be decoded", "restore the canonical task ID in the fact row", err)
		}
		row.taskID = &taskID
	}
	payloadColumn := 4
	if kind == factSelectorEvidence {
		row.digest = readBlob(stmt, 4)
		payloadColumn = 5
	}
	if stmt.ColumnType(payloadColumn) == zs.TypeNull {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "payload is NULL", "restore the fact payload from its committed canonical row")
	}
	row.payload = []byte(stmt.ColumnText(payloadColumn))
	if !json.Valid(row.payload) {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "payload is not valid JSON", "restore the fact payload from its committed canonical row")
	}
	actorColumn := payloadColumn + 1
	if stmt.ColumnType(actorColumn) == zs.TypeNull {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "effective actor attribution is missing", "restore journal_attributed and its producing anchor")
	}
	actor, err := journalParseActor(stmt.ColumnText(actorColumn))
	if err != nil {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "effective actor attribution cannot be decoded", "restore the canonical actor ID on the producing anchor", err)
	}
	row.effectiveActorID = actor
	operationColumn := actorColumn + 1
	if stmt.ColumnType(operationColumn) == zs.TypeNull {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "producing operation ID is missing", "restore the journal_operations row referenced by the fact")
	}
	row.producingOperationID = journal.OperationID(stmt.ColumnText(operationColumn))
	if err := journal.ValidateOperationID(row.producingOperationID); err != nil {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "producing operation ID is malformed", "restore the canonical operation identifier", err)
	}
	if stmt.ColumnType(operationColumn+1) == zs.TypeNull {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "producing operation journal ID is missing", "restore the journal operation anchor referenced by the fact")
	}
	row.producingOperationJournalID = journal.JournalID(stmt.ColumnInt64(operationColumn + 1))
	if row.producingOperationJournalID <= 0 {
		return factPageRow{}, factQueryIntegrityError(row.journalID, "producing operation journal ID is not positive", "restore the journal operation anchor referenced by the fact")
	}
	return row, nil
}

// verifyFactPageTopologyRow validates only the bounded candidates returned by
// the shared page matcher. The page SQL uses LEFT JOIN diagnostics so a broken
// producer or attribution relationship is visible instead of being omitted by
// an INNER JOIN. The candidate count is capped at Limit+1 by that same query.
func verifyFactPageTopologyRow(stmt *zs.Stmt, kind factSelectorKind, factID journal.JournalID, start int) error {
	expectedKind := journal.JournalKindDecision
	if kind == factSelectorEvidence {
		expectedKind = journal.JournalKindEvidence
	}
	if kind != factSelectorDecision && kind != factSelectorEvidence {
		return factQueryIntegrityError(factID, "unknown fact subtype", "use the closed decision or evidence selector")
	}
	if factID <= 0 {
		return factQueryIntegrityError(factID, "fact journal ID is not positive", "restore the fact and journal primary-key rows")
	}
	if stmt.ColumnType(start) == zs.TypeNull {
		return factQueryIntegrityError(factID, "fact has no journal supertype row", "restore the journal row for the fact")
	}
	if journal.JournalID(stmt.ColumnInt64(start)) != factID {
		return factQueryIntegrityError(factID, "fact journal relationship points at a different row", "restore the fact and journal primary-key rows")
	}
	if journal.JournalKind(stmt.ColumnInt(start+1)) != expectedKind {
		return factQueryIntegrityError(factID, "fact journal discriminator does not match its subtype table", "restore the matching journal kind and subtype row")
	}
	if stmt.ColumnType(start+2) == zs.TypeNull {
		return factQueryIntegrityError(factID, "fact has no producing operation relationship", "restore the fact's producing operation journal ID")
	}
	producerID := journal.JournalID(stmt.ColumnInt64(start + 2))
	if producerID <= 0 {
		return factQueryIntegrityError(factID, "fact has a non-positive producing operation journal ID", "restore the producing operation anchor")
	}
	if stmt.ColumnType(start+3) == zs.TypeNull {
		return factQueryIntegrityError(factID, "journal_attributed has no row for the fact", "restore the attribution view and the fact journal row")
	}
	if journal.JournalKind(stmt.ColumnInt(start+4)) != expectedKind {
		return factQueryIntegrityError(factID, "journal_attributed has the wrong subtype discriminator", "restore the fact journal kind and attribution relationship")
	}
	if stmt.ColumnType(start+5) == zs.TypeNull {
		return factQueryIntegrityError(factID, "effective actor attribution is NULL", "restore the actor on the producing operation anchor")
	}
	if _, err := journalParseActor(stmt.ColumnText(start + 5)); err != nil {
		return factQueryIntegrityError(factID, "effective actor attribution is malformed", "restore the canonical actor ID on the producing operation anchor", err)
	}
	if stmt.ColumnType(start+6) == zs.TypeNull || journal.JournalID(stmt.ColumnInt64(start+6)) != producerID {
		return factQueryIntegrityError(factID, "attribution points at a different producing operation", "restore journal_attributed to the fact journal relationship")
	}
	if stmt.ColumnType(start+7) == zs.TypeNull {
		return factQueryIntegrityError(factID, "producing operation row is missing", "restore journal_operations for the producing operation journal ID")
	}
	if stmt.ColumnType(start+8) == zs.TypeNull {
		return factQueryIntegrityError(factID, "producing operation ID is missing", "restore the operation identity on journal_operations")
	}
	if err := journal.ValidateOperationID(journal.OperationID(stmt.ColumnText(start + 8))); err != nil {
		return factQueryIntegrityError(factID, "producing operation ID is malformed", "restore the canonical operation identity", err)
	}
	if stmt.ColumnType(start+9) == zs.TypeNull || journal.JournalKind(stmt.ColumnInt(start+9)) != journal.JournalKindOperation {
		return factQueryIntegrityError(factID, "producing operation is not an operation journal anchor", "restore the operation journal discriminator and subtype row")
	}
	return nil
}
