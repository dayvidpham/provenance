# Provenance — Agent Coding Standards

This document defines the coding conventions and quality gates for the Provenance
project. All contributors (human and AI) must follow these standards.

## Project Identity

- **Module:** `github.com/dayvidpham/provenance`
- **Language:** Go 1.24+
- **CGo:** disabled (`CGO_ENABLED=0`) — all dependencies must be pure Go

## Directory Structure

```
provenance/
├── doc.go                  # Package documentation
├── provenance.go           # Tracker interface + OpenSQLite/OpenMemory constructors
├── tracker.go              # sqliteTracker implementation of Tracker
├── adapter.go              # RegistryFromBestiary adapter (bestiary → ModelRegistry)
├── models.go               # inMemoryRegistry, NewRegistry, DefaultModelRegistry
├── options.go              # Functional options (WithModelRegistry, etc.)
├── reexports.go            # Type aliases re-exporting pkg/ptypes + pkg/namespace
├── tracker_test.go         # Integration tests for Tracker (black-box)
├── models_test.go          # ModelRegistry tests (black-box)
├── demo_test.go            # End-to-end demos (black-box)
├── reexport_test.go        # Verifies type aliases match ptypes originals
├── create_permutation_test.go
│
├── pkg/
│   ├── ptypes/             # Zero-dependency public type package
│   │   ├── types.go        # TaskID, AgentID, ActivityID, Task, Agent (TPT), Edge, etc.
│   │   ├── enums.go        # All enums: Status, Priority, TaskType, EdgeKind, AgentKind,
│   │   │                   #   Provider (string), Role, Phase, Stage
│   │   ├── errors.go       # Sentinel errors and error constructors
│   │   ├── models.go       # ModelEntry, ModelID, ModelRegistry interface
│   │   └── *_test.go       # Type, enum, and parse permutation tests
│   │
│   └── namespace/          # Namespace URI utilities (DefaultNamespace, FromGitRemote, …)
│       ├── namespace.go
│       └── namespace_test.go
│
├── internal/
│   ├── sqlite/             # SQL persistence (zombiezen). No graph logic.
│   │   ├── db.go           # Open/Close, WAL config, schema migration, ml_models seeding
│   │   ├── tasks.go        # Task CRUD
│   │   ├── edges.go        # Edge insert/delete/query
│   │   ├── agents.go       # Agent TPT CRUD (base + 3 child tables)
│   │   ├── activities.go   # Activity CRUD
│   │   ├── labels.go       # Label add/remove/query
│   │   ├── comments.go     # Comment add/query
│   │   └── db_test.go      # SQLite integration tests
│   │
│   ├── graph/              # dominikbraun/graph Store backed by internal/sqlite
│   │   ├── store.go
│   │   └── store_test.go
│   │
│   ├── helpers/            # Graph traversal (Ancestors/Descendants)
│   │   ├── ancestors.go
│   │   └── ancestors_test.go
│   │
│   └── testutil/           # Shared test fixtures (e.g., TestModels)
│       └── fixtures.go
│
├── cmd/
│   └── demo/               # Runnable demonstration of the full library API
│       └── main.go
│
├── docs/                   # Historical proposals (superseded — for audit trail)
│   ├── PROPOSAL-1.md
│   ├── PROPOSAL-2.md
│   ├── FOLLOWUP_PROPOSAL-1.md
│   └── FOLLOWUP_PROPOSAL-2.md
│
├── go.mod, go.sum
├── flake.nix, flake.lock   # Nix dev shell (optional)
├── LICENSE, .gitignore
├── Makefile                # fmt, lint, test, build targets
├── README.md
├── CONCEPTS.md             # PROV-O / PROV-DM domain alignment
├── CONTRIBUTING.md         # Development workflow guide
├── AGENTS.md               # Agent-facing onboarding (bd usage, etc.)
└── CLAUDE.md               # This file
```

### Package Responsibilities

