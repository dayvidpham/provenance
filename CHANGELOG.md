# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Breaking Changes

#### Stored databases: pre-v0.0.4 files are unsupported

- The canonical mutation wire layout changed while keeping the
  `provenance.mutation.v1` tag. A mutation envelope now carries a
  condition-count frame (and its encoded conditions) between the version field
  and the effect count; before this release the effect count followed the
  version directly. The tag is unchanged, so a pre-v0.0.4 row is not detected as
  a different codec — it simply fails to decode.
- **Databases created before v0.0.4 are out of scope and are not supported.**
  There is no `provenance.mutation.v2` tag, no v1-layout decoder, and no
  migration. Delete the database file and recreate it; there is nothing to
  migrate and restoring the same database from a backup cannot help, because a
  backup of a pre-v0.0.4 database is equally undecodable.
- Rationale for the decision: this package has no external consumers at this
  time, and its only user (pasture) runs fresh databases. Paying for a dual-codec
  decode path and a migration for zero affected installations would add a
  permanent legacy surface to the canonical encoding — the part of the system
  that most needs one exact, verifiable shape.
- Decode-failure diagnostics were corrected to match this decision: the startup
  canonical preflight, the startup canonical validation and replay paths, the
  fact-context backfill and validation paths, and stored-operation replay
  comparison no longer tell the operator to "restore from backup". They now state
  that a pre-v0.0.4 database is unsupported and must be deleted and recreated,
  and that a decode failure on a v0.0.4-or-later database means that row is
  corrupt.
- DBOS durable workflow inputs recorded by an earlier build are covered by the
  same decision: `DBOSApplyInput` carries canonical mutation bytes in the new
  layout and gained a `kind` field, so an in-flight workflow input written before
  v0.0.4 is not replayable.
- Checkpointed DBOS *step outputs* are covered too, for the same reason and by
  the same decision. `DBOSStepOutcome` and its `CanonicalApplyFailure` arm are
  persisted as JSON and decoded through a strict decoder that rejects unknown and
  duplicate keys. `CanonicalApplyFailure` was reshaped in this release — the
  `conflict_field` string is gone, and `conflict_axis`, `conflict_index`, the
  five condition fields, and the two activity fields are new — so a pre-v0.0.4
  checkpoint fails to decode on its `conflict_field` key rather than being read
  with a missing field. Delete the durable store along with the database; there
  is no partial-recovery path.

#### Public API

- `JournalAPI` is renamed to `Journal`, and `Tracker.Journal()` now returns
  `Journal`. Any code naming the old type must be updated.
- The `Journal` interface gained `Facts() FactQueryAPI`. Any type outside this
  module that implemented the old `JournalAPI` no longer satisfies it.
- The `Tracker` interface gained
  `InitializeGovernedRoot(context.Context, RootGenesisRequest) (OperationClosure, error)`.
  Any type outside this module that implemented `Tracker` no longer satisfies it.
- `PrepareMutationV1(effects []Effect)` is removed and replaced by
  `Canonicalize(in OperationInput)`, the sole public preparation boundary. It
  takes the whole operation input because conditions are now part of the
  canonical envelope, not a bare effect slice.
- `OperationConflict` is reshaped: the `Field string` member is replaced by
  `Axis ConflictAxis` (the closed five-axis set `ConflictActor`,
  `ConflictAuthority`, `ConflictCommand`, `ConflictCondition`, `ConflictEffect`)
  plus `Index int`, which is `-1` for a scalar axis or a length mismatch and the
  element index otherwise. `Error()` moved to a pointer receiver, so
  `*OperationConflict` — not `OperationConflict` — is the type to use with
  `errors.As`.
- `DBOSDiagFieldConflictField` is removed and replaced by
  `DBOSDiagFieldConflictAxis`, alongside new `DBOSDiagFieldConflictIndex`,
  `DBOSDiagFieldConditionIndex`, `DBOSDiagFieldConditionKind`,
  `DBOSDiagFieldConditionReason`, `DBOSDiagFieldAssertedJournalID`,
  `DBOSDiagFieldActualJournalID`, `DBOSDiagFieldActivityID`, and
  `DBOSDiagFieldExistingJournalID` fields.
- `CanonicalApplyFailure` — the public checkpointed failure arm of
  `DBOSStepOutcome` — loses its `ConflictField string` member
  (`json:"conflict_field"`) and gains `ConflictAxis *journal.ConflictAxis` plus
  `ConflictIndex *int` (`json:"conflict_axis"` / `json:"conflict_index"`),
  matching the `OperationConflict` reshape above. It also gains the optional
  condition metadata (`ConditionIndex`, `ConditionKind`, `ConditionReason`,
  `AssertedJournalID`, `ActualJournalID`) and activity metadata (`ActivityID`,
  `ExistingJournalID`). Code that read `failure.ConflictField` must switch over
  `failure.ConflictAxis` and `failure.ConflictIndex`; the JSON reshape is also a
  durable-state break, recorded above.
