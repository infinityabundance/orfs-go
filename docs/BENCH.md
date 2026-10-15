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

Receipt: none for the planner court itself; the resident-body admission court
(`TestManifestAdmissionScanResistance`, §7h) writes
`evidence/performance/manifest-admission.json`. See
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

> **Framing — these are zero-byte payloads, not durable appends.**
> `groupcommit.json` runs with `payload_bytes: 0`, so the 233,352 ops/s figure is
> an **internal batched-publish** rate (transaction framing, commit root
> publication, and the barrier), **not** a durable 4 KiB `write`+`fsync` append
> rate. It answers *"how much does coalescing the barrier win?"*, not *"how fast
> can this filesystem durably append 4 KiB?"*. The durable append+fsync numbers
> are §7i (the mounted head-to-head) and
> [`WORM.md`](WORM.md) / [`AUDIT-SINK.md`](AUDIT-SINK.md). Do not read 233k as an
> append rate.

Run:

```sh
sh scripts/container-go-courts.sh   # writes evidence/performance/groupcommit.json
```

Receipt: `evidence/performance/groupcommit.json` (schema
`orfs.performance.groupcommit.v1`).

Measured (this host, container overlay/ext4, 64 writers × 16 transactions,
`maxBatch=64`, `maxWait=2 ms`, metadata-sized transactions):

| arm | p50 | p99 | throughput (zero-byte payloads) |
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
**bounded** spread, ~5 s per arm, one run, 2 GiB / 4 CPU), **after** the global
read lock was removed ([`CONCURRENCY.md`](CONCURRENCY.md)):

| GOGC | GOMEMLIMIT | p50 | p99 | p99.9 | kIOPS | gcN | gcp99 | RSS MiB | mutex s |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 400 | off | 30.8 µs | 0.4 ms | 45.1 ms | 333.1 | 1287 | 65 µs | 26.3 | 93.1 |
| 100 | off | 43.2 µs | 9.6 ms | 21.4 ms | 287.7 | 7820 | 65 µs | 17.4 | 55.5 |
| 50 | off | 288.3 µs | 1.7 ms | **3.9 ms** | 216.0 | 14291 | 65 µs | 15.6 | 24.6 |
| 20 | off | 310.8 µs | 2.0 ms | 6.4 ms | 187.5 | 14081 | 82 µs | 15.2 | 16.0 |
| 100 | 256MiB | 43.7 µs | 10.1 ms | 20.9 ms | 286.5 | 9874 | 65 µs | 15.6 | 55.9 |
| 100 | 512MiB | 46.3 µs | 10.4 ms | 21.4 ms | 273.5 | 9430 | 65 µs | 15.9 | 61.9 |
| off | 256MiB | 32.7 µs | 0.1 ms | 76.1 ms | 316.0 | 62 | 57 µs | **249.9** | 18.6 |
| off | 512MiB | 33.4 µs | 0.2 ms | **78.2 ms** | 312.2 | 30 | 98 µs | **493.0** | 24.3 |
| off | off | **daemon_died** | — | — | — | — | — | — | — |

Stated plainly: **GC pauses are still not the tail** (p99 pause ≤ 98 µs everywhere);
the tail is the allocation rate (~2–3 GB/s) and, where it remains, the residual
reader-path `sync.Mutex`, not the namespace lock — which is gone. Removing the lock
cut `/sync/mutex/wait` **1.5–10×** (2.4× at `GOGC=100`; same-session before/after in
[`RUNTIME.md`](RUNTIME.md) §3c), raised reader throughput ~2.5×, and moved p99.9 at
`GOGC=50` from 16.7 to **3.9 ms**; the `RWMutex` no longer appears in the mutex or
block profile. `GOGC=off` with no memory limit is still fatal (the daemon is
OOM-killed ~1.3 s in). Lower `GOGC` still buys tail at the cost of median;
`GOMEMLIMIT` is inert until it binds. Full analysis, the before/after, and the
GC-trace/lock-profile excerpts: [`RUNTIME.md`](RUNTIME.md).

## 7h. Manifested-body admission court

Court: `internal/manifest` (`TestManifestAdmissionScanResistance`,
`TestManifestAdmissionPreservesBytes`).

Drives the **live** manifested read path (`namespace.Read` →
`manifest.Cache.ManifestedRead`) over one identical scan-heavy trace and varies
only the admission policy of the resident-body cache: **S3-FIFO** (the default,
bounded), **LRU** (bounded), and the legacy **unbounded map** (nothing evicted).
The workload is 512 manifested inodes — a 32-inode hot set and a 480-inode
cold scan walked in **bursts of 96** (a directory walk / backup scan) — with a
bounded capacity of 64 bodies.

Measured (this host, container, single run):

| Shape | Arm | resident bodies | hot-set hit rate | re-certifications |
| --- | --- | ---: | ---: | ---: |
| burst (cold burst > capacity) | **S3-FIFO** | **64** | **1.00** | 480 |
| burst | LRU | 64 | 0.00 | 640 |
| burst | unbounded map | 512 (grows with the scan) | 1.00 | 480 |
| interleaved (hot set re-referenced) | S3-FIFO / LRU / map | 64 / 64 / 512 | 1.00 / 1.00 / 1.00 | 480 each |

