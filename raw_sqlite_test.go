package provenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"
)

// rawSQLiteConn is a deliberately small database/sql test fixture adapter. It
// keeps corruption and invariant tests on the same Modernc driver as production
// without introducing a second SQLite implementation into the test graph.
type rawSQLiteConn struct {
	db   *sql.DB
	conn *sql.Conn
}

type rawExecOptions struct {
	Args       []any
	ResultFunc func(*rawSQLiteStmt) error
}

type rawSQLiteStmt struct {
	values []any
}

func (stmt *rawSQLiteStmt) value(column int) any {
	if column < 0 || column >= len(stmt.values) {
		panic(fmt.Sprintf("raw SQLite test column %d outside row width %d", column, len(stmt.values)))
	}
	return stmt.values[column]
}

func (stmt *rawSQLiteStmt) ColumnInt(column int) int { return int(stmt.ColumnInt64(column)) }

func (stmt *rawSQLiteStmt) ColumnInt64(column int) int64 {
	switch value := stmt.value(column).(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case int32:
		return int64(value)
	case []byte:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			panic(fmt.Sprintf("raw SQLite test integer column %d %q: %v", column, value, err))
		}
		return parsed
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			panic(fmt.Sprintf("raw SQLite test integer column %d %q: %v", column, value, err))
		}
		return parsed
	default:
		panic(fmt.Sprintf("raw SQLite test integer column %d has unsupported type %T", column, value))
	}
}

func (stmt *rawSQLiteStmt) ColumnText(column int) string {
	switch value := stmt.value(column).(type) {
	case nil:
		return ""
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

func (stmt *rawSQLiteStmt) ColumnLen(column int) int {
	switch value := stmt.value(column).(type) {
	case nil:
		return 0
	case string:
		return len(value)
	case []byte:
		return len(value)
	default:
		return len(fmt.Sprint(value))
	}
}

func (stmt *rawSQLiteStmt) ColumnBytes(column int, dst []byte) {
	copy(dst, []byte(stmt.ColumnText(column)))
}

func rawSQLiteTestDSN(path, mode string) string {
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": []string{mode}}
	uri.RawQuery = query.Encode()
	return uri.String()
}

func openRawSQLiteTestConn(t *testing.T, path, mode string) *rawSQLiteConn {
	t.Helper()
	db, err := sql.Open("sqlite", rawSQLiteTestDSN(path, mode))
	if err != nil {
		t.Fatalf("open existing private SQLite test fixture %q: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatalf("lease raw SQLite test connection for %q: %v", path, err)
	}
	raw := &rawSQLiteConn{db: db, conn: conn}
	if _, err := raw.conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000"); err != nil {
		_ = raw.Close()
		t.Fatalf("bound raw SQLite test connection wait for %q: %v", path, err)
	}
	return raw
}

func (conn *rawSQLiteConn) Close() error {
	return errors.Join(conn.conn.Close(), conn.db.Close())
}

func withRawSQLiteTestConn(t *testing.T, path string, fn func(*rawSQLiteConn)) {
	t.Helper()
	conn := openRawSQLiteTestConn(t, path, "rw")
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close raw SQLite test connection for %q: %v", path, err)
		}
	}()
	fn(conn)
}

func rawExecute(conn *rawSQLiteConn, query string, options *rawExecOptions) (err error) {
	if options == nil || options.ResultFunc == nil {
		_, err := conn.conn.ExecContext(context.Background(), query, optionsArgs(options)...)
		return err
	}
	rows, err := conn.conn.QueryContext(context.Background(), query, options.Args...)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := rows.Close()
		if closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		if err := options.ResultFunc(&rawSQLiteStmt{values: values}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func optionsArgs(options *rawExecOptions) []any {
	if options == nil {
		return nil
	}
	return options.Args
}

func rawExecuteTransient(conn *rawSQLiteConn, query string, options *rawExecOptions) error {
	return rawExecute(conn, query, options)
}
