# Provenance — Agent Coding Standards

This document defines the coding conventions and quality gates for the Provenance
project. All contributors (human and AI) must follow these standards.

## Project Identity

- **Module:** `github.com/dayvidpham/provenance`
- **Language:** Go 1.24+
- **CGo:** production builds use `CGO_ENABLED=0`; race tests use
  `CGO_ENABLED=1` as required by Go's race detector. Dependencies remain pure Go.

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
│   ├── ptypes/             # Public type package (imports bestiary for IsValid catalog check)
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
│   ├── sqlite/             # SQL persistence (database/sql + modernc.org/sqlite). No graph logic.
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
| `pkg/ptypes` | **Public type definitions and bestiary delegation**. Holds every public type, enum, and sentinel error. Imports bestiary for `Provider.IsValid()` catalog validation; does not import `database/sql` or any SQLite driver. This is what allows `internal/sqlite` to import the types without creating an import cycle through the root package. Consumers should not import this directly — use the root re-exports. |
| `pkg/namespace` | Namespace URI derivation (git remote → canonical HTTPS, working dir → `file://`). Used to scope IDs. Re-exported by root. |
| `internal/sqlite` | **All SQL operations**. Owns the `database/sql` pool over the `modernc.org/sqlite` driver, plus connection leasing, transactions, journal reduction, projection reads, replay, and migration. No graph logic — pure relational CRUD including agent table-per-type operations and ml_models seeding from the registry. |
| `internal/graph` | Implements `dominikbraun/graph.Store[string, Task]` backed by `internal/sqlite`. Bridges graph library and persistence. |
| `internal/helpers` | Graph traversal utilities (Ancestors, Descendants) composed from dominikbraun/graph primitives (DFS + PredecessorMap). |
| `internal/testutil` | Shared test fixtures (e.g., known-model lists for seeding test databases). |
| `cmd/demo` | Runnable demonstration that exercises the full library API end-to-end. Not a CLI — it's a scripted scenario to verify integration. Run with `go run ./cmd/demo`. |

### Why root and `pkg/ptypes` are split

The root package implements `sqliteTracker`, which delegates to `internal/sqlite`. `internal/sqlite` needs the type definitions (`Task`, `TaskID`, `MLAgent`, …) to write SQL against. If those types lived at the root, you'd have an import cycle: `root → internal/sqlite → root`.

The split solves it: `pkg/ptypes` holds type definitions (importing only bestiary for `Provider.IsValid()` catalog validation), `internal/sqlite` imports `ptypes`, and the root re-exports every `ptypes` symbol via Go type aliases (`type TaskID = ptypes.TaskID`). The aliases are transparent at compile time — `provenance.TaskID` and `ptypes.TaskID` are the *same* type — so consumers get a clean import surface (`provenance.TaskID`) without ever seeing the internal split. Critically, bestiary does not import provenance or `pkg/ptypes`, so there is no cyclic import risk from `ptypes → bestiary`.

## Dependencies (Approved)

Direct dependencies pinned in `go.mod`:

| Package | Purpose | Version |
|---------|---------|---------|
| `github.com/dayvidpham/bestiary` | ML model catalog (single source of truth for `DefaultModelRegistry`) | v0.0.2 |
| `github.com/dominikbraun/graph` | Directed graph operations, topological sort, cycle detection | v0.23.0 |
| `github.com/google/uuid` | UUIDv7 generation for IDs | v1.6.0 |
| `gopkg.in/yaml.v3` | YAML parsing (used by namespace and frontmatter helpers) | v3.0.1 |
| `modernc.org/sqlite` | Pure-Go SQLite driver, used through `database/sql` (journal, projections, local state) | v1.52.0 |
| `github.com/dbos-inc/dbos-transact-golang` | Durable-execution runtime for the DBOS adapter and borrowed-pool integration | v1.2.0 |

No other direct external dependencies may be added without supervisor approval. Indirect (transitive) dependencies are tracked in `go.mod`'s `indirect` block — see `CONTRIBUTING.md` for why `zombiezen.com/go/sqlite` still appears there.

## Go Conventions

### SQLite driver: `database/sql` over `modernc.org/sqlite`

