# Concepts

This document maps provenance's domain model to the [PROV-O](https://www.w3.org/TR/prov-o/) ontology and [PROV-DM](https://www.w3.org/TR/prov-dm/) data model. Every type and relationship in provenance has a corresponding PROV concept.

## Core Types

PROV-DM defines three core types. Provenance implements all three.

### Entity (prov:Entity) -- Task

A **Task** is a work product: a feature request, a proposal, an implementation slice, a code review finding. Tasks are the things that are produced, consumed, and tracked.

| PROV-DM | Provenance | Notes |
|---------|-----------|-------|
| `prov:Entity` | `Task` | Every task has a namespace-scoped UUIDv7 ID (`TaskID`) |
| Entity attributes | `Title`, `Description`, `Status`, `Priority`, `Type`, `Phase` | Extended attributes beyond PROV-DM |
| Entity identifier | `TaskID` (`{Namespace}--{UUIDv7}`) | Globally unique, time-sortable |

Tasks have lifecycle states (`Status`): Open, InProgress, Closed. They are classified by `TaskType` (bug, feature, task, epic, chore) and by `Phase` (request, elicit, propose, review, etc.) which identifies where in the workflow they were created.

### Agent (prov:Agent) -- Agent hierarchy

An **Agent** is something that bears responsibility for a task. Provenance uses a table-per-type (TPT) hierarchy to model three kinds of agents.

| PROV-DM | Provenance | Notes |
|---------|-----------|-------|
| `prov:Agent` | `Agent` (base) | Discriminated by `AgentKind` |
| `prov:Person` | `HumanAgent` | Name + contact (email, handle) |
| `prov:SoftwareAgent` | `SoftwareAgent` | Name + version + source URL |
| (no direct equivalent) | `MLAgent` | Role + model (from bestiary catalog) |

