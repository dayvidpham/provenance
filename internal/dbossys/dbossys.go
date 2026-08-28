// Package dbossys owns the DBOS system-database preflight that Provenance runs
// before it hands a SQLite handle to the DBOS runtime, and the classification of
// the failures that construction can return.
//
// It deliberately imports no DBOS SQLite driver. Registering that driver is the
// responsibility of the packages that actually open a system database, so this
// package's own test binary can observe the real missing-driver failure.
package dbossys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// FirstSupportedMigrationVersion is the first DBOS system-schema migration
// version that the supported DBOS runtime introduces, and therefore the floor
// this package enforces.
//
// The superseded runtime (dbos-transact-golang v0.20.0) stopped at SQLite
// migration 41; the supported runtime (v1.2.0) starts its own history at
// migration 42 and ends at 107. Migrations 1 through 41 are byte-identical
// between the two, so the recorded version alone separates the two shapes.
// Source: dbos/internal/sysdb/sqlite_migrations.go, BuildSqliteMigrations.
const FirstSupportedMigrationVersion int64 = 42

// migrationTable is the single-row table the DBOS runtime keeps its system
// schema version in. Source: dbos/internal/sysdb/system_database.go,
// MigrationTable.
const migrationTable = "dbos_migrations"

// missingSQLiteDriverMarker is the stable sentence the DBOS runtime emits when
// no SQLite driver package was blank-imported into the binary. The runtime
// requires that driver even for a caller-supplied handle, because its
// busy/locked classification uses the driver's error-code extractor.
// Source: dbos/internal/sysdb/sqlite_driver.go, registeredSQLiteDriver.
const missingSQLiteDriverMarker = "SQLite support is not linked into this binary"

// ErrSupersededSystemSchema marks every refusal of a system database that a
// superseded DBOS runtime created. Callers match it with errors.Is.
var ErrSupersededSystemSchema = errors.New("dbossys: superseded DBOS system schema")

// SchemaState is the closed classification of a candidate DBOS system database.
type SchemaState int

const (
	// SchemaStateUnknown is the zero value and is never returned on success.
	SchemaStateUnknown SchemaState = iota
	// SchemaStateFresh means the database records no DBOS system schema. The
	// DBOS runtime creates one on its first launch.
	SchemaStateFresh
	// SchemaStateSupported means the recorded schema version is at or above the
	// supported floor.
	SchemaStateSupported
	// SchemaStateSuperseded means a superseded DBOS runtime wrote the schema.
	// Provenance refuses it: there is no supported in-place upgrade.
	SchemaStateSuperseded
)

func (s SchemaState) String() string {
	switch s {
	case SchemaStateFresh:
		return "fresh"
	case SchemaStateSupported:
		return "supported"
	case SchemaStateSuperseded:
		return "superseded"
	default:
		return "unknown"
	}
}

// OpenFailure is the closed classification of a DBOS construction failure.
type OpenFailure int

const (
	// OpenFailureNone reports that there was no failure.
	OpenFailureNone OpenFailure = iota
	// OpenFailureUnknown reports a failure this package does not recognise.
	OpenFailureUnknown
	// OpenFailureMissingSQLiteDriver reports that the binary linked no DBOS
	// SQLite driver. It is a build defect, not a transient condition.
	OpenFailureMissingSQLiteDriver
)

func (f OpenFailure) String() string {
	switch f {
	case OpenFailureNone:
		return "none"
	case OpenFailureMissingSQLiteDriver:
		return "missing-sqlite-driver"
	default:
		return "unknown"
	}
}

// Retryable reports whether repeating the same construction can ever succeed.
// An unrecognised failure stays retryable so it remains on the caller's normal
// retry channel; a missing driver never becomes present at run time.
func (f OpenFailure) Retryable() bool {
	switch f {
	case OpenFailureMissingSQLiteDriver:
		return false
	default:
		return true
	}
}

