# Operations Runbook

**Scope.** A single OrFS-GO volume, mounted or inspected with the `orfs` binary
inside a pinned container. The backing directory (`volume.meta`, `or.log/`) is
the source of truth; **`or.log` is authoritative, everything else is derived**.

## 1. Growth monitoring: logical vs physical

The authoritative `or.log/` **only ever grows** (Law I): committed history is
never truncated, reclaimed, or rewritten, so `physical_bytes` rises
monotonically. Capacity planning is a matter of the *rate*
(`bytes_per_transaction`) and the retained size, never of reclamation.

`doctor --capacity` is the report to watch:

```sh
orfs doctor --capacity --backing <dir>          # human-readable
orfs doctor --capacity --backing <dir> --json   # schema orfs.capacity.report.v1
```

| Field | Meaning |
| --- | --- |
| `physical_bytes` | retained authoritative bytes: committed `or.log/*.seg` + `commit.watermark` (excludes derived `.or_bloom/`) |
| `segment_count` | number of log segments |
| `authoritative_bytes` | the committed root `R` (end offset) |
| `free_bytes` | live backing free space (`f_bavail × frsize`) |
| `mount_point` / `device` / `filesystem` | backing mount identity |
| `reserve_bytes` / `headroom_bytes` | the reserve and the free space above it |
| `transaction_count` | committed transactions (excluding genesis) |
| `bytes_per_transaction` | a sampled average over the last 64 committed boundaries |
| `estimated_transactions_remaining` | `headroom / bytes_per_transaction` |
| `verdict` | `ok` / `warn` / `exhausted` |
| `degraded` / `degraded_reason` | the read-only latch state |
| `manifested_bytes` / `logical_bytes` / `amplification_ratio` | the bytes surface from `internal/observe` (the ratio is committed root / logical file bytes) |
| `breach_total` / `quarantine_segments` / `quarantine_bytes` | capacity refusals and retained abandoned tails |
| `fsync_p50_seconds` / `fsync_p99_seconds` / `group_commit_batch_last` / `group_commit_batch_avg` | the runtime counters, from the live process or the persisted snapshot |

Verdict rules:

| Verdict | Condition | Action |
| --- | --- | --- |
| `ok` | headroom ≥ reserve **and** ≥ one transaction | none |
| `warn` | `0 < headroom < reserve`, or headroom < one transaction | plan to grow/relocate |
| `exhausted` | `headroom == 0` (`free ≤ reserve`) | commits are being refused now |

An unreadable backing is reported `warn`, never silently `ok`.

**Remedies are only free space or a larger/other device.** Never trim `or.log/`.
Deleting the derived `.or_bloom/` directory is safe (it is rebuildable and
changes nothing recoverable), but it does not reclaim authoritative history.
`orfs stat <backing>` reports the physical category breakdown; a full
manifested-byte accounting and amplification curve over a multi-day run is the
follow-up (`docs/LIMITATIONS.md` §5).

### 1a. Concealed expansion (Plane C)

A manifested body accelerates reads and is **not exempt from physical
accounting**: it consumes backing space behind the namespace. Publish one for an
inode:

```sh
orfs manifest <backing> --inode <id>          # publish a manifested body
orfs manifest <backing> --inode <id> --json   # the manifest, as JSON
```

A mounted volume installs the seam automatically: any read of a manifested inode
is served from the body, or from authority when the identity is stale or
certification fails. See `docs/MANIFESTATION.md`. The manifested plane is
derived — a failure in it never fails an authoritative commit.

### 1b. Operator metrics (`orfs metrics`)

The full observability surface is a dependency-free Prometheus text exposition:

```sh
orfs metrics <backing>            # Prometheus text (version 0.0.4) to stdout
orfs metrics <backing> --json     # schema orfs.metrics.v1
orfs metrics <backing> --skip-verify
```

It is read-only. It runs a `verify --deep` pass to source
`orfs_verify_deep_failures`. On-disk families (physical / manifested / logical
bytes, amplification, quarantine, degraded) are recomputed on every call;
**process-scoped** families (capacity refusals, the barrier histogram, batch
sizes) come from the `metrics.json` snapshot the mount daemon writes beside
`volume.meta` when it shuts down, or, when no snapshot exists, are emitted as an
explicit **0** whose `# HELP` says so. There is no HTTP endpoint: feed it to a
Prometheus textfile collector. See [`WORM.md`](WORM.md) for the metric table, the
append-only WORM case study, and the Grafana dashboard.

## 2. Recovery runbook

```sh
orfs recover <backing>           # report the committed boundary; quarantine an abandoned tail
orfs verify --deep <backing>     # validate and classify; read-only, repairs nothing
orfs inspect <backing>           # identity, segments, root, accounting
```

- **When to run `recover`:** after any unclean event (crash/kill, power loss,
  container OOM-kill mid-write, interrupted mount) and whenever `verify --deep`
  surfaces a non-empty tail.
- **Repaired:** a torn or uncommitted tail beyond the last `TX_COMMIT`,
  classified and moved aside into `or.log/quarantine/` (**never truncating it**);
  committed bytes untouched.
- **Refused (never silent):** an incomplete/inconsistent commit bracket; a
  malformed *committed* record; a torn record in a **non-final** segment; a
  physical gap or duplicate segment; a committed root extending past the bytes
  that exist; an unsupported format version.
