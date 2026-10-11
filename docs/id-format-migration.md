# Identity format cutover

Namespaced task, actor/agent, activity and comment identities now render as
`<namespace>/<uuid>`. Parsing splits the **last** slash, preserving namespace
bytes (including `--`, nested slashes and a trailing slash). UUID suffixes must
have the 36-character hyphenated shape. Nil and non-v7 UUIDs remain supported.
Retired `namespace--uuid` input is rejected; there are no runtime aliases.

## Storage boundary

`InspectIDFormat(ctx, reader)` reads through a database or caller transaction;
it neither activates schema nor writes. Populated unmarked stores are retired.
Unknown markers fail closed. Fresh empty stores get the slash marker in the
schema activation transaction. File and borrowed-pool opens refuse retired
stores before activation or journal decoding.

The `provenance_id_format` singleton is persistent format metadata and belongs
in database backups and snapshot comparisons. A marker does not prove integrity:
production integrity and projection verification remain necessary.

## Offline transaction APIs

The application owns writer exclusion, consistent SQLite backup, runtime-state
preflight, the SQL transaction and its Commit/Rollback. A raw file copy of an
open WAL database is not a consistent backup. Keep the original archive: active
canonical bytes and their identity-dependent digests change representation.

1. Inspect and preflight under the application's exclusive writer transaction.
2. Call `ConvertIDsInTx(ctx, tx, options)` for Provenance-owned typed columns,
   canonical mutations, allocation requests and their owner-derived digests.
   External owners may translate declared structured fields using the effect
   and context callbacks. They must not rewrite prose, raw host bindings, opaque
   authority/operation/result IDs, or host-body content digests.
   The context callback alone is not a canonical rewrite: the effect callback
   must translate matching canonical-backed extension contexts identically.
   Extension contexts in canonical conditions have no rewrite hook and must
   remain identity-equivalent; unsupported translations fail verification.
   This first converter treats edge targets as namespaced task references.
   Previously accepted opaque non-identity edge targets are unsupported and
   fail preflight with the offending column/key; they require an explicit
   target-owner contract, not a guessed reinterpretation or discarded edge.
3. Convert application-owned tables through their owners in **the same tx**.
4. Call `VerifyIDConversionInTx(ctx, tx)` for journal, canonical, allocation,
   evidence and projection-owner checks. This uses no pooled connection.
5. Bind `NewTransactionEvidenceReader(ctx, tx)` for application-owner replay.
   It uses the production evidence page/scanner and effective-actor semantics.
   Carry the first page's snapshot watermark across all subsequent pages.
   Replay and verify external projections and the complete raw evidence
   inventory without invoking reclamation.
6. Call `PublishIDFormatInTx(ctx, tx)` **last**, after all application-owner
   checks. It repeats dependency integrity and SQLite constraints, then writes
   the marker. Commit only after success; roll back the entire transaction on
   any error. No helper owns transaction termination.
7. Close handles, reopen with the actual upgraded tracker, and explicitly run
   its production integrity verifier. A repeated conversion of a marked store
   reports `AlreadyCurrent` without rewriting identities.

Session command digests change only when their stored value matches an exact
previous Session owner recipe. Caller-supplied unmatched command digests stay
opaque. Composed allocation supplemental commands use the allocation owner's
domain-separated recipe. Unknown identity-bearing serialized execution state
requires a supported owner migration or draining with the previous binary;
these APIs do not rewrite vendor DBOS history.

The APIs never reclaim raw payload blobs or convert native host evidence. A
later ordinary application read may reclaim eligible aged **unnamed** blobs
under its existing policy; record conversion-time preservation separately.
Rollback after publication means restoring the complete archive with compatible
older binaries, not reversing individual live rows.

## Authentic corpus

`testdata/id-format/` contains released v0.3.0 Provenance / v0.0.16 Pasture
production-writer bytes, semantic expectations, module pins, checksum and a
separate builder. Its README records the previous writer's definition-reference
limitations explicitly. It is not a runtime compatibility reader or evidence
of a live-store migration. Application CLI, lifecycle replay and operator
cutover verification are separate integration responsibilities.
