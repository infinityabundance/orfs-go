# Limitations and Honest Non-Claims

This file is the standing list of what OrFS-GO does **not** claim. It exists so
that absence is explicit, never implied.

## 1. Performance is honest, and mostly a loss

No performance superiority is claimed. The external baseline court now runs
(`docs/BENCH.md` §5) and publishes the FUSE tax: 4K random read p50 ~24x worse
and p99 ~44x worse than the container filesystem, and ~472k IOPS at 64 readers
against ~11.3M native. The only measured *win* is the concealed read planner's
p50 (~2x on a warm manifested body), with p99 explicitly not claimed. Several
courts remain unrun and are named as such in `docs/BENCH.md` §8 (F2, cross-FS
determinism, a multi-day amplification curve, group commit, S3-FIFO admission).
No number that a court did not measure is published.

## 2. Mount latency is `Θ(retained history)`

Recovery replays the committed log; it is not constant-time. A usable index
checkpoint bounds the *replay* to a suffix, and a committed-root directory can
skip the whole-log *discovery* scan, but neither is implemented in this build,
so a mount after a large (2^30-byte) history is expected to be slow. The
history-scaling court and its numbers are pending (`docs/OPERATIONS.md` §3).

## 3. Power-loss durability is simulated only

Crash consistency is exercised by a process-crash court (kill mid-write, reopen
independently), by the recovery logic itself, and by a simulated volatile
device-cache model (`internal/powerloss`: barrier / partial / torn-tail flushes)
whose court proves recovery reaches a complete committed boundary or refuses —
never exposing uncommitted data. **No real-hardware durability is claimed:**
the model proves safety relative to the modelled device, and no profile asserts
power-loss durability. Only an instrumented power-loss rig can test real
hardware.

## 4. Trailing-segment loss is detected, with a residual window

The commit watermark detects a trailing committed segment that is missing or
truncated (the recovered root is short of the watermark). A crash *between*
committing bytes and persisting the watermark leaves the watermark one point
behind, so loss of exactly those not-yet-watermarked bytes is undetectable by
construction. Interior gaps, sequence breaks, and roots past the surviving bytes
are refused.

## 5. The manifestation frontier is wired, with honest numbers

The EKBLOOM integration is a **live** read path. `internal/manifest.Cache`
certifies a manifested body against the exact `(inode, inode version, committed
root)`, verifies its self-digest, and — when an authoritative oracle is installed
— binds it to the bytes authority holds now. `namespace.Read` consults the seam
and falls back to authority on any miss, stale identity, or failed certification;
a manifested fault can never change the answer or touch authority. The mounted
FUSE path installs the seam. `orfs manifest <backing> --inode <id>` publishes a
body.

Measured on this host (unoptimised test build, warm cache), for a 1 MiB file and
4 KiB random reads: the manifested **p50 was ~2× better** than authoritative
(~240 ns vs ~480 ns). **p99 was within noise** (sometimes better, sometimes
worse) and is **not** claimed as an improvement. The default root-profile body
transforms ~1.00× the logical bytes (it is essentially a full history); profiles
that spend more space for more speed are the follow-up.

What remains: an external baseline court, a multi-day amplification curve,
profile selection (choosing a body by measured worth), and F2. No manifested
superiority beyond the p50 figure is claimed. `docs/REJECTED.md` records the one
deliberately-rejected candidate (the image pixel-surface pyramid).

## 6. F2 (FUSE-over-`io_uring`) is not wired

The transport today is F0: `/dev/fuse` with synchronous backing I/O. The
queue-affine F2 transport, its SQ/CQ-pressure counters, and the honest
F0-vs-F2 p50/p95/p99/p99.9 publication are a tracked follow-up. No transport
superiority is claimed.

## 7. Logical deletion is not secure erasure

`unlink`/`rmdir`/truncate/rename change only the visible namespace; the bytes
remain forever (Law I). OrFS is inappropriate for workflows requiring reliable
physical deletion unless a separately specified retention and key-destruction
policy is applied on top.

## 8. Security: integrity, not authentication

The on-disk checks are CRC32C framing plus BLAKE3 content digests. There is no
MAC, signature, or encryption. An adversary with backing-store write access can
rewrite history. Non-withdrawal is a design law, not a cryptographic guarantee.
See `docs/SECURITY.md`.

## 9. Container is not kernel isolation

FUSE runs against the host kernel. The container workflow pins the toolchain and
caps resources; it does not isolate the kernel. Host kernel behaviour (FUSE ABI,
mount privileges) still applies.

## 10. Incomplete surface

- No kernel-served FUSE passthrough and no named historical snapshots (`orfs
  snapshot` reports its owning phase).
- No concealed global search directory (`orfs query` reports its owning phase).
- Rename does not yet honour `RENAME_NOREPLACE`/`RENAME_EXCHANGE` flags from the
  kernel path (an explicit exchange is available in the namespace API).
- The `INDEX_CHECKPOINT` record is decoded and replayed into a checkpoint list
  but is not yet used to bound replay.
- The mounted court (`scripts/mount-court.sh`) exercises the POSIX core, but the
  external `pjdfstest` suite and an `fsx`/`fsstress`-equivalent fault-injection
  court are not yet wired into `container-verify.sh`.
