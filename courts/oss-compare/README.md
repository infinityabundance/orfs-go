# Open-source storage head-to-head court — step 1 (OrFS-GO vs OrFS-Rust)

A **court** in this repository is an executable test or script that produces a
machine-readable *receipt* and reports honestly. This court is the first step of a
mounted, head-to-head comparison of storage implementations on **the same machine,
the same device, and the same container caps**, driven by **one shared workload
driver**.

Step 1 compares:

- **OrFS-GO** — this repository's Go implementation, built by the pinned court
  image;
- **OrFS-Rust** — the Rust reference, `orfs` v0.2.0, staged at
  `.cache/orfs-rs/orfs` by [`scripts/container-rust-build.sh`](../../scripts/container-rust-build.sh).

Later steps add SeaweedFS FUSE, Btrfs snapshots, and OpenZFS to the same driver
and the same rules. The point of step 1 is not a winner; it is the **comparable
number** plus an honest record of every way the two configurations differ.

## Layout

| Path | What it is |
| --- | --- |
| [`../../cmd/orfscompare`](../../cmd/orfscompare) | The shared, deterministic workload driver (`prepare` + `run`). |
| [`../../cmd/osscomparesummary`](../../cmd/osscomparesummary) | Merges the per-arm run receipts into the court receipt. |
| [`../../internal/osscompare`](../../internal/osscompare) | The receipt vocabulary + the merge rules (row keys, determinism). |
| [`../../scripts/oss-compare-court.sh`](../../scripts/oss-compare-court.sh) | The court entry point — runs *inside* the court container as root. |
| [`../../scripts/container-oss-compare.sh`](../../scripts/container-oss-compare.sh) | The host wrapper (capped court container, Rust binary bind-mounted). |
| [`../../evidence/oss-compare.json`](../../evidence/oss-compare.json) | The receipt (schema `orfs.oss-compare.v1`). |

## The shared driver

`cmd/orfscompare` drives a **mounted** directory, one implementation at a time.
It has two workloads and they are the same code for every system:

**`append` — the durable append.** Every writer owns its own precreated file and
appends fixed 4 KiB records with `write(2)`+`fsync(2)` per record: **one
durability barrier per record**, equal on both systems. It runs at **1 and 64
concurrent writers** for a bounded duration and reports operations/s, bytes/s and
the per-record latency `p50/p95/p99/p99.9`. This is the durable-append figure — it
is deliberately **not** the zero-byte batched-publish number in
`evidence/performance/groupcommit.json` (see [`BENCH.md`](../../docs/BENCH.md)
§7d).

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

## How the two systems are mounted

Both are mounted on the same machine inside the pinned court container
(`--platform linux/amd64`, root, `/dev/fuse`, `CAP_SYS_ADMIN`, and the shared
`scripts/lib.sh` caps). The court uses the settings that make the two
**comparable**, and records every difference that remains.

| Knob | OrFS-GO | OrFS-Rust 0.2.0 |
| --- | --- | --- |
| Mount command | `orfs mount <vol> <mnt> --direct-io` | `orfs mount <vol> <mnt> --direct-io --durability sync --backing-io sync` |
| FUSE userspace | go-fuse/v2 | the `fuser` crate |
| Transport | `f0` (default multi-reader loop) | `fuse` (classical) |
| Durability | one barrier per publish (group commit **OFF**: `ORFS_GROUP_COMMIT` unset) | `--durability sync`: one `fsync` per transaction |
| Backing I/O | synchronous pwrite/pread | pinned to sync (`--backing-io sync`; its default here is `io_uring`) |
| `--direct-io` | yes (`FOPEN_DIRECT_IO`) | yes (`FOPEN_DIRECT_IO`) |
| Segment size | 8 MiB | 8 MiB |
| Record / read file | 4 KiB seeded records, 64 MiB seeded read file | same |

## Comparability rules

The receipt is **data, not a leaderboard**. These rules are enforced in the driver
and the merge, and stated in the receipt's `comparability` block:

1. **Same machine, same device, same caps.** One container enforces the same
   memory/swap/CPU/pids caps for both mounts; the caps are recorded.
2. **Equal durability.** Both take one `fsync` per 4 KiB record. The append arm is
   *the same* `write`+`fsync` on both sides.
3. **Same `--direct-io`.** Both mount with `FOPEN_DIRECT_IO`. Where a knob is
   unavailable on one side, it is recorded as a caveat rather than silently
   differing.
4. **Same payload and geometry.** The same 4 KiB record size, the same seeded
   contents, the same segment size, the same read file.
5. **Every difference is named.** The `comparability` block lists the FUSE
   library, transport shape, backing-I/O engine, and any captured daemon notes.
   A **documented asymmetry beats a fake parity**.
6. **Rows are keyed.** A row is `{system, workload, writers, readers, block_size}`;
   the merge rejects a duplicate key, and two rows are comparable **only** at the
   same coordinate.
7. **No ranking.** The receipt carries no ordering, score, or winner. Read the
   comparability block before drawing any conclusion.

If OrFS-Rust cannot be mounted the same way, the court runs it as best it allows
(or skips its rows) and records the exact difference and the captured daemon
stderr in `comparability.systems."OrFS-Rust"` — it never claims parity it did not
measure.

## Run it

```sh
sh scripts/container-oss-compare.sh
```

The wrapper builds the Rust reference if needed, builds the court image, and runs
the court in the capped container with the Rust binary bind-mounted read-only. A
host without docker or `/dev/fuse` reports `SKIPPED` rather than a false pass. The
court is wired into [`scripts/container-verify.sh`](../../scripts/container-verify.sh)
(SKIPPED-aware).

Knobs (all optional, forwarded by the wrapper):

| Env | Default | Meaning |
| --- | --- | --- |
| `SEGMENT_SIZE` | `8388608` | OrFS volume segment size (both systems) |
| `APPEND_SECONDS` | `5` | duration of each append arm |
| `READ_SECONDS` | `4` | duration of each read arm |
| `WRITERS` | `1 64` | append writer counts to measure |
| `READERS` | `1 64` | read concurrency levels to measure |
| `BLOCK_SIZES` | `4096 65536 1048576` | read block sizes |
| `RECORD_BYTES` | `4096` | append record size |
| `READ_MIB` | `64` | seeded read file size |
| `MAX_APPEND_MIB` | `1024` | per-writer append byte cap (safety bound) |
| `SEED` | `1` | deterministic seed |

## Honest notes

- **It is a bounded laboratory run, not an endurance test.** One run of the
  matrix; run-to-run variance is expected and is not quantified here.
- **The driver measures the syscall, on the mounted path.** Append latency is the
  `write`+`fsync` pair; read latency is the `pread`. It includes each
  implementation's own FUSE round trip and durability cost, which is exactly what
  an operator comparing two mounted filesystems needs.
- **The two daemons are independent implementations.** The dominant uncontrolled
  variable is the FUSE userspace library (go-fuse/v2 vs the `fuser` crate); the
  receipt says so rather than pretending the comparison is library-free.
- **This is one comparator pair.** It does not establish that either
  implementation is "faster" in general; it publishes one comparable measurement
  under a recorded configuration.
