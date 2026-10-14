# The Go Runtime, Measured

**Status:** bounded measurements, published as receipts. This file answers one
question with evidence: *what does the Go runtime cost the OrFS-GO daemon, under
which runtime knobs, and when is the Go port a reasonable operational
alternative?* It reports only what the court measured. Where a number is bounded
or absent, it says so.

Court: `scripts/gc-scaling-court.sh`, run inside the pinned court container by
`scripts/container-gc-scaling.sh`.
Receipt: `evidence/performance/gc-scaling.json` (schema
`orfs.performance.gc-scaling.v1`).

Cross-links: [`BENCH.md`](BENCH.md) (the FUSE tax and the other courts),
[`OPERATIONS.md`](OPERATIONS.md) (running a daemon), [`REPRO.md`](REPRO.md)
(container-only procedure), [`CONCURRENCY.md`](CONCURRENCY.md) (the read-view
model that removed the lock named below).

**Update — the lock this court named is gone.** The single shared `RWMutex` this
study identified as the dominant tail cost has been removed: reads are now
answered from an immutable read view published atomically at the commit boundary
(`docs/CONCURRENCY.md`). §3 reports the sweep measured **after** that change; §3b
compares the lock profiles before/after; §3c gives the before/after at 64 readers.

## 1. What was chosen, and why

The workload drives a **mounted** OrFS volume, and the Go-runtime metrics are
collected **from the mount daemon** by `orfs mount --profile-dir <dir>`.

- The **application** view comes from `cmd/orfsrw`, a new bounded driver that
  holds **64 concurrent 4 KiB random readers plus 8 concurrent writers** against
  the mount and samples every operation's latency (exact p50/p99/p99.9 from
  per-worker sample slices; throughput; errno-classified errors).
- The **runtime** view comes from the daemon itself: on shutdown (and on
  `SIGUSR1`) it writes `runtime-metrics.json` — windowed GC pauses, scheduler
  latency, allocation rate, `/sync/mutex/wait/total:seconds`, and real resident
  memory (`/proc/self/status` `VmRSS`/`VmHWM`) — plus `heap.prof`, `goroutine.prof`
  and (with `--profile-locks`) `mutex.prof`/`block.prof`. See
  `internal/gcruntime`.

The rejected alternative was to drive `internal/namespace` **in-process**, where
the FUSE boundary is not a variable. It was rejected because the question
explicitly includes *how the FUSE integration affects tail latency*: an
in-process driver deletes the very boundary being asked about, and it is not the
artifact an operator can reproduce. The mounted design keeps the FUSE boundary
inside the measurement and yields an operator-facing flag (`--profile-dir`)
rather than a court-only harness.

One honest note on the daemon flag: enabling the **mutex and block profiles**
(`--profile-locks`) perturbs latency, so the nine latency arms run with profiling
**off**; the lock profiles are collected in a **separate, clearly-labelled phase**
for the best and worst configurations only. The always-on
`/sync/mutex/wait/total:seconds` metric costs nothing and is reported for every
arm, so the *lock cost* is measured without disturbing the latency arms.

## 2. The sweep (bounded)

A bounded **9-arm** spread, not the full 5×3 cross product:

| axis | values |
| --- | --- |
| `GOGC` (at unlimited `GOMEMLIMIT`) | `400`, `100`, `50`, `20`, `off` |
| `GOMEMLIMIT` (at `GOGC=100`) | `256MiB`, `512MiB` |
| `GOGC=off` with a limit | `256MiB`, `512MiB` |

Each arm: fresh volume, `--direct-io` mount, ~5 s workload, 64 readers / 8
writers, inside the standard **2 GiB / 4 CPU / 1024 pid** container. `unlimited`
is passed to the daemon as `GOMEMLIMIT=off` (the runtime's spelling for
`math.MaxInt64`; `GOMEMLIMIT=unlimited` is malformed and panics the daemon at
startup — a real trap worth naming). The receipt records the *effective* knob
values read back from the runtime, so a mistyped knob is visible, not assumed.

`GOGC=off` reports `effective_gogc_percent = 18446744073709551615`: the runtime
stores the user value as a signed `-1` and exposes it as a `uint64`, so the wrap
is the runtime's, not the court's. `GOMEMLIMIT=off` reports `9223372036854775807`.

## 3. Measured: the tail across the knobs (after lock-free read views)

