package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
)

// TransactionEvidenceReader observes only the caller's transaction, including
// uncommitted journal truth. It never acquires a pool connection or owns the
// transaction lifecycle. Callers carry the first page's watermark to later pages.
type TransactionEvidenceReader struct {
	ctx context.Context
	tx  *sql.Tx
}

func NewTransactionEvidenceReader(ctx context.Context, tx *sql.Tx) (*TransactionEvidenceReader, error) {
	if ctx == nil || tx == nil {
		return nil, fmt.Errorf("NewTransactionEvidenceReader: missing context or SQL transaction; when: binding migration replay; no evidence read; fix: supply the caller's active transaction and context")
	}
	return &TransactionEvidenceReader{ctx: ctx, tx: tx}, nil
}

// LookupCommitted reconstructs the operation's emitted events and typed result
// slots using only the caller's transaction, with the same validation as the
// ordinary journal lookup. Committed means present in this transaction's view:
// it includes the caller's uncommitted changes and does not commit them. Unlike
// QueryEvidence's page watermark, this lookup observes the full transaction view.
// The caller must keep that view stable if resolving a multi-page snapshot.
func (reader *TransactionEvidenceReader) LookupCommitted(op journal.OperationID) (journal.CommittedResult, error) {
	if err := journal.ValidateOperationID(op); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("TransactionEvidenceReader.LookupCommitted: %w", err)
	}
	scope := &connScope{ctx: reader.ctx, conn: migrationConnection{reader.tx}, projectionTarget: projectionTargetLive}
	result, err := scope.lookupCommitted(op)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("TransactionEvidenceReader.LookupCommitted: operation %q in caller transaction: %w", op, err)
	}
	return result, nil
}

func (reader *TransactionEvidenceReader) QueryEvidence(q journal.EvidenceQuery) (journal.EvidencePage, error) {
	if err := q.Validate(); err != nil {
		return journal.EvidencePage{}, fmt.Errorf("QueryEvidenceInTx: %w", err)
	}
	scope := &connScope{ctx: reader.ctx, conn: migrationConnection{reader.tx}, projectionTarget: projectionTargetLive}
	if err := scope.requireCanonicalFactContextSchema("bounded transaction fact query"); err != nil {
		return journal.EvidencePage{}, err
	}
	if err := resolveFactSnapshotInTransaction(reader.ctx, reader.tx, &q.Page); err != nil {
		return journal.EvidencePage{}, err
	}
	kinds := make([]string, len(q.Kinds))
	for i, kind := range q.Kinds {
		kinds[i] = string(kind)
	}
	binding, err := buildFactPageMatchBinding(factSelectorEvidence, q.Page, q.Filter, kinds)
	if err != nil {
		return journal.EvidencePage{}, err
	}
	rows, err := readFactPageInTransaction(reader.ctx, reader.tx, binding)
	if err != nil {
		return journal.EvidencePage{}, err
	}
	var next *journal.FactCursor
	if len(rows) > q.Page.Limit {
		rows = rows[:q.Page.Limit]
		next = &journal.FactCursor{SnapshotMaxJournalID: q.Page.SnapshotMaxJournalID, AfterJournalID: rows[len(rows)-1].journalID}
	}
	return evidencePage(rows, q.Page.SnapshotMaxJournalID, next), nil
}

// Both ordinary and transaction-bound pages use this same row topology scanner
// and selected-fact owner verifier. A multi-kind page runs the static
// single-kind statement once per kind and merges the bounded heads here; every
// stream is ordered by journal_id and cut to the page bound, so the merge
// contains the global first page. Close page rows before fetching contexts so
// the supplied transaction needs only its one pinned connection.
func readFactPageInTransaction(ctx context.Context, reader sqlQueryer, binding factMatchBinding) ([]factPageRow, error) {
	statement := binding.kind.singleKindPageSQL()
	shared := binding.args[:firstPageKindParam-1]
	rows := make([]factPageRow, 0, binding.pageLimit*len(binding.pageKinds))
	for _, kind := range binding.pageKinds {
		args := append(append([]any{}, shared...), kind)
		kindRows, err := readSingleKindFactPage(ctx, reader, statement, args, binding.kind)
		if err != nil {
			return nil, err
		}
		rows = append(rows, kindRows...)
	}
	// Every per-kind stream is ordered by journal_id and bounded to pageLimit.
	// Merging the heads therefore contains the global first pageLimit rows; the
	// sort only orders the merge. Kinds are de-duplicated before the binding is
	// built, so the same fact cannot appear twice.
	if len(binding.pageKinds) > 1 {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].journalID < rows[j].journalID })
	}
	if len(rows) > binding.pageLimit {
		rows = rows[:binding.pageLimit]
	}
	for i := range rows {
		contexts, err := verifySelectedFactContextInTransaction(ctx, transactionFactReader{reader: reader}, binding.contexts, int64(rows[i].journalID))
		if err != nil {
			return nil, err
		}
		rows[i].contexts = contexts
	}
	return rows, nil
}

// readSingleKindFactPage executes the static single-kind page query and scans
// its bounded rows. Contexts are loaded once after the merged page is cut.
func readSingleKindFactPage(ctx context.Context, reader sqlQueryer, statement string, args []any, kind factSelectorKind) ([]factPageRow, error) {
	result, err := reader.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("bounded fact page SQL: %w", err)
	}
	var rows []factPageRow
	for result.Next() {
		row, scanErr := scanFactPageRow(result, kind)
		if scanErr != nil {
			_ = result.Close()
			return nil, scanErr
		}
		rows = append(rows, row)
	}
	iterationErr := result.Err()
	closeErr := result.Close()
	if iterationErr != nil {
		return nil, fmt.Errorf("bounded fact page SQL: iterate rows: %w", iterationErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("bounded fact page SQL: close rows: %w", closeErr)
	}
	return rows, nil
}

type transactionFactReader struct{ reader sqlQueryer }

func (reader transactionFactReader) Query(ctx context.Context, query string, args ...any) (fusedtx.Rows, error) {
	return reader.reader.QueryContext(ctx, query, args...)
}

func (reader transactionFactReader) QueryRow(ctx context.Context, query string, args ...any) fusedtx.Row {
	return reader.reader.QueryRowContext(ctx, query, args...)
}
