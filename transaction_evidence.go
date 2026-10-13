package provenance

import (
	"context"
	"database/sql"

	"github.com/dayvidpham/provenance/internal/sqlite"
)

// TransactionEvidenceReader provides QueryEvidence and LookupCommitted bound to
// the same caller-supplied SQL transaction, including its uncommitted changes.
// It never leases another connection or commits/rolls back the transaction.
// Its lifetime ends with that transaction; the caller serializes its use.
// Evidence page watermarks do not restrict LookupCommitted's transaction view.
type TransactionEvidenceReader = sqlite.TransactionEvidenceReader

// NewTransactionEvidenceReader binds evidence and committed-result reads to tx.
// The caller retains transaction ownership and supplies the read context.
func NewTransactionEvidenceReader(ctx context.Context, tx *sql.Tx) (*TransactionEvidenceReader, error) {
	return sqlite.NewTransactionEvidenceReader(ctx, tx)
}
