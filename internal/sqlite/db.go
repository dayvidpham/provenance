// Package sqlite provides the SQLite persistence layer for the Provenance task
// dependency tracker.
//
// A DB owns a database/sql pool backed by modernc.org/sqlite. Operations that
// need connection-local state (TEMP tables, PRAGMAs, or an explicit SQLite write
// transaction) hold a connScope, which pins one *sql.Conn until it is released.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	moderncsqlite "modernc.org/sqlite"
)

// runtimePoolSize is the bounded file-backed database/sql pool size. SQLite
// permits many readers but only one writer; four connections cover the normal
// Apply-plus-reader workload without creating an unbounded connection fan-out.
const runtimePoolSize = 4

// memoryPoolSize keeps every :memory: open serialized. A unique shared-cache
// URI allows activation and later scopes to see the same database while this
// one pooled connection keeps the memory database alive.
const memoryPoolSize = 1

const (
	sqliteDriverName = "sqlite"
	busyTimeoutMS    = 5000
)

// closeResult runs one close operation and publishes its result to every
// concurrent and later caller.
type closeResult struct {
	once sync.Once
	err  error
}

func (result *closeResult) do(closeFunc func() error) error {
	result.once.Do(func() { result.err = closeFunc() })
	return result.err
}

// DB is the pooled SQLite database handle and persistence owner. It owns the
// database/sql lifecycle only; each operation explicitly leases a connScope.
type DB struct {
	db           *sql.DB
	ownsPool     bool
	foreignKeys  foreignKeyDiscipline
	lifecycleMu  sync.Mutex
	closed       bool
	activeScopes sync.WaitGroup
	scopeCancels map[*connScope]context.CancelFunc
	close        closeResult
	// factHooks are per-instance test seams; production leaves them nil. See
	// factQueryTestHooks in facts.go.
	factHooks factQueryTestHooks
}

// foreignKeyDiscipline is the closed selector for how a lease establishes
// PRAGMA foreign_keys, which is connection-local in SQLite and therefore a
// per-lease concern rather than a database-wide setting.
type foreignKeyDiscipline uint8

const (
	// foreignKeysPoolOwned is the zero value and applies to a pool this package
	// opened. The pool DSN already carries foreign_keys(1), so every connection
	// starts enforcing; a lease re-arms it anyway (see armForeignKeys) and never
	// restores anything on release, because ON is this pool's invariant.
	foreignKeysPoolOwned foreignKeyDiscipline = iota
	// foreignKeysPoolBorrowed applies to a caller-owned pool passed to
	// OpenBorrowed. Provenance needs enforcement for the duration of its own
	// lease, but the caller retains pool-configuration ownership, so the prior
	// connection-local value is captured and restored on release.
	foreignKeysPoolBorrowed
)

// pragmaControlTimeout bounds the out-of-band PRAGMA statements that capture and
// restore a borrowed connection's state. They are not the caller's operation, so
// they never inherit the caller's (possibly already expired) context.
const pragmaControlTimeout = time.Second

// sqlQueryer is the deliberately small package-internal SQL contract shared by
// the reducer and query implementations. It is satisfied by both *sql.Conn and
// *sql.Tx. Keeping this to the three standard database/sql methods avoids a
// second storage abstraction while allowing a caller to remain on a pinned
// connection when TEMP tables or PRAGMAs require connection affinity.
type sqlQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// sqlRowScanner permits shared row decoders to accept *sql.Row and *sql.Rows.
type sqlRowScanner interface {
	Scan(...any) error
}

var (
	_ sqlQueryer = (*sql.Conn)(nil)
	_ sqlQueryer = (*sql.Tx)(nil)
)

// projectionTarget is a closed selector for complete static projection SQL
// variants. Identifiers are never caller data.
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

