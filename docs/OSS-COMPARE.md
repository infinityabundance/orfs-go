# OrFS-GO versus open-source storage: what the courts actually measured

This document answers one question, with measured numbers and no ranking:

> **Can a system retain *every* committed historical transition while achieving
> comparable tail latency, throughput, and physical-space economics?**

It gathers the results of the courts in this repository — a mounted head-to-head
against the Rust reference and SeaweedFS, and a retention comparison against
CoW-snapshot filesystems — into one place, states exactly which rows may be
compared, and names what is **not** established.

Every number below comes from a committed receipt under `evidence/`. No number a
court did not measure appears here. Nothing here is a leaderboard: rows are keyed
by `{system, workload, writers, readers, block_size}`, and two rows are
comparable only at the same coordinate under the caveats stated with them.

- Head-to-head (OrFS-GO / OrFS-Rust / SeaweedFS): [`BENCH.md`](BENCH.md) §7i,
  receipt [`../evidence/oss-compare.json`](../evidence/oss-compare.json)
- Retention (Btrfs snapshots): [`BENCH.md`](BENCH.md) §7j, receipt
  [`../evidence/retention-btrfs.json`](../evidence/retention-btrfs.json)
- Retention (OpenZFS): [`BENCH.md`](BENCH.md) §7k — **real OpenZFS in a KVM micro-VM** (the host has no ZFS module and is never modified), receipt
  [`../evidence/retention-zfs.json`](../evidence/retention-zfs.json)
- Native external baselines (ext4/tmpfs/overlay): [`BENCH.md`](BENCH.md) §5,
  receipt [`../evidence/performance/baseline.json`](../evidence/performance/baseline.json)
- Retained-history amplification: [`BENCH.md`](BENCH.md) §7, receipt
  [`../evidence/performance/amplification.json`](../evidence/performance/amplification.json)

---

## 1. The answer, in one paragraph

**On retention, yes; on raw throughput, no.** OrFS-GO retains *every* committed
transition **by construction** — the authoritative log is append-only and is never
GC'd — with **no snapshot step**, at **1.116×** physical amplification for the
retention workload, against **1.246×** for a Btrfs read-only snapshot after every
epoch (comparable space economics, and OrFS needs no operation to create a
retained state). It is **not** an ext4 replacement: as a FUSE filesystem it pays
the FUSE tax (4 KiB random read p99 ~44× a native ext4/tmpfs, §5) and its read
throughput is **0.5–0.9×** the Rust reference's under the same driver. So the
honest summary is: this is a credible **audit / WORM / provenance** filesystem
where exact, never-truncated history matters more than raw IOPS — not a
general-purpose high-throughput store. That framing is the standing position of
[`LIMITATIONS.md`](LIMITATIONS.md) §1 and the WORM/audit-sink case studies
([`WORM.md`](WORM.md), [`AUDIT-SINK.md`](AUDIT-SINK.md)).

## 2. The systems, and what is comparable

| system | what it is | comparable with OrFS-GO on |
| --- | --- | --- |
| **OrFS-GO** | this repository: pure-Go FUSE, append-only authoritative log, permanent history | — |
| **OrFS-Rust 0.2.0** | the Rust reference, same design and wire format | append, read (equal durability, same `--direct-io`, same backing) |
| **SeaweedFS 3.99** | a client/server filesystem (`weed mount` over master+volume+filer) | **nothing on durability**; read/append rows only as raw syscalls |
| **Btrfs** | a native CoW local filesystem with read-only snapshots | retention *space economics*; not read/append latency (native vs FUSE) |
| **OpenZFS** | a native CoW local filesystem with snapshots | retention economics, **only within its own guest** — real OpenZFS is measured in a KVM micro-VM whose numbers are guest-inflated (§7k) |
| **ext4 / tmpfs / overlay** | native local baselines | the *FUSE tax* only (§5) |

