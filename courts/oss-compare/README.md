# Open-source storage head-to-head court — step 2 (OrFS-GO vs OrFS-Rust vs SeaweedFS)

A **court** in this repository is an executable test or script that produces a
machine-readable *receipt* and reports honestly. This court is a mounted,
head-to-head comparison of storage implementations on **the same machine, the same
device, and the same container caps**, driven by **one shared workload driver**.

Step 2 compares three systems:

- **OrFS-GO** — this repository's Go implementation, built by the pinned court
  image;
- **OrFS-Rust** — the Rust reference, `orfs` v0.2.0, staged at
  `.cache/orfs-rs/orfs` by [`scripts/container-rust-build.sh`](../../scripts/container-rust-build.sh);
- **SeaweedFS** — its FUSE client, `weed mount`, over a single-node master +
  volume + filer. The `weed` binary is staged from the pinned
  `chrislusf/seaweedfs:3.99` image by `stage_seaweedfs` in
  [`scripts/lib.sh`](../../scripts/lib.sh): the image is pulled with an explicit
  `--platform linux/amd64` and the single static binary is extracted with
  `docker create` + `docker cp` (the image is **never run on the host**), then
  bind-mounted read-only into the capped court container.

Later steps add Btrfs snapshots and OpenZFS to the same driver and the same rules.
The point of the court is not a winner; it is the **comparable number** plus an
honest record of every way the configurations differ.

## Layout

| Path | What it is |
| --- | --- |
| [`../../cmd/orfscompare`](../../cmd/orfscompare) | The shared, deterministic workload driver (`prepare` + `run`). |
| [`../../cmd/osscomparesummary`](../../cmd/osscomparesummary) | Merges the per-arm run receipts into the court receipt; `-expect-systems` refuses to drop a claimed system. |
| [`../../internal/osscompare`](../../internal/osscompare) | The receipt vocabulary + the merge rules (row keys, determinism, system set). |
| [`../../scripts/oss-compare-court.sh`](../../scripts/oss-compare-court.sh) | The court entry point — runs *inside* the court container as root. |
| [`../../scripts/container-oss-compare.sh`](../../scripts/container-oss-compare.sh) | The host wrapper (capped court container; Rust and `weed` binaries bind-mounted). |
| [`../../scripts/lib.sh`](../../scripts/lib.sh) | The pinned SeaweedFS image + `stage_seaweedfs`. |
| [`../../evidence/oss-compare.json`](../../evidence/oss-compare.json) | The receipt (schema `orfs.oss-compare.v1`). |

## The shared driver

`cmd/orfscompare` drives a **mounted** directory, one mounted filesystem at a time.
It has two workloads and they are the same code for every system:

**`append` — the durable append.** Every writer owns its own precreated file and
appends fixed 4 KiB records with `write(2)`+`fsync(2)` per record: **one
durability barrier per record**. It runs at **1 and 64 concurrent writers** for a
bounded duration and reports operations/s, bytes/s and the per-record latency
`p50/p95/p99/p99.9`. This is the durable-append figure — it is deliberately **not**
the zero-byte batched-publish number in `evidence/performance/groupcommit.json`
(see [`BENCH.md`](../../docs/BENCH.md) §7d). The driver issues the *same* syscalls
on every system, but a system is only equal-durability if its `fsync` is a durable
barrier; the court records whether it is (see the not-comparable axes).

**`read` — block-aligned random reads.** Over a seeded file: `pread` at uniform
random block-aligned offsets at **4 KiB, 64 KiB and 1 MiB** block sizes, at 1 and
64 readers, reporting IOPS, bytes/s and `p50/p95/p99/p99.9`.

Design guarantees:

- **Deterministic.** A fixed `-seed` (default 1) seeds every worker's RNG and the
  seeded read file's contents, so two runs issue the same shape of I/O.
- **Bounded.** A duration always ends the run, and a per-writer byte cap
  (`-max-bytes-per-writer`) bounds appended growth (OrFS retains appended history,
  so an unbounded append storm is not acceptable). The receipt records the elapsed
  window and whether an arm ended at the cap; if it did, its rate is a lower bound.
