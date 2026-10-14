# Btrfs retained-snapshots retention court

A **court** in this repository is an executable test or script that produces a
machine-readable *receipt* and reports honestly. This court answers a
**retention** question, not an IOPS question:

> Can a system retain every committed historical transition while achieving
> comparable tail latency, throughput and physical-space economics?

OrFS retains history in its **authoritative log** (non-withdrawal, Law I): every
committed transition is kept, with no extra step. Btrfs retains it with a
**read-only snapshot** taken after every epoch. The court drives both with **one
shared, deterministic, bounded epoch workload** on the same machine, the same
container caps, and same-sized loop-mounted Btrfs backings.

## Layout

| Path | What it is |
| --- | --- |
| [`../../cmd/retentionbench`](../../cmd/retentionbench) | The workload driver (`prepare`/`append`/`read`/`space`/`snapshot`). |
| [`../../cmd/retentionsummary`](../../cmd/retentionsummary) | Merges the per-arm receipts into the court receipt. |
| [`../../internal/retention`](../../internal/retention) | The receipt vocabulary + the merge rules (row keys, determinism). |
| [`../../scripts/retention-btrfs-court.sh`](../../scripts/retention-btrfs-court.sh) | The court entry point — runs *inside* the container as root. |
| [`../../scripts/container-retention-btrfs.sh`](../../scripts/container-retention-btrfs.sh) | The host wrapper (privileged court container: loop devices + `mkfs.btrfs`). |
| [`../../evidence/retention-btrfs.json`](../../evidence/retention-btrfs.json) | The receipt (schema `orfs.retention.btrfs.v1`). |

## The workload

`cmd/retentionbench` drives a **mounted directory**, one arm at a time and one
system at a time, with the same durable-append shape as the head-to-head court
([`../oss-compare/README.md`](../oss-compare/README.md)):

- **append** — every writer owns its own precreated file and appends fixed 4 KiB
  records with `write(2)`+`fsync(2)` per record: **one durability barrier per
  record**, equal on both systems. But unlike the head-to-head court it appends a
  **defined count** of records per epoch, so the committed state after each epoch
  is well defined and is exactly what is retained. Latency is exact observed
  samples (per-worker slices merged and sorted), so a p99.9 is a real sample.
- **read** — block-aligned uniform-random `pread` over the current state or a
  retained snapshot, reporting IOPS/bytes/s and `p50/p95/p99/p99.9`.
- **space** — physical vs logical bytes of one state: `btrfs filesystem usage`
  (device Used) and `btrfs filesystem du -s` (per-subvolume Total/Exclusive/Set
  shared), or, for OrFS-GO, the disk blocks of the authoritative `or.log`.
- **snapshot** — a timed `btrfs subvolume snapshot -r`, the cost of creating one
  retained historical state.

Design guarantees, as in the head-to-head court: **deterministic** (a fixed seed
plus a record body that encodes its position in history, so two runs are
byte-identical), **bounded** (an epoch has a fixed record count and a time safety
valve), **exact latency**, and **errno-classified** errors.

## What is measured, and what is not comparable

The court's `comparability` block names the axes explicitly:

**Comparable.** Current-state append (the same defined epoch over a loop-mounted
Btrfs backing, at the syscall level, on both systems); current-state read
(OrFS-GO adds its FUSE round trip); physical-vs-logical bytes (retained physical
bytes per logical byte).

**Not comparable (recorded, never faked).**

1. **Historical-state read.** Btrfs serves it from a read-only snapshot; OrFS-GO
   retains every transition but does **not** serve named historical roots in this
   build (see [`../../docs/LIMITATIONS.md`](../../docs/LIMITATIONS.md)), so this
   axis is Btrfs-only. The OrFS-GO current-state read serves the same newest
   committed bytes.
2. **Snapshot-creation cost.** OrFS-GO has no snapshot step — it retains every
   committed transition at append time. The Btrfs snapshot-creation row has no
   OrFS-GO counterpart.
3. **Snapshot extent sharing.** Btrfs snapshots share unchanged extents, so a
   retained snapshot's *exclusive* bytes can be near zero. OrFS-GO's retained
   bytes are its appended log, which never shares extents. Both are measured, but
   they are not the same computation.

## Run it

```sh
sh scripts/container-retention-btrfs.sh
```

The wrapper builds the court image and runs the court in a **privileged** capped
container (root + `/dev/fuse` + loop devices + `mkfs.btrfs`, with the shared
`scripts/lib.sh` caps). A host without docker, `/dev/fuse`, or `/dev/loop-control`
reports `SKIPPED` rather than a false pass; the court itself also reports a
precise `SKIPPED` if `mkfs.btrfs` or the loop mount is unavailable inside the
container. The court is wired into
[`scripts/container-verify.sh`](../../scripts/container-verify.sh) (SKIPPED-aware).

Knobs (all optional, forwarded by the wrapper):

| Env | Default | Meaning |
| --- | --- | --- |
| `SEGMENT_SIZE` | `8388608` | OrFS volume segment size |
| `IMG_MIB` | `512` | loop image size (each arm gets its own) |
| `EPOCHS` | `8` | epochs / retained states |
| `WRITERS` | `1` | concurrent writers |
| `RECORDS_PER_EPOCH` | `256` | records appended per writer per epoch |
| `RECORD_BYTES` | `4096` | append record size |
| `APPEND_SECONDS` | `30` | per-epoch safety time bound |
| `READ_SECONDS` | `1` | duration of each read arm |
| `READERS` | `1` | read concurrency |
| `BLOCK_SIZE` | `4096` | read block size |
| `READ_EPOCHS` | `all` | `all`, or a space list of epochs to read |
| `SEED` | `1` | deterministic seed |
| `ORFS_ARM` | `1` | run the OrFS-GO half (0 = Btrfs only) |

## Honest notes

- **It is a bounded laboratory run, not an endurance test.** One run of the
  matrix; run-to-run variance is expected and is not quantified here. The default
  workload retains 8 states / 8 MiB of logical data (small by design, so it fits
  the 2 GiB / 4 CPU / pids-1024 caps).
- **The read arms are buffered (page-cache-warm).** A snapshot read is measured
  immediately after it is created, so both the current and the historical reads
  hit cache; the axis therefore shows that a retained historical read is *no
  slower* than a current read, not a cold-device number. No cold-cache read and
  no `O_DIRECT` read is measured here (`docs/LIMITATIONS.md`).
- **The append comparison includes an inherent difference.** The OrFS-GO append
  pays the FUSE round trip and the daemon's own framing; the Btrfs append pays
  only the kernel Btrfs write+fsync. That difference is the FUSE tax, recorded and
  not removed.
- **The physical-bytes accounting is measured but not identical.** Btrfs physical
  is the whole device's `Used` (data + CoW metadata); OrFS-GO physical is the
  authoritative `or.log` blocks. See the non-comparability list above.
