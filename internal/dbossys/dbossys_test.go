package dbossys_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "modernc.org/sqlite"

	"github.com/dayvidpham/provenance/internal/dbossys"
)

// This package deliberately does NOT blank-import
// github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite. Its test binary
// therefore links no DBOS SQLite driver, which is the only way to observe the
// real missing-driver failure that a production binary must never produce.

// openFixtureCopy copies an immutable testdata database into the case's own
// temporary directory and opens that private copy read-write.
func openFixtureCopy(t *testing.T, name string) *sql.DB {
	t.Helper()
	source := filepath.Join("..", "..", "testdata", "dbos", name)
	target := filepath.Join(t.TempDir(), name)
	copyFile(t, source, target)
	db, err := sql.Open("sqlite", "file:"+target+"?_pragma=busy_timeout(5000)&mode=rw")
	if err != nil {
		t.Fatalf("open fixture copy %s: %v", target, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openEmptyDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping fresh database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestInspectSchemaReportsFreshDatabase(t *testing.T) {
	t.Parallel()
	state, version, err := dbossys.InspectSchema(context.Background(), openEmptyDatabase(t))
	if err != nil {
		t.Fatalf("InspectSchema on a fresh database: %v", err)
	}
	if state != dbossys.SchemaStateFresh {
		t.Errorf("state=%s want %s", state, dbossys.SchemaStateFresh)
	}
	if version != 0 {
		t.Errorf("version=%d want 0 on a database with no migration table", version)
	}
}

// The fixture is a real database built by dbos-transact-golang v0.20.0: it is
// the exact durable shape the clean-cut policy refuses.
func TestInspectSchemaReportsSupersededVersionForV020Database(t *testing.T) {
	t.Parallel()
	state, version, err := dbossys.InspectSchema(context.Background(), openFixtureCopy(t, "dbos_system_v020.db"))
	if err != nil {
		t.Fatalf("InspectSchema on the v0.20 fixture: %v", err)
	}
	if state != dbossys.SchemaStateSuperseded {
		t.Errorf("state=%s want %s", state, dbossys.SchemaStateSuperseded)
	}
	if version != 41 {
		t.Errorf("version=%d want 41, the last migration the superseded runtime applied", version)
	}
	if version >= dbossys.FirstSupportedMigrationVersion {
		t.Errorf("fixture version %d is not below the supported floor %d: the fixture no longer proves anything",
			version, dbossys.FirstSupportedMigrationVersion)
	}
}

func TestRequireSupportedSchemaAcceptsFreshDatabase(t *testing.T) {
	t.Parallel()
	if err := dbossys.RequireSupportedSchema(context.Background(), openEmptyDatabase(t), "fresh.db"); err != nil {
		t.Fatalf("RequireSupportedSchema rejected a fresh database: %v", err)
	}
}

func TestRequireSupportedSchemaRefusesV020DatabaseWithActionableError(t *testing.T) {
	t.Parallel()
	err := dbossys.RequireSupportedSchema(context.Background(), openFixtureCopy(t, "dbos_system_v020.db"), "fixture.db")
	if err == nil {
		t.Fatal("RequireSupportedSchema accepted a superseded system schema; the clean-cut policy is not enforced")
	}
	if !errors.Is(err, dbossys.ErrSupersededSystemSchema) {
		t.Errorf("error does not match ErrSupersededSystemSchema: %v", err)
	}
	message := err.Error()
	// The error must answer what, why, where, impact, and fix.
	for _, want := range []string{
		"dbossys.RequireSupportedSchema",
		"fixture.db",
		"41",
		"42",
		"no in-place upgrade",
		"nothing was opened",
		"delete the database file",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal message is missing %q; got: %s", want, message)
		}
	}
}

// The DBOS runtime refuses to construct a context over a custom SQLite handle
// unless the driver package is blank-imported, because its busy/locked
// classification needs that driver's error-code extractor. Provenance must
// report that failure as a permanent build defect, never as a transient error
// worth retrying.
func TestMissingSQLiteDriverIsClassifiedNonRetryable(t *testing.T) {
	t.Parallel()
	_, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName:        "provenance-missing-driver-probe",
		SQLiteSystemDB: openEmptyDatabase(t),
	})
	if err == nil {
		t.Fatal("dbos.NewContext succeeded without a registered SQLite driver: this test package must not link one")
	}
	failure := dbossys.ClassifyOpenFailure(err)
	if failure != dbossys.OpenFailureMissingSQLiteDriver {
		t.Fatalf("ClassifyOpenFailure=%s want %s for: %v", failure, dbossys.OpenFailureMissingSQLiteDriver, err)
	}
	if failure.Retryable() {
		t.Error("the missing-driver failure is reported as retryable; retrying can never register a driver")
	}
	described := dbossys.DescribeOpenFailure(failure, "probe.db", err)
	for _, want := range []string{
		"probe.db",
		"github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite",
		"retrying cannot fix it",
	} {
		if !strings.Contains(described.Error(), want) {
			t.Errorf("described failure is missing %q; got: %s", want, described.Error())
		}
	}
}

func TestClassifyOpenFailureLeavesUnknownErrorsRetryable(t *testing.T) {
	t.Parallel()
	failure := dbossys.ClassifyOpenFailure(errors.New("database is locked"))
	if failure != dbossys.OpenFailureUnknown {
		t.Fatalf("ClassifyOpenFailure=%s want %s", failure, dbossys.OpenFailureUnknown)
	}
	if !failure.Retryable() {
		t.Error("an unclassified open failure must stay on the caller's retry channel")
	}
}

func TestClassifyOpenFailureReportsNoFailureForNil(t *testing.T) {
	t.Parallel()
	if got := dbossys.ClassifyOpenFailure(nil); got != dbossys.OpenFailureNone {
		t.Fatalf("ClassifyOpenFailure(nil)=%s want %s", got, dbossys.OpenFailureNone)
	}
}

// copyFile writes an immutable fixture's bytes to a private mutable path.
func copyFile(t *testing.T, source, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read immutable fixture %s: %v", source, err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write private fixture copy %s: %v", target, err)
	}
}
