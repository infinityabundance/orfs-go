# OrFS-GO

**Non-withdrawal, concealed-expansion FUSE filesystem** — a native, pure-Go
Linux filesystem with permanent authoritative history, byte-compatible with the
Rust `OrFS` reference, and no CGO.

Governing objective: **spend manifested space to maximise speed.**

OrFS is two laws made executable:

- **Law I — Non-withdrawal.** Committed authoritative history is never
  overwritten, truncated, reused, or garbage-collected. Every committed
  transition extends the previous history: `L_{n+1} = L_n ⊕ Δ_n`.
- **Law II — Concealed expansion.** Additional operational structures are
  physically manifested behind the ordinary logical namespace, deliberately
  spending storage, memory, and construction work to accelerate reads.

`or.log` is the source of truth. FUSE is the Linux interface. The EKBLOOM
concealed-expansion codec is consumed as an independent, pure-Go dependency.

## Status

This is a young port built in ordered phases (see `docs/PROGRESS.md`). The CLI
is honest: a command that is part of the design but not yet wired rejects the
request with its owning phase rather than pretending to work.

Working today:

| Area | State |
| --- | --- |
| Record envelope, registry, all typed payloads (byte-exact) | implemented |
| `or.log` append-only log; transactions; bracket verification | implemented |
| Genesis identity with `format_version` (`v0` legacy / `v1`) | implemented |
| Segments, contiguity locator, discovery | implemented |
| Recovery: newest valid committed generation; quarantine (never truncate) | implemented |
| Commit watermark (trailing-segment-loss detector) | implemented |
| Namespace: inodes, dentries, extents, replay | implemented |
| POSIX ops: create/mkdir/write/read/unlink/rmdir/rename/link/symlink/xattr/setattr | implemented |
| Permissions: class rule + supplementary groups; sticky bit; EPERM on dir hard-link | implemented |
| Capacity guard: `doctor --capacity`, typed `CapacityExhausted` | implemented |
| FUSE mount (`hanwen/go-fuse`); recovery-verified round trips | implemented |
| Concealed expansion: live EKBLOOM manifested read path with exact-identity certification and honest fallback | implemented |
| Sealed dense-ordinal inode spine for the historical role | implemented |
| Simulated power-loss device + crash-consistency court | implemented |
| Frozen `legacy_v0` / `current_v1` fixtures + byte-identity court | implemented |
| `orfs init` / `inspect` / `verify [--deep]` / `recover` / `doctor` / `mount` / `manifest` / `stat` | implemented |
| Growth/amplification, external `fio` baseline, 64-thread scaling, transport, fsstress, capacity/quarantine, scan-resistant admission courts | implemented — receipts under `evidence/`, honest numbers in `docs/BENCH.md` |
| Cross-FS determinism (identical `or.log` bytes across ext4/xfs/btrfs/tmpfs) | implemented — `evidence/correctness/cross-fs.json` |
| pjdfstest court | implemented — **8798/8798 passing** (`docs/LIMITATIONS.md` §11) |
| Transport (`--transport f0 \| f0-single \| fuse-uring`) with a loud F2 fallback; F2 (`io_uring`) probed live — the kernel supports it, go-fuse exposes none (see `docs/TRANSPORT.md`) | implemented — `evidence/performance/transport.json` |
| Operator metrics (`orfs metrics`, Prometheus text + `--json`) and an append-only WORM case study | implemented — `evidence/worm-case-study.json`, `docs/WORM.md` |

Honest non-claims: the **image pixel-surface pyramid is rejected, not merely
unimplemented** (`docs/REJECTED.md`). No performance superiority is claimed; the
FUSE tax is published in `docs/BENCH.md`. A true pure-Go F2 transport, group
commit, and a per-read allocation court remain planned (`docs/PROGRESS.md`).

## Quick start

```sh
go build -o orfs ./cmd/orfs

./orfs init /var/lib/orfs/demo --segment-size 64MiB
./orfs mount /var/lib/orfs/demo /mnt/orfs &
# ... ordinary POSIX operations under /mnt/orfs ...
umount /mnt/orfs

./orfs verify --deep /var/lib/orfs/demo
./orfs doctor --capacity --backing /var/lib/orfs/demo
./orfs metrics /var/lib/orfs/demo              # Prometheus text exposition
```

## Documentation

See [`docs/README.md`](docs/README.md) for the full map. Benchmarks and their
receipts are in [`docs/BENCH.md`](docs/BENCH.md); reproduction in
[`docs/REPRO.md`](docs/REPRO.md). Highlights:
[`ORFS-SPEC.md`](docs/ORFS-SPEC.md) (the laws and planes),
[`ON_DISK.md`](docs/ON_DISK.md) (byte-exact format),
[`MIGRATION.md`](docs/MIGRATION.md) (`or.log` v0 → v1),
[`MANIFESTATION.md`](docs/MANIFESTATION.md) (the concealed read path),
[`TRANSPORT.md`](docs/TRANSPORT.md) (F0/F1/F2, the measured F0 comparison, and why F2 is not reachable here),
[`OPERATIONS.md`](docs/OPERATIONS.md) (growth, recovery, capacity),
[`WORM.md`](docs/WORM.md) (the append-only WORM case study and the metrics surface),
[`SECURITY.md`](docs/SECURITY.md), [`LIMITATIONS.md`](docs/LIMITATIONS.md),
[`REJECTED.md`](docs/REJECTED.md), [`PROGRESS.md`](docs/PROGRESS.md).

## Development is container-only

All compilation, tests, fuzzers, and mount experiments run inside a
digest-pinned, resource-capped container. Nothing is built directly on the host.

```sh
sh scripts/container-build.sh     # build
sh scripts/container-test.sh      # vet + default suite
sh scripts/container-check.sh     # gofmt + vet
sh scripts/container-fuse-test.sh # the mounted POSIX court (root + /dev/fuse)
sh scripts/container-verify.sh    # the pre-PR gate, with a PASS/FAIL/SKIPPED summary
sh scripts/container-verify.sh --fast
```

Container FUSE still depends on the host kernel; this workflow does **not**
provide kernel isolation.

## Layout

```
cmd/orfs          the CLI
internal/record   the record envelope, registry, and typed payloads
internal/segment  segment files, naming, contiguity locator
internal/log      the authoritative log: scan, transactions, recovery, publish
internal/wm       the commit watermark
internal/volume   volume.meta, genesis identity, init/open
internal/index    inode/dentry/extent model and log replay
internal/namespace  POSIX operations expressed as transactions
internal/capacity   the capacity guard
internal/fsck     read-only verification
internal/doctor   the capability probe and capacity report
internal/metrics  the dependency-free Prometheus exposition primitives
internal/observe  collects the metrics from a volume
internal/fusefs   the FUSE projection
internal/transport the F0/F1/F2 transport story: probe, negotiate, serve mode
internal/fixture  the frozen mini-volumes for the format court
docs/             normative specifications
tests/fixtures/   frozen compatibility fixtures
scripts/          pinned-container execution
```

## License

MIT OR Apache-2.0.
