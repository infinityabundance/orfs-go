#!/bin/sh
# One-command continuous verification: the pre-PR gate.
#
# Runs fmt/vet, the default suite, the race detector, a bounded fuzz pass, the
# growth courts, and the mounted FUSE/fsstress/baseline courts plus the cross-FS
# determinism and pjdfstest courts, printing a
# per-stage PASS/FAIL/SKIPPED summary. A stage
# that cannot run is reported SKIPPED and is not counted as a pass: a green
# summary never hides a court that did not actually run.
#
# Usage:
#   sh scripts/container-verify.sh          # full pre-PR court
#   sh scripts/container-verify.sh --fast   # cheap, always-available subset
set +e
. "$(dirname "$0")/lib.sh"

FAST=0
[ "${1:-}" = "--fast" ] && FAST=1

FAILED=0
SKIPPED=0
run_stage() {
  name=$1; shift
  printf '\n=== %s ===\n' "$name"
  if "$@"; then
    printf '>>> %s: PASS\n' "$name"
  else
    printf '>>> %s: FAIL\n' "$name"
    FAILED=$((FAILED + 1))
  fi
}
run_host() {
  name=$1; shift
  printf '\n=== %s ===\n' "$name"
  if "$@"; then
    printf '>>> %s: PASS\n' "$name"
  else
    printf '>>> %s: FAIL\n' "$name"
    FAILED=$((FAILED + 1))
  fi
}
skip() { printf '\n=== %s: SKIPPED (%s) ===\n' "$1" "$2"; SKIPPED=$((SKIPPED + 1)); }

if ! have_docker; then
  skip "container stages" "no usable docker daemon"
else
  run_stage "check (gofmt + vet)" sh "$(dirname "$0")/container-check.sh"
  run_stage "test (default suite)" sh "$(dirname "$0")/container-test.sh"
  if [ "$FAST" -eq 0 ]; then
    run_stage "race (concurrent immutable reads)" \
      docker_run go test -race ./internal/...
    run_stage "fuzz (bounded campaign)" \
      docker_run sh -c 'go test ./internal/record/ -run=^$ -fuzz=FuzzDecodeFrame -fuzztime=20s'
    run_stage "growth courts (amplification, capacity, quarantine)" \
      sh "$(dirname "$0")/container-go-courts.sh"
    if have_fuse; then
      run_stage "fuse (mounted POSIX court)" sh "$(dirname "$0")/container-fuse-test.sh"
      run_stage "fsstress (bounded op storm + POSIX extras)" sh "$(dirname "$0")/container-fsstress.sh"
      run_stage "baseline (fio vs ext4/tmpfs)" sh "$(dirname "$0")/container-baseline.sh"
      run_stage "transport court (F0 vs f0-single, 4K randread)" sh "$(dirname "$0")/container-transport.sh"
      run_stage "gc-scaling court (GOGC x GOMEMLIMIT sweep: app tail + Go runtime)" \
        sh "$(dirname "$0")/container-gc-scaling.sh"
      run_stage "cross-fs determinism (identical log bytes across ext4/xfs/btrfs/tmpfs)" \
        sh "$(dirname "$0")/container-crossfs.sh"
      run_stage "pjdfstest (upstream POSIX syscall suite vs the mount)" \
        sh "$(dirname "$0")/container-pjdfstest.sh"
      run_stage "worm (append-only WORM case study)" \
        sh "$(dirname "$0")/container-worm.sh"
      run_stage "audit-sink (JSON audit-log sink: inotify tail, immutability, scrape)" \
        sh "$(dirname "$0")/container-auditsink.sh"
      run_stage "cross-language (Rust reference <-> Go: fixtures, mounts, handoffs, recovery)" \
        sh "$(dirname "$0")/container-crosslang.sh"
      run_stage "oss-compare (mounted OrFS-GO vs OrFS-Rust vs SeaweedFS: append+fsync and read)" \
        sh "$(dirname "$0")/container-oss-compare.sh"
      run_stage "retention-btrfs (Btrfs read-only snapshots vs the OrFS-GO log: retain every committed state)" \
        sh "$(dirname "$0")/container-retention-btrfs.sh"
    else
      skip "fuse (mounted POSIX court)" "no /dev/fuse"
      skip "fsstress (bounded op storm + POSIX extras)" "no /dev/fuse"
      skip "baseline (fio vs ext4/tmpfs)" "no /dev/fuse"
      skip "transport court (F0 vs f0-single, 4K randread)" "no /dev/fuse"
      skip "gc-scaling court (GOGC x GOMEMLIMIT sweep: app tail + Go runtime)" "no /dev/fuse"
      skip "cross-fs determinism (identical log bytes across ext4/xfs/btrfs/tmpfs)" "no /dev/fuse"
      skip "pjdfstest (upstream POSIX syscall suite vs the mount)" "no /dev/fuse"
      skip "worm (append-only WORM case study)" "no /dev/fuse"
      skip "audit-sink (JSON audit-log sink: inotify tail, immutability, scrape)" "no /dev/fuse"
      skip "cross-language (Rust reference <-> Go: fixtures, mounts, handoffs, recovery)" "no /dev/fuse"
      skip "oss-compare (mounted OrFS-GO vs OrFS-Rust vs SeaweedFS: append+fsync and read)" "no /dev/fuse"
      skip "retention-btrfs (Btrfs read-only snapshots vs the OrFS-GO log: retain every committed state)" "no /dev/fuse"
    fi
  else
    skip "race (concurrent immutable reads)" "--fast"
    skip "fuzz (bounded campaign)" "--fast"
    skip "growth courts (amplification, capacity, quarantine)" "--fast"
    skip "fuse (mounted POSIX court)" "--fast"
    skip "fsstress (bounded op storm + POSIX extras)" "--fast"
    skip "baseline (fio vs ext4/tmpfs)" "--fast"
    skip "transport court (F0 vs f0-single, 4K randread)" "--fast"
    skip "gc-scaling court (GOGC x GOMEMLIMIT sweep: app tail + Go runtime)" "--fast"
    skip "cross-fs determinism (identical log bytes across ext4/xfs/btrfs/tmpfs)" "--fast"
    skip "pjdfstest (upstream POSIX syscall suite vs the mount)" "--fast"
    skip "worm (append-only WORM case study)" "--fast"
    skip "audit-sink (JSON audit-log sink: inotify tail, immutability, scrape)" "--fast"
    skip "cross-language (Rust reference <-> Go: fixtures, mounts, handoffs, recovery)" "--fast"
    skip "oss-compare (mounted OrFS-GO vs OrFS-Rust vs SeaweedFS: append+fsync and read)" "--fast"
    skip "retention-btrfs (Btrfs read-only snapshots vs the OrFS-GO log: retain every committed state)" "--fast"
  fi
fi

printf '\n============================================\n'
if [ "$FAILED" -eq 0 ]; then
  printf 'verify: PASS (%d skipped)\n' "$SKIPPED"
  exit 0
fi
printf 'verify: FAIL — %d stage(s) failed, %d skipped\n' "$FAILED" "$SKIPPED"
exit 1
