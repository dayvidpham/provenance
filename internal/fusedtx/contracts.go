// Package fusedtx provides the narrow application-SQL boundary used by DBOS
// fused transactions.
package fusedtx

import "context"

// Result reports the effect of a SQL write without exposing a driver-specific
// result type to application reducers.
type Result interface {
	RowsAffected() (int64, error)
}

// Row is one SQL result row. Scan reports driver and query errors.
type Row interface {
	Scan(dest ...any) error
}

// Rows is an iterated SQL result set.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// SQLReader is the read-only application SQL surface available within a fused
// transaction. It deliberately omits transaction lifecycle and DBOS controls.
type SQLReader interface {
	Query(context.Context, string, ...any) (Rows, error)
	QueryRow(context.Context, string, ...any) Row
}

// SQLTx is the application SQL surface available to a fused transaction
// callback. DBOS owns begin, commit, rollback, retries, and checkpointing.
type SQLTx interface {
	SQLReader
	Exec(context.Context, string, ...any) (Result, error)
}

// Callback is application work executed in DBOS's transaction. R is the
// callback result that DBOS checkpoints with the application writes.
type Callback[R any] func(context.Context, SQLTx) (R, error)
