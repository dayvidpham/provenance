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
   `INTEGER` surrogate key. Every authorization check, replay, lifecycle, and
   convergence decision orders by `JournalID`, never by `RecordedAt`. Display
   listings may additionally be ordered by `RecordedAt` as a non-causal readable
   timeline (doctrine 2, §12.1), but no causal decision ever consults that order.
2. **`RecordedAt` is display/audit metadata — a readable timeline, never
   causality.** Its purpose is to present a readable timeline over what happened
   (§12.1), and the query surface exposes an `OrderByRecordedAt` display order for
   exactly that. It never establishes causality, authorization, lifecycle order,
   or migration replay order. Two journal rows may share an identical `RecordedAt`
   (clock granularity, concurrent commits, or honestly-replayed legacy
   timestamps); `JournalID` still totally orders them, and the timeline order
   breaks the tie by `JournalID`.
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

**Attribute naming (logical vs physical).** This contract names attributes by
their **Go-level type names in PascalCase** — `JournalID`, `ActorID`, `TaskID`,
`RecordedAt`, `ProducedByOperationJournalID` — for every relation. The **physical
SQL columns** use the repository's `snake_case` convention uniformly:
`journal_id`, `actor_id`, `task_id`, `recorded_at`,
`produced_by_operation_journal_id`, `kind_id`. The mapping is mechanical
(PascalCase attribute ↔ its `snake_case` column); in particular the journal
supertype's primary-key column is `journal_id`, consistent with its siblings
`kind_id` / `actor_id` / `recorded_at`. Illustrative DDL and SQL in this document
uses the logical PascalCase names for readability; the executable schema and every
query string use the `snake_case` columns.

## 2. Journal supertype

