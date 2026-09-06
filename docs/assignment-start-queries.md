# Assignment-start queries

SQLite journals expose the optional `provenance.AssignmentStartQueryAPI`.
`OpenSQLite`, `OpenMemory`, and `OpenBorrowedSQLite` support it. Neither the
required `Tracker` nor the required `Journal` interface changes. An external
journal implementation can omit this capability. Test the interface assertion
before use:

```go
reader, ok := tracker.Journal().(provenance.AssignmentStartQueryAPI)
if !ok {
    return fmt.Errorf("journal does not support assignment-start queries; use a SQLite tracker")
}
query := provenance.AssignmentStartQuery{
    Page: provenance.AssignmentStartPageRequest{Limit: 64},
}
page, err := reader.QueryAssignmentStarts(query)
```

The borrowed reader checks its owner's pool before each call. After that pool
closes, the call returns `*provenance.StoreUnavailableError`. Closing the tracker
does not close the caller-owned pool. No raw SQL handle is exposed by this API.

## Identity and filtering

Each row identifies one committed assignment start. `AuthorityJournalID` is the
exact start authority, not a material-event ID, producer anchor, or ancestor.
`ProducingOperationID` and `ProducingOperationJournalID` identify the operation
that made it. Parent and predecessor IDs are optional; they do not prove a
consumer's role or current governance. Slot `owner-responsibility` is the public
string for SQLite slot zero. It is not a role such as reviewer or supervisor.

Task, assignment, actor and operation filters each accept at most
`MaxFactFilterValues` values. Slot filters accept at most
`MaxAssignmentStartSlotFilters` values; only `SlotOwnerResponsibility` is valid.
Empty filters match all starts. Values are deduplicated without changing caller
slices. Values within a dimension are ORed; dimensions are ANDed. Assignment and
operation IDs must be nonempty and contain no control characters. Task and actor
IDs must be valid typed identities. Malformed filters fail before SQL, with an
`ErrInvalidQuery` cause.

## Snapshot and continuation

`Limit` must be `1..MaxFactPageSize`. It limits **consumed authority candidates**,
not returned starts. The page always returns `SnapshotPinned=true`.

- Fresh scan: set `SnapshotPinned=false`, `SnapshotMaxJournalID=0`. The call
  resolves the journal maximum inside its short read transaction. A nonzero
  `AfterJournalID` can resume above a completed high-water mark, but must not
  exceed the resolved maximum.
- Continuation: copy all three fields from `Next` into the next page request,
  keep the desired limit, and replay the filters. The cursor is exclusive.
  It does not authenticate filters. Changed filters are a different stateless
  query, not an automatically rejected request.
- Pinned empty: `SnapshotPinned=true`, snapshot zero, cursor zero. This remains
  empty after a writer commits. It does not resolve the maximum again. Start a
  new unpinned scan to see that writer. Other fact APIs have different zero
  semantics; do not pass zero to them as a pinned empty snapshot.
- Pinned nonzero: the snapshot boundary must still exist in `journal`. Negative
  IDs, a cursor above its snapshot, and a nonzero unpinned snapshot are invalid.

Valid bootstrap authorities and valid assignment ends consume capacity without
returning start rows. Filters also can remove all returned rows. Thus **empty
Rows with nonnil Next is not exhaustion**. Advance even when Rows is empty.
`Next=nil` means that the candidate scan at this boundary is exhausted.

The query fetches and validates up to `Limit+1` candidates, including lookahead.
It consumes only the first `Limit`. `Next.AfterJournalID` is the last consumed
candidate, not the lookahead and not the last matching result. Lookahead is read
again on continuation. Writes after a pinned boundary are not visible to that
continuation; a fresh scan above a completed boundary sees later candidates.

## Integrity and physical work

Candidate IDs are the ordered, deduplicated union of authority-kind journal
rows, authority subtype rows, and assignment-transition rows in `(after,
snapshot]`. Nullable joins preserve missing/wrong supertypes, missing transitions
and episodes, corrupt identifiers and slots, and broken producers. Validation
runs **before filters**, including for lookahead. Damage returns
`ErrSubtypeIntegrity` with the candidate and repair guidance; it does not produce
a partial page or silently omit a candidate.

A valid end needs a valid strictly earlier start of the same assignment. Each
fetched end uses at most one extra indexed diagnostic query. That query fetches
one prior start in a valid store (up to two to reject damaged duplicate starts),
and validates it without recursion. Each diagnostic also runs an indexed
same-marker subquery capped at two rows to reject duplicate markers. These
checks include starts below the page cursor. A sole start changed to an end is
an integrity error; a valid start followed by an end is not.

These are bounds on candidates, diagnostic results, and query calls, **not on
all rows SQLite visits** during UNION, ordering, joins or index searches. In the
real-driver test `TestAssignmentStartsPhysicalQueryEnvelope`, a pinned page of
65 end candidates with `Limit=64` used 69 query calls, 133 driver-fetched rows,
and 4 execution calls: 65 top-level rows, 65 prior-start rows, and 3
boundary/connection-lifecycle rows. The same-marker subquery work is inside
SQLite and is not part of that driver-row count. This measurement is not a
consumer gate-cost approval; consumers must include their other reads, writes,
and contention work when they set a runtime budget.

Episode-only orphan rows have no ordered JournalID anchor. This page API does
not detect them. Exhaustion is **not a whole-store integrity certificate**.
Use the public `Journal.VerifyIntegrity()` for whole-store audit before an
operator declares global recovery complete. This API does not reconstruct
consumer material facts, roles, or complete review batches on its own.
