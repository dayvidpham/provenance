# Test performance

This document records measured test-suite costs and candidate optimizations. It
is a planning aid, not evidence that an optimization has already landed. It
complements the comprehensive [test-infrastructure guide](../TESTING.md). The
authoritative test gates remain:

```bash
go test -count=2 ./...
CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./...
```

Performance work must preserve the production code paths, SQLite durability
checks, corruption-byte invariants, concurrent-writer coverage, and uncached
race run.

## Recorded baseline

### Raw per-case corruption preparation: 2026-07-22

The startup matrix now uses a narrow test-only zombiezen connection to mutate
each private baseline copy instead of successfully calling production
`OpenSQLite` before corruption. The raw connection uses
`OpenReadWrite|OpenURI` without `OpenCreate`, applies only the foreign-key and
check-constraint test PRAGMAs, proves every DML changes exactly one row, and
proves the canonical trigger exists before and is absent after DDL. It then
truncates WAL, closes, proves no SQLite sidecars remain, and proves the main
bytes differ from the pristine baseline before the one unchanged production
failed-open assertion.

| Gate | Current wall time | Comparison | Difference |
|---|---:|---:|---:|
| Focused startup + pool normal, `-count=2` | `10.871s` | `14.087s` | `-3.216s` (`-22.83%`) |
| Focused startup + pool race, `-count=2 -timeout=20m` | `35.363s` | `133.490s` | `-98.127s` (`-73.51%`) |
| Full normal, `go test -count=2 ./...` | `161.90s` | `164.450s` | `-2.550s` (`-1.55%`) |
| Full race, `CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./...` | `561.22s` | `655.360s` | `-94.140s` (`-14.36%`) |

The root package reported `160.643s` normal and `559.774s` race. Against the
earlier `641.044s` same-host race baseline, the current race wall time is
`79.824s` lower (`-12.45%`). These are single same-host runs in a dirty PR14
worktree, so they demonstrate that the isolated setup cost moved but are not a
controlled median or a general CI speed claim. The pool-16 setting remains test
harness policy and a realistic example, not production enforcement.

### Immutable startup baseline and pool-16 harness: 2026-07-22

The validated-copy startup matrix and corrected caller-owned pool policy passed
all repeated focused and full gates on the same host:

| Gate | Current wall time | `174.871s` / `641.044s` comparison | Difference |
|---|---:|---:|---:|
| `go test -count=2 ./...` | `164.450s` | `174.871s` | `-10.421s` (`-5.96%`) |
| `CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./...` | `655.360s` | `641.044s` | `+14.316s` (`+2.23%`) |

The normal root package reported `163.205s`; the race root package reported
`653.409s`. The focused startup plus pool selection passed in `14.087s` normal,
and the corrected pool-only selection passed in `4.636s`. The combined startup
and pool race selection passed in `133.490s`. The full normal improvement is
consistent with removing 97 full fixture builds, while the full race result did
not improve on this run and must not be represented as a race-suite speedup.
Compare repeated medians under controlled host load before attributing the
`+2.23%` race difference to code rather than host variance.

### Contention-layer correction gates: 2026-07-22

Both authoritative repeated suites passed on the same host and Nix development
shell as the latest comparison values:

| Gate | Current wall time | Latest comparison | Difference |
|---|---:|---:|---:|
| `go test -count=2 ./...` | `174.871s` | `174.261s` | `+0.610s` (`+0.35%`) |
| `CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./...` | `641.044s` | `635.397s` | `+5.647s` (`+0.89%`) |

The differences are small enough to treat as normal host noise rather than a
performance change. The non-race root package reported `174.381s`; the race root
package reported `636.806s`. No full test suite was run with `CGO_ENABLED=0`
because that duplicates the normal suite without testing another supported
execution mode; `CGO_ENABLED=0 go build ./...` remains the static-compatibility
gate.

### Full-suite run: 2026-07-21

