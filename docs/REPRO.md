# Reproducing the OrFS-GO Courts

**Status:** every court in [`BENCH.md`](BENCH.md) is one script, one command, one
receipt. Nothing runs on the host.

## 1. The container-only rule

All compilation, tests, fuzzers, and mount experiments run inside a
digest-pinned, resource-capped container. `scripts/lib.sh` fixes the image
(`golang:1.26-bookworm`), the caps (default 4 GiB memory, 8 CPUs), and the caches
(`.cache/`, bind-mounted and reused across runs). One container runs at a time.

Nothing is built or mounted directly on the host, and a host that cannot supply
what a court needs reports `SKIPPED`, never a false pass:

- no usable docker daemon -> container courts `SKIPPED`;
- no `/dev/fuse` -> mounted courts `SKIPPED`.

Container FUSE still runs against the host kernel; this is not kernel isolation.

## 2. The court image

The baseline and fsstress courts run inside the Dockerfile's `court` target,
built by `build_court_image()` (`scripts/lib.sh`) as `orfs-go-court:local`
(override with `ORFS_COURT_IMAGE`):

```sh
docker build --platform linux/amd64 --target court -t orfs-go-court:local .
```

It bundles the Go toolchain, `fio`, `fuse3`, `psmisc`, `util-linux`, and
`coreutils`, and builds three tools:

| Tool | Purpose |
| --- | --- |
| `/out/orfs` | the CLI (the mounted courts) |
| `/out/fiosummary` | turns `fio` JSON into a compact, labelled receipt |
| `/out/orfsstress` | the bounded native operation storm |

The image is run as root with the repository's `evidence/` bind-mounted at
`/evidence`, so receipts land under the repository's `evidence/`.

## 3. Go courts write receipts only under `ORFS_EVIDENCE=1`

The in-Go courts (`internal/orbench`, `internal/log`) write their receipts only
when `ORFS_EVIDENCE=1`:

```sh
sh scripts/container-go-courts.sh
```

That sets `ORFS_EVIDENCE=1` and runs
`go test ./internal/orbench/ ./internal/log/ -run "TestAmplification|TestCapacity|TestQuarantine"`,
writing `evidence/performance/amplification.json` and
`evidence/recovery/{capacity,quarantine}.json`. It also runs the crash-consistency
court (`internal/log/TestCrashConsistencyCourt`, `ORFS_CRASH_ITERS=1000`), writing
`evidence/crash.json`.

An ordinary `go test ./...` (and therefore `scripts/container-test.sh` and
`scripts/container-verify.sh`) leaves tracked evidence untouched.

## 4. Mounted courts need `/dev/fuse` and `CAP_SYS_ADMIN`

The mounted courts (`scripts/mount-court.sh`, `scripts/baseline-court.sh`,
`scripts/fsstress-court.sh`) run as root with:

- `--device /dev/fuse`;
- `--cap-add SYS_ADMIN`;
- `--security-opt apparmor=unconfined --security-opt seccomp=unconfined`.

This is container-only: mounting requires `CAP_SYS_ADMIN` and `/dev/fuse`
(`SECURITY.md` §3). `scripts/container-baseline.sh` and
`scripts/container-fsstress.sh` build the court image and run it for you; the Go
courts do not need `/dev/fuse`.

## 5. Court -> command -> receipt

| Court | Command | Receipt |
| --- | --- | --- |
| growth / amplification (`internal/orbench`) | `sh scripts/container-go-courts.sh` | `evidence/performance/amplification.json` |
| planner correctness + latency (`internal/manifest`) | `sh scripts/container-test.sh` | none (test log) |
| inode spine size (`internal/inodex`) | `sh scripts/container-test.sh` | none (test log) |
| micro-benchmarks | `sh scripts/container-bench.sh` | `evidence/performance/bench.txt` |
| external baseline (single job) | `sh scripts/container-baseline.sh` | `evidence/performance/baseline.json` |
| 64-thread scaling | `sh scripts/container-baseline.sh` | `evidence/performance/scale.json` |
| transport probe (FUSE-over-io_uring negotiation) | `sh scripts/container-transport-probe.sh` | none (probe JSON on stdout) |
| transport court (F0 vs f0-single, 4K randread + FUSE backpressure) | `sh scripts/container-transport.sh` | `evidence/performance/transport.json` |
| fsstress-equivalent + POSIX extras | `sh scripts/container-fsstress.sh` | `evidence/performance/fsstress.json` |
| mounted POSIX court | `sh scripts/container-fuse-test.sh` | none (PASS/FAIL) |
| recovery: capacity / quarantine | `sh scripts/container-go-courts.sh` | `evidence/recovery/capacity.json`, `evidence/recovery/quarantine.json` |
| crash consistency (real `SIGKILL` mid-append) | `sh scripts/container-go-courts.sh` | `evidence/crash.json` |
| cross-FS determinism (ext4/xfs/btrfs/tmpfs identical log bytes) | `sh scripts/container-crossfs.sh` | `evidence/correctness/cross-fs.json` |
| pjdfstest POSIX conformance suite | `sh scripts/container-pjdfstest.sh` | `evidence/performance/pjdfstest.json` |
| full pre-PR gate | `sh scripts/container-verify.sh` | per-stage PASS/FAIL/SKIPPED summary |

`scripts/container-crossfs.sh` and `scripts/container-pjdfstest.sh` need a
**`--privileged`** court container (`docker_run_court_privileged` in
`scripts/lib.sh`): the cross-FS court creates and mounts loop devices
(`mkfs.ext4`/`xfs`/`btrfs`), and the pjdfstest court builds the suite from source
and runs with the full device surface. Both still require `/dev/fuse` to mount
OrFS, so a host without docker or `/dev/fuse` reports them `SKIPPED`, never a
false pass.

`sh scripts/container-verify.sh --fast` runs the cheap subset. Every stage a host
cannot run is reported `SKIPPED`, and a `SKIPPED` stage is never counted as a
pass.

## 6. Run-churn is expected

Re-running a timing court **rewrites its receipt in place**: `baseline.json`,
`scale.json`, and `bench.txt` are snapshots of the last run, not an append-only
history. That is expected — a fresh measurement replaces a stale one. (This is
the opposite of `or.log`, which is non-withdrawal by Law I; a receipt is just a
receipt.)

A court that cannot run reports `SKIPPED` or `WARN` and is never counted as a
pass, so a green summary never hides a court that did not actually run.