The **admission win** is on the burst shape: a cold burst longer than the cache
flushes a recency-only LRU (hot-set hit rate 0.00) while S3-FIFO retains the hot
working set (1.00) at the same bounded 64 bodies, avoiding 160 re-certifications.
The unbounded map retains the hot set too — but at 512 resident bodies, growing
with the scan; S3-FIFO gets the same hot-set retention with a fixed bound.

Honest framing: this is a **hit-rate** measurement (hits = a read served from an
already-open certified body; misses = re-loaded and re-certified from the store).
`p50_ns` is in the receipt but the burst shape's p50 is dominated by the
unavoidable cold misses, so no latency win is claimed; the deterministic
read-path benefit is the fall in re-certifications. On the **interleaved** shape
(the reference court's shape, where the hot set is re-referenced within the cache
window) **LRU matches S3-FIFO** — there is no admission win there. Admission never
changes the bytes returned: `TestManifestAdmissionPreservesBytes` drives a
capacity-2 cache (constant eviction and re-certification) and every read is
byte-identical to authority.

Run:

```sh
sh scripts/container-go-courts.sh   # writes evidence/performance/manifest-admission.json
```

Receipt: `evidence/performance/manifest-admission.json` (schema
`orfs.performance.manifest-admission.v1`).

## 7i. Mounted head-to-head: OrFS-GO vs OrFS-Rust vs SeaweedFS (append+fsync and read)

Court: `scripts/oss-compare-court.sh` + `cmd/orfscompare` (the shared workload
driver) + `cmd/osscomparesummary` (merge), run inside the court container by
`scripts/container-oss-compare.sh`. Design and the comparability rules:
[`../courts/oss-compare/README.md`](../courts/oss-compare/README.md).

Three systems are driven by the same deterministic workload, on the same machine,
the same device, and the same container caps: **OrFS-GO**, **OrFS-Rust 0.2.0**, and
**SeaweedFS 3.99** — its FUSE client, `weed mount`, over a single-node master +
volume + filer (pinned image `chrislusf/seaweedfs:3.99`,
`@sha256:8d5b323911a996d5ea152115306bed468d9a849d72bcbb3800ea8d91b7728563`).

Measures **the durable append a storage engineer asks for**: fixed 4 KiB records
appended with `write(2)`+`fsync(2)` per record — one durability barrier per
record **requested of the mount** — at **1 and 64 concurrent writers** (each
writer on its own precreated file), for a bounded 5 s arm. It also measures **block-aligned random reads** at
**4 KiB / 64 KiB / 1 MiB**, at 1 and 64 readers, for a bounded 4 s arm. Latencies
are exact observed samples (per-worker slices merged and sorted), so a p99.9 is a
real sample, and error rates are errno-classified (**0 on every row of this run**).
The seed is fixed (`SEED=1`).

