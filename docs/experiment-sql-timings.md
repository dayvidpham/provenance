# Private transaction timing experiment

This unpublished lane adds no exported API or default configuration. Build with
`-tags pasture_experiment` **and** set `PASTURE_EXPERIMENT_SQL_TIMINGS` to an
absolute private JSONL path. Without either opt-in the helpers do no recording
or I/O. Use a separate path per process; existing files must be regular and have
no group/other permission bits. New files are mode 0600. Keep the parent directory
private. No SQL, payload, domain identity, database path, or error text is emitted.
Do not enable this on an operator/live store.

The normal wrappers have nil hooks; only experiment-tagged package initialization
installs the recorder factory and artifact writer. Shared declarations preserve
the unchanged SQL architecture guard, which type-checks all production files
without build-tag filtering. No runtime registration API is exposed.

Each JSONL line has exactly these fields (all times are integer nanoseconds):

```json
{"pid":123,"op":1,"method":"Apply","phase":"commit","start_ns":1000000000,"end_ns":1000000010,"outcome":"ok"}
```

`pid` + process-local monotonic `op` identifies an invocation, not a domain
operation. `method` is `Apply` (the shared prepared-operation owner, including
assignment transfer) or `ApplyBatchContext`. Recording starts after pure input
preparation, before `bindScope`. Invalid/unprepared input has no records.
Intervals use a wall-clock start plus monotonic elapsed time; cross-process
alignment therefore depends on the host wall clock. File order is completion
order, **not start order**. Sort by start if needed.

| Phase | Actual owner boundary |
| --- | --- |
| `sql_conn` | Immediately around `database/sql.DB.Conn`, excluding context creation and scope admission/PRAGMA setup |
| `begin_attempt` | Each actual transaction-opening `ExecContext(BEGIN...)`, including failed attempts |
| `begin_wait` | First attempt through successful BEGIN return, or terminal failure/cleanup; includes existing retries, backoff, rearming, and scheduler time |
| `begin_probe` / `rollback_probe` | Existing zero-budget contention evidence probe, separate from normal attempts |
| `rollback_begin_cleanup` | Existing cleanup following a failed BEGIN; error does not imply a transaction was acquired |
| `transaction_scope` | Successful BEGIN return through successful COMMIT return, or through rollback return on failure |
| `body` | Invocation/return of the transaction closure, excluding COMMIT and outer rollback |
| `fold` | Each prepared fold, including its savepoint cleanup, replay/conflict handling and nested gates |
| `gate_subtype_integrity`, `gate_actor_placement`, `gate_fact_context_integrity` | Invocation/return of the respective existing gate |
| `commit` / `rollback` | Immediately around the actual outer transaction SQL call |

Ordinary call outcomes are `ok`, `busy`, `canceled`, `deadline`, or `error`.
Transaction outcomes are `committed`, `rolled_back`,
`commit_error_rolled_back`, or `rollback_error`. Failed BEGIN has no
`transaction_scope`. A failed COMMIT acknowledgement does not prove no commit;
even a subsequent failed rollback can mean an uncertain durable outcome.
Panics are not an experimental lane scenario and are not classified as errors.

The hierarchy overlaps: do **not** add body + folds + gates. Gates can be nested
inside folds, while batch-wide gates run inside body after all folds. Body less
the union of its children is the remaining closure work (including ledger work).
`begin_wait` overlaps attempts/cleanup, not the successfully acquired transaction.
Instrumentation bookkeeping and scheduler time are included; this is not a
zero-overhead profiler. The BEGIN calls measure driver + SQLite busy handling +
Go scheduling, not isolated SQLite wait. `transaction_scope` on BEGIN IMMEDIATE
is an observed upper-bound **proxy** for exclusive SQLite writer reservation,
not a strict physical-lock-duration measurement: acquisition and any lock
residence before BEGIN returns are outside it, while COMMIT can release the lock
before returning. Neither whole-turn elapsed time nor COMMIT-call elapsed time
is an fsync attribution. Deferred BEGIN or failed attempts cannot be interpreted
as acquired writer reservation.

Events accumulate in the synchronous operation's private context recorder.
JSON encoding, opening and appending occur only after the transaction has
completed and its connection scope has released. One process-wide mutex protects
JSONL framing for concurrent daemon callers; it protects no database invariant.
The synchronous flush can delay API return, but is outside transaction timing.
Private-artifact I/O failures do not change the operation result, so the lane
driver must reject missing/truncated operations rather than infer zero latency.
Interrupted processes can lose their entire buffered invocation. Flush has no
fsync guarantee. The batch/flush experiment driver and workload accounting remain
consumer-owned; these events do not change batch sizes, flush delays, retries,
integrity guards, or production defaults.
