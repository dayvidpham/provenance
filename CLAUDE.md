# Providence — Agent Coding Standards

This document defines the coding conventions and quality gates for the Providence
project. All contributors (human and AI) must follow these standards.

## Project Identity

- **Module:** `github.com/dayvidpham/providence`
- **Language:** Go 1.24+
- **CGo:** disabled (`CGO_ENABLED=0`) — all dependencies must be pure Go

## Directory Structure

```
providence/
├── providence.go           # Package doc + public facade (Tracker interface + constructors)
├── types.go                # All public types: TaskID, AgentID, ActivityID, Task, Agent (TPT), Edge, etc.
├── enums.go                # All iota enums: Status, Priority, TaskType, EdgeKind, AgentKind, Provider, Role, Phase, Stage
├── errors.go               # Sentinel errors and error constructors
├── tracker.go              # sqliteTracker implementation of Tracker interface
├── tracker_test.go         # Integration tests for Tracker (black-box, package providence_test)
├── internal/
│   ├── graph/
│   │   ├── store.go        # dominikbraun/graph Store[TaskID, Task] implementation backed by SQLite
│   │   └── store_test.go   # Store implementation tests
│   ├── sqlite/
│   │   ├── db.go           # Database open/close, WAL config, schema migration
│   │   ├── schema.go       # CREATE TABLE statements, indexes, migrations
│   │   ├── tasks.go        # Task CRUD SQL operations
│   │   ├── edges.go        # Edge insert/delete/query SQL operations
│   │   ├── agents.go       # Agent TPT CRUD (base + 3 child tables)
│   │   ├── activities.go   # Activity CRUD SQL operations
│   │   ├── labels.go       # Label add/remove/query SQL operations
│   │   ├── comments.go     # Comment add/query SQL operations
│   │   └── db_test.go      # SQLite integration tests
│   └── helpers/
│       ├── ancestors.go    # Ancestors/Descendants composed from DFS + PredecessorMap
│       └── ancestors_test.go
├── docs/
│   ├── PROPOSAL-1.md       # Original proposal (superseded)
│   └── PROPOSAL-2.md       # Current architecture and implementation plan
├── go.mod
├── go.sum
├── LICENSE
├── .gitignore
├── Makefile                # fmt, lint, test, build targets
├── CLAUDE.md               # This file
└── CONTRIBUTING.md         # Development workflow guide
```

### Package Responsibilities

| Package | Role |
|---------|------|
| `providence` (root) | Public API surface. All exported types, the `Tracker` interface, constructors (`OpenSQLite`, `OpenMemory`), and the internal `sqliteTracker` implementation. Consumers (e.g., pasture) import only this package. |
| `internal/sqlite` | All SQL operations. Encapsulates the zombiezen SQLite driver. No graph logic here — pure relational CRUD including agent table-per-type operations. |
| `internal/graph` | Implements `dominikbraun/graph.Store[TaskID, Task]` backed by `internal/sqlite`. Bridges graph library and persistence. |
| `internal/helpers` | Graph traversal utilities (Ancestors, Descendants) composed from dominikbraun/graph primitives. |

## Dependencies (Approved)

| Package | Purpose | Version |
|---------|---------|---------|
| `github.com/dominikbraun/graph` | Directed graph operations, topological sort, cycle detection | v0.23.0 |
| `github.com/google/uuid` | UUIDv7 generation | v1.6.0 |
| `zombiezen.com/go/sqlite` | Pure-Go SQLite (audit trail, local state) | latest |

No other external dependencies may be added without supervisor approval.

## Go Conventions

### No CGo
```go
// build constraint at top of any file that must remain CGo-free
//go:build !cgo
```
All SQLite usage MUST use `zombiezen.com/go/sqlite` (pure Go), never `mattn/go-sqlite3` or `modernc.org/sqlite`.

### Strongly-Typed Enums
Prefer typed constants with iota over bare strings or integers. All enums must include `String()`, `MarshalText()`, `UnmarshalText()`, and `IsValid()` methods:

