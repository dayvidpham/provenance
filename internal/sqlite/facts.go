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

// factPageKind is the closed dispatch boundary for the two fact subtype page
// queries. Keeping the SQL variants explicit prevents caller-controlled table
// or column identifiers from reaching SQLite.
type factPageKind uint8

const (
	factPageDecision factPageKind = iota + 1
	factPageEvidence
)

func (kind factPageKind) pageSQL() string {
	switch kind {
	case factPageDecision:
		return `SELECT d.journal_id, ja.recorded_at, d.task_id, d.decision_kind,
       d.payload, ja.effective_actor_id, jo.operation_id,
       ja.produced_by_operation_journal_id
FROM journal_decisions d
JOIN journal_attributed ja ON ja.journal_id = d.journal_id
JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
WHERE d.journal_id <= ?1
  AND d.journal_id > ?2
  AND d.decision_kind IN (SELECT value FROM json_each(?3))
  AND (NOT ?4 OR d.task_id IS ?5)
  AND (NOT ?6 OR ja.effective_actor_id IN (SELECT value FROM json_each(?7)))
  AND (NOT ?8 OR jo.operation_id IN (SELECT value FROM json_each(?9)))
  AND (NOT ?10 OR (SELECT COUNT(*) FROM json_each(?11) f
       WHERE EXISTS (SELECT ?10 FROM journal_task_event_contexts c
                     WHERE c.event_journal_id = d.journal_id
                       AND c.attached_by_journal_id <= ?1
                       AND c.context_kind = json_extract(f.value,?12)
                       AND c.context_identity = json_extract(f.value,?13))) = ?10)
ORDER BY d.journal_id ASC
LIMIT ?14`
	case factPageEvidence:
		return `SELECT e.journal_id, ja.recorded_at, e.task_id, e.evidence_kind,
       e.content_digest, e.payload, ja.effective_actor_id, jo.operation_id,
       ja.produced_by_operation_journal_id
FROM journal_evidence e
JOIN journal_attributed ja ON ja.journal_id = e.journal_id
JOIN journal_operations jo ON jo.journal_id = ja.produced_by_operation_journal_id
WHERE e.journal_id <= ?1
  AND e.journal_id > ?2
  AND e.evidence_kind IN (SELECT value FROM json_each(?3))
  AND (NOT ?4 OR e.task_id IS ?5)
  AND (NOT ?6 OR ja.effective_actor_id IN (SELECT value FROM json_each(?7)))
  AND (NOT ?8 OR jo.operation_id IN (SELECT value FROM json_each(?9)))
  AND (NOT ?10 OR (SELECT COUNT(*) FROM json_each(?11) f
       WHERE EXISTS (SELECT ?10 FROM journal_task_event_contexts c
                     WHERE c.event_journal_id = e.journal_id
                       AND c.attached_by_journal_id <= ?1
                       AND c.context_kind = json_extract(f.value,?12)
                       AND c.context_identity = json_extract(f.value,?13))) = ?10)
ORDER BY e.journal_id ASC
LIMIT ?14`
	default:
		panic("unknown fact page kind")
	}
}

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
	filter, err := normalizeFactQueryFilter(q.Filter)
	if err != nil {
		return journal.DecisionPage{}, fmt.Errorf("QueryDecisions: %w", err)
	}
	kinds := make([]string, len(q.Kinds))
	for i, kind := range q.Kinds {
		kinds[i] = string(kind)
	}
	rows, snapshot, next, err := db.queryFacts(factPageDecision, q.Page, filter, kinds)
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
	filter, err := normalizeFactQueryFilter(q.Filter)
	if err != nil {
		return journal.EvidencePage{}, fmt.Errorf("QueryEvidence: %w", err)
	}
	kinds := make([]string, len(q.Kinds))
	for i, kind := range q.Kinds {
		kinds[i] = string(kind)
	}
	rows, snapshot, next, err := db.queryFacts(factPageEvidence, q.Page, filter, kinds)
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