// InspectSchema reports the DBOS system-schema state of db and the migration
// version it records. A database with no migration table is fresh and reports
// version 0. InspectSchema only reads; it creates and changes nothing.
func InspectSchema(ctx context.Context, db *sql.DB) (SchemaState, int64, error) {
	if db == nil {
		return SchemaStateUnknown, 0, fmt.Errorf(
			"dbossys.InspectSchema: database handle is nil -- where: DBOS system-schema preflight; " +
				"impact: no schema state was determined and nothing was opened; " +
				"fix: pass the exact live *sql.DB that will become the DBOS system handle")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var present int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, migrationTable).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return SchemaStateFresh, 0, nil
	}
	if err != nil {
		return SchemaStateUnknown, 0, fmt.Errorf(
			"dbossys.InspectSchema: probe sqlite_master for the %s table -- where: DBOS system-schema preflight; "+
				"impact: the schema state is unknown and nothing was opened; "+
				"fix: repair or replace the SQLite file, then retry: %w", migrationTable, err)
	}
	var version int64
	err = db.QueryRowContext(ctx, `SELECT version FROM `+migrationTable+` LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		// The table exists but records no version: a run that created the table
		// and then stopped. The DBOS runtime treats that as version 0 and
		// migrates from the start, so it is a fresh database here too.
		return SchemaStateFresh, 0, nil
	}
	if err != nil {
		return SchemaStateUnknown, 0, fmt.Errorf(
			"dbossys.InspectSchema: read the recorded version from %s -- where: DBOS system-schema preflight; "+
				"impact: the schema state is unknown and nothing was opened; "+
				"fix: repair or replace the SQLite file, then retry: %w", migrationTable, err)
	}
	if version < FirstSupportedMigrationVersion {
		return SchemaStateSuperseded, version, nil
	}
	return SchemaStateSupported, version, nil
}

// RequireSupportedSchema refuses a system database that a superseded DBOS
// runtime created. origin names the database for the operator, normally its
// path or DSN.
//
// The refusal is deliberate and final: Provenance supports no in-place upgrade
// of a superseded system database, so it must not let the DBOS runtime migrate
// one in place.
func RequireSupportedSchema(ctx context.Context, db *sql.DB, origin string) error {
	if origin == "" {
		origin = "the DBOS system database"
	}
	state, version, err := InspectSchema(ctx, db)
	if err != nil {
		return fmt.Errorf("dbossys.RequireSupportedSchema: inspect %s: %w", origin, err)
	}
	if state != SchemaStateSuperseded {
		return nil
	}
	return fmt.Errorf(
		"dbossys.RequireSupportedSchema: %s records DBOS system schema version %d, below the supported floor %d "+
			"-- where: DBOS system-schema preflight, before any DBOS context is created; "+
			"why: a superseded DBOS runtime wrote it, and this build supports no in-place upgrade of that durable state; "+
			"impact: nothing was opened, launched, or migrated, and the file is unchanged; "+
			"fix: drain or abandon the old workflows, delete the database file %s (and its -wal and -shm siblings), "+
			"then let this build create a fresh one: %w",
		origin, version, FirstSupportedMigrationVersion, origin, ErrSupersededSystemSchema)
}

// ClassifyOpenFailure maps a DBOS construction error onto the closed set of
// failures Provenance reports differently.
func ClassifyOpenFailure(err error) OpenFailure {
	if err == nil {
		return OpenFailureNone
	}
	if strings.Contains(err.Error(), missingSQLiteDriverMarker) {
		return OpenFailureMissingSQLiteDriver
	}
	return OpenFailureUnknown
}

// DescribeOpenFailure wraps a classified construction failure in an actionable
// error. origin names the database for the operator.
func DescribeOpenFailure(failure OpenFailure, origin string, err error) error {
	if err == nil {
		return nil
	}
	if origin == "" {
		origin = "the DBOS system database"
	}
	switch failure {
	case OpenFailureMissingSQLiteDriver:
		return fmt.Errorf(
			"dbossys: this binary links no DBOS SQLite driver, so it cannot open %s "+
				"-- where: DBOS context construction; "+
				"why: the DBOS runtime needs that driver even for a caller-supplied handle, to classify busy and locked errors; "+
				"impact: no DBOS context, workflow, or durable write exists, and retrying cannot fix it; "+
				`fix: add the blank import _ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite" to the binary, then rebuild: %w`,
			origin, err)
	default:
		return fmt.Errorf(
			"dbossys: DBOS context construction failed for %s -- where: DBOS context construction; "+
				"impact: no DBOS context, workflow, or durable write exists; "+
				"fix: read the wrapped runtime error, repair the named condition, then retry: %w",
			origin, err)
	}
}