```go
// Correct
type Status int

const (
    StatusOpen       Status = iota // Task is created but not yet started
    StatusInProgress               // Work is actively happening
    StatusClosed                   // Work is complete
)

// Wrong
const (
    StatusOpen = "open"            // stringly typed
    StatusClosed = 1               // magic number with no name
)
```

### ID Types
All ID types follow the format `{Namespace}--{UUIDv7}` with `String()` and `Parse*()` methods:

```go
type TaskID struct {
    Namespace string
    UUID      uuid.UUID
}

// String returns the wire format: "namespace--uuid".
func (id TaskID) String() string {
    return id.Namespace + "--" + id.UUID.String()
}

// ParseTaskID parses "namespace--uuid" into a TaskID.
// Uses strings.LastIndex to split on the rightmost "--" separator.
func ParseTaskID(s string) (TaskID, error) { ... }
```

### Actionable Errors
Every error must describe: what went wrong, why, where, when, and how to fix it.
```go
// Correct
fmt.Errorf("sqlite: failed to open database %q: %w — ensure the file exists, is readable, and is a valid SQLite database", path, err)

// Wrong
fmt.Errorf("database error")
```

### Graph Hashing
For dominikbraun/graph operations, implement the `Hash` function as:
```go
func (id TaskID) Hash() string {
    return id.String()
}
```

## Testing

### Mandatory flags
```bash
go test -race ./...
```
The `-race` flag is mandatory for all test runs to detect concurrent access issues.

**Note:** The `-race` flag requires cgo (`CGO_ENABLED=1`). This is only for tests; production binaries are built with `CGO_ENABLED=0`.

### Test file conventions
- Test files: `*_test.go` using `package foo_test` (black-box) or `package foo` (white-box).
- Import the actual production package — never a test-only re-export.
- Use dependency injection (interface mocks) for external services (SQLite, graph operations).
- Focus on integration tests over brittle unit tests.

### Quality gates (must pass before every commit)
```bash
make fmt    # gofmt — fails if any file needs formatting
make lint   # go vet ./...
make test   # CGO_ENABLED=1 go test -race ./... (race detector requires cgo)
make build  # CGO_ENABLED=0 go build ./... (production binaries are cgo-free)
```

## Build

```bash
make build          # produces bin/providence (if cmd exists)
make test           # go test -race ./...
make lint           # go vet ./...
make fmt            # gofmt -w .
make clean          # rm -rf bin/
```

Cross-compilation:
```bash
GOOS=linux   GOARCH=amd64  CGO_ENABLED=0 go build ./...
GOOS=darwin  GOARCH=arm64  CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64  CGO_ENABLED=0 go build ./...
```

## Commit Convention

Use Conventional Commits:
```
feat(providence): add Tracker interface and OpenSQLite constructor
fix(sqlite): handle empty task list gracefully
chore(providence): update go.sum after dependency bump
docs: clarify EdgeKind semantics
```

**IMPORTANT:** Workers must use `git agent-commit` instead of `git commit`:
```bash
git agent-commit -m "feat(providence): add Tracker interface"
```

## SQLite and Database Conventions

- Database schema lives in `internal/sqlite/schema.go` as CREATE TABLE statements.
- All schema changes must include migration logic in `internal/sqlite/db.go`.
- Use WAL (Write-Ahead Logging) mode for concurrent read access.
- Use prepared statements for all queries to prevent SQL injection.
- Test all database operations with in-memory SQLite (`:memory:`) in `*_test.go`.

## Type-Per-Type Hierarchy (Agent)

Providence models Agents using a table-per-type (TPT) pattern:

- Base table `agents` stores: `id`, `kind_id` (discriminator), `namespace`, `uuid`, `created_at`
- Child tables `agents_human`, `agents_ml`, `agents_software` store kind-specific attributes
- Always query through the base table first; use `kind_id` to determine which child table to load from

Example:
```go
// Query base agent to get kind
row := db.QueryRow("SELECT kind_id FROM agents WHERE id = ?", agentID)
// Load kind-specific fields from child table based on kind_id
```