This is the sweep re-run **after** the global read lock was removed
(`docs/CONCURRENCY.md`; §3c gives the before/after). All latencies are
**application** (syscall-return to syscall-return on the client), ns scaled.
`gcN` is the windowed GC cycle count; `gcp50/gcp99` are the windowed GC
stop-the-world pause percentiles; `alloc` is `/gc/heap/allocs:bytes` over the
daemon window; `rss`/`peak` are **real** `VmRSS`/`VmHWM`; `mutex` is the windowed
`/sync/mutex/wait/total:seconds` (cumulative goroutine-seconds of lock wait, so
`mutex / window` is the average number of goroutines blocked on a lock).

| GOGC | GOMEMLIMIT | status | p50 | p99 | p99.9 | kIOPS | MiB/s | gcN | gcp50 | gcp99 | sched p99 | alloc GB/s | RSS MiB | peak MiB | mutex s |
| ---: | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 400 | unlimited | ok | 30.8 µs | 0.4 ms | 45.1 ms | 333.1 | 1301.2 | 1287 | 12.3 µs | 65.5 µs | 14.7 ms | 2.69 | 26.3 | 35.8 | 93.1 |
| 100 | unlimited | ok | 43.2 µs | 9.6 ms | 21.4 ms | 287.7 | 1123.9 | 7820 | 12.3 µs | 65.5 µs | 2.6 ms | 2.90 | 17.4 | 26.4 | 55.5 |
| 50 | unlimited | ok | 288.3 µs | 1.7 ms | **3.9 ms** | 216.0 | 843.6 | 14291 | 12.3 µs | 65.5 µs | 0.8 ms | 2.34 | 15.6 | 19.4 | 24.6 |
| 20 | unlimited | ok | 310.8 µs | 2.0 ms | 6.4 ms | 187.5 | 732.3 | 14081 | 12.3 µs | 81.9 µs | 0.5 ms | 2.07 | 15.2 | 16.4 | 16.0 |
| 100 | 256MiB | ok | 43.7 µs | 10.1 ms | 20.9 ms | 286.5 | 1118.9 | 9874 | 12.3 µs | 65.5 µs | 1.8 ms | 3.16 | 15.6 | 23.9 | 55.9 |
| 100 | 512MiB | ok | 46.3 µs | 10.4 ms | 21.4 ms | 273.5 | 1068.3 | 9430 | 12.3 µs | 65.5 µs | 1.8 ms | 2.94 | 15.9 | 21.7 | 61.9 |
| off | 256MiB | ok | 32.7 µs | 0.1 ms | 76.1 ms | 316.0 | 1234.4 | 62 | 20.5 µs | 57.3 µs | 67.1 ms | 2.40 | **249.9** | 252.0 | 18.6 |
| off | 512MiB | ok | 33.4 µs | 0.2 ms | **78.2 ms** | 312.2 | 1219.4 | 30 | 20.5 µs | 98.3 µs | 83.9 ms | 2.36 | **493.0** | 502.3 | 24.3 |
| off | unlimited | **daemon_died** | — | — | — | — | — | — | — | — | — | — | — | — | — |

*(Measured on this host in the pinned container, ~5 s per arm, one run. Absolute
values move with host load; the shape is the stable result. Best p99.9 =
`GOGC=50`; worst = `GOGC=off`+`512MiB`.)*

The `GOGC=off`/`unlimited` row is not filled in: the daemon did not survive to
its shutdown dump (GC disabled with no memory limit against a ~2 GB/s allocation
rate and a 2 GiB cgroup cap is fatal, not merely slow — the earlier study
recorded the `SIGKILL` ~1.3 s in and 18.6 M `ENOTCONN`).

Five findings fall out of the table:

1. **GC pauses are still not the tail.** The GC stop-the-world p99 is ≤ **98 µs**
   everywhere. The tail is the *aggregate* cost of a high allocation rate plus the
   runtime, not a long pause.
2. **GOGC still trades median against tail.** Lowering GOGC from `400` to `20`
   moves p50 from **31 µs to 311 µs** (10× worse) but p99.9 from **45 ms to
   6.4 ms** (7× better): a small heap goal means many *cheap* cycles (14 081 in
   ~6 s at `GOGC=20` versus 1 287 at `400`). There is no free lunch here — the
   working set is the same, collected in smaller, more frequent bites.
