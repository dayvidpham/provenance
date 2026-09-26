# Actor-ownership queries

`ActorOwnershipQueryAPI` is an optional read capability implemented by SQLite
stores opened with `OpenSQLite`, `OpenMemory`, and `OpenBorrowedSQLite`. It does
not extend `Journal`, `ContextJournal`, or `Tracker`. Callers obtain it by
asserting the journal value:

```go
api, ok := tracker.Journal().(provenance.ActorOwnershipQueryAPI)
if !ok {
    return errors.New("the open Provenance store does not expose actor-ownership reads")
}
snapshot, err := api.QueryActorOwnership(ctx, provenance.ActorOwnershipQuery{
    Actor:         actor,
    MaterialKinds: []provenance.EventKind{"pasture.assignment.started"},
    EvidenceKinds: []provenance.EvidenceKind{"pasture.assignment.command"},
})
```

## Snapshot contract

`QueryActorOwnership` validates the actor, context, and kind lists before it
leases a connection. Empty kind lists are valid and skip their corresponding
read stage. Duplicate kinds are ignored in new slices; the caller's slices are
not changed.

One deferred SQLite transaction reads:

1. the maximum journal ID (`Through`);
2. whether the actor has an `agents` row (`ActorKnown`);
3. tasks whose live `owner_id` is the actor, joined to the newest active
   owner-responsibility episode selected by the writer's rule;
4. selected task events produced by each winning operation or its governed
   allocation supplement; and
5. selected evidence produced by those same operations.

The result is ordered by `StartedJournalID`, then `TaskID`. Materials and
evidence are ordered by their journal IDs. `PredecessorAssignmentID` is returned
as stored and is never followed.

A transfer successor can have no material. Provenance does not guess the
Pasture transfer writer's follow-up operation name from a suffix or any other
convention. Consumers must handle that accepted residual explicitly.

## Integrity and limits

The only result bound is `MaxActorOwnershipResultBytes` (8 MiB). Row counts are
not bounded. Statements 3 through 5 ask SQLite for the byte length of eight
variable-width result values beside the value and suppress an individual value
when it cannot fit the remaining budget:

- `tasks.id`, `episodes.assignment_id`, `episodes.predecessor_assignment_id`, and
  `journal_operations.operation_id` in statement 3;
- `journal_task_events.event_kind` and `journal_task_events.payload` in statement
  4; and
- `journal_evidence.evidence_kind` and `journal_evidence.payload` in statement 5.

Three further variable-width selections cross the wire without that suppression,
and the difference is deliberate rather than an oversight.

- `episodes.actor_id` is **counted but never suppressed**. Its byte length is
  selected beside the value and added to the row total, so a store holding an
  oversized actor identifier is refused by the bound as a size refusal, with
  `*ActorOwnershipLimitError` and nothing else. It carries no suppression clause
  because a suppressed occupant would arrive as a NULL, and the owned-task stage
  reports a NULL occupant as `*OwnerProjectionMismatchError` — which would tell
  an operator that `tasks.owner_id` disagrees with the writer's winning-episode
  rule about a store where nothing disagrees. Reaching the state also needs an
  oversized `agents.id` already stored, because `episodes.actor_id` is
  `REFERENCES agents(id)` and no supported writer produces one.
- The `producers.task_id` join key in statements 4 and 5 is selected without a
  length. It is a re-read of a task ID that statement 3 already measured and
  budgeted, and the producer bindings it is drawn from are built in Go from those
  same already-scanned, already-guarded identifiers, so it cannot introduce a
  value the bound has not already seen.

Go still adds all lengths for a row before copying the row. The first crossing
row returns:

- `*ActorOwnershipLimitError`;
- `errors.Is(err, ErrActorOwnershipLimit) == true`;
- the first running total above the limit in `ObservedBytes`; and
- work completed before that row in `Work`.

No partial snapshot is returned. For the eight suppressed columns, a value
larger than the remaining result budget is suppressed by SQLite and is never
copied into Go memory. The values named above are always copied, and only their
length is budgeted, so the bound decides where the read stops rather than what it
carries. The budget is compared per value on the wire; the cumulative bound is
the read's own accounting, which refuses the crossing row rather than returning
what it has copied so far. The two statements' `ORDER BY` and the row count are
not affected by the budget: a statement returns every row its filters select, and
only the byte total decides where the read stops.

If `tasks.owner_id` disagrees with the writer's winning-episode rule, the read
returns `*OwnerProjectionMismatchError`. `errors.Is(err, ErrProjectionDivergence)`
is true. A stored row the owning stage's own result set rejects returns
`*ActorOwnershipIntegrityError`, whose `Stage` names the stage that read it —
`owned-tasks`, `materials`, or `evidence` — so a consumer can report where the
damage is instead of guessing. `errors.Is(err, ErrSubtypeIntegrity)` is true, and
a stored decode cause stays reachable through `errors.Is` and `errors.As`.
`Journal.VerifyIntegrity` and `Journal.ReplayProjections` are the operator checks
named by these diagnostics; both only read.

## Snapshot, cancellation, and lifecycle behavior

`Through`, `ActorKnown`, tasks, materials, and evidence all come from the same
SQLite snapshot. Concurrent committed writers cannot split that result across
statements. A deferred read does not take SQLite's write lock, so it completes
while another connection holds `BEGIN IMMEDIATE`.

Cancellation is preserved through `errors.Is` for `context.Canceled` and
`context.DeadlineExceeded`. The read rolls back and releases its pinned
connection before returning. `limitTransactionBusyTimeout` caps a lock wait at
the smaller of the profile's `busy_timeout` and the caller's remaining
deadline.

After a borrowed tracker or its owning DBOS root closes, the borrowed forwarder
returns `*StoreUnavailableError` before entering SQLite. `Tracker.Close` still
does not close the caller-owned `*sql.DB`; closing that handle is what makes the
liveness precheck fail.

This read performs no schema change, DDL, migration, index creation, or
projection write. It therefore works with existing v0.2.0-supported databases
without a schema migration.
