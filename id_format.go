package provenance

import (
	"context"
	"database/sql"

	"github.com/dayvidpham/provenance/internal/sqlite"
)

type IDFormat = sqlite.IDFormat
type IDFormatInspection = sqlite.IDFormatInspection

const (
	IDFormatEmpty   = sqlite.IDFormatEmpty
	IDFormatRetired = sqlite.IDFormatRetired
	IDFormatSlash   = sqlite.IDFormatSlash
)

var ErrIDFormat = sqlite.ErrIDFormat

// IDFormatReader may be a database or caller-owned SQL transaction. Inspection
// never activates schema, begins a transaction, or writes a marker.
type IDFormatReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func InspectIDFormat(ctx context.Context, reader IDFormatReader) (IDFormatInspection, error) {
	return sqlite.InspectIDFormat(ctx, reader)
}