// connScope is the package's connection-ownership contract. conn is pinned for
// the scope's lifetime; release returns it to database/sql exactly once.
type connScope struct {
	conn             *sql.Conn
	ctx              context.Context
	projectionTarget projectionTarget
	releaseOnce      sync.Once
	releaseFunc      func()
	cancelOnce       sync.Once
	cancelFunc       context.CancelFunc
	// restoreForeignKeysOff records that this lease turned PRAGMA foreign_keys ON
	// over a borrowed connection that arrived with it OFF, so release owes the
	// caller the OFF value back. It is written at bind and read at release, both
	// on the scope's owning goroutine.
	restoreForeignKeysOff bool
	// discarded marks a connection already poisoned for the pool. Nothing further
	// is executed on it.
	discarded bool
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

// discard marks a leased connection unusable before releasing its scope. This
// is reserved for transaction-cleanup paths where returning the underlying
// SQLite connection to the pool could expose unknown transaction/PRAGMA state.
func (scope *connScope) discard() {
	if scope == nil || scope.conn == nil {
		return
	}
	scope.markBad()
	scope.release()
}

// markBad poisons the underlying driver connection so database/sql retires it
// instead of returning it to the pool. This is the fail-closed action for any
// path that cannot prove the connection's transaction or PRAGMA state.
func (scope *connScope) markBad() {
	if scope == nil || scope.conn == nil {
		return
	}
	scope.discarded = true
	_ = scope.conn.Raw(func(any) error { return driver.ErrBadConn })
}

func (scope *connScope) cancel() {
	if scope == nil || scope.cancelFunc == nil {
		return
	}
	scope.cancelOnce.Do(scope.cancelFunc)
}

// bindScope is the sole runtime connection-ownership entry point. Every caller
// that needs TEMP state, a connection-local PRAGMA, or an explicit transaction
// uses the returned pinned connection until release.
func (db *DB) bindScope(ctx context.Context, target projectionTarget) (*connScope, error) {
	// A scope gets its own cancellation boundary so Close can stop only scopes
	// admitted through this DB, including scopes whose callers use Background.
	scopeCtx, cancel := context.WithCancel(ctx)
	conn, err := db.db.Conn(scopeCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf(
			"sqlite: lease database/sql connection: %w; caller cannot continue; "+
				"fix: release outstanding scopes, retry with a live context, or reopen the database after Close",
			err)
	}
	scope := &connScope{
		conn:             conn,
		ctx:              scopeCtx,
		projectionTarget: target,
		cancelFunc:       cancel,
	}

	// Admission and Close share lifecycleMu. A scope that wins this lock is
	// registered before Close snapshots cancellation callbacks; a scope that
	// loses returns its connection without touching the caller-owned pool.
	db.lifecycleMu.Lock()
	if db.closed {
		db.lifecycleMu.Unlock()
		scope.cancel()
		_ = conn.Close()
		return nil, closedScopeError()
	}
	if db.scopeCancels == nil {
		db.scopeCancels = make(map[*connScope]context.CancelFunc)
	}
	db.scopeCancels[scope] = cancel
	db.activeScopes.Add(1)
	db.lifecycleMu.Unlock()

	scope.releaseFunc = func() {
		db.releaseScope(scope)
	}
	if err := scope.armForeignKeys(db.foreignKeys); err != nil {
		scope.release()
		return nil, err
	}
	return scope, nil
}

// releaseScope removes a registered scope before invoking cancellation so an
// AfterFunc or another callback that re-enters DB lifecycle code cannot deadlock
// on lifecycleMu. It returns the connection before decrementing the drain count,
// so Close cannot close an owned pool while a released connection is in flight.
func (db *DB) releaseScope(scope *connScope) {
	db.lifecycleMu.Lock()
	_, registered := db.scopeCancels[scope]
	if registered {
		delete(db.scopeCancels, scope)
	}
	db.lifecycleMu.Unlock()
	if !registered {
		return
	}
	scope.restoreBorrowedPragmas()
	scope.cancel()
	_ = scope.conn.Close()
	db.activeScopes.Done()
}

func closedScopeError() error {
	return errors.New(
		"sqlite: lease database/sql connection: sql: database is closed; caller cannot continue; " +
			"fix: release outstanding scopes, retry with a live context, or reopen the database after Close")
}

// borrowConnScope binds a connection whose lifetime belongs to activation or
// preflight. Releasing the returned scope is intentionally a no-op.
func borrowConnScope(conn *sql.Conn, target projectionTarget) *connScope {
	return &connScope{
		conn:             conn,
		ctx:              context.Background(),
		projectionTarget: target,
	}
}

// Close invalidates this DB instance exactly once. It first rejects future
// leases, then waits for already-pinned scopes to release before closing an
// owned pool. A borrowed DB only invalidates this local instance; its caller
// retains ownership of the supplied *sql.DB.
func (db *DB) Close() error {
	return db.close.do(func() error {
		db.lifecycleMu.Lock()
		db.closed = true
		cancels := make([]*connScope, 0, len(db.scopeCancels))
		for scope := range db.scopeCancels {
			cancels = append(cancels, scope)
		}
		db.lifecycleMu.Unlock()
		for _, scope := range cancels {
			scope.cancel()
		}
		db.activeScopes.Wait()
		if !db.ownsPool {
			return nil
		}
		if err := db.db.Close(); err != nil {
			return fmt.Errorf("sqlite.DB.Close: close database/sql pool: %w", err)
		}
		return nil
	})
}

// Open opens or creates dbPath, validates an existing journal before opening a
// write-capable activation connection, applies the schema idempotently, and
// returns a ready runtime pool. :memory: receives a process-unique shared-cache
// name so parallel opens remain isolated.
func Open(dbPath string, models []ptypes.ModelEntry) (*DB, error) {
	target, err := resolveOpenTarget(dbPath)
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open: resolve %q: %w", dbPath, err)
	}

	existingJournal := false
	if !target.isMemory {
		existed, err := existingDatabaseFile(target.path)
		if err != nil {
			return nil, fmt.Errorf("sqlite.Open: inspect path %q before read-only preflight: %w", target.path, err)
		}
		if existed {
			existingJournal, err = preflightExistingReadOnly(target, models)
			if err != nil {
				return nil, fmt.Errorf("sqlite.Open: read-only startup preflight failed on %q: %w", target.display, err)
			}
		}
	}

	if target.isMemory {
		return openInMemory(target, models)
	}
	return openFileBacked(target, existingJournal, models)
}

// OpenBorrowed activates Provenance's schema on a caller-owned Modernc
// database/sql pool and returns a store that uses that exact pool. The caller
// retains all pool lifecycle and configuration ownership; DB.Close invalidates
// only this store instance and never closes the caller's pool. A file-backed
// caller remains required by the public borrowed-store API, but this package has
// no path bridge or second connection.
func OpenBorrowed(runtime *sql.DB, models []ptypes.ModelEntry) (*DB, error) {
	if runtime == nil {
		return nil, errors.New("borrowed database/sql pool is nil")
	}
	if err := runtime.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping borrowed database/sql pool: %w", err)
	}
	activationConn, err := runtime.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("lease borrowed activation connection: %w", err)
	}
	activation := borrowConnScope(activationConn, projectionTargetLive)
	// Activation rewrites busy_timeout and foreign_keys on this connection, which
	// belongs to the caller's pool. Snapshot both before the first attempt so the
	// connection goes back exactly as it arrived.
	restorePragmas, err := captureBorrowedActivationPragmas(activation)
	if err != nil {
		activation.markBad()
		_ = activationConn.Close()
		return nil, fmt.Errorf("activate borrowed database/schema: %w", err)
	}
	// The borrowed pool's file is shared by definition, so activation waits out a
	// concurrent activation or migration within the bounded budget instead of
	// failing this open on the first contended attempt.
	err = activateSchemaWithRetry(activation, models, defaultActivationRetryPolicy())
	restoreErr := restorePragmas()
	closeErr := activationConn.Close()
	if err != nil {
		if restoreErr != nil {
			return nil, fmt.Errorf("activate borrowed database/schema: %w", errors.Join(err, restoreErr))
		}
		return nil, fmt.Errorf("activate borrowed database/schema: %w", err)
	}
	if restoreErr != nil {
		return nil, restoreErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("return borrowed activation connection: %w", closeErr)
	}
	return &DB{db: runtime, foreignKeys: foreignKeysPoolBorrowed}, nil
}

type openTarget struct {
	display       string
	path          string
	runtimeDSN    string
	activationDSN string
	readOnlyDSN   string
	isMemory      bool
}

// memoryDBCounter creates independent shared-cache names for :memory: opens.
var memoryDBCounter atomic.Uint64

func resolveOpenTarget(dbPath string) (openTarget, error) {
	if dbPath == "" {
		return openTarget{}, errors.New("database path is empty; supply a filesystem path, file: URI, or :memory:")
	}

	baseDSN, path, isMemory, err := normalizeSQLiteTarget(dbPath)
	if err != nil {
		return openTarget{}, err
	}
	writeMode := map[string]string(nil)
	if !isMemory {
		// Open has always been a read-write-create lifecycle API. Preserve that
		// contract even when a caller supplies a file: URI with a stale mode=ro.
		writeMode = map[string]string{"mode": "rwc"}
	}
	activationDSN, err := withSQLiteQuery(baseDSN, writeMode, []string{fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)})
	if err != nil {
		return openTarget{}, err
	}
	runtimePragmas := []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS),
		"foreign_keys(1)",
		"synchronous(NORMAL)",
	}
	if !isMemory {
		runtimePragmas = append(runtimePragmas, "journal_mode(WAL)")
	}
	runtimeDSN, err := withSQLiteQuery(baseDSN, writeMode, runtimePragmas)
	if err != nil {
		return openTarget{}, err
	}
	readOnlyDSN, err := withSQLiteQuery(baseDSN, map[string]string{"mode": "ro"}, nil)
	if err != nil {
		return openTarget{}, err
	}
	return openTarget{
		display:       dbPath,
		path:          path,
		runtimeDSN:    runtimeDSN,
		activationDSN: activationDSN,
		readOnlyDSN:   readOnlyDSN,
		isMemory:      isMemory,
	}, nil
}

