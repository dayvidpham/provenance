// Package sqlite provides the SQLite persistence layer for the Provenance
// task dependency tracker. It implements all CRUD operations for tasks, edges,
// agents, labels, comments, and activities.
//
// This package imports pkg/ptypes for all type definitions and uses
// zombiezen.com/go/sqlite for pure-Go SQLite access (no CGo required at
// runtime, though CGo tests use the C library for the race detector).
//
// # Connection model
//
// A [DB] is the pool-backed SQLite database handle and persistence owner. It
// owns exactly one sqlitex.Pool and its race-safe close lifecycle state. It
// holds no connection of its own: every SQL statement runs on a [connScope]
// whose ownership is explicit. There is exactly one runtime ownership entry
// point, [DB.bindScope]: the restored `bindScope(ctx, target)` selector lets
// ordinary operations lease a scope explicitly targeted at the live projection.
// Replay does not lease a separate shadow scope; TEMP tables are connection-local,
// so it stages them on its already-owned scope, repoints that scope's selector in
// place for the refold, and restores the live target in a defer. Startup instead
// borrows a single activation scope with [borrowConnScope] and threads it through
// every schema, seed, integrity, replay, and pragma helper. Every leased scope
// owns an independent context-controlled pool lease, for both file and memory
// storage.
package sqlite

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// runtimePoolSize is the connection-pool size for file-backed databases.
// Four concurrent connections support typical parallel Apply + read workloads
// while staying well within SQLite's practical per-process connection budget.
const runtimePoolSize = 4

// memoryPoolSize is the connection-pool size for a unique shared-cache
// ":memory:" database. One connection gives bounded serial in-memory semantics
// while keeping acquisition context-cancellable through Pool.Take.
const memoryPoolSize = 1

// closeResult runs one close operation and publishes its result to every
// concurrent and later caller. It is deliberately only lifecycle state, not a
// pool abstraction or dependency-injection boundary.
type closeResult struct {
	once sync.Once
	err  error
}

func (result *closeResult) do(closeFunc func() error) error {
	result.once.Do(func() {
		result.err = closeFunc()
	})
	return result.err
}

// DB is the pool-backed SQLite database handle and persistence owner for safe
// concurrent access. It owns the runtime pool, schema lifecycle, transaction
// paths, and shutdown boundary; individual operations lease [connScope]
// connections from it. Use [Open] to create a new DB instance.
//
// It holds no connection, no mutex, and no projection selector:
// connection ownership is always explicit and always lives on a [connScope].
type DB struct {
	// pool is the sole runtime connection source. Callers bind a connScope.
	pool *sqlitex.Pool

	// close makes shutdown exactly-once and publishes one result to every
	// concurrent and later caller.
	close closeResult
}

// projectionTarget is a closed selector for complete static SQL variants.
// SQLite cannot bind identifiers, so arbitrary table names are never stored.
// It lives on a connScope, never on a DB.
type projectionTarget uint8

const (
	projectionTargetLive projectionTarget = iota
	projectionTargetShadow
)

func (target projectionTarget) label() string {
	switch target {
	case projectionTargetLive:
		return "live projection"
	case projectionTargetShadow:
		return "shadow projection"
	default:
		panic("unknown projection target")
	}
}

