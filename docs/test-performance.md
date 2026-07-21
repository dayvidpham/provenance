# Test performance

This document records measured test-suite costs and candidate optimizations. It
is a planning aid, not evidence that an optimization has already landed. The
authoritative quality gate remains:

```bash
CGO_ENABLED=1 go test -race -count=1 ./...
```

Performance work must preserve the production code paths, SQLite durability
checks, corruption-byte invariants, concurrent-writer coverage, and uncached
race run.

## Recorded baseline

### Full-suite run: 2026-07-21

This is the comparison baseline for future optimization work. The worktree had
documentation edits, but the tested Go tree was exactly commit
`ca31d786fd4363db18b49dc2b7430d7c678cb85b`.

| Property | Value |
|---|---|
| Command | `nix develop -c bash -c 'TIMEFORMAT="elapsed=%3R user=%3U system=%3S"; time env CGO_ENABLED=1 go test -race -count=1 ./...'` |
| Result | PASS |
| Wall / user / system | `257.033s` / `266.272s` / `13.418s` |
| Root package | `256.330s` |
| `internal/sqlite` | `39.759s` |
| Other tested packages | `1.011s` to `5.462s` |
| Go | `go1.26.1 linux/amd64` from the Nix development shell |
| Host | Linux `7.1.3`, x86-64 |
| CPU | AMD Ryzen 9 7950X3D, 16 cores / 32 threads |
| Memory visible to host | 61 GiB RAM, 68 GiB swap |

The full command deliberately used `-count=1`, so test results were not served
from Go's test cache. Build artifacts may have been warm. The host was not
otherwise isolated, so use repeated runs and compare medians before claiming a
small improvement. Preserve this row as historical data when adding a newer
baseline.

### Earlier hotspot samples

Earlier uncached race measurements on the same Proposal 50 journal branch did
not record an exact commit or controlled host-load snapshot. They are retained
for prioritization, not precise before/after claims. Repeated runs showed a
stable ordering of costs:

| Scope | Observed time | Interpretation |
|---|---:|---|
| Full race suite | about 230 seconds | End-to-end critical path |
| Root package | about 228-244 seconds | Dominates the full suite |
| `internal/sqlite` | about 34 seconds | Significant, but not the critical path |
| `TestStartupCorruptionMatrixLeavesBytesUnchanged` | about 53-63 seconds | Repeated database setup across corruption cases |
| Contract corpus execution | about 31-32 seconds | Repeated setup across fixture partitions |
| Authority race coverage | about 25 seconds | Deliberately exercises real concurrent state |
| DBOS matrix | about 19 seconds | Repeated durable process and database setup |
| Nix startup | about 0.86 seconds | Too small to explain the suite runtime |
| Warm compile/startup | about 2.6 seconds | Also not the primary bottleneck |

Both sets of measurements suggest reducing repeated setup before changing
assertions or splitting the race gate. Nix and compiler startup are not useful
first targets.

## Measurement workflow

Reproduce the baseline before and after each change. Run one experiment at a
time and retain the uncached full-suite result.

```bash
# Authoritative wall clock and correctness gate.
time CGO_ENABLED=1 go test -race -count=1 ./...

# Package-level attribution.
time CGO_ENABLED=1 go test -race -count=1 .
time CGO_ENABLED=1 go test -race -count=1 ./internal/sqlite

# Representative root-package costs.
time CGO_ENABLED=1 go test -race -count=1 . \
  -run '^TestStartupCorruptionMatrixLeavesBytesUnchanged$'
time CGO_ENABLED=1 go test -race -count=1 . \
  -run '^TestContractCorpus'
time CGO_ENABLED=1 go test -race -count=1 . \
  -run 'Authority.*Race|Concurrent.*Operation|Concurrent.*Session'
time CGO_ENABLED=1 go test -race -count=1 . \
  -run '^TestDBOS'
```

Use `go test -race -v` when comparing the sum of individual test durations to
wall time. Similar totals indicate a serial critical path; a much larger sum
indicates useful overlap. Use CPU profiles to attribute setup cost rather than
assuming SQLite itself is the bottleneck:

```bash
CGO_ENABLED=1 go test -count=1 -cpuprofile=cpu.prof .
go tool pprof -top -cum cpu.prof
```

Profiles are local artifacts and must not be committed.

## Candidate optimizations

