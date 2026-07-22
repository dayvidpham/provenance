# Architecture

This document describes Provenance's runtime layers, persistence model, and the
boundaries between its domain API, global journal, SQLite projections, and DBOS
durable execution. For domain terminology and PROV-O correspondence, see
[`CONCEPTS.md`](../CONCEPTS.md). The normative relational details are in
[`journal-relational-contract.md`](journal-relational-contract.md).

## Design Goals

Provenance is a local, concurrent task and lineage store for multi-agent work. Its
architecture prioritizes:

- One public Go package for consumers.
- Strongly typed task, actor, activity, relation, and journal contracts.
- One canonical order for every durable mutation.
- Atomic mutation and projection updates in SQLite.
- Replayable history and fail-closed integrity checks.
- Optional durable execution without coupling domain types to DBOS storage.
- Pure-Go production builds.

## System Overview

```text
Application
  |
  v
provenance.Tracker -------------------- ModelRegistry
  | reads                                  |
  |                                        v
  |                                  bestiary catalog
  |
  +--> Tracker.As(actor, authority) --> Session mutation SDK
  |                                      |
  |                                      v
  |                              canonical OperationInput
  |                                      |
  |                    +-----------------+-----------------+
  |                    |                                   |
  |                    v                                   v
  |             JournalAPI.Apply                    DBOSAdapter.Apply
  |                    |                                   |
  |                    +-----------------+-----------------+
  |                                      v
  |                              SQLite transaction
  |                         journal + typed subtypes
  |                         materialized projections
  |                                      |
  +---------------- reads ---------------+
                         |
                         +--> graph adapter and traversal
```

The root `provenance` package is the composition boundary. It exposes the public
interfaces and wires the type, SQLite, graph, model-catalog, and optional DBOS
components together.

## Public API And Types

`Tracker` is the main read and resource-lifecycle interface. `OpenSQLite` creates
a file-backed tracker and `OpenMemory` creates an ephemeral tracker. Both use the
same SQLite implementation and schema.

Public types are defined in `pkg/ptypes` and re-exported from the root package as
Go aliases. This split avoids an import cycle: `internal/sqlite` can depend on
the types without importing the root package, while callers only need to import
`github.com/dayvidpham/provenance`.

IDs combine a namespace with a UUIDv7. Closed state fields use typed enums.
External string contracts, such as model providers, use named string types with
validation rather than untyped strings.

## Domain And Lineage Model

The domain follows the three PROV-DM categories:

- Tasks are entities and work products.
- Human, ML, and software actors are agents represented with a table-per-type
  hierarchy.
- Activities are bounded actions performed by agents.

Typed edges record dependencies and lineage. `blocked_by` edges form the only
scheduling graph and must remain acyclic. Other edge kinds record attribution,
generation, derivation, supersession, and discovery without affecting readiness.

Labels and comments annotate tasks. They are also journaled mutations, so their
authors, authority, and position in history remain queryable.

## Reads, Mutations, And Authority

`Tracker` serves reads such as `Show`, `List`, `Ready`, `Blocked`, lineage
queries, actor lookup, and activity lookup. Mutations use a `Session` obtained
with `Tracker.As(actor, authority)`.

A session binds the committing actor and governing authority once. Its task
lifecycle, edge, label, and comment verbs build typed effects and commit one
logical operation through the journal. `Session.Atomic` groups multiple effects
into one transaction when they must succeed or fail together.

Task status follows a static state machine. `Start`, `Stop`, `CloseTask`, and
`Reopen` are distinct lifecycle events; metadata-only `Update` cannot change
status or ownership. Forced transitions are explicit, remain authorized, and are
recorded in history rather than bypassing the journal.

Every new journaled call receives an operation ID. Callers that may retry after
an ambiguous response must pin and reuse a stable `OperationID`. Session methods
that accept `ApplyOption` support `WithOperationID` directly. The edge, label,
and comment convenience methods do not accept options; retry-safe callers use
`Session.Atomic` with `WithOperationID`, or construct a direct
`JournalAPI.Apply`. An exact retry returns the committed result; reuse with
changed canonical input returns a typed conflict.