| Package | Role |
|---------|------|
| `provenance` (root) | **Public API surface**. Consumers (e.g., pasture) import only this package. Holds the `Tracker` interface, constructors (`OpenSQLite`, `OpenMemory`), the `sqliteTracker` implementation, and the bestiary adapter. Re-exports every `pkg/ptypes` and `pkg/namespace` symbol via type aliases (`reexports.go`) so consumers see `provenance.TaskID` rather than `ptypes.TaskID`. |
| `pkg/ptypes` | **Zero-dependency type definitions**. Holds every public type, enum, and sentinel error. Imports nothing outside stdlib (no bestiary, no zombiezen). This is what allows `internal/sqlite` to import the types without creating an import cycle through the root package. Consumers should not import this directly — use the root re-exports. |
| `pkg/namespace` | Namespace URI derivation (git remote → canonical HTTPS, working dir → `file://`). Used to scope IDs. Re-exported by root. |
| `internal/sqlite` | **All SQL operations**. Encapsulates the zombiezen SQLite driver. No graph logic — pure relational CRUD including agent table-per-type operations and ml_models seeding from the registry. |
| `internal/graph` | Implements `dominikbraun/graph.Store[string, Task]` backed by `internal/sqlite`. Bridges graph library and persistence. |
| `internal/helpers` | Graph traversal utilities (Ancestors, Descendants) composed from dominikbraun/graph primitives (DFS + PredecessorMap). |
| `internal/testutil` | Shared test fixtures (e.g., known-model lists for seeding test databases). |
| `cmd/demo` | Runnable demonstration that exercises the full library API end-to-end. Not a CLI — it's a scripted scenario to verify integration. Run with `go run ./cmd/demo`. |

### Why root and `pkg/ptypes` are split

The root package implements `sqliteTracker`, which delegates to `internal/sqlite`. `internal/sqlite` needs the type definitions (`Task`, `TaskID`, `MLAgent`, …) to write SQL against. If those types lived at the root, you'd have an import cycle: `root → internal/sqlite → root`.

The split solves it: `pkg/ptypes` holds type definitions only (zero deps), `internal/sqlite` imports `ptypes`, and the root re-exports every `ptypes` symbol via Go type aliases (`type TaskID = ptypes.TaskID`). The aliases are transparent at compile time — `provenance.TaskID` and `ptypes.TaskID` are the *same* type — so consumers get a clean import surface (`provenance.TaskID`) without ever seeing the internal split.

## Dependencies (Approved)

Direct dependencies pinned in `go.mod`:

| Package | Purpose | Version |
|---------|---------|---------|
| `github.com/dayvidpham/bestiary` | ML model catalog (single source of truth for `DefaultModelRegistry`) | v0.1.0 |
| `github.com/dominikbraun/graph` | Directed graph operations, topological sort, cycle detection | v0.23.0 |
| `github.com/google/uuid` | UUIDv7 generation for IDs | v1.6.0 |
| `gopkg.in/yaml.v3` | YAML parsing (used by namespace and frontmatter helpers) | v3.0.1 |
| `zombiezen.com/go/sqlite` | Pure-Go SQLite (audit trail, local state) | v1.4.2 |

No other direct external dependencies may be added without supervisor approval. Indirect (transitive) dependencies are tracked in `go.mod`'s `indirect` block — see `CONTRIBUTING.md` for why `modernc.org/sqlite` appears there.

## Go Conventions

### No CGo
Production builds run with `CGO_ENABLED=0`. All direct SQLite usage MUST go through `zombiezen.com/go/sqlite` (pure Go). Never import `github.com/mattn/go-sqlite3` (CGo) and never import `modernc.org/sqlite` directly — even though `modernc.org/sqlite` is pure Go, we standardise on the zombiezen API at the source level.

`modernc.org/sqlite` *does* appear as an indirect dependency in `go.mod` because zombiezen and bestiary use it transitively. That's expected and CGo-free; see `CONTRIBUTING.md` for the rationale.