### 2.1 `journal`

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | `INTEGER` | no | database-generated (`AUTOINCREMENT`), PK |
| `JournalKind` | FK → `journal_kinds.id` | no | discriminator: `operation` \| `task_event` \| `authority` \| `decision` \| `evidence` |
| `ActorID` | FK → `agents.id` (ActorID domain) | yes | the actor whose committed operation produced this row; present **only** on an anchor row (`ProducedByOperationJournalID IS NULL` — an operation anchor, genesis, or migration baseline). A subordinate (operation-produced) row carries `ActorID` NULL and derives its actor from its anchor via the [§8.5](#85-journal_attributed-view-not-a-base-relation) self-join. `ActorID NOT NULL` **iff** `ProducedByOperationJournalID IS NULL`, enforced by a `CHECK` constraint and [§10 rule 5](#10-totality-rules) |
| `RecordedAt` | `INTEGER` (UnixNano, UTC) | no | audit/display metadata only — see [§7](#7-recordedat-doctrine) |
| `ProducedByOperationJournalID` | FK → `journal_operations.JournalID` | yes | the operation that produced this row; NULL **only** on the row that is itself the operation anchor (`JournalKind = 'operation'`) — see [§5](#5-totality-rules); **S1.1 staging note:** at the journal-base layer this column is uniformly NULL on `task_event` rows too (no `journal_operations` anchor exists yet to reference), so at that layer every `task_event` row is an anchor and legitimately carries `ActorID` — see the staging note after [§10 rule 2](#10-totality-rules) |

**Functional dependencies:** `{JournalID} → {JournalKind, ActorID, RecordedAt, ProducedByOperationJournalID}`. `ActorID` is stored on **anchor rows only** (those with `ProducedByOperationJournalID IS NULL`); a subordinate row's `ActorID` column is NULL, and its effective actor is *derived*, not stored — it equals its anchor's `ActorID`, reachable by the self-join `journal j → journal anchor ON anchor.JournalID = j.ProducedByOperationJournalID`. There is therefore no `{ProducedByOperationJournalID} → {ActorID}` data dependency held *within* `journal` at all: the determinant column is populated on exactly the rows whose `ActorID` is NULL, so the stored relation carries no repeated actor to depend on. `JournalID` is a surrogate with no other functional source (two rows may legitimately share identical `(JournalKind, ActorID, RecordedAt)`, e.g. a timestamp collision or two same-actor anchors in the same batch).

**Candidate keys:** `{JournalID}` only.

**Primary key:** `JournalID`.

**Foreign keys:** `JournalKind → journal_kinds(id)`; `ActorID → agents(id)`; `ProducedByOperationJournalID → journal_operations(JournalID)`.

**Committing-actor model.** The *committing* actor of an operation — the actor who executed the operation, not necessarily the domain subject of the effect — is recorded **once**, on the operation's anchor row. Every subordinate row the operation produces derives that same committing actor from the anchor (§8.5); it is never restamped on the produced row. A responsibility-slot occupant is carried separately on `journal_authority_assignment_episodes.ActorID` (§4.4) and may differ from the committing actor (a Pasture-system actor executing a transfer or a migration on behalf of an occupant, §13).

**BCNF.** The surrogate `{JournalID}` is the only candidate key, and every FD on `journal` — `{JournalID} → {JournalKind, ActorID, RecordedAt, ProducedByOperationJournalID}` — has that candidate key as its determinant, so **`journal` is in BCNF with no decomposition and no controlled redundancy**. This is the point of storing `ActorID` on anchor rows only: the earlier design that repeated each produced row's committing `ActorID` alongside its `ProducedByOperationJournalID` introduced the non-key FD `{ProducedByOperationJournalID} → {ActorID}` and the corresponding agreement obligation; removing the repeated column removes both. Actor–anchor *disagreement* is now structurally impossible — a subordinate row has no actor column to disagree with — rather than something a reducer invariant must forbid. The one placement invariant that remains (`ActorID NOT NULL` exactly on anchor rows) is expressed directly by a `CHECK` constraint and re-checked by [§10 rule 5](#10-totality-rules); its must-fail history is `authority_evidence.yaml` / `subordinate-row-carrying-actor-rejected`.

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
joined out to each row's typed subtype table by `JournalKind`. This flat,
ordered closure is exactly what reconstructs `CommittedExact.EmittedEvents`
(the non-slot-keyed `[]EventID`). The *slot-keyed* portion of a committed
result — `CanonicalMutationResult`'s caller-chosen local handles mapped to
concrete IDs — needs one further column that this closure does not carry (which
slot name the caller used for each produced row); that mapping is persisted by
`journal_operation_result_slots` (§3.2).

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
| `AuthorityJournalID` | FK → `journal_authorities.JournalID` | yes | the authority this operation executed under; NULL **only** on a genesis operation whose sole effect is producing one `bootstrap` authority — see [§4.6](#46-genesis-the-authority-base-case) |
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

### 3.2 `journal_operation_result_slots`

`LookupCommitted` must return, for a committed operation, its
`CanonicalMutationResult` — the caller-chosen local handle name for each thing
the operation allocated (`CanonicalTaskResult{Slot ResultSlotID; ID TaskID}`,
and its `Assignment`/`Activity`/`Event` siblings), mapped to the concrete ID it
resolved to. Canonical typed local handles let later mutations in one batch
reference earlier allocations, and the replay-stable result maps every handle to
its concrete ID. This mapping is **not** reconstructable from the §2.1
`ProducedByOperationJournalID` closure (which enumerates *which* rows an
operation produced but records no slot name), from the one-way
`CommandDigest`/`MutationDigest` (not reversible), or from `LookupCommitted`'s
own `OperationLookupIdentity{Actor, Authority, Command}` signature (which cannot
resupply the original slot list) — so it must be durably persisted at
Apply-commit time:

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `JournalID` | FK → `journal_operations.JournalID` | no | part of PK; the producing operation's anchor |
| `ResultSlotID` | `TEXT` | no | part of PK; the caller's local handle name (e.g. `new-task-1`) |
| `ProducedJournalID` | FK → `journal.JournalID` | no | the concrete produced row this slot resolved to |

**FD:** `{JournalID, ResultSlotID} → {ProducedJournalID}`. In addition, an
induced FD `{ProducedJournalID} → {JournalID}` holds over the whole relation,
*given* the own-operation integrity invariant below: `ProducedJournalID`
identifies a `journal` row, and that row's own
`ProducedByOperationJournalID` (§2.1) is fixed the moment the row is
produced, so once a slot row names a `ProducedJournalID`, the `JournalID`
determined by that produced row's `ProducedByOperationJournalID` is fixed
too — the same derive-`JournalID`-from-the-produced-row's-`PBOJID`
relationship used for the near-key FD in §4.4. This is a genuine functional
dependency, not merely a restated foreign key: it holds because of the
invariant, not because of the FK alone (the two FKs, `JournalID →
journal_operations` and `ProducedJournalID → journal`, are independently
satisfiable by a slot row whose `ProducedJournalID` points at a row produced
by some *other* operation).

**Own-operation integrity invariant (reducer-enforced):** a slot row's
`ProducedJournalID` must identify a `journal` row whose own
`ProducedByOperationJournalID` equals the slot row's own `JournalID` — i.e. a
result slot may only map to a row produced by *its own* operation, never to a
row produced by a different operation. Referential integrity alone (the two
FKs above) cannot express this — it guarantees `ProducedJournalID` and
`JournalID` each reference an *existing* row, not that they name the *same*
operation. Left unenforced, a slot row could point at another operation's
produced row, which would corrupt that operation's reconstructed
`CanonicalMutationResult` (the §3.2 reconstruction query filters
`WHERE JournalID = :anchor` and returns whatever `ProducedJournalID` sits in
that row, trusting it belongs to the anchor's own operation). This is a
reducer-level business rule the schema cannot express, in the same §14.3
pattern as orphaned-evidence rejection: checked and rejected before commit,
not merely assumed from the FKs. Codified as totality rule 9 (§10).

**Candidate key / PK:** `{JournalID, ResultSlotID}`. Whether `ProducedJournalID`
is itself a second candidate key depends on whether slot-aliasing (two
`ResultSlotID`s within one operation resolving to the same `ProducedJournalID`)
is permitted; this contract permits it (a caller may legitimately reference
the same allocated row under two local handles), so `ProducedJournalID` is
**not** `UNIQUE` and is **not** a second candidate key. Under that choice,
`{ProducedJournalID} → {JournalID}` is a nontrivial FD whose determinant is
not a superkey, pinned by the own-operation integrity invariant above rather
than decomposed away. This is the schema's **one** deliberate controlled
denormalization: unlike §2.1's actor column — which is now stored on anchor
rows only, so `journal` needs no controlled redundancy and no agreement
invariant at all — a result-slot row genuinely needs `JournalID` (its owning
operation anchor) present locally to key the slot, and the derived
`{ProducedJournalID} → {JournalID}` relationship cannot be normalized away
without losing that key. **BCNF:** not strictly satisfied on
`{ProducedJournalID} → {JournalID}` for the reason just given; accepted as
controlled redundancy with the required agreement enforced as the reducer
invariant of §10 rule 9 rather than a normal-form decomposition.

`CanonicalMutationResult` reconstructs as `SELECT ResultSlotID,
ProducedJournalID, JournalKind FROM journal_operation_result_slots JOIN journal
ON ProducedJournalID = journal.JournalID WHERE
journal_operation_result_slots.JournalID = :anchor`, bucketed into
`Tasks`/`Assignments`/`Activities`/`Events` by `JournalKind`. `EmittedEvents`
(the flat, non-slot-keyed ordered event list) needs no row here — it is fully
covered by the §2.1 `ProducedByOperationJournalID` closure. `operation_results.yaml`
carries the slot-reconstruction histories.

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
theorem; it is dependency-preserving because each original FD lands wholly
within one component — `{AssignmentID} → {TaskID, SlotID, ActorID,
PredecessorAssignmentID}` in the episodes table and `{AssignmentID, Transition}
→ {JournalID}` in the transitions table — and `{JournalID} →` the remaining
attributes is then recoverable by transitivity through those preserved FDs, so
no dependency is lost across the split):

**`journal_authority_assignment_episodes`**

| Attribute | Domain | Nullable | Notes |
|---|---|---|---|
| `AssignmentID` | `TEXT` | no | PK — stable identity of one occupancy episode |
| `TaskID` | FK → `tasks.id` | no | the task the responsibility slot is on |
| `SlotID` | FK → `assignment_slots.id` | no | e.g. `owner-responsibility`; extensible for future slots |
| `ActorID` | FK → `agents.id` | no | the actor holding (or having held) the slot for this episode |
| `PredecessorAssignmentID` | FK → `journal_authority_assignment_episodes.AssignmentID`, self | yes | the episode this one succeeds, for a CAS transfer; `UNIQUE` — see [§8.2](#82-single-consumption-ownership-assignment-evidence) |

FDs: `{AssignmentID} → {TaskID, SlotID, ActorID, PredecessorAssignmentID}`;
and, over rows with `PredecessorAssignmentID` NOT NULL, the near-key
partial-function dependency `{PredecessorAssignmentID} → {AssignmentID, TaskID,
SlotID, ActorID}` induced by the `UNIQUE(PredecessorAssignmentID)` constraint
(§14.2). A nullable `UNIQUE` column is not a candidate key in the strict
relational sense — NULLs are permitted and are not compared equal — so it adds
no candidate key and leaves BCNF unaffected. Candidate key / PK:
`{AssignmentID}`. BCNF: trivial — the sole candidate key `{AssignmentID}` is
literally the whole-episode identity; every FD determinant here is either
`{AssignmentID}` itself or the near-key `{PredecessorAssignmentID}`, and the
latter determines exactly the attributes a candidate key would, so no
determinant is a proper subset of a key.

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

### 4.6 Genesis: the authority base case

Totality rule 2 (§10) makes every non-anchor journal row cite a producing
operation, and §3.1 makes every operation cite an executing authority. Treated
as *universal* NOT NULL constraints these two rules have **no base case**: the
first authority could never be produced (its producing operation would itself
need a prior authority), and the first operation could never execute (it would
need a prior authority to already exist) — a genuine foreign-key cycle
(`journal.ProducedByOperationJournalID → journal_operations →
journal_operations.AuthorityJournalID → journal_authorities → journal`) that no
insert order satisfies under immediate FK checking. The contract breaks this
cycle with exactly one narrow base case, mirroring the
`ProducedByOperationJournalID` NULL-only-on-anchor pattern of §2.1:

- **A genesis operation** is a `journal_operations` row with
  `AuthorityJournalID = NULL`. It is the **only** row in the whole schema
  permitted a NULL `AuthorityJournalID`. Its sole permitted effect is producing
  exactly one `journal_authorities` row with `AuthorityKind = 'bootstrap'`
  (with its `journal_authority_bootstraps` detail row). It produces no task
  events, no assignment transitions, no decisions, and no evidence.
- The bootstrap authority the genesis operation produces is an ordinary effect
  row: its `journal.ProducedByOperationJournalID` is the genesis operation's own
  `JournalID`, so **totality rule 2 stays universal** — every authority,
  including the first, is produced by exactly one operation. Only
  `AuthorityJournalID`'s NOT NULL requirement is relaxed, and only for the
  genesis operation.
- **Legal insert order** (immediate FK checking; no deferred constraints
  required): (1) insert the genesis operation's `journal` anchor row
  (`ProducedByOperationJournalID = NULL`); (2) insert its `journal_operations`
  row (`AuthorityJournalID = NULL`); (3) insert the bootstrap authority's
  `journal` row (`ProducedByOperationJournalID` = the genesis operation's
  `JournalID`); (4) insert its `journal_authorities` and
  `journal_authority_bootstraps` rows. No foreign key is ever unsatisfied at
  insert time, so **no `PRAGMA defer_foreign_keys` or deferred-constraint
  mechanism is needed** — this is a stated schema property, not an incidental
  one.
- **A genesis operation is accepted only when no `journal_operations` row yet
  exists** — i.e. it is the first operation in the journal. This closes the
  obvious hole: were arbitrary NULL-authority operations allowed, any caller
  could mint a bootstrap authority under no authorization. Reducer invariants
  §10 rules 6–7 enforce the NULL-only-on-genesis, first-operation-only, and
  sole-bootstrap-effect rules.
- **Precedence vs. §9.4's idempotent short-circuit.** The genesis operation
  has an `OperationID` like any other operation, so a legitimate retry of it
  (e.g. a network retry of first-boot, same `OperationID`, arriving after the
  bootstrap authority already committed) is presented against a
  now-non-empty journal. §9.4's `OperationID`-presence check is evaluated
  **before** the "no `journal_operations` row yet exists" genesis-validity
  check above: if `journal_operations` already holds a row for the proposed
  `OperationID`, §9.4's four-field identity short-circuit fires first and
  returns the original bootstrap authority unchanged — the genesis-validity
  check above is never reached for that operation. The genesis-validity
  check only ever rejects a genesis presenting a **new** (not-yet-seen)
  `OperationID` against a non-empty journal — i.e. a distinct, illegitimate
  second bootstrap attempt, not an idempotent retry of the first one. See
  §9.4 and §10 rule 6 for the corresponding cross-references.
- **Migration (§13) executes under a bootstrap authority.** A migration run
  first establishes (or requires already-established) the `pasture-system`
  bootstrap authority via one genesis operation as above; every per-task
  baseline anchor operation it then writes is an ordinary non-genesis operation
  whose `AuthorityJournalID` is NOT NULL and equal to that bootstrap authority's
  `JournalID`. Migration never writes a NULL-authority operation except the
  single genesis operation, and never fabricates an authority.

`genesis_bootstrap.yaml` carries the must-pass genesis history, the
same-`OperationID` genesis-retry-hits-§9.4-short-circuit must-pass history, and
the must-fail histories (NULL authority on a non-genesis operation, a second
genesis against a non-empty journal, and a genesis producing a non-bootstrap
effect).

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

`ContextKind` mixes built-in values the reducer *interprets* for
identity-encoding validation (`task`/`activity`/`actor`/`git`) with open
caller-extension namespaces it records opaquely. The column is open-in-storage
but the built-in list is closed-in-behavior. The criterion that keeps
`JournalKind`/`AuthorityKind`/`Transition`/`SlotID` closed integer lookups
(§2.2) is precisely *whether the schema or reducer dispatches on the value*: an
open kind flows through Provenance opaquely, so it stays validated `TEXT`. If a
future slice ever adds an attribute that functionally depends on a specific
built-in `ContextKind` value — i.e. the reducer or schema begins to dispatch on
it beyond identity-encoding validation — that value set **graduates to a closed
integer lookup at that point**, by that same criterion.

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

### 7.3 Namespace-claim integrity (reducer rules)

Two properties the schema cannot express structurally are enforced by the
reducer, documented here per the §14.3 convention so the proof corpus carries a
dedicated negative case for each:

1. **Non-overlapping ranges.** A new `actor_namespace_claims` row whose
   `[RangeMin, RangeMax]` intersects the range of any existing claim is rejected
   before commit, with an actionable error naming **both** the new and the
   conflicting namespace. `PRIMARY KEY(Namespace)` prevents duplicate namespace
   *names* but cannot express range disjointness across different names, so an
   unchecked overlap would surface only later as an accidental
   `fixed_actor_manifest_entries`/`agents` primary-key collision, with no
   diagnostic tying it back to the two conflicting claims.
2. **Entry-in-range.** A `fixed_actor_manifest_entries` row's `ActorID` must
   decode, via its namespace's `Codec`, to an ordinal lying within that
   namespace's `[RangeMin, RangeMax]`; an entry whose `ActorID` falls outside
   its claimed range (or does not decode under the claimed codec) is rejected.
   The `Namespace` foreign key guarantees the claim *exists* but cannot express
   that the entry actually belongs inside the claimed range.

`actor_namespace.yaml` carries a must-fail overlap history and a must-fail
out-of-range entry history (plus a must-pass disjoint-claims history).

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

**S1.1 → S1.3 staging note.** At the journal-base layer (`dayvidpham/provenance#4`,
S1.1) `tasks.LastJournalID` ships **nullable**: the pre-journal direct-write
task-creation path predates the shared reducer, so existing and newly created
tasks have no watermark to populate until every task write is routed through
`Apply`/`Open` (§9). This is a deliberate staging gap, not a schema bug — the
column is tightened to `NOT NULL` (as stated above) by the shared-reducer
slice (`dayvidpham/provenance#5`, S1.3) once all task writes are
journal-anchored.

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

**Attribution function (per `JournalKind`).** Which `ActorID` a produced row
attributes is fixed per kind, not left to the implementer — the two candidate
actors (a row's *effective* committing actor versus, for an assignment episode,
the occupant `journal_authority_assignment_episodes.ActorID`) can differ, so
the choice is stated explicitly. Because a subordinate row no longer stores its
committing actor (§2.1), the "committing actor" a rule names below is the
**derived effective actor** — `COALESCE(journal.ActorID, anchor.ActorID)` via
the §8.5 anchor self-join, equivalently the `effective_actor_id` column of the
`journal_attributed` view — never a hand-read of the (NULL) subordinate
`journal.ActorID` column:

| Produced row kind | Attributed `ActorID` |
|---|---|
| authority assignment episode (its `started` transition) | the episode's occupant, `journal_authority_assignment_episodes.ActorID` — **not** the derived committing actor |
| `journal_task_events` | the row's derived committing actor (the authoring actor, per the committing-actor model of §2.1) |
| `journal_evidence` | the row's derived committing actor (the actor who attached it) |
| `journal_decisions` with a non-NULL `TaskID` | the row's derived committing actor |
| `journal_decisions` with `TaskID` NULL | no edge (attribution is per task; an untasked decision attributes nothing) |

The committing actor of an operation does **not** earn an attribution edge
merely for committing: a Pasture-system actor executing a transfer or a
migration on behalf of an occupant attributes the *occupant* (via the episode's
`ActorID`), never itself. `owner_responsibility.yaml` /
`attribution-credits-occupant-not-committing-system-actor` pins this
occupant-not-committer case.

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

### 8.5 `journal_attributed` (view, not a base relation)

The base `journal` relation stores each committing actor exactly once, on the
anchor row (§2.1). Because a subordinate row's `ActorID` is NULL, every consumer
that wants "which actor is this row attributed to" would otherwise have to
hand-write the anchor self-join and its `COALESCE`. `journal_attributed` is the
single **read-only, denormalized** surface that does it once, so no consumer
re-derives it:

```
CREATE VIEW journal_attributed AS
SELECT j.JournalID                        AS JournalID,
       j.JournalKind                      AS JournalKind,
       COALESCE(j.ActorID, anchor.ActorID) AS EffectiveActorID,
       j.RecordedAt                       AS RecordedAt,
       j.ProducedByOperationJournalID     AS ProducedByOperationJournalID
FROM journal j
LEFT JOIN journal anchor
  ON anchor.JournalID = j.ProducedByOperationJournalID
```

`EffectiveActorID` is `j.ActorID` on an anchor row (its own stored actor) and
the anchor's `ActorID` on a subordinate row (the committing actor it derives).
It is the consumer-facing attribution surface: internal query and attribution
reads that need a row's actor (§8.2, §8.3, the ordered query surface of §8.3/§12)
go through this view or the equivalent inline join, never through a bare read of
the subordinate `journal.ActorID` column. Like §8.3/§8.4 it is a keyless,
derived, reproducible projection — it carries no independent key beyond the
`JournalID` it inherits, stores nothing, and is a pure function of `journal`, so
it needs no FD/BCNF analysis of its own. It is `LEFT JOIN`, not `JOIN`, so an
anchor row (whose `ProducedByOperationJournalID` is NULL) still appears, with
`EffectiveActorID = j.ActorID`.

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
what makes `LastJournalID` (§8.1) a meaningful convergence checkpoint. This
determinism presupposes that `journal` rows already have a total order
(`JournalID`); for the effects *within* one not-yet-committed operation, which
have no `JournalID` assigned yet, that order is fixed by §9.3.1 before folding
begins.

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

### 9.3.1 Intra-operation effect order

§9.3 folds an operation's $N$ effects one at a time; the order in which they are
folded — which effect is $1$ and which is $k{+}1$ — is exactly the order the
caller's `Mutation`/command structure lists its child effects: an ordinal
array/slice position, fixed before folding begins. It is **never** re-derived
from Go map or set iteration order, from a sort over any non-ordinal field (a
`TaskID`, `SlotID`, digest, or `RecordedAt`), or from any other
non-deterministic source. This order fixes both each effect's relative
`JournalID` assignment and the state each subsequent effect's §9.3 authority
check is evaluated against, so it is a precondition of §9.1's determinism claim:
two independent constructions of the same logical operation must fold — and
therefore journal — their effects identically. An implementation that collected
an operation's effects into a Go map (e.g. keyed by `TaskID` or `SlotID`, a
natural shape given the keying elsewhere in this schema) before folding would
silently violate this rule under Go's randomized map iteration; this section is
the rule such an implementation violates.

### 9.4 Idempotent replay short-circuit

If `journal_operations` already holds a row for the proposed `OperationID`,
the reducer compares the proposed operation's exact replay identity —
`(ActorID` from `journal`, `AuthorityJournalID, CommandDigest,
MutationDigest)` — against the stored row. An exact match short-circuits:
the effect-folding step in §9.3 is **not** invoked at all, and the already-committed
effect closure (`§2.1`'s `ProducedByOperationJournalID` query) is returned
unchanged. Any mismatch on the four-field identity is a typed conflict, never
a re-execution and never a partial write.

**Precedence vs. genesis validity (§4.6, §10 rule 6).** This `OperationID`-presence
check is evaluated **before** any operation-kind-specific validity check,
including §4.6/§10 rule 6's genesis "no `journal_operations` row yet exists"
check. Concretely: a retry of the genesis operation itself (same
`OperationID`, arriving after the journal is no longer empty) hits this
short-circuit first and returns the original bootstrap authority; it never
reaches rule 6's genesis-validity check, so it is not rejected as a "second
genesis". Rule 6 only ever evaluates — and only ever rejects — a genesis
presenting a **new** `OperationID` against a non-empty journal.

### 9.5 Fail-closed atomicity

If effect-folding fails at effect $k$ of $N$ — whatever the cause, including
injected faults — none of effects $1..k-1$ remain committed. The whole
operation's journal rows, including its own anchor row in `journal_operations`,
roll back as a single SQL transaction. This is unchanged from the existing
`STRICT`/transactional discipline already used across the live schema; the
journal model does not relax it.

### 9.6 Concurrency contention point

Two concurrent `Apply` calls contend at a single serialization point: SQLite's
single-writer transaction semantics, under which at most one write transaction
commits at a time and the second-executing transaction observes the first's
already-committed rows. The contract's two "exactly one concurrent writer wins"
outcomes resolve there:

- **Transfer CAS** (`owner_responsibility.yaml` /
  `transfer-cas-single-winner-loser-gets-stale-conflict`). Both transfer
  operations validate their precondition against `Reduce(history, J_current − 1)`.
  Because writes serialize, the loser's transaction runs after the winner's has
  committed; its §9.3 precondition re-check now observes the winner's committed
  `ended` transition on episode A, finds A no longer active, and the reducer
  rejects it with a typed `stale-episode-conflict`, writing nothing. The
  `UNIQUE(AssignmentID, Transition)` and `UNIQUE(PredecessorAssignmentID)`
  constraints are a defence-in-depth backstop (a raw violation would surface only
  if two writers somehow both reached the write step), but the **primary,
  specified** mechanism is the serialized precondition re-check yielding the
  typed conflict — not a raw database error.
- **Concurrent same-new-`OperationID`** (§9.4, §11). Two callers may each look up
  the proposed new `OperationID`, each see no existing `journal_operations` row,
  and each attempt to insert the anchor. Serialization means one insert commits
  first; the second transaction's insert then violates
  `journal_operations.OperationID UNIQUE`. The reducer catches that violation and
  re-runs the §9.4 idempotent-replay path against the now-committed row: an exact
  four-field identity match returns the original anchor (idempotent success);
  otherwise it surfaces the typed `CommittedConflict`.

In both cases the caller observes a **typed** conflict/idempotent result, never a
raw SQLite constraint error: the translation from any backstop constraint
violation to the typed result happens inside the reducer, at the `Apply`
boundary, before returning to the caller.

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

   **S1.1 staging note.** At the journal-base layer (`dayvidpham/provenance#4`,
   S1.1) no `journal_operations` subtype table exists yet, so there is no
   operation anchor for a `task_event` row's `ProducedByOperationJournalID` to
   reference. `AppendTaskEvent` — the pre-operations base primitive the
   operations layer wraps — writes `task_event` rows with
   `ProducedByOperationJournalID = NULL` uniformly, which does not yet satisfy
   this rule. Rule 2's `NOT NULL` enforcement (and the `ProducedByOperationJournalID
   → journal_operations.JournalID` foreign key from §2.1) takes hold starting
   with the operations slice (`dayvidpham/provenance#5`, S1.2), when
   `journal_operations` lands and every effect-producing operation anchors its
   rows to it. `VerifyIntegrity`'s §10 rule 8 subtype-integrity guard does not
   check rule 2 and is unaffected by this staging gap.
3. Common fields (`JournalKind`, `ActorID`, `RecordedAt`) are never
   duplicated on a subtype row; a subtype row's only own attributes are the
   ones that do not already exist on the supertype.
4. `OperationID` is never a determinant of ordering and never a primary key
   anywhere in this schema (§3.1, §6).
5. **Anchor-only actor placement.** A journal row carries a stored `ActorID`
   **iff** it is an anchor row: `ActorID NOT NULL` exactly when
   `ProducedByOperationJournalID IS NULL` (an operation anchor, genesis, or
   migration baseline), and `ActorID NULL` on every subordinate (operation-produced)
   row, whose committing actor is instead *derived* from its anchor (§2.1, §8.5).
   The earlier committing-actor *agreement* rule — which forbade a produced row
   restamping a **different** actor than its anchor — is retired: a subordinate
   row has no `ActorID` column to disagree, so disagreement is structurally
   impossible rather than reducer-guarded. This placement invariant is expressed
   directly by a `CHECK ((ActorID IS NULL) = (ProducedByOperationJournalID IS NOT
   NULL))` constraint on `journal` **and** re-checked by the reducer /
   `VerifyIntegrity`: `Apply` rejects any input effect that would stamp an actor
   on a subordinate row, and `VerifyIntegrity` scans for any stored subordinate
   row carrying an `ActorID` (or any anchor row missing one) and rejects it. Its
   must-fail history is `authority_evidence.yaml` /
   `subordinate-row-carrying-actor-rejected`.
6. **Genesis NULL-authority discipline.** `journal_operations.AuthorityJournalID`
   is `NULL` only on a **genesis operation** (§4.6), and a genesis operation is
   accepted only when no `journal_operations` row yet exists — i.e. it is the
   first operation in the journal. Any later operation presenting a `NULL`
   `AuthorityJournalID`, or any genesis attempted against a non-empty journal, is
   rejected before commit. **Precedence:** this check is evaluated only after
   §9.4's `OperationID`-presence short-circuit — a same-`OperationID` retry of
   the genesis operation is resolved by §9.4 (returns the original bootstrap
   authority) before this rule ever runs, so this rule rejects only a genesis
   presenting a *new* `OperationID` against a non-empty journal, never an
   idempotent retry of the first one. See §4.6's precedence note.
7. **Genesis sole effect.** A genesis operation's only effect is exactly one
   `journal_authorities` row with `AuthorityKind = 'bootstrap'` (plus its
   `journal_authority_bootstraps` detail row): no task events, assignment
   transitions, decisions, or evidence. A `NULL`-authority operation producing any
   other or additional effect is rejected.
8. **Subtype totality, exclusivity, and discriminator agreement.** Enforced by
   the reducer at both inheritance levels (`journal` → its
   `journal_operations`/`journal_task_events`/`journal_authorities`/`journal_decisions`/`journal_evidence`
   subtypes, and `journal_authorities` → its `journal_authority_bootstraps`/assignment
   detail rows). Because doctrine 3 (rule 3 above) forbids repeating the
   discriminator on subtype rows, the schema cannot express these with a composite
   `(JournalID, JournalKind)` foreign key, so the reducer enforces, before commit:
   *(totality)* every `journal` row has exactly one subtype row selected by its
   `JournalKind`; *(exclusivity)* no `JournalID` appears in two subtype tables;
   *(agreement)* a subtype row's table matches its `journal.JournalKind`, and an
   authority's bootstrap/assignment detail matches its `AuthorityKind`.
   `subtype_integrity.yaml` carries the must-fail histories (no subtype, two
   subtypes, discriminator mismatch, and the authority-level
   bootstrap-with-transition mismatch).
9. **Result-slot own-operation integrity.** A `journal_operation_result_slots`
   row's `ProducedJournalID` must identify a `journal` row whose own
   `ProducedByOperationJournalID` equals the slot row's own `JournalID` — a
   result slot may only map to a row produced by its own operation (§3.2). The
   two FKs alone permit a slot row pointing at another operation's produced
   row; the reducer rejects this before commit.

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
`journal.RecordedAt`. Its whole point is to provide **a readable timeline over
what happened** — see §12.1. It is **non-causal**: it is never used to

- decide whether one journal row's effect happened "before" another for
  authority purposes (§9.3 uses `JournalID`, always);
- decide replay/migration order (§13 uses a documented deterministic
  pre-migration sort, not the migrated timestamps, precisely because two
  legacy rows can carry identical or misordered timestamps);
- order the **canonical** view of history — replay, authorization, lifecycle,
  and convergence order by `JournalID` and nothing else.

`RecordedAt` **is** used to order **display-facing listings** — the readable
timeline of §12.1 — but only there, and only as a non-causal presentation order.

Two journal rows sharing an identical `RecordedAt` is expected, not an error;
`JournalID` still totally orders them, and the timeline order breaks the tie by
`JournalID`. See [`ordering.yaml`](../testdata/contract/ordering.yaml) for the
adversarial cases pinning this.

### 12.1 Readable timeline

The whole point of `RecordedAt` is to provide a readable timeline over what
happened, so the journal query surface exposes a **display order** over it.
The `OrderByRecordedAt` dimension orders results by `(RecordedAt, JournalID)`:
wall-clock time, with `JournalID` as the composite tiebreak so equal timestamps
and backdated rows still yield a total, stable order.

**Display-vs-canonical firewall.** There are two order dimensions, and they are
kept strictly apart:

- **`OrderByRecordedAt` — non-causal display order.** A readable timeline over
  what happened. It NEVER establishes causality: no authorization, replay,
  lifecycle, or convergence decision consults it. It is the **default for
  display-facing listing queries** — an unqualified display query gets the
  readable timeline — because that is what a human reading history wants first.
- **`OrderByJournalID` — the canonical order.** The sole order for replay,
  authorization, lifecycle, and convergence. It remains available explicitly on
  the query surface, and it is the *only* order those causal paths ever use. The
  timeline dimension changes nothing about them.

**Composite cursor.** A timeline walk paginates with a composite **exclusive**
cursor `(after_recorded_at, after_journal_id)`: the next page returns rows whose
`(recorded_at, journal_id)` is strictly greater than the last row of the previous
page. Because the tiebreak is the total `JournalID` order, the walk never skips
or duplicates a row across equal timestamps or a backdated (timestamp-regressing)
row. The snapshot watermark is unchanged — still `JournalID`-bounded
(`journal_id <= snapshot`) — so a walk sees a consistent prefix of history under
either order.

**Covering index.** `CREATE INDEX idx_journal_recorded_at ON journal
(recorded_at, journal_id)` pairs with the dimension so the timeline walk seeks and
range-scans on `(recorded_at, journal_id)` without a filesort. The canonical order
continues to use the `journal_id` primary key.

The must-pass history
[`ordering.yaml`](../testdata/contract/ordering.yaml)/`timeline-walk-orders-by-recordedat-with-journalid-tiebreak`
proves a paginated timeline walk across an equal-timestamp tie and a backdated row
returns every row exactly once in `(recorded_at, journal_id)` order, while the
canonical `JournalID` query still returns commit order.

## 13. Legacy-baseline semantics

Migrating a pre-journal database installs one `journal_operations` anchor per
existing task, in a deterministic pre-migration order (legacy `created_at`
ascending, then legacy `id` ascending — a documented, reproducible sort over
the *pre-migration* table, independent of any journal content, since none
exists yet). Because `legacy id` is `tasks.id`, the existing table's own
primary key, it is globally unique, so `(created_at, id)` is already a total,
unique order regardless of how many legacy rows share an identical `created_at`
— `id` alone breaks every tie.

The whole migration executes under the `pasture-system` bootstrap authority
established by the genesis operation (§4.6): the genesis operation is the first
row written, then per-task anchors follow, each an ordinary non-genesis
operation citing that bootstrap authority. Each per-task baseline **anchor**
row stores `journal.ActorID` = the Pasture system actor (the actor *executing*
the migration, per the committing-actor model of §2.1); every subordinate
baseline row it produces (the migration-marker event, the assignment
transitions) carries `ActorID` NULL and *derives* that same system actor from
its anchor (§8.5) — the anchor-only placement of §2.1/§10 rule 5 applies to
migration baselines exactly as to native operations. The system actor is
**never** the legacy owner — the legacy owner appears only as the occupant
`ActorID` on the assignment episode (item 2). Each per-task baseline anchor's `OperationID` is a
deterministic function of the legacy task id — `provenance.migration.baseline--<legacy
tasks.id>` — so a re-run after a partially-recovered migration (or an accidental
second invocation) presents the identical `OperationID` per task and hits §9.4's
idempotent short-circuit (or a typed conflict), never a duplicate baseline
anchor. The anchor row's own `journal.RecordedAt` is the legacy task's
`updated_at` — the same honest legacy value used for its migration-marker event
(item 1), never the migration's wall-clock time. For each legacy task, in that
order:

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
   with a typed `MigrationOwnerUnmappableError` (§13.1); no task is migrated
   with a synthesized or guessed `ActorID`, and no partial baseline is left
   committed for any other task in the same run.

Because the resulting baseline rows are ordinary `journal`/`journal_operations`/
`journal_authority_*` rows — not a special pre-journal state format — a
subsequent real transfer chains off a migrated episode exactly as it would
off any other episode (`PredecessorAssignmentID` pointing at the migrated
`AssignmentID`). There is no separate "final row" shortcut anywhere in this
schema for legacy state: `Reduce` (§9) replays baseline rows through the
identical fold used for every other row.

**External-schema preflight.** Before the migration transaction opens, the
migration routine verifies the pre-journal schema's exact expected shape —
table **and** column presence/absence, in **both** directions: a missing
expected table, a missing expected column, an unexpected extra table, or an
unexpected extra column. Any mismatch — or any other topology corruption —
fails closed with a typed `SchemaPreflightError` (§13.1) before any row is
written. See
[`topology_corruption.yaml`](../testdata/contract/topology_corruption.yaml).

### 13.1 Fail-closed error shape

Both fail-closed paths in this section return a **typed, named-field error** —
not a prose-only "actionable error" — mirroring `dayvidpham/provenance#6`'s
established `StoreUnavailableError{Operation, Store, Stage, Impact, Fix, Cause}`
/ `CheckpointDivergenceError{Operation, Stage, Impact, Fix, Cause}` convention.
Each carries every component of the repo's six-part actionable-error contract:
**what** failed, **why**, **where**, **when**, caller **impact**, and **how to
fix**.

- **`MigrationOwnerUnmappableError`** (item 4):
  `{ Operation, Task, RawOwner, Stage, Why, Impact, Fix, Cause }`.
  - *what*: `Task` + `RawOwner` — the offending legacy task id and the raw
    unmappable owner string;
  - *why*: `Why` — the owner string resolved to no registered `ActorID`;
  - *where*: `Operation` + `Stage` — the migration routine and the
    owner-resolution stage;
  - *when*: `Stage` — owner resolution, before any baseline row for the run is
    committed;
  - *impact*: `Impact` — the entire migration transaction rolled back, zero
    baselines for any task;
  - *fix*: `Fix` — register the owner as an actor, or correct/remove the legacy
    owner string, then re-run (the deterministic `OperationID` scheme makes the
    re-run idempotent).
- **`SchemaPreflightError`** (external-schema preflight):
  `{ Operation, ExpectedShape, FoundShape, Stage, Why, Impact, Fix, Cause }`.
  - *what*: `ExpectedShape` vs `FoundShape` — the exact table/column the
    preflight expected and what it actually found (missing table, missing
    expected column, or unexpected extra column);
  - *why*: `Why` — the live schema does not match the shape this build
    understands;
  - *where*: `Operation` + `Stage` — the preflight routine and the specific
    table/column check;
  - *when*: `Stage` — preflight, strictly before any transaction opens;
  - *impact*: `Impact` — no row of any kind is written, activation halts;
  - *fix*: `Fix` — restore the expected schema shape or run the correct forward
    migration, then re-open.

Every field is non-empty on return. An implementation that fails closed with a
bare `errors.New("unmappable owner")` — carrying only a *what* — does **not**
satisfy this contract even though it technically "fails closed"; the corpus
must-fail cases in `baseline_migration.yaml` and `topology_corruption.yaml`
assert each of the six actionable components is present and non-empty
(`errorHasWhat`/`errorHasWhy`/`errorHasWhere`/`errorHasWhen`/`errorHasImpact`/`errorHasFix`),
so an inadequately actionable fail-closed error cannot pass this corpus.

### 13.2 Migrated/native observational equivalence

For any legacy task `T`, the `ProjectionState` (§9) produced by `Reduce` over
`[T`'s migrated baseline rows, followed by a sequence of native operations
`O_1..O_n]` is **identical** to the `ProjectionState` produced by `Reduce` over
a native-only history that creates an equivalent task and applies the same
logical sequence of operations reaching the same state. Migrated and native
history prefixes are observationally indistinguishable to the reducer beyond
`RecordedAt` provenance: because §13's baseline rows are ordinary
`journal`/`journal_operations`/`journal_authority_*` rows folded through the
identical §9 reducer — not a special pre-journal state format — nothing
downstream of a migrated episode (a transfer chaining off its `AssignmentID`, a
close ending its occupancy, an attribution edge) can observe that the episode
originated in migration rather than in a native `StartTaskAssignment`. This is
the concrete statement of the acceptance criterion "migrated/native histories
are observationally equivalent", pinned by `baseline_migration.yaml` /
`migrated-then-native-extended-equals-native-only`.

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

### 14.4 Assignment-transition lifecycle order

`UNIQUE(AssignmentID, Transition)` (§4.4) bounds an episode to at most one
`started` and at most one `ended` row but cannot express their required order.
The reducer enforces, before commit, for every `AssignmentID`: a `started`
transition appears first (exactly once); an `ended` transition is optional (at
most once) and, when present, must carry a strictly greater `JournalID` than
that episode's `started` transition. An `ended` transition with no prior
`started`, or a `started`/`ended` pair written in inverted `JournalID` order
(including both written in one batch with `ended` folded before `started`,
which §9.3's sequential per-effect fold also surfaces), is rejected. This is the
base lifecycle-order rule the neighboring orphan rule (§14.3) implicitly
assumes — §14.3 speaks of a predecessor that "has no ended transition row (…or
was never properly started)", which presupposes exactly this ordering. Stated
here (per the §14.3 convention) because the schema cannot express it;
`authority_evidence.yaml` carries `ended-transition-without-started-rejected`
and `ended-before-started-in-one-batch-rejected`.

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
| [`authority_evidence.yaml`](../testdata/contract/authority_evidence.yaml) | 9 | Per-effect authority at each `JournalID`; orphaned/multiply-consumed evidence; anchor-only actor placement (subordinate row carrying actor rejected); assignment-transition lifecycle order; regressions (a), (d) |
| [`owner_responsibility.yaml`](../testdata/contract/owner_responsibility.yaml) | 6 | Owner-responsibility end bound to legal close; transfer-CAS and transfer-crash atomicity; occupant attribution; regression (c) |
| [`baseline_migration.yaml`](../testdata/contract/baseline_migration.yaml) | 7 | Fresh/legacy-assigned/legacy-terminal/unmappable-owner baseline transitions; honest timestamps; actionable migration-error fields; migrated/native observational equivalence; idempotent re-run; regression (g) |
| [`topology_corruption.yaml`](../testdata/contract/topology_corruption.yaml) | 6 | Fail-closed on missing/corrupted external schema (table + column, both directions); actionable preflight-error fields; regression (f) |
| [`genesis_bootstrap.yaml`](../testdata/contract/genesis_bootstrap.yaml) | 5 | Genesis authority base case; NULL-authority discipline (first-operation-only, sole-bootstrap-effect); same-`OperationID` genesis retry short-circuit |
| [`operation_results.yaml`](../testdata/contract/operation_results.yaml) | 3 | `ResultSlotID` → produced-row mapping reconstruction; EmittedEvents via the produced closure; rule-9 result-slot own-operation integrity (must-fail) |
| [`subtype_integrity.yaml`](../testdata/contract/subtype_integrity.yaml) | 4 | Subtype totality/exclusivity/discriminator agreement (both inheritance levels) |
| [`actor_namespace.yaml`](../testdata/contract/actor_namespace.yaml) | 3 | Namespace-claim range disjointness; entry-in-range validation |

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
