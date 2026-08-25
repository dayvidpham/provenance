# Test performance

This document records measured test-suite costs and candidate optimizations. It
is a planning aid, not evidence that an optimization has already landed. It
complements the comprehensive [test-infrastructure guide](../TESTING.md), which
is the single source of truth for test gates. There is exactly one authoritative
suite and it is race-only:

```bash
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m ./...
```

There is no non-race wave, and no invocation specifies `-count`, `-p`, or
`-parallel`. Local iteration uses the same command narrowed with `-run` or a
package path, never with `-race` or `CGO_ENABLED=1` dropped. `CGO_ENABLED=0` is
a build-only gate. See [TESTING.md](../TESTING.md#authoritative-gates).

Historical measurements below keep their original commands for comparability:
they record what was actually run at the time and must not be rewritten. They
are evidence, not instructions.

Performance work must preserve the production code paths, SQLite durability
checks, corruption-byte invariants, concurrent-writer coverage, and the single
uncached race run.

## Recorded baseline

### DBOS queue polling: no configuration seam, and no measurable cost: 2026-08-25

An investigation asked whether the DBOS queue runner's one-second polling
cadence could be lowered through a production configuration seam, on the
hypothesis that it quantised every DBOS context launch. Both halves of the
hypothesis failed: no such seam exists in `dbos-transact-golang v0.20.0`, and
the cadence is not measurably part of the suite's cost. The dominant per-launch
cost is durable construction — DBOS system-database migration plus Provenance
schema activation — not waiting.

#### Why no seam exists

Read against the pinned `v0.20.0` module source:

| Fact | Location |
|---|---|
| The only queue in a Provenance process is DBOS's reserved internal queue, created during context construction by the deprecated in-memory registration path | `dbos/dbos.go:573` |
| Its base polling interval is the package default, one second, and no option is passed | `dbos/internal/models/queue.go:11` |
| `dbos.Config` exposes no queue polling field; `SchedulerPollingInterval` covers only dynamic cron reconciliation | `dbos/dbos.go:36-53` |
| `RegisterQueue` refuses the reserved name, because the in-memory registration already holds it | `dbos/queue.go:369` |
| `SetPollingInterval` applies only to database-backed queues | `dbos/queue.go:168` |
| `RetrieveQueue` and `ListQueues` read the `queues` table, which never contains the internal queue | `dbos/queue.go:451`, `dbos/queue.go:474` |
| A worker reloads its configuration only when its queue is database-backed, so even a later row could not retune the already-running internal worker | `dbos/queue.go:703` |
| The supervisor's own reconcile tick is a separate hard-coded one second | `dbos/queue.go:584` |

A runtime probe against a live context confirmed the reading: before and after
launch, `RetrieveQueue("_dbos_internal_queue")` returned a nil queue and a nil
error, `ListQueues` returned zero queues, and `RegisterQueue` on that name
failed with `cannot register database-backed queue "_dbos_internal_queue": an
in-memory queue with that name already exists`.

Reaching the interval would therefore require reflection or an internal import.
Neither is acceptable, so nothing was wired. The upstream ask is to accept a
polling interval for the internal queue on `dbos.Config`, or to let
`RegisterQueue` retune the reserved queue. It is not fixed in the later
`v1.2.0`, which hard-codes the same one-second default on a now-unexported
internal queue value and rejects the reserved name explicitly
(`dbos/queue.go:512`, `dbos/queue.go:324`).

Separately, a Provenance-level polling knob would have had nothing to configure
even if the library allowed it: Provenance registers no queue and enqueues no
workflow. Every workflow runs through `dbos.RunWorkflow` on the owning context,
so queue polling is never on a Provenance latency path.

#### What the cadence actually costs

A launch-heavy workload was run under the race detector with sixteen additional
idle launched contexts alive — sixteen extra internal-queue workers and
supervisors ticking every second — interleaved with the same workload run with
none, three rounds alternating to control for drift:

| Round | Bare | With 16 idle launched contexts |
|---|---:|---:|
| 0 | `6.151s` | `5.564s` |
| 1 | `6.303s` | `6.693s` |
| 2 | `6.793s` | `7.701s` |

The difference changes sign between rounds and is smaller than the upward drift
across the run, so the tick's contribution is below host noise on this host.
These are single same-host runs taken while other work shared the machine; they
bound the effect rather than measure it precisely.

#### Where the per-launch cost is instead

One `OpenBoundGovernedAllocator` + `Launch` + genesis + composed allocation +
`Close` cycle, measured phase by phase over repeated isolated iterations:

| Phase | Normal | Race |
|---|---:|---:|
| `sql.Open` + ping | `26-55ms` | `28-47ms` |
| `dbos.NewDBOSContext` (system-database migration) | `342-695ms` | `492-589ms` |
| `OpenBorrowedSQLite` (Provenance schema activation) | `39-44ms` | `990ms-1.019s` |
| Launch | `~9ms` | `12-37ms` |
| Genesis workflow | `29-42ms` | `65-94ms` |
| Composed allocation workflow | `30-40ms` | `90-121ms` |
| Close | `19-38ms` | `23-37ms` |

Construction is therefore 85-95% of a launch, and schema activation is the part
the race detector amplifies most, from tens of milliseconds to a full second.
Against the recorded inventory of 161 context launches in one suite run, the
construction phases alone account for roughly 245 seconds of race-mode work,
which is the same order as the whole 291-second race baseline. The two levers
that can move it are launching fewer contexts and making activation cheaper;
polling cadence is not a lever.

For comparison, the recorded heaviest single test,
`TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts`
with 51 launches, took `15.79s`, `16.54s`, and `16.26s` in three isolated race
runs, versus `100.74s` in the loaded baseline inventory. That spread tracks
contention over shared construction cost, not a polling quantum: a
one-second-per-launch quantum could account for at most 51 seconds of an
85-second gap, and the idle-context experiment above shows the tick does not
produce it.

### Proposal 54 CPU and result-polling correction: 2026-07-22

`-count=16` was rejected because it repeats every test sixteen times; it does
not allocate sixteen CPUs. A corrected full run at `-p=16 -cpu=16
-parallel=16 -count=1` took `80.44s` normal and `280.91s` under race before
parallelization. Isolated top-level DBOS matrices and retry families were then
marked parallel while state-sharing subtests and the process-wide goroutine leak
check remained serial. The root package fell to `30.228s` normal and `180.095s`
race before result-polling optimization.

The two longest families were measured independently under race with
`-cpu=2 -count=1` and no explicit `-parallel`:

| Result polling interval | Retry/terminal | Canonical-family retry |
|---|---:|---:|
| 50 ms | `4.18s` | `5.79s` |
| 100 ms | `4.89s` | `6.35s` |
| 200 ms | `6.41s` | `7.62s` |
| DBOS 1 s default, earlier 16-CPU measurement | `19.13s` | `19.18s` |

The package default is 50 ms. `DBOSAdapterConfig.ResultPollingInterval`
accepts explicit values from 10 ms through 5 s. These are single same-host runs
from a dirty PR14 worktree, useful for relative selection rather than general CI
claims.

After selecting 50 ms and restoring zero-value harness configuration, the full
strict normal scheduler matrix (`-cpu=1,16`) passed in `37.06s` wall and the
full race gate (`-cpu=16`) passed in `182.88s` wall. The race gate remained near
the earlier `183.18s` because the shortened DBOS families overlap another race
critical path; isolated family latency, not full race wall time, is the measured
gain from result polling.

The subsequent default-runner pass removed explicit `-cpu`, `-parallel`, and
`-p` settings from the authoritative gates, parallelized isolated fixtures, and
used empty model registries only in harnesses that do not exercise ML models.
The authority race loops now reuse one tracker with unique task, assignment, and
operation identities per iteration. The 325-case create matrix runs five
isolated task-type partitions, and the 98-case startup corruption matrix runs
against parallel private copies of one immutable baseline. Coverage counts,
production SQLite opens, corruption-byte checks, and race iteration counts are
unchanged.

| Current default-runner gate | Wall time | Root package |
|---|---:|---:|
| Normal | `10.485s` | `9.070s` |
| Race | `72.46s` | `70.570s` |

The full race wall time fell from the initial `120.46s` default-runner baseline
to `72.46s` (`-48.00s`, `-39.85%`). Focused race package times were `4.412s` for
the two authority races together, `2.705s` for all create permutations, and
`5.300s` for the startup corruption matrix. Under the full workload, those same
tests still report contention-inflated elapsed times, so isolated timings must
not be summed or presented as end-to-end savings. The exploratory sub-60-second
goal was not reached without reducing stress counts or required production-path
coverage; neither compromise was accepted.

### Raw per-case corruption preparation: 2026-07-22

Driver note (later than this measurement, and not a re-measurement): the raw
fixture was written against `zombiezen.com/go/sqlite`, whose flags were
`OpenReadWrite|OpenURI` without `OpenCreate`. The SQLite driver has since moved
to `database/sql` over `modernc.org/sqlite`, and the fixture moved with it:
`raw_sqlite_test.go` now opens a `file:` DSN with `mode=rw` (existing file
required, none created), pins a single connection, and sets
`busy_timeout=5000`. The preparation steps and assertions described below are
unchanged; the timings below were measured on the earlier driver and are not
restated as current.

The startup matrix now uses a narrow test-only raw connection to mutate each
private baseline copy instead of successfully calling production `OpenSQLite`
before corruption. The raw connection opens the existing file read-write without
creating one, applies only the foreign-key and check-constraint test PRAGMAs,
proves every DML changes exactly one row, and proves the canonical trigger exists
before and is absent after DDL. It then
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

Earlier uncached race measurements on the same Proposal 54 journal branch did
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
# Authoritative wall clock and correctness gate (one race suite, no -count).
time CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m ./...

# Package-level attribution. -cpu is pinned here because this is a documented
# measurement experiment, not a gate.
time CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m .
time CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m ./internal/sqlite

# Representative root-package costs.
time CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m . \
  -run '^TestStartupCorruptionMatrixLeavesBytesUnchanged$'
time CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m . \
  -run '^TestContractCorpus'
time CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m . \
  -run 'Authority.*Race|Concurrent.*Operation|Concurrent.*Session'
time CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m . \
  -run '^TestDBOS'
```

Use `CGO_ENABLED=1 go test -race -cpu=2 -timeout=20m -v` when comparing individual test durations to
wall time. Similar totals indicate a serial critical path; a much larger sum
indicates useful overlap. Use CPU profiles to attribute setup cost rather than
assuming SQLite itself is the bottleneck:

```bash
CGO_ENABLED=1 go test -race -cpu=2 -cpuprofile=cpu.prof .
go tool pprof -top -cum cpu.prof
```

Profiles are local artifacts and must not be committed.

## Regression traps: what made the suite roughly four times slower

The historical race suite took about 230-257 seconds in single-pass runs and
more than 600 seconds when the entire suite was repeated. The current
default-runner race gate takes about 72 seconds on the same host. This is a
roughly 3-4x comparison across several commits, not a controlled attribution to
one change. The regressions were cumulative: repeated setup, unnecessary
waiting, serialized independent cases, and accidentally repeated gates all
contributed. Future contributors should avoid reintroducing these patterns.

| Regression trap | Why it inflated runtime | Evidence observed here | Prevention rule |
|---|---|---|---|
| Treating `-count` as a CPU setting | `-count=N` executes every selected test N times; it does not allocate N CPUs. | `-count=2` race runs exceeded 560-650 seconds, and the proposed `-count=16` command would have repeated the complete suite sixteen times. | Authoritative gates specify no `-count` at all. Leave `-cpu`, `-p`, and `-parallel` unset unless a documented experiment needs them. |
| DBOS's one-second default result polling in test harnesses | Every durable-result observation paid coarse polling latency even when the local result was already available. | The two main retry families took about 19 seconds each at one second, versus 4-6 seconds with 50 ms polling. | Keep Provenance's validated zero-value `ResultPollingInterval` default at 50 ms. Use explicit intervals only when testing the interval contract, and do not replace polling with sleeps. |
| Rebuilding and reopening a complete valid SQLite fixture for every corruption case | Schema creation, migrations, fixture writes, integrity verification, WAL handling, and production reopen were repeated before each one-row mutation. | The startup corruption family repeatedly appeared as a 53-63 second race hotspot before validated baseline copying; raw per-copy mutation reduced its focused repeated race measurement by 73.51%. | Build, production-validate, checkpoint, close, and digest one immutable baseline; byte-copy it to a private path per case. Never use the baseline for tests whose contract is creation, migration, crash recovery, or shared-WAL behavior. |
| Recreating a tracker and schema inside every iteration of one race scenario | Forty race attempts paid full `OpenMemory`, schema, actor, and bootstrap setup forty times. | The two authority races fell to 4.412 seconds together after reusing one tracker with unique per-iteration identities. | Reuse setup only within one test when every iteration uses distinct task, assignment, and operation IDs and accumulated state cannot alter the invariant. Keep independent tests isolated. |
| Serializing independent matrix and corpus cases | Private databases and immutable inputs waited behind unrelated cases despite having no shared mutable state. | Partitioning all 325 create combinations reduced focused race package time from 10.677 to 2.705 seconds; parallel private startup copies reduced that focused family to 5.300 seconds. | Use `t.Parallel()` only after proving private paths, no package globals, no cwd/environment mutation, safe cleanup, and bounded peak RSS. Preserve exact membership/count checks. |
| Constructing the full bestiary model registry in non-ML fixtures | Unrelated task, journal, corruption, and concurrency tests repeatedly allocated and seeded model data they never queried. | Profiling attributed roughly 208 MB of aggregate allocation to repeated default-registry construction; it was not the race wall-time critical path, but was avoidable setup. | Inject `NewRegistry(nil)` into non-ML harnesses. Keep explicit default-registry fixtures for every test that registers or validates ML models. |
| Assuming more parallelism always improves wall time | SQLite writers, filesystem work, race shadow memory, and production-sized fixtures contend when too many heavy tests overlap. | Moving the create matrix and concurrent-create stress test to the serial phase regressed the full race gate from about 76 to 89 seconds; broad parallel runs also inflated individual tests by 4-10x under load. | Compare the full uncached gate, not only focused timings. Retain parallelism when overlap is net-positive, but do not add nested or broad parallelism without race, RSS, and full-suite measurements. |
| Repeating an identical full race gate in multiple jobs or local commands | Every duplicate invocation pays the complete schema, DBOS, corruption, and race-detector cost again without covering another configuration. | Historical repeated-suite commands approximately doubled already-expensive single-pass measurements. | Run one authoritative uncached race gate per configuration. Use focused cached runs for iteration and reserve another full run for materially different targets. |

Do not “fix” a regression by lowering exact corpus membership, corruption cases,
race iterations, concurrent operation counts, production reopen checks, or
integrity assertions. Optimize fixture construction and scheduling first, then
record both focused and full-gate evidence in this document.

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
the final configured `CGO_ENABLED=1 go test -race ... ./...` result. CI jobs should not repeat an
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
