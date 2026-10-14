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
`p50` / `p95` / `p99` / `p99.9`; `fiosummary` preserves `p50`, `p95`, `p99`,
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

## 6b. Transport court (F0 vs the closest honest alternative)

Court: `scripts/transport-court.sh`, run inside the court container by
`scripts/container-transport.sh`. It also runs the FUSE INIT negotiation probe
(`scripts/container-transport-probe.sh`, `cmd/fuseuringprobe`).

F2 (FUSE-over-`io_uring`) is **not reachable from pure Go via go-fuse**: this
kernel compiles in and runtime-enables `CONFIG_FUSE_IO_URING`, and the probe
shows it advertising `FUSE_OVER_IO_URING` in the INIT it sends (the follow-up
request then stops arriving on `/dev/fuse`), but `hanwen/go-fuse/v2` v2.12.0
exposes no io_uring transport at all — it only names the capability bit. See
[`TRANSPORT.md`](TRANSPORT.md). The court therefore compares the F0 shapes this
build *can* serve: the default multi-reader loop against `f0-single`.

Receipt: `evidence/performance/transport.json` (schema
`orfs.performance.transport.v1`). Measured (this host, 64 MiB file, 4K random
read, `iodepth=1`, ~8 s, `--direct-io`):

| mode | numjobs | p50 | p95 | p99 | p99.9 | IOPS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `f0` | 1 | 8,512 ns | 13,376 ns | 31,872 ns | 62,208 ns | 98,582 |
| `f0-single` | 1 | 8,256 ns | 11,712 ns | 29,312 ns | 50,944 ns | 103,612 |
| `f0` | 64 | 148,480 ns | 354,304 ns | 1,712,128 ns | 9,633,792 ns | 271,855 |
| `f0-single` | 64 | 366,592 ns | 536,576 ns | 22,151,168 ns | 69,730,304 ns | 85,096 |

At one reader the two are within noise; at 64 the multi-reader loop wins ~3.2× the
IOPS and ~13× the p99. SQ/CQ counters are not obtained (no io_uring in use) and
the receipt says so rather than inventing them; the kernel FUSE connection's
pending-request backlog (`waiting`) is reported instead.

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

## 7b. Cross-FS determinism court

Court: `scripts/crossfs-court.sh`, run inside a **privileged** court container
(loop devices + `mkfs`) by `scripts/container-crossfs.sh`.

Measures that the backing store cannot change the bytes authority writes. For
each of ext4, xfs, btrfs, and tmpfs the court creates a backing, initialises an
OrFS volume with a fixed UUID and a pinned `ORFS_CLOCK_EPOCH`, replays one
identical deterministic operation sequence, and hashes every authoritative
`or.log/*.seg` segment.

Measured: all four backings produced the **identical segment sha256**
`35bd5d57e1f85525238a13f1609d2c65737b37731ec14234634f4f5e8260d990`. A court where
fewer than two backings could be compared FAILs (a one-backed "agreement" proves
nothing), and a backing that cannot run is named, never counted as a pass.

Run:

```sh
sh scripts/container-crossfs.sh
```

Receipt: `evidence/correctness/cross-fs.json` (schema
`orfs.correctness.crossfs.v1`).

## 7c. POSIX conformance court (pjdfstest)

