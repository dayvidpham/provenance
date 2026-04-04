# Contributing to Provenance

This guide covers the development workflow for Provenance contributors. For coding standards, see [CLAUDE.md](CLAUDE.md).

## Getting Started

### Prerequisites

- Go 1.24 or later
- `make` (for build targets)
- SQLite 3 (development libraries)

### Clone and Set Up

```bash
git clone https://github.com/dayvidpham/provenance.git
cd provenance
go mod download
```

### Verify Your Setup

```bash
make fmt   # Check formatting (should produce no output)
make lint  # Run go vet (should produce no output)
```

## Development Workflow

### 1. Create a Branch

All work happens on feature branches:

```bash
git checkout -b feat/your-feature-name
```

Follow Conventional Commits for branch names:
- `feat/` — new feature
- `fix/` — bug fix
- `docs/` — documentation only
- `test/` — test additions or improvements
- `chore/` — tooling, dependencies, refactoring

### 2. Work in Slices

Provenance is organized in vertical slices. Each slice includes:

- **L1 (Types):** Define public types, interfaces, and enums
- **L2 (Tests):** Write integration tests that import production code
- **L3 (Implementation):** Implement production code to make L2 tests pass

Example: Slice 5 (Makefile + CI Foundation)
- L1: Define Makefile targets, CLAUDE.md structure
- L2: Verify go.mod deps resolve, CGO_ENABLED=0 build works
- L3: Write Makefile, go.mod, CLAUDE.md, CONTRIBUTING.md

Work backwards from the end point (what the user needs) to the types:
1. What is the final delivery? (CLI, library interface, API?)
2. What types does it need?
3. What tests verify it works?
4. What implementation makes those tests pass?

### 3. Make Your Changes

For each slice:

**L1 (Types):** Define structs, interfaces, and enums
```bash
# Edit types.go, enums.go, etc.
make fmt   # Format
make lint  # Check for obvious errors
# git add and commit when happy
```

**L2 (Tests):** Write integration tests that will initially fail
```bash
# Edit *_test.go files
# Import the actual production package (e.g., "github.com/dayvidpham/provenance")
# Tests will fail at this point — that's expected!
make test  # Should show failures
```

**L3 (Implementation):** Implement production code to make tests pass
```bash
# Edit .go files (not *_test.go)
# Make your implementation
make test  # Tests should now pass
make build # Verify CGO_ENABLED=0 build succeeds
```

### 4. Run Quality Gates

Before committing, ensure all gates pass:

```bash
make fmt    # Reformat code
make lint   # Check for errors
make test   # Run tests
make build  # Build with CGO_ENABLED=0
```

All four targets must succeed. If any fails, fix the issue and re-run.

### 5. Commit Your Work

Use `git agent-commit` (not `git commit`):

```bash
git agent-commit -m "feat(provenance): add Tracker interface"
```

For multi-line commit messages:
```bash
git agent-commit -m "feat(provenance): add Tracker interface

Implement the Tracker interface and OpenSQLite constructor as described in
PROPOSAL-2. This enables consumers to create task trackers backed by SQLite.

- Add Tracker interface with methods for task CRUD
- Add OpenSQLite and OpenMemory constructors
- Implement sqliteTracker with WAL mode"
```

Follow Conventional Commits format:
- `feat(scope): description` — new feature
- `fix(scope): description` — bug fix
- `docs: description` — documentation
- `test(scope): description` — test additions
- `chore(scope): description` — tooling, dependencies

### 6. Push and Create a Pull Request

Push your branch:
```bash
git push -u origin feat/your-feature-name
```

Create a pull request on GitHub with:
- Clear title (under 70 characters)
- Description of what changed and why
- Reference to any Beads issues (e.g., "Closes aura-plugins-abc123")

## Package Structure

The Provenance package is organized to separate concerns:

| Directory | Purpose |
|-----------|---------|
| Root package | Public types, Tracker interface, constructors. Only thing consumers import. |
| `internal/sqlite/` | All SQL operations and schema. Encapsulates the database layer. |
| `internal/graph/` | Graph operations backed by SQLite. Implements dominikbraun/graph.Store. |
| `internal/helpers/` | Utility functions for graph traversal (Ancestors, Descendants). |

### Adding New Types

1. Define the type in `types.go` (public, root package)
2. Add any related enums to `enums.go`
3. Add SQL operations in `internal/sqlite/` (e.g., `internal/sqlite/tasks.go`)
4. Add graph store operations in `internal/graph/store.go`
5. Write integration tests in `tracker_test.go`

### Adding New Enums

1. Define the enum in `enums.go` with iota constants
2. Implement `String()`, `MarshalText()`, `UnmarshalText()`, `IsValid()`
3. Add tests in `enums_test.go`

