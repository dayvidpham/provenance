# Testing Provenance

This guide is the source of truth for Provenance test infrastructure. Test
performance work must preserve the reviewed relational, startup-corruption,
DBOS durability, and SQLite contention semantics. Historical measurements and
optimization experiments live in [docs/test-performance.md](docs/test-performance.md).

## Authoritative gates

All local, focused, CI-readiness, and landing test invocations use the race
detector. The typical full-suite invocation is:

```bash
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m ./...
```

The remaining non-test gates are:

```bash
go vet ./...
ast-grep scan --config sgconfig.yml --globs '!vendor/**' --globs '!worktree/**' .
CGO_ENABLED=0 go build ./...
nix flake check --no-build
```

`CGO_ENABLED=0` is a build-only compatibility gate. There is no CGO-disabled
test mode and no CGO-disabled race mode: do not run focused, package, full, or
race tests with `CGO_ENABLED=0`. Focused local iteration may narrow `-run` or
the package list, but retains `CGO_ENABLED=1`, `-race`, `-shuffle=on`,
`-fullpath`, and `-timeout=20m`. A focused diagnostic does not replace the full
CI/landing suite.

CI leaves `-cpu`, `-p`, and `-parallel` unset so Go uses the runner's available
processors and default package/test concurrency. Do not specify `-count`.
`-shuffle=on` reports a seed; reproduce an order failure with
`-shuffle=<reported-seed>`.

## Test map

| Layer | Purpose | Representative files |
|---|---|---|
| Pure types and contracts | Closed enums, parsing, canonical mutation shapes, state machines | `pkg/ptypes/*_test.go`, `internal/journal/*_test.go` |
| Strict YAML corpora | Typed positive and negative behavior tables executed through production handlers | `internal/testcorpus/`, `testdata/contract/`, `journal_corpus_test.go`, `dbos_contract_fixture_test.go` |
| Standalone SQLite | Real `OpenMemory`/`OpenSQLite` persistence and query behavior | `tracker_test.go`, `session_test.go`, `internal/sqlite/*_test.go` |
| Corruption and startup | Fail-closed activation, topology checks, and byte immutability | `canonical_startup_matrix_test.go`, `canonical_preflight_error_test.go`, `journal_spine_corruption_test.go` |
| Concurrency | Real independent writers, sessions, authority winners, and typed losers | `journal_concurrent_writers_test.go`, `journal_authority_revocation_race_test.go` |
| Recovery and migration | Fresh creation, reopen, legacy shape, migration, and recovery behavior | `journal_recovery_corpus_test.go`, `canonical_journal_contract_test.go` |
| Registered DBOS plus borrowed SQLite | Real `dbos.DBOSContext`, registered workflows/steps, and one borrowed physical SQLite file | `dbos_harness_test.go`, `dbos_matrix_test.go`, `dbos_retry_terminal_test.go`, `dbos_store_test.go` |
| Crash and subprocess | Process death at durable boundaries followed by production recovery | `dbos_crashgap_test.go`, `dbos_compilefail_test.go` |

Concrete local helpers are intentionally narrow. `internal/testcorpus.LoadYAML`,
`LoadCorpus`, `ScopeTable.Validate`, and `CheckPartition` own strict corpus
loading and membership. `buildStartupFixture`,
`buildValidatedStartupBaseline`, `checkpointAndCloseStartupFixture`, and
`writeStartupBaselineCopy` in `canonical_startup_matrix_test.go` own the one
approved immutable SQLite baseline lifecycle. `openSharedSQL` in
`dbos_crashgap_test.go`, plus `newDBOSStack`/`newDBOSStackUnlaunched` in
`dbos_harness_test.go`, own real registered DBOS and borrowed-SQLite setup.
`openFileDB` in `dbos_store_test.go` owns the smaller borrowed-store harness.
Keep helper scope at the package boundary rather than creating a second generic
test framework.

## Fixture contracts

Fixture loaders must decode into typed structures with `yaml.Decoder.KnownFields(true)`,
accept exactly one YAML document, reject duplicate case names or keys, and use
closed typed classifications/operators. `internal/testcorpus.LoadYAML`,
`LoadCorpus`, `ScopeTable.Validate`, and `CheckPartition` are the Provenance
reference implementation.

Counts are anti-vacuity guards, not proof of coverage. Assert exact identity
membership in both directions: every required case/operator is present and
every loaded case/operator is registered. Keep expected values independent of
the production calculation so the same defect cannot construct both sides of
an assertion.

Every negative case must start from a valid positive control and change one
intentional dimension. Loader and harness tests need synthetic-break/meta-gates
that demonstrate unknown fields, duplicate names, stale or missing registry
entries, and omitted assertions actually fail. Embedded fixture bytes are
immutable source data. Clone slices, maps, nested values, or files before any
mutation; each case receives a private mutable copy.

Schema's prior art is rooted at
`/home/minttea/dev/peasant-labs/schema/develop/TESTING.md` and
`/home/minttea/dev/peasant-labs/schema/develop/testcase/`; it supplies strict
typed fixture, freshness, synthetic-break, and clone patterns. Peasant's
relevant test infrastructure is
`/home/minttea/dev/peasant-labs/peasant/develop/internal/store/storetest/golden.go`,
`/home/minttea/dev/peasant-labs/peasant/develop/cmd/peasant/helpers_test.go`, and
`/home/minttea/dev/peasant-labs/peasant/develop/cmd/peasant/main_test.go`.
Provenance retains its own generic `internal/testcorpus` API and exact closed-set
checks rather than importing a test-only implementation.

