# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Added

- **`pkg/provo`** — PROV-O / RO-Crate exporter of a Tracker graph using the
  `data-labour-prov` vocabulary (the data-labour integration). Pure reads:
  `provo.ExportTurtle(w, tr, opts)` emits deterministic Turtle conforming to the
  vocabulary's SHACL shapes; `provo.ExportROCrate(dir, tr, opts)` writes a minimal RO-Crate
  (`graph.ttl` + `ro-crate-metadata.json`). Tasks→`prov:Entity`, activities→
  `prov:Activity`/`p-plan:Activity`, agents→`prov:Person`/`prov:SoftwareAgent`/
  `:LLMAgent`, and the derived_from/supersedes/discovered_from/generated_by/
  attributed_to edges to their PROV relations; blocked_by is not exported.
- **`Tracker.AllActors() ([]Agent, error)`** and **`Tracker.AllEdges() ([]Edge, error)`**
  — whole-graph bulk reads (deterministically ordered) for graph consumers such as
  the exporter.
- **`Edge.CreatedAt time.Time`** — the edge creation timestamp, now surfaced from the
  existing `edges.created_at` column by `Edges`/`AllEdges` (zero for traversal-only
  helpers such as `DepTree`).

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