Apply these in order. Each step should have an isolated benchmark commit or
recorded before/after result so a regression can be reverted independently.

### 1. Copy a validated SQLite baseline

Build and migrate one pristine database per compatible schema/setup family,
close it, and copy its bytes into each test's private temporary directory. Each
test must open its own copy; tests must never share a live database connection
or mutable database file.

This is the highest-confidence candidate for the startup corruption matrix,
contract corpus, and DBOS matrix because those tests repeatedly need the same
valid starting state before applying distinct mutations or failure scenarios.

The copied baseline must be validated before publication and immutable after
publication. Tests that specifically exercise first-open migration or database
creation must continue to construct from scratch.

### 2. Inject per-test paths and setup

Pass database and state paths through existing public or internal production
seams rather than process-global environment mutation. Per-test `t.TempDir()`
paths permit safe `t.Parallel()` use and prevent accidental reads from a
developer's real home or state directories.

Only mark tests parallel after proving they do not mutate package globals,
environment variables, process working directories, clocks, or a shared SQLite
file. Serial tests should state the global resource that requires serialization.

### 3. Reuse immutable construction, not mutable state

Parse immutable fixture corpora and construct read-only expected values once.
Keep each test's tracker, session, database, transactions, and files isolated.
For authority race cases, reuse setup within one scenario only when the scenario
still begins from the same required state and all goroutines exercise real
production operations.

### 4. Inject retry and timeout policy

Tests that prove a final typed outcome should not pay production backoff between
attempts. Inject a zero or bounded test retry policy through the same production
configuration seam used at runtime. Dedicated retry tests must continue to
exercise the real retry count and cancellation behavior with a controlled clock
or synchronization point instead of sleeps.

### 5. Remove duplicate full-suite execution

Run the full uncached race suite once per authoritative gate. Local workflows
may use narrow package runs while iterating, but a narrow run does not replace
the final `go test -race -count=1 ./...` result. CI jobs should not repeat an
identical full race invocation unless they run on a materially different target
or configuration.

### 6. Evaluate race-detector exit delay separately

`GORACE=atexit_sleep_ms=0` may remove the race detector's default process-exit
delay. Adopt it only after proving local and CI race reports remain complete and
the change yields a measurable full-suite improvement. It is lower priority than
database setup because process startup is not the measured critical path.

## Patterns adopted from Peasant Labs

Two existing Peasant Labs test strategies are useful here:

- Schema contract tests use embedded typed fixture corpora, aggressive safe
  parallelism, exact or minimum coverage guards, and synthetic-break tests. The
  reusable lesson is to optimize fixture loading without weakening anti-vacuity
  checks or replacing real production imports with test-only exports.
- Peasant integration tests reduced an uncached race package from 86.9 seconds
  to 19.6 seconds by measuring first, right-sizing SQLite pools, injecting paths
  instead of mutating process-global environment, copying validated migrated
  databases, and removing real retry sleeps. Its store package required a pool
  of two rather than one because some tests hold one connection while acquiring
  another; pool sizing must therefore be derived from Provenance behavior, not
  copied mechanically.
- Peasant also found that parallel race tests multiplied production-sized memory
  allocations. Any Provenance parallelization should measure peak RSS as well as
  elapsed time so lower wall time does not create CI out-of-memory failures.

These are transferable techniques, not proof that they are safe in Provenance.
Every adoption still requires the validation rules below.

## Validation rules

An optimization is acceptable only when all of the following remain true:

1. Tests import and execute production code paths; there is no test-only
   implementation or dual export.
2. Every mutable database and filesystem tree belongs to exactly one test.
3. Corruption tests compare bytes before and after rejected startup exactly as
   they do today.
4. Concurrent tests retain real goroutines, independent sessions/connections,
   and typed winner/loser outcomes.
5. Corpus tests retain completeness, minimum/exact counts where applicable,
   named cases, and synthetic negative controls.
6. Migration and fresh-database tests do not consume a pre-migrated baseline.
7. `go vet ./...`, the repository's `ast-grep` scan, the full uncached race
   suite, and the static build all pass.
8. Before/after measurements use the same machine, command, package cache state,
   and representative test selection.

Prefer the smallest setup change that moves the measured critical path. Stop an
experiment if it requires weaker assertions, shared mutable state, sleeps for
synchronization, or a second production behavior used only by tests.