This is the comparison baseline for future optimization work. The worktree had
documentation edits, but the tested Go tree was exactly commit
`ca31d786fd4363db18b49dc2b7430d7c678cb85b`.

| Property | Value |
|---|---|
| Command | Superseded single-pass full race command (recorded before the repeated-suite requirement) |
| Result | PASS |
| Wall / user / system | `257.033s` / `266.272s` / `13.418s` |
| Root package | `256.330s` |
| `internal/sqlite` | `39.759s` |
| Other tested packages | `1.011s` to `5.462s` |
| Go | `go1.26.1 linux/amd64` from the Nix development shell |
| Host | Linux `7.1.3`, x86-64 |
| CPU | AMD Ryzen 9 7950X3D, 16 cores / 32 threads |
| Memory visible to host | 61 GiB RAM, 68 GiB swap |

The historical command forced an uncached single pass but no longer satisfies
the repeated-suite requirement. Build artifacts may have been warm. The host was
not otherwise isolated, so use the current repeated commands and compare medians
before claiming a small improvement. Preserve this row as historical data when
adding a newer baseline.

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
# Authoritative wall clock and correctness gates.
time go test -count=2 ./...
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./...

# Package-level attribution.
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m .
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./internal/sqlite

# Representative root-package costs.
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m . \
  -run '^TestStartupCorruptionMatrixLeavesBytesUnchanged$'
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m . \
  -run '^TestContractCorpus'
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m . \
  -run 'Authority.*Race|Concurrent.*Operation|Concurrent.*Session'
time CGO_ENABLED=1 go test -race -count=2 -timeout=20m . \
  -run '^TestDBOS'
```

Use `go test -race -count=2 -timeout=20m -v` when comparing individual test durations to
wall time. Similar totals indicate a serial critical path; a much larger sum
indicates useful overlap. Use CPU profiles to attribute setup cost rather than
assuming SQLite itself is the bottleneck:

```bash
CGO_ENABLED=1 go test -count=2 -cpuprofile=cpu.prof .
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

### 4. Keep retry ownership at the durable boundary

SQLite's `busy_timeout=5000` is the sole local contention wait. Borrowed writes
perform one inner operation and return an escaped `BUSY` or `LOCKED` unchanged;
the DBOS adapter's injected step options control durable retry count and timing.
Dedicated retry tests inject faults at the borrowed call boundary and assert one
borrowed attempt per DBOS callback without elapsed-time or sleep-sequence checks.

### 5. Remove duplicate full-suite execution

Run the full uncached race suite once per authoritative gate. Local workflows
may use narrow package runs while iterating, but a narrow run does not replace
the final `go test -race -count=2 -timeout=20m ./...` result. CI jobs should not repeat an
identical full race invocation unless they run on a materially different target
or configuration.

### 6. Evaluate race-detector exit delay separately

`GORACE=atexit_sleep_ms=0` may remove the race detector's default process-exit
delay. Adopt it only after proving local and CI race reports remain complete and
the change yields a measurable full-suite improvement. It is lower priority than
database setup because process startup is not the measured critical path.

## Patterns adopted from Peasant Labs

Peasant's concrete golden lifecycle is in
`/home/minttea/dev/peasant-labs/peasant/develop/internal/store/storetest/golden.go`:
`sync.Once` runs production `store.Open` and migrations, closes the store,
byte-copies the closed main file into each test's private directory, and reopens
the copy with `WithSkipMigrations`. Integrity/reopen validation, explicit WAL
checkpoint validation, sidecar absence proof, and digest pinning are Provenance
hardening, not behavior provided by Peasant's helper. Fresh-create, migration,
crash, and shared-WAL tests must not consume a golden.