func (db *DB) queryFacts(kind factPageKind, page journal.FactPageRequest, filter journal.FactFilter, kinds []string) ([]factPageRow, journal.JournalID, *journal.FactCursor, error) {
	kindJSON, err := json.Marshal(kinds)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode fact kinds: %w", err)
	}
	actorValues := make([]string, len(filter.EffectiveActorIDs))
	for i, actor := range filter.EffectiveActorIDs {
		actorValues[i] = actor.String()
	}
	actorJSON, err := json.Marshal(actorValues)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode fact actor filters: %w", err)
	}
	operationValues := make([]string, len(filter.OperationIDs))
	for i, operation := range filter.OperationIDs {
		operationValues[i] = string(operation)
	}
	operationJSON, err := json.Marshal(operationValues)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode fact operation filters: %w", err)
	}
	contextJSON, err := json.Marshal(filter.RequiredContexts)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode fact context filters: %w", err)
	}

	filterTask := 0
	var taskValue any
	switch filter.TaskScope.Kind {
	case journal.FactTaskAny:
	case journal.FactTaskUnscoped:
		filterTask = 1
	case journal.FactTaskExact:
		filterTask = 1
		taskValue = filter.TaskScope.TaskID.String()
	default:
		return nil, 0, nil, fmt.Errorf("%w: unknown task scope kind %d", journal.ErrInvalidQuery, filter.TaskScope.Kind)
	}
	actorFilter := 0
	if len(filter.EffectiveActorIDs) > 0 {
		actorFilter = 1
	}
	operationFilter := 0
	if len(filter.OperationIDs) > 0 {
		operationFilter = 1
	}

	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("lease connection: %w", err)
	}
	defer scope.release()
	var txErr error
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&txErr)

	snapshot := int64(page.SnapshotMaxJournalID)
	if snapshot == 0 {
		if txErr = sqlitex.Execute(scope.conn, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal", &sqlitex.ExecOptions{
			Args: []any{0},
			ResultFunc: func(stmt *zs.Stmt) error {
				snapshot = stmt.ColumnInt64(0)
				return nil
			},
		}); txErr != nil {
			return nil, 0, nil, fmt.Errorf("snapshot watermark: %w", txErr)
		}
	}

	args := []any{
		snapshot,
		int64(page.AfterJournalID),
		string(kindJSON),
		filterTask,
		taskValue,
		actorFilter,
		string(actorJSON),
		operationFilter,
		string(operationJSON),
		len(filter.RequiredContexts),
		string(contextJSON),
		"$.kind",
		"$.identity",
		page.Limit + 1,
	}

	rows := make([]factPageRow, 0, page.Limit+1)
	if txErr = sqlitex.Execute(scope.conn, kind.pageSQL(), &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			row, scanErr := scanFactPageRow(stmt, kind)
			if scanErr != nil {
				return scanErr
			}
			rows = append(rows, row)
			return nil
		},
	}); txErr != nil {
		return nil, 0, nil, fmt.Errorf("page query: %w", txErr)
	}
	for i := range rows {
		contexts, contextErr := scope.loadFactContexts(int64(rows[i].journalID), snapshot)
		if contextErr != nil {
			txErr = contextErr
			return nil, 0, nil, contextErr
		}
		rows[i].contexts = contexts
	}

	var next *journal.FactCursor
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		last := rows[len(rows)-1]
		next = &journal.FactCursor{SnapshotMaxJournalID: journal.JournalID(snapshot), AfterJournalID: last.journalID}
	}
	return rows, journal.JournalID(snapshot), next, nil
}

func scanFactPageRow(stmt *zs.Stmt, kind factPageKind) (factPageRow, error) {
	row := factPageRow{
		journalID:  journal.JournalID(stmt.ColumnInt64(0)),
		recordedAt: time.Unix(0, stmt.ColumnInt64(1)).UTC(),
		kind:       stmt.ColumnText(3),
	}
	if row.journalID <= 0 {
		return factPageRow{}, fmt.Errorf("decode fact row: journal id %d is not positive", row.journalID)
	}
	if stmt.ColumnType(2) != zs.TypeNull {
		taskID, err := journalParseTask(stmt.ColumnText(2))
		if err != nil {
			return factPageRow{}, err
		}
		row.taskID = &taskID
	}
	payloadColumn := 4
	if kind == factPageEvidence {
		row.digest = readBlob(stmt, 4)
		payloadColumn = 5
	}
	row.payload = []byte(stmt.ColumnText(payloadColumn))
	if !json.Valid(row.payload) {
		return factPageRow{}, fmt.Errorf("decode fact row %d: payload is not valid JSON", row.journalID)
	}
	actorColumn := payloadColumn + 1
	actor, err := journalParseActor(stmt.ColumnText(actorColumn))
	if err != nil {
		return factPageRow{}, err
	}
	row.effectiveActorID = actor
	operationColumn := actorColumn + 1
	row.producingOperationID = journal.OperationID(stmt.ColumnText(operationColumn))
	if err := journal.ValidateOperationID(row.producingOperationID); err != nil {
		return factPageRow{}, fmt.Errorf("decode fact row %d: producing operation: %w", row.journalID, err)
	}
	row.producingOperationJournalID = journal.JournalID(stmt.ColumnInt64(operationColumn + 1))
	if row.producingOperationJournalID <= 0 {
		return factPageRow{}, fmt.Errorf("decode fact row %d: producing operation journal id %d is not positive", row.journalID, row.producingOperationJournalID)
	}
	return row, nil
}

func (scope *connScope) loadFactContexts(journalID, snapshot int64) ([]journal.EventContext, error) {
	var contexts []journal.EventContext
	if err := sqlitex.Execute(scope.conn, "SELECT context_kind, context_identity FROM journal_task_event_contexts WHERE event_journal_id = ?1 AND attached_by_journal_id <= ?2 ORDER BY context_kind, context_identity", &sqlitex.ExecOptions{
		Args: []any{journalID, snapshot},
		ResultFunc: func(stmt *zs.Stmt) error {
			context, err := journal.DecodeStoredEventContext(journal.EventContextKind(stmt.ColumnText(0)), stmt.ColumnText(1))
			if err != nil {
				return err
			}
			contexts = append(contexts, context)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("load fact contexts %d: %w", journalID, err)
	}
	return contexts, nil
}