## Global Journal And Projections

Provenance owns one append-only global journal. Database-generated `JournalID`
is the sole causal order. `RecordedAt` is audit and display metadata and never
determines authorization, replay order, or lifecycle order.

The journal uses class-table inheritance: a common row records order and
attribution, while exactly one typed subtype records an operation, task event,
authority event, decision, or evidence event. Produced rows point to their
operation anchor, so an operation's closure is reconstructed rather than copied
into a second ledger.

Apply validates authorization and operation invariants, inserts the journal
rows, and passes each produced effect row through the shared projection fold in
one SQLite transaction. `ReplayProjections` reads the journal in `JournalID`
order, runs that same projection fold against shadow tables, and compares the
result with the stored live projections. `VerifyIntegrity` checks
supertype/subtype and attribution invariants. Startup and legacy migration fail
closed when schema or history cannot be interpreted safely.

Authority is itself journaled. Genesis establishes the first bootstrap
authority; assignment episodes govern later responsibility. Authorization is
checked per effect at the operation's journal position, not inferred from graph
edges or current wall-clock time.

## SQLite Storage

`internal/sqlite` owns schema management, SQL, transactions, journal reduction,
projection reads, replay, and migration. SQL uses the pure-Go zombiezen driver.
Stable data values are bound as parameters; typed selectors choose among the few
legitimate query shapes where SQL identifiers cannot be bound.

The database runs in WAL mode. A process-local write lock serializes operations
that must observe and update journal state atomically, while SQLite provides
cross-connection transaction isolation. Collections and wire fields are bounded,
and external or persisted input is validated before it reaches mutation logic.

`OpenBorrowedSQLite` supports a DBOS-owned, file-backed `database/sql` handle. A
separate zombiezen connection opens the same file and WAL because the two drivers
cannot share a connection object. DBOS retains lifecycle ownership; Provenance
uses the borrowed handle as a liveness sentinel, closes only its bridge
connection, and rejects pathless or in-memory borrowed databases.
Provenance accepts the caller-owned `database/sql` pool exactly as supplied and
does not validate or mutate its limits. Applications should size that pool for
their own DBOS readers and durable background work; the integration harnesses
use 16 open connections as a realistic recommendation, not a required minimum.
Smaller deliberate pools remain supported. WAL remains single-writer regardless
of pool size.
Every borrowed public write checks that lifecycle sentinel and then executes its
inner operation exactly once. SQLite's `busy_timeout=5000` is the sole local
contention wait; Provenance adds no Go sleep or retry loop. Direct writes return
an escaped `BUSY` or `LOCKED` error unchanged after SQLite's wait.

## Dependency Graph

`internal/graph` adapts SQLite task and edge storage to
`dominikbraun/graph`. `internal/helpers` implements ancestor and descendant
traversal over that adapter. The relational store remains authoritative; the
graph layer does not maintain a second persistent copy.

Only `blocked_by` participates in readiness and cycle checks. `Ready` and
`Blocked` use relational queries, while `DepTree`, `Ancestors`, and `Descendants`
provide graph-shaped views of the same persisted edges.

## Model Catalog

`ModelRegistry` abstracts the ML model catalog used to validate ML-agent
registration and seed SQLite reference data. `DefaultModelRegistry` adapts
`bestiary.Models()` as the source of truth. Tests and applications can inject a
different registry with `WithModelRegistry` without replacing storage or tracker
logic.

The public `Provider` is string-typed to align with bestiary. SQLite preserves
normalized relational keys by resolving provider names through its reference
table at the database boundary.

## Package Boundaries