### Strongly-Typed Enums
Prefer named types with explicit constants over bare strings or integers. All enums must implement `String()`, `MarshalText()`, `UnmarshalText()`, and `IsValid()`.

The default form is `iota`-based `int` enums (used for `Status`, `Priority`, `TaskType`, `EdgeKind`, `AgentKind`, `Role`, `Phase`, `Stage`):

```go
type Status int

const (
    StatusOpen       Status = iota // Task is created but not yet started
    StatusInProgress               // Work is actively happening
    StatusClosed                   // Work is complete
)
```

Use a `string` underlying type when the enum needs to interop with an external string-typed contract (e.g., `Provider` mirrors `bestiary.Provider`). The required methods still apply, and `IsValid()`/`UnmarshalText()` should be **case-insensitive** for string enums:

```go
type Provider string

const (
    ProviderAnthropic Provider = "anthropic"
    ProviderGoogle    Provider = "google"
    ProviderOpenAI    Provider = "openai"
    ProviderLocal     Provider = "local"
)
```

What's wrong is bare untyped constants:

```go
// Wrong — stringly typed, no IsValid, no compiler enforcement
const StatusOpen = "open"

// Wrong — magic number with no enum type
const StatusClosed = 1
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
CGO_ENABLED=1 go test -race -count=1 ./...
```
Tests run with `CGO_ENABLED=1` and `-race` to detect concurrent access issues. Production builds use `CGO_ENABLED=0`.

### Test file conventions
- Test files: `*_test.go` using `package foo_test` (black-box) or `package foo` (white-box).
- Import the actual production package — never a test-only re-export.
- Use dependency injection (interface mocks) for external services (SQLite, graph operations).
- Focus on integration tests over brittle unit tests.

### Quality gates (must pass before every commit)
```bash
make fmt    # gofmt — fails if any file needs formatting
make lint   # go vet ./... + ast-grep scan
make test   # CGO_ENABLED=1 go test -race -count=1 ./...
make build  # CGO_ENABLED=0 go build ./...
```

## Build

```bash
make fmt            # gofmt -w .
make lint           # go vet ./... + ast-grep scan
make test           # CGO_ENABLED=1 go test -race -count=1 ./...
make build          # runs fmt + lint + test, then CGO_ENABLED=0 go build ./...
make clean          # rm -rf bin/
```

`make build` is the full quality gate — it depends on `fmt`, `lint`, and `test` before invoking `go build`.

Cross-compilation:
```bash
GOOS=linux   GOARCH=amd64  CGO_ENABLED=0 go build ./...
GOOS=darwin  GOARCH=arm64  CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64  CGO_ENABLED=0 go build ./...
```

## Commit Convention

Use Conventional Commits:
```
feat(provenance): add Tracker interface and OpenSQLite constructor
fix(sqlite): handle empty task list gracefully
chore(provenance): update go.sum after dependency bump
docs: clarify EdgeKind semantics
```

**IMPORTANT:** Workers must use `git agent-commit` instead of `git commit`:
```bash
git agent-commit -m "feat(provenance): add Tracker interface"
```

## SQLite and Database Conventions

- Database schema and CREATE TABLE statements live in `internal/sqlite/db.go`.
- All schema changes must include migration logic in `internal/sqlite/db.go`.
- Use WAL (Write-Ahead Logging) mode for concurrent read access.
- Use prepared statements for all queries to prevent SQL injection.
- Test all database operations with in-memory SQLite (`:memory:`) in `*_test.go`.

## Type-Per-Type Hierarchy (Agent)

Provenance models Agents using a table-per-type (TPT) pattern:

- Base table `agents` stores: `id`, `kind_id` (discriminator), `namespace`, `uuid`, `created_at`
- Child tables `agents_human`, `agents_ml`, `agents_software` store kind-specific attributes
- Always query through the base table first; use `kind_id` to determine which child table to load from

Example:
```go
// Query base agent to get kind
row := db.QueryRow("SELECT kind_id FROM agents WHERE id = ?", agentID)
// Load kind-specific fields from child table based on kind_id
```