// Open opens (or creates) a SQLite database at dbPath and returns an
// initialised DB. Pass ":memory:" for an in-memory database.
//
// For file-backed databases a pool of [runtimePoolSize] connections is opened.
// For ":memory:" the URI "file:memdbN?mode=memory&cache=shared" is used with
// [memoryPoolSize] connections (N is a unique process-level counter). The
// shared-cache form ensures that a separate preflight/activation connection and
// the pool see the same logical database.
//
// The schema is applied idempotently on every open (CREATE TABLE IF NOT EXISTS).
// Reference data (enums) is inserted via INSERT OR IGNORE.
// The models parameter provides the ML model entries to seed into ml_models.
func Open(dbPath string, models []ptypes.ModelEntry) (*DB, error) {
	// Resolve the URI and pool size for the given dbPath.
	poolURI, poolSize, isMemory := resolvePoolTarget(dbPath)

	// -------------------------------------------------------------------------
	// Step 1: For file-backed databases run a read-only preflight first.
	//         For in-memory databases there is nothing to preflight.
	// -------------------------------------------------------------------------
	existingJournal := false
	if !isMemory {
		existed := false
		if info, err := os.Stat(dbPath); err == nil {
			existed = info.Size() > 0
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("sqlite.Open: inspect path %q before read-only preflight: %w", dbPath, err)
		}
		if existed {
			var err error
			existingJournal, err = preflightExistingReadOnly(dbPath, models)
			if err != nil {
				return nil, fmt.Errorf("sqlite.Open: read-only startup preflight failed on %q: %w", dbPath, err)
			}
		}
	}

	// -------------------------------------------------------------------------
	// Step 2: Open the runtime pool.
	//
	// For in-memory databases the pool must be opened BEFORE activation so
	// that at least one connection keeps the shared-cache database alive
	// throughout activation. Closing the only connection to a shared-cache
	// in-memory database destroys it.
	//
	// For file-backed databases the pool is opened after activation so that
	// activation has exclusive write access during schema migration.
	// -------------------------------------------------------------------------
	if isMemory {
		return openInMemory(poolURI, models)
	}
	return openFileBacked(dbPath, poolURI, poolSize, existingJournal, models)
}

// openInMemory handles Open for ":memory:" databases.
// The pool is opened first to keep the shared-cache database alive, then
// activation runs on a leased connection from the pool.
func openInMemory(poolURI string, models []ptypes.ModelEntry) (*DB, error) {
	pool, err := sqlitex.NewPool(poolURI, sqlitex.PoolOptions{
		Flags:       zs.OpenReadWrite | zs.OpenCreate | zs.OpenWAL | zs.OpenURI,
		PoolSize:    memoryPoolSize,
		PrepareConn: runtimePrepareConn,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.Open: failed to open in-memory pool at %q: %w",
			poolURI, err,
		)
	}

	// Activation borrows exactly one connection and owns it for the whole
	// activation window; every activation helper below takes that one scope.
	activationConn, err := pool.Take(context.Background())
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("sqlite.Open: failed to take connection for in-memory activation at %q: %w", poolURI, err)
	}

	activation := borrowConnScope(activationConn, projectionTargetLive)
	// Apply activation pragmas: busy_timeout is already set by PrepareConn but
	// we need foreign_keys=OFF for schema rebuilds.
	if err := sqlitex.ExecuteTransient(activationConn, "PRAGMA foreign_keys=OFF;", nil); err != nil {
		pool.Put(activationConn)
		_ = pool.Close()
		return nil, fmt.Errorf("sqlite.Open: disable FK enforcement for in-memory activation at %q: %w", poolURI, err)
	}

	var activationErr error
	end := sqlitex.Save(activationConn)
	activationErr = func() error {
		if err := activation.ensureSchema(models); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := activation.verifyIntegrity(); err != nil {
			return fmt.Errorf("whole-journal integrity: %w", err)
		}
		if _, err := activation.replayProjections(); err != nil {
			return fmt.Errorf("journal replay: %w", err)
		}
		return nil
	}()
	end(&activationErr)
	if activationErr != nil {
		pool.Put(activationConn)
		_ = pool.Close()
		return nil, fmt.Errorf("sqlite.Open: in-memory activation failed at %q: %w", poolURI, activationErr)
	}
	// Re-enable FK enforcement on this connection after schema activation.
	if err := sqlitex.ExecuteTransient(activationConn, "PRAGMA foreign_keys=ON;", nil); err != nil {
		pool.Put(activationConn)
		_ = pool.Close()
		return nil, fmt.Errorf("sqlite.Open: re-enable FK enforcement after in-memory activation at %q: %w", poolURI, err)
	}

	// Activation is finished: return its borrowed connection so the whole pool
	// is available to runtime leases.
	pool.Put(activationConn)

	return &DB{pool: pool}, nil
}