| Package | Responsibility |
|---|---|
| `provenance` | Public API, tracker composition, sessions, DBOS adapter, model adapter, and type re-exports |
| `pkg/ptypes` | Public domain types, IDs, enums, model interfaces, and sentinel errors |
| `pkg/namespace` | Namespace derivation and normalization |
| `internal/journal` | Journal types, status FSM, effect families, canonical mutation codec, and replay model |
| `internal/sqlite` | Schema, SQL, atomic reducer, projections, replay, and migration |
| `internal/graph` | SQLite-backed graph-store adapter |
| `internal/helpers` | Graph traversal algorithms |
| `internal/testcorpus` | Declarative adversarial-history execution and assertions |

Dependency direction points inward toward types and contracts. Internal packages
do not import the root package, and consumers should not depend on implementation
packages.

## DBOS Durable Execution

DBOS is optional. Standalone trackers commit through `JournalAPI.Apply`; callers
that need crash-resumable execution use `DBOSAdapter.Apply`. Both paths converge
on the same journal reducer and SQLite projections.

### Durable Mutation Path

```text
Caller
  |
  | OperationInput
  v
Canonical journal codec
  |  validates and normalizes effects
  |  derives canonical mutation bytes and digest
  v
Provenance DBOS adapter
  |  adds closed unversioned operation context and workflow identity
  v
DBOS workflow and step
  |  checkpoints input and outcome
  v
SQLite journal reducer
```

Each layer has a separate responsibility:

- The Provenance API expresses the caller's requested operation using domain
  types.
- The canonical journal codec defines which fields constitute the logical
  mutation and produces its stable byte representation.
- The DBOS adapter transports that representation through durable workflow
  history and controls replay.
- The SQLite reducer commits the mutation and reconstructs its journal-anchored
  result.

DBOS decides when a workflow runs, resumes, or returns a checkpoint. It does not
define which Provenance fields are logically significant or how a domain error
must behave after recovery. Those are Provenance contracts.

### Why Provenance Owns The Codec

Passing ordinary Go values through DBOS serialization would make persisted
history depend implicitly on the current struct layout and DBOS serialization
behavior. A field rename, added metadata field, or dependency upgrade could then
change retry identity or make an old checkpoint unreadable.

The explicit codec instead provides these guarantees:

- Logically identical mutations have identical canonical bytes.
- A change to any canonical operand changes the mutation digest.
- Operation-level audit time (`OperationInput.RecordedAt`) remains persisted
  without changing logical retry identity. A per-effect
  `RecordedAtOverride` is a canonical operand and does change identity.
- Unknown schemas, variants, fields, duplicate fields, trailing data, and
  oversized values fail closed before domain writes.
- The single supported DBOS contract fails closed on every other schema.
- Provenance domain failures retain their typed `errors.Is` and `errors.As`
  behavior after DBOS recovery.

The codec is therefore not an alternative workflow engine. It is the stable
domain boundary used by the DBOS workflow engine.

### Input Formats

`DBOSApplyInput` is the sole supported write and recovery format. It contains:

- A closed outer schema tag.
- A bounded, ordered context frame containing operation identity, actor,
  authority, command digest, and audit timestamp.
- The canonical mutation bytes produced by the journal codec.

The mutation digest is derived from the canonical bytes. A caller-supplied
digest is never authoritative. Durable identity strings are unversioned: the
active contract had no persisted consumers or records when version suffixes were removed.
The sole authority is the const block in `dbos_contract.go`. There is no
compatibility decoder, second workflow registration, or migration path.

### Workflow And Retry Identity

Step retry options (`DBOSStepOptions` nested in `DBOSAdapterConfig`) set the
bounded retry policy: zero fields resolve to 3 retries, 50 ms base interval,
and factor 2. Valid nonzero overrides are validated and translated to the pinned
v0.16 options before registration.

Each DBOS step callback makes one borrowed journal attempt. SQLite first owns
its bounded local wait through `busy_timeout=5000`; if `BUSY` or `LOCKED` still
escapes, the adapter leaves it on the Go-error channel and DBOS consumes one
configured durable retry. Provenance never nests a Go sleep/retry loop inside a
DBOS attempt.