// resolvePoolTarget remains the narrow testable target-selection seam. It
// returns the fully configured runtime DSN and the bounded connection count.
func resolvePoolTarget(dbPath string) (uri string, poolSize int, isMemory bool) {
	target, err := resolveOpenTarget(dbPath)
	if err != nil {
		return "", 0, false
	}
	if target.isMemory {
		return target.runtimeDSN, memoryPoolSize, true
	}
	return target.runtimeDSN, runtimePoolSize, false
}

func normalizeSQLiteTarget(dbPath string) (dsn, path string, isMemory bool, _ error) {
	if dbPath == ":memory:" {
		n := memoryDBCounter.Add(1)
		return fmt.Sprintf("file:provenance-memdb-%d?mode=memory&cache=shared", n), "", true, nil
	}
	if strings.HasPrefix(dbPath, "file:") {
		values, err := sqliteQueryValues(dbPath)
		if err != nil {
			return "", "", false, err
		}
		if values.Get("mode") == "memory" || strings.HasPrefix(strings.TrimPrefix(dbPath, "file:"), ":memory:") {
			return dbPath, "", true, nil
		}
		path, err := fileURIPath(dbPath)
		if err != nil {
			return "", "", false, err
		}
		return dbPath, path, false, nil
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	return u.String(), dbPath, false, nil
}

func sqliteQueryValues(dsn string) (url.Values, error) {
	_, rawQuery, found := strings.Cut(dsn, "?")
	if !found {
		return make(url.Values), nil
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, fmt.Errorf("parse SQLite URI query: %w", err)
	}
	return values, nil
}

// withSQLiteQuery preserves all caller-provided URI fields while replacing only
// explicit keys such as mode and appending connection initialization PRAGMAs.
func withSQLiteQuery(dsn string, replace map[string]string, pragmas []string) (string, error) {
	base, _, _ := strings.Cut(dsn, "?")
	values, err := sqliteQueryValues(dsn)
	if err != nil {
		return "", err
	}
	for key, value := range replace {
		values.Del(key)
		if value != "" {
			values.Set(key, value)
		}
	}
	for _, pragma := range pragmas {
		values.Add("_pragma", pragma)
	}
	encoded := values.Encode()
	if encoded == "" {
		return base, nil
	}
	return base + "?" + encoded, nil
}

func fileURIPath(dsn string) (string, error) {
	raw, _, _ := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
	if strings.HasPrefix(raw, "//") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse file URI: %w", err)
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", fmt.Errorf("file URI host %q is unsupported for local preflight; use a local file: URI or path", u.Host)
		}
		if u.Path == "" {
			return "", errors.New("file URI contains no filesystem path")
		}
		return u.Path, nil
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("decode file URI path: %w", err)
	}
	if path == "" {
		return "", errors.New("file URI contains no filesystem path")
	}
	return path, nil
}

func existingDatabaseFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return false, fmt.Errorf("database path is a directory")
		}
		return info.Size() > 0, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func openConfiguredSQLDB(dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func openInMemory(target openTarget, models []ptypes.ModelEntry) (*DB, error) {
	runtime, err := openConfiguredSQLDB(target.runtimeDSN, memoryPoolSize)
	if err != nil {
		return nil, fmt.Errorf("open in-memory runtime pool %q: %w", target.display, err)
	}
	activationConn, err := runtime.Conn(context.Background())
	if err != nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("lease in-memory activation connection %q: %w", target.display, err)
	}
	activation := borrowConnScope(activationConn, projectionTargetLive)
	// A process-unique memory database has no other writer, so there is nothing to
	// wait for beyond the single attempt's own busy timeout.
	if err := activateSchema(activation, models, defaultActivationRetryPolicy()); err != nil {
		_ = activationConn.Close()
		_ = runtime.Close()
		return nil, fmt.Errorf("activate in-memory database %q: %w", target.display, err)
	}
	if err := activationConn.Close(); err != nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("return in-memory activation connection %q: %w", target.display, err)
	}
	return &DB{db: runtime, ownsPool: true}, nil
}

