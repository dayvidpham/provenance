package provenance

import (
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func withRawSQLiteTestConn(t *testing.T, path string, fn func(*sqlite.Conn)) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenURI)
	if err != nil {
		t.Fatalf("open existing private SQLite test fixture %q: %v", path, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close raw SQLite test connection for %q: %v", path, err)
		}
	}()
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA busy_timeout=5000`, nil); err != nil {
		t.Fatalf("bound raw SQLite test connection wait for %q: %v", path, err)
	}
	fn(conn)
}
