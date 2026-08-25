package sqlite

import (
	"context"
	"database/sql"

	"github.com/dayvidpham/provenance/internal/fusedtx"
)

// allocationSQLTx adapts a pinned Modernc connection to the same deliberately
// narrow SQL contract used by DBOS fused callbacks. It has no transaction
// lifecycle methods: standalone callers own BEGIN IMMEDIATE and fused callers
// let DBOS own the outer transaction.
type allocationSQLTx struct {
	conn *sql.Conn
}

var _ fusedtx.SQLTx = allocationSQLTx{}

func (tx allocationSQLTx) Exec(ctx context.Context, query string, args ...any) (fusedtx.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx allocationSQLTx) Query(ctx context.Context, query string, args ...any) (fusedtx.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx allocationSQLTx) QueryRow(ctx context.Context, query string, args ...any) fusedtx.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}
