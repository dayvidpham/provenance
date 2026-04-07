# Provenance

A task dependency tracker with full [PROV-O](https://www.w3.org/TR/prov-o/) lineage for multi-agent workflows.

Provenance tracks work products, their dependencies, and their provenance across planning and implementation phases. It models three PROV-DM core types -- Entities (tasks), Agents (human/ML/software), and Activities -- connected by typed edges that record both dependencies and lineage.

Backed by SQLite (pure Go, no cgo). Uses [bestiary](https://github.com/dayvidpham/bestiary) as its ML model catalog (110+ models from [models.dev](https://models.dev)).

## Install

```bash
go get github.com/dayvidpham/provenance
```

## Example

```go
tr, _ := provenance.OpenMemory()
defer tr.Close()

// Create a task
task, _ := tr.Create("my-project", "Implement feature X", "",
    provenance.TaskTypeFeature, provenance.PriorityHigh, provenance.PhaseRequest)

// Register an ML agent from the bestiary catalog
agent, _ := tr.RegisterMLAgent("my-project",
    provenance.RoleArchitect, provenance.ProviderAnthropic,
    provenance.ModelID("claude-opus-4-6"))

// Track provenance: task attributed to agent
tr.AddEdge(task.ID, agent.ID.String(), provenance.EdgeAttributedTo)
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

## Development

Requires Go 1.24+. [Nix](https://nixos.org/) optional for reproducible toolchain:

```bash
nix develop             # enters devshell with Go, gopls, ast-grep, delve

make fmt                # gofmt
make lint               # go vet + ast-grep
make test               # CGO_ENABLED=1 go test -race -count=1 ./...
make build              # CGO_ENABLED=0 go build ./...
```

## License

MIT -- see [LICENSE](LICENSE).
