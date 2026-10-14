# OrFS-GO Benchmark Frontier

**Status:** courts exist for what is measured; everything else is named as not yet
measured. No claim outruns the evidence in the repository.

A *court* is an executable test or script that produces a machine-readable
*receipt* under `evidence/`. A court reports honestly: a bounded run says so, a
target that cannot run is named and skipped rather than counted as a pass, and a
number that was not measured is not published.

The law this file serves: **authority establishes truth; a concealed structure
only accelerates observation of it.** Benchmarks here therefore measure
observation speed and the space it costs, never a change to what the bytes say.

## 1. How a court reports

- Receipts are JSON (or plain text for the micro-benchmarks) under `evidence/`.
- Go courts write their receipt only when `ORFS_EVIDENCE=1`, so an ordinary
  `go test ./...` does not churn tracked evidence.
- Mounted courts run only inside the pinned court container (see
  [`REPRO.md`](REPRO.md)).
- A bounded run is labelled bounded. `evidence/performance/amplification.json`
  is a 24-epoch laboratory run, not a multi-day curve.

## 2. Growth / amplification court (Law I)

Court: `internal/orbench`, `TestAmplificationGrowsMonotonically`.

Measures: each epoch overwrites one file, so the old `DATA_EXTENT` bytes are
retained and the authoritative footprint grows while the logical content stays
one payload. Two arms are compared: **authoritative** (`or.log` alone) and
**manifested** (`or.log` plus Plane C bodies). The court asserts non-withdrawal:
the authoritative physical footprint rises strictly every epoch, the
amplification ratio never falls, and the manifested arm spends strictly more.

Run:

```sh
sh scripts/container-go-courts.sh   # runs the Go courts with ORFS_EVIDENCE=1
```

Receipt: `evidence/performance/amplification.json` (schema
`orfs.performance.amplification.v1`).

Measured (this host, unoptimised test build): **24 epochs, 16 KiB payload, 4 MiB
segments**. Authoritative amplification (log bytes / logical bytes) rose
**1.07 -> 24.56**. The manifested arm spent strictly more: **811,109 B total
physical against 16,384 B logical**. The run is bounded; it is not a multi-day
curve.

## 3. Concealed read planner court (Plane C)

Court: `internal/manifest/planner_test.go`
(`TestPlannerSelectionAndCorrectness`, `TestPlannerLatency`).

Measures that a manifested read is byte-identical to authority and faster on the
median.

- **Correctness:** 200 pseudo-random ranges (offset + size) over a 256 KiB
  corpus read manifested and authoritative; every range byte-identical; zero
  misses/refusals on a freshly certified body; a stale identity is a fallback,
  not a served body.
- **Latency:** 1 MiB file, 4 KiB random reads: manifested **p50 ~240 ns** versus
  authoritative **~480 ns** (~2x better p50). **p99 was within noise and is not
  claimed.**
- **Amplification:** the default root-profile body transforms ~**1.00x** the
  logical bytes (it is essentially a full history).

Run: the court is part of the default suite.

```sh
sh scripts/container-test.sh   # go vet + go test ./...
```

Receipt: none. The court prints its numbers; `evidence/` is not written. See
[`MANIFESTATION.md`](MANIFESTATION.md) §7.

## 4. Sealed inode spine court

Court: `internal/inodex` (`TestSpineIsMuchSmallerThanIndex`, `BenchmarkSpineGet`,
`BenchmarkMapGet`).

Measures the immutable historical inode role as a dense, sealed, ordinal spine
against a pointer-rich `map[uint64]*index.Inode`.

- Footprint: **16 bytes/inode**, measured **10.5x smaller** than a full
  `index.Inode` struct.
- Lookup: **~0.19 ns** versus **~7.5 ns** per lookup against a map (~40x).

Run:

```sh
sh scripts/container-test.sh   # the size court
sh scripts/container-bench.sh  # BenchmarkSpineGet / BenchmarkMapGet -> bench.txt
```

Receipt: `evidence/performance/bench.txt` for the micro-benchmarks; the size
ratio is a test log.

## 5. External baseline court (the FUSE tax, published)

Court: `scripts/baseline-court.sh`, run inside the court container by
`scripts/container-baseline.sh`.

Measures `fio` 4K random reads, `iodepth=1`, against three targets **on the same
box**:

1. OrFS, mounted with `--direct-io`;
2. the container filesystem (ext4/overlay);
3. a 512 MiB `tmpfs`.

Run:

```sh
sh scripts/container-baseline.sh
```

Receipt: `evidence/performance/baseline.json` (schema
`orfs.performance.baseline.v1`), carrying the `p50` / `p99` / `p99.9`
completion latencies per target.

**A FUSE tax is expected and is published, not hidden.** Measured on this host
(container, 64 MiB file, 4K random read, iodepth=1, ~8 s, `--direct-io` for OrFS):

| Target | p50 | p99 | p99.9 | IOPS |
| --- | ---: | ---: | ---: | ---: |
| OrFS (F0, `--direct-io`) | 8,896 ns | 28,544 ns | 55,552 ns | 102,195 |
| container fs (ext4/overlay) | 370 ns | 644 ns | 1,020 ns | 1,911,238 |
| tmpfs | 302 ns | 604 ns | 948 ns | 2,188,104 |

That is **~24x worse p50 and ~44x worse p99** than the container filesystem — the
FUSE tax, stated plainly. This is an unoptimised daemon (every read crosses the
FUSE boundary into a Go process that takes a lock and walks an extent map). It is
not a claim of parity; it is the honest floor an auditor should start from.