Court: the upstream [`pjdfstest`](https://github.com/pjd/pjdfstest) POSIX
system-call suite, built from source and run against an OrFS mount inside a
**privileged** court container by `scripts/container-pjdfstest.sh`.

Measures how much of the POSIX namespace surface the mount actually conforms to.
**The result is now clean:** 8798 assertions, **all 8798 passed, 0 failed**. The
receipt's `deviations` array is empty of real deviations; a passing run never
allowlists. The suite also passes cleanly on tmpfs; this run shows the OrFS mount
now tracks it.

A run that completes writes its receipt and exits 0 (failures would be deviations,
not a court failure); a `verify --deep` that reports `[FAIL]` after the run is a
real integrity failure and exits 1. The court is part of the
`container-verify.sh` gate.

Run:

```sh
sh scripts/container-pjdfstest.sh
```

Receipt: `evidence/performance/pjdfstest.json` (schema
`orfs.performance.pjdfstest.v1`).

## 7d. Group commit court

Court: `internal/log` (`TestGroupCommitCourt`), run inside the court container by
`scripts/container-go-courts.sh` with `ORFS_EVIDENCE=1`.

Measures publish-latency `p50`/`p99` and throughput for N concurrent
single-transaction writers against one `Log`, with group commit **OFF** (one
durability barrier per publish) versus **ON** (one `fsync` and one watermark
write per batch, bounded by a max count and a max wait). Each transaction keeps
its own committed root and its own contiguous sequence numbers, and the bytes
written are identical to a per-publish run — only the moment of the barrier
moves. The court also probes the uncontended cost of a single publish so the
reader can see how much headroom batching actually has.

Run:

```sh
sh scripts/container-go-courts.sh   # writes evidence/performance/groupcommit.json
```

Receipt: `evidence/performance/groupcommit.json` (schema
`orfs.performance.groupcommit.v1`).

Measured (this host, container overlay/ext4, 64 writers × 16 transactions,
`maxBatch=64`, `maxWait=2 ms`, metadata-sized transactions):

| arm | p50 | p99 | throughput |
| --- | ---: | ---: | ---: |
| OFF (barrier per publish) | 1,061,159 ns | 2,199,864 ns | 54,134 ops/s |
| ON (barrier per batch) | 236,601 ns | 571,937 ns | 233,352 ops/s |

That is **4.31× throughput and 4.49× better p50**. The win is honest, not the
ideal 64× a full batch barrier would give: on this backing an *uncontended*
single publish (encode + append + one `fsync`) costs only **17,943 ns**, so the
ON arm is CPU-bound at ~233k ops/s rather than barrier-bound. The win therefore
tracks the backing store's `fsync` cost — on a backing with millisecond `fsync`
it would approach the batch factor, and on `tmpfs`, where `fsync` is nearly free,
it would be small or absent. Stated plainly rather than inflated.

## 7e. Per-read allocation court

Court: `internal/namespace` (`TestAuthoritativeReadAllocs`, benchmark
`BenchmarkAuthoritativeRead4K`), run inside the court container by
`scripts/container-go-courts.sh` with `ORFS_EVIDENCE=1`.

Measures the steady-state **authoritative** read path (no manifest registered) —
`Namespace.Read` / `readUnchecked` — over a 1 MiB single-extent file on a
filesystem-backed volume, readers walked across the whole file. The destination
`[]byte` a `Read` returns is one unavoidable allocation; the court asserts it is
the **only** per-read allocation, so a change that reintroduces a per-read
planner allocation fails the court.

Measured (this host, unoptimised test build): **before 2 allocs/op** (the
destination plus the fresh `[]Piece` that `PlanRead` allocated — 4120 B/op) ->
**after 1 alloc/op** (the destination alone — 4096 B/op) at **~470 ns/op** for a
4 KiB read. The change is surgical: `ExtentMap.PlanReadInto` fills a
caller-owned slice and `readUnchecked` passes a small fixed-size array, so the
common single-extent read allocates nothing for the plan. The reference reached
2 allocs/read; this reaches 1.

Run:

```sh
sh scripts/container-go-courts.sh   # writes evidence/performance/allocs.json
```

Receipt: `evidence/performance/allocs.json` (schema `orfs.performance.allocs.v1`).

## 7f. Crash-consistency court (real process kill)

Court: `internal/log` (`TestCrashConsistencyCourt`), run inside the court
container by `scripts/container-go-courts.sh` with `ORFS_EVIDENCE=1`.

Measures what happens when a **real writer process is `SIGKILL`ed mid-append** —
not a clean shutdown and not the simulated device (`internal/powerloss`). A small
helper (`cmd/orfscrashtest`) opens a volume and publishes transactions (an
`INODE_CREATE` batch, one durability barrier per publish) in a tight loop until it
is killed. It installs no signal handler and performs no cleanup, so `kill -9`
terminates it exactly as a crash would.

Each iteration creates a fresh volume, spawns the writer, waits a random small
interval, `SIGKILL`s it, then reopens read-write (which quarantines the abandoned
tail) and asserts: the recovered root is the newest valid committed boundary; the
committed prefix is byte-identical to the bytes that survived the kill; every
abandoned byte is **moved** into `quarantine/` (never truncated); and
`verify --deep` passes. It then publishes one further transaction to prove the log
continues at the recovered root. Segments are sized (genesis frame + one
transaction) so every committed transaction ends at a segment boundary and each
append begins in a fresh trailing segment, so a mid-append kill always leaves a
movable tail.

Run:

```sh
sh scripts/container-go-courts.sh   # writes evidence/crash.json
```

Receipt: `evidence/crash.json` (schema `orfs.performance.crash.v1`).

Measured (this host, container overlay/ext4): **1000 kills**. **379** landed
mid-append and left an abandoned tail (**4** torn, **375** uncommitted); the other
**621** landed after a transaction's bytes were complete (e.g. during its fsync
barrier), so recovery reached that transaction with no tail. **All 379 tails were
quarantined (moved, never truncated)**, and **0 truncations, 0 prefix mismatches,
0 recovery failures** were observed: `verify --deep` passed all 1000 recovered
volumes and all 1000 continued with one further committed transaction at the
recovered root. This is a process kill, not power loss — bytes in the page cache
survive it, so device-cache reordering and torn sectors remain the domain of the
simulated `internal/powerloss` court.