func openFileBacked(target openTarget, existingJournal bool, models []ptypes.ModelEntry) (*DB, error) {
	activationDB, err := openConfiguredSQLDB(target.activationDSN, 1)
	if err != nil {
		return nil, fmt.Errorf("open activation connection for %q: %w", target.display, err)
	}
	activationConn, err := activationDB.Conn(context.Background())
	if err != nil {
		_ = activationDB.Close()
		return nil, fmt.Errorf("lease activation connection for %q: %w", target.display, err)
	}
	activation := borrowConnScope(activationConn, projectionTargetLive)

	actualExisting, err := activation.tableExists("journal")
	if err == nil && actualExisting != existingJournal {
		err = fmt.Errorf("schema changed between read-only preflight (journal=%t) and activation (journal=%t); retry after concurrent schema work finishes", existingJournal, actualExisting)
	}
	if err == nil {
		// A file-backed open contends with any other process holding this path's
		// write lock, so it shares the borrowed path's bounded activation budget.
		err = activateSchemaWithRetry(activation, models, defaultActivationRetryPolicy())
	}
	if err == nil {
		// Pre-existing, deliberately left as is: these two pragmas run after the
		// activation budget, so two processes creating the SAME new file can still
		// race the journal-mode switch and surface SQLite's own non-actionable
		// "delete, want wal" mismatch. Folding them into the budget would mean
		// retrying the entire schema activation (O(journal) replay included) to
		// redo one pragma, and widening the retry surface past the single
		// sanctioned exception; the right fix is a narrower journal-mode
		// reconciliation, which is out of scope for this contention fix.
		err = activation.enableWAL()
	}
	if err == nil {
		err = activation.setSynchronousNormal()
	}
	closeErr := activationConn.Close()
	poolCloseErr := activationDB.Close()
	if err != nil {
		return nil, fmt.Errorf("activate file database %q: %w", target.display, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close activation connection for %q: %w", target.display, closeErr)
	}
	if poolCloseErr != nil {
		return nil, fmt.Errorf("close activation pool for %q: %w", target.display, poolCloseErr)
	}

	runtime, err := openConfiguredSQLDB(target.runtimeDSN, runtimePoolSize)
	if err != nil {
		return nil, fmt.Errorf("open runtime pool for %q: %w", target.display, err)
	}
	return &DB{db: runtime, ownsPool: true}, nil
}

// activationRetryPolicy bounds a contended schema activation. attemptBusyTimeout
// is the connection-local SQLite busy wait each attempt gets (SQLite's own
// contention wait, honoured because activation now begins with BEGIN IMMEDIATE);
// ceiling bounds the total wall time spent across attempts, and the delays shape
// the backoff between them. Values are injectable so contention tests can pin the
// ceiling behaviour without waiting the production budget.
type activationRetryPolicy struct {
	attemptBusyTimeout time.Duration
	ceiling            time.Duration
	initialDelay       time.Duration
	maxDelay           time.Duration
}

// defaultActivationRetryPolicy is the production activation budget: SQLite's
// standard busy_timeout per attempt, retried with backoff for up to 30 seconds so
// a concurrent migrator holding the shared file's write lock across several
// busy_timeout windows delays this open instead of failing it.
func defaultActivationRetryPolicy() activationRetryPolicy {
	return activationRetryPolicy{
		attemptBusyTimeout: busyTimeoutMS * time.Millisecond,
		ceiling:            30 * time.Second,
		initialDelay:       10 * time.Millisecond,
		maxDelay:           time.Second,
	}
}

// activateSchemaWithRetry runs activation against a database file that other
// processes or pools may be writing. A single attempt already waits out ordinary
// contention inside SQLite (attemptBusyTimeout); the loop exists only for the
// case where a concurrent activation or migration holds the write lock longer
// than one busy window. Retrying is safe because a failed attempt commits
// nothing: runScopedTransaction rolls back on every error path and the pragmas
// are re-applied at the head of each attempt.
//
// This is not a general storage retry framework, and it is not a substitute for
// busy_timeout: it is the bounded outer wait for exactly one operation, whose
// per-attempt wait is still SQLite's. See TESTING.md, "Waiting and retries":
// activation is the single sanctioned exception, and it does not extend to
// storage operations, which still make one inner attempt.
//
// The budget is uncancellable in practice today: both retrying call sites reach
// this through borrowConnScope, whose ctx is context.Background, because Open and
// OpenBorrowed take no context. The cancellation arm below is kept so that a
// future context-carrying caller is honoured the moment one exists, rather than
// silently waiting out the whole budget.
func activateSchemaWithRetry(scope *connScope, models []ptypes.ModelEntry, policy activationRetryPolicy) error {
	started := time.Now()
	deadline := started.Add(policy.ceiling)
	// A non-positive delay would turn the bounded wait into a spin, so the backoff
	// always starts at a real duration.
	delay := max(policy.initialDelay, time.Millisecond)
	for attempt := 1; ; attempt++ {
		err := activateSchema(scope, models, policy)
		if err == nil {
			return nil
		}
		if !isBusyError(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return activationContendedError(scope, policy, attempt, time.Since(started), err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-scope.ctx.Done():
			timer.Stop()
			cancelTarget, _ := activationTargetDisplay(scope)
			return fmt.Errorf(
				"activate SQLite schema on %s: canceled after %d contended attempt(s) while waiting for the file's write lock (%w); "+
					"nothing was written — every attempt rolled back; retry the open once the caller's context is live again",
				cancelTarget, attempt, errors.Join(scope.ctx.Err(), err),
			)
		case <-timer.C:
		}
		delay *= 2
		if policy.maxDelay > 0 && delay > policy.maxDelay {
			delay = policy.maxDelay
		}
	}
}

// activationContendedError reports an exhausted activation budget in the terms
// the operator needs: which file, how long was spent, what is holding it, that
// nothing was written, and how to clear it.
func activationContendedError(scope *connScope, policy activationRetryPolicy, attempts int, elapsed time.Duration, last error) error {
	target, resolved := activationTargetDisplay(scope)
	// The budget bounds when the last attempt may START, so the measured elapsed
	// time legitimately exceeds it by up to one attempt plus one backoff delay.
	// Report what was actually spent and name the budget separately rather than
	// implying the two are the same number.
	identify := ""
	if resolved {
		identify = fmt.Sprintf(", or identify the holder with `fuser %s` (or `lsof %s`) and stop it before retrying", target, target)
	}
	return fmt.Errorf(
		"activate SQLite schema on %s: gave up after %s; budget %s; %d attempt(s) with the database still locked (%w) — "+
			"where: internal/sqlite.activateSchemaWithRetry, startup schema activation; "+
			"why: another process or pool held this file's write lock for the whole budget, most likely a concurrent migrator "+
			"activating the same database; "+
			"impact: this open failed and no schema or seed row was written, because every attempt rolled back; "+
			"fix: wait for the other writer to finish and retry the open%s",
		target, elapsed.Round(time.Millisecond), policy.ceiling, attempts, last, identify,
	)
}

// activationTargetDisplay names the contended file for operator-facing errors.
// It asks SQLite itself so a borrowed pool, whose path this package never sees,
// still reports a real path. It runs only on a failure path.
func activationTargetDisplay(scope *connScope) (display string, resolved bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var file string
	if err := scope.conn.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil || file == "" {
		return "the activating SQLite database", false
	}
	return file, true
}

func activateSchema(scope *connScope, models []ptypes.ModelEntry, policy activationRetryPolicy) error {
	if err := scope.applyActivationPragmas(policy.attemptBusyTimeout); err != nil {
		return err
	}
	// BEGIN IMMEDIATE, not BEGIN. Activation reads before it writes (~30 CREATE
	// TABLE IF NOT EXISTS statements precede the reference-data seeding), and a
	// deferred transaction would take the read lock first and then need a
	// read-to-write promotion, on which SQLite never invokes the busy handler:
	// contention would fail instantly and bypass busy_timeout entirely.
	//
	// Tradeoff, accepted deliberately: taking the write lock at BEGIN holds it
	// for the read-only integrity and replay probes too, lengthening the lock
	// hold. Those probes are not hoisted out of the transaction because
	// activation's contract is that the schema, its verification, and the replay
	// commit or roll back together; verifying outside the write lock would
	// validate a snapshot that another writer could invalidate before COMMIT.
	err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if err := scope.ensureSchema(models); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := scope.verifyIntegrity(); err != nil {
			return fmt.Errorf("whole-journal integrity: %w", err)
		}
		if _, err := scope.replayProjections(); err != nil {
			return fmt.Errorf("journal replay: %w", err)
		}
		return nil
	})
	restoreErr := scope.enableForeignKeys()
	if err != nil {
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore foreign key enforcement: %w", restoreErr))
		}
		return err
	}
	return restoreErr
}