- **Exact latency.** Each worker keeps a private sample slice (no lock on the hot
  path); the slices are merged and sorted at the end, so a p99.9 is a real observed
  sample, not a bucket edge. A per-worker sample cap bounds the driver's memory,
  and the receipt flags `sample_buffer_truncated` if it binds.
- **Errno-classified errors.** Every failed or short operation is charged to an
  errno class, and the arm reports an error rate.
- **Prepare outside the window.** `prepare` creates each writer's file (and the
  seeded read file) before the measured window, so file creation is not charged to
  the append number and every writer has a dedicated file.
- **Order and isolation.** Within each system the **read arms run before the append
  arms**, on a quiet volume: OrFS retains appended history, so measuring appends
  first would evict the read file from the backing page cache and turn every
  `--direct-io` read into a backing I/O. The systems' arms run **strictly
  sequentially**, and a finished system's daemon and volume (or SeaweedFS's mount
  and cluster) are released before the next system's arms, so no two compete for
  the shared memory cap (an earlier run OOM-killed a daemon mid-arm on a 2 GiB cap).

### The receipt can be partial, and says so

The comparability block carries an `arms` object:
`{expected_per_system, OrFS-GO:{ran,failed}, OrFS-Rust:{ran,failed},
SeaweedFS:{measured,status,ran,failed}, failed:[…]}`.
Every arm that fails (its daemon died, an I/O error, a timeout) is counted and
**named** in `failed`, so `ran + failed == expected_per_system` for each system
that mounted. A mounted system that lost an arm leaves a **partial receipt**, and
the court **FAILs** — it never reports PASS over missing rows. A system that did
**not** mount reports `measured:false` with its exact status (a named flavour of
`not_attempted`/`server_failed`/`mount_failed`/`prepare_failed`), is printed as a
`SKIPPED` line, and is recorded — never silently omitted.

## How the systems are mounted

All are mounted on the same machine inside the pinned court container
(`--platform linux/amd64`, root, `/dev/fuse`, `CAP_SYS_ADMIN`, and the shared
`scripts/lib.sh` caps). The court uses the settings that make the systems
**comparable**, and records every difference that remains.

| Knob | OrFS-GO | OrFS-Rust 0.2.0 | SeaweedFS 3.99 |
| --- | --- | --- | --- |
| Bring-up | `orfs init` then `orfs mount <vol> <mnt> --direct-io` | `orfs init` then `orfs mount … --durability sync --backing-io sync` | `weed server -filer=true` (master+volume+filer) then `weed mount` |
| FUSE userspace | go-fuse/v2 | the `fuser` crate | go-fuse/v2 (the same library as OrFS-GO) |
| Transport | `f0` (default multi-reader loop) | `fuse` (classical) | go-fuse (classical) |
| Durability | one barrier per publish (group commit **OFF**) | `--durability sync`: one `fsync` per transaction | `Fsync` = flush dirty pages to the co-located volume server + gRPC filer commit; **not** an fsync of the volume `.dat`/filer store (**not** a durable barrier) |
| Data path | in-process daemon | in-process daemon | client/server: loopback HTTP to the volume server, gRPC to the filer |
| Backing I/O | synchronous pwrite/pread | pinned to sync (`--backing-io sync`) | volume server writes its own `.dat`/`.idx`; no per-write fsync |
| `--direct-io` | yes (`FOPEN_DIRECT_IO`) | yes (`FOPEN_DIRECT_IO`) | no such knob |
| Segment size | 8 MiB | 8 MiB | — (chunked, not segment-based) |
| Record / read file | 4 KiB seeded records, 64 MiB seeded read file | same | same |

## Comparability rules

The receipt is **data, not a leaderboard**. These rules are enforced in the driver
and the merge, and stated in the receipt's `comparability` block:

1. **Same machine, same device, same caps.** One container enforces the same
   memory/swap/CPU/pids caps for every system; the caps are recorded.
2. **Equal durability is claimed only where it holds.** The append arm is the same
   `write`+`fsync` on every system, but OrFS-GO and OrFS-Rust each take one durable
   `fsync` per 4 KiB record while SeaweedFS's `Fsync` flushes to a co-located
   server without fsyncing stable storage. That difference is a `not_comparable`
   axis (`append durability`); the SeaweedFS append rows are never read as
   equal-durability.
