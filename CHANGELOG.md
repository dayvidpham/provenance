# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

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