// openFileBacked handles Open for file-backed databases.
// Activation runs on a dedicated connection that is closed before the pool
// is opened, giving activation exclusive schema write access.
func openFileBacked(dbPath, poolURI string, poolSize int, existingJournal bool, models []ptypes.ModelEntry) (*DB, error) {
	activationConn, err := zs.OpenConn(poolURI, zs.OpenReadWrite|zs.OpenCreate|zs.OpenURI)
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.Open: failed to open activation connection at %q (resolved URI %q): %w — "+
				"ensure the path is writable, the parent directory exists, "+
				"and no other process holds an exclusive lock",
			dbPath, poolURI, err,
		)
	}

	activation := borrowConnScope(activationConn, projectionTargetLive)
	if err := activation.applyActivationPragmas(); err != nil {
		_ = activationConn.Close()
		return nil, fmt.Errorf("sqlite.Open: failed to apply activation pragmas on %q: %w", dbPath, err)
	}

	existing, err := activation.tableExists("journal")
	if err != nil {
		_ = activationConn.Close()
		return nil, fmt.Errorf("sqlite.Open: inspect existing schema on %q: %w", dbPath, err)
	}
	if existing != existingJournal {
		_ = activationConn.Close()
		return nil, fmt.Errorf(
			"sqlite.Open: schema changed between read-only preflight (journal=%t) and activation (journal=%t) on %q; "+
				"retry after concurrent schema work finishes",
			existingJournal, existing, dbPath,
		)
	}

	var activationErr error
	end := sqlitex.Save(activationConn)
	activationErr = func() error {
		if err := activation.ensureSchema(models); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := activation.verifyIntegrity(); err != nil {
			return fmt.Errorf("whole-journal integrity: %w", err)
		}
		if _, err := activation.replayProjections(); err != nil {
			return fmt.Errorf("journal replay: %w", err)
		}
		return nil
	}()
	end(&activationErr)
	if activationErr != nil {
		_ = activationConn.Close()
		return nil, fmt.Errorf("sqlite.Open: transactional startup validation failed on %q: %w", dbPath, activationErr)
	}
	// Enable WAL on the activation connection before closing it. WAL is a
	// file-level persistent property; per-connection pool flags (OpenWAL)
	// confirm the mode but the initial activation must set it first.
	if err := activation.enableWAL(); err != nil {
		_ = activationConn.Close()
		return nil, fmt.Errorf("sqlite.Open: enable WAL after validated activation on %q: %w", dbPath, err)
	}
	if err := activationConn.Close(); err != nil {
		return nil, fmt.Errorf("sqlite.Open: close activation connection on %q: %w", dbPath, err)
	}

	pool, err := sqlitex.NewPool(poolURI, sqlitex.PoolOptions{
		Flags:       zs.OpenReadWrite | zs.OpenCreate | zs.OpenWAL | zs.OpenURI,
		PoolSize:    poolSize,
		PrepareConn: runtimePrepareConn,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.Open: failed to open runtime pool (size=%d) at %q (resolved URI %q): %w — "+
				"ensure the path is accessible and no other process holds a conflicting lock",
			poolSize, dbPath, poolURI, err,
		)
	}

	return &DB{pool: pool}, nil
}

// memoryDBCounter generates unique names for shared-cache in-memory databases.
// Each call to Open(":memory:", ...) gets its own isolated database so parallel
// tests do not share schema state. The counter is process-global and monotonically
// increasing; it never needs to be reset.
var memoryDBCounter atomic.Uint64

// resolvePoolTarget maps an Open dbPath to the URI, pool size, and in-memory flag.
//
//   - ":memory:" becomes a unique shared-cache URI of the form
//     "file:memdbN?mode=memory&cache=shared" with [memoryPoolSize] and
//     isMemory=true.
//     Each Open(":memory:") call allocates its own unique name via
//     [memoryDBCounter] so parallel callers do not share the same database.
//   - Any other path is used as-is with [runtimePoolSize] and isMemory=false.
func resolvePoolTarget(dbPath string) (uri string, poolSize int, isMemory bool) {
	if dbPath == ":memory:" {
		n := memoryDBCounter.Add(1)
		return fmt.Sprintf("file:memdb%d?mode=memory&cache=shared", n), memoryPoolSize, true
	}
	return dbPath, runtimePoolSize, false
}

