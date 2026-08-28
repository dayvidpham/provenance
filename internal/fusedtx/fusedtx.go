package fusedtx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	dbos "github.com/dbos-inc/dbos-transact-golang/dbos"
	// The DBOS runtime keeps no SQLite driver of its own: it uses whichever
	// driver this blank import registers, even when the caller supplies the
	// *sql.DB handle. Without the import every construction fails at run time,
	// and the runtime loses the error-code extractor it needs to tell a busy or
	// locked database apart from a permanent failure.
	// Source: dbos/internal/sysdb/sqlite_driver.go and dbos/driver/sqlite.
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"
	_ "modernc.org/sqlite"

	"github.com/dayvidpham/provenance/internal/dbossys"
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
	root       dbos.Context
	systemDB   *sql.DB
	dataSource *dbos.DataSource
	closeOnce  sync.Once
	closeErr   error
}

// BindSystem borrows a host-created DBOS root and its exact SQLite system
// handle. It creates only the matching DBOS data source: it never opens a
// database, creates or launches a DBOS root, or assumes shutdown ownership.
// Hosts must call it before launching root so workflows can be registered on
// the same application boundary.
func BindSystem(root dbos.Context, systemDB *sql.DB) (*System, error) {
	if root == nil {
		return nil, fmt.Errorf("fusedtx.BindSystem: DBOS root is nil -- where: host-bound fused system construction; impact: no composed workflow can be registered; fix: pass the exact context returned by dbos.NewContext before launch")
	}
	if systemDB == nil {
		return nil, fmt.Errorf("fusedtx.BindSystem: system database is nil -- where: host-bound fused system construction; impact: DBOS and Provenance cannot share one transaction source; fix: pass the exact *sql.DB used as DBOS Config.SQLiteSystemDB")
	}
	dataSource, err := dbos.NewDataSource(root, systemDB)
	if err != nil {
		return nil, fmt.Errorf("fusedtx.BindSystem: bind DBOS data source to the host system database -- where: host-bound fused system construction; impact: the composed workflow was not registered; fix: pass the exact pre-launch DBOS root and its SQLiteSystemDB handle: %w", err)
	}
	return &System{root: root, systemDB: systemDB, dataSource: dataSource}, nil
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

	// Refuse a system database that a superseded DBOS runtime wrote, BEFORE any
	// DBOS context exists. The runtime would otherwise migrate it in place on
	// construction, and this build supports no such upgrade.
	if err := dbossys.RequireSupportedSchema(ctx, systemDB, config.SQLiteDSN); err != nil {
		_ = systemDB.Close()
		return nil, fmt.Errorf("fusedtx.OpenSystem: %w", err)
	}

	// Constructing the context also creates DBOS's reserved internal queue. It is
	// the one queue that stays in process rather than in the queues table, so a
	// same-named RegisterQueue is rejected and its polling cadence stays at the
	// package default; dbos.Config exposes no field for it. Provenance neither
	// registers a queue nor enqueues a workflow, so the polling never dequeues
	// work and is not on any workflow-latency path. It is NOT free of side
	// effects, though: the queue supervisor's fixed once-a-second reconcile tick
	// executes an UPDATE on workflow_status against the system database, and in
	// fusedtx that database IS the application's SQLite file — so the tick
	// periodically takes the single-writer lock and can surface as SQLITE_BUSY
	// under concurrent application writes. The supported runtime narrows that
	// UPDATE to rows owned by Config.AppName, which this constructor always
	// sets, but it does not remove the write. See
	// docs/perf/parallel-governed-allocation-family.md for the measured
	// contention and docs/test-performance.md for the upstream ask.
	// Source: dbos/queue.go, queueRunner.run, and
	// dbos/internal/sysdb/system_database.go, SysDB.TransitionDelayedWorkflows.
	root, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            config.AppName,
		ApplicationVersion: config.ApplicationVersion,
		Logger:             config.Logger,
		SQLiteSystemDB:     systemDB,
	})
	if err != nil {
		_ = systemDB.Close()
		return nil, fmt.Errorf("fusedtx.OpenSystem: create DBOS root over owned system handle: %w",
			dbossys.DescribeOpenFailure(dbossys.ClassifyOpenFailure(err), config.SQLiteDSN, err))
	}
	dataSource, err := dbos.NewDataSource(root, systemDB)
	if err != nil {
		// The root is torn down, so the handle must not be closed here as well:
		// the DBOS runtime owns closing the pool it was given.
		_ = dbos.Shutdown(root, 30*time.Second)
		return nil, fmt.Errorf(
			"fusedtx.OpenSystem: create the DBOS data source with the owned system handle: %w; "+
				"where: fused transaction construction; impact: application SQL is not available through DBOS; "+
				"fix: repair the SQLite system database and recreate the fused capability",
			err)
	}
	return &System{root: root, systemDB: systemDB, dataSource: dataSource}, nil
}

// Root exposes the DBOS root only to packages inside this module's internal
// boundary. The root package wraps System in a public capability without
// exposing this method or the exact SQLite handle to ordinary callers.
func (s *System) Root() dbos.Context { return s.root }

// DB exposes the factory-owned handle only within the Go internal boundary so
// the root wrapper can activate the matching Provenance schema. It must not be
// closed directly; System.Close owns the DBOS lifecycle.
func (s *System) DB() *sql.DB { return s.systemDB }

// Close shuts down the DBOS root exactly once and reports the outcome. A
// non-nil error means the timeout expired while DBOS resources were still
// running: the shared SQLite handle is then still in use, so no caller may
// close it. DBOS owns the factory-created system pool's close operation, so
// System deliberately does not close it either way.
//
// Close is idempotent and repeats the first outcome on every later call.
func (s *System) Close(timeoutDuration time.Duration) error {
	if s == nil || s.root == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if err := dbos.Shutdown(s.root, timeoutDuration); err != nil {
			s.closeErr = fmt.Errorf(
				"fusedtx.System.Close: DBOS shutdown did not finish within %s -- where: fused system teardown; "+
					"impact: DBOS resources are still running on the shared SQLite handle, so that handle must NOT be closed; "+
					"fix: allow a longer timeout, or stop the workflows that are still running, then close again: %w",
				timeoutDuration, err)
		}
	})
	return s.closeErr
}

// Run invokes callback through DBOS's public RunAsTransaction API. DBOS owns
// transaction begin, commit, rollback, retries, and operation_outputs
// checkpointing; callback receives only the local application SQL surface.
func Run[R any](ctx dbos.Context, system *System, callback Callback[R]) (R, error) {
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
