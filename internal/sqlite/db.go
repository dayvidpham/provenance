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
}

// Open opens (or creates) a SQLite database at dbPath and returns an
// initialised DB. Pass ":memory:" for an in-memory database.
//
// The schema is applied idempotently on every open (CREATE TABLE IF NOT EXISTS).
// Reference data (enums) is inserted via INSERT OR IGNORE.
// The models parameter provides the ML model entries to seed into ml_models.
func Open(dbPath string, models []ptypes.ModelEntry) (*DB, error) {
	conn, err := zs.OpenConn(dbPath, zs.OpenReadWrite|zs.OpenCreate|zs.OpenWAL|zs.OpenURI)
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.Open: failed to open SQLite at %q: %w — "+
				"ensure the path is writable, the parent directory exists, "+
				"and no other process holds an exclusive lock",
			dbPath, err,
		)
	}

	db := &DB{conn: conn}

	if err := db.applyPragmas(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: failed to apply pragmas on %q: %w", dbPath, err)
	}

	if err := db.ensureSchema(models); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite.Open: failed to apply schema on %q: %w", dbPath, err)
	}

	return db, nil
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

func (db *DB) applyPragmas() error {
	for _, p := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	} {
		if err := sqlitex.ExecuteTransient(db.conn, p, nil); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Schema DDL
// ---------------------------------------------------------------------------

func (db *DB) ensureSchema(models []ptypes.ModelEntry) error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS statuses (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS priorities (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS task_types (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS edge_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS agent_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS providers (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS roles (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS phases (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT`,
		`CREATE TABLE IF NOT EXISTS ml_models (
			id          INTEGER PRIMARY KEY,
			provider_id INTEGER NOT NULL REFERENCES providers(id),
			name        TEXT NOT NULL,
			UNIQUE (provider_id, name)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS agents (
			id      TEXT PRIMARY KEY,
			kind_id INTEGER NOT NULL REFERENCES agent_kinds(id)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS agents_human (
			agent_id TEXT PRIMARY KEY REFERENCES agents(id),
			name     TEXT NOT NULL,
			contact  TEXT NOT NULL DEFAULT ''
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS agents_ml (
			agent_id TEXT PRIMARY KEY REFERENCES agents(id),
			role_id  INTEGER NOT NULL REFERENCES roles(id),
			model_id INTEGER NOT NULL REFERENCES ml_models(id)
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS agents_software (
			agent_id TEXT PRIMARY KEY REFERENCES agents(id),
			name     TEXT NOT NULL,
			version  TEXT NOT NULL DEFAULT '',
			source   TEXT NOT NULL DEFAULT ''
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id           TEXT PRIMARY KEY,
			namespace    TEXT NOT NULL,
			title        TEXT NOT NULL,
			description  TEXT NOT NULL DEFAULT '',
			status_id    INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),
			priority_id  INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),
			type_id      INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),
			phase_id     INTEGER NOT NULL REFERENCES phases(id),
			owner_id     TEXT REFERENCES agents(id),
			notes        TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			closed_at    INTEGER,
			close_reason TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status    ON tasks (status_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_priority  ON tasks (priority_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_type      ON tasks (type_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_phase     ON tasks (phase_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_owner     ON tasks (owner_id)`,
		`CREATE TABLE IF NOT EXISTS edges (
			source_id  TEXT NOT NULL REFERENCES tasks(id),
			target_id  TEXT NOT NULL,
			kind_id    INTEGER NOT NULL REFERENCES edge_kinds(id),
			created_at INTEGER NOT NULL,
			PRIMARY KEY (source_id, target_id, kind_id)
		) STRICT, WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_edges_source ON edges (source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_target ON edges (target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_kind   ON edges (kind_id)`,
		`CREATE TABLE IF NOT EXISTS activities (
			id         TEXT PRIMARY KEY,
			agent_id   TEXT NOT NULL REFERENCES agents(id),
			phase_id   INTEGER NOT NULL REFERENCES phases(id),
			stage_id   INTEGER NOT NULL REFERENCES stages(id),
			started_at INTEGER NOT NULL,
			ended_at   INTEGER,
			notes      TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_activities_agent ON activities (agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_activities_phase ON activities (phase_id)`,
		`CREATE TABLE IF NOT EXISTS labels (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			name    TEXT NOT NULL,
			PRIMARY KEY (task_id, name)
		) STRICT, WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_labels_name ON labels (name)`,
		`CREATE TABLE IF NOT EXISTS comments (
			id         TEXT PRIMARY KEY,
			task_id    TEXT NOT NULL REFERENCES tasks(id),
			author_id  TEXT NOT NULL REFERENCES agents(id),
			body       TEXT NOT NULL,
			created_at INTEGER NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_comments_task   ON comments (task_id)`,
		`CREATE INDEX IF NOT EXISTS idx_comments_author ON comments (author_id)`,
	}

	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureSchema: %w — statement: %s", err, stmt[:min(len(stmt), 80)])
		}
	}
	return db.seedReferenceData(models)
}

// ---------------------------------------------------------------------------
// Seed data
// ---------------------------------------------------------------------------

func (db *DB) seedReferenceData(models []ptypes.ModelEntry) error {
	seeds := []string{
		`INSERT OR IGNORE INTO statuses (id, name) VALUES (0,'open'),(1,'in_progress'),(2,'closed')`,
		`INSERT OR IGNORE INTO priorities (id, name) VALUES (0,'critical'),(1,'high'),(2,'medium'),(3,'low'),(4,'backlog')`,
		`INSERT OR IGNORE INTO task_types (id, name) VALUES (0,'bug'),(1,'feature'),(2,'task'),(3,'epic'),(4,'chore')`,
		`INSERT OR IGNORE INTO edge_kinds (id, name) VALUES (0,'blocked_by'),(1,'derived_from'),(2,'supersedes'),(3,'discovered_from'),(4,'generated_by'),(5,'attributed_to')`,
		`INSERT OR IGNORE INTO agent_kinds (id, name) VALUES (0,'human'),(1,'machine_learning'),(2,'software')`,
		`INSERT OR IGNORE INTO providers (id, name) VALUES (0,'anthropic'),(1,'google'),(2,'openai'),(3,'local')`,
		`INSERT OR IGNORE INTO roles (id, name) VALUES (0,'human'),(1,'architect'),(2,'supervisor'),(3,'worker'),(4,'reviewer')`,
		`INSERT OR IGNORE INTO phases (id, name) VALUES (0,'request'),(1,'elicit'),(2,'propose'),(3,'review'),(4,'plan_uat'),(5,'ratify'),(6,'handoff'),(7,'impl_plan'),(8,'worker_slices'),(9,'code_review'),(10,'impl_uat'),(11,'landing'),(12,'unscoped')`,
		`INSERT OR IGNORE INTO stages (id, name) VALUES (0,'not_started'),(1,'in_progress'),(2,'blocked'),(3,'complete')`,
	}
	for _, seed := range seeds {
		if err := sqlitex.ExecuteTransient(db.conn, seed, nil); err != nil {
			return fmt.Errorf("seedReferenceData: %w — seed: %s", err, seed[:min(len(seed), 80)])
		}
	}

	// Seed ml_models from the provided model registry entries.
	if err := db.seedMLModels(models); err != nil {
		return fmt.Errorf("seedReferenceData: %w", err)
	}
	return nil
}

// seedMLModels inserts model entries into the ml_models table.
// Uses INSERT OR IGNORE so existing rows are preserved on re-open.
// Each model is inserted with parameterized queries to prevent SQL injection.
func (db *DB) seedMLModels(models []ptypes.ModelEntry) error {
	for _, m := range models {
		if err := sqlitex.Execute(db.conn,
			`INSERT OR IGNORE INTO ml_models (provider_id, name) VALUES (?, ?)`,
			&sqlitex.ExecOptions{Args: []any{int(m.Provider), m.Name}},
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