`MLAgent` extends PROV-DM's concept of `SoftwareAgent` with ML-specific attributes: the agent's `Role` in the workflow (architect, supervisor, worker, reviewer) and the `MLModel` it runs (identified by provider + model ID from the [bestiary](https://github.com/dayvidpham/bestiary) catalog).

**Roles** (`Role`): Human, Architect, Supervisor, Worker, Reviewer. These correspond to the agent roles in the Aura Protocol workflow.

**Providers** (`Provider`): String-typed to align with bestiary. `IsValid` uses
the catalog's case-sensitive provider names; text decoding trims whitespace and
normalizes case before validation.

### Activity (prov:Activity) -- Activity

An **Activity** is a bounded action performed by an agent. Activities have a start time, an optional end time, and are scoped to a `Phase` and `Stage`.

| PROV-DM | Provenance | Notes |
|---------|-----------|-------|
| `prov:Activity` | `Activity` | Identified by `ActivityID` |
| `prov:startedAtTime` | `StartedAt` | Always set at creation |
| `prov:endedAtTime` | `EndedAt` | Set by `EndActivity()`, nil until then |
| Activity attributes | `Phase`, `Stage`, `Notes` | Extended attributes |

`Stage` captures fine-grained progress within a phase: NotStarted, InProgress, Blocked, Complete.

## Relations (Edges)

PROV-DM defines a set of relations between entities, agents, and activities. Provenance implements these as typed `Edge` values with an `EdgeKind` discriminator.

### PROV-DM Relations

| PROV-DM Relation | Provenance EdgeKind | Source | Target | Semantics |
|------------------|-------------------|--------|--------|-----------|
| `prov:wasGeneratedBy` | `EdgeGeneratedBy` | Task | Activity | Which activity produced this task |
| `prov:wasAttributedTo` | `EdgeAttributedTo` | Task | Agent | Which agent is responsible for this task |
| `prov:wasDerivedFrom` | `EdgeDerivedFrom` | Task | Task | PROPOSAL-2 derived from PROPOSAL-1 |

### Extended Relations

These relations are not in PROV-DM but are essential for task dependency tracking:

| Provenance EdgeKind | Source | Target | Semantics |
|-------------------|--------|--------|-----------|
| `EdgeBlockedBy` | Task | Task | Dependency -- target must complete before source is ready. Cycle detection enforced. |
| `EdgeSupersedes` | Task | Task | PROPOSAL-3 supersedes PROPOSAL-2. Informational lineage, not a dependency. |
| `EdgeDiscoveredFrom` | Task | Task | Bug found during work on parent task. |

### Dependency Graph

`EdgeBlockedBy` is the only edge kind that affects task readiness. The blocked-by subgraph is a directed acyclic graph (DAG) enforced by cycle detection at insertion time.

- `Ready()` -- returns tasks with no open blockers
- `Blocked()` -- returns tasks with at least one open blocker
- `Ancestors(id)` -- all tasks that transitively block the given task
- `Descendants(id)` -- all tasks that are transitively waiting for the given task

Other edge kinds (DerivedFrom, Supersedes, DiscoveredFrom, GeneratedBy, AttributedTo) are informational lineage -- they record provenance but do not affect scheduling.

## Supporting Concepts

### Labels

String tags attached to tasks. Used for phase tracking (`aura:p1-user:s1_1-classify`), severity classification (`aura:severity:blocker`), and workflow state (`aura:superseded`). Add and remove are idempotent journaled mutations.

### Comments

Timestamped notes on tasks, authored by an agent. Used for review votes, progress updates, and audit trail. Comments are append-only journaled mutations.

### Namespace

Every ID includes a namespace (e.g., `aura-plugins`, `my-project`) that scopes entities to a project. Derived from the git remote URL or working directory via `DefaultNamespace()`.

### ModelRegistry

A queryable catalog of ML models used to seed the `ml_models` reference table and validate model names at agent registration time. Backed by [bestiary](https://github.com/dayvidpham/bestiary) (models.dev data). See `ModelRegistry` interface: `Models()`, `Lookup()`, `ModelsByProvider()`.

## Mutation And History Model

All task lifecycle, edge, label, and comment mutations are performed through a
`Session` bound to a committing actor and governing authority. Each call creates
one logical operation containing typed effects; `Session.Atomic` can commit
multiple effects together.

Provenance stores operations in one append-only global journal. `JournalID`, a
database-generated integer, is the sole causal order. `RecordedAt` provides a
readable audit timeline but never controls authorization, lifecycle, or replay.

The journal also records authority lifecycles, decisions, and evidence. Task,
edge, label, and comment tables are materialized projections. Apply validates
and inserts each operation before using the same projection fold that replay
runs against shadow tables. Exact retries identified by a stable `OperationID`
return the original committed result; changed canonical input under the same ID
returns a typed conflict.

DBOS integration can durably execute these operations across crashes and lost
responses. Provenance owns the versioned canonical input and outcome codecs;
DBOS owns workflow execution and checkpointing. See
[`docs/architecture.md`](docs/architecture.md) for the complete component model
and [`docs/journal-relational-contract.md`](docs/journal-relational-contract.md)
for the normative relational contract.

## PROV-O Alignment Summary

```
PROV-O                     Provenance
------                     ----------
prov:Entity            --> Task
prov:Agent             --> Agent (HumanAgent | MLAgent | SoftwareAgent)
prov:Activity          --> Activity

prov:wasGeneratedBy    --> EdgeGeneratedBy    (Task --> Activity)
prov:wasAttributedTo   --> EdgeAttributedTo   (Task --> Agent)
prov:wasDerivedFrom    --> EdgeDerivedFrom    (Task --> Task)

(extended)
                       --> EdgeBlockedBy      (Task --> Task, DAG-enforced)
                       --> EdgeSupersedes     (Task --> Task)
                       --> EdgeDiscoveredFrom (Task --> Task)
```

## SQLite Persistence

The persistence layer uses a single SQLite database with WAL mode. The global
journal uses class-table inheritance: one ordered supertype row plus exactly one
typed subtype row. Operations and their materialized projections commit in one
transaction. Startup integrity checks, full replay, and legacy migration fail
closed on incompatible schema or history.

Reference data (statuses, priorities, providers, etc.) is stored in lookup tables
with integer primary keys. The `ml_models` table bridges the string-typed
`Provider` from bestiary to relational foreign keys through
`providers(id, name)`.

See `docs/journal-relational-contract.md` for the normative journal schema and
`internal/sqlite` for the executable schema and queries.
