# Provenance

A task dependency tracker with full [PROV-O](https://www.w3.org/TR/prov-o/) lineage for multi-agent workflows.

Provenance tracks work products, their dependencies, and their provenance across planning and implementation phases. It is backed by SQLite (pure Go, no cgo) and uses [bestiary](https://github.com/dayvidpham/bestiary) as its model catalog.

## Install

```bash
go get github.com/dayvidpham/provenance
```

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/dayvidpham/provenance"
)

func main() {
    tr, _ := provenance.OpenMemory()
    defer tr.Close()

    // Create a task
    task, _ := tr.Create("my-project", "Implement feature X", "",
        provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)

    // Register an ML agent (model from bestiary catalog)
    agent, _ := tr.RegisterMLAgent("my-project",
        provenance.RoleArchitect, provenance.ProviderAnthropic,
        provenance.ModelID("claude-opus-4-6"))

    // Track provenance: task attributed to agent
    tr.AddEdge(task.ID, agent.ID.String(), provenance.EdgeAttributedTo)

    fmt.Printf("Task: %s\nAgent: %s (model: %s)\n",
        task.ID, agent.ID, agent.Model.Name)
}
```

## Demo

Run the integration demo to see provenance + bestiary working end-to-end:

```bash
go run ./cmd/demo
```

Output:

```
=== Provenance + Bestiary Integration Demo ===

Bestiary catalog: 110 models (22 Anthropic, 37 Google, 51 OpenAI)

Provider validation (case-insensitive):
  Provider("anthropic").IsValid() = true
  Provider("ANTHROPIC").IsValid() = true

Registering agents from bestiary catalog:
  Architect: aura--019d59de-b98b-76b0-b005-6808a2d945bb
    Role: architect | Model: claude-opus-4-6 | Provider: anthropic
  Worker:    aura--019d59de-b997-79e2-87b1-2671921d4186
    Role: worker | Model: gemini-2.0-flash | Provider: google

Read-back from SQLite (verify string Provider, not integer):
  Architect: Provider="anthropic"  Model="claude-opus-4-6"  Role=architect
  Worker:    Provider="google"  Model="gemini-2.0-flash"  Role=worker

...

=== Demo complete ===
```

## Key Concepts

**Tracker** is the central API. Open with `OpenSQLite(path)` for persistent storage or `OpenMemory()` for tests.

**Tasks** are work products (PROV-O Entities) with status, priority, type, and phase. Every task has a namespace-scoped UUIDv7 ID.

**Agents** track who did the work (PROV-O Agents). Three kinds:
- `HumanAgent` -- a person
- `MLAgent` -- an AI model (registered from the bestiary catalog)
- `SoftwareAgent` -- a tool or script

**Edges** are typed relationships between entities:
- `EdgeBlockedBy` -- dependency (cycle-detected)
- `EdgeDerivedFrom`, `EdgeSupersedes` -- lineage
- `EdgeGeneratedBy` -- task to activity
- `EdgeAttributedTo` -- task to agent

**ModelRegistry** provides the catalog of known ML models. Defaults to [bestiary](https://github.com/dayvidpham/bestiary) (110+ models from models.dev). Override with `WithModelRegistry()`:

```go
// Custom model source
tr, _ := provenance.OpenSQLite(path,
    provenance.WithModelRegistry(
        provenance.RegistryFromBestiary(bestiary.StaticModels())))

// Test-only models
tr, _ := provenance.OpenMemory(
    provenance.WithModelRegistry(
        provenance.NewRegistry([]provenance.ModelEntry{
            {Provider: provenance.ProviderAnthropic, Name: "test-model"},
        })))
```

## Development

Requires Go 1.24+ and [Nix](https://nixos.org/) (optional, for reproducible toolchain):

```bash
# With Nix
nix develop     # enters devshell with Go, gopls, ast-grep, delve

# Quality gates
make fmt        # gofmt
make lint       # go vet + ast-grep
make test       # CGO_ENABLED=1 go test -race -count=1 ./...
make build      # CGO_ENABLED=0 go build ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for development workflow and [CLAUDE.md](CLAUDE.md) for coding standards.

## License

MIT -- see [LICENSE](LICENSE).