`go build ./...` runs with `CGO_ENABLED=0`, so every dependency must be pure Go.
The tests are the exception: the authoritative suite is race-only and therefore
runs with `CGO_ENABLED=1` (see `CONTRIBUTING.md` and `TESTING.md`).

All SQLite access goes through the standard `database/sql` API with the
`modernc.org/sqlite` driver, registered under the driver name `"sqlite"`
(`internal/sqlite/db.go`, `sqliteDriverName`). Rules:

- Never import `github.com/mattn/go-sqlite3` (CGo).
- Do not reintroduce `zombiezen.com/go/sqlite`. `internal/sqlite` migrated off it;
  `sql_architecture_test.go` in the repository root fails if any non-test file in
  `internal/sqlite`, `internal/allocation`, or `internal/fusedtx` imports
  `zombiezen.com/go/sqlite` (or a subpackage), or calls the retired driver-specific
  methods `Execute`, `ExecuteTransient`, `LastInsertRowID`, or `Changes`. All
  production SQL must end at `ExecContext`, `QueryContext`, or `QueryRowContext`.
- `zombiezen.com/go/sqlite` *does* remain an indirect dependency in `go.mod`,
  because `github.com/dayvidpham/bestiary` still uses it. That is expected and
  CGo-free; see `CONTRIBUTING.md`.

### SQLite pool, DSN, and transaction discipline

`internal/sqlite` owns the persistence stack: it is the only package that opens a
pool holding Provenance's *journal* schema, and the conventions below are
enforced by the code in `internal/sqlite/db.go` — match them rather than
inventing a second pattern.

It is not the only package in the module that opens a `*sql.DB`.
`internal/fusedtx.OpenSystem` opens the DBOS *system* database from a
caller-supplied DSN it does not validate, with its own limits (16 open / 8 idle),
and none of the DSN discipline below applies to it: whatever `busy_timeout`,
`journal_mode`, and `foreign_keys` that DSN carries is what DBOS gets. Supplying
a WAL, non-zero-`busy_timeout` DSN there is the caller's obligation. This is
deliberate rather than an oversight — a post-connect probe would sample one
connection of a lazily grown pool and prove nothing about the rest (the same
reasoning recorded on `armForeignKeys`) — but it means "the module runs on WAL
with `busy_timeout=5000`" is a statement about `Open`, not about every handle in
the process.

**Pools and DSNs.** `Open` resolves one `openTarget` that carries the runtime,
activation, and read-only DSNs for a path. Connection settings are carried in
the DSN as `_pragma` values rather than applied ad hoc after connect: the
runtime DSN sets `busy_timeout(5000)`, `foreign_keys(1)`, `synchronous(NORMAL)`,
and — for file-backed databases — `journal_mode(WAL)`. A file-backed runtime pool
is bounded at 4 connections (`runtimePoolSize`, enough for the Apply-plus-reader
workload without unbounded fan-out); `:memory:` is rewritten to a process-unique
`file:provenance-memdb-N?mode=memory&cache=shared` URI with a pool of 1
(`memoryPoolSize`) so parallel opens stay isolated and the memory database stays
alive. `withSQLiteQuery` preserves caller-supplied URI fields, so extend a DSN
through it instead of string-concatenating query parameters.

**Connection ownership.** `database/sql` hands out arbitrary connections, so any
operation that needs connection-local state — a TEMP table, a connection-local
PRAGMA, or an explicit transaction — must pin one connection through
`DB.bindScope`, which returns a `connScope` and registers it for lifecycle
tracking. Release each scope exactly once (`release`); use `discard` only on
transaction-cleanup paths where the connection's transaction or PRAGMA state is
unknown. `borrowConnScope` wraps a connection whose lifetime belongs to
activation or preflight; releasing it is deliberately a no-op. `DB.Close` first
refuses new leases, then cancels and drains registered scopes, and only then
closes a pool it owns.