// runTransaction executes one explicit SQLite transaction on a pinned
// connection. It is intentionally small: callers retain standard database/sql
// operations and use this only where transaction SQL must share conn-local state.
// limitTransactionBusyTimeout bounds SQLite's synchronous busy handler by the
// caller's deadline. modernc can report a canceled ExecContext while SQLite is
// still waiting under a longer busy_timeout; capping the connection-local value
// lets BEGIN finish with a definite SQLite error before that deadline and avoids
// leaving an invisible pending write transaction on the pinned connection.
func limitTransactionBusyTimeout(ctx context.Context, conn sqlQueryer) (func() error, error) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return func() error { return nil }, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, ctx.Err()
	}
	limitMS := int(remaining.Milliseconds())
	if limitMS < 1 {
		limitMS = 1
	}

	controlCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var previousMS int
	if err := conn.QueryRowContext(controlCtx, "PRAGMA busy_timeout").Scan(&previousMS); err != nil {
		return nil, fmt.Errorf("read SQLite busy timeout before transaction: %w", err)
	}
	if previousMS <= limitMS {
		return func() error { return nil }, nil
	}
	if _, err := conn.ExecContext(controlCtx, fmt.Sprintf("PRAGMA busy_timeout=%d", limitMS)); err != nil {
		return nil, fmt.Errorf("limit SQLite busy timeout to caller deadline: %w", err)
	}
	return func() error {
		restoreCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := conn.ExecContext(restoreCtx, fmt.Sprintf("PRAGMA busy_timeout=%d", previousMS)); err != nil {
			return fmt.Errorf("restore SQLite busy timeout to %dms: %w", previousMS, err)
		}
		return nil
	}, nil
}

func runScopedTransaction(ctx context.Context, conn sqlQueryer, begin string, operation func() error) (err error) {
	restoreBusyTimeout, err := limitTransactionBusyTimeout(ctx, conn)
	if err != nil {
		return err
	}
	defer func() {
		if restoreErr := restoreBusyTimeout(); restoreErr != nil {
			if err == nil {
				err = restoreErr
			} else {
				err = errors.Join(err, restoreErr)
			}
		}
	}()
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		// database/sql may report a context deadline while a driver is completing
		// BEGIN on the pinned connection. Clear that possible transaction with an
		// independent bounded context so a timed-out contender cannot retain a
		// write lock and starve the writer that it was contending with.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = conn.ExecContext(rollbackCtx, "ROLLBACK")
		cancel()
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// The caller's context can be canceled precisely because the transaction
		// needs to abort. Use a fresh bounded context to guarantee cleanup on the
		// connection rather than leaving it write-locked after cancellation.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, rollbackErr := conn.ExecContext(rollbackCtx, "ROLLBACK")
		cancel()
		if rollbackErr != nil {
			if err == nil {
				err = fmt.Errorf("rollback uncommitted SQLite transaction: %w", rollbackErr)
			} else {
				err = errors.Join(err, fmt.Errorf("rollback failed: %w", rollbackErr))
			}
		}
	}()
	if err = operation(); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit SQLite transaction: %w", err)
	}
	committed = true
	return nil
}

// runImmediateTransaction owns the explicit BEGIN IMMEDIATE pattern used where
// read-before-write checks must hold SQLite write ownership on this exact
// connection. It is not a generic storage framework.
func runImmediateTransaction(ctx context.Context, conn *sql.Conn, operation func() error) error {
	return runScopedTransaction(ctx, conn, "BEGIN IMMEDIATE", operation)
}

// Pragmas --------------------------------------------------------------------

// readForeignKeysPragma reports this connection's current connection-local
// foreign-key enforcement state.
func readForeignKeysPragma(ctx context.Context, conn *sql.Conn) (bool, error) {
	var enabled int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		return false, fmt.Errorf(
			"sqlite: read PRAGMA foreign_keys on the leased connection: %w; "+
				"where: internal/sqlite, connection lease; impact: this operation cannot establish or restore "+
				"foreign-key enforcement and is refused rather than run unenforced; "+
				"fix: verify the pool's connections are live and retry", err)
	}
	return enabled != 0, nil
}

// readBusyTimeoutPragma reports this connection's current busy-timeout budget in
// milliseconds.
func readBusyTimeoutPragma(ctx context.Context, conn *sql.Conn) (int64, error) {
	var busyTimeoutMS int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeoutMS); err != nil {
		return 0, fmt.Errorf(
			"sqlite: read PRAGMA busy_timeout on the leased connection: %w; "+
				"where: internal/sqlite, connection lease; impact: this operation cannot prove the connection's "+
				"contention budget, so the connection's configuration cannot be established or restored; "+
				"fix: verify the pool's connections are live and retry", err)
	}
	return busyTimeoutMS, nil
}

// pragmaOnOff renders a boolean pragma value the way SQLite reports it back.
func pragmaOnOff(enabled bool) string {
	if enabled {
		return "ON"
	}
	return "OFF"
}

// setBooleanPragmaVerified writes a connection-local boolean PRAGMA and proves
// the write landed by reading the value back.
//
// The read-back is the whole point. SQLite silently ignores PRAGMA foreign_keys
// while a transaction is open on the connection (see pauseForeignKeys): the Exec
// reports success and changes nothing. A restore that trusts that success hands
// the connection back to its pool carrying the enforcement state this package
// chose rather than the state its owner configured, and nothing ever notices. So
// every write of a pragma this package captures and restores is verified, and an
// unverifiable one is reported to the caller, whose fail-closed action is to
// retire the connection.
func setBooleanPragmaVerified(ctx context.Context, conn *sql.Conn, pragma string, enabled bool) error {
	value := pragmaOnOff(enabled)
	if _, err := conn.ExecContext(ctx, "PRAGMA "+pragma+"="+value); err != nil {
		return fmt.Errorf("set PRAGMA %s=%s: %w", pragma, value, err)
	}
	var readBack int
	if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&readBack); err != nil {
		return fmt.Errorf(
			"read back PRAGMA %s after setting it to %s: %w; where: internal/sqlite, verified pragma write; "+
				"impact: the write cannot be proven, so the connection's state is unknown; fix: verify the "+
				"connection is still live and retry", pragma, value, err)
	}
	if (readBack != 0) != enabled {
		return fmt.Errorf(
			"set PRAGMA %s=%s: the connection still reports %s; why: SQLite ignores this pragma while a "+
				"transaction is open on the connection, so the statement succeeded without taking effect; "+
				"where: internal/sqlite, verified pragma write; impact: the connection's state is not the one "+
				"this package requires, so it must not be handed back to the pool; fix: close every transaction "+
				"on the connection before changing this pragma",
			pragma, value, pragmaOnOff(readBack != 0))
	}
	return nil
}