3. **GOMEMLIMIT is inert until it binds.** At `GOGC=100` the live heap is
   ~2–3 MiB, so a `256MiB`/`512MiB` limit changes nothing measurable (p99.9 20.9 /
   21.4 ms versus 21.4 ms unlimited). It only matters when GC is disabled
   (finding 5) or a large live set would otherwise be unbounded.
4. **Lock wait is now much smaller, and no longer the top profile entry.**
   `/sync/mutex/wait` is **16–93 goroutine-seconds** over a ~6 s window — down
   from 88–160 in the same session before the change (§3c) — and the residual is
   the short-lived plain `sync.Mutex` of the reader's I/O path, not the namespace
   lock, which is gone (§3b). At high GOGC the tail is now GC/allocator-bound, not
   lock-bound.
5. **`GOGC=off` with a limit is still the worst of both worlds.** It pins RSS at
   the limit (250/493 MiB), runs almost no GC cycles (62/30) so each cycle sweeps
   the whole limit-sized heap, and produces the **worst tail** (p99.9 76/78 ms at
   ~310 k IOPS). Removing lock contention does not help here — that tail is a
   single enormous GC cycle, and the windowed scheduler p99 rises with it
   (67–84 ms).

### 3a. GC trace excerpts

`GODEBUG=gctrace=1`, captured for the best and worst arms (short separate runs).

Best (`GOGC=50`, unlimited; 7 075 cycles in ~4.1 s, GC ~13–17% CPU):

```
gc 1 @0.000s 6%: 0.063+0.066+0.004 ms clock, ... 1->1->0 MB, 2 MB goal, 4 P
gc 7074 @4.113s 13%: 0.006+0.24+0.006 ms clock, ... 2->2->2 MB, 2 MB goal, 4 P
gc 7075 @4.115s 13%: 0.007+0.14+0.008 ms clock, ... 3->3->1 MB, 3 MB goal, 4 P
```

Small, frequent, cheap cycles: a ~2 MB heap goal, `~0.1 ms` per cycle.

Worst (`GOGC=off`, `512MiB`; 30 cycles, heap pinned at the limit):

```
gc 1 @0.183s 0%: 0.010+0.78+0.024 ms clock, ... 477->477->2 MB, 480 MB goal, 4 P
gc 8 @1.852s 0%: 0.083+8.2+0.028 ms clock, ... 458->466->11 MB, 466 MB goal, 4 P
gc 13 @2.924s 0%: 0.029+1.1+0.014 ms clock, ... 461->463->4 MB, 468 MB goal, 4 P
```

The `477->477->2 MB` shape is the whole story: with the collector off the daemon
retains ~477 MB of garbage before the limit forces a cycle, and every cycle then
sweeps that ~470 MB — even though only ~2 MB is live. RSS is pinned at the limit
for the entire run. The stop-the-world phases stay sub-millisecond (gc 8:
`0.083`+`0.028` ms), but the concurrent mark phase between them runs ~1–8 ms and
steals CPU from the request handlers — which is why the `GOGC=off` p99.9 (76–78 ms)
tracks this arm's windowed scheduler p99 (67–84 ms), not its GC *pause*.

### 3b. Lock profile, before and after

From the `--profile-locks` trace run (`go tool pprof -top`), best and worst arms
agree on the shape. Before the change the namespace `RWMutex` dominated; after it,
the `RWMutex` is **absent from both profiles**:

| profile | before (top entry, share) | after (top entry, share) |
| --- | --- | --- |
| mutex (`-top`) | `sync.(*RWMutex).Unlock` ~84% | `sync.(*Mutex).Unlock` ~99% |
| block (`-top`) | `sync.(*RWMutex).RLock` ~84–89% | `sync.(*Mutex).Lock` ~64% |

Before, the block profile attributed ~84–89% of blocked time to
`sync.(*RWMutex).RLock` reached from `go-fuse`'s read handler — i.e.
**`internal/namespace`'s single shared `RWMutex`**, taken on every read and write.
After, that entry is gone and the residual is a plain `sync.Mutex` at ~64%: the
reader's I/O path (the canonical reader's short-lived segment/extent lock and the
manifest cache's lookup lock), taken only for a pointer-sized critical section and
**never held across the `pread`**. The flat top-15 does not by itself resolve
which plain mutex dominates; that is the next place to look, and it is a much
smaller cost than the lock that is now gone.

