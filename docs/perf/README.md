# Test-performance measurement records

Raw `go test -json` streams and the timing inventories derived from them, kept
so later optimization work can diff against a fixed reference rather than a
remembered number. Narrative analysis lives in
[../test-performance.md](../test-performance.md); the gate definitions live in
[../../TESTING.md](../../TESTING.md).

`docs/` is excluded from the shipped-vocabulary scan in `hygiene_test.go`, so
files here are records, not test fixtures, and no test reads them.

## Host and toolchain for every record here

| Property | Value |
|---|---|
| CPU | AMD Ryzen 9 7950X3D, 16 cores / 32 threads |
| Go | `go1.26.1 linux/amd64`, Nix development shell |
| Base commit | `8bf1a9b` (release v0.0.4) |
| Runner flags | defaults: no `-cpu`, `-p`, or `-parallel` |

`GOMAXPROCS` is the host's 32 threads, not CI's core count. A 16-core CI runner
should be expected to take roughly twice these wall times; the reported
900-second figure is consistent with the 418-second race baseline here.

## Records

| File | Suite | Wall |
|---|---|---:|
| `baseline-plain.json.gz` / `inventory-baseline-plain.md` | `go test -count=1 ./...` | 78s |
| `baseline-race.json.gz` / `inventory-baseline-race.md` | `CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -count=1 ./...` | 418s |
| `after-parallel-forged-receipts-race.json.gz` / `inventory-after-parallel-forged-receipts-race.md` | race, at commit `19e29e6` | 291s |

[parallel-governed-allocation-family.md](parallel-governed-allocation-family.md)
records the peak-RSS measurements, the `-parallel` bounding experiment, and the
pre-existing load flake found while parallelising the rest of that family. It has
no `go test -json` stream: its host was shared with two other test-optimization
workers, so its wall times are provisional and only its comparative measurements
are load-independent.

The `-count=1` and `-json` flags are measurement instrumentation. They are not
part of the authoritative gate, which specifies no `-count`.

## Reproducing

```bash
go test -count=1 -json ./... > plain.json
CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -count=1 -json ./... > race.json
```

Each inventory reports per-package elapsed time, the split between serial and
`t.Parallel()` top-level tests in the root package, the serial-phase cost
attributed by file, DBOS context launches per test, and every top-level test
over one second.

The serial figure is the one to watch: Go runs a package's non-parallel
top-level tests one at a time before the parallel ones, so the sum of serial
elapsed times is a floor on the package's wall time no matter how many cores
the runner has.