// setBusyTimeoutVerified writes the connection-local busy timeout and proves the
// write landed, for the same reason setBooleanPragmaVerified does: an
// unverified restore can leave a borrowed connection carrying a contention
// budget its owner never chose.
func setBusyTimeoutVerified(ctx context.Context, conn *sql.Conn, milliseconds int64) error {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", milliseconds)); err != nil {
		return fmt.Errorf("set PRAGMA busy_timeout=%d: %w", milliseconds, err)
	}
	readBack, err := readBusyTimeoutPragma(ctx, conn)
	if err != nil {
		return err
	}
	if readBack != milliseconds {
		return fmt.Errorf(
			"set PRAGMA busy_timeout=%d: the connection still reports %dms; where: internal/sqlite, verified "+
				"pragma write; impact: the connection's contention budget is not the one requested, so it must "+
				"not be handed back to the pool; fix: verify nothing else is rewriting busy_timeout on this "+
				"connection and retry", milliseconds, readBack)
	}
	return nil
}

// armForeignKeys establishes this package's lease invariant — PRAGMA
// foreign_keys=ON for the whole lease — and decides what release owes the pool.
//
// Design decision (borrowed pools), recorded deliberately: the prior value is
// captured and restored rather than demanded pool-wide at OpenBorrowed. A
// verify-at-open alternative cannot actually be enforced: database/sql creates
// connections lazily and exposes no way to enumerate or pin the pool's
// connections, so a check at open would sample exactly one connection and assert
// nothing about the others — false assurance, while still silently rewriting the
// caller's configuration. Capture/restore is checkable on every connection this
// package ever touches, and it keeps OpenBorrowed's documented contract that the
// caller retains pool-configuration ownership: whatever the caller configured
// (including foreign_keys OFF, and therefore whether ON DELETE CASCADE fires for
// the caller's own statements) is what the caller gets back, on every connection
// alike. Callers that need cascade must configure their own pool for it; this
// package never makes that choice on their behalf, and never leaves half the
// pool disagreeing with the other half.
func (scope *connScope) armForeignKeys(discipline foreignKeyDiscipline) error {
	switch discipline {
	case foreignKeysPoolOwned:
		// Self-healing. The owned pool's DSN already carries foreign_keys(1), but a
		// lease that toggled enforcement OFF for a table rebuild and then failed to
		// restore it would otherwise leave that one pooled connection unenforced for
		// the rest of the process, making integrity depend on which connection an
		// operation happened to draw. Re-arming here bounds that drift to a single
		// lease. Nothing is restored on release: ON is this pool's invariant.
		if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON"); err != nil {
			return fmt.Errorf(
				"sqlite: enable foreign-key enforcement on the leased connection: %w; "+
					"where: internal/sqlite, connection lease; impact: the operation is refused rather than run "+
					"with foreign keys unenforced; fix: retry with a live context, or reopen the database", err)
		}
		return nil
	case foreignKeysPoolBorrowed:
		enabled, err := readForeignKeysPragma(scope.ctx, scope.conn)
		if err != nil {
			return err
		}
		if enabled {
			return nil
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON"); err != nil {
			return fmt.Errorf(
				"sqlite: enable foreign-key enforcement on the borrowed connection: %w; "+
					"where: internal/sqlite, borrowed-pool connection lease; impact: the operation is refused "+
					"rather than run with foreign keys unenforced; fix: retry with a live context, or verify the "+
					"borrowed pool is still open", err)
		}
		scope.restoreForeignKeysOff = true
		return nil
	default:
		panic("unknown foreign key discipline")
	}
}

// restoreBorrowedPragmas hands a borrowed connection back to its owner's pool
// with exactly the connection-local PRAGMA state it arrived with. If the restore
// cannot be proven, the connection is retired instead of returned, so the caller
// can never draw a connection this package silently reconfigured.
func (scope *connScope) restoreBorrowedPragmas() {
	if scope == nil || scope.conn == nil || scope.discarded || !scope.restoreForeignKeysOff {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pragmaControlTimeout)
	defer cancel()
	// Verified, not fire-and-forget: if this lease is released while a transaction
	// is open on the connection, the restore statement succeeds and does nothing,
	// and the caller would get a connection back with Provenance's enforcement
	// still on. The read-back is what turns that silent hole into a retirement.
	if err := setBooleanPragmaVerified(ctx, scope.conn, "foreign_keys", false); err != nil {
		scope.markBad()
		return
	}
	scope.restoreForeignKeysOff = false
}

// pauseForeignKeys turns connection-local foreign-key enforcement off for a
// table rebuild or an adversarial fixture that must move rows past their
// constraints, and returns the restore. PRAGMA foreign_keys is a no-op inside a
// transaction, so it brackets the transaction instead of living in it.
//
// The restore takes the operation's own error and returns the error the caller
// should return, so a failed restore is never dropped on an already-failed path.
// A connection whose enforcement cannot be proven restored is retired rather than
// returned to the pool; on top of that, every lease re-arms enforcement at bind
// (see armForeignKeys), so drift cannot outlive one lease either way.
//
// Usage requires a named error return:
//
//	restoreFK, err := scope.pauseForeignKeys("Caller")
//	if err != nil {
//		return err
//	}
//	defer func() { err = restoreFK(err) }()
func (scope *connScope) pauseForeignKeys(what string) (func(error) error, error) {
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return nil, fmt.Errorf(
			"%s: disable foreign-key enforcement on the pinned connection: %w; "+
				"where: internal/sqlite, before the bracketing transaction; impact: nothing ran and nothing "+
				"was written; fix: retry with a live context on an open database", what, err)
	}
	return func(operationErr error) error {
		// The caller's context may already be canceled or expired; restoring this
		// connection's enforcement is not the caller's operation, so it is bounded
		// on its own.
		ctx, cancel := context.WithTimeout(context.Background(), pragmaControlTimeout)
		defer cancel()
		if err := setBooleanPragmaVerified(ctx, scope.conn, "foreign_keys", true); err != nil {
			scope.markBad()
			return errors.Join(operationErr, fmt.Errorf(
				"%s: restore foreign-key enforcement on the pinned connection: %w; "+
					"where: internal/sqlite, after the bracketing transaction; impact: the connection was "+
					"retired instead of returned to the pool, so no later operation runs unenforced; "+
					"fix: verify the database is still open and retry the operation", what, err))
		}
		return operationErr
	}, nil
}