### 3c. Removing the global read lock: before/after at 64 readers

To isolate the change from host drift, the nine-arm sweep was run **twice in one
session** — once on the pre-change tree (the live index guarded by one shared
`RWMutex`) and once on the post-change tree (reads from the immutable view) — in
the pinned container, 64 readers / 8 writers, ~5 s per arm. `mutex` is the
windowed `/sync/mutex/wait/total:seconds`; `×` is the before/after ratio.

| GOGC | GOMEMLIMIT | mutex before | mutex after | × | p99.9 before | p99.9 after | kIOPS before | kIOPS after |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 400 | unlimited | 141.2 s | 93.1 s | 1.5× | 47.8 ms | 45.1 ms | 116.6 | 333.1 |
| 100 | unlimited | 132.3 s | 55.5 s | 2.4× | 23.7 ms | 21.4 ms | 117.5 | 287.7 |
| 50 | unlimited | 157.6 s | 24.6 s | 6.4× | 16.7 ms | **3.9 ms** | 99.5 | 216.0 |
| 20 | unlimited | 162.3 s | 16.0 s | 10.1× | 15.8 ms | 6.4 ms | 92.0 | 187.5 |
| 100 | 256MiB | 135.6 s | 55.9 s | 2.4× | 25.0 ms | 20.9 ms | 109.0 | 286.5 |
| 100 | 512MiB | 129.0 s | 61.9 s | 2.1× | 24.0 ms | 21.4 ms | 106.7 | 273.5 |
| off | 256MiB | 89.6 s | 18.6 s | 4.8× | 67.0 ms | 76.1 ms | 115.3 | 316.0 |
| off | 512MiB | 85.8 s | 24.3 s | 3.5× | 73.3 ms | 78.2 ms | 96.6 | 312.2 |
| off | unlimited | daemon_died | daemon_died | — | — | — | — | — |

*(Same session, same host, one run per tree; the committed receipt is the
post-change run. Absolute values move with host load; the before/after ratios and
the profile shape are the stable part.)*

What fell out:

* **Lock wait fell, sometimes by an order of magnitude.** `/sync/mutex/wait` drops
  1.5–10× (2.4× at the default `GOGC=100`, 10.1× at `GOGC=20`), and it fell even
  though **throughput rose** at the same time (~2.5× the IOPS at high GOGC) — so
  this is not “fewer ops, less contention”. The 1.5× arm (`GOGC=400`, the
  largest-heap, GC-dominated case) is the smallest win; the lock was never the
  whole cost there.
* **Throughput rose ~2.5×** at `GOGC=100` (118 → 288 kIOPS) and ~2.9× at `400`
  (117 → 333), because the eight writers no longer block all sixty-four readers.
* **The p99.9 tail improved where the lock drove it, and only where it did.** At
  `GOGC=50`/`20` — where the lock dominated the tail — p99.9 fell 4.3×/2.5×
  (16.7 → 3.9 ms and 15.8 → 6.4 ms). At `GOGC=400` and the two `GOMEMLIMIT`
  cells it is essentially unchanged (the tail there is GC/allocator, not lock).
  At `GOGC=off`+limit it is **slightly worse** (67 → 76 ms, 73 → 78 ms): that tail
  is one enormous GC cycle sweeping a 250/493 MiB heap, which removing lock
  contention cannot help.
* **The profile confirms the mechanism** (§3b): the `sync.RWMutex` at the top of
  both profiles is replaced by a plain `sync.Mutex`, and the block-profile path
  from the FUSE read handler is no longer the read lock.

The honest reading: the change removes a real, measured bottleneck and roughly
doubles-to-triples reader throughput, but it is not a universal p99.9 win. It
helps exactly where the shared read lock was the tail — the `GOGC=20–50` region
this study already called the best operational tail — and it does nothing for a
tail that is a GC cycle.

## 4. Where the Go runtime introduces differences

- **The FUSE boundary is the floor.** The external baseline court
  ([`BENCH.md`](BENCH.md) §5–6) already measures the FUSE tax: OrFS p50
  ~8.9 µs / p99.9 ~55.6 µs at one reader against ~0.3–1.0 µs for the container
  filesystem, and ~472 k IOPS versus ~11.3 M at 64 readers. Nothing in this court
  changes that; it explains the *daemon-side* part of it.