**Owned vs borrowed pools.** `Open` creates and owns its pool (`ownsPool: true`).
`OpenBorrowed` (public entry point `provenance.OpenBorrowedSQLite`) activates the
schema on a caller-owned `database/sql` pool and then uses that exact pool: it
never closes it, never validates or mutates its limits, and `DB.Close`
invalidates only the Provenance instance. Its DSN pragmas are the caller's too —
`busy_timeout`, `journal_mode`, and `synchronous` are never set on a borrowed
pool — with one exception this package does own: `foreign_keys` is forced ON for
the span of every lease and restored to the caller's captured value on release,
verified by read-back, retiring the connection when the restore cannot be proven
(SQLite ignores the pragma inside a transaction, so an unverified restore is a
silent leak of Provenance's state into the caller's pool). There is no second connection and no
cross-driver bridge — one pool, one physical database.

**Transactions.** Use `runImmediateTransaction` (`BEGIN IMMEDIATE`) wherever a
transaction reads before it writes. A deferred `BEGIN` takes the read lock first
and then needs a read-to-write promotion, on which SQLite never invokes the busy
handler, so contention would fail instantly and bypass `busy_timeout` entirely.
`runScopedTransaction` owns the rollback guarantees: it caps the connection's
`busy_timeout` at the caller's deadline, and rolls back on every non-commit path
using a fresh bounded context so a canceled caller cannot leave the connection
write-locked.

**Waiting.** On pools `Open` owns, SQLite's `busy_timeout=5000` is the only local
contention wait (a borrowed pool waits for whatever its owner's DSN configured,
and the DBOS system handle for whatever the caller passed to `OpenSystem`);
production code adds no sleep or private retry loop (the ast-grep lint gate
rejects production `time.Sleep`). The single sanctioned exception is schema
activation, which wraps a bounded 30s outer budget around
`activateSchemaWithRetry` whose per-attempt wait is still `busy_timeout`. Do not
extend it to storage operations — see `TESTING.md`, "Waiting and retries", and
`CONTRIBUTING.md`.

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

Use a `string` underlying type when the enum needs to interop with an external string-typed contract (e.g., `Provider` mirrors `bestiary.Provider`). The required methods still apply. For most string enums, `IsValid()` and `UnmarshalText()` should be **case-insensitive**. The exception is `Provider`: `IsValid()` is **case-sensitive** because it delegates to `bestiary.Provider(p).IsKnown()`, a case-sensitive catalog match against upstream models.dev provider names. `UnmarshalText()` for `Provider` applies normalization (trim whitespace, lowercase) before delegating the trimmed string to the case-sensitive check.

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
# The single authoritative suite — local, focused, CI readiness, and landing
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m ./...

# Narrow the scope during iteration
CGO_ENABLED=1 go test -race -run TestName ./internal/sqlite
```
There is one suite, and it always uses the race detector under
`CGO_ENABLED=1`. There is no separate non-race wave.

**Never pass `-count`.** Repeated execution is not evidence of correctness:
determinism must be argued from synchronization structure, not sampled by
re-running. The same applies to wrapping a test in a shell loop to hunt flakes.

`-cpu`, `-p`, and `-parallel` control scheduler, package, and `t.Parallel`
concurrency respectively; CI leaves them unset to use the runner's available
processors.
Production builds use `CGO_ENABLED=0`; do not run tests with `CGO_ENABLED=0`.

### Test file conventions
- Test files: `*_test.go` using `package foo_test` (black-box) or `package foo` (white-box).
- Import the actual production package — never a test-only re-export.
- Use dependency injection (interface mocks) for external services (SQLite, graph operations).
- Focus on integration tests over brittle unit tests.

### Quality gates (must pass before every commit)
```bash
make fmt    # gofmt — fails if any file needs formatting
make lint   # go vet ./... + ast-grep scan
make test   # one authoritative CGO_ENABLED=1 race-only suite
make build  # CGO_ENABLED=0 go build ./...
```

## Build

```bash
make fmt            # gofmt -w .
make lint           # go vet ./... + ast-grep scan
make test           # one authoritative CGO_ENABLED=1 race-only suite
make build          # runs fmt + lint + test, then CGO_ENABLED=0 go build ./...
make clean          # rm -rf bin/
```

`make build` is the full quality gate — it depends on `fmt`, `lint`, and the
single race-only `test` target before invoking `go build`.

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