The adapter derives one workflow identity from its captured unversioned contract,
application version, and canonical `OperationID`. It separately derives the step
and input-collision fingerprint from the complete actor, authority, command, and
canonical mutation contract. This creates two important retry outcomes:

- The same operation with the same canonical mutation attaches to the durable
  execution and returns its checkpointed result without invoking the mutation
  callback again.
- The same operation with a changed canonical operand fails as a typed conflict
  before another workflow performs domain writes.

Crash recovery therefore does not rely on the caller remembering whether it
received the previous response. The durable history and journal determine the
answer.

### Outcome Format

`DBOSStepOutcome` is a closed checkpoint format with exactly one success or
failure variant.

A success contains journal-anchored state: the operation's anchor journal row,
emitted task-event rows, and slot-to-produced-row bindings. A domain failure is
encoded as a closed `ApplyFailureKind` string (a stable durable wire enum)
plus its actionable details. Exactly one descriptor match checkpoints the
domain failure; zero matches preserve the original Go error, while multiple
matches return a typed ambiguity with stable contract-order evidence on DBOS's
retryable Go-error channel. Descriptor constructors return fresh values, so no
caller can mutate shared classification authority. Unknown discriminators and failure records whose nested
operationID differs from the outer outcome fail closed.

This explicit failure representation is necessary because the pinned DBOS
version serializes an ordinary Go step error as text, which loses its concrete
type. Provenance returns genuine DBOS infrastructure failures through the DBOS
error channel. A positive closed classifier checkpoints only known typed
Provenance domain failures; SQLite operational errors and unclassified failures
remain DBOS Go errors so the current step can consume its bounded retry budget.
After canonical input validation, the adapter performs one bounded strict public
DBOS lookup before journal preflight. A terminal ERROR returns immediately; only
absent or nonterminal state proceeds to journal replay validation and full-input
collision checks.
The configured step budget is terminal: after `1 + MaxRetries` failed attempts,
DBOS records the sole infrastructure `ERROR` authority. The adapter returns a
typed `DBOSDiagnosticError` carrying operation/workflow identity and the DBOS
error code. Diagnostic class, field, and stage use one closed constant vocabulary;
dynamic context-frame positions are reported separately as numeric positions.
`ApplyWaitCanceledError` and `CheckpointDivergenceError` retain their concrete
error APIs while using the same typed stage vocabulary. The DBOS workflow ID is
derived only from the captured contract, application version, and canonical
`OperationID`; the full canonical input fingerprint remains the durable step and
input-collision guard. Replaying the same operation, including changed input under
the same terminal ID, reads that terminal error without fold
callbacks or durable writes. It does not resume, fork, create a recovery
attempt, checkpoint a domain failure, or write a second infrastructure ledger;
after repair, genuinely new work must use a new `OperationID`.

## Verification Strategy

Integration tests exercise the public production paths against real SQLite. The
journal corpus supplies valid and adversarial histories to both apply and replay,
which prevents the incremental and reconstruction paths from silently diverging.

The DBOS suite treats persisted formats and retry behavior as architecture:

- Strict typed YAML corpora under `testdata/contract` pin independently authored
  context bytes, canonical mutation bytes, mutation digests, fingerprints,
  complete decoded semantics, malformed frames, the closed failure outcome wire,
  and the shared exhaustive retry baseline with an exact typed 88-target bijection.
- Corpus loaders reject unknown YAML fields, trailing documents, duplicate or
  empty names, invalid classification/provenance, incomplete mutation metadata,
  and values outside closed symbolic memberships.
- Family and exhaustive retry tests cover every canonical mutation operand.
- Replay tests require zero callbacks and zero writes for completed work.
- Crash-gap tests exercise failures around domain commit and DBOS checkpoints.
- Race tests cover simultaneous exact and changed retries.
- Compile-fail tests prevent callers from bypassing the adapter with raw DBOS
  options.

Relevant implementation files are `dbos_wire.go`, `dbos_outcome.go`, and
`dbos_adapter.go`. Canonical mutation encoding is owned by
`internal/journal/canonical_mutation.go`.