## SQLite lifecycle

Use an immutable golden only when schema creation or migration is not the
behavior under test:

1. Build and migrate the complete fixture through production APIs.
2. Validate integrity and a production reopen.
3. Checkpoint WAL fully, close every connection, and prove the main database no
   longer depends on `-wal`, `-shm`, or rollback-journal files.
4. Capture the closed main-database bytes and digest as immutable source data.
5. Write those bytes to a unique path under each case's `t.TempDir()`.
6. Open each private copy with the narrow test-only raw zombiezen handle using
   existing-file read-write flags without `OpenCreate`; mutate, checkpoint,
   close, and prove sidecar absence and changed main-file bytes.
7. Call production `OpenSQLite` once on the corrupt copy and retain nil-tracker,
   typed/topology error, diagnostic-token, and failed-open byte-equality guards.

`canonical_startup_matrix_test.go` is the local example. It builds the full
startup fixture once per top-level test pass, validates a copied reopen, then
runs all 98 serial corruptions against private copies. The raw handle is test
preparation only; production rejection remains the behavior under test.

Peasant's exact prior-art file is
`/home/minttea/dev/peasant-labs/peasant/develop/internal/store/storetest/golden.go`.
It uses `sync.Once` to run production `store.Open` and migrations, closes the
store, byte-copies the closed main file per test, and reopens each copy with
`WithSkipMigrations`. Provenance adds stronger publication checks not present in
that Peasant helper: integrity verification before and after a production
reopen, explicit WAL checkpoint validation, sidecar absence proof, and source
digest pinning. Skipping repeated setup is safe only when schema behavior is
outside the test's contract; Provenance does not add a skip-migrations API.

Never share a live connection, mutable database, WAL, or SHM between tests. Do
not use a golden for fresh database creation, migration/upgrade, crash recovery,
or DBOS borrowed/shared-WAL tests. Those tests must exercise their real
lifecycle from scratch.

## Isolation and parallelism

Inject database, state, and output paths through existing production seams and
give every test private paths. Do not mutate the process working directory or
global environment to create isolation. Keep trackers, sessions, connections,
files, and mutable expected data private.

Add `t.Parallel()` only after proving there is no package-global mutation,
shared file or sidecar, shared DBOS root, hidden environment/cwd dependency, or
peak-RSS regression at CI concurrency. DBOS and shared-WAL tests remain serial
initially. Correctness and deterministic diagnostics take priority over lower
wall time.

Peasant's single-connection test pool, process-wide `TestMain` environment
setup, and broad parallelization are not directly transferable. Peasant's pool
size of one does not model registered DBOS readers and durable background work,
so Provenance DBOS harnesses use 16 open and 8 idle connections as a realistic
test and example configuration. This is a recommendation, not a required
minimum: `OpenBorrowedSQLite` accepts caller-owned pool limits as supplied and
never validates or mutates them, so smaller deliberate pools remain supported.
The shared WAL is still single-writer regardless of connection count. Path
injection and private copies transfer; global cwd/environment overrides do not.

## Performance measurement

Before changing test scheduling, fixture construction, DBOS polling, or CI
flags, read [Regression traps: what made the suite roughly four times slower](docs/test-performance.md#regression-traps-what-made-the-suite-roughly-four-times-slower).
That history is the checklist for avoiding previously measured regressions.

Measure before and after on the same host and with `GOMAXPROCS` set to CI's core
count. Use several runs and compare medians, not a single favorable result.

```bash
time CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -v -run '^TestName$' .
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '^TestName$' -cpuprofile=cpu.prof .
go tool pprof -top -cum cpu.prof
```

Sum the per-test durations from `-v` and compare them with wall time: similar
values identify a serial critical path; a much larger sum indicates overlap.
All comparisons use the race-enabled command because that is the supported test
configuration. Capture `/proc/<pid>/status` `VmHWM` (or an equivalent
process-tree peak-RSS measurement); Go heap profiles omit C allocations,
SQLite, and race shadow memory. Report command, commit/worktree state, Go
version, CPU, memory, `GOMAXPROCS`, wall/user/system time, package/test time, and
before/after medians.

Use one diagnostic profile at a time to minimize instrumentation distortion:

```bash
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '^TestName$' -cpuprofile=cpu.out .
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '^TestName$' -memprofile=mem.out .
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '^TestName$' -blockprofile=block.out .
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '^TestName$' -mutexprofile=mutex.out .
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '^TestName$' -trace=trace.out .
```

For debugging, add `-v` for human-readable events or `-json` for machine
processing. Use `-failfast` only during local diagnosis because it suppresses
later failures. Reproduce an order-dependent failure with the reported
`-shuffle=<seed>`, and use `-fullpath` when logs need actionable source paths.

## Waiting and retries

Production code must not use sleeps or private retry loops for contention.
Tests should use explicit readiness/condition signals with bounded timeouts as
failure ceilings rather than asserting elapsed sleeps. SQLite
`busy_timeout=5000` owns local lock waiting.
Borrowed operations make one inner attempt and return escaped `BUSY`/`LOCKED`
unchanged; DBOS step options own durable retry policy. Assert attempts,
callbacks, durable state, typed outcomes, and replay behavior, not elapsed time
or observed sleep intervals.