The single most important caveat: **OrFS-GO and OrFS-Rust are the only
apples-to-apples pair in this document.** Everything else differs in durability,
data path, or latency mechanism, and is marked not-comparable for that axis in the
receipts.

## 3. The retention result (the core finding)

The retention court (§7j) drives one workload — 8 epochs, each appending 256
records of 4 KiB with `write(2)`+`fsync(2)` per record — against a Btrfs subvolume
snapshotted **read-only after every epoch** and against an OrFS-GO volume that
retains every transition in its log with **no snapshot step**. After 8 epochs both
systems hold every committed state: Btrfs has 8 snapshots, OrFS-GO has 8 committed
transitions.

| | retained states | mechanism | physical / logical (8 MiB logical) | per-snapshot exclusive bytes | snapshot create |
| --- | ---: | --- | ---: | ---: | ---: |
| **Btrfs** | 8 | read-only snapshot after each epoch | **1.246×** (10.45 MiB) | **0** (all extents shared) | 673.6 / **730.3** / 810.6 µs (min/median/max) |
| **OrFS-GO** | 8 | none — every transition is already in the log | **1.116×** (9.36 MiB) | n/a | n/a |

Read this carefully:

- **Space economics are comparable, and OrFS-GO is slightly leaner here** (1.116×
  vs 1.246×) — but the two ratios are *different computations* (Btrfs physical is
  the whole device's `Used` including CoW metadata; OrFS-GO physical is the
  authoritative `or.log` bytes), so this is **not** a like-for-like ranking. What
  it shows is that both retain every state for roughly the same order of cost.
- **Btrfs snapshots are cheap because they share extents** (exclusive bytes 0) —
  that is the CoW design working as intended, and it is a genuine strength.
- **OrFS-GO needs no snapshot**: the retained state is a by-product of the append.
  There is no snapshot create to schedule, nothing to coalesce, and no chance of
  forgetting to snapshot. That — not a throughput number — is the reason to choose
  OrFS-GO.
- **A warm historical snapshot read is as fast as a warm current read** for Btrfs
  (p50 0.19 vs 0.18 µs). OrFS-GO's equivalent axis is **not measured**, because
  this build does not serve named historical roots ([`LIMITATIONS.md`](LIMITATIONS.md)
  §10); it is recorded as a non-comparability, not as a win.

The price of permanent history is visible in the amplification court (§7): a
workload that keeps overwriting the *same* file grows its retained physical bytes
monotonically, **1.07× → 24.56×** of the logical file over 24 epochs
(`evidence/performance/amplification.json`). Non-withdrawal means history is never
reclaimed; that is the design, and it is not free.

## 4. Throughput and tail latency

### 4.1 OrFS-GO vs OrFS-Rust — the comparable pair (§7i)

Both are FUSE daemons with the same design, mounted with equal durability (one
`fsync` per appended record) and the same `--direct-io`, on the same machine under
the same 2 GiB/4 CPU/pids-1024 caps. In the current run:

| | append 1 writer | append 64 writers | read 4K/1 | read 4K/64 | read 1M/1 | read 1M/64 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| **OrFS-GO** | 20,523 ops/s (p50 39.2 µs) | 28,910 (p50 1.83 ms) | 100,469 IOPS | 349,091 | 3,572 | 8,259 |
| **OrFS-Rust** | 29,274 ops/s (p50 32.5 µs) | 30,942 (p50 2.04 ms) | 128,264 | 506,775 | 6,990 | 15,827 |
| ratio (GO/Rust) | 0.70× | 0.93× | 0.78× | 0.69× | 0.51× | 0.52× |

OrFS-GO is **0.5–0.93×** the reference across the board. The largest losses are
the 1 MiB reads (0.51–0.52×) and the 64-reader tails — go-fuse transport and
per-request buffering, not the log or the index. This is published as measured.