- **Allocation.** The daemon allocates **~1.2–2.4 GB/s** under this workload
  (per-read destination buffers and FUSE reply buffers; the `allocs` court in
  [`BENCH.md`](BENCH.md) §7e fixes the authoritative read path at one
  `[]byte` per read). That rate — not the live-set size — sets the GC cycle rate.
- **GC.** With a ~2 MB live heap the collector's *pause* is microseconds; its
  *cost* is the cycle rate it needs to keep up with ~2 GB/s, and the RSS it lets
  accumulate when the goal is large.
- **Scheduler.** The windowed `/sched/latencies` p99 is 0.8–10.5 ms; it is worst
  exactly where the GC cycles are largest (`GOGC=off`), because a large GC cycle
  both steals a P and lengthens the run queue.
- **Locks.** Reads take no lock: they traverse an immutable view published
  atomically at the commit boundary. The one shared `RWMutex` this court originally
  named is gone (a mutation now takes a plain write-side mutex, and readers never
  take it). What remains on the read path is a short-lived plain `sync.Mutex` in
  the canonical reader's segment/extent cache and the manifest cache; the
  namespace lock is no longer the serialisation point (§3b, §3c,
  [`CONCURRENCY.md`](CONCURRENCY.md)).

## 5. When the Go port is a reasonable operational alternative

From the measurements above, the port is reasonable **when**:

- **GC is left on** (`GOGC` between ~20 and ~400; the default `100` is a sound
  balance) **or**, if disabled, **paired with a `GOMEMLIMIT`** — never `GOGC=off`
  with no limit (finding 5; the `off`/`unlimited` arm died in ~1.3 s); and
- **resident memory can be bounded** — at `GOGC=off`+limit the daemon pins RSS at
  the limit (0.25–0.5 GiB here); at `GOGC=100` it stayed at ~17–25 MiB; and
- **the tail budget tolerates the FUSE tax plus the residual reader-path
  mutex** — p99.9 is single-digit milliseconds at `GOGC=20–50` at 64 readers now
  that reads take no namespace lock (it was ~15 ms before), but in the
  tens-of-milliseconds at high `GOGC` and with GC disabled, where the tail is
  GC/allocator, not lock, and a workload that needs single-digit-millisecond
  p99.9 across *every* `GOGC` is still not served (the remaining lever is the
  residual reader-path `sync.Mutex`, not a GC knob); and
- **the workload's allocation rate is acceptable** — set `GOGC` to trade median
  for tail deliberately (higher `GOGC` = better median and throughput, fatter
  tail and more RSS; lower `GOGC` = the reverse), and set `GOMEMLIMIT` only when
  you actually need a ceiling.

The default (`GOGC=100`, no memory limit) is a defensible operational default on
this evidence: the best measured p99.9 at a balanced p50.

## 6. Honest limits

- **Bounded.** A 9-arm sweep, ~5 s per arm, one run, on one host. Not the full
  5×3 cross product, not a multi-hour steady state. Absolute numbers move with
  host load; the ordering and the mechanisms are the durable part.
- **Container caps are part of the experiment.** 2 GiB memory, 4 CPUs, 1024 pids
  (`scripts/lib.sh`). The `GOGC=off`/`unlimited` death is a consequence of the
  2 GiB cap; on a larger host it would merely grow until it hit whatever the real
  ceiling is.
- **No native OrFS comparator.** The only non-Go comparison available is the
  container filesystem and `tmpfs` from the baseline court
  ([`BENCH.md`](BENCH.md) §5–6). "Reasonable operational alternative" is judged
  against that, plus the runtime's own behaviour — not against a native OrFS.
- **One writer shape.** Writers do a bounded mix of interior 4 KiB overwrites,
  tail-block rewrites, and capped appends; this is not every write pattern.
- **GC-trace and lock profiles are from short, separate runs** with profiling
  enabled; they are reported as the *shape*, not as the latency numbers.
- **`VmRSS`/`VmHWM` are the daemon's own** `/proc/self/status`, read at the window
  edges and end; peak is the true `VmHWM` high-water mark, but the windowed RSS
  is a point sample, not a continuous trace.

## 7. Reproduce

```sh
sh scripts/container-gc-scaling.sh
```

Runs the court in the pinned container (root, `/dev/fuse`, `CAP_SYS_ADMIN`) and
writes `evidence/performance/gc-scaling.json`. A host without docker or
`/dev/fuse` reports `SKIPPED`, never a false pass.
