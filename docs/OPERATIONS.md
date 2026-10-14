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

Mounting (and any open) replays the committed log: recovery is
**`Θ(retained history)`**, not constant-time. The design commits to this; a
usable index checkpoint bounds the *replay* to a suffix but discovery still
scans the log, so a checkpoint is not always faster.

The measurement of mount latency after a large (2^30-byte) history is a tracked
follow-up; the exact court is:

```sh
sh scripts/container-verify.sh        # after adding the history-scaling court
# drive N epochs, then time `orfs inspect` and a `mount` + first read
```

Until that court runs, no latency number is published — see
`docs/LIMITATIONS.md`.

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
