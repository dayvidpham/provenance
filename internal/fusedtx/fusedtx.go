package fusedtx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "modernc.org/sqlite"
)

// SystemConfig is the closed construction contract for an owned DBOS SQLite
// system. No existing DBOS context or database handle can be supplied: OpenSystem
// opens one handle, gives that exact pointer to DBOS, and constructs the data
// source from it before returning the opaque capability.
type SystemConfig struct {
	SQLiteDSN          string
	AppName            string
	ApplicationVersion string
	Logger             *slog.Logger
}

// System owns one DBOS root, its sole SQLite system handle, and the exact-handle
// DataSource. It is internal to this module; root-package capabilities use it to
// ensure application code cannot pair an unrelated DBOS context with another
// same-file SQLite handle.
type System struct {
	root       dbos.DBOSContext
	systemDB   *sql.DB
	dataSource *dbos.DataSource
	closeOnce  sync.Once
}

// OpenSystem creates the DBOS root and exact system handle as one ownership
// unit. Close shuts down that root; because the handle is factory-owned, callers
// never need to close or retain a second SQLite pool.
func OpenSystem(ctx context.Context, config SystemConfig) (*System, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if config.SQLiteDSN == "" {
		return nil, fmt.Errorf("fusedtx.OpenSystem: SQLiteDSN is empty -- where: fused system construction; impact: no exact DBOS system handle can be created; fix: provide a durable Modernc SQLite DSN")
	}
	if config.AppName == "" {
		return nil, fmt.Errorf("fusedtx.OpenSystem: AppName is empty -- where: fused system construction; impact: DBOS cannot create a root context; fix: provide a stable application name")
	}
	systemDB, err := sql.Open("sqlite", config.SQLiteDSN)
	if err != nil {
		return nil, fmt.Errorf("fusedtx.OpenSystem: open owned SQLite system handle: %w", err)
	}
	systemDB.SetMaxOpenConns(16)
	systemDB.SetMaxIdleConns(8)
	if err := systemDB.PingContext(ctx); err != nil {
		_ = systemDB.Close()
		return nil, fmt.Errorf("fusedtx.OpenSystem: ping owned SQLite system handle: %w", err)
	}

	root, err := dbos.NewDBOSContext(ctx, dbos.Config{
		AppName:            config.AppName,
		ApplicationVersion: config.ApplicationVersion,
		Logger:             config.Logger,
		SqliteSystemDB:     systemDB,
	})
	if err != nil {
		_ = systemDB.Close()
		return nil, fmt.Errorf("fusedtx.OpenSystem: create DBOS root over owned system handle: %w", err)
	}
	dataSource, err := dbos.NewDataSource(root, systemDB)
	if err != nil {
		dbos.Shutdown(root, 30*time.Second)
		return nil, fmt.Errorf(
			"fusedtx.OpenSystem: create DBOS v0.20 data source with the owned system handle: %w; "+
				"where: fused transaction construction; impact: application SQL is not available through DBOS; "+
				"fix: repair the SQLite system database and recreate the fused capability",
			err)
	}
	return &System{root: root, systemDB: systemDB, dataSource: dataSource}, nil
}

// Root exposes the DBOS root only to packages inside this module's internal
// boundary. The root package wraps System in a public capability without
// exposing this method or the exact SQLite handle to ordinary callers.
func (s *System) Root() dbos.DBOSContext { return s.root }

// DB exposes the factory-owned handle only within the Go internal boundary so
// the root wrapper can activate the matching Provenance schema. It must not be
// closed directly; System.Close owns the DBOS lifecycle.
func (s *System) DB() *sql.DB { return s.systemDB }

// Close shuts down the DBOS root exactly once. DBOS owns the factory-created
// system pool's close operation, so System deliberately does not close it again.
func (s *System) Close(timeoutDuration time.Duration) {
	if s == nil || s.root == nil {
		return
	}
	s.closeOnce.Do(func() { dbos.Shutdown(s.root, timeoutDuration) })
}

// Run invokes callback through DBOS's public RunAsTransaction API. DBOS owns
// transaction begin, commit, rollback, retries, and operation_outputs
// checkpointing; callback receives only the local application SQL surface.
func Run[R any](ctx dbos.DBOSContext, system *System, callback Callback[R]) (R, error) {
	if ctx == nil {
		return *new(R), fmt.Errorf(
			"fusedtx.Run: workflow context is nil -- where: fused transaction execution; " +
				"impact: the application callback was not run; fix: call Run from a registered DBOS workflow with its DBOSContext")
	}
	if system == nil || system.dataSource == nil {
		return *new(R), fmt.Errorf(
			"fusedtx.Run: fused system is nil or uninitialized -- where: fused transaction execution; " +
				"impact: the application callback was not run; fix: construct the root-owned fused capability before running its workflow")
	}
	if callback == nil {
		return *new(R), fmt.Errorf(
			"fusedtx.Run: application callback is nil -- where: fused transaction execution; " +
				"impact: no application SQL or DBOS checkpoint was attempted; fix: provide a callback that returns the application result")
	}

	return dbos.RunAsTransaction(ctx, system.dataSource, func(callbackCtx context.Context, tx dbos.Tx) (R, error) {
		return callback(callbackCtx, dbosTxAdapter{tx: tx})
	})
}

// dbosTxAdapter prevents DBOS result, row, cursor, and transaction-control
// types from crossing into application reducers.
type dbosTxAdapter struct {
	tx dbos.Tx
}

var _ SQLTx = dbosTxAdapter{}

func (a dbosTxAdapter) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	result, err := a.tx.Exec(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (a dbosTxAdapter) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := a.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (a dbosTxAdapter) QueryRow(ctx context.Context, query string, args ...any) Row {
	return a.tx.QueryRow(ctx, query, args...)
}
