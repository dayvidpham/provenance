package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dayvidpham/provenance/internal/journal"
)

// factQueryTestHooks are unexported, no-op-by-default seams a package test
// installs on ONE DB instance before that instance is queried. snapshotBarrier
// fires inside the fact-query read transaction, after the snapshot is fixed and
// before the bounded page SQL runs, so a test can prove the ordering.
//
// These are per-DB, not package-global: the fact-query path is read by many
// parallel tests at once, and a package-global hook would be a shared variable
// written by one test while the others read it on their hot path.  A test
// installs its barrier with installFactQuerySnapshotBarrier on the DB it owns,
// before starting the goroutines that query it.
type factQueryTestHooks struct {
	snapshotBarrier func(factSelectorKind, int64)
}

// installFactQuerySnapshotBarrier installs a test barrier on this DB instance.
// It must be called before any concurrent query against the same instance.
func (db *DB) installFactQuerySnapshotBarrier(barrier func(factSelectorKind, int64)) {
	db.factHooks.snapshotBarrier = barrier
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
	kinds := make([]string, len(q.Kinds))
	for i, kind := range q.Kinds {
		kinds[i] = string(kind)
	}
	rows, snapshot, next, err := db.queryFacts(factSelectorDecision, q.Page, q.Filter, kinds)
	if err != nil {
		return journal.DecisionPage{}, fmt.Errorf("QueryDecisions: %w", err)
	}
	page := journal.DecisionPage{SnapshotMaxJournalID: snapshot, Next: next, Rows: make([]journal.DecisionRow, 0, len(rows))}
	for _, row := range rows {
		page.Rows = append(page.Rows, journal.DecisionRow{
			JournalID: row.journalID, RecordedAt: row.recordedAt, TaskID: row.taskID,
			DecisionKind: journal.DecisionKind(row.kind), Payload: append([]byte(nil), row.payload...),
			Contexts: append([]journal.EventContext(nil), row.contexts...), EffectiveActorID: row.effectiveActorID,
			ProducingOperationID: row.producingOperationID, ProducingOperationJournalID: row.producingOperationJournalID,
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
	page := journal.EvidencePage{SnapshotMaxJournalID: snapshot, Next: next, Rows: make([]journal.EvidenceRow, 0, len(rows))}
	for _, row := range rows {
		page.Rows = append(page.Rows, journal.EvidenceRow{
			JournalID: row.journalID, RecordedAt: row.recordedAt, TaskID: row.taskID,
			EvidenceKind: journal.EvidenceKind(row.kind), ContentDigest: append([]byte(nil), row.digest...),
			Payload: append([]byte(nil), row.payload...), Contexts: append([]journal.EventContext(nil), row.contexts...),
			EffectiveActorID: row.effectiveActorID, ProducingOperationID: row.producingOperationID,
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
	return journal.FactFilter{TaskScope: in.TaskScope, RequiredContexts: contexts, EffectiveActorIDs: actors, OperationIDs: operations}, nil
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

func (db *DB) queryFacts(kind factSelectorKind, page journal.FactPageRequest, filter journal.FactFilter, kinds []string) ([]factPageRow, journal.JournalID, *journal.FactCursor, error) {
	binding, err := buildFactPageMatchBinding(kind, page, filter, kinds)
	if err != nil {
		return nil, 0, nil, err
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("lease connection for read-only fact page: %w", err)
	}
	defer scope.release()

	// The snapshot lookup, topology validation, page fetch, and verified context
	// loads share one explicit pinned read transaction. In WAL mode that fixes the
	// snapshot before the bounded page query without holding write ownership.
	rows := make([]factPageRow, 0, page.Limit+1)
	var snapshot journal.JournalID
	if err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		if err := scope.resolveFactSnapshot(&page); err != nil {
			return err
		}
		snapshot = page.SnapshotMaxJournalID
		if page.SnapshotMaxJournalID != binding.snapshotMax || page.AfterJournalID != binding.afterJournal {
			var bindErr error
			binding, bindErr = buildFactPageMatchBinding(kind, page, filter, kinds)
			if bindErr != nil {
				return bindErr
			}
		}
		if barrier := db.factHooks.snapshotBarrier; barrier != nil {
			barrier(kind, int64(snapshot))
		}
		if err := scope.requireCanonicalFactContextSchema("bounded fact query"); err != nil {
			return err
		}

		result, err := scope.conn.QueryContext(scope.ctx, binding.kind.pageMatchSQL(), binding.args...)
		if err != nil {
			return fmt.Errorf("bounded fact page SQL: %w", err)
		}
		defer result.Close()
		for result.Next() {
			row, err := scanFactPageRow(result, kind)
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		if err := result.Err(); err != nil {
			return fmt.Errorf("bounded fact page SQL: iterate rows: %w", err)
		}
		for i := range rows {
			contexts, err := scope.loadVerifiedFactContexts(binding.contexts, int64(rows[i].journalID))
			if err != nil {
				return err
			}
			rows[i].contexts = contexts
		}
		return nil
	}); err != nil {
		return nil, 0, nil, err
	}

	var next *journal.FactCursor
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
		if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal", 0).Scan(&maxID); err != nil {
			return fmt.Errorf("resolve fact snapshot: read committed journal maximum: %w", err)
		}
		page.SnapshotMaxJournalID = journal.JournalID(maxID)
		return nil
	}
	var boundary int64
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id FROM journal WHERE journal_id=?1", int64(page.SnapshotMaxJournalID)).Scan(&boundary)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: snapshot watermark %d is not an existing committed journal boundary — where: fact query snapshot validation; when: before page SQL; impact: pagination could include facts appended after a forged boundary; fix: use the SnapshotMaxJournalID returned by an earlier page or leave it zero for a new positional query", journal.ErrInvalidQuery, page.SnapshotMaxJournalID)
	}
	if err != nil {
		return fmt.Errorf("resolve fact snapshot %d: validate committed journal boundary: %w", page.SnapshotMaxJournalID, err)
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

type factPageTopology struct {
	factJournalID        sql.NullInt64
	factJournalKind      sql.NullInt64
	factProducer         sql.NullInt64
	attributedJournalID  sql.NullInt64
	attributedKind       sql.NullInt64
	attributedActor      sql.NullString
	attributedProducer   sql.NullInt64
	operationJournalID   sql.NullInt64
	operationID          sql.NullString
	operationJournalKind sql.NullInt64
}

type factPageScan struct {
	journalID        sql.NullInt64
	recordedAt       sql.NullInt64
	taskID           sql.NullString
	kind             sql.NullString
	digest           []byte
	payload          []byte
	effectiveActorID sql.NullString
	operationID      sql.NullString
	producerID       sql.NullInt64
	topology         factPageTopology
}

func scanFactPageRow(row sqlRowScanner, kind factSelectorKind) (factPageRow, error) {
	var scanned factPageScan
	var scanErr error
	if kind == factSelectorDecision {
		scanErr = row.Scan(
			&scanned.journalID, &scanned.recordedAt, &scanned.taskID, &scanned.kind, &scanned.payload,
			&scanned.effectiveActorID, &scanned.operationID, &scanned.producerID,
			&scanned.topology.factJournalID, &scanned.topology.factJournalKind, &scanned.topology.factProducer,
			&scanned.topology.attributedJournalID, &scanned.topology.attributedKind, &scanned.topology.attributedActor,
			&scanned.topology.attributedProducer, &scanned.topology.operationJournalID,
			&scanned.topology.operationID, &scanned.topology.operationJournalKind,
		)
	} else if kind == factSelectorEvidence {
		scanErr = row.Scan(
			&scanned.journalID, &scanned.recordedAt, &scanned.taskID, &scanned.kind, &scanned.digest, &scanned.payload,
			&scanned.effectiveActorID, &scanned.operationID, &scanned.producerID,
			&scanned.topology.factJournalID, &scanned.topology.factJournalKind, &scanned.topology.factProducer,
			&scanned.topology.attributedJournalID, &scanned.topology.attributedKind, &scanned.topology.attributedActor,
			&scanned.topology.attributedProducer, &scanned.topology.operationJournalID,
			&scanned.topology.operationID, &scanned.topology.operationJournalKind,
		)
	} else {
		return factPageRow{}, factQueryIntegrityError(0, "unknown fact subtype", "use the closed decision or evidence selector")
	}
	if scanErr != nil {
		return factPageRow{}, fmt.Errorf("scan bounded fact page row: %w", scanErr)
	}
	if !scanned.journalID.Valid {
		return factPageRow{}, factQueryIntegrityError(0, "fact journal ID is NULL", "restore the fact primary-key row")
	}
	factID := journal.JournalID(scanned.journalID.Int64)
	if err := verifyFactPageTopologyRow(scanned.topology, kind, factID); err != nil {
		return factPageRow{}, err
	}
	if factID <= 0 {
		return factPageRow{}, factQueryIntegrityError(factID, "journal id is not positive", "restore the fact and journal primary-key rows")
	}
	if !scanned.recordedAt.Valid {
		return factPageRow{}, factQueryIntegrityError(factID, "recorded timestamp is missing", "restore the fact journal row")
	}
	if !scanned.kind.Valid {
		return factPageRow{}, factQueryIntegrityError(factID, "fact kind is NULL", "restore the fact subtype row")
	}
	if scanned.payload == nil {
		return factPageRow{}, factQueryIntegrityError(factID, "payload is NULL", "restore the fact payload from its committed canonical row")
	}
	if !json.Valid(scanned.payload) {
		return factPageRow{}, factQueryIntegrityError(factID, "payload is not valid JSON", "restore the fact payload from its committed canonical row")
	}
	if !scanned.effectiveActorID.Valid {
		return factPageRow{}, factQueryIntegrityError(factID, "effective actor attribution is missing", "restore journal_attributed and its producing anchor")
	}
	actor, err := journalParseActor(scanned.effectiveActorID.String)
	if err != nil {
		return factPageRow{}, factQueryIntegrityError(factID, "effective actor attribution cannot be decoded", "restore the canonical actor ID on the producing anchor", err)
	}
	if !scanned.operationID.Valid {
		return factPageRow{}, factQueryIntegrityError(factID, "producing operation ID is missing", "restore the journal_operations row referenced by the fact")
	}
	operationID := journal.OperationID(scanned.operationID.String)
	if err := journal.ValidateOperationID(operationID); err != nil {
		return factPageRow{}, factQueryIntegrityError(factID, "producing operation ID is malformed", "restore the canonical operation identifier", err)
	}
	if !scanned.producerID.Valid || scanned.producerID.Int64 <= 0 {
		return factPageRow{}, factQueryIntegrityError(factID, "producing operation journal ID is missing or non-positive", "restore the operation journal anchor referenced by the fact")
	}
	result := factPageRow{
		journalID: factID, recordedAt: time.Unix(0, scanned.recordedAt.Int64).UTC(), kind: scanned.kind.String,
		payload: append([]byte(nil), scanned.payload...), digest: append([]byte(nil), scanned.digest...),
		effectiveActorID: actor, producingOperationID: operationID,
		producingOperationJournalID: journal.JournalID(scanned.producerID.Int64),
	}
	if scanned.taskID.Valid {
		task, err := journalParseTask(scanned.taskID.String)
		if err != nil {
			return factPageRow{}, factQueryIntegrityError(factID, "task ID cannot be decoded", "restore the canonical task ID in the fact row", err)
		}
		result.taskID = &task
	}
	return result, nil
}

// verifyFactPageTopologyRow validates only the bounded candidates returned by
// the shared page matcher. The page SQL uses LEFT JOIN diagnostics so a broken
// producer or attribution relationship is visible instead of being omitted by
// an INNER JOIN. The candidate count is capped at Limit+1 by that same query.
func verifyFactPageTopologyRow(topology factPageTopology, kind factSelectorKind, factID journal.JournalID) error {
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
	if !topology.factJournalID.Valid {
		return factQueryIntegrityError(factID, "fact has no journal supertype row", "restore the journal row for the fact")
	}
	if journal.JournalID(topology.factJournalID.Int64) != factID {
		return factQueryIntegrityError(factID, "fact journal relationship points at a different row", "restore the fact and journal primary-key rows")
	}
	if !topology.factJournalKind.Valid || journal.JournalKind(topology.factJournalKind.Int64) != expectedKind {
		return factQueryIntegrityError(factID, "fact journal discriminator does not match its subtype table", "restore the matching journal kind and subtype row")
	}
	if !topology.factProducer.Valid || topology.factProducer.Int64 <= 0 {
		return factQueryIntegrityError(factID, "fact has no producing operation relationship", "restore the fact's producing operation journal ID")
	}
	producerID := journal.JournalID(topology.factProducer.Int64)
	if !topology.attributedJournalID.Valid {
		return factQueryIntegrityError(factID, "journal_attributed has no row for the fact", "restore the attribution view and the fact journal row")
	}
	if !topology.attributedKind.Valid || journal.JournalKind(topology.attributedKind.Int64) != expectedKind {
		return factQueryIntegrityError(factID, "journal_attributed has the wrong subtype discriminator", "restore the fact journal kind and attribution relationship")
	}
	if !topology.attributedActor.Valid {
		return factQueryIntegrityError(factID, "effective actor attribution is NULL", "restore the actor on the producing operation anchor")
	}
	if _, err := journalParseActor(topology.attributedActor.String); err != nil {
		return factQueryIntegrityError(factID, "effective actor attribution is malformed", "restore the canonical actor ID on the producing operation anchor", err)
	}
	if !topology.attributedProducer.Valid || journal.JournalID(topology.attributedProducer.Int64) != producerID {
		return factQueryIntegrityError(factID, "attribution points at a different producing operation", "restore journal_attributed to the fact journal relationship")
	}
	if !topology.operationJournalID.Valid {
		return factQueryIntegrityError(factID, "producing operation row is missing", "restore journal_operations for the producing operation journal ID")
	}
	if !topology.operationID.Valid {
		return factQueryIntegrityError(factID, "producing operation ID is missing", "restore the operation identity on journal_operations")
	}
	if err := journal.ValidateOperationID(journal.OperationID(topology.operationID.String)); err != nil {
		return factQueryIntegrityError(factID, "producing operation ID is malformed", "restore the canonical operation identity", err)
	}
	if !topology.operationJournalKind.Valid || journal.JournalKind(topology.operationJournalKind.Int64) != journal.JournalKindOperation {
		return factQueryIntegrityError(factID, "producing operation is not an operation journal anchor", "restore the operation journal discriminator and subtype row")
	}
	return nil
}
