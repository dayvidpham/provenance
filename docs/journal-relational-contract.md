# Provenance global journal — relational contract

Status: normative. This document is the relational contract and proof corpus
specification for the global-journal architecture amendment tracked by
`dayvidpham/provenance#4`, `#5`, and `#6`. It supersedes the `TaskEvent`-only
schema described in the historical bodies
of those issues and in `dayvidpham/provenance#7`'s corpus harness for any
detail that conflicts with what follows. Existing implementation attempts on
prior `provenance-4--*`/`provenance-5--*`/`provenance-7--*` branches are
salvage input, not canonical schema; concrete regressions they exposed are
cited by name throughout as motivation for specific design choices, and are
carried forward as named adversarial histories in the proof corpus (see
[Adversarial proof corpus](#adversarial-proof-corpus)).

This document does not implement anything. It defines every relation, its
attributes and typed-ID domains, its functional dependencies and candidate
keys, its chosen primary/foreign keys, and — where a naive design would
violate BCNF — the decomposition that resolves it. Implementation
(`dayvidpham/provenance#4`'s journal base, `#5`'s operations/authority, `#6`'s
DBOS adapter) consumes this document; it does not restate or reinterpret its
own schema.

## 1. Architectural principle

Provenance owns exactly one global, append-only, database-generated,
integer-ordered journal. Every persisted fact about task/domain history,
committed operations, authority lifecycle, decisions, and material-work
evidence is a row in this journal or in a typed subtype table keyed by the
journal row it extends. There is no second ordering domain, no second
validation reducer, and no redundant closure list stored beside the journal.

Three doctrines govern every relation below:

1. **`JournalID` is the sole canonical order.** It is a database-generated
   `INTEGER` surrogate key. Every query, authorization check, and replay
   decision orders by `JournalID`, never by `RecordedAt`.
2. **`RecordedAt` is audit/display metadata only.** It never establishes
   causality, authorization, lifecycle order, or migration replay order. Two
   journal rows may share an identical `RecordedAt` (clock granularity,
   concurrent commits, or honestly-replayed legacy timestamps); `JournalID`
   still totally orders them.
3. **Common fields live on the supertype exactly once.** No subtype table
   repeats `JournalKind`, `ActorID`, or `RecordedAt`.

Typed-ID domains used throughout (Go-level types; SQL storage type in
parentheses): `TaskID`/`ActorID`/`ActivityID`/`CommentID` (`TEXT`,
`namespace--uuidv7`, unchanged from the existing schema — `AgentID` remains a
deprecated source alias for `ActorID`, identical wire format), `JournalID`
(`INTEGER`), `OperationID` (`TEXT`, caller-supplied), `OperationAuthorityID`
(`TEXT`, opaque), `AssignmentID` (`TEXT`, opaque), `EventKind` /
`ContextKind` / `DecisionKind` / `EvidenceKind` (`TEXT`, namespaced
`provenance.foo.bar` / `pasture.foo.bar` strings, open/extensible, validated
but not FK-enforced against a closed lookup table), `CommandDigest` /
`MutationDigest` / `ContentDigest` (`BLOB`, opaque digests), `GitOID`
(`TEXT`, 40- or 64-char lower-case hex).

A relation-naming convention is used to keep the discriminator visible: every
table that is a typed subtype of the journal is named `journal_<family>`, and
its primary key is always `JournalID`, a foreign key into `journal`. This is
class-table inheritance: one row in `journal`, at most one matching row in
exactly one subtype table selected by `JournalKind`.

## 2. Journal supertype

### 2.1 `journal`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | `INTEGER` | no | database-generated (`AUTOINCREMENT`), PK |
| `JournalKind` | FK → `journal_kinds.id` | no | discriminator: `operation` \| `task_event` \| `authority` \| `decision` \| `evidence` |
| `ActorID` | FK → `agents.id` (ActorID domain) | no | the actor whose committed operation produced this row |
| `RecordedAt` | `INTEGER` (UnixNano, UTC) | no | audit/display metadata only — see [§7](#7-recordedat-doctrine) |
| `ProducedByOperationJournalID` | FK → `journal_operations.JournalID` | yes | the operation that produced this row; NULL **only** on the row that is itself the operation anchor (`JournalKind = 'operation'`) — see [§5](#5-totality-rules) |

**Functional dependencies:** `{JournalID} → {JournalKind, ActorID, RecordedAt, ProducedByOperationJournalID}`. `JournalID` is a surrogate with no other functional source (two rows may legitimately share identical `(JournalKind, ActorID, RecordedAt)`, e.g. a timestamp collision or two same-actor events in the same batch).

**Candidate keys:** `{JournalID}` only.

**Primary key:** `JournalID`.

**Foreign keys:** `JournalKind → journal_kinds(id)`; `ActorID → agents(id)`; `ProducedByOperationJournalID → journal_operations(JournalID)`.

**BCNF:** the only candidate key is the whole key on the only nontrivial FD's left side, so `journal` is trivially in BCNF — there is no room for a violation with a single-attribute key and no other candidate key.

This one column — `ProducedByOperationJournalID` — is the structural fix for
the salvage regression where a superseded implementation stored operation
result closure as `committed_operations.event_ids_json`, a JSON list that
could drift from the actual `task_events.operation_id` foreign keys (caught
by that implementation's own `TestIntegratedGateRejectsExactSchemaAndFullEventClosureDrift`).
Under this contract there is exactly one place an operation's effect closure
is recorded — this foreign key — so drift between "what the operation says it
produced" and "what actually exists" is structurally impossible, not merely
checked. An operation's full result is the query
`SELECT * FROM journal WHERE ProducedByOperationJournalID = :anchor ORDER BY JournalID`,
joined out to each row's typed subtype table by `JournalKind`.

### 2.2 `journal_kinds` (closed lookup)

`(id INTEGER PK, name TEXT UNIQUE NOT NULL)`, seeded with exactly:
`operation`, `task_event`, `authority`, `decision`, `evidence`. FD `{id} ↔
{name}` (both directions since `name` is `UNIQUE NOT NULL`); two candidate
keys `{id}`, `{name}`; PK `id`; trivially BCNF (single non-key attribute,
determined by either key, no other dependency). This is the same closed-enum
shape as the existing `statuses`/`priorities`/`task_types`/`edge_kinds`
tables and is not repeated in detail for the other closed-enum lookup tables
introduced below (`authority_kinds`, `assignment_slots`,
`assignment_transitions`): each has identical shape and identical BCNF
argument, differing only in which values are seeded.

## 3. Committed operations

### 3.1 `journal_operations`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal.JournalID` | no | PK; the operation's own anchor row |
| `OperationID` | `TEXT` | no | caller-supplied alternate key, `UNIQUE` |
| `AuthorityJournalID` | FK → `journal_authorities.JournalID` | no | the authority this operation executed under |
| `CommandDigest` | `BLOB` | no | opaque caller-domain digest of the closed command |
| `MutationDigest` | `BLOB` | no | Provenance-derived structural digest, independent of `CommandDigest` |

**Functional dependencies:** `{JournalID} → {OperationID, AuthorityJournalID, CommandDigest, MutationDigest}` and, since `OperationID` is `UNIQUE NOT NULL` and this table has exactly one row per operation, `{OperationID} → {JournalID, AuthorityJournalID, CommandDigest, MutationDigest}`.

**Candidate keys:** `{JournalID}`, `{OperationID}`.

**Primary key:** `JournalID` — consistent with the class-table-inheritance convention used across every journal subtype. `OperationID` is the alternate key: it is **never** the primary key and is **never** an ordering source (see [§6](#6-operationid-doctrine)).

**Foreign keys:** `JournalID → journal(JournalID)`; `AuthorityJournalID → journal_authorities(JournalID)`.

**BCNF:** with two single-attribute candidate keys and no non-key attribute functionally determining a proper subset of the other key's attributes, `journal_operations` is in BCNF without decomposition.

`(ActorID, AuthorityJournalID, CommandDigest, MutationDigest)` — the *exact
replay identity* — is **not** a schema-level uniqueness constraint on this
table; it is an equality check the reducer performs at lookup time against
whatever row already carries the proposed `OperationID` (see
[§9.4](#94-idempotent-replay-short-circuit)). Declaring it `UNIQUE` would
incorrectly forbid two unrelated operations from coincidentally producing
identical digests under different `OperationID`s, which is not itself an
error.

## 4. Authority lifecycle

### 4.1 `authority_kinds` (closed lookup)

Values: `bootstrap`, `assignment`. Same shape/BCNF argument as `journal_kinds`.

### 4.2 `journal_authorities`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal.JournalID` | no | PK |
| `AuthorityKind` | FK → `authority_kinds.id` | no | `bootstrap` \| `assignment` |
| `OperationAuthorityID` | `TEXT` | no | opaque alternate key, `UNIQUE`, used as `MutationContext.Authority` |

**FDs:** `{JournalID} → {AuthorityKind, OperationAuthorityID}`; `{OperationAuthorityID} → {JournalID, AuthorityKind}`.

**Candidate keys:** `{JournalID}`, `{OperationAuthorityID}`. **PK:** `JournalID`. **BCNF:** trivial (single non-key attribute, determined identically by either key).

### 4.3 `journal_authority_bootstraps`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal_authorities.JournalID` | no | PK |
| `Label` | `TEXT` | no | operator-facing bootstrap identity name |

**FD:** `{JournalID} → {Label}`. **Candidate key / PK:** `{JournalID}`. **BCNF:** trivial (single key, single non-key attribute).

### 4.4 Assignment lifecycle — a worked BCNF decomposition

A naive single-table design for "who holds which responsibility slot on
which task" looks like this, folding every lifecycle transition (start, end)
into one row per occupancy episode:

```
journal_authority_assignments_NAIVE(
    JournalID PK,
    AssignmentID,      -- stable episode identity
    Transition,         -- 'started' | 'ended'
    TaskID,
    SlotID,
    ActorID,
    PredecessorAssignmentID
)
```

This table has two candidate keys: `{JournalID}` (every transition is its own
journal row, so it always gets a fresh `JournalID`) and `{AssignmentID,
Transition}` (an episode has at most one `started` row and at most one `ended`
row, enforced by `UNIQUE(AssignmentID, Transition)`).

It is **not** in BCNF. `TaskID`, `SlotID`, `ActorID`, and
`PredecessorAssignmentID` are invariant for the whole episode — a
responsibility-assignment episode cannot change which task, slot, or actor it
concerns between its `started` and `ended` rows; a change of task/slot/actor
is by definition a **new** episode (a transfer), not a mutation of the old
one. That gives the nontrivial FD

```
{AssignmentID} → {TaskID, SlotID, ActorID, PredecessorAssignmentID}
```

whose left side, `{AssignmentID}` alone, is **not** a superkey of this table
(the real candidate key needs `Transition` too). This is a textbook BCNF
violation: a determinant that is a proper subset of a candidate key.

**Decomposition** (lossless, dependency-preserving — `AssignmentID` is a
candidate key of the first table below, so it is a superkey of the split,
which guarantees a lossless join per the standard BCNF decomposition
theorem):

**`journal_authority_assignment_episodes`**

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `AssignmentID` | `TEXT` | no | PK — stable identity of one occupancy episode |
| `TaskID` | FK → `tasks.id` | no | the task the responsibility slot is on |
| `SlotID` | FK → `assignment_slots.id` | no | e.g. `owner-responsibility`; extensible for future slots |
| `ActorID` | FK → `agents.id` | no | the actor holding (or having held) the slot for this episode |
| `PredecessorAssignmentID` | FK → `journal_authority_assignment_episodes.AssignmentID`, self | yes | the episode this one succeeds, for a CAS transfer; `UNIQUE` — see [§8.2](#82-single-consumption-ownership-assignment-evidence) |

FD: `{AssignmentID} → {TaskID, SlotID, ActorID, PredecessorAssignmentID}`.
Candidate key / PK: `{AssignmentID}`. BCNF: trivial — single key, and it is
literally the whole-episode identity, so every attribute is properly
dependent on it and on nothing smaller.

**`journal_authority_assignment_transitions`**

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal_authorities.JournalID` | no | PK |
| `AssignmentID` | FK → `journal_authority_assignment_episodes.AssignmentID` | no | which episode this transition belongs to |
| `Transition` | FK → `assignment_transitions.id` | no | `started` \| `ended`; `UNIQUE(AssignmentID, Transition)` |

FDs: `{JournalID} → {AssignmentID, Transition}`; `{AssignmentID, Transition} →
{JournalID}`. Candidate keys: `{JournalID}`, `{AssignmentID, Transition}`. PK:
`JournalID`. BCNF: trivial — two keys, no other attribute, no non-key
determinant.

A "transfer" (CAS ownership change) is two writes inside one operation: an
`ended` transition on the old `AssignmentID`, and a `started` transition on a
**new** `AssignmentID` whose `PredecessorAssignmentID` is the old one. Nothing
about an episode row is ever updated in place after creation — the table is
append-only, matching the journal's own append-only discipline. "Current
occupant of slot `X` on task `Y`" is a *projection*, computed as: among
episodes for `(TaskID=Y, SlotID=X)` that have a `started` transition and no
`ended` transition, the one whose `started` transition has the greatest
`JournalID`. It is not stored as a mutable column anywhere.

### 4.5 `assignment_slots`, `assignment_transitions` (closed lookups)

`assignment_slots` seeds at minimum `owner-responsibility`, extensible for
future non-owner slots without a schema change. `assignment_transitions`
seeds exactly `started`, `ended`. Same shape/BCNF argument as §2.2.

## 5. Task and domain events

### 5.1 `journal_task_events`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal.JournalID` | no | PK |
| `TaskID` | FK → `tasks.id` | no | |
| `EventKind` | `TEXT` (namespaced, validated) | no | e.g. `provenance.task.created`, `pasture.review.recorded` |
| `Payload` | `TEXT` (`json_valid` CHECK) | no | opaque; Provenance validates envelope shape only, never caller-domain fields |

**FD:** `{JournalID} → {TaskID, EventKind, Payload}`. **Candidate key / PK:** `{JournalID}` — no other candidate key exists; many events legitimately share identical `(TaskID, EventKind)` (e.g. two `task.updated` events on the same task). **BCNF:** trivial.

`EventKind` is deliberately a validated namespaced `TEXT` column, not an FK
into a closed lookup table: Pasture and future callers define their own
kinds (`pasture.review.recorded`) that Provenance never enumerates in
advance. This is the same open/closed distinction the historical `#4` body
already drew, carried forward unchanged.

### 5.2 `journal_task_event_contexts`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `EventJournalID` | FK → `journal_task_events.JournalID` | no | part of PK |
| `ContextKind` | `TEXT` | no | part of PK; built-in (`task`/`activity`/`actor`/`git`) or caller-extension namespaced |
| `ContextIdentity` | `TEXT` | no | part of PK; encoded identity string |
| `AttachedByJournalID` | FK → `journal_task_events.JournalID` | no | the (possibly later) event that attached this edge |

**FD:** `{EventJournalID, ContextKind, ContextIdentity} → {AttachedByJournalID}`. One event may have many context edges attached by many different later events, so no proper subset of the key determines `AttachedByJournalID`. **Candidate key / PK:** the full triple. **BCNF:** trivial (single key). The row is written exactly once (`INSERT`, never `UPDATE`) — the first event to attach a given `(EventJournalID, ContextKind, ContextIdentity)` edge owns `AttachedByJournalID` permanently; a later attempt to attach the identical edge is a no-op, not a pointer move. This is what lets a snapshot query bound visibility by `AttachedByJournalID <= watermark` and get a reproducible answer regardless of when the read happens.

Context sets are canonicalized and deduplicated by `(ContextKind,
ContextIdentity)` at construction, exactly as in the salvage
`CanonicalEventContexts` implementation, which is carried forward unchanged
in behavior (validate-then-sort-then-dedup, sort key `(kind, identity)`
lexical order).

## 6. Decisions and material-work evidence

### 6.1 `journal_decisions`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal.JournalID` | no | PK |
| `DecisionKind` | `TEXT` (namespaced, validated) | no | e.g. `pasture.review.vote`, `pasture.uat.verdict` |
| `TaskID` | FK → `tasks.id` | yes | a decision need not be scoped to one task |
| `Payload` | `TEXT` (`json_valid` CHECK) | no | opaque, caller-validated |

**FD:** `{JournalID} → {DecisionKind, TaskID, Payload}`. **Candidate key / PK:** `{JournalID}`. **BCNF:** trivial. `DecisionKind` and `Payload` follow the same open/opaque doctrine as `EventKind`/`Payload` in §5.1 — Provenance records the fact of a decision; it never interprets Pasture's verdict vocabulary.

### 6.2 `journal_evidence`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal.JournalID` | no | PK |
| `EvidenceKind` | `TEXT` (namespaced, validated) | no | e.g. `pasture.git.commit`, `provenance.snapshot.created` |
| `TaskID` | FK → `tasks.id` | yes | |
| `ContentDigest` | `BLOB` | no | canonical digest of the evidence payload (e.g. a `GitOID` or content hash) |
| `Payload` | `TEXT` (`json_valid` CHECK) | no | opaque |

**FD:** `{JournalID} → {EvidenceKind, TaskID, ContentDigest, Payload}`. **Candidate key / PK:** `{JournalID}`. **BCNF:** trivial.

### 6.3 `immutable_tasks`

`CreateImmutableTaskSnapshot` (the generic snapshot command from the
historical `#5` body) is one `journal_evidence` row with `EvidenceKind =
'provenance.snapshot.created'` plus a marker row here:

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `TaskID` | FK → `tasks.id` | no | PK |
| `SnapshotEvidenceJournalID` | FK → `journal_evidence.JournalID` | no | `UNIQUE` |

**FDs:** `{TaskID} → {SnapshotEvidenceJournalID}`; `{SnapshotEvidenceJournalID} → {TaskID}` (one evidence row creates at most one immutable task). **Candidate keys:** `{TaskID}`, `{SnapshotEvidenceJournalID}`. **PK:** `TaskID`. **BCNF:** trivial.

The reducer consults this table's presence, not any status field, before
accepting an `update`/`status`/`phase`/`owner`/`assignment`/`close`/`reopen`
mutation against a `TaskID`: presence of a row rejects the mutation
outright. A snapshot task remains an ordinary `TaskID` for relationship
targets (edges, attribution, evidence) — only the seven listed mutation
families are closed off.

## 7. Actor domain

The existing `agents` / `agents_human` / `agents_ml` / `agents_software`
table-per-type hierarchy is carried forward unchanged in shape. `ActorID` is
the Go-level typed domain name over the same `namespace--uuidv7` wire format
already stored in `agents.id`; `AgentID` remains a deprecated source-level
alias for one coordinated release, not a second column or a second identity
domain. No schema change is required here beyond what §7.1–7.2 add.

### 7.1 `actor_namespace_claims`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `Namespace` | `TEXT` | no | PK |
| `ClaimantID` | `TEXT` | no | opaque claimant identity, e.g. `pasture-system` |
| `RangeMin` | `BLOB(16)` | no | inclusive lower bound of the claimed fixed-UUID range |
| `RangeMax` | `BLOB(16)` | no | inclusive upper bound |
| `Codec` | `TEXT` | no | the ordinal→UUID encoding this claimant uses within its range |

**FD:** `{Namespace} → {ClaimantID, RangeMin, RangeMax, Codec}`. **Candidate key / PK:** `{Namespace}`. **BCNF:** trivial.

A claim is idempotent only when a re-registration exactly matches the stored
`(ClaimantID, RangeMin, RangeMax, Codec)`; the `PRIMARY KEY(Namespace)`
constraint alone forces any differing re-registration to be an explicit
application-level conflict, not a silent overwrite — the reducer must
compare-then-reject, since SQL alone can express "at most one row per
namespace" but not "and it must match."

### 7.2 `fixed_actor_manifest_entries`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `ActorID` | FK → `agents.id` | no | PK |
| `Namespace` | FK → `actor_namespace_claims.Namespace` | no | |
| `ActorKind` | FK → `agent_kinds.id` | no | |
| `Name` | `TEXT` | no | `UNIQUE(Namespace, Name)` |
| `Metadata` | `TEXT` (`json_valid` CHECK) | no | |

**FDs:** `{ActorID} → {Namespace, ActorKind, Name, Metadata}` (globally unique); `{Namespace, Name} → {ActorID, ActorKind, Metadata}`. **Candidate keys:** `{ActorID}`, `{Namespace, Name}`. **PK:** `ActorID`. **BCNF:** trivial — two keys, no attribute outside either key determines a proper subset of the other.

**System namespace reservation.** Pasture claims `Namespace = 'pasture-system'`
via one `actor_namespace_claims` row (`dayvidpham/pasture#14`, historical `#5`
body). Its `Codec` deterministically maps small ordinals to UUIDs within
`[RangeMin, RangeMax]`; "actor IDs 0–1023 reserved, actor 0 is the default
Pasture system identity" (the amendment's shorthand) means ordinals 0–1023 of
that codec, with ordinal 0 pre-registered as a `fixed_actor_manifest_entries`
row before any other Pasture activity can reference it. Provenance's registry
here is generic — it stores a claimant, a range, and a codec name, never
Pasture's specific ordinal-to-name mapping — so `EnsureActorNamespaceManifest`
and `FixedActorSpec` (historical `#5` body) remain implementable directly on
top of these two relations without Provenance embedding any Pasture name or
semantics.

## 8. Projections

Projections are the *only* relations this contract allows to be read as
"current state" outside the journal itself. Every one of them must be a pure,
reproducible function of ordered journal history up to some watermark — see
[§9](#9-shared-reducer-contract) for the reducer contract that maintains
them.

### 8.1 `tasks` (existing table, reused as the current-state projection)

The existing `tasks` table (`id`, `status_id`, `priority_id`, `type_id`,
`phase_id`, `owner_id`, …) is retained verbatim as the current-task-state
projection, with one addition:

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `LastJournalID` | FK → `journal.JournalID` | no | the watermark this row's derived state reflects |

This column is not user-facing; it lets `Open`'s replay verify convergence
("does the stored projection match `Reduce(history, LastJournalID)`?") and
lets audit tooling confirm a row reflects journal history up to and including
`LastJournalID` and nothing beyond. `owner_id` on this table is written
**exclusively** by the shared reducer (§9), never by a direct `UPDATE`
outside `Apply`/`Open` — it is the "current occupant of the
`owner-responsibility` slot" projection described in §4.4, materialized here
for query convenience rather than recomputed from `journal_authority_assignment_episodes`
on every read.

### 8.2 `task_attributions`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `TaskID` | FK → `tasks.id` | no | part of PK |
| `ActorID` | FK → `agents.id` | no | part of PK |
| `FirstJournalID` | FK → `journal.JournalID` | no | earliest journal row establishing this actor's material contribution |

**FD:** `{TaskID, ActorID} → {FirstJournalID}`. **Candidate key / PK:** `{TaskID, ActorID}`. **BCNF:** trivial.

Append-only: a row is inserted the first time an actor materially
contributes to a task (an authority-assignment episode, a task event they
authored, evidence they attached) and is **never** deleted or overwritten
when `Task.Owner` later changes. `Task.Owner` (the current
`owner-responsibility` occupant, §8.1) and cumulative `task_attributions`
(every actor who ever materially contributed) are two independent
projections over the same journal history; they diverge by design once
ownership transfers.

### 8.3 `task_event_activity` (view, not a base relation)

The filtered, task-scoped read that `pasture task event list` serves is
`SELECT * FROM journal JOIN journal_task_events USING (JournalID) WHERE
TaskID = :id ORDER BY JournalID`. It carries no independent key beyond the
`JournalID` it inherits and requires no FD/BCNF analysis of its own — it is a
query, not a stored relation. Downstream code must not treat this filtered
view as the complete history: the global journal API additionally exposes
unfiltered ordered access across every `JournalKind`.

### 8.4 `assignment_current` (view, not a base relation)

"Who currently holds slot `S` on task `T`" is `SELECT episode.* FROM
journal_authority_assignment_episodes episode JOIN
journal_authority_assignment_transitions started ON started.AssignmentID =
episode.AssignmentID AND started.Transition = 'started' WHERE episode.TaskID
= T AND episode.SlotID = S AND NOT EXISTS (SELECT 1 FROM
journal_authority_assignment_transitions ended WHERE ended.AssignmentID =
episode.AssignmentID AND ended.Transition = 'ended') ORDER BY
started.JournalID DESC LIMIT 1`. Also a view; also carries no independent key.

## 9. Shared reducer contract

`Apply` (live mutation) and `Open` (startup replay) MUST call one reducer.
This is a specification constraint on every relation above, not an
implementation detail: the schema is shaped so that the reducer can be a
**pure** function of journal rows, with no other mutable input feeding it.

Let `Reduce(history, uptoJournalID) → ProjectionState` denote that function,
where `ProjectionState` is exactly the tuple of projections in §8
(`tasks.*` current-state columns, `task_attributions` rows, the
`assignment_current` view's underlying episode/transition rows) as of
`uptoJournalID`.

### 9.1 Determinism

`Reduce` depends only on the ordered sequence of `journal` rows (joined to
their typed subtype row) with `JournalID ≤ uptoJournalID`. No wall-clock
read, random value, or external service call may influence its output. Given
the same prefix, `Reduce` always returns the same `ProjectionState` — this is
what makes `LastJournalID` (§8.1) a meaningful convergence checkpoint.

### 9.2 Incrementality equals bulk-replay equivalence

For any `J1 ≤ J2`: replaying the whole prefix `history[..J2]` from empty
state produces the same `ProjectionState` as seeding `Reduce` with
`Reduce(history, J1)` and folding only `history[J1+1..J2]`. This licenses
`Apply` — which only ever sees one new operation's rows appended to prior
state — and `Open` — which replays the entire history after a restart — to
share one reducer implementation: `Apply`'s "state visible at the start of a
transaction" is `Reduce(history, J_before)`; it writes new rows, then the
same fold step produces `Reduce(history, J_after)`. `Open` is simply this
same fold applied from `J=0` in one pass. There is no second switch
statement over event kinds, no second closure-validation pass, and no
duplicated case list — this is the direct fix for the salvage regression
where a superseded implementation's `Apply` reducer switched on 43 `mutation.Mutation`
command-type branches while a wholly separate `Open`-time
`taskLifecycleProjection.apply` switched on 9 `journal.EventKind` branches,
requiring a long sequence of follow-up patches (evidenced by that
implementation's own commit history) to keep the two in sync.

### 9.3 Per-effect authority checkpoint

Before the reducer accepts a proposed effect row (a `journal_task_events`,
`journal_authority_*`, `journal_decisions`, or `journal_evidence` row) as
part of committing one operation, it evaluates that specific effect's
authority precondition against `Reduce(history, J_current - 1)` — every row
committed strictly before this effect, **including earlier effects of the
same in-flight operation**. Concretely: when one operation folds a batch of
$N$ effects inside a single transaction, the reducer must fold them one at a
time, re-deriving working state after each, and validate effect $k{+}1$
against that updated state — never against the snapshot taken once at the
start of the whole transaction. This is the fix for the salvage regression
where a single authority check gated an entire `ConditionalMutationBatch` and
its children reused that one already-validated authority value rather than
being independently re-authorized against their own emerging state — the
concrete case is worked out in
[`authority_evidence.yaml`](../testdata/contract/authority_evidence.yaml).

### 9.4 Idempotent replay short-circuit

If `journal_operations` already holds a row for the proposed `OperationID`,
the reducer compares the proposed operation's exact replay identity —
`(ActorID` from `journal`, `AuthorityJournalID, CommandDigest,
MutationDigest)` — against the stored row. An exact match short-circuits:
the effect-folding step in §9.3 is **not** invoked at all, and the already-committed
effect closure (`§2.1`'s `ProducedByOperationJournalID` query) is returned
unchanged. Any mismatch on the four-field identity is a typed conflict, never
a re-execution and never a partial write.

### 9.5 Fail-closed atomicity

If effect-folding fails at effect $k$ of $N$ — whatever the cause, including
injected faults — none of effects $1..k-1$ remain committed. The whole
operation's journal rows, including its own anchor row in `journal_operations`,
roll back as a single SQL transaction. This is unchanged from the existing
`STRICT`/transactional discipline already used across the live schema; the
journal model does not relax it.

## 10. Totality rules

1. Every committed logical operation produces exactly one `journal_operations`
   row, including an operation with zero task-domain effects (a pure
   authority-lifecycle operation, or an explicit zero-length
   `AppendTaskEventBatch`). The anchor row's own `journal.ProducedByOperationJournalID`
   is `NULL`; every other row it produces has that column set to the
   anchor's `JournalID`.
2. Every non-anchor journal row (`JournalKind ∈ {task_event, authority,
   decision, evidence}`) has exactly one producing operation —
   `ProducedByOperationJournalID` is `NOT NULL` and refers to exactly one
   `journal_operations.JournalID`.
3. Common fields (`JournalKind`, `ActorID`, `RecordedAt`) are never
   duplicated on a subtype row; a subtype row's only own attributes are the
   ones that do not already exist on the supertype.
4. `OperationID` is never a determinant of ordering and never a primary key
   anywhere in this schema (§3.1, §6).

## 11. OperationID doctrine

`OperationID` is a caller-supplied, globally unique, typed alternate key
(`journal_operations.OperationID UNIQUE NOT NULL`) whose sole purpose is
idempotency correlation across retries. It is:

- **not** the primary key of `journal_operations` (`JournalID` is, §3.1);
- **not** a source of ordering anywhere — ordering is exclusively `JournalID`;
- compared, not enumerated: the reducer's replay check (§9.4) looks up the
  single row for a given `OperationID` and compares four fields; it never
  scans or orders by `OperationID`.

Reusing an `OperationID` with a different `(ActorID, AuthorityJournalID,
CommandDigest, MutationDigest)` tuple is a typed conflict and commits
nothing.

## 12. RecordedAt doctrine

`RecordedAt` is copied from caller-supplied wall-clock time (or, during
legacy migration, from the historical row being migrated — §13) into
`journal.RecordedAt` for audit and display only. It is never:

- used to order query results (`JournalID` is, always);
- used to decide whether one journal row's effect happened "before" another
  for authority purposes (§9.3 uses `JournalID`, always);
- used to decide replay/migration order (§13 uses a documented deterministic
  pre-migration sort, not the migrated timestamps, precisely because two
  legacy rows can carry identical or misordered timestamps).

Two journal rows sharing an identical `RecordedAt` is expected, not an error;
`JournalID` still totally orders them. See
[`ordering.yaml`](../testdata/contract/ordering.yaml) for the adversarial
cases pinning this.

## 13. Legacy-baseline semantics

Migrating a pre-journal database installs one `journal_operations` anchor per
existing task, in a deterministic pre-migration order (legacy `created_at`
ascending, then legacy `id` ascending — a documented, reproducible sort over
the *pre-migration* table, independent of any journal content, since none
exists yet). For each legacy task, in that order:

1. One `journal_task_events` row, `EventKind = 'provenance.task.migrated'`,
   `RecordedAt` set to the legacy row's own `updated_at` — **never** the
   migration's own wall-clock time. There is no second timestamp column
   anywhere in this schema for a baseline row to accidentally use instead
   (unlike a superseded implementation, which additionally stored a
   `committed_operations.committed_at` populated with `time.Now()` beside the
   event's honestly-migrated `recorded_at` — that second column does not
   exist here, so there is no place left for a wall-clock value to leak into
   a supposedly historical record).
2. If the legacy task has a non-`NULL` owner: one
   `journal_authority_assignment_episodes` row (`SlotID =
   'owner-responsibility'`, `ActorID` = the legacy owner, `PredecessorAssignmentID
   = NULL`) and one `started` `journal_authority_assignment_transitions` row,
   `RecordedAt` = the same legacy `updated_at`.
3. If the legacy task's status is `Closed`: an additional `ended` transition
   row on that same episode, `RecordedAt` = the legacy `closed_at` if present,
   else the legacy `updated_at` — again never wall-clock `now`.
4. If the legacy owner does not resolve to a registered `ActorID` (an
   unmappable owner string, e.g. orphaned free-text left over from a
   pre-Provenance import): the **entire migration transaction** fails closed
   with an actionable error identifying the task and the raw owner value; no
   task is migrated with a synthesized or guessed `ActorID`, and no partial
   baseline is left committed for any other task in the same run.

Because the resulting baseline rows are ordinary `journal`/`journal_operations`/
`journal_authority_*` rows — not a special pre-journal state format — a
subsequent real transfer chains off a migrated episode exactly as it would
off any other episode (`PredecessorAssignmentID` pointing at the migrated
`AssignmentID`). There is no separate "final row" shortcut anywhere in this
schema for legacy state: `Reduce` (§9) replays baseline rows through the
identical fold used for every other row.

**External-schema preflight.** Before the migration transaction opens, the
migration routine verifies the pre-journal schema's exact expected shape
(table and column presence/absence). Any mismatch — including topology
corruption such as an unexpected extra column — fails closed with an
actionable diagnostic identifying what was expected and what was found,
before any row is written. See
[`topology_corruption.yaml`](../testdata/contract/topology_corruption.yaml).

## 14. Authority consumption rules

### 14.1 Per-effect authorization at the effect's own JournalID

Specified in §9.3: every consuming effect is authorized against
`Reduce(history, J_current - 1)`, the state visible strictly before that
effect's own journal position — including effects from earlier in the same
operation — never against a single pre-transaction snapshot reused for every
child of a batch.

### 14.2 Single-consumption ownership-assignment evidence

`journal_authority_assignment_episodes.PredecessorAssignmentID` carries a
`UNIQUE` constraint. A given episode can be cited as the predecessor of **at
most one** successor episode — two different new assignments cannot both
claim to succeed the same old one. This is the schema-level enforcement of
"multiply consumed evidence is rejected."

### 14.3 Orphaned evidence rejection

Referential integrity alone (the `PredecessorAssignmentID` foreign key)
guarantees the cited `AssignmentID` *exists*; it cannot express "and it has
an `ended` transition." That additional property is a reducer-level
business-rule check, layered on top of the FK: a `started` transition whose
episode names a `PredecessorAssignmentID` that has no `ended` transition row
(i.e. the predecessor is still active, or was never properly started) is
rejected before commit. This is documented here as a business rule the
schema cannot fully express structurally, precisely so the proof corpus
carries a dedicated negative case for it rather than relying on the FK alone
to appear sufficient.

## 15. Projection invariants

Restated from §8 for completeness: every projection (`tasks.*` current-state
columns, `task_attributions`, `task_event_activity`,
`assignment_current`) is reproducible **solely** from ordered `journal`
history — no projection may be seeded, patched, or reconciled from any
non-journal input. `tasks.LastJournalID` exists specifically so this claim is
checkable: `Open` may assert `stored_projection == Reduce(history,
stored_projection.LastJournalID)` for every task before accepting a database
as converged.

## Adversarial proof corpus

Concrete adversarial histories live under `testdata/contract/` as
`Corpus[I,E]` YAML, following the same shape as the salvage `#7` corpus
harness (`must-pass`/`must-fail` classification, non-empty `provenance:
{source, ref}`, one named `mutation: {description, operator}` per case) so
`dayvidpham/provenance#7`'s harness can load these files directly once
implemented. Every case's `provenance.ref` names a GitHub issue
(`provenance#4`/`#5`/`#6`) or a specific salvage regression by the test name
that first exposed it — never a Beads ID or a proposal/slice/phase label.

| File | Cases | Covers |
|---|---|---|
| [`ordering.yaml`](../testdata/contract/ordering.yaml) | 5 | Timestamp collisions; wall-clock-vs-`JournalID` divergence; regression (a) foundation |
| [`zero_event_operations.yaml`](../testdata/contract/zero_event_operations.yaml) | 5 | Zero-task-event operation anchoring; regression (b) |
| [`retry_reopen_cancellation.yaml`](../testdata/contract/retry_reopen_cancellation.yaml) | 5 | Retry/reopen/cancellation; regression (e) |
| [`authority_evidence.yaml`](../testdata/contract/authority_evidence.yaml) | 6 | Per-effect authority at each `JournalID`; orphaned/multiply-consumed evidence; regressions (a), (d) |
| [`owner_responsibility.yaml`](../testdata/contract/owner_responsibility.yaml) | 4 | Owner-responsibility end bound to legal close; regression (c) |
| [`baseline_migration.yaml`](../testdata/contract/baseline_migration.yaml) | 5 | Fresh/legacy-assigned/legacy-terminal/unmappable-owner baseline transitions; honest timestamps; regression (g) |
| [`topology_corruption.yaml`](../testdata/contract/topology_corruption.yaml) | 5 | Fail-closed on missing/corrupted external schema; regression (f) |

**Seven regression obligations, each with at least one named history**
(file → case name):

- (a) per-effect authorization at the effect's own journal position →
  `authority_evidence.yaml` / `effect-authorized-against-state-at-own-journalid`
- (b) logical ordering of zero-event operation authority →
  `zero_event_operations.yaml` / `zero-event-operation-orders-authority-not-recordedat`
- (c) owner-responsibility end bound to legal close →
  `owner_responsibility.yaml` / `task-close-without-ending-assignment-rejected`
- (d) responsibility-assignment semantics bound to explicit lifecycle records →
  `authority_evidence.yaml` / `orphaned-predecessor-assignment-rejected`
- (e) `Apply` aligned with lifecycle invariants via the shared reducer →
  `retry_reopen_cancellation.yaml` / `apply-and-open-converge-on-same-projection`
- (f) fail-closed on missing external schema dependencies →
  `topology_corruption.yaml` / `missing-journal-table-fails-closed-before-any-write`
- (g) honest, non-fabricated timestamps in bootstrap/migration repair →
  `baseline_migration.yaml` / `fabricated-wallclock-timestamp-in-baseline-rejected`

Coverage guards (non-vacuity, exact/minimum case counts, closed-enum
freshness against `journal_kinds`/`authority_kinds`/`assignment_transitions`)
and negative controls proving the harness itself fails on a vacuous,
truncated, stale, or unknown-operator corpus are `#7`'s responsibility to
wire up against these files; this document fixes their content, not their
runner.