The two OrFS implementations are mounted with equal durability (one `fsync` per
appended record) and the same `--direct-io`: OrFS-GO is a plain `orfs mount`
(group commit OFF: one barrier per publish); OrFS-Rust 0.2.0 is mounted
`--durability sync` (one `fsync` per transaction) with `--backing-io sync` (to
match the Go daemon's synchronous backing I/O). SeaweedFS is mounted as it comes
(`weed mount`, which exposes no `--direct-io` knob) and its **durability is not
the same** — it flushes to a co-located server, it does not fsync to stable
storage (see the not-comparable axes below). All three share the 2 GiB / 4 CPU /
pids-1024 caps, the same seeded 4 KiB record payload, and the same 64 MiB read
file.

Run:

```sh
sh scripts/container-oss-compare.sh
```

Receipt: `evidence/oss-compare.json` (schema `orfs.oss-compare.v1`; 24 rows;
`systems: ["OrFS-GO","OrFS-Rust","SeaweedFS"]`).

Measured (this host, one bounded run — run-to-run variance is expected and is not
quantified here). **Append: 4 KiB `write`+`fsync` per record, 5 s arms:**

| system | writers | ops/s | bytes/s | p50 | p95 | p99 | p99.9 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| OrFS-GO | 1 | 20,523 | 80.2 MiB/s | 39.2 µs | 87.8 µs | 137.3 µs | 871.4 µs |
| OrFS-GO | 64 | 28,910 | 112.9 MiB/s | 1.83 ms | 3.60 ms | 4.30 ms | 6.08 ms |
| OrFS-Rust | 1 | 29,274 | 114.4 MiB/s | 32.5 µs | 40.8 µs | 77.5 µs | 139.1 µs |
| OrFS-Rust | 64 | 30,942 | 120.9 MiB/s | 2.04 ms | 2.37 ms | 2.60 ms | 3.26 ms |
| SeaweedFS | 1 | 193 | 0.75 MiB/s | 3.51 ms | 19.3 ms | 50.8 ms | 56.1 ms |
| SeaweedFS | 64 | 1,970 | 7.7 MiB/s | 14.6 ms | 83.5 ms | 91.5 ms | 102 ms |

**Read: block-aligned uniform random, 4 s arms, 64 MiB file:**

| system | readers | block | IOPS | bytes/s | p50 | p95 | p99 | p99.9 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| OrFS-GO | 1 | 4 KiB | 100,469 | 392.5 MiB/s | 8.9 µs | 12.7 µs | 30.2 µs | 61.7 µs |
| OrFS-GO | 64 | 4 KiB | 349,091 | 1,363.6 MiB/s | 12.7 µs | 96.3 µs | 1.95 ms | 22.2 ms |
| OrFS-GO | 1 | 64 KiB | 49,422 | 3,088.3 MiB/s | 16.6 µs | 41.1 µs | 54.8 µs | 104.7 µs |
| OrFS-GO | 64 | 64 KiB | 145,802 | 9,112.4 MiB/s | 344.7 µs | 863.0 µs | 2.03 ms | 6.76 ms |
| OrFS-GO | 1 | 1 MiB | 3,572 | 3,572.1 MiB/s | 263.0 µs | 388.9 µs | 526.2 µs | 1.32 ms |
| OrFS-GO | 64 | 1 MiB | 8,259 | 8,259.0 MiB/s | 6.27 ms | 17.7 ms | 36.9 ms | 119 ms |
| OrFS-Rust | 1 | 4 KiB | 128,264 | 501.0 MiB/s | 7.4 µs | 9.0 µs | 11.4 µs | 27.0 µs |
| OrFS-Rust | 64 | 4 KiB | 506,775 | 1,979.6 MiB/s | 7.4 µs | 10.5 µs | 14.0 µs | 80.9 µs |
| OrFS-Rust | 1 | 64 KiB | 75,892 | 4,742.9 MiB/s | 12.7 µs | 16.2 µs | 21.2 µs | 36.0 µs |
| OrFS-Rust | 64 | 64 KiB | 336,047 | 21,000.0 MiB/s | 11.7 µs | 16.6 µs | 22.2 µs | 48.0 ms |
| OrFS-Rust | 1 | 1 MiB | 6,990 | 6,990.0 MiB/s | 140.8 µs | 172.8 µs | 201.5 µs | 288.1 µs |
| OrFS-Rust | 64 | 1 MiB | 15,827 | 15,827.0 MiB/s | 3.77 ms | 5.53 ms | 10.6 ms | 12.5 ms |
| SeaweedFS | 1 | 4 KiB | 1,586 | 6.2 MiB/s | 322.6 µs | 808.8 µs | 1.54 ms | 50.0 ms |
| SeaweedFS | 64 | 4 KiB | 2,237 | 8.7 MiB/s | 20.8 ms | 74.3 ms | 79.1 ms | 89.5 ms |
| SeaweedFS | 1 | 64 KiB | 210,240 | 13,140 MiB/s | 3.4 µs | 6.8 µs | 9.9 µs | 457.4 µs |
| SeaweedFS | 64 | 64 KiB | 985,842 | 61,614 MiB/s | 2.2 µs | 6.3 µs | 9.1 µs | 18.0 µs |
| SeaweedFS | 1 | 1 MiB | 19,889 | 19,889 MiB/s | 40.8 µs | 101.8 µs | 130.4 µs | 226.2 µs |
| SeaweedFS | 64 | 1 MiB | 47,684 | 47,684 MiB/s | 47.8 µs | 287.5 µs | 20.1 ms | 208 ms |

**Honest framing (the OrFS pair).** OrFS-GO is within about **0.7–0.9×** of
OrFS-Rust on the append rows of this run: the durable **single-writer append is
20,523 vs 29,274 ops/s** (p50 39.2 µs vs 32.5 µs) and the concurrent-append rows
converge (28,910 vs 30,942 at 64 writers). The clearest remaining Go losses are
the **large-block reads** (1 MiB: 3,572 vs 6,990 at 1 reader, 0.51×; 8,259 vs
15,827 at 64 readers, 0.52×) and the 64-reader tail — go-fuse transport and
per-request buffering, *not* the log or the index. That is reported as measured,
without spin: the point of the court is the *comparable number*, not a Go win. It
is **not** a leaderboard: rows are keyed by {system, workload, writers, readers,
block_size}, and two rows are comparable only at the same coordinate and only
under the caveats below.

**The step-1 single-writer append loss is fixed: extent structural sharing
(Phase 27 parity).** The first run of this court measured OrFS-GO at **3,097
ops/s** (p50 297 µs) single-writer — **8.8×** behind OrFS-Rust. Root cause: the
file extent map (`internal/index.ExtentMap`) was a flat `[]Extent`, and the log
frames each 4 KiB append so that consecutive appends' bytes are *not* contiguous
in the log; the extents therefore never coalesce and the list grew 1:1 with the
append count. `Clone()` — called once per inode clone on every commit — and
`replace()` were both **O(E)**, so each write was O(N) and an append run was
**O(N²)**: fatal for the audit/WORM workload, whose whole point is unbounded
history. (This is exactly the O(history) publish-path cost the Rust reference
removed in its Phase 27, `.reference/OrFS/src/persistent.rs`; the Go port had
adopted persistent treaps for the inode/dentry spines but not for extents.)

The fix makes `ExtentMap` a persistent, path-copying treap keyed by `Start` — a
faithful port of the reference's `PMap`: `Clone` is O(1) (a pointer copy), a
write path-copies only the O(log E) nodes on the affected range plus the extents
it splices, and a read visits only the extents that intersect the range **and
allocates nothing**. Evidence:

- `evidence/performance/append-scaling.json` — per-append cost is **flat** (27.9 µs
  at N=256 and 27.9 µs at N=8,192; growth ratio **1.001×**, bound 2.0×), where it
  previously rose from 35 µs to 74 µs over that range and kept rising.
- A **20,000-step randomized differential test** against the original flat
  implementation (`internal/index/extentmap_model_test.go`) proves the new map is
  observably identical after every step: same size, same ordered maximal extent
  sequence, same read plan.
- A `PlanReadInto` over a 16,384-extent file resolves in **0 allocations**
  (`TestPlanReadIntoZeroAlloc`).

The result is the row above: **20,523 ops/s** (p50 39.2 µs), down from 297 µs
p50 — inside 1.5× of the Rust reference instead of 8.8× behind it.

**SeaweedFS (the third system).** SeaweedFS is a **client/server** filesystem: a
single-node `weed server` (master + volume + filer) plus its FUSE client
(`weed mount`), staged from the pinned image and driven by the *same* driver on
the *same* caps. Two results are worth stating plainly, and neither is a win for
OrFS:

- **Its `fsync` is not the OrFS barrier.** The driver issues the same
  `write(2)`+`fsync(2)` per 4 KiB record, but `weed mount` disposes of `Fsync` by
  flushing the client's dirty pages to the co-located volume server (an HTTP PUT
  over loopback) and committing filer metadata over gRPC; it does **not** fsync
  the volume `.dat` file or the filer store to stable storage. So the append rows
  are **not** one-durability-barrier-per-record numbers and must not be read as
  equal-durability with the OrFS rows.