// runtimePrepareConn is the PrepareConn callback for the runtime pool.
// sqlitex.Pool calls it exactly once per connection the first time it is leased.
// It applies per-connection PRAGMAs:
//
//   - foreign_keys=ON  — enforce referential integrity on every write.
//   - busy_timeout=5000 — retry write-lock acquisition for up to 5 s before SQLITE_BUSY.
func runtimePrepareConn(conn *zs.Conn) error {
	for _, pragma := range []string{
		"PRAGMA foreign_keys=ON;",
		"PRAGMA busy_timeout=5000;",
	} {
		if err := sqlitex.ExecuteTransient(conn, pragma, nil); err != nil {
			return fmt.Errorf(
				"sqlite: runtimePrepareConn: failed to apply %q: %w — "+
					"where: pool connection initialization (PrepareConn); "+
					"when: first Take of this connection; "+
					"impact: connection cannot be used; it will be retried on next Take; "+
					"fix: this is an internal error; ensure the SQLite library is functional",
				pragma, err,
			)
		}
	}
	return nil
}

// connScope is the single connection-ownership contract in this package: it
// carries the connection every SQL statement runs on and the complete static
// SQL projection variant that connection is executing against. A scope must be
// released exactly as an owned resource; release is idempotent so cleanup paths
// cannot return a lease twice. A borrowed (activation) scope releases to a
// no-op, so activation lifetime stays with its owner.
type connScope struct {
	conn             *zs.Conn
	projectionTarget projectionTarget
	releaseOnce      sync.Once
	releaseFunc      func()
}

func (scope *connScope) release() {
	if scope == nil {
		return
	}
	scope.releaseOnce.Do(func() {
		if scope.releaseFunc != nil {
			scope.releaseFunc()
		}
	})
}

// bindScope is the single runtime connection-ownership entry point. It leases
// one pool connection and returns the scope that owns it until release.
//
// The caller states the complete static SQL projection variant at the bind site;
// ordinary operations pass projectionTargetLive. The target is explicit because
// projection SQL cannot bind identifiers and TEMP shadow tables are local to the
// connection that owns them. Replay therefore stages its TEMP tables on its
// already-owned scope, repoints scope.projectionTarget to projectionTargetShadow
// in place, and restores projectionTargetLive in a defer before dropping them;
// it does not obtain a separate shadow scope through binding.
//
// Pool.Take owns context interruption for file and memory storage.
func (db *DB) bindScope(ctx context.Context, target projectionTarget) (*connScope, error) {
	conn, err := db.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite: bind connection during operation: %w; caller cannot continue; "+
				"fix: return outstanding scopes, retry with a live context, or open a new DB if this pool was closed",
			err,
		)
	}
	return &connScope{
		conn:             conn,
		projectionTarget: target,
		releaseFunc:      func() { db.pool.Put(conn) },
	}, nil
}

// borrowConnScope binds activation-owned connection lifetime to an operation
// scope without transferring ownership. Releasing a borrowed scope is a no-op.
func borrowConnScope(conn *zs.Conn, target projectionTarget) *connScope {
	return &connScope{conn: conn, projectionTarget: target}
}

