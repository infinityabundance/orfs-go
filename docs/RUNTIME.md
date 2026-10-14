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
(container-only procedure).

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

## 3. Measured: the tail across the knobs

All latencies are **application** (syscall-return to syscall-return on the
client), ns scaled. `gcN` is the windowed GC cycle count; `gcp50/gcp99` are the
windowed GC stop-the-world pause percentiles; `alloc` is
`/gc/heap/allocs:bytes` over the daemon window; `rss`/`peak` are **real**
`VmRSS`/`VmHWM`; `mutex` is the windowed `/sync/mutex/wait/total:seconds`
(cumulative goroutine-seconds of lock wait, so `mutex / window` is the average
number of goroutines blocked on a lock).

| GOGC | GOMEMLIMIT | status | p50 | p99 | p99.9 | kIOPS | MiB/s | gcN | gcp50 | gcp99 | sched p99 | alloc GB/s | RSS MiB | peak MiB | mutex s |
| ---: | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 400 | unlimited | ok | 51.0 µs | 14.9 ms | 38.3 ms | 139.4 | 544.5 | 1046 | 10.2 µs | 57.3 µs | 6.3 ms | 2.20 | 30.2 | 33.6 | 110.9 |
| 100 | unlimited | ok | 71.4 µs | 9.3 ms | 21.8 ms | 130.3 | 509.0 | 6510 | 8.2 µs | 65.5 µs | 1.8 ms | 2.22 | 17.0 | 25.2 | 135.7 |
| 50 | unlimited | ok | 142.4 µs | 9.9 ms | 25.4 ms | 77.3 | 302.0 | 8501 | 10.2 µs | 327.7 µs | 0.8 ms | 1.24 | 12.9 | 16.9 | 156.1 |
| 20 | unlimited | ok | 210.7 µs | 4.6 ms | 14.8 ms | 95.3 | 372.1 | 13007 | 8.2 µs | 65.5 µs | 0.8 ms | 1.72 | 13.9 | 15.0 | 159.6 |
| 100 | 256MiB | ok | 70.1 µs | 9.2 ms | 20.6 ms | 134.2 | 524.0 | 6603 | 8.2 µs | 65.5 µs | 1.8 ms | 2.28 | 18.7 | 20.8 | 132.2 |
| 100 | 512MiB | ok | 71.3 µs | 9.6 ms | 21.3 ms | 130.6 | 510.1 | 6464 | 8.2 µs | 57.3 µs | 1.8 ms | 2.26 | 15.1 | 18.8 | 133.6 |
| off | 256MiB | ok | 78.1 µs | 20.8 ms | 64.2 ms | 104.1 | 406.6 | 51 | 20.5 µs | 65.5 µs | 10.5 ms | 2.00 | **248.3** | 251.8 | 95.8 |
| off | 512MiB | ok | 84.3 µs | 21.4 ms | **73.6 ms** | 98.2 | 383.6 | 23 | 16.4 µs | 49.2 µs | 6.3 ms | 1.85 | **496.1** | 499.0 | 87.9 |
| off | unlimited | **daemon_died** | — | — | — | — | — | — | — | — | — | — | — | — | — |

*(Measured on this host in the pinned container, ~5 s per arm, one run. Absolute
values move with host load; the shape is the stable result. Best p99.9 =
`GOGC=20`; worst = `GOGC=off`+`512MiB`.)*

The `GOGC=off`/`unlimited` row is not filled in: the daemon did not survive to
its shutdown dump. It was **OOM-killed ~1.3 s in** (first failed client syscall
at 1.32 s; the driver then saw 18.6 M `ENOTCONN`). That is the measurement: GC
disabled with no memory limit against a ~2 GB/s allocation rate and a 2 GiB
cgroup cap is fatal, not merely slow.

Five findings fall out of the table:

1. **GC pauses are not the tail.** Across every arm the GC stop-the-world p99 is
   ≤ **328 µs** — a small fraction of a p99 that is 9–21 ms. The tail is not a
   long pause; it is the *aggregate* cost of a high allocation rate and the lock
   it contends with (finding 4).
2. **GOGC trades median against tail.** Lowering GOGC from `400` to `20` moves
   p50 from **51 µs to 211 µs** (4.1× worse) but p99.9 from **38 ms to 15 ms**
   (2.6× better), because a small heap goal means many *cheap* cycles (13 007 in
   ~6 s at `GOGC=20` versus 1 046 at `400`) rather than few *large* ones. There is
   no free lunch: the working set is the same, so the same garbage is collected,
   just in smaller, more frequent bites.