- `OpenBorrowedSQLite` no longer opens a second internal connection on the
  borrowed database's file: it activates the schema on the borrowed `*sql.DB`
  pool itself. The signature is unchanged, but the borrowed pool now carries all
  Provenance traffic, so its size and lifetime govern Provenance's throughput and
  availability. Its *pragmas* do not govern Provenance's own writes: every lease
  Provenance takes on the borrowed pool forces `foreign_keys=ON` for the lease,
  and returns the connection with the caller's captured value, verified by
  read-back — a restore that cannot be proven retires the connection instead of
  returning it. So Provenance's writes are always foreign-key enforced regardless
  of how the caller configured the pool, and the caller's own statements always
  see the caller's own value. Everything else on the connection remains the
  caller's, and the caller's DSN must supply them: Provenance never sets
  `journal_mode` or `synchronous` on a borrowed pool (unlike `Open`, which owns
  its pool and sets WAL there), and it rewrites `busy_timeout` only for the span
  of schema activation on the one activation connection, restoring the captured
  value — again verified by read-back — before handing that connection back. A
  shared file therefore wants a WAL, non-zero-`busy_timeout` DSN from the caller.
The four removals that follow are recorded for completeness, not as breaks a
released consumer can hit: `BindGovernedAllocator`, `GovernedAllocationDepth`,
`GovernedAllocationSupplementPolicy`/`GovernedAllocationSupplementPolicyV1`, and
the `GovernedAllocationComposedBatch*` aliases were all introduced *and* removed
after v0.0.3 and appear in no released tag. Upgrading from v0.0.3 requires no
action for any of them; only code tracking this branch between releases is
affected.

