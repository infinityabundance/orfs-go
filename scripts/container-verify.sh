#!/bin/sh
# One-command continuous verification: the pre-PR gate.
#
# Runs fmt/vet, the default suite, the race detector, a bounded fuzz pass, the
# growth courts, and the mounted FUSE/fsstress/baseline courts, printing a
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
    else
      skip "fuse (mounted POSIX court)" "no /dev/fuse"
      skip "fsstress (bounded op storm + POSIX extras)" "no /dev/fuse"
      skip "baseline (fio vs ext4/tmpfs)" "no /dev/fuse"
    fi
  else
    skip "race (concurrent immutable reads)" "--fast"
    skip "fuzz (bounded campaign)" "--fast"
    skip "growth courts (amplification, capacity, quarantine)" "--fast"
    skip "fuse (mounted POSIX court)" "--fast"
    skip "fsstress (bounded op storm + POSIX extras)" "--fast"
    skip "baseline (fio vs ext4/tmpfs)" "--fast"
  fi
fi

printf '\n============================================\n'
if [ "$FAILED" -eq 0 ]; then
  printf 'verify: PASS (%d skipped)\n' "$SKIPPED"
  exit 0
fi
printf 'verify: FAIL — %d stage(s) failed, %d skipped\n' "$FAILED" "$SKIPPED"
exit 1