- **Read-only forensics:** `verify` and `inspect` modify not a single byte. The
  committed prefix `[0, R)` is byte-identical before and after recovery.
- **One honest residual:** a tail that begins *inside* the committed segment is
  retained in place (not truncated). After recovery continues in a fresh
  segment, a later fresh open refuses it as a torn record in a non-final
  segment. No committed byte is lost or rewritten.

Do not hand-edit or delete anything under `or.log/`; that violates non-withdrawal.

## 3. Mount latency after a long history

Mounting (and any open) reconstructs the index from the committed log. An
**index checkpoint** bounds the *replay* to the suffix after it, and the derived
committed-root directory (`index/`) removes the checkpoint *discovery* scan; the
authoritative `log.Open` scan still covers the whole log in every case, so a
mount is **not** constant-time. See `docs/SNAPSHOTS.md` for the design and
`docs/LIMITATIONS.md` §2 for the honest residual.

**Take a checkpoint** (opt-in; off by default) so a later open can skip replay:

```sh
orfs checkpoint <backing>                      # take one now
ORFS_CHECKPOINT_EVERY=10000 orfs mount ...     # then every 10k committed txns
```

**The measured startup curve.** Court `internal/orbench`
(`TestCheckpointRecoveryCourt`), receipt `evidence/recovery/checkpoints.json`
(schema `orfs.recovery.checkpoints.v1`). Each size is `2^k` committed bytes;
each arm is best-of-10 in the pinned container. Go `HeapInuse`, not RSS. Absolute
microseconds vary run-to-run with host load; the shape is the stable result.

*Overwrite* (log grows; index constant — the milestone's target shape):

| committed root | txns | full replay | ckpt + suffix | directory | open (full) | open (ckpt) |
| --: | --: | --: | --: | --: | --: | --: |
| 6 KiB | 1 | 7 µs | 9 µs | 10 µs | 37 µs | 39 µs |
| 71 KiB | 15 | 44 µs | 19 µs | 10 µs | 238 µs | 172 µs |
| 1.0 MiB | 230 | 621 µs | 158 µs | 10 µs | 3 041 µs | 2 149 µs |

*Namespace* (index grows with the log):

| committed root | txns | full replay | ckpt + suffix | directory | open (full) | open (ckpt) |
| --: | --: | --: | --: | --: | --: | --: |
| 6 KiB | 8 | 17 µs | 15 µs | 9 µs | 61 µs | 54 µs |
| 77 KiB | 106 | 215 µs | 105 µs | 29 µs | 547 µs | 395 µs |
| 1.2 MiB | 1 690 | 3 586 µs | 1 544 µs | 352 µs | 8 238 µs | 5 979 µs |

Run the court:

```sh
sh scripts/run.sh env ORFS_EVIDENCE=1 \
  go test ./internal/orbench/ -run TestCheckpointRecoveryCourt -count=1 -v
```

Observations an operator should carry:

- On a **write-heavy** log (large history, small namespace) the directory-assisted
  reconstruction is **flat** (10–11 µs from 2^10 to 2^20) while full replay grows
  `Θ(committed bytes)` (7 → 621 µs); the replayed suffix is a constant 852 bytes.
  Checkpoint more often when the index is large and history grows fast; the
  **residual is the `log.Open` scan**, which is unchanged and still `Θ(log)`.
- On a **namespace-heavy** log the index is itself `Θ(history)`, so both arms
  grow; a checkpoint replaces per-record frame-decode-and-apply with one snapshot
  decode (≈2.3×) and the directory removes discovery (≈10× at 2^20).
- On a trivially small log the checkpoint's decode/validate can cost more than
  replaying it; the checkpoint is an accelerator for a log that dwarfs the index.
- The directory-assisted open is the fastest path; it is **derived**, so deleting
  `index/` or a corrupt/over-claiming directory only costs a fallback to the
  authoritative scan — never a wrong result. `orfs inspect` prints the
  `index source`, the checkpoint root, and the bytes/records replayed.

## 4. Capacity exhaustion / read-only transition

1. **Pre-flight refusal.** A transaction's whole framed size is admitted before
   the first byte; otherwise `CapacityExhausted` (`ENOSPC`) is returned, the
   transaction is not acknowledged, **no byte is written**, and the previous
   root is untouched. The guard does not latch while positive headroom remains,
   so a smaller transaction that still clears the reserve keeps working.
2. **Read-only latch.** Only when the reserve floor is breached (zero headroom)
   or on a device error does the volume latch read-only: the triggering
   mutation returns `ENOSPC`/`EIO`, further mutations return `ReadOnly`
   (`EROFS`), and reads keep working. The latch is in-memory and per handle;
   reopening re-evaluates capacity.

## 5. Conventions

- **Container-only.** All compilation, tests, fuzzers, and mounts run inside a
  digest-pinned, resource-capped container (`scripts/`). One container at a
  time. Container FUSE still depends on the host kernel; this is not kernel
  isolation.
- **Directory hard-links** return `EPERM`; **supplementary groups** are read from
  `/proc/<pid>/status` (unreadable `/proc` over-denies, never over-grants).
- **Security:** the mount is the boundary; on-disk checks are integrity, not
  authentication. Protect the backing directory more strictly than the mount.
  See `docs/SECURITY.md`.