3. **Same `--direct-io` where available.** OrFS-GO and OrFS-Rust mount with
   `FOPEN_DIRECT_IO`; `weed mount` exposes no such knob, recorded as a
   `not_comparable` axis rather than silently differing.
4. **Same payload and geometry.** The same 4 KiB record size, the same seeded
   contents, and the same read file.
5. **Every difference is named.** The `systems` block lists each FUSE library,
   transport shape, backing-I/O engine, and captured daemon notes; the
   `not_comparable` block names the axes that must not be ranked (durability, data
   path, read-path caching shape, direct-I/O). A **documented asymmetry beats a
   fake parity**.
6. **Rows are keyed.** A row is `{system, workload, writers, readers, block_size}`;
   the merge rejects a duplicate key, and two rows are comparable **only** at the
   same coordinate.
7. **No ranking.** The receipt carries no ordering, score, or winner. Read the
   comparability block before drawing any conclusion.
8. **No silent holes.** Every expected arm must run; `comparability.arms` names
   any that did not, `-expect-systems` makes the merge refuse to drop a system it
   has rows for, and a partial receipt is a FAIL, not a PASS. A system that could
   not be mounted is recorded as a named `SKIPPED`, never as a pass.

If OrFS-Rust or SeaweedFS cannot be mounted the same way, the court runs it as
best it allows (or skips its rows), records the exact difference and the captured
daemon stderr in `comparability.systems`, and prints a `SKIPPED` line — it never
claims parity it did not measure.

## Run it

```sh
sh scripts/container-oss-compare.sh
```

The wrapper builds the Rust reference if needed, stages the `weed` binary from
the pinned image, builds the court image, and runs the court in the capped
container with the Rust and `weed` binaries bind-mounted read-only. A host without
docker or `/dev/fuse` reports `SKIPPED` rather than a false pass; a host that
cannot pull the SeaweedFS image still measures the OrFS pair and records the
SeaweedFS `SKIPPED`. The court is wired into
[`scripts/container-verify.sh`](../../scripts/container-verify.sh) (SKIPPED-aware).

Knobs (all optional, forwarded by the wrapper):

| Env | Default | Meaning |
| --- | --- | --- |
| `SEGMENT_SIZE` | `8388608` | OrFS volume segment size (both OrFS systems) |
| `APPEND_SECONDS` | `5` | duration of each append arm |
| `READ_SECONDS` | `4` | duration of each read arm |
| `WRITERS` | `1 64` | append writer counts to measure |
| `READERS` | `1 64` | read concurrency levels to measure |
| `BLOCK_SIZES` | `4096 65536 1048576` | read block sizes |
| `RECORD_BYTES` | `4096` | append record size |
| `READ_MIB` | `64` | seeded read file size |
| `MAX_APPEND_MIB` | `1024` | per-writer append byte cap (safety bound) |
| `SEED` | `1` | deterministic seed |
| `SW_GOGC` / `SW_GOMEMLIMIT` | `50` / `700MiB` | Go GC bounds on both `weed` processes (fit the shared 2 GiB cap; recorded) |
| `ORFS_SEAWEEDFS_IMAGE` | `chrislusf/seaweedfs:3.99` | the pinned SeaweedFS image |

## Honest notes

- **It is a bounded laboratory run, not an endurance test.** One run of the
  matrix; run-to-run variance is expected and is not quantified here.
- **The driver measures the syscall, on the mounted path.** Append latency is the
  `write`+`fsync` pair; read latency is the `pread`. It includes each
  implementation's own FUSE round trip — and, for SeaweedFS, its loopback network
  round trips and client cache — which is exactly what an operator comparing
  mounted filesystems needs, provided the durability caveat is kept in view.
- **The daemons are independent implementations.** OrFS-GO and SeaweedFS share
  go-fuse/v2; OrFS-Rust uses the `fuser` crate. The receipt says so rather than
  pretending the comparison is library-free.
- **This is one configuration each.** It does not establish that any
  implementation is "faster" in general; it publishes comparable measurements
  under a recorded configuration.