- **Its read shape is not the OrFS read path.** Under the identical `pread`
  workload SeaweedFS ran **4 KiB random reads at ~1.6–2.2k IOPS** but **64 KiB
  random reads at ~0.2–1.0M IOPS** (a ~100–600× gap), and a preceding warm 64 KiB
  pass did not change the 4 KiB rate (checked directly): its client serves some
  read sizes from an in-process chunk cache and others with a backing round trip.

The append rows are dominated by the round trips the workload actually makes:
the single-writer 4 KiB append is **193 ops/s** (p50 3.51 ms) and the 64-writer
append is **1,970 ops/s** (p50 14.6 ms) — about **106×** and **15×** below
OrFS-GO's 20,523 and 28,910 ops/s at the same coordinates. That is measured, not
asserted, and it is **not** a like-for-like durability comparison (first bullet
above). This is the first court to run a *client/server* system on the same
driver; the numbers put its network-crossing data path beside the local OrFS
daemons without ranking them.

On the shared 2 GiB cap, SeaweedFS's stack (server + client) plus the driver
exceeded the cap at SeaweedFS's default Go GC settings on an earlier run and the
kernel OOM-killed the volume server mid-arm. The published run bounds both `weed`
processes with `GOGC=50 GOMEMLIMIT=700MiB` (recorded in
`comparability.systems.SeaweedFS.go_runtime_env` and as a not-comparable axis);
without it, one arm was lost and the court correctly **FAIL**ed with a partial
receipt rather than publishing a hole.

**Measurement order and memory.** Within each system the **read arms run before
the append arms**, on a quiet volume: because OrFS retains appended history, a
4 KiB append is committed, deferred, and its data-offset in its transaction, so
measuring appends first would evict the read file from the backing page cache and
make every `--direct-io` read a backing I/O (this is why the read rows here differ
from the first run of the court). All systems are measured in the same order. The
systems' arms also run **strictly sequentially**, and a finished system's daemon
and volume (or, for SeaweedFS, its mount and cluster) are **released before the
next system's arms** — so no two compete for the shared 2 GiB cap (earlier runs
OOM-killed the Rust daemon, and the SeaweedFS volume server, mid-arm on that cap).
The `comparability.arms` block records that every expected arm ran (`8/8` for each
of OrFS-GO, OrFS-Rust, and SeaweedFS).

