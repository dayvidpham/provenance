package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IDFormat is a store-wide identity representation, not a compatibility mode.
type IDFormat string

const (
	IDFormatEmpty   IDFormat = "empty"
	IDFormatRetired IDFormat = "retired-two-dash"
	IDFormatSlash   IDFormat = "namespace-slash-uuid.v1"
)

var ErrIDFormat = errors.New("provenance: store identity format requires offline conversion")

type IDFormatInspection struct {
	Format    IDFormat
	Populated bool
}

type IDFormatReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// InspectIDFormat performs only reads and works on the caller's transaction.
// Unmarked populated stores are retired even if some keys already contain slashes:
// guessing from a sample would conceal an interrupted or partial conversion.
func InspectIDFormat(ctx context.Context, reader IDFormatReader) (IDFormatInspection, error) {
	if ctx == nil || reader == nil {
		return IDFormatInspection{}, fmt.Errorf("InspectIDFormat: nil reader; no store inspected; fix: supply the open database or caller transaction")
	}
	switch typed := reader.(type) {
	case *sql.DB:
		if typed == nil {
			return IDFormatInspection{}, fmt.Errorf("InspectIDFormat: nil database; no store inspected; fix: supply an open database")
		}
	case *sql.Tx:
		if typed == nil {
			return IDFormatInspection{}, fmt.Errorf("InspectIDFormat: nil transaction; no store inspected; fix: supply an active caller transaction")
		}
	case *sql.Conn:
		if typed == nil {
			return IDFormatInspection{}, fmt.Errorf("InspectIDFormat: nil connection; no store inspected; fix: supply a leased connection")
		}
	}
	result := IDFormatInspection{Format: IDFormatEmpty}
	for _, table := range []string{"tasks", "agents", "activities", "comments", "journal", "actor_namespace_claims", "fixed_actor_manifest_entries", "governed_allocation_operations"} {
		var exists bool
		if err := reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			return result, fmt.Errorf("InspectIDFormat: inspect table %s before startup; store unchanged; fix: restore a readable SQLite store: %w", table, err)
		}
		if exists {
			var populated bool
			if err := reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM \""+table+"\")").Scan(&populated); err != nil {
				return result, fmt.Errorf("InspectIDFormat: inspect rows in %s before startup; store unchanged; fix: restore a readable SQLite store: %w", table, err)
			}
			result.Populated = result.Populated || populated
		}
	}
	var markerExists bool
	if err := reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='provenance_id_format')").Scan(&markerExists); err != nil {
		return result, fmt.Errorf("InspectIDFormat: inspect marker schema: %w", err)
	}
	if markerExists {
		var rows, invalid int
		if err := reader.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(CASE WHEN singleton<>1 OR singleton IS NULL OR format IS NULL OR format='' THEN 1 ELSE 0 END),0) FROM provenance_id_format").Scan(&rows, &invalid); err != nil {
			return result, fmt.Errorf("%w: inspect marker shape before activation; no identities decoded; fix: restore the complete archive: %v", ErrIDFormat, err)
		}
		if rows > 1 || invalid > 0 {
			return result, fmt.Errorf("%w: invalid singleton marker rows (%d total, %d malformed); when: before activation; store unchanged; fix: restore complete format metadata from archive", ErrIDFormat, rows, invalid)
		}
		var marker string
		if err := reader.QueryRowContext(ctx, "SELECT format FROM provenance_id_format WHERE singleton=1").Scan(&marker); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("InspectIDFormat: read marker before startup; no identity decoded; fix: restore the complete archived store: %w", err)
		}
		if marker != "" {
			if IDFormat(marker) != IDFormatSlash {
				return result, fmt.Errorf("%w: unknown marker %q in provenance_id_format; when: before startup decoding; impact: store unchanged; fix: use its matching supported binary or restore the archive", ErrIDFormat, marker)
			}
			result.Format = IDFormatSlash
			return result, nil
		}
	}
	if result.Populated {
		result.Format = IDFormatRetired
	}
	return result, nil
}

func requireCurrentIDFormat(ctx context.Context, reader IDFormatReader, path string) error {
	inspection, err := InspectIDFormat(ctx, reader)
	if err != nil {
		return err
	}
	if inspection.Format == IDFormatRetired {
		if path == "" {
			if err := reader.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
				return fmt.Errorf("%w: identify populated unmarked store before activation: %v; no store data written; fix: stop writers and inspect the caller's database path before offline conversion", ErrIDFormat, err)
			}
		}
		return fmt.Errorf("%w: populated unmarked store %q; where: identity-format startup guard; when: before schema activation, actor registration or journal decode; impact: this open stopped without writing store data; fix: stop all writers, retain a consistent SQLite backup, run `pasture migrate ids --dry-run --db %q`, then `pasture migrate ids --db %q`; restart only upgraded binaries", ErrIDFormat, path, path, path)
	}
	return nil
}

const idFormatSchema = `CREATE TABLE IF NOT EXISTS provenance_id_format (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 format TEXT NOT NULL CHECK(format='namespace-slash-uuid.v1')
)`

func initializeEmptyIDFormat(ctx context.Context, conn sqlQueryer) error {
	inspection, err := InspectIDFormat(ctx, conn)
	if err != nil {
		return err
	}
	if inspection.Format == IDFormatSlash {
		return nil
	}
	if inspection.Populated {
		return requireCurrentIDFormat(ctx, conn, "the activating SQLite database")
	}
	if _, err := conn.ExecContext(ctx, idFormatSchema); err != nil {
		return fmt.Errorf("initializeEmptyIDFormat: create marker inside activation transaction: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO provenance_id_format(singleton,format) VALUES(1,?)", string(IDFormatSlash)); err != nil {
		return fmt.Errorf("initializeEmptyIDFormat: mark empty store inside activation transaction: %w", err)
	}
	return nil
}
