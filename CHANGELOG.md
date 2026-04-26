# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Breaking Changes

- `provenance.IsValid(p)` and `provenance.IsKnown(p)` package-level functions
  removed. Callers should use `p.IsValid()` method on the `Provider` type
  instead. The method delegates to `bestiary.Provider(p).IsKnown()` — same
  semantics.
- `pkg/ptypes` is no longer zero-dependency: it now imports `bestiary` directly.
  This reverses the FIX-4 architectural decision from the prior wave (UAT-2),
  which had imposed a zero-dep constraint on `pkg/ptypes`.

### Migration

- `if provenance.IsValid(p) { ... }` → `if p.IsValid() { ... }`
- `if provenance.IsKnown(p) { ... }` → `if p.IsValid() { ... }` (semantics identical)
