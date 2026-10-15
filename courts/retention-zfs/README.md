# OpenZFS retained-snapshot retention court

A **court** in this repository is an executable test or script that produces a
machine-readable *receipt* and reports honestly. This court answers, for a fourth
system, the same **retention** question the Btrfs court
([`../retention-btrfs/README.md`](../retention-btrfs/README.md)) answers:

> Can a system retain every committed historical transition while achieving
> comparable tail latency, throughput and physical-space economics?

OrFS retains history in its **authoritative log** (non-withdrawal, Law I): every
committed transition is kept, with no extra step. OpenZFS retains it with a
**read-only snapshot** (`zfs snapshot`) taken after every epoch. The court drives
both with **one shared, deterministic, bounded epoch workload** on the same
machine, the same container caps, and same-sized backings.

## Layout

| Path | What it is |
| --- | --- |
| [`../../cmd/retentionbench`](../../cmd/retentionbench) | The shared workload driver (`prepare`/`append`/`read`/`space`/`snapshot`), extended with an OpenZFS target. |
| [`../../cmd/retentionsummary`](../../cmd/retentionsummary) | Merges the per-arm receipts into the court receipt (`-schema`/`-comparator`). |
| [`../../internal/retention`](../../internal/retention) | The receipt vocabulary + the merge rules, and the OpenZFS space/snapshot accounting. |
| [`../../scripts/retention-zfs-court.sh`](../../scripts/retention-zfs-court.sh) | The court entry point — runs *inside* the container as root. |
| [`../../scripts/container-retention-zfs.sh`](../../scripts/container-retention-zfs.sh) | The host wrapper (privileged court container: file-backed `zpool` vdev + loop devices). |
| [`../../evidence/retention-zfs.json`](../../evidence/retention-zfs.json) | The receipt (schema `orfs.retention.zfs.v1`). **SKIPPED** on this host. |

## The workload

`cmd/retentionbench` drives a **mounted directory**, one arm at a time and one
system at a time, with the same durable-append shape as the Btrfs court:

- **append** — every writer owns its own precreated file and appends fixed 4 KiB
  records with `write(2)`+`fsync(2)` per record: **one durability barrier per
  record**, equal on both systems. It appends a **defined count** of records per
  epoch, so the committed state after each epoch is well defined and is exactly
  what is retained. Latency is exact observed samples, so a p99.9 is a real
  sample.
- **read** — block-aligned uniform-random `pread` over the current state or a
  retained snapshot (a dataset's `.zfs/snapshot/<name>` path), reporting
  IOPS/bytes/s and `p50/p95/p99/p99.9`.
- **space** — physical vs logical bytes of one state: `zfs list -Hp -o
  used,referenced,logicalused,logicalreferenced` for OpenZFS (a snapshot's `used`
  is the space freed by destroying it, its uniquely-retained bytes), or the disk
  blocks of the authoritative `or.log` for OrFS-GO.
- **snapshot** — a timed `zfs snapshot <dataset>@<name>`, the cost of creating one
  retained historical state.

Design guarantees, as in the Btrfs court: **deterministic**, **bounded** (a fixed
record count per epoch and a time safety valve), **exact latency**, and
**errno-classified** errors. The driver's append/read arms are unchanged; only the
space and snapshot arms gained a `zfs` kind, and the Btrfs court's behaviour is
unchanged (`internal/retention` tests pin it).

## Feasibility first, and this host says SKIPPED

OpenZFS is an **out-of-tree kernel module**: `zpool`/`zfs` cannot create, import,
or even query a pool without it, and loading a kernel module is a **host-level
change** that is out of scope for this repository (`docs/LIMITATIONS.md`).
`scripts/retention-zfs-court.sh` therefore probes for a reachable OpenZFS *before*
building anything, records the exact commands it ran and their verbatim output in
the receipt, emits a single precise `SKIPPED` line, and exits 0.

On the host this repository was developed on, that probe fails:

- **no `/dev/zfs`** (`ls: cannot access '/dev/zfs': No such file or directory`);
- **no zfs module**: `grep -w zfs /proc/modules` is empty, `grep -w zfs
  /proc/filesystems` is empty, and `modprobe -n zfs` (a *query* only) reports
  `FATAL: Module zfs not found in directory /lib/modules/7.2.9-2-cachyos`;
- the court image *ships* the `zpool`/`zfs` userspace tools (`zfsutils-linux`), but
  a file-backed `zpool create` fails verbatim with `The ZFS modules are not
  loaded. Try running '/sbin/modprobe zfs' as root to load them.`

So the receipt is a **SKIPPED finding, not a measurement**: no number is
published, because none was measured. A precise SKIPPED that says exactly what is
missing is the honest deliverable here — the alternative (loading the module on
the host, or substituting a different ZFS) is out of scope.

## What is measured, and what is not comparable

When OpenZFS *is* reachable, the court names the axes explicitly in its
`comparability` block (identical to the Btrfs court):

**Comparable.** Current-state append; current-state read (OrFS-GO adds its FUSE
round trip); physical-vs-logical bytes (retained physical bytes per logical byte).

**Not comparable (recorded, never faked).**

1. **Historical-state read.** OpenZFS serves it from a snapshot; OrFS-GO does
   **not** serve named historical roots in this build, so this axis is
   OpenZFS-only.
2. **Snapshot-creation cost.** OrFS-GO has no snapshot step — it retains every
   committed transition at append time.
3. **Snapshot extent sharing.** OpenZFS snapshots share unchanged blocks
   copy-on-write, so a retained snapshot's `used` can be near zero; OrFS-GO's
   retained bytes never share.
4. **The read comparison is native-vs-FUSE, not retention.** The FUSE-tax gap is
   recorded as a difference, not a ranking.

## Run it

```sh
sh scripts/container-retention-zfs.sh
```

The wrapper builds the court image and runs the court in a **privileged** capped
container (root + file-backed `zpool` vdev + `/dev/fuse`; the host's `/dev/zfs` is
bind-mounted read-only *when it exists*). A host without docker, `/dev/fuse`, or
`/dev/loop-control` reports `SKIPPED`; so does the court itself, with a
reproducible feasibility receipt, when OpenZFS is not reachable. The court is
wired into [`scripts/container-verify.sh`](../../scripts/container-verify.sh)
(SKIPPED-aware).

Knobs (all optional, forwarded by the wrapper):

| Env | Default | Meaning |
| --- | --- | --- |
| `SEGMENT_SIZE` | `8388608` | OrFS volume segment size |
| `IMG_MIB` | `512` | vdev / loop image size (each arm gets its own) |
| `POOL` | `orfsret` | zpool name |
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
| `ORFS_ARM` | `1` | run the OrFS-GO half (0 = OpenZFS only) |

## Honest notes

- **The receipt is a SKIPPED finding on this host.** It records the feasibility
  evidence (commands + verbatim output) so the finding is reproducible; the
  `arms` block reports the would-be arm counts with `ran: 0`, `failed: 0`, and
  `skipped: true`, and in a measured run a lost arm **FAILs** the court, exactly
  as in the Btrfs court.
- **It is a bounded laboratory run, not an endurance test** (when it runs): one
  run of the matrix, run-to-run variance expected and not quantified.
- **The append comparison includes the FUSE tax** (when it runs): the OrFS-GO
  append pays the FUSE round trip and the daemon's framing; the OpenZFS append
  pays only the kernel write+fsync.
- **The physical-bytes accounting is measured but not identical** (when it runs):
  OpenZFS physical is `zfs list -o used`; OrFS-GO physical is the authoritative
  `or.log` blocks.
