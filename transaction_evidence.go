package provenance

import (
	"context"
	"database/sql"

	"github.com/dayvidpham/provenance/internal/sqlite"
)

// TransactionEvidenceReader has the production evidence-page contract, bound to
// the SQL transaction supplied at construction. Its lifetime ends with that tx.
type TransactionEvidenceReader = sqlite.TransactionEvidenceReader

func NewTransactionEvidenceReader(ctx context.Context, tx *sql.Tx) (*TransactionEvidenceReader, error) {
	return sqlite.NewTransactionEvidenceReader(ctx, tx)
}