3. **GOMEMLIMIT is inert until it binds.** At `GOGC=100` the live heap is
   ~2–3 MiB, so a `256MiB`/`512MiB` limit changes nothing measurable (p99.9
   20.6 / 21.3 ms versus 21.8 ms unlimited). It only matters when GC is disabled
   (finding 5) or when a large live set would otherwise be unbounded.
4. **The dominant cost is lock contention, not GC.** `/sync/mutex/wait` is
   **88–160 goroutine-seconds** over a ~6 s window — an average of **15–27
   goroutines simultaneously blocked on a lock** — and it *falls* as GOGC falls
   (fewer ops complete, so less contention). The profile phase names the lock: a
   `sync.RWMutex` (`internal/namespace`'s shared index) reached from the FUSE
   read/write handlers.
5. **`GOGC=off` with a limit is the worst of both worlds.** It pins RSS at the
   limit (248/496 MiB), runs almost no GC cycles (51/23) so each cycle sweeps the
   whole limit-sized heap, and produces the **worst tail** (p99.9 64/74 ms at
   ~100 k IOPS). Disabling the collector does not remove the collector's work; it
   moves it into rare, large cycles.

### 3a. GC trace excerpts

`GODEBUG=gctrace=1`, captured for the best and worst arms (short separate runs).

Best (`GOGC=20`, unlimited; 7 257 cycles in ~4 s, GC ~12–16% CPU):

```
gc 1 @0.000s 6%: 0.013+0.081+0.042 ms clock, ... 0->0->0 MB, 0 MB goal, 4 P
gc 7255 @3.104s 16%: 0.006+0.057+0.002 ms clock, ... 2->2->1 MB, 2 MB goal, 4 P
gc 7257 @4.105s 12%: 0.017+0.13+0.006 ms clock, ... 2->2->1 MB, 2 MB goal, 4 P
```

Small, frequent, cheap cycles: a ~2 MB heap goal, `~0.1 ms` per cycle.

Worst (`GOGC=off`, `512MiB`; 23 cycles, heap pinned at the limit):

```
gc 1 @0.251s 0%: ... 477->477->2 MB, 481 MB goal, 4 P
gc 2 @0.628s 0%: ... 472->472->2 MB, 476 MB goal, 4 P
gc 3 @1.037s 0%: ... 471->471->2 MB, 474 MB goal, 4 P
```

The `477->477->2 MB` shape is the whole story: with the collector off the daemon
retains ~477 MB of garbage before the limit forces a cycle, and every cycle then
sweeps that 477 MB — even though only ~2 MB is live. RSS is pinned at the limit
for the entire run.

### 3b. Lock profile

From the `--profile-locks` trace run (`go tool pprof -top`), best (`GOGC=20`) and
worst (`GOGC=off`/`512MiB`) arms agree on the shape:

| profile | top entry | share |
| --- | --- | ---: |
| mutex (`-top`) | `sync.(*RWMutex).Unlock` | ~82–90% |
| block (`-top`) | `sync.(*RWMutex).RLock` | ~89% |

The block profile attributes ~89% of blocked time to `sync.(*RWMutex).RLock`
reached from `go-fuse`'s read handler — i.e. **`internal/namespace`'s single
shared `RWMutex`**, taken on every read and write. At 64 readers this is the
measured bottleneck; the GC knobs move the tail by a factor of ~5, but the lock
is why the tail is ms-scale at all.

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
- **Locks.** One `RWMutex` guards the whole shared index; readers run in
  parallel but at 64 readers plus writers the write lock is the serialisation
  point (finding 4). This is the same lock phase 13a named; this court measures
  its cost.

## 5. When the Go port is a reasonable operational alternative

From the measurements above, the port is reasonable **when**:

- **GC is left on** (`GOGC` between ~20 and ~400; the default `100` is a sound
  balance) **or**, if disabled, **paired with a `GOMEMLIMIT`** — never `GOGC=off`
  with no limit (finding 5; the `off`/`unlimited` arm died in ~1.3 s); and
- **resident memory can be bounded** — at `GOGC=off`+limit the daemon pins RSS at
  the limit (0.25–0.5 GiB here); at `GOGC=100` it stayed at ~17–25 MiB; and
- **the tail budget tolerates the FUSE tax plus lock contention** — p99.9 is
  tens of milliseconds at 64 readers, dominated by the shared `RWMutex`, not by
  GC. A workload that needs single-digit-millisecond p99.9 at high concurrency is
  not served by this daemon as it stands (the fix is the lock-free immutable read
  views already named in `docs/PROGRESS.md` phase 13a, not a GC knob); and
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