**Comparability caveats (every way the mounted configurations differ — from the
receipt's `comparability` block):**

- **FUSE userspace library.** go-fuse/v2 is used by OrFS-GO *and* SeaweedFS; the
  `fuser` crate by OrFS-Rust. All three speak classical `/dev/fuse` with one
  request per operation, but they are independent daemons.
- **Transport shape.** Go serves its default `f0` multi-reader loop; Rust serves
  its classical `fuse` transport; SeaweedFS serves go-fuse. Same wire protocol,
  different daemon concurrency shapes.
- **Backing I/O.** The Go daemon is synchronous (pwrite/pread); Rust was pinned to
  `--backing-io sync` to match, because its default on this host is `io_uring`
  and the Go build has no io_uring backing path; SeaweedFS's FUSE client PUTs data
  over loopback HTTP to a co-located volume server that writes its own `.dat`.
- **Durability is NOT equal across all three.** OrFS-GO and OrFS-Rust each take
  one durable `fsync` per 4 KiB record; SeaweedFS's `Fsync` flushes to a
  co-located server without fsyncing the volume `.dat` or the filer store. These
  are the `not_comparable` axes: **append durability**, **data path**, and
  **read-path caching shape**.
- **`--direct-io`.** OrFS-GO and OrFS-Rust are both mounted with it
  (`FOPEN_DIRECT_IO`); `weed mount` exposes no such knob. Recorded, not removed.
- **Memory budget.** The shared 2 GiB cap must hold SeaweedFS's whole stack plus
  the driver; both `weed` processes run under `GOGC=50 GOMEMLIMIT=700MiB` to fit
  (an earlier run OOM-killed the volume server mid-arm at default GC). This is a
  SeaweedFS-only environment-fit knob and is recorded.
- **Rust daemon stderr.** The receipt records the Rust daemon's captured stderr:
  a `fuser` `poll` WARN and an unmount WARN. The driver's arms still recorded
  error rate 0 everywhere.
- **Host variance.** Page-cache state, the kernel scheduler, and the backing
  device's write cache are not controlled beyond the shared container;
  run-to-run variance is expected and is not quantified here.
- **Order and memory (methodology, identical for all systems).** Reads are
  measured before appends on a quiet volume; the systems' arms run strictly
  sequentially and a finished system's daemon and volume are released before the
  next system's arms, so no two compete for the shared 2 GiB cap. See
  [`../courts/oss-compare/README.md`](../courts/oss-compare/README.md).
- **Equal, not caveats:** the 4 KiB record size, the seeded payload, the 64 MiB
  read file, the 8 MiB segment size (OrFS pair), and `--direct-io` (OrFS pair) are
  the same across the systems that support them.

No ranking is implied: rows are keyed by `{system, workload, writers, readers,
block_size}`, and two rows are comparable only at the same coordinate and only
under the caveats above.

## 7j. Retention court: Btrfs read-only snapshots vs the OrFS-GO log

Court: `scripts/retention-btrfs-court.sh` + `cmd/retentionbench` (the driver) +
`cmd/retentionsummary` (merge), run inside a privileged court container by
`scripts/container-retention-btrfs.sh`. Design and the comparability rules:
[`../courts/retention-btrfs/README.md`](../courts/retention-btrfs/README.md).

This is a **retention** court, not a leaderboard: *can a system retain every
committed historical transition while achieving comparable tail latency,
throughput and physical-space economics?* OrFS retains history in its
authoritative log (non-withdrawal, Law I); Btrfs retains it with a **read-only
snapshot after every epoch**. One defined, bounded epoch workload drives both: 8
epochs, each appending 256 records of 4 KiB per writer with `write(2)`+`fsync(2)`
per record (one durability barrier per record), on same-sized loop-mounted Btrfs
backings, under the shared 2 GiB / 4 CPU / pids-1024 caps. Each retained state is
8 MiB of logical data; run-to-run variance is expected and is not quantified.

**Snapshot cost** (`btrfs subvolume snapshot -r`, one per epoch):

| | min | median | max |
| ---: | ---: | ---: | ---: |
| snapshot create | 673.6 µs | 730.3 µs | 810.6 µs |

**Append / current-state read / historical-state read** (epoch 0 and epoch 7):

| system | arm | epoch | ops/s | IOPS | p50 | p95 | p99 | p99.9 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Btrfs | append 4 KiB+fsync | 0 | 15 409 | — | 58.1 µs | 95.9 µs | 126.3 µs | 153.8 µs |
| Btrfs | append 4 KiB+fsync | 7 | 15 673 | — | 58.2 µs | 85.8 µs | 226.9 µs | 672.4 µs |
| OrFS-GO | append 4 KiB+fsync | 0 | 3 252 | — | 294.9 µs | 418.4 µs | 666.2 µs | 1 462.5 µs |
| OrFS-GO | append 4 KiB+fsync | 7 | 3 564 | — | 264.9 µs | 406.6 µs | 624.6 µs | 944.3 µs |
| Btrfs | read current (warm) | 0 | — | 4 133 002 | 0.16 µs | 0.19 µs | 0.25 µs | 0.49 µs |
| Btrfs | read current (warm) | 7 | — | 3 856 372 | 0.18 µs | 0.26 µs | 0.39 µs | 0.67 µs |
| Btrfs | read snapshot (warm) | 0 | — | 3 937 318 | 0.17 µs | 0.21 µs | 0.28 µs | 0.60 µs |
| Btrfs | read snapshot (warm) | 7 | — | 3 637 293 | 0.19 µs | 0.28 µs | 0.39 µs | 5.73 µs |
| OrFS-GO | read current (buffered) | 0 | — | 104 386 | 8.34 µs | 12.54 µs | 29.89 µs | 66.58 µs |
| OrFS-GO | read current (buffered) | 7 | — | 94 940 | 8.68 µs | 12.93 µs | 30.43 µs | 66.92 µs |

**Physical vs logical bytes** (retaining every state; device `Used` for Btrfs,
authoritative `or.log` blocks for OrFS-GO):

| system | epoch | state | logical | physical | phys/logical | snapshot exclusive | snapshot set-shared |
| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |
| Btrfs | 0 | current | 1 048 576 | 1 363 149 | 1.300× | 1 048 576 | 0 |
| Btrfs | 0 | snapshot | 1 048 576 | 1 541 407 | 1.470× | 0 | 1 048 576 |
| Btrfs | 7 | current | 8 388 608 | 10 265 559 | 1.224× | 1 048 576 | 7 340 032 |
| Btrfs | 7 | snapshot | 8 388 608 | 10 454 303 | 1.246× | 0 | 8 388 608 |
| OrFS-GO | 0 | current | 1 048 576 | 1 175 552 | 1.121× | — | — |
| OrFS-GO | 7 | current | 8 388 608 | 9 363 456 | 1.116× | — | — |

**Honest framing.** *Both* systems retain every committed state: after 8 epochs
Btrfs has 8 read-only snapshots and OrFS-GO has 8 committed transitions in its
log. On the space axis Btrfs pays **10.45 MiB** of device bytes to retain 8 MiB of
logical data (**1.246×**, its CoW metadata included), while OrFS-GO's
authoritative log holds the same history in **9.36 MiB** (**1.116×**); each
retained Btrfs snapshot's **exclusive** bytes are **0** (all extents are shared
with the live subvolume), which is the space-economics point: a snapshot is cheap
because it shares. On throughput, the **kernel Btrfs append is ~4.4× the OrFS-GO
FUSE append** (15 673 vs 3 564 ops/s at epoch 7; p50 58.2 vs 264.9 µs) and the
**warm current read is ~41× the OrFS-GO FUSE read** (3.86 M vs 94.9 k IOPS):
this is the FUSE tax the head-to-head court already publishes (§7i), now shown
under a retention workload. The retention-specific result is that a **historical
snapshot read is as fast as a current read** (Btrfs p50 0.19 vs 0.18 µs warm), so
retaining the state added no observable read cost.