Peasant's measured optimization sequence reduced an uncached race package from
`86.9s` to `55.1s` after right-sizing the SQLite test pool and then to `19.6s`
after path injection and parallelism. Its single-connection test pool and
`TestMain` process environment/pool override in
`cmd/peasant/main_test.go` were package-wide test policy, while the flags in
`cmd/peasant/helpers_test.go` injected per-invocation paths without per-test
environment mutation.
These are lessons, not Provenance defaults. Peasant's pool of one does not apply
to registered DBOS harnesses, which use 16 open and 8 idle connections as a
realistic test and recommended example because DBOS has readers and durable
background work. Provenance does not validate or mutate caller-owned pool
limits, 16 is not a required minimum, and smaller deliberate pools remain
supported. SQLite WAL remains single-writer regardless of connection count.

The same work exposed a memory caveat: parallel race tests multiplied
production-sized allocations and could be OOM-killed. Measure process-tree peak
RSS or `/proc/<pid>/status` `VmHWM` with `GOMAXPROCS` matching CI; Go memory
profiles omit SQLite/C allocations and race shadow memory.

## Schema testcase patterns

Schema's exact top-level policy reference is
`/home/minttea/dev/peasant-labs/schema/develop/TESTING.md`; its implementation
reference is `/home/minttea/dev/peasant-labs/schema/develop/testcase/`.
Transferable patterns are strict typed single-document decoding with known
fields and unique case
keys, exact identity membership rather than counts alone, independently
authored expected values, and immutable embedded fixture bytes. A case that
mutates an input first clones all mutable slices/maps/nested values.

Freshness gates prove every enumerated contract member has a current case and no
stale fixture remains. Synthetic-break tests deliberately remove, add, or alter
one member and require the harness to fail. Negative controls begin from a valid
positive case and apply one controlled invalid mutation; malformed fixture data
must not accidentally become the thing under test.

## Provenance transfer matrix

| Prior-art pattern | Provenance use | Local reference |
|---|---|---|
| Validated closed golden copied per case | Adopted only for the startup corruption matrix; raw per-copy mutation is test preparation and each corrupt copy still receives one production rejection open | `canonical_startup_matrix_test.go` |
| Strict typed one-document fixture loading | Adopted | `internal/testcorpus/corpus.go` |
| Exact case/operator membership and freshness | Adopted; counts remain supplemental anti-vacuity guards | `journal_corpus_test.go`, `internal/testcorpus/scope.go` |
| Independent expected values and synthetic breaks | Required for contract corpora and loader meta-tests | `internal/testcorpus/*_test.go`, DBOS contract tests |
| Private path injection | Required where an existing production seam accepts a path | `dbos_harness_test.go`, recovery/corruption tests |
| Broad `t.Parallel()` | Deferred pending isolation and peak-RSS proof | DBOS/shared-WAL suites remain serial |
| Peasant single-connection test pool | Excluded for registered DBOS harnesses; pool 16 is realistic evidence/recommendation, not a public minimum | `dbos_harness_test.go`, `dbos_store_test.go`, `dbos_crashgap_test.go` |
| Package-wide `TestMain` environment mutation | Excluded; no global cwd/env isolation policy is introduced | No Provenance `TestMain` |
| `WithSkipMigrations` | Excluded from production/test APIs; Provenance production-validates the pristine baseline, uses a raw test-only handle solely to corrupt each private copy, then calls production `OpenSQLite` once to prove rejection | `canonical_startup_matrix_test.go` |
| Golden reuse for DBOS, migration, fresh creation, or crash tests | Explicitly excluded because it would bypass the behavior under test | `dbos_crashgap_test.go`, `journal_recovery_corpus_test.go`, `dbos_harness_test.go` |

These techniques are transferable only when their invariants match Provenance.
They do not justify shared mutable state, test-only production options, weaker
assertions, or changes to reviewed contention/retry ownership.

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
7. `go vet ./...`, the repository's `ast-grep` scan, both repeated test suites,
   and the `CGO_ENABLED=0` static build all pass.
8. Before/after measurements use the same machine, command, package cache state,
   and representative test selection.

Prefer the smallest setup change that moves the measured critical path. Stop an
experiment if it requires weaker assertions, shared mutable state, sleeps for
synchronization, or a second production behavior used only by tests.
