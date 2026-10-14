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

### 1.1 The append path: the O(N²) write cost is fixed; extent-count growth remains

An OrFS append is a new `DATA_EXTENT` whose bytes live at a *new* log offset, so
a file's logical extents cannot coalesce and its extent count grows **1:1 with
its append count** — a property of non-withdrawal, shared with the Rust
reference. The head-to-head court's first run exposed the consequence: the extent
map was a flat slice, so `Clone` (once per inode clone on every commit) and the
write splice were both `O(E)`, making each append `O(N)` and an append run
**`O(N²)`** — 3,097 ops/s single-writer, 8.8× behind the reference.

That is now fixed. `internal/index.ExtentMap` is a persistent, path-copying
**treap** (a faithful port of the reference's Phase 27 `PMap`: clone is `O(1)`,
a write is `O(log E + k)`, a read visits only intersecting extents and allocates
nothing). Per-append cost is measured **flat** from 256 to 8,192 appends
(`evidence/performance/append-scaling.json`, growth ratio 1.001×;
`docs/BENCH.md` §7i), and the court now measures **18,533 ops/s** single-writer —
within 1.5× of the reference instead of 8.8× behind it.

What nevertheless remains, and is **not** claimed: `E` still grows 1:1 with
appends, so an append-only file's extent list — and therefore its index
checkpoint snapshot — grows with its retained history. A whole-index snapshot
payload is bounded by the record payload limit (64 MiB), so roughly **2.6 million
cumulative file extents** is the point at which `TakeCheckpoint` refuses
(`NotSupported`); past that the checkpoint simply cannot be written, and the
volume falls back to full replay (commits themselves are unaffected — the
checkpoint is best effort). No extent-list compaction or run coalescing is
implemented, and the extent list is not compressed on disk. Neither is a number a
court has measured for time-to-refusal.

## 2. Mount replay is bounded by the checkpoint interval; the open scan is not

Recovery reconstructs the index by **replaying** the committed log. A usable
index checkpoint now bounds the *replay* to the suffix after it, and the derived
committed-root directory removes the checkpoint *discovery* scan, but the
authoritative open (`log.Open`) still scans and validates the whole log to
establish the committed root — so a mount is **not** constant-time. The measured
curve (`evidence/recovery/checkpoints.json`, `docs/OPERATIONS.md` §3) shows, for
a **write-heavy** log (large history, small namespace), full replay growing
`Θ(committed bytes)` (7 µs → 621 µs from 2^10 to 2^20) while the directory-assisted
index reconstruction is **flat** (10 µs → 10 µs) and the replayed suffix is a
constant 852 bytes. For a **namespace-heavy** log the index is itself
`Θ(history)`, so the checkpoint bounds the *replay* but not the index decode
(3.6 ms full vs 1.5 ms checkpoint vs 0.35 ms directory at 2^20). On a trivially
small log the checkpoint costs slightly more than it saves (as the reference also
reports). **No mount-time bound is claimed beyond these measurements** — in
particular the `log.Open` scan is unbounded here. A checkpoint only ever lets
recovery skip replay work; it never changes which root is committed.

## 3. Power-loss durability is simulated only

Crash consistency is exercised two ways. A **real-process crash court** kills the
`cmd/orfscrashtest` writer with `SIGKILL` at a random point mid-append (no signal
handler, no cleanup), then reopens the volume and proves recovery reaches the
newest valid committed boundary with the abandoned tail quarantined (moved, never
truncated), the committed prefix byte-identical, and `verify --deep` clean
(`docs/BENCH.md` §7f; receipt `evidence/crash.json`). Separately, a **simulated
volatile device-cache model** (`internal/powerloss`: barrier / partial / torn-tail
flushes) proves recovery reaches a complete committed boundary or refuses —
never exposing uncommitted data. **No real-hardware durability is claimed:** the
process-kill court models a crash, not a power cut (bytes in the page cache
survive a `kill -9`), the model proves safety relative to the modelled device, and
no profile asserts power-loss durability. Only an instrumented power-loss rig can
test real hardware.

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
- The `INDEX_CHECKPOINT` record now bounds replay: the newest valid checkpoint is
  loaded and only the suffix after it is replayed, with an explicit corrupt
  fallback policy and a derived committed-root directory for discovery. Named
  *historical* roots (`SNAPSHOT_ROOT` indexing, `orfs snapshot`) are still not
  materialised: `SNAPSHOT_ROOT` is decoded into an alias table but a historical
  view is not yet served from it.
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