**Comparability caveats (from the receipt's `comparability` block).** Every row
above is provisional on these differences, and the axes marked not comparable are
**not** compared:

- **Historical-state read is Btrfs-only.** OrFS-GO does not serve named
  historical roots in this build, so there is no OrFS-GO snapshot-read row; its
  current-state read serves the newest committed bytes.
- **Snapshot creation is Btrfs-only.** OrFS-GO has no snapshot step to measure.
- **The physical-bytes accounting schemes differ.** Btrfs physical is the whole
  device's `Used` (data plus CoW metadata); OrFS-GO physical is the authoritative
  `or.log` blocks. Both are measured; they are not the same computation.
- **The append comparison carries the FUSE tax.** The OrFS-GO append pays the
  FUSE round trip and the daemon's framing; the Btrfs append pays only the kernel
  Btrfs `write`+`fsync`. Recorded, not removed.
- **Reads are warm (buffered).** Each snapshot read runs just after the snapshot
  is created, so reads hit cache; the axis shows historical reads are no slower
  than current reads, not a cold-device number.
- **The read comparison is native-vs-FUSE, not retention.** The Btrfs read is a
  native local filesystem read served from the page cache (no FUSE, no daemon);
  the OrFS-GO read is a FUSE request plus a daemon `pread`. The ~41× gap is the
  cost of the FUSE data path (the same tax §7i publishes between two FUSE
  daemons), not a retention cost, and the two rows do not rank each other.
- **Host variance.** Page-cache state, the scheduler, and the backing device's
  write cache are uncontrolled beyond the shared container.

No ranking is implied: rows are keyed by `{system, kind, state, epoch, writers,
readers, block_size}`, and two rows are comparable only at the same coordinate
and only under the caveats above. The receipt is
`evidence/retention-btrfs.json` (schema `orfs.retention.btrfs.v1`).

## 7k. Retention court: real OpenZFS snapshots vs the OrFS-GO log (measured in a KVM micro-VM)

Court: `scripts/retention-zfs-court.sh` + `cmd/retentionbench` (the shared driver,
extended with a `zfs` space/snapshot kind) + `cmd/retentionsummary`, run inside a
privileged court container by `scripts/container-retention-zfs.sh`. Design and the
comparability rules:
[`../courts/retention-zfs/README.md`](../courts/retention-zfs/README.md).

This is the **fourth system** for the §7j retention question, on the same axes and
the same driver: OpenZFS retains every committed state with `zfs snapshot`, a
read-only CoW snapshot of a dataset, and the court accounts physical vs logical
bytes from `zfs list -o used,referenced,logicalused,logicalreferenced`.

**How it is measured.** OpenZFS is an out-of-tree **kernel module**, and this host
has none: `/dev/zfs` is absent, `zfs` is in neither `/proc/modules` nor
`/proc/filesystems`, and `modprobe -n zfs` (a *query* only) reports `FATAL: Module
zfs not found in directory /lib/modules/7.2.9-2-cachyos`. Loading a module is a
**host change** and is out of scope, so the court boots a **KVM micro-VM** whose
*guest* kernel provides OpenZFS: a pinned Ubuntu 24.04 minimal cloud image (kernel
`6.8.0-146-generic`, `zfs-2.2.2-0ubuntu9.5` / `zfs-kmod-2.2.2`, sha256
`2f119f50…`) under QEMU/KVM on the host's `/dev/kvm`. **Both arms run inside that
one guest**, on the same file-backed vdev and the same virtio disk, and the
guest's own `zpool`/`zfs` build the pool, take the snapshots and account the
space. The host is never modified — OpenZFS lives only in the guest.

Run:

```sh
sh scripts/container-retention-zfs.sh
```

Receipt: `evidence/retention-zfs.json` (schema `orfs.retention.zfs.v1`; 40 rows,
24 space rows, 8 snapshots; arms 24/24 OpenZFS and 16/16 OrFS-GO). The verbatim
`zfs list` accounting, the pinned image and digest, the guest kernel and ZFS
version, and every comparability caveat are recorded in the receipt.

Measured (one bounded run; 8 epochs; epochs 0 and 7 shown). **Append — 4 KiB
`write`+`fsync` per record, inside the guest:**

| system | epoch | ops/s | p50 | p99 |
| --- | ---: | ---: | ---: | ---: |
| OpenZFS | 0 | 920 | 1.06 ms | 1.36 ms |
| OpenZFS | 7 | 898 | 1.07 ms | 2.13 ms |
| OrFS-GO | 0 | 55 | 18.0 ms | 28.5 ms |
| OrFS-GO | 7 | 81 | 11.9 ms | 18.1 ms |

**Read — block-aligned uniform random, epoch 7:**

| system | state | IOPS | p50 | p99 |
| --- | --- | ---: | ---: | ---: |
| OpenZFS | current | 105,279 | 8.5 µs | 27.9 µs |
| OpenZFS | **snapshot (historical)** | 110,408 | 8.3 µs | 26.7 µs |
| OrFS-GO | current | 21,705 | 41.6 µs | 77.0 µs |

**Snapshot cost** (`zfs snapshot`, one per epoch): **~7.4–7.8 ms** (~10× the
Btrfs §7j figure, which is the nested-VM tax, not an OpenZFS property).

**Physical vs logical bytes, epoch 7:**

| system | state | logical | physical (`used`) | note |
| --- | --- | ---: | ---: | --- |
| OpenZFS | current | 7,354,368 | 219,136 | compression-aware (`logicalused` 7,406,592) |
| OpenZFS | snapshot | 8,402,944 | **0** | all blocks shared with the live dataset (CoW) |
| OrFS-GO | current | 8,388,608 | 9,371,648 | 1.117× `or.log`, uncompressed |

**Honest framing.**

- **The retention result is the point, and it holds:** a read-only OpenZFS
  snapshot is created in ~7.4 ms, its uniquely-retained bytes are **0** (every
  block is shared with the live dataset — CoW working), and a **historical
  snapshot read is exactly as fast as a current read** (110,408 vs 105,279 IOPS;
  p50 8.3 vs 8.5 µs). Retaining 8 states added no measurable read cost.
- **These absolute numbers are NOT comparable to §7j.** Both arms here run inside
  a 384 MiB KVM guest on a virtio disk, so every latency carries
  nested-virtualisation and virtio cost; the container-only Btrfs court does not.
  The two systems *are* comparable **to each other** (same guest, same disk), and
  that is the only cross-system reading this section supports.
- **The space figures are different computations and are not ranked.** OpenZFS's
  physical figure is **compression-aware** — a new dataset defaults to
  `compression=lz4`, so 219 KiB of device blocks hold 7.35 MiB of logical data —
  while OrFS-GO's `or.log` bytes are uncompressed. A snapshot's `used` of 0 is
  extent sharing, not a 0-byte cost. Do **not** read 0.03× against OrFS's 1.117×.
- **The two arms' logical bases differ slightly in this run** (7.35 MiB vs
  8.39 MiB); the receipt records both, and no ratio is claimed across them.