## 7g. Go-runtime GC-scaling court

Court: `scripts/gc-scaling-court.sh` + `cmd/orfsrw` (workload driver) +
`orfs mount --profile-dir` (runtime dump) + `cmd/gcsummary` (merge), run inside
the court container by `scripts/container-gc-scaling.sh`.

Measures *what the Go runtime costs the daemon*. It drives a mounted volume with
**64 concurrent 4 KiB random readers plus 8 concurrent writers** (`cmd/orfsrw`:
exact application p50/p99/p99.9, throughput, errno-classified errors) while the
mount daemon, told `--profile-dir`, writes a windowed runtime-metrics dump on
shutdown: GC stop-the-world pauses (`/sched/pauses/total/gc:seconds`), scheduler
latency (`/sched/latencies:seconds`), allocation rate (`/gc/heap/allocs:bytes`),
lock wait (`/sync/mutex/wait/total:seconds`), and **real** resident memory
(`/proc/self/status` `VmRSS`/`VmHWM`), plus `heap.prof` and (with
`--profile-locks`) `mutex.prof`/`block.prof`. It sweeps `GOGC`
(`400/100/50/20/off`) and `GOMEMLIMIT` (`256MiB/512MiB/off`).

Run:

```sh
sh scripts/container-gc-scaling.sh
```

Receipt: `evidence/performance/gc-scaling.json` (schema
`orfs.performance.gc-scaling.v1`). Measured (this host, container, 9-arm
**bounded** spread, ~5 s per arm, one run, 2 GiB / 4 CPU):

| GOGC | GOMEMLIMIT | p50 | p99 | p99.9 | kIOPS | gcN | gcp99 | RSS MiB | mutex s |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 400 | off | 51.0 µs | 14.9 ms | 38.3 ms | 139.4 | 1046 | 57 µs | 30.2 | 110.9 |
| 100 | off | 71.4 µs | 9.3 ms | 21.8 ms | 130.3 | 6510 | 65 µs | 17.0 | 135.7 |
| 50 | off | 142.4 µs | 9.9 ms | 25.4 ms | 77.3 | 8501 | 328 µs | 12.9 | 156.1 |
| 20 | off | 210.7 µs | 4.6 ms | 14.8 ms | 95.3 | 13007 | 65 µs | 13.9 | 159.6 |
| 100 | 256MiB | 70.1 µs | 9.2 ms | 20.6 ms | 134.2 | 6603 | 65 µs | 18.7 | 132.2 |
| 100 | 512MiB | 71.3 µs | 9.6 ms | 21.3 ms | 130.6 | 6464 | 57 µs | 15.1 | 133.6 |
| off | 256MiB | 78.1 µs | 20.8 ms | 64.2 ms | 104.1 | 51 | 65 µs | **248.3** | 95.8 |
| off | 512MiB | 84.3 µs | 21.4 ms | **73.6 ms** | 98.2 | 23 | 49 µs | **496.1** | 87.9 |
| off | off | **daemon_died** | — | — | — | — | — | — | — |

