# Limitations and Honest Non-Claims

This file is the standing list of what OrFS-GO does **not** claim. It exists so
that absence is explicit, never implied.

## 1. Performance is honest, and mostly a loss

No performance superiority is claimed. The external baseline court now runs
(`docs/BENCH.md` §5) and publishes the FUSE tax: 4K random read p50 ~24x worse
and p99 ~44x worse than the container filesystem, and ~472k IOPS at 64 readers
against ~11.3M native. The only measured *win* is the concealed read planner's
p50 (~2x on a warm manifested body), with p99 explicitly not claimed. Cross-FS
determinism is now measured and holds: identical authoritative log bytes across
ext4/xfs/btrfs/tmpfs (§11). Several
courts remain unrun and are named as such in `docs/BENCH.md` §8 (a multi-day
amplification curve, group commit); the transport court now runs, and F2 is
named unreachable rather than deferred (`docs/TRANSPORT.md`). No number that a
court did not measure is published.

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

What remains: a multi-day amplification curve, profile selection (choosing a body
by measured worth), and the raw pure-Go transport F2 would require. No manifested
superiority beyond the p50 figure is claimed. `docs/REJECTED.md` records the one
deliberately-rejected candidate (the image pixel-surface pyramid).

## 6. F2 (FUSE-over-`io_uring`) is not reachable from this build

The transport today is F0: `/dev/fuse` with synchronous backing I/O, served by
`hanwen/go-fuse/v2` v2.12.0. That library exposes **no** FUSE-over-`io_uring`
transport — only the name of the capability bit (`fuse/types.go:325`,
`fuse/print.go:58`); its transport is a `read(2)`/`writev(2)` loop. The kernel on
this host *does* support F2 (`CONFIG_FUSE_IO_URING=y`, `enable_uring=Y`, and the
negotiation probe shows it advertising `FUSE_OVER_IO_URING`), but it is
unreachable from pure Go through go-fuse, and a from-scratch io_uring transport
would mean reimplementing the FUSE server (out of scope). `orfs mount
--transport fuse-uring` therefore reports the exact reason and falls back to F0,
loudly, never silently. The measured F0 comparison (multi-reader vs single-loop)
is published: `evidence/performance/transport.json` and
[`TRANSPORT.md`](TRANSPORT.md). No transport superiority is claimed.

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
- The mounted court (`scripts/mount-court.sh`) and the external `pjdfstest`
  court (`scripts/container-pjdfstest.sh`) exercise the POSIX surface; the
  conformance deviations they find are published, not allowlisted (§11).

## 11. POSIX conformance is measured, and now clean

Two conformance facts are now measured rather than asserted, and both are
published in full.

**Cross-FS determinism HOLDS.** `scripts/container-crossfs.sh` initialises an OrFS
volume on each of ext4, xfs, btrfs, and tmpfs with a fixed UUID and a pinned
`ORFS_CLOCK_EPOCH`, replays one identical operation sequence against each, and
hashes every authoritative `or.log/*.seg` segment. All four backings produced the
**identical sha256**
`35bd5d57e1f85525238a13f1609d2c65737b37731ec14234634f4f5e8260d990`: the backing
store does not change the bytes authority writes. Receipt:
`evidence/correctness/cross-fs.json` (schema `orfs.correctness.crossfs.v1`).

**The mounted POSIX surface now conforms.** The upstream `pjdfstest` suite
(<https://github.com/pjd/pjdfstest>, built from source) was run against an OrFS
mount: **8798 assertions, all 8798 passed, 0 failed**. Receipt:
`evidence/performance/pjdfstest.json` (schema
`orfs.performance.pjdfstest.v1`). The same suite also passes cleanly on tmpfs.

Closing the deviations required special-file support (`mknod`/`mkfifo`/`bind`:
FIFO, socket, and device inodes with device numbers), the rename sticky-bit and
link-count rules, independent ctime semantics for metadata changes, the chmod
`S_ISGID` clear for a non-group caller, and `default_permissions` so the kernel
enforces DAC on special files whose `open(2)` never reaches the daemon.