- **Historical-state read and snapshot creation are OpenZFS-only.** OrFS-GO does
  not serve named historical roots in this build, and it has no snapshot step —
  it retains every transition at append time ([`LIMITATIONS.md`](LIMITATIONS.md) §10).
- **Nothing outside the container was modified**, and the OpenZFS module exists
  only in the guest.

Two engineering facts worth recording, because they cost real time to find and
are not obvious:

- **A micro-VM does not fit the FUSE-court cap at the default sizing.** With a
  512 MiB or 1 GiB guest, QEMU is OOM-killed (`rc=137`) under the shared 2 GiB
  cap; a **384 MiB** guest fits.
- **The VM's disk must live on a real filesystem, not overlayfs.** `cache=none`
  (O_DIRECT) does **not** bypass the page cache on overlayfs, so the guest's
  writes accumulate in the container's page cache until the cap is hit and QEMU
  is killed — measured. Putting the VM work dir on a disk-backed bind mount fixed
  it; the court passes `ZFSVM_WORKDIR` for this.

The container-only `zfs-fuse` userspace port was also reachable and would fit the
cap with no VM; it is **not** OpenZFS (an old userspace port, no
`logicalused`/`logicalreferenced`, no `.zfs/snapshot`), so the court measures real
OpenZFS in the guest instead and records that choice.

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
| scan-resistant admission | S3-FIFO 95.2% vs FIFO 88.0% vs LRU 95.2% (key trace) | `evidence/performance/cache.json` |
| manifested-body admission | live manifested read path, burst scan: S3-FIFO hot-set hit rate 1.00 at 64 resident bodies vs LRU 0.00 (same bound) and unbounded map 1.00 at 512 residents; interleaved shape: LRU matches (no win) | `evidence/performance/manifest-admission.json` |
| capacity guard | typed `CapacityExhausted` before the first byte | `evidence/recovery/capacity.json` |
| quarantine | torn tail moved, never truncated | `evidence/recovery/quarantine.json` |
| cross-FS determinism | identical segment sha256 across ext4/xfs/btrfs/tmpfs | `evidence/correctness/cross-fs.json` |
| pjdfstest conformance | 8798/8798 assertions pass; 0 deviations | `evidence/performance/pjdfstest.json` |
| group commit | 4.31× throughput / 4.49× better p50 vs a barrier per publish (bounded by the backing store's fsync cost). **Zero-byte payloads: an internal batched-publish rate, not a durable 4 KiB append rate** (see §7d) | `evidence/performance/groupcommit.json` |
| per-read allocations | authoritative 4 KiB read: 2 -> 1 allocs/op (~470 ns/op); the returned `[]byte` is the only allocation | `evidence/performance/allocs.json` |
| crash consistency (real `SIGKILL`) | 1000 kills mid-append: 379 abandoned tails (4 torn / 375 uncommitted), all quarantined (moved, never truncated); 0 truncations / 0 prefix mismatches / 0 recover failures; `verify --deep` clean | `evidence/crash.json` |
| Go-runtime GC scaling | 9-arm GOGC×GOMEMLIMIT sweep under 64 readers + 8 writers: p99.9 3.9 ms (`GOGC=50`) … 78.2 ms (`GOGC=off`+512MiB); GC pauses ≤ 98 µs (not the tail); ~2–3 GB/s allocation; `GOGC=off`+no limit OOM-killed ~1.3 s in. **After** removing the global read lock: `/sync/mutex/wait` down 1.5–10× (2.4× at `GOGC=100`), reader throughput ~2.5×, and the `RWMutex` gone from the lock profiles | `evidence/performance/gc-scaling.json` |
| lock-free read views | a commit publishes an immutable namespace view (path-copying treap + persistent dentry map) through an `atomic.Pointer`; readers traverse it with no lock. Same-session A/B at 64 readers / 8 writers: mutex wait 141.2→93.1 s (`GOGC=400`) … 162.3→16.0 s (`GOGC=20`); throughput 117→288 kIOPS (`GOGC=100`); p99.9 16.7→3.9 ms (`GOGC=50`); `RWMutex` absent from mutex/block profiles | `docs/CONCURRENCY.md`, `docs/RUNTIME.md` §3b/§3c |
| mounted head-to-head (OrFS-GO vs OrFS-Rust vs SeaweedFS) | shared deterministic driver, same caps: durable 4 KiB append+fsync at 1/64 writers and 4K/64K/1MiB random reads at 1/64 readers. OrFS-GO within 0.7×–0.9× of OrFS-Rust on the append rows; SeaweedFS's FUSE client is 15×–106× slower on append and its `fsync` is a flush-to-server, not a durable barrier (recorded as not comparable). Not a leaderboard | `evidence/oss-compare.json` |
| retention (Btrfs read-only snapshots vs the OrFS-GO log) | one defined epoch workload retains every committed state: Btrfs keeps 8 read-only snapshots (each exclusive 0, all extents shared) at 1.246× device bytes for 8 MiB logical; OrFS-GO keeps 0 snapshots at 1.116× `or.log` bytes; snapshot create ~0.73 ms; a warm historical read equals a warm current read. Historical-read and snapshot-cost axes are Btrfs-only (recorded as not comparable). Not a leaderboard | `evidence/retention-btrfs.json` |

Explicitly not yet measured (absence is deliberate, not implied):

| Frontier | State |
| --- | --- |
| F2 FUSE-over-`io_uring` transport | **not reachable from pure Go via go-fuse** — the kernel supports it (probed live), the linked library does not; the measured F0 comparison is published (`TRANSPORT.md`, §6b) |
| Multi-day amplification curve; profile selection by measured worth | not run |
| Mount latency after a large (2^30-byte) history | not run (`OPERATIONS.md` §3) |
| Open-source head-to-head, later steps | step 2 ships (OrFS-GO vs OrFS-Rust vs SeaweedFS FUSE, §7i); Btrfs read-only snapshots ship as the retention court (§7j); OpenZFS ships as the fourth retention court but is **SKIPPED here** (no `/dev/zfs`, no zfs kernel module; §7k) |
| OpenZFS retention court (fourth system) | **SKIPPED** — the host has no `/dev/zfs` and no zfs kernel module reachable from a container; `zpool create` on a file-backed vdev reports `The ZFS modules are not loaded`. Loading the module is a host-level change and is out of scope, so the court records the feasibility evidence and publishes no number (§7k) |

No performance superiority is claimed beyond the measured planner p50 figure and
the inode-spine measurements. The Go-runtime court measures the daemon's own
cost; it does not claim the port is faster than a native filesystem (see
[`RUNTIME.md`](RUNTIME.md) §6).

## 9. Reproduce

Every court above is one script, one command, one receipt. See
[`REPRO.md`](REPRO.md) for the container-only procedure, the `/dev/fuse` and
capability requirements, and the exact receipt paths.