Stated plainly: **GC pauses are not the tail** (p99 pause ≤ 328 µs everywhere);
the tail is the allocation rate (~2 GB/s) and a single shared `RWMutex` (88–160
goroutine-seconds of lock wait over ~6 s — ~15–27 goroutines blocked on
average), and `GOGC=off` with no memory limit is fatal (the daemon was
OOM-killed ~1.3 s in). Lower `GOGC` buys tail at the cost of median; `GOMEMLIMIT`
is inert until it binds. Full analysis and the GC-trace/lock-profile excerpts:
[`RUNTIME.md`](RUNTIME.md).

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
| transport (F0 vs f0-single; F2 negotiation probe) | F2 not reachable via go-fuse; kernel supports it (probed) — F0-single costs ~3.2× IOPS and ~13× p99 at 64 readers | `evidence/performance/transport.json` |
| scan-resistant admission | S3-FIFO 95.2% vs FIFO 88.0% vs LRU 95.2% | `evidence/performance/cache.json` |
| capacity guard | typed `CapacityExhausted` before the first byte | `evidence/recovery/capacity.json` |
| quarantine | torn tail moved, never truncated | `evidence/recovery/quarantine.json` |
| cross-FS determinism | identical segment sha256 across ext4/xfs/btrfs/tmpfs | `evidence/correctness/cross-fs.json` |
| pjdfstest conformance | 8798/8798 assertions pass; 0 deviations | `evidence/performance/pjdfstest.json` |
| group commit | 4.31× throughput / 4.49× better p50 vs a barrier per publish (bounded by the backing store's fsync cost) | `evidence/performance/groupcommit.json` |
| per-read allocations | authoritative 4 KiB read: 2 -> 1 allocs/op (~470 ns/op); the returned `[]byte` is the only allocation | `evidence/performance/allocs.json` |
| crash consistency (real `SIGKILL`) | 1000 kills mid-append: 379 abandoned tails (4 torn / 375 uncommitted), all quarantined (moved, never truncated); 0 truncations / 0 prefix mismatches / 0 recover failures; `verify --deep` clean | `evidence/crash.json` |
| Go-runtime GC scaling | 9-arm GOGC×GOMEMLIMIT sweep under 64 readers + 8 writers: p99.9 14.8 ms (`GOGC=20`) … 73.6 ms (`GOGC=off`+512MiB); GC pauses ≤ 328 µs (not the tail); ~2 GB/s allocation; `GOGC=off`+no limit OOM-killed ~1.3 s in; lock wait dominant | `evidence/performance/gc-scaling.json` |

Explicitly not yet measured (absence is deliberate, not implied):

| Frontier | State |
| --- | --- |
| F2 FUSE-over-`io_uring` transport | **not reachable from pure Go via go-fuse** — the kernel supports it (probed live), the linked library does not; the measured F0 comparison is published (`TRANSPORT.md`, §6b) |
| Multi-day amplification curve; profile selection by measured worth | not run |
| Mount latency after a large (2^30-byte) history | not run (`OPERATIONS.md` §3) |

No performance superiority is claimed beyond the measured planner p50 figure and
the inode-spine measurements. The Go-runtime court measures the daemon's own
cost; it does not claim the port is faster than a native filesystem (see
[`RUNTIME.md`](RUNTIME.md) §6).

## 9. Reproduce

Every court above is one script, one command, one receipt. See
[`REPRO.md`](REPRO.md) for the container-only procedure, the `/dev/fuse` and
capability requirements, and the exact receipt paths.