- `BindGovernedAllocator` is removed. It could not succeed: every call returned
  an error because the pinned DBOS release exposes no way to prove that a
  supplied `*sql.DB` is the handle stored inside the supplied root. Use
  `OpenBoundGovernedAllocator` (owns its root) or `NewHostBoundGovernedAllocator`
  (borrows the host's root and system handle at the one construction site).
- `GovernedAllocationDepth` is removed. No code path ever produced that error
  kind: governed allocation bounds breadth (`MaxGovernedAllocationChildren`), not
  ancestry depth, so the classification could never be observed by `errors.As` on
  a `GovernedAllocationError`.
- `GovernedAllocationSupplementPolicy` and `GovernedAllocationSupplementPolicyV1`
  are removed. The supplemental policy is fixed by the canonical encoder and is
  never accepted from or returned to a caller, so the re-exports named a value
  callers could not supply or observe.
- `GovernedAllocationComposedBatchRequest` and
  `GovernedAllocationComposedBatchResult` are removed. They were transparent
  aliases of `GovernedAllocationComposedRequest` and
  `GovernedAllocationComposedResult` — the same types, not a second contract —
  and having two names for one composed-allocation request implied a batch
  contract that has never existed: both the one-child and the multi-child entry
  points take the surviving type, which carries
  1..`MaxGovernedAllocationChildren` ordered children. Migration: rename
  `GovernedAllocationComposedBatchRequest` to
  `GovernedAllocationComposedRequest` and
  `GovernedAllocationComposedBatchResult` to
  `GovernedAllocationComposedResult`. The method names are unchanged, so
  `RunAllocateComposedBatch` and `AllocateGovernedComposedBatch` still name the
  multi-child entry points. The one known consumer (pasture) is updated in
  lockstep with its next dependency bump, so the rename never lands
  un-atomically.

#### Dependencies

- `github.com/dbos-inc/dbos-transact-golang` v0.16.0 → v0.20.0.
- **The DBOS durable fingerprint and workflow identity are re-keyed.** The
  pinned-library string that salts fingerprint derivation still read `v0.16.0`
  after the dependency moved to `v0.20.0`. That string is not documentation: it
  is hashed into every durable workflow ID and step name, so correcting it to
  `v0.20.0` re-keys the entire durable namespace. Every workflow ID this build
  derives differs from the one an earlier build derived for the same operation.
  This is a durable-state break, and it is taken under the same ratified window
  as the rest of this release: pre-v0.0.4 durable state is already declared
  non-replayable, so there is no reachable workflow for it to strand. The
  constant is frozen from here on — it does not track `go.mod`, and a future DBOS
  upgrade must leave it alone unless a drain-and-cut is decided deliberately.
  The canonical wire encoding is unaffected: every mutation digest in the
  independently pinned wire corpus is byte-identical, and only the 15 fingerprint
  values in `testdata/contract/dbos_wire_positive.yaml` were re-pinned.
- SQLite persistence moved from `zombiezen.com/go/sqlite` to
  `modernc.org/sqlite` (`database/sql`). `zombiezen.com/go/sqlite` remains only
  as an indirect dependency. Callers that shared a handle with Provenance through
  the zombiezen API must supply a `database/sql` pool instead.

### Migration

- Delete and recreate any database created before v0.0.4; nothing about it is
  recoverable by this build. There is no supported upgrade path and none is
  planned.
- `JournalAPI` → `Journal`; `PrepareMutationV1(effects)` →
  `Canonicalize(OperationInput{Effects: effects})`;
  `DBOSDiagFieldConflictField` → `DBOSDiagFieldConflictAxis`.
- Replace `conflict.Field` string comparisons with a switch over
  `conflict.Axis`, and match conflicts with `errors.As(err, &target)` where
  `target` is a `*OperationConflict`. The same applies to the checkpointed
  `CanonicalApplyFailure.ConflictField` → `ConflictAxis`/`ConflictIndex`.
- `GovernedAllocationErrorKind` values are renumbered. Removing the unreachable
  `GovernedAllocationDepth` from the middle of the closed set shifted the three
  kinds after it: `Collision` 6 → 5, `Genesis` 7 → 6, `Corruption` 8 → 7. Source
  code that names the constants needs no change. The numbers matter because the
  kind is a `uint8` that rides in DBOS durable output (the checkpointed governed
  allocation failure arm), so a failure checkpointed by an earlier build would
  now decode as the wrong kind. This is a durable-state break and it is covered
  by the same ratified window as the rest of this release — pre-v0.0.4 durable
  state is already declared non-replayable — so it is documented here rather than
  guarded. Never renumber this set again without a deliberate drain-and-cut:
  append new kinds at the end.

## v0.0.3 - 2026-07-23

### Breaking Changes

- Task, edge, label, and comment mutations moved from `Tracker` to the
  journal-backed `Session` API returned by `Tracker.As(actor, authority)`.
  Callers must establish or reuse a journal authority before writing.
- `provenance.IsValid(p)` and `provenance.IsKnown(p)` package-level functions
  removed. Callers should use `p.IsValid()` method on the `Provider` type
  instead. The method delegates to `bestiary.Provider(p).IsKnown()` — same
  semantics.
- `pkg/ptypes` is no longer zero-dependency: it now imports `bestiary` directly.
  This reverses the FIX-4 architectural decision from the prior wave (UAT-2),
  which had imposed a zero-dep constraint on `pkg/ptypes`.

### Migration

- Replace direct `Tracker` writes with the corresponding `Session` methods.
  Use `Session.Atomic` when several typed effects must commit as one operation.
- `if provenance.IsValid(p) { ... }` → `if p.IsValid() { ... }`
- `if provenance.IsKnown(p) { ... }` → `if p.IsValid() { ... }` (semantics identical)

### Added

- A canonical, globally ordered journal with typed operations, effects,
  authorities, assignments, decisions, evidence, replay, and legacy migration.
- The `Session` mutation SDK and `Session.Atomic` multi-effect transaction API.
- Atomic fixed-ID software-agent registration with namespace and manifest
  conflict validation.
- A DBOS v0.16 durable-execution adapter that binds retries and recovery to the
  canonical Provenance operation identity instead of introducing a second
  commit ledger.
- Static lifecycle transitions, canonical mutation encoding, schema preflight,
  projection-convergence checks, and corruption diagnostics.

### Changed

- Task projections, relationships, labels, and comments are derived through the
  same journal reducer used by normal execution, replay, and migration.
- SQLite startup validates the journal spine, schema watermark, subtype
  integrity, and projection convergence before accepting writes.
- Test gates now include deterministic retry/reopen matrices, concurrent-writer
  and authority-revocation races, DBOS crash-gap recovery, and a CGO-disabled
  build check.

### Fixed

- Canonical retries now preserve allocated identities, compare complete
  mutation descriptors, and fail closed on conflicting replay.
- DBOS retries now preserve terminal domain outcomes and reject malformed or
  mismatched durable records with actionable diagnostics.
- Genesis bootstrap and fixed-agent activation now converge safely under
  concurrent retries without partial writes.
- Nix packaging now uses a fixed dependency hash and excludes generated vendor
  and linked-worktree trees from first-party source hygiene scans.
