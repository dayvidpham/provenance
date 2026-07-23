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

	// SQLite table rebuilds must run with FK enforcement disabled before the
	// activation transaction starts. VerifyIntegrity checks the complete FK graph
	// before commit; runtime enforcement is restored after the transaction ends.
	if err := db.applyActivationPragmas(); err != nil {
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
	if err = db.applyActivationPragmas(); err != nil {
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

func (db *DB) applyActivationPragmas() error {
	for _, pragma := range []string{"PRAGMA busy_timeout=5000;", "PRAGMA foreign_keys=OFF;"} {
		if err := sqlitex.ExecuteTransient(db.conn, pragma, nil); err != nil {
			return fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	return nil
}

func (db *DB) enableWAL() error {
	return sqlitex.ExecuteTransient(db.conn, "PRAGMA journal_mode=WAL", nil)
}

func (db *DB) enableForeignKeys() error {
	return sqlitex.ExecuteTransient(db.conn, "PRAGMA foreign_keys=ON", nil)
}

// ---------------------------------------------------------------------------
// Schema DDL
// ---------------------------------------------------------------------------

func (db *DB) ensureSchema(models []ptypes.ModelEntry) error {
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
		// Plan layer (roadmap §3.1): plans reify the Phase enum. plan_steps orders one
		// Phase per step. Both are direct-write projection tables (like activities),
		// outside the §15 journal-convergence set.
		"CREATE TABLE IF NOT EXISTS plans (id TEXT PRIMARY KEY, title TEXT NOT NULL, version TEXT NOT NULL DEFAULT '') STRICT",
		"CREATE TABLE IF NOT EXISTS plan_steps (plan_id TEXT NOT NULL REFERENCES plans(id), ordinal INTEGER NOT NULL, phase_id INTEGER NOT NULL REFERENCES phases(id), title TEXT NOT NULL DEFAULT '', PRIMARY KEY (plan_id, ordinal)) STRICT, WITHOUT ROWID",
		"CREATE INDEX IF NOT EXISTS idx_plan_steps_plan ON plan_steps (plan_id)",
		// Derivation qualifier (roadmap §3.3): derivation_kinds is the seeded lookup of
		// the paper's seven controlled-vocabulary values. derivation_qualifiers attaches
		// exactly one typed reason to a derivation relationship — the composite FK onto
		// edges plus the CHECK make a qualifier on a non-derivation edge structurally
		// impossible (a qualifier can reference only a derived_from(1) or supersedes(2)
		// edge that exists). ON DELETE CASCADE removes the qualifier when its edge is
		// removed. Keyed by (source, target): one qualifier per derivation relationship,
		// however it is expressed — so the SHACL DerivationShape's single-valued
		// :derivationKind is satisfied.
		"CREATE TABLE IF NOT EXISTS derivation_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT",
		"CREATE TABLE IF NOT EXISTS derivation_qualifiers (source_id TEXT NOT NULL, target_id TEXT NOT NULL, edge_kind_id INTEGER NOT NULL, derivation_kind_id INTEGER NOT NULL REFERENCES derivation_kinds(id), activity_id TEXT REFERENCES activities(id), created_at INTEGER NOT NULL, PRIMARY KEY (source_id, target_id), FOREIGN KEY (source_id, target_id, edge_kind_id) REFERENCES edges(source_id, target_id, kind_id) ON DELETE CASCADE, CHECK (edge_kind_id IN (1, 2))) STRICT, WITHOUT ROWID",
		"CREATE INDEX IF NOT EXISTS idx_derivation_qualifiers_target ON derivation_qualifiers (target_id)",
	}

	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureSchema: statement %q: %w", stmt, err)
		}
	}
	// Idempotent additive migration (roadmap §3.1): a pre-plan-layer database gets the
	// nullable activities.plan_id FK column added (a no-op once present). Mirrors the
	// established ensureTasksWatermarkColumnLocked column-add approach.
	if err := db.ensureActivitiesPlanColumnLocked(); err != nil {
		return err
	}
	if err := db.seedReferenceData(models); err != nil {
		return err
	}
	if err := db.seedBuiltinPlanLocked(); err != nil {
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
		{seedDerivationKinds, []string{"label_correction", "deduplication", "difficulty_filtering", "translation", "contamination_scrubbing", "adversarial_filtering", "verification_subset"}},
	}
	for _, seed := range seeds {
		for id, name := range seed.names {
			if err := sqlitex.Execute(db.conn, seed.kind.query(), &sqlitex.ExecOptions{Args: []any{id, name}}); err != nil {
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
	seedDerivationKinds
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
	case seedDerivationKinds:
		return "INSERT OR IGNORE INTO derivation_kinds (id,name) VALUES (?1,?2)"
	default:
		panic("unknown reference seed kind")
	}
}

// seedMLModels inserts model entries into the ml_models table.
// Uses INSERT OR IGNORE so existing rows are preserved on re-open.
// Each model is inserted with parameterized queries to prevent SQL injection.
func (db *DB) seedMLModels(models []ptypes.ModelEntry) error {
	var existing int
	if err := sqlitex.Execute(db.conn, "SELECT COUNT(*) FROM ml_models", &sqlitex.ExecOptions{
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
	endTx := sqlitex.Save(db.conn)
	defer endTx(&err)
	for _, m := range models {
		if err = sqlitex.Execute(db.conn, "INSERT OR IGNORE INTO ml_models (provider_id, name) VALUES ((SELECT id FROM providers WHERE name = ?1), ?2)", &sqlitex.ExecOptions{Args: []any{string(m.Provider), string(m.Name)}}); err != nil {
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
//	id, agent_id, phase_id, stage_id, started_at, ended_at, notes, plan_id
//
// (8 columns, indexed 0–7). plan_id is nullable (NULL = legacy/unplanned).
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

	var planID *ptypes.PlanID
	if !stmt.ColumnIsNull(7) {
		pid, perr := ptypes.ParsePlanID(stmt.ColumnText(7))
		if perr != nil {
			return ptypes.Activity{}, fmt.Errorf("scanActivity: invalid plan_id %q: %w", stmt.ColumnText(7), perr)
		}
		planID = &pid
	}

	return ptypes.Activity{
		ID:        id,
		AgentID:   agentID,
		Phase:     ptypes.Phase(stmt.ColumnInt(2)),
		Stage:     ptypes.Stage(stmt.ColumnInt(3)),
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Notes:     stmt.ColumnText(6),
		PlanID:    planID,
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