Example:
```go
type Status int

const (
    StatusOpen       Status = iota
    StatusInProgress
    StatusClosed
)

func (s Status) String() string {
    switch s {
    case StatusOpen:
        return "open"
    case StatusInProgress:
        return "in_progress"
    case StatusClosed:
        return "closed"
    default:
        return fmt.Sprintf("Status(%d)", s)
    }
}

func (s Status) MarshalText() ([]byte, error) {
    return []byte(s.String()), nil
}

func (s *Status) UnmarshalText(text []byte) error {
    switch string(text) {
    case "open":
        *s = StatusOpen
    case "in_progress":
        *s = StatusInProgress
    case "closed":
        *s = StatusClosed
    default:
        return fmt.Errorf("unknown status: %s", string(text))
    }
    return nil
}

func (s Status) IsValid() bool {
    return s >= StatusOpen && s <= StatusClosed
}
```

## Testing Strategy

Provenance uses **integration tests** as the primary testing approach:

- **Integration tests:** Test the full Tracker interface with real SQLite (via `:memory:`)
- **Unit tests:** Test SQL operations and graph functions with simple inputs/outputs
- **Mock dependencies:** Use interface mocks only for external services (e.g., time providers, file systems)

### Example Test Structure

```go
// tracker_test.go — integration test of the Tracker interface
package provenance_test

import (
    "testing"
    "github.com/dayvidpham/provenance"
    "github.com/google/uuid"
)

func TestTrackerCreateTask(t *testing.T) {
    // Create in-memory tracker
    tracker, err := provenance.OpenMemory()
    if err != nil {
        t.Fatalf("OpenMemory failed: %v", err)
    }
    defer tracker.Close()

    // Create task
    task := provenance.Task{
        ID:       provenance.TaskID{Namespace: "test", UUID: uuid.New()},
        Title:    "Test Task",
        Status:   provenance.StatusOpen,
        Priority: provenance.PriorityMedium,
    }

    // Store task
    err = tracker.CreateTask(task)
    if err != nil {
        t.Errorf("CreateTask failed: %v", err)
    }

    // Retrieve and verify
    retrieved, err := tracker.GetTask(task.ID)
    if err != nil {
        t.Errorf("GetTask failed: %v", err)
    }
    if retrieved.Title != task.Title {
        t.Errorf("Title mismatch: got %q, want %q", retrieved.Title, task.Title)
    }
}
```

### Running Tests

```bash
# Run all tests (CGO_ENABLED=0 — pure Go, no cgo)
CGO_ENABLED=0 go test -count=1 ./...

# Run a specific test
CGO_ENABLED=0 go test -count=1 ./... -run TestTrackerCreateTask

# Run tests with verbose output
CGO_ENABLED=0 go test -count=1 -v ./...

# Run tests with coverage
CGO_ENABLED=0 go test -count=1 -cover ./...
```

## Build Targets

All build targets are defined in the `Makefile`:

```bash
make fmt    # Format all Go files with gofmt
make lint   # Run go vet for static analysis
make test   # CGO_ENABLED=0 go test -count=1 ./...
make build  # CGO_ENABLED=0 go build ./...
make clean  # Remove bin/ directory
```

Each target can be run independently. All four quality gates (`fmt`, `lint`, `test`, `build`) must pass before committing.

## Troubleshooting

### "go: warning: 'all' matched no packages"

This warning appears when go.mod has dependencies but no source files import them yet. It's harmless and resolves once you add code.

### CGO_ENABLED=0 build fails

Ensure you're not importing C libraries or cgo-dependent packages. Provenance dependencies must all be pure Go:
- ✓ `zombiezen.com/go/sqlite` (pure Go)
- ✗ `github.com/mattn/go-sqlite3` (CGo)

### Make targets not found

Ensure you're in the provenance root directory and have `make` installed:
```bash
which make       # Should show /usr/bin/make or similar
ls -la Makefile  # Should exist
```

## Slice Planning

Work is organized in vertical slices. Each slice is independent and can be implemented in parallel.

### Slice Structure

A slice consists of three layers:

1. **L1 (Types):** Define types, interfaces, enums
   - Exit condition: File imports without error
   - No dependencies on L2 or L3

2. **L2 (Tests):** Write integration tests
   - Exit condition: Tests written, import production code, typecheck passes, tests fail (expected)
   - Tests import the actual production package (not a test-only export)

3. **L3 (Implementation):** Implement production code
   - Exit condition: All L2 tests pass, no TODO placeholders, real dependencies wired, production code path verified

### Example: Slice 1 (Core Types)

**L1 Tasks:**
- Define TaskID, AgentID, ActivityID, CommentID types
- Define Status, Priority, TaskType, EdgeKind, AgentKind enums
- Define Task, Agent (TPT), Activity, Edge, Label, Comment structs

**L2 Tasks:**
- Write tests for ID string formatting and parsing
- Write tests for enum String/MarshalText/UnmarshalText methods
- Verify all types compile and serialize correctly

**L3 Tasks:**
- Implement String() methods on all ID types
- Implement MarshalText/UnmarshalText on all enums
- Implement validation logic (IsValid())

## Questions?

For questions about the development process, architecture, or testing strategy, see:
- [CLAUDE.md](CLAUDE.md) — Coding standards and conventions
- [PROPOSAL-2.md](docs/PROPOSAL-2.md) — Architecture and design decisions
- GitHub Issues — Report bugs or request features
