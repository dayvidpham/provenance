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
not bounded. Statements 3 through 5 ask SQLite for the byte length of every
variable-width result value beside the value and suppress an individual value
when it cannot fit the remaining budget. Go still adds all lengths for a row
before copying the row. The first crossing row returns:

- `*ActorOwnershipLimitError`;
- `errors.Is(err, ErrActorOwnershipLimit) == true`;
- the first running total above the limit in `ObservedBytes`; and
- work completed before that row in `Work`.

No partial snapshot is returned. A value larger than the remaining result budget
is suppressed by SQLite and is never copied into Go memory. The budget is
compared per value on the wire; the cumulative bound is the read's own
accounting, which refuses the crossing row rather than returning what it has
copied so far. The two statements' `ORDER BY` and the row count are not affected
by the budget: a statement returns every row its filters select, and only the
byte total decides where the read stops.

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
