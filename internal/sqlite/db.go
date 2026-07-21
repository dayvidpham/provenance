// Package sqlite provides the SQLite persistence layer for the Provenance
// task dependency tracker. It implements all CRUD operations for tasks, edges,
// agents, labels, comments, and activities.
//
// This package imports pkg/ptypes for all type definitions and uses
// zombiezen.com/go/sqlite for pure-Go SQLite access (no CGo required at
// runtime, though CGo tests use the C library for the race detector).
//
// The DB struct holds a single SQLite connection guarded by a sync.Mutex.
// All exported methods acquire the mutex before accessing the connection.
package sqlite

import (
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// DB wraps a single SQLite connection with a mutex for safe concurrent access.
// Use Open to create a new DB instance.
type DB struct {
	mu   sync.Mutex
	conn *zs.Conn
	// projectionTarget is a closed selector for complete static SQL variants.
	// SQLite cannot bind identifiers, so arbitrary table names are never stored.
	projectionTarget projectionTarget
}

type projectionTarget uint8

const (
	projectionTargetLive projectionTarget = iota
	projectionTargetShadow
)

func (target projectionTarget) label() string {
	if target == projectionTargetShadow {
		return "shadow projection"
	}
	return "live projection"
}

// Open opens (or creates) a SQLite database at dbPath and returns an
// initialised DB. Pass ":memory:" for an in-memory database.
//
// The schema is applied idempotently on every open (CREATE TABLE IF NOT EXISTS).
// Reference data (enums) is inserted via INSERT OR IGNORE.
// The models parameter provides the ML model entries to seed into ml_models.
func Open(dbPath string, models []ptypes.ModelEntry) (*DB, error) {
	existed := false
	if dbPath != ":memory:" {
		if info, err := os.Stat(dbPath); err == nil {
			existed = info.Size() > 0
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("sqlite.Open: inspect path %q before read-only preflight: %w", dbPath, err)
		}
	}
	existingJournal := false
	if existed {
		var err error
		existingJournal, err = preflightExistingReadOnly(dbPath, models)
		if err != nil {
			return nil, fmt.Errorf("sqlite.Open: read-only startup preflight failed on %q: %w", dbPath, err)
		}
	}

	conn, err := zs.OpenConn(dbPath, zs.OpenReadWrite|zs.OpenCreate|zs.OpenURI)
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.Open: failed to open SQLite at %q: %w — "+
				"ensure the path is writable, the parent directory exists, "+
				"and no other process holds an exclusive lock",
			dbPath, err,
		)
	}

	db := &DB{conn: conn}

	if err := db.applyNonPersistentPragmas(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: failed to apply pragmas on %q: %w", dbPath, err)
	}

	existing, err := db.tableExistsLocked("journal")
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: inspect existing schema on %q: %w", dbPath, err)
	}
	if existing != existingJournal {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: schema changed between read-only preflight (journal=%t) and activation (journal=%t) on %q; retry after concurrent schema work finishes", existingJournal, existing, dbPath)
	}
	activate := func() error {
		if err := db.ensureSchema(models); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := db.VerifyIntegrity(); err != nil {
			return fmt.Errorf("whole-journal integrity: %w", err)
		}
		if _, err := db.ReplayProjections(); err != nil {
			return fmt.Errorf("journal replay: %w", err)
		}
		return nil
	}
	var activationErr error
	end := sqlitex.Save(conn)
	activationErr = activate()
	end(&activationErr)
	err = activationErr
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: transactional startup validation failed on %q: %w", dbPath, err)
	}
	if err := db.enableForeignKeys(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: enable runtime foreign-key enforcement on %q: %w", dbPath, err)
	}
	if err := db.enableWAL(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: enable WAL after validated activation on %q: %w", dbPath, err)
	}

	return db, nil
}

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
	db := &DB{conn: conn}
	defer conn.Close()
	existing, err := db.tableExistsLocked("journal")
	if err != nil {
		return existing, err
	}
	if existing {
		if err := db.preflightCanonicalColumnsReadOnly(); err != nil {
			return true, err
		}
		if err := db.VerifyIntegrity(); err != nil {
			return true, err
		}
		if _, err := db.ReplayProjections(); err != nil {
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
	db := &DB{conn: clone}
	if err = db.applyNonPersistentPragmas(); err != nil {
		return err
	}
	var activationErr error
	end := sqlitex.Save(clone)
	if activationErr = db.ensureSchema(models); activationErr == nil {
		activationErr = db.VerifyIntegrity()
	}
	if activationErr == nil {
		_, activationErr = db.ReplayProjections()
	}
	end(&activationErr)
	if activationErr != nil {
		return fmt.Errorf("isolated activation clone rejected existing database: %w", activationErr)
	}
	return nil
}

// Conn returns the underlying SQLite connection. This is exposed so that
// the root package's graphStore can access the connection for vertex/edge
// operations without duplicating SQL. The caller MUST hold the DB mutex
// (via Lock/Unlock) when using this connection.
func (db *DB) Conn() *zs.Conn {
	return db.conn
}

// Lock acquires the DB mutex. Use this when you need direct access to Conn().
func (db *DB) Lock() {
	db.mu.Lock()
}

// Unlock releases the DB mutex.
func (db *DB) Unlock() {
	db.mu.Unlock()
}

// Close releases the SQLite connection. Safe to call multiple times.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.conn == nil {
		return nil
	}
	err := db.conn.Close()
	db.conn = nil
	if err != nil {
		return fmt.Errorf(
			"sqlite.DB.Close: failed to close SQLite connection: %w — "+
				"this may indicate uncommitted transactions",
			err,
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pragmas
// ---------------------------------------------------------------------------

func (db *DB) applyNonPersistentPragmas() error {
	for _, p := range []sqlStatement{sqlStatement270, sqlStatement271} {
		if err := executeStatement(db.conn, p, nil); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

func (db *DB) enableWAL() error {
	return executeStatement(db.conn, sqlStatement032, nil)
}

func (db *DB) enableForeignKeys() error {
	return executeStatement(db.conn, sqlStatement033, nil)
}

// ---------------------------------------------------------------------------
// Schema DDL
// ---------------------------------------------------------------------------

func (db *DB) ensureSchema(models []ptypes.ModelEntry) error {
	ddl := []sqlStatement{sqlStatement272, sqlStatement273, sqlStatement274, sqlStatement275, sqlStatement276, sqlStatement277, sqlStatement278, sqlStatement279, sqlStatement280, sqlStatement281, sqlStatement282, sqlStatement283, sqlStatement284, sqlStatement285, sqlStatement286, sqlStatement231, sqlStatement287, sqlStatement288, sqlStatement289, sqlStatement290, sqlStatement291, sqlStatement292, sqlStatement293, sqlStatement294, sqlStatement295, sqlStatement296, sqlStatement297, sqlStatement298, sqlStatement299, sqlStatement300, sqlStatement301, sqlStatement302, sqlStatement303}

	for _, stmt := range ddl {
		if err := executeStatement(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureSchema: statement %d: %w", stmt, err)
		}
	}
	if err := db.seedReferenceData(models); err != nil {
		return err
	}
	if err := db.ensureJournalSchema(); err != nil {
		return err
	}
	return db.ensureOperationsSchema()
}

// ---------------------------------------------------------------------------
// Seed data
// ---------------------------------------------------------------------------

func (db *DB) seedReferenceData(models []ptypes.ModelEntry) error {
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
			if err := executeStatement(db.conn, seed.kind.statement(), &sqlitex.ExecOptions{Args: []any{id, name}}); err != nil {
				return fmt.Errorf("seedReferenceData: kind %d id %d: %w", seed.kind, id, err)
			}
		}
	}

	// Seed ml_models from the provided model registry entries.
	if err := db.seedMLModels(models); err != nil {
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

func (kind referenceSeedKind) statement() sqlStatement {
	switch kind {
	case seedStatuses:
		return sqlStatement034
	case seedPriorities:
		return sqlStatement035
	case seedTaskTypes:
		return sqlStatement036
	case seedEdgeKinds:
		return sqlStatement037
	case seedAgentKinds:
		return sqlStatement038
	case seedProviders:
		return sqlStatement039
	case seedRoles:
		return sqlStatement040
	case seedPhases:
		return sqlStatement041
	case seedStages:
		return sqlStatement042
	default:
		panic("unknown reference seed kind")
	}
}

// seedMLModels inserts model entries into the ml_models table.
// Uses INSERT OR IGNORE so existing rows are preserved on re-open.
// Each model is inserted with parameterized queries to prevent SQL injection.
func (db *DB) seedMLModels(models []ptypes.ModelEntry) error {
	var existing int
	if err := executeStatement(db.conn,
		sqlStatement043,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *zs.Stmt) error {
				existing = stmt.ColumnInt(0)
				return nil
			},
		},
	); err != nil {
		return fmt.Errorf("seedMLModels: count existing models: %w", err)
	}
	if existing >= len(models) {
		return nil
	}

	var err error
	endTx := sqlitex.Save(db.conn)
	defer endTx(&err)
	for _, m := range models {
		if err = executeStatement(db.conn,
			sqlStatement044,
			&sqlitex.ExecOptions{Args: []any{string(m.Provider), string(m.Name)}},
		); err != nil {
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
// (14 columns, indexed 0–13).
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
// (7 columns, indexed 0–6).
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
// (5 columns, indexed 0–4).
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