// suppressCheckConstraints turns connection-local CHECK enforcement off for an
// adversarial fixture that must land a row past a structural CHECK so a
// production reducer-level guard is what catches it, and returns the restore.
//
// Same shape and same reasoning as pauseForeignKeys: the restore takes the
// operation's own error and returns the error the caller should return, the
// restore is proven by read-back, and a connection whose enforcement cannot be
// proven restored is retired rather than returned to the pool. A fixture that
// left CHECK enforcement off on a pooled connection would silently disarm the
// schema for every later operation that drew it.
//
// Usage requires a named error return:
//
//	restoreChecks, err := scope.suppressCheckConstraints("Caller")
//	if err != nil {
//		return err
//	}
//	defer func() { err = restoreChecks(err) }()
func (scope *connScope) suppressCheckConstraints(what string) (func(error) error, error) {
	if err := setBooleanPragmaVerified(scope.ctx, scope.conn, "ignore_check_constraints", true); err != nil {
		return nil, fmt.Errorf(
			"%s: disable CHECK enforcement on the pinned connection: %w; "+
				"where: internal/sqlite, before the bracketing transaction; impact: nothing ran and nothing "+
				"was written; fix: retry with a live context on an open database", what, err)
	}
	return func(operationErr error) error {
		// The caller's context may already be canceled or expired; restoring this
		// connection's enforcement is not the caller's operation, so it is bounded
		// on its own.
		ctx, cancel := context.WithTimeout(context.Background(), pragmaControlTimeout)
		defer cancel()
		if err := setBooleanPragmaVerified(ctx, scope.conn, "ignore_check_constraints", false); err != nil {
			scope.markBad()
			return errors.Join(operationErr, fmt.Errorf(
				"%s: restore CHECK enforcement on the pinned connection: %w; "+
					"where: internal/sqlite, after the bracketing transaction; impact: the connection was "+
					"retired instead of returned to the pool, so no later operation runs with CHECK "+
					"constraints disabled; fix: verify the database is still open and retry the operation",
				what, err))
		}
		return operationErr
	}, nil
}

// captureBorrowedActivationPragmas snapshots the two connection-local pragmas
// activation rewrites (busy_timeout and foreign_keys) on a borrowed connection
// and returns the restore for the caller to run before handing the connection
// back. The restore reports its own failure: unlike a per-operation lease, an
// activation failure that also loses the caller's pool configuration is exactly
// the condition the caller must be told about.
func captureBorrowedActivationPragmas(scope *connScope) (func() error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pragmaControlTimeout)
	defer cancel()
	foreignKeys, err := readForeignKeysPragma(ctx, scope.conn)
	if err != nil {
		return nil, err
	}
	busyTimeoutMS, err := readBusyTimeoutPragma(ctx, scope.conn)
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.OpenBorrowed: %w; where: internal/sqlite.OpenBorrowed, schema activation; impact: "+
				"activation is refused because the caller's pool configuration could not be preserved; "+
				"fix: verify the borrowed pool is live and retry OpenBorrowed", err)
	}
	foreignKeysValue := "OFF"
	if foreignKeys {
		foreignKeysValue = "ON"
	}
	return func() error {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), pragmaControlTimeout)
		defer restoreCancel()
		var restoreErr error
		// Both restores are proven by read-back: an activation that left a
		// transaction open on this connection would otherwise make the foreign_keys
		// statement a silent no-op and hand the caller a connection Provenance
		// reconfigured.
		if err := setBusyTimeoutVerified(restoreCtx, scope.conn, busyTimeoutMS); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore borrowed busy_timeout to %dms: %w", busyTimeoutMS, err))
		}
		if err := setBooleanPragmaVerified(restoreCtx, scope.conn, "foreign_keys", foreignKeys); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore borrowed foreign_keys to %s: %w", foreignKeysValue, err))
		}
		if restoreErr == nil {
			return nil
		}
		// Fail closed: the connection is retired rather than returned with a pragma
		// state this package cannot prove.
		scope.markBad()
		return fmt.Errorf(
			"sqlite.OpenBorrowed: %w; where: internal/sqlite.OpenBorrowed, returning the activation connection; "+
				"impact: the borrowed connection was retired instead of returned to your pool, and this open "+
				"failed; fix: verify the borrowed pool is healthy and retry OpenBorrowed", restoreErr)
	}, nil
}

func (scope *connScope) applyActivationPragmas(busyTimeout time.Duration) error {
	busyTimeoutMilliseconds := busyTimeout.Milliseconds()
	if busyTimeoutMilliseconds < 0 {
		busyTimeoutMilliseconds = 0
	}
	if _, err := scope.conn.ExecContext(scope.ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMilliseconds)); err != nil {
		return fmt.Errorf("set activation busy timeout: %w", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable foreign key enforcement for activation: %w", err)
	}
	return nil
}

func (scope *connScope) enableForeignKeys() error {
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign key enforcement after activation: %w", err)
	}
	return nil
}

func (scope *connScope) enableWAL() error {
	var mode string
	if err := scope.conn.QueryRowContext(scope.ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("set WAL journal mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("set WAL journal mode: SQLite returned %q, want wal", mode)
	}
	return nil
}

func (scope *connScope) setSynchronousNormal() error {
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA synchronous=NORMAL"); err != nil {
		return fmt.Errorf("set synchronous=NORMAL: %w", err)
	}
	return nil
}

// Schema DDL -----------------------------------------------------------------

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
	for _, statement := range ddl {
		if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
			return fmt.Errorf("ensureSchema: statement %q: %w", statement, err)
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

// Seed data ------------------------------------------------------------------

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
			if _, err := scope.conn.ExecContext(scope.ctx, seed.kind.query(), id, name); err != nil {
				return fmt.Errorf("seedReferenceData: kind %d id %d: %w", seed.kind, id, err)
			}
		}
	}
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

func (scope *connScope) seedMLModels(models []ptypes.ModelEntry) error {
	var existing int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM ml_models").Scan(&existing); err != nil {
		return fmt.Errorf("seedMLModels: count existing models: %w", err)
	}
	if existing >= len(models) {
		return nil
	}
	for _, model := range models {
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO ml_models (provider_id, name) VALUES ((SELECT id FROM providers WHERE name = ?1), ?2)", string(model.Provider), string(model.Name)); err != nil {
			return fmt.Errorf("seedMLModels: insert (%s, %q): %w", model.Provider.String(), model.Name, err)
		}
	}
	return nil
}

// Scan helpers ---------------------------------------------------------------

