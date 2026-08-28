package dbossys_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "modernc.org/sqlite"

	"github.com/dayvidpham/provenance/internal/dbosfixture"
	"github.com/dayvidpham/provenance/internal/dbossys"
)

// This package deliberately does NOT blank-import
// github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite. Its test binary
// therefore links no DBOS SQLite driver, which is the only way to observe the
// real missing-driver failure that a production binary must never produce.

// testdataDBOSDir is this package's relative path to the immutable DBOS
// fixtures.
var testdataDBOSDir = filepath.Join("..", "..", "testdata", "dbos")

// openSupersededFixtureCopy verifies the fixture's pinned digest, copies it into
// the case's own temporary directory, and opens that private copy read-write.
func openSupersededFixtureCopy(t *testing.T) *sql.DB {
	t.Helper()
	target, _ := dbosfixture.PrivateDBOSSystemV020Copy(t, testdataDBOSDir)
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
	state, version, err := dbossys.InspectSchema(context.Background(), openSupersededFixtureCopy(t))
	if err != nil {
		t.Fatalf("InspectSchema on the v0.20 fixture: %v", err)
	}
	if state != dbossys.SchemaStateSuperseded {
		t.Errorf("state=%s want %s", state, dbossys.SchemaStateSuperseded)
	}
	if version != dbosfixture.SupersededSystemSchemaVersion {
		t.Errorf("version=%d want %d, the last migration the superseded runtime applied",
			version, dbosfixture.SupersededSystemSchemaVersion)
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
	err := dbossys.RequireSupportedSchema(context.Background(), openSupersededFixtureCopy(t), "fixture.db")
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

// The refusal has to be actionable in a shell. A production caller passes its
// DSN as origin, and a durable Modernc DSN carries a "file:" scheme and a pragma
// query string, so the DSN is not a file name anybody can delete. The fix clause
// must therefore name the plain path the handle really uses, plus its two SQLite
// siblings, and must carry no query string at all.
func TestRequireSupportedSchemaNamesTheRealFileNotTheDSN(t *testing.T) {
	t.Parallel()
	target, _ := dbosfixture.PrivateDBOSSystemV020Copy(t, testdataDBOSDir)
	dsn := "file:" + target + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&mode=rw"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open the fixture copy through a production-shaped DSN: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	err = dbossys.RequireSupportedSchema(context.Background(), db, dsn)
	if err == nil {
		t.Fatal("RequireSupportedSchema accepted a superseded system schema")
	}
	message := err.Error()

	// The plain path is named before anything else, so a reader sees the file
	// first and the DSN only as secondary context.
	if !strings.Contains(message, target) {
		t.Errorf("refusal never names the real file %s; got: %s", target, message)
	}

	fixIndex := strings.Index(message, "fix: ")
	if fixIndex < 0 {
		t.Fatalf("refusal has no fix clause: %s", message)
	}
	fix := message[fixIndex:]
	for _, want := range []string{target, target + "-wal", target + "-shm"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix clause does not name %s; got: %s", want, fix)
		}
	}
	// A reader copies the fix clause into a shell. Anything from the DSN's query
	// string there would make the command wrong.
	for _, forbidden := range []string{"_pragma", "file:", "mode=rw", "?"} {
		if strings.Contains(fix, forbidden) {
			t.Errorf("fix clause contains DSN syntax %q, which no shell accepts as a file name; got: %s", forbidden, fix)
		}
	}
}

// A concurrent first launch commits the runtime's migrations one at a time, so a
// second opener can read a version below the floor while the database is being
// created, not because it is superseded. The refusal must say so before it tells
// anybody to delete a file.
func TestRequireSupportedSchemaWarnsAboutAConcurrentFirstLaunch(t *testing.T) {
	t.Parallel()
	err := dbossys.RequireSupportedSchema(context.Background(), openSupersededFixtureCopy(t), "fixture.db")
	if err == nil {
		t.Fatal("RequireSupportedSchema accepted a superseded system schema")
	}
	message := err.Error()
	caution := strings.Index(message, "caution: ")
	fix := strings.Index(message, "fix: ")
	if caution < 0 {
		t.Fatalf("refusal does not mention the concurrent-first-launch transient: %s", message)
	}
	if fix < 0 || caution > fix {
		t.Errorf("the caution must precede the deletion instruction; got: %s", message)
	}
	for _, want := range []string{"still creating", "no other process"} {
		if !strings.Contains(message, want) {
			t.Errorf("caution is missing %q; got: %s", want, message)
		}
	}
}

// MainDatabaseFile answers with the file SQLite itself reports, and reports an
// empty path (not an error) for a database that has no file.
func TestMainDatabaseFileReportsTheFileBehindADSN(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "named.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a named database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	got, err := dbossys.MainDatabaseFile(context.Background(), db)
	if err != nil {
		t.Fatalf("MainDatabaseFile on a named database: %v", err)
	}
	if got != path {
		t.Errorf("MainDatabaseFile = %q, want the plain path %q", got, path)
	}
}

func TestMainDatabaseFileReportsNoPathForAnInMemoryDatabase(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open an in-memory database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	got, err := dbossys.MainDatabaseFile(context.Background(), db)
	if err != nil {
		t.Fatalf("MainDatabaseFile on an in-memory database: %v", err)
	}
	if got != "" {
		t.Errorf("MainDatabaseFile = %q, want an empty path for a database with no file", got)
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
