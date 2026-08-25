# Parallelising the governed-allocation family

Record for the change that added `t.Parallel()` to the 58 remaining serial
top-level tests of the `governed_allocation_*_test.go` files, to the lone serial
test of `dbos_matrix_test.go`, and to the three tests of
`dbos_assignment_transfer_test.go`.

Host and toolchain are the ones in [README.md](README.md), with one difference
that matters for every number here: **two unrelated test-optimization workers
were running their own suites on the same machine throughout.** Treat all wall
times below as provisional. The peak-RSS figures and the flake attribution are
comparative measurements taken under that same shared load, so they remain
meaningful; the absolute seconds do not.

## Peak resident set

`/usr/bin/time -v` is not in the development shell, so peak process-tree RSS was
sampled from `/proc` (the sum of `VmRSS` over the `go test` process tree, sampled
every 100 ms, maximum retained).

| Run | `GOMAXPROCS` | `-parallel` | Wall | Peak tree RSS |
|---|---:|---:|---:|---:|
| Root package, race, before | 16 | default | 307s | 778 MiB |
| Root package, race, after | 16 | default | 174s | 1022 MiB |
| Root package, race, after | 16 | default | 174s | 1347 MiB |
| Root package, race, after | 32 | default | 180s | 1273 MiB |
| Root package, race, after | 16 | 4 | 229s | 1301 MiB |

Peak RSS rose from 778 MiB to a 1.0-1.35 GiB band. **It is not a function of the
parallel width.** Holding `GOMAXPROCS` at 16 and bounding the runner to
`-parallel=4` cost 55 seconds of wall time and did not lower the peak at all
(1301 MiB), and doubling `GOMAXPROCS` to 32 did not raise it (1273 MiB). The
band is run-to-run variance under the race detector, whose shadow memory tracks
cumulative allocation rather than concurrent liveness.

A `-parallel` bound was therefore **not** adopted: the measurement shows it buys
no memory and costs time. If a future runner does need the ceiling lowered,
`-parallel` is the lever, but it must be re-measured rather than assumed to work.

## Pre-existing load flake found while gating this change

Under a loaded host the fused governed-allocation tests fail intermittently with:

```
RunInitializeRoot: retrieve DBOS workflow result: DBOS Error WorkflowExecutionError:
Workflow <name>-genesis-workflow execution error: decoding step result to expected
type allocation.OperationClosure: failed to decode json data: decode governed
operation closure: unsupported or structurally incomplete closure
```

That message is misleading. The real failure is visible one line earlier in the
same log:

```
WARN workflow error type cannot be gob-encoded; persisting its message only:
  error="insert governed operation journal anchor: database is locked (5) (SQLITE_BUSY)"
WARN Retrying transient sqlite error
  error="insert governed operation journal anchor: database is locked (5) (SQLITE_BUSY)"
```

The chain is:

1. The genesis step loses the single-writer lock on its own database file and
   fails with a transient `SQLITE_BUSY`. The other writer is that same
   allocator's DBOS queue runner, which is the one-second tick this epic is
   already chasing.
2. DBOS checkpoints the step with an empty output and the error string.
3. The retry reads that checkpoint back. In
   `dbos-transact-golang v0.20.0`, `RunAsStep` (`dbos/workflow.go:2064-2073`)
   decodes the recorded output *before* it looks at the recorded step error, and
   returns the decode failure while discarding the error it was handed. An empty
   output cannot decode into an `OperationClosure`, so a transient, retryable
   lock conflict is reported as a permanent structural corruption of the result.

**This flake is not caused by parallelising the family.** It was attributed by
running the family-only suite eight times alternating between the parallelised
tree and the unmodified baseline, in the same shell, under the same host load:

| Tree | Failing runs |
|---|---:|
| With `t.Parallel()` | 4 / 8 |
| Baseline (all serial) | 3 / 8 |

The failure signature is identical on both sides, and the failure rate is also
flat across runner widths on the parallelised tree (2/6 at `-parallel=1`, 1/6 at
2, 0/6 at 4, 2/6 at 8). A serial run is not protected, so reverting the
annotations would not fix it. The two follow-ups it implies are outside this
change: removing the queue-runner tick contention, and reporting the swallowed
step error upstream.