// ScanTask converts the documented 14-column task selection into a ptypes.Task.
func ScanTask(row sqlRowScanner) (ptypes.Task, error) {
	var (
		idText, ignoredNamespace, title, description, notes, closeReason string
		status, priority, taskType, phase                                int
		owner                                                            sql.NullString
		createdAt, updatedAt                                             int64
		closedAt                                                         sql.NullInt64
	)
	if err := row.Scan(&idText, &ignoredNamespace, &title, &description, &status, &priority, &taskType, &phase, &owner, &notes, &createdAt, &updatedAt, &closedAt, &closeReason); err != nil {
		return ptypes.Task{}, fmt.Errorf("scan task row: %w", err)
	}
	id, err := ptypes.ParseTaskID(idText)
	if err != nil {
		return ptypes.Task{}, fmt.Errorf("scanTask: invalid task ID %q: %w", idText, err)
	}
	var ownerID *ptypes.AgentID
	if owner.Valid {
		parsed, err := ptypes.ParseAgentID(owner.String)
		if err != nil {
			return ptypes.Task{}, fmt.Errorf("scanTask: invalid owner_id %q: %w", owner.String, err)
		}
		ownerID = &parsed
	}
	var closed *time.Time
	if closedAt.Valid {
		value := time.Unix(0, closedAt.Int64).UTC()
		closed = &value
	}
	return ptypes.Task{
		ID: id, Title: title, Description: description, Status: ptypes.Status(status),
		Priority: ptypes.Priority(priority), Type: ptypes.TaskType(taskType), Phase: ptypes.Phase(phase),
		Owner: ownerID, Notes: notes, CreatedAt: time.Unix(0, createdAt).UTC(),
		UpdatedAt: time.Unix(0, updatedAt).UTC(), ClosedAt: closed, CloseReason: closeReason,
	}, nil
}

func ScanActivity(row sqlRowScanner) (ptypes.Activity, error) {
	var idText, agentText, notes string
	var phase, stage int
	var startedAt int64
	var endedAt sql.NullInt64
	if err := row.Scan(&idText, &agentText, &phase, &stage, &startedAt, &endedAt, &notes); err != nil {
		return ptypes.Activity{}, fmt.Errorf("scan activity row: %w", err)
	}
	id, err := ptypes.ParseActivityID(idText)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("scanActivity: invalid activity ID %q: %w", idText, err)
	}
	agentID, err := ptypes.ParseAgentID(agentText)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("scanActivity: invalid agent_id %q: %w", agentText, err)
	}
	var ended *time.Time
	if endedAt.Valid {
		value := time.Unix(0, endedAt.Int64).UTC()
		ended = &value
	}
	return ptypes.Activity{ID: id, AgentID: agentID, Phase: ptypes.Phase(phase), Stage: ptypes.Stage(stage), StartedAt: time.Unix(0, startedAt).UTC(), EndedAt: ended, Notes: notes}, nil
}

func ScanComment(row sqlRowScanner) (ptypes.Comment, error) {
	var idText, taskText, authorText, body string
	var createdAt int64
	if err := row.Scan(&idText, &taskText, &authorText, &body, &createdAt); err != nil {
		return ptypes.Comment{}, fmt.Errorf("scan comment row: %w", err)
	}
	id, err := ptypes.ParseCommentID(idText)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("scanComment: invalid comment ID %q: %w", idText, err)
	}
	taskID, err := ptypes.ParseTaskID(taskText)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("scanComment: invalid task_id %q: %w", taskText, err)
	}
	authorID, err := ptypes.ParseAgentID(authorText)
	if err != nil {
		return ptypes.Comment{}, fmt.Errorf("scanComment: invalid author_id %q: %w", authorText, err)
	}
	return ptypes.Comment{ID: id, TaskID: taskID, AuthorID: authorID, Body: body, CreatedAt: time.Unix(0, createdAt).UTC()}, nil
}

func TimeToNullInt(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixNano()
}

// Startup preflight ----------------------------------------------------------

func preflightExistingReadOnly(target openTarget, models []ptypes.ModelEntry) (bool, error) {
	readOnlyDSN := target.readOnlyDSN
	if _, err := os.Stat(target.path + "-wal"); os.IsNotExist(err) {
		readOnlyDSN, err = withSQLiteQuery(readOnlyDSN, map[string]string{"immutable": "1"}, nil)
		if err != nil {
			return false, err
		}
	} else if err != nil {
		return false, fmt.Errorf("inspect WAL sidecar before read-only preflight: %w", err)
	}
	readOnly, err := openConfiguredSQLDB(readOnlyDSN, 1)
	if err != nil {
		return false, err
	}
	defer readOnly.Close()
	conn, err := readOnly.Conn(context.Background())
	if err != nil {
		return false, err
	}
	defer conn.Close()
	preflight := borrowConnScope(conn, projectionTargetLive)
	existing, err := preflight.tableExists("journal")
	if err != nil {
		return existing, err
	}
	if existing {
		contextSchema, err := preflight.classifyFactContextSchema()
		if err != nil {
			return true, err
		}
		if err := preflight.preflightCanonicalColumnsReadOnly(); err != nil {
			return true, err
		}
		if contextSchema == factContextSchemaLegacy {
			if err := preflight.verifyIntegrityReadOnlyLegacyCompatible(); err != nil {
				return true, err
			}
			if _, err := preflight.replayProjectionsReadOnlyLegacyCompatible(); err != nil {
				return true, err
			}
		} else {
			if err := preflight.verifyIntegrity(); err != nil {
				return true, err
			}
			if _, err := preflight.replayProjections(); err != nil {
				return true, err
			}
		}
	}
	if err := preflightActivationClone(conn, models); err != nil {
		return existing, err
	}
	return existing, nil
}

type moderncBackuper interface {
	NewBackup(string) (*moderncsqlite.Backup, error)
}

func preflightActivationClone(source *sql.Conn, models []ptypes.ModelEntry) error {
	n := memoryDBCounter.Add(1)
	cloneURI := fmt.Sprintf("file:provenance-preflight-%d?mode=memory&cache=shared", n)
	cloneDSN, err := withSQLiteQuery(cloneURI, nil, []string{fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)})
	if err != nil {
		return err
	}
	cloneDB, err := openConfiguredSQLDB(cloneDSN, 1)
	if err != nil {
		return fmt.Errorf("open isolated activation clone: %w", err)
	}
	defer cloneDB.Close()
	cloneConn, err := cloneDB.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("lease isolated activation clone: %w", err)
	}
	defer cloneConn.Close()

	if err := source.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(moderncBackuper)
		if !ok {
			return errors.New("modernc driver connection does not expose NewBackup; cannot perform read-only activation clone")
		}
		backup, err := backuper.NewBackup(cloneURI)
		if err != nil {
			return fmt.Errorf("start read-only activation clone: %w", err)
		}
		_, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		if stepErr != nil {
			if finishErr != nil {
				return errors.Join(fmt.Errorf("copy read-only activation clone: %w", stepErr), fmt.Errorf("finish read-only activation clone: %w", finishErr))
			}
			return fmt.Errorf("copy read-only activation clone: %w", stepErr)
		}
		if finishErr != nil {
			return fmt.Errorf("finish read-only activation clone: %w", finishErr)
		}
		return nil
	}); err != nil {
		return err
	}

	activation := borrowConnScope(cloneConn, projectionTargetLive)
	// The clone is a private copy owned by this preflight; no other writer exists.
	if err := activateSchema(activation, models, defaultActivationRetryPolicy()); err != nil {
		return fmt.Errorf("isolated activation clone rejected existing database: %w", err)
	}
	return nil
}