**This was much worse before.** The first run of this court measured OrFS-GO at
**3,097 ops/s** single-writer (p50 297 µs) — **8.8×** behind the reference. The
cause was a genuine algorithmic defect: the file extent map was a flat slice, and
because each 4 KiB append's bytes live at a new log offset the extents never
coalesce, so the list grew 1:1 with the append count and every write was O(N) —
an **O(N²)** append run, fatal for the unbounded-history workload OrFS exists for.
It is fixed by making the extent map a persistent path-copying treap (the Rust
reference's Phase 27 `PMap`; `internal/index/extentmap.go`), proven equivalent to
the old flat map by a 20,000-step randomized differential test, with per-append
cost measured **flat** from 256 to 8,192 appends
(`evidence/performance/append-scaling.json`). See [`BENCH.md`](BENCH.md) §7i.

### 4.2 SeaweedFS — measured, but not comparable on durability (§7i)

| | append 1 writer | append 64 writers | read 4K/1 | read 64K/1 | read 1M/1 |
| --- | ---: | ---: | ---: | ---: | ---: |
| **SeaweedFS 3.99** | 193 ops/s (p50 3.51 ms) | 1,970 | 1,586 IOPS | 210,240 | 19,889 |

**Do not read these as equal-durability results.** SeaweedFS's FUSE client
implements `Fsync` as a flush of client dirty pages to the co-located volume
server over loopback HTTP plus a gRPC filer commit
(`weed/mount/weedfs_file_sync.go`: `Fsync` → `doFlush` → `dirtyPages.FlushData`);
it does **not** fsync the volume `.dat` or the filer store. The driver issues
`write(2)`+`fsync(2)` per record, but SeaweedFS offers no documented per-record
durability barrier, so its append rate is **not** one-barrier-per-record. It is
also a client/server filesystem (data over the loopback network stack, metadata
over gRPC) with no `--direct-io` knob, and its read path served 64 KiB reads from
an in-process chunk cache (~0.2–1.0M IOPS) while 4 KiB reads ran only ~1.6–2.2k
IOPS. All four axes are recorded in the receipt's `not_comparable` block.

### 4.3 The FUSE tax against native filesystems (§5, §6)

4 KiB random reads on the same box, at `iodepth=1`
(`evidence/performance/baseline.json`) and at `numjobs=64`
(`evidence/performance/scale.json`):

| target | p50 (1) | p99 (1) | IOPS (1) | p50 (64) | p99 (64) | IOPS (64) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| tmpfs | 302 ns | 604 ns | 2,188,104 | 270 ns | 1,096 ns | 14,576,468 |
| ext4 / overlay | 370 ns | 644 ns | 1,911,238 | 422 ns | 1,336 ns | 11,327,140 |
| **OrFS (FUSE, `--direct-io`)** | **8,896 ns** | **28,544 ns** | **102,195** | **95,744 ns** | **593,920 ns** | **472,024** |

That is roughly **24× worse p50 and 44× worse p99** at `iodepth=1`, and OrFS
reaches ~472k IOPS at 64 readers against ~11.3M for the container filesystem: the
daemon does not scale like a native stack. The FUSE tax is published rather than
hidden. A native CoW filesystem's read row is not a fair comparator for a FUSE
filesystem's read row, which is why the Btrfs and OpenZFS courts compare
**retention economics**, not read latency.

## 5. What OrFS-GO does *not* win

Stated plainly, so no reader has to infer it:

- **Raw throughput and tail latency vs native filesystems.** ~24×/44× worse 4K
  random read p50/p99 than ext4/tmpfs (§4.3); a native filesystem is simply
  faster. OrFS is not an ext4 replacement.
- **Read throughput vs the Rust reference.** 0.5–0.93× (§4.1); the large-block
  reads are the worst.
- **Cold-cache historical reads.** Not measured; the Btrfs historical read is
  warm, and OrFS-GO has no historical-read axis at all (§7j caveats).
- **Named historical roots / mount-time bounds.** Not served in this build; the
  authoritative open scan is unbounded ([`LIMITATIONS.md`](LIMITATIONS.md) §2, §10).
- **F2 (FUSE-over-io_uring).** Not implemented; `--transport fuse-uring`
  negotiates and falls back loudly ([`TRANSPORT.md`](TRANSPORT.md)).
- **Space economics under a no-overwrite workload.** Amplification grows
  monotonically with retained history by design (§3); there is no compaction.
- **OpenZFS physical bytes are not ranked.** Real OpenZFS is now measured (§7k), but inside a KVM micro-VM on a virtio disk, and its physical figure is compression-aware (`compression=lz4` by default) — so those numbers are guest-inflated and are not like-for-like with OrFS's uncompressed `or.log`. They compare the two arms to each other only.

## 6. Methodology and comparability rules

Every court in this document follows the rules in
[`courts/oss-compare/README.md`](../courts/oss-compare/README.md):

1. **Same machine, same device, same caps** — one pinned container, 2 GiB / 4 CPU
   / pids 1024, recorded in each receipt.
2. **One shared driver** — `cmd/orfscompare` / `cmd/retentionbench` issue the same
   syscalls (`write`+`fsync` per 4 KiB record; block-aligned random `pread` at
   4 KiB / 64 KiB / 1 MiB) against every mounted system.
3. **Equal durability where claimed** — one `fsync` per appended record on OrFS-GO
   and OrFS-Rust; recorded as *not* equal for SeaweedFS.
4. **The same `--direct-io` where available** — recorded as unavailable for
   SeaweedFS.
5. **Every difference is named** in the receipt's `comparability` /
   `not_comparable` block. A documented asymmetry beats a fake parity.
6. **No ranking** — rows are keyed and never ordered or scored.
7. **No silent holes** — a lost arm is counted and named, and a partial receipt is
   a **FAIL**, never a PASS.

Additional discipline learned while running these courts and now enforced:

- **Order and memory.** Within each system the read arms run before the append
  arms, on a quiet volume, because a retained append history evicts the read file
  and turns every `--direct-io` read into a backing I/O. The two systems' arms run
  strictly sequentially, and a finished system's daemon and volume are released
  before the next system's arms, so neither competes for the shared memory cap (an
  earlier run OOM-killed a daemon mid-arm).
