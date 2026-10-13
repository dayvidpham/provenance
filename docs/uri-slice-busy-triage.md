# URI slice: SQLite BUSY baseline triage

The URI-SLICE-1 review reported two failures from full race suites:

| Review | Fusedtx shuffle seed | Failure |
| --- | --- | --- |
| B | `1791857067084600145` | `TestExactSystemHandleCommitsApplicationAndCheckpoint`, `fusedtx_integration_test.go:46` |
| C | `1791857112652906894` | `TestSecondHandleCannotBecomeFusedDataSource`, `fusedtx_integration_test.go:135` |

Both returned `database is locked (5) (SQLITE_BUSY)` from workflow result
retrieval. The review logs are `/tmp/opencode/uri-review-b-race.log` and
`/tmp/opencode/review-c-uri-tests.log`; neither failure is discarded by this
investigation. This family is already tracked by
[provenance #44](https://github.com/dayvidpham/provenance/issues/44), which records
the family's pre-existing contention disposition and main/activation-track
parity evidence. No
failure rate or specific SQLite extended result code is inferred here.

## Matched package baseline

The baseline is fetched `origin/main` at
`6529af8728a382b82094adeefc77ef0b4ab895ce`, in the clean worktree/branch
`provenance-42--test--busy-baseline`. The URI branch's production revision is
`51e60945e5f6eae860f71964fa525f2e9e5efeb1`. Both use **Go 1.26.8 linux/amd64**
through `GOTOOLCHAIN=go1.26.8` in the same Nix dev shell. The shell banner says
Go 1.26.1; `/tmp/opencode/uri44-toolchain.log` records the actual overridden
compiler version. CGO and the race detector are enabled in every test.

Each cell below is one package invocation for one reported seed, not a loop or
`-count` repetition. Main and branch commands ran sequentially, with the same
flags and no artificial competing writer:

```sh
nix develop --command env GOTOOLCHAIN=go1.26.8 CGO_ENABLED=1 \
  go test -race -shuffle=<reported-seed> -fullpath -timeout=20m ./internal/fusedtx
```

| Target | Seed `1791857067084600145` | Seed `1791857112652906894` |
| --- | --- | --- |
| Main `6529af8` | PASS, 4.439s | PASS, 4.413s |
| URI `51e6094` | PASS, 4.384s | PASS, 4.356s |

Raw logs: `/tmp/opencode/uri44-{main,branch}-seed-<seed>.log`.
The recorded failures **were not reproduced** by these matched focused runs.
A shuffle seed reproduces test order, not SQLite writer interleaving or host
load. These passes are observations, not determinism or an environment-only
waiver of the full gate.

`git diff origin/main -- internal/fusedtx internal/dbossys go.mod go.sum
flake.lock` is empty. The URI parser, storage activation and conversion changes
are not attributed to this unchanged family merely because the full-suite
review failed. The distinct release metadata in `flake.nix` does not change
these Go sources, module pins or locked Nix inputs.

Full-suite baseline and authoritative URI results are recorded below only when
their separate invocations finish. No assertions, retry options, SQLite waits,
test concurrency flags, sleeps or package exclusions are changed to obtain a
passing result; issue #44 remains open for owner-level contention hardening.

## Structural disposition

The fusedtx test binary reaches `internal/fusedtx`, `internal/dbossys`, the
stdlib-only `internal/dbosfixture`, DBOS v1.2.0 and modernc SQLite v1.54.0. It
does **not** reach `pkg/ptypes`, `internal/sqlite` or the URI conversion owners.
Both callbacks insert directly into `fused_sentinel`. Consequently these
failures do not establish a URI code-path regression. Different whole-suite
workloads can still change scheduling/load and expose a pre-existing defect.

The existing owner boundary explains a contention hazard, without claiming a
specific extended error code from the observed generic BUSY:

- `fusedtx.OpenSystem` opens its caller pool using the supplied DSN. The test DSN
  has `busy_timeout(5000)` and WAL but no `_txlock=immediate`.
- `fusedtx.Run` calls `dbos.RunAsTransaction` without step options. Its exact
  shared system/data-source pool selects DBOS's shared `runAsTxn` path, with
  zero default callback retries (`dbos/workflow.go`, `prepareStepExecution` and
  `runAsTxn`). DBOS's internally opened SQLite pool adds immediate transaction
  mode; a supplied pool is used as-is (`internal/sysdb/sqlite_pool.go`).
- The shared path begins a transaction, reads `CheckOperationExecution`, then
  calls the writing callback. A deferred read-to-write promotion can contend
  with DBOS's launched queue supervisor/worker writes. SQLite's busy timeout
  does not make every promotion retryable in place; the owner must acquire the
  appropriate transaction mode or retry a fresh transaction.
- The shared path can persist a callback failure in `operation_outputs` and
  commit it before its outer transaction-conflict retrier runs. A replayed error
  is no longer the original SQLite driver error. This leaves an owner-level
  retry/checkpoint hazard; it is not fixed by adding a sleep or retrying the
  test. Production fused entry points share this boundary.

These unchanged semantics belong to #44 / the fused DBOS transaction owner.
This investigation does not patch them, declare them harmless, waive a review
BLOCKER, or estimate their frequency. The matched seed attempts did not
reproduce the failure; the existing #44 disposition, identical linked source
and structural trace support **pre-existing contention, not an established URI
regression**. A successful full gate is separate evidence, not a fix for #44.

## Full-suite observations

The single full main diagnostic used the second review's fusedtx seed globally:

```sh
nix develop --command env GOTOOLCHAIN=go1.26.8 CGO_ENABLED=1 \
  go test -race -shuffle=1791857112652906894 -fullpath -timeout=20m ./...
```

Main `6529af8`: **PASS**, root 182.738s, fusedtx 4.980s; log
`/tmp/opencode/uri44-main-full-seed-1791857112652906894.log`. This fixes test
order, not the original review's host scheduling or other packages' random seeds.
The baseline run completed before launching the URI authoritative full gate;
the two full suites did not run concurrently.

URI authoritative gate: **PASS**, root 181.567s, fusedtx 4.643s, SQLite 58.495s:

```sh
nix develop --command env GOTOOLCHAIN=go1.26.8 CGO_ENABLED=1 \
  go test -race -shuffle=on -fullpath -timeout=20m ./...
```

Log: `/tmp/opencode/uri44-branch-authoritative-full-race.log`. This was one
authoritative invocation, with no retry after a failure. `go vet ./...`,
`CGO_ENABLED=0 go build ./...`, the configured ast-grep scan, Nix evaluation
(`nix flake check --no-build`) and `git diff --check` also passed. Static/Nix
logs: `/tmp/opencode/uri44-static.log`, `/tmp/opencode/uri44-nix.log`.

The prior corpus exception is withdrawn/superseded. S1 acceptance still depends
on [PROPOSAL-17](https://github.com/dayvidpham/pasture--01a133dc-d614-74f9-90a7-7dfe402a3fe8)
landing genuine definition writers and its approved complete-envelope
occurrence, followed by nonzero coverage consumption and independent review.
The legacy archive and manifest are unchanged, including archive SHA-256
`e2ec1b5721624ab80aa9d04f7283d5ae3fc4d403ace9fab49616056cb0e27fc0`.
