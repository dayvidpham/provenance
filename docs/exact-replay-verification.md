# Exact-replay verification reuse

An exact existing-operation replay used to enumerate every canonical operation
to verify fact-context sets, even when the durable verified-as-of ledger already
covered the same state. This is not a duplicated gate within one Apply: it is
repeated verification across calls on an unchanged, previously verified store.

The existing canonical identity comparison still runs first. At the original
fact-context gate, `exactReplayFactContextsVerified` may reuse the durable proof
only within the caller's current SQLite transaction snapshot:

- Generation, journal tail identity, covered-table presence and build shape must
  match the persisted verified ledger. Missing/unreadable/stale proof falls back.
- Structural activation probes, exact ledger/context-table definitions and the
  actual SQL of every generation trigger must still match. Trigger names alone
  cannot prove that a same-name no-op replacement detects writes.
- Additional host-owned tables are allowed. They are not fact-context inputs.
- Nothing is cached on a tracker, connection or transaction scope. A fresh fold
  in a mixed batch changes the generation; a later replay rechecks and falls
  back to the full gate over that transaction's uncommitted state.

The original full fact-context gate runs on every uncertainty. Canonical
identity/conflict classification, committed-result reconstruction, authority
checks and all new-write reducer/gate paths are unchanged. This optimization
does not publish or advance a ledger, alter a timeout, add a retry, or change
transaction ownership/atomicity. The ledger's existing threat boundary remains:
it detects accidental/out-of-band changes, not an adversary forging the ledger
or deliberately restoring generation/DDL after tampering.

## Regression sensitivity

File-backed tests count the actual canonical-journal enumeration: clean exact
replay performs zero scans and leaves every durable table unchanged. Missing or
stale ledger, unreadable ledger, dropped/replaced generation triggers, unrelated
historical context damage and non-fact canonical corruption take the original
path. A same-name no-op trigger followed by hidden context deletion is rejected.
In-memory stores without a durable proof perform the full scan. Mixed-batch
tests assert scan invalidation, result taxonomy and complete rollback on conflict.

Executed mutation controls distinguish safety from performance: unconditionally
skipping the gate makes unrelated/hidden context-corruption tests fail;
unconditionally retaining the full scan makes the clean replay scan-count test
fail. Restoring the guard passes both classes. The consumer's persisted system
identity suite also retains missing/noncanonical genesis, actor mismatch,
crash/retry and authority checks.

This is an unpublished source change. Consumer experiments use an explicit
private Go workspace; no release, installed dependency or operator store is
changed by the optimization work.