- **Nothing runs on the host.** All builds, tests, benchmarks and courts run
  inside resource-capped containers (`scripts/run.sh`, `scripts/container-*.sh`).
- **An honest SKIPPED is a success.** A precise, single-line SKIPPED naming the
  missing capability is preferred to a workaround that changes the question.

## 7. Reproduce

```sh
sh scripts/container-oss-compare.sh        # OrFS-GO vs OrFS-Rust vs SeaweedFS  -> evidence/oss-compare.json
sh scripts/container-retention-btrfs.sh    # Btrfs retention vs the OrFS-GO log -> evidence/retention-btrfs.json
sh scripts/container-retention-zfs.sh      # real OpenZFS in a KVM micro-VM       -> evidence/retention-zfs.json
sh scripts/container-verify.sh             # the whole court gate, SKIPPED-aware
```

Each wrapper reports `PASS`, `FAIL`, or `SKIPPED (reason)`; a missing `/dev/fuse`,
a missing privileged container, or an un-pullable image is a SKIPPED, never a false
pass.

## 8. Status: what is still missing

This is an honest status line, not a roadmap promise. See
[`LIMITATIONS.md`](LIMITATIONS.md) for the standing list.

- **Named historical roots** are not served (no OrFS-GO historical-read row).
- **F2 transport** (FUSE-over-io_uring) is unimplemented; `fuse-uring` negotiates
  and falls back.
- **No cold-cache retention number**, no long-history retention curve, no
  endurance curve; all retention numbers are one bounded run.
- **OpenZFS numbers are guest-inflated** — they are measured inside a KVM micro-VM (nested virtio, a 384 MiB guest), so they compare only within that guest and not against the container courts.
- **SeaweedFS is not equal-durability**; its rows are syscall-comparable only.
- **Run-to-run variance is not quantified**; every court states that once.