Documented caveat: `tmpfs` rejects `O_DIRECT`. The court retries that one target
buffered and prints
`NOTE: <label>: O_DIRECT refused by the backing store; ran buffered I/O`; the
fallback is reported, never hidden.

## 6. 64-thread scaling court

Court: the same `scripts/baseline-court.sh`, second pass at `numjobs=64`.

Measures the identical 4K random-read workload at 64 concurrent readers,
emitting completion-latency percentiles at that concurrency. The design target is
`p50` / `p95` / `p99` / `p99.9`; `fiosummary` currently preserves `p50`, `p99`,
`p99.9`, and `p99.99`.

Receipt: `evidence/performance/scale.json` (schema
`orfs.performance.scale.v1`). Measured on this host at `numjobs=64`:

| Target | p50 | p99 | p99.9 | IOPS |
| --- | ---: | ---: | ---: | ---: |
| OrFS (F0, `--direct-io`) | 95,744 ns | 593,920 ns | 1,810,432 ns | 472,024 |
| container fs (ext4/overlay) | 422 ns | 1,336 ns | 2,512 ns | 11,327,140 |
| tmpfs | 270 ns | 1,096 ns | 2,008 ns | 14,576,468 |

OrFS reaches ~472k IOPS at 64 readers against ~11.3M for the container
filesystem: the daemon does not scale like a native stack. Stated honestly.

## 7. fsstress-equivalent court

Court: `scripts/fsstress-court.sh` + `cmd/orfsstress`, run inside the court
container by `scripts/container-fsstress.sh`.

Measures a bounded, seeded, native operation storm against the mounted namespace
(defaults: 8 workers, 15 seconds, seed 1) across
create/write/read/unlink/mkdir/rmdir/rename/link/symlink/xattr/truncate/chmod,
plus the POSIX extras the namespace claims:

- symlink + readlink round trip;
- hard-link `nlink` reaching 2;
- `user.*` xattr set/get;
- a directory hard-link refused with `EPERM`.

It then unmounts cleanly and **gates on `verify --deep`**: a `[FAIL]` line fails
the court. An unexpected errno during the storm fails the court, and the receipt
is kept.

Run:

```sh
sh scripts/container-fsstress.sh
```

Receipt: `evidence/performance/fsstress.json` (the `orfsstress` histogram of
operations and classified errors).

## 7a. Scan-resistant admission court

Court: `internal/fcache` (`TestScanResistance`).

Reproduces the reference volatile-cache court: one identical scan-heavy key trace
(512-key scan interleaved with a 20-key hot set, 10,752 touches, capacity 32)
replayed through three eviction policies, differing only in that policy.

Measured (this host): **S3-FIFO 95.2%**, **FIFO 88.0%**, **LRU 95.2%** —
reproducing the reference numbers exactly.

Honest framing: this is a **hit-rate** measurement, not a latency one. On this
interleaved trace admission **matches** LRU (the hot set is re-referenced every
scan step) and the win is over the insertion-ordered FIFO, which the scan
flushes. No latency win is claimed for admission.

Run:

```sh
sh scripts/container-go-courts.sh   # writes evidence/performance/cache.json
```

Receipt: `evidence/performance/cache.json` (schema `orfs.performance.cache.v1`).

## 8. The frontier: measured versus not yet measured

Measured today (all bounded, laboratory conditions):

| Court | What | Receipt |
| --- | --- | --- |
| amplification growth | authoritative amplification 1.07 -> 24.56 over 24 epochs | `evidence/performance/amplification.json` |
| planner correctness | 200 ranges byte-identical to authority | none (test log) |
| planner latency | p50 ~2x better; p99 not claimed | none (test log) |
| inode spine | 10.5x smaller; ~40x faster than a map | `evidence/performance/bench.txt` |
| fsstress | bounded storm + POSIX extras + `verify --deep` | `evidence/performance/fsstress.json` |
| external baseline | 4K randread p50/p99/p99.9 vs ext4/tmpfs | `evidence/performance/baseline.json` |
| 64-thread scaling | 4K randread at 64 readers | `evidence/performance/scale.json` |
| scan-resistant admission | S3-FIFO 95.2% vs FIFO 88.0% vs LRU 95.2% | `evidence/performance/cache.json` |
| capacity guard | typed `CapacityExhausted` before the first byte | `evidence/recovery/capacity.json` |
| quarantine | torn tail moved, never truncated | `evidence/recovery/quarantine.json` |

Explicitly not yet measured (absence is deliberate, not implied):

| Frontier | State |
| --- | --- |
| F2 FUSE-over-`io_uring` transport; the F0-vs-F2 p50/p95/p99/p99.9 publication | not wired; F0 today (`LIMITATIONS.md` §6) |
| Cross-FS determinism (ext4/xfs/btrfs/tmpfs identical fingerprints) | not run (needs privileged loop mounts) |
| Multi-day amplification curve; profile selection by measured worth | not run |
| Mount latency after a large (2^30-byte) history | not run (`OPERATIONS.md` §3) |
| Group commit | not implemented |

No performance superiority is claimed beyond the measured planner p50 figure and
the inode-spine measurements.

## 9. Reproduce

Every court above is one script, one command, one receipt. See
[`REPRO.md`](REPRO.md) for the container-only procedure, the `/dev/fuse` and
capability requirements, and the exact receipt paths.