// Close shuts down the pool. It is safe to call from multiple goroutines. Every
// caller waits for the first close attempt and observes the same result.
//
// Shutdown runs solely through the pool: Pool.Close interrupts every in-flight
// statement on every leased connection and then drains, returning once each
// outstanding scope has been released. The underlying SQLite files are not
// deleted.
func (db *DB) Close() error {
	return db.close.do(func() error {
		if err := db.pool.Close(); err != nil {
			return fmt.Errorf(
				"sqlite.DB.Close: failed to close connection pool: %w; "+
					"the DB is shut down but one or more SQLite connections failed to close",
				err,
			)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Pragmas
// ---------------------------------------------------------------------------

// applyActivationPragmas configures the caller's activation scope only.
// FK enforcement is disabled during schema rebuilds. The runtime pool's
// runtimePrepareConn re-enables FK enforcement per-connection.
func (scope *connScope) applyActivationPragmas() error {
	for _, pragma := range []string{"PRAGMA busy_timeout=5000;", "PRAGMA foreign_keys=OFF;"} {
		if err := sqlitex.ExecuteTransient(scope.conn, pragma, nil); err != nil {
			return fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	return nil
}

// enableWAL sets the file-level persistent journal mode on the caller's
// activation scope.
func (scope *connScope) enableWAL() error {
	return sqlitex.ExecuteTransient(scope.conn, "PRAGMA journal_mode=WAL", nil)
}

// ---------------------------------------------------------------------------
// Schema DDL
// ---------------------------------------------------------------------------

func (scope *connScope) ensureSchema(models []ptypes.ModelEntry) error {
	ddl := []string{
		"CREATE TABLE IF NOT EXISTS statuses (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS priorities (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS task_types (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS edge_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS agent_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS providers (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS roles (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS phases (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		`CREATE TABLE IF NOT EXISTS ml_models (
			id INTEGER PRIMARY KEY, provider_id INTEGER NOT NULL REFERENCES providers(id), name TEXT NOT NULL,
			UNIQUE (provider_id, name)) STRICT`,
		"CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY, kind_id INTEGER NOT NULL REFERENCES agent_kinds(id)) STRICT",
		"CREATE TABLE IF NOT EXISTS agents_human (agent_id TEXT PRIMARY KEY REFERENCES agents(id), name TEXT NOT NULL, contact TEXT NOT NULL DEFAULT '') STRICT, WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS agents_ml (agent_id TEXT PRIMARY KEY REFERENCES agents(id), role_id INTEGER NOT NULL REFERENCES roles(id), model_id INTEGER NOT NULL REFERENCES ml_models(id)) STRICT, WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS agents_software (agent_id TEXT PRIMARY KEY REFERENCES agents(id), name TEXT NOT NULL, version TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '') STRICT, WITHOUT ROWID",
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),
			priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id), type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),
			phase_id INTEGER NOT NULL REFERENCES phases(id), owner_id TEXT REFERENCES agents(id), notes TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, closed_at INTEGER, close_reason TEXT NOT NULL DEFAULT '',
			last_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)) STRICT`,
		"CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks (priority_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks (type_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_phase ON tasks (phase_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks (owner_id)",
		"CREATE TABLE IF NOT EXISTS edges (source_id TEXT NOT NULL REFERENCES tasks(id), target_id TEXT NOT NULL, kind_id INTEGER NOT NULL REFERENCES edge_kinds(id), created_at INTEGER NOT NULL, PRIMARY KEY (source_id, target_id, kind_id)) STRICT, WITHOUT ROWID",
		"CREATE INDEX IF NOT EXISTS idx_edges_source ON edges (source_id)",
		"CREATE INDEX IF NOT EXISTS idx_edges_target ON edges (target_id)",
		"CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges (kind_id)",
		"CREATE TABLE IF NOT EXISTS activities (id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), phase_id INTEGER NOT NULL REFERENCES phases(id), stage_id INTEGER NOT NULL REFERENCES stages(id), started_at INTEGER NOT NULL, ended_at INTEGER, notes TEXT NOT NULL DEFAULT '') STRICT",
		"CREATE INDEX IF NOT EXISTS idx_activities_agent ON activities (agent_id)",
		"CREATE INDEX IF NOT EXISTS idx_activities_phase ON activities (phase_id)",
		"CREATE TABLE IF NOT EXISTS labels (task_id TEXT NOT NULL REFERENCES tasks(id), name TEXT NOT NULL, PRIMARY KEY (task_id, name)) STRICT, WITHOUT ROWID",
		"CREATE INDEX IF NOT EXISTS idx_labels_name ON labels (name)",
		"CREATE TABLE IF NOT EXISTS comments (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), author_id TEXT NOT NULL REFERENCES agents(id), body TEXT NOT NULL, created_at INTEGER NOT NULL) STRICT",
		"CREATE INDEX IF NOT EXISTS idx_comments_task ON comments (task_id)",
		"CREATE INDEX IF NOT EXISTS idx_comments_author ON comments (author_id)",
	}

	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(scope.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureSchema: statement %q: %w", stmt, err)
		}
	}
	if err := scope.seedReferenceData(models); err != nil {
		return err
	}
	if err := scope.ensureJournalSchema(); err != nil {
		return err
	}
	return scope.ensureOperationsSchema()
}

// ---------------------------------------------------------------------------
// Seed data
// ---------------------------------------------------------------------------

func (scope *connScope) seedReferenceData(models []ptypes.ModelEntry) error {
	seeds := []struct {
		kind  referenceSeedKind
		names []string
	}{
		{seedStatuses, []string{"open", "in_progress", "closed"}},
		{seedPriorities, []string{"critical", "high", "medium", "low", "backlog"}},
		{seedTaskTypes, []string{"bug", "feature", "task", "epic", "chore"}},
		{seedEdgeKinds, []string{"blocked_by", "derived_from", "supersedes", "discovered_from", "generated_by", "attributed_to"}},
		{seedAgentKinds, []string{"human", "machine_learning", "software"}},
		{seedProviders, []string{"anthropic", "google", "openai", "local"}},
		{seedRoles, []string{"human", "architect", "supervisor", "worker", "reviewer"}},
		{seedPhases, []string{"request", "elicit", "propose", "review", "plan_uat", "ratify", "handoff", "impl_plan", "worker_slices", "code_review", "impl_uat", "landing", "unscoped"}},
		{seedStages, []string{"not_started", "in_progress", "blocked", "complete"}},
	}
	for _, seed := range seeds {
		for id, name := range seed.names {
			if err := sqlitex.Execute(scope.conn, seed.kind.query(), &sqlitex.ExecOptions{Args: []any{id, name}}); err != nil {
				return fmt.Errorf("seedReferenceData: kind %d id %d: %w", seed.kind, id, err)
			}
		}
	}

	// Seed ml_models from the provided model registry entries.
	if err := scope.seedMLModels(models); err != nil {
		return fmt.Errorf("seedReferenceData: %w", err)
	}
	return nil
}

type referenceSeedKind uint8

const (
	seedStatuses referenceSeedKind = iota + 1
	seedPriorities
	seedTaskTypes
	seedEdgeKinds
	seedAgentKinds
	seedProviders
	seedRoles
	seedPhases
	seedStages
)

func (kind referenceSeedKind) query() string {
	switch kind {
	case seedStatuses:
		return "INSERT OR IGNORE INTO statuses (id,name) VALUES (?1,?2)"
	case seedPriorities:
		return "INSERT OR IGNORE INTO priorities (id,name) VALUES (?1,?2)"
	case seedTaskTypes:
		return "INSERT OR IGNORE INTO task_types (id,name) VALUES (?1,?2)"
	case seedEdgeKinds:
		return "INSERT OR IGNORE INTO edge_kinds (id,name) VALUES (?1,?2)"
	case seedAgentKinds:
		return "INSERT OR IGNORE INTO agent_kinds (id,name) VALUES (?1,?2)"
	case seedProviders:
		return "INSERT OR IGNORE INTO providers (id,name) VALUES (?1,?2)"
	case seedRoles:
		return "INSERT OR IGNORE INTO roles (id,name) VALUES (?1,?2)"
	case seedPhases:
		return "INSERT OR IGNORE INTO phases (id,name) VALUES (?1,?2)"
	case seedStages:
		return "INSERT OR IGNORE INTO stages (id,name) VALUES (?1,?2)"
	default:
		panic("unknown reference seed kind")
	}
}

// seedMLModels inserts model entries into the ml_models table.
// Uses INSERT OR IGNORE so existing rows are preserved on re-open.
// Each model is inserted with parameterized queries to prevent SQL injection.
func (scope *connScope) seedMLModels(models []ptypes.ModelEntry) error {
	var existing int
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM ml_models", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			existing = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("seedMLModels: count existing models: %w", err)
	}
	if existing >= len(models) {
		return nil
	}

	var err error
	endTx := sqlitex.Save(scope.conn)
	defer endTx(&err)
	for _, m := range models {
		if err = sqlitex.Execute(scope.conn, "INSERT OR IGNORE INTO ml_models (provider_id, name) VALUES ((SELECT id FROM providers WHERE name = ?1), ?2)", &sqlitex.ExecOptions{Args: []any{string(m.Provider), string(m.Name)}}); err != nil {
			return fmt.Errorf("seedMLModels: inserting model (%s, %q): %w",
				m.Provider.String(), m.Name, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scan helpers (shared by multiple CRUD files)
// ---------------------------------------------------------------------------

// ScanTask converts a SQL result row into a ptypes.Task.
// The stmt must select:
//
//	id, namespace, title, description, status_id, priority_id, type_id,
//	phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
//
// (14 columns, indexed 0-13).
func ScanTask(stmt *zs.Stmt) (ptypes.Task, error) {
	idStr := stmt.ColumnText(0)
	id, err := ptypes.ParseTaskID(idStr)
	if err != nil {
		return ptypes.Task{}, fmt.Errorf("scanTask: invalid task ID %q: %w", idStr, err)
	}

	var ownerID *ptypes.AgentID
	if !stmt.ColumnIsNull(8) {
		aid, err := ptypes.ParseAgentID(stmt.ColumnText(8))
		if err != nil {
			return ptypes.Task{}, fmt.Errorf("scanTask: invalid owner_id %q: %w", stmt.ColumnText(8), err)
		}
		ownerID = &aid
	}

	createdAt := time.Unix(0, stmt.ColumnInt64(10)).UTC()
	updatedAt := time.Unix(0, stmt.ColumnInt64(11)).UTC()

	var closedAt *time.Time
	if !stmt.ColumnIsNull(12) {
		ct := time.Unix(0, stmt.ColumnInt64(12)).UTC()
		closedAt = &ct
	}

	return ptypes.Task{
		ID:          id,
		Title:       stmt.ColumnText(2),
		Description: stmt.ColumnText(3),
		Status:      ptypes.Status(stmt.ColumnInt(4)),
		Priority:    ptypes.Priority(stmt.ColumnInt(5)),
		Type:        ptypes.TaskType(stmt.ColumnInt(6)),
		Phase:       ptypes.Phase(stmt.ColumnInt(7)),
		Owner:       ownerID,
		Notes:       stmt.ColumnText(9),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ClosedAt:    closedAt,
		CloseReason: stmt.ColumnText(13),
	}, nil
}

// ScanActivity converts a SQL result row into a ptypes.Activity.
// The stmt must select:
//
//	id, agent_id, phase_id, stage_id, started_at, ended_at, notes
//
// (7 columns, indexed 0-6).
func ScanActivity(stmt *zs.Stmt) (ptypes.Activity, error) {
	idStr := stmt.ColumnText(0)
	id, err := ptypes.ParseActivityID(idStr)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("scanActivity: invalid activity ID %q: %w", idStr, err)
	}

	agentIDStr := stmt.ColumnText(1)
	agentID, err := ptypes.ParseAgentID(agentIDStr)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("scanActivity: invalid agent_id %q: %w", agentIDStr, err)
	}

	startedAt := time.Unix(0, stmt.ColumnInt64(4)).UTC()
	var endedAt *time.Time
	if !stmt.ColumnIsNull(5) {
		et := time.Unix(0, stmt.ColumnInt64(5)).UTC()
		endedAt = &et
	}

	return ptypes.Activity{
		ID:        id,
		AgentID:   agentID,
		Phase:     ptypes.Phase(stmt.ColumnInt(2)),
		Stage:     ptypes.Stage(stmt.ColumnInt(3)),
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Notes:     stmt.ColumnText(6),
	}, nil
}

// ScanComment converts a SQL result row into a ptypes.Comment.
// The stmt must select:
//
//	id, task_id, author_id, body, created_at
//
// (5 columns, indexed 0-4).
func ScanComment(stmt *zs.Stmt) (ptypes.Comment, error) {
	idStr := stmt.ColumnText(0)
	id, err := ptypes.ParseCommentID(idStr)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("scanComment: invalid comment ID %q: %w", idStr, err)
	}
	taskIDStr := stmt.ColumnText(1)
	taskID, err := ptypes.ParseTaskID(taskIDStr)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("scanComment: invalid task_id %q: %w", taskIDStr, err)
	}
	authorIDStr := stmt.ColumnText(2)
	authorID, err := ptypes.ParseAgentID(authorIDStr)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("scanComment: invalid author_id %q: %w", authorIDStr, err)
	}
	return ptypes.Comment{
		ID:        id,
		TaskID:    taskID,
		AuthorID:  authorID,
		Body:      stmt.ColumnText(3),
		CreatedAt: time.Unix(0, stmt.ColumnInt64(4)).UTC(),
	}, nil
}

// TimeToNullInt converts *time.Time to a nullable int64 value for SQLite.
// Returns nil if t is nil, otherwise returns t.UnixNano().
func TimeToNullInt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

// ---------------------------------------------------------------------------
// Startup preflight helpers (called before pool creation)
// ---------------------------------------------------------------------------

// preflightExistingReadOnly validates an existing SQLite file using a separate
// read-only connection opened outside the pool. This runs before the activation
// connection is opened so that a corrupt or incompatible database is detected
// before any write-capable connection is established.
func preflightExistingReadOnly(dbPath string, models []ptypes.ModelEntry) (bool, error) {
	u := url.URL{Scheme: "file", Path: dbPath}
	if _, err := os.Stat(dbPath + "-wal"); os.IsNotExist(err) {
		query := u.Query()
		query.Set("immutable", "1")
		u.RawQuery = query.Encode()
	} else if err != nil {
		return false, fmt.Errorf("inspect WAL sidecar before read-only preflight: %w", err)
	}
	conn, err := zs.OpenConn(u.String(), zs.OpenReadOnly|zs.OpenURI)
	if err != nil {
		return false, err
	}
	preflight := borrowConnScope(conn, projectionTargetLive)
	defer conn.Close()
	existing, err := preflight.tableExists("journal")
	if err != nil {
		return existing, err
	}
	if existing {
		if _, err := preflight.classifyFactContextSchema(); err != nil {
			return true, err
		}
		if err := preflight.preflightCanonicalColumnsReadOnly(); err != nil {
			return true, err
		}
		if err := preflight.verifyIntegrity(); err != nil {
			return true, err
		}
		if _, err := preflight.replayProjections(); err != nil {
			return true, err
		}
	}
	if err := preflightActivationClone(conn, models); err != nil {
		return existing, err
	}
	return existing, nil
}

func preflightActivationClone(source *zs.Conn, models []ptypes.ModelEntry) error {
	clone, err := zs.OpenConn(":memory:", zs.OpenReadWrite|zs.OpenCreate|zs.OpenURI)
	if err != nil {
		return fmt.Errorf("open isolated activation clone: %w", err)
	}
	defer clone.Close()
	backup, err := zs.NewBackup(clone, "main", source, "main")
	if err != nil {
		return fmt.Errorf("start read-only activation clone: %w", err)
	}
	if _, err = backup.Step(-1); err != nil {
		_ = backup.Close()
		return fmt.Errorf("copy read-only activation clone: %w", err)
	}
	if err = backup.Close(); err != nil {
		return fmt.Errorf("finish read-only activation clone: %w", err)
	}
	activation := borrowConnScope(clone, projectionTargetLive)
	if err = activation.applyActivationPragmas(); err != nil {
		return err
	}
	var activationErr error
	end := sqlitex.Save(clone)
	if activationErr = activation.ensureSchema(models); activationErr == nil {
		activationErr = activation.verifyIntegrity()
	}
	if activationErr == nil {
		_, activationErr = activation.replayProjections()
	}
	end(&activationErr)
	if activationErr != nil {
		return fmt.Errorf("isolated activation clone rejected existing database: %w", activationErr)
	}
	return nil
}
