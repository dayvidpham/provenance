# Provenance

A task dependency tracker with full [PROV-O](https://www.w3.org/TR/prov-o/) lineage for multi-agent workflows.

Provenance tracks work products, their dependencies, and their provenance across planning and implementation phases. It models three PROV-DM core types -- Entities (tasks), Agents (human/ML/software), and Activities -- connected by typed edges that record both dependencies and lineage.

Backed by SQLite (pure Go, no cgo). Uses [bestiary](https://github.com/dayvidpham/bestiary) as its ML model catalog (110+ models from [models.dev](https://models.dev)).

## Capabilities

- A globally ordered journal for typed task operations, authority assignments,
  decisions, evidence, and event attribution.
- An actor-and-authority-bound `Session` API, including atomic multi-effect
  commits, deterministic replay, and legacy-store migration.
- PROV-O entities, agents, activities, and relationships with fixed-ID software
  agent registration for reproducible tool identities.
- An optional DBOS durable-execution adapter that retries the same canonical
  journal operation without creating a second commit ledger.

## Install

```bash
go get github.com/dayvidpham/provenance
```

## Example

```go
tr, _ := provenance.OpenMemory()
defer tr.Close()

// Register the software actor that will commit task mutations.
system, _ := tr.RegisterSoftwareAgent(
    "my-project", "task-system", "1", "example")

// Establish the initial authority and bind a journaled mutation session.
genesis, _ := tr.Journal().Apply(provenance.OperationInput{
    OperationID:    "example-genesis",
    ActorID:        system.ID,
    CommandDigest:  []byte("example-genesis-command"),
    MutationDigest: []byte("example-genesis-mutation"),
    Effects: []provenance.Effect{{
        Sort:           provenance.EffectBootstrapAuthority,
        BootstrapLabel: "task-system",
        ResultSlot:     "authority",
    }},
})
authority := genesis.ResultSlots[0].ProducedJournalID
session := tr.As(system.ID, authority)

// Create a task through the ordered journal.
task, _ := session.Create("my-project", "Implement feature X", "",
    provenance.TaskTypeFeature, provenance.PriorityHigh,
    provenance.PhaseRequest)

// Register an ML agent from the bestiary catalog
agent, _ := tr.RegisterMLAgent("my-project",
    provenance.RoleArchitect, provenance.ProviderAnthropic,
    provenance.ModelID("claude-opus-4-6"))

// Track provenance through the same authorized session.
session.AddEdge(task.ID, agent.ID.String(), provenance.EdgeAttributedTo)
```

## Demo

```bash
go run ./cmd/demo
```

Exercises the full stack: bestiary catalog exploration, multi-provider agent registration (Anthropic + Google), PROV-O lineage edges, and persistence across sessions.

## Documentation

- [CONCEPTS.md](CONCEPTS.md) -- domain model, PROV-O/PROV-DM alignment, edge semantics, all type definitions
- [CONTRIBUTING.md](CONTRIBUTING.md) -- development workflow, testing, commit conventions
- [CLAUDE.md](CLAUDE.md) -- coding standards, directory structure, quality gates
- [docs/architecture.md](docs/architecture.md) -- components, package boundaries, journal and SQLite design, graph model, and DBOS durability
- [docs/test-performance.md](docs/test-performance.md) -- measured test costs, regression traps, and safe optimization experiments

## Development

Requires Go 1.25+. [Nix](https://nixos.org/) optional for reproducible toolchain:

```bash
nix develop             # enters devshell with Go, gopls, ast-grep, delve

make fmt                # gofmt
make lint               # go vet + ast-grep
make test               # strict normal scheduler matrix + CGO1 race gate
make test-local         # cached local iteration
make build              # CGO_ENABLED=0 go build ./...
```

`make lint` also enforces that production code contains no `time.Sleep` calls.
SQLite's `busy_timeout=5000` is the sole local contention wait; DBOS owns durable
retry policy. Full tests are not run with `CGO_ENABLED=0`.

## License

MIT -- see [LICENSE](LICENSE).
