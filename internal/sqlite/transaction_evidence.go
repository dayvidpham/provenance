package sqlite

import (
	"context"
	"database/sql"
	"fmt"

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
// and selected-fact owner verifier. Close page rows before fetching contexts so
// the supplied transaction needs only its one pinned connection.
func readFactPageInTransaction(ctx context.Context, reader sqlQueryer, binding factMatchBinding) ([]factPageRow, error) {
	result, err := reader.QueryContext(ctx, binding.kind.pageMatchSQL(), binding.args...)
	if err != nil {
		return nil, fmt.Errorf("bounded fact page SQL: %w", err)
	}
	var rows []factPageRow
	for result.Next() {
		row, scanErr := scanFactPageRow(result, binding.kind)
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
	for i := range rows {
		contexts, err := verifySelectedFactContextInTransaction(ctx, transactionFactReader{reader: reader}, binding.contexts, int64(rows[i].journalID))
		if err != nil {
			return nil, err
		}
		rows[i].contexts = contexts
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
