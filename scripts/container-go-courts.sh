#!/bin/sh
# Run the in-Go courts with ORFS_EVIDENCE=1 so their receipts are written into
# the repository: evidence/performance/{amplification,cache,groupcommit,allocs}.json,
# evidence/recovery/{capacity,quarantine}.json, and evidence/crash.json.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-go-courts: SKIPPED (no usable docker daemon)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/performance" "${REPO_ROOT}/evidence/recovery"
docker_run env ORFS_EVIDENCE=1 sh -c \
  'go test ./internal/orbench/ ./internal/log/ -run "TestAmplification|TestCapacity|TestQuarantine"'
docker_run env ORFS_EVIDENCE=1 sh -c \
  'go test ./internal/fcache/ -run TestScanResistance'
# Group commit: N concurrent writers, one barrier per publish (OFF) vs one
# barrier per batch (ON). Run here so the backing is the container's
# overlay/ext4, where fsync has real cost.
docker_run env ORFS_EVIDENCE=1 sh -c \
  'go test ./internal/log/ -run TestGroupCommitCourt -count=1'
# Per-read allocation court: the steady-state authoritative 4 KiB read path over
# a 1 MiB file, asserting the destination []byte is the only per-read allocation.
# Writes evidence/performance/allocs.json (schema orfs.performance.allocs.v1).
docker_run env ORFS_EVIDENCE=1 sh -c \
  'go test ./internal/namespace/ -run TestAuthoritativeReadAllocs -count=1'
# Crash-consistency court: a REAL SIGKILL of the cmd/orfscrashtest writer at a
# random point mid-append, then reopen read-write (quarantining the abandoned
# tail) and verify --deep. 1000 iterations. Writes evidence/crash.json (schema
# orfs.performance.crash.v1).
docker_run env ORFS_EVIDENCE=1 ORFS_CRASH_ITERS=1000 ORFS_CRASH_BUDGET_S=300 sh -c \
  'go test ./internal/log/ -run TestCrashConsistencyCourt -count=1'
# Checkpoint recovery court: bound mount replay to a committed checkpoint and
# measure the startup-work / Go-heap curve across exponentially increasing
# retained histories (2^k committed bytes), comparing full replay, checkpoint +
# suffix replay, and directory-assisted discovery. Asserts the bounded result
# equals full replay at every size. Writes evidence/recovery/checkpoints.json
# (schema orfs.recovery.checkpoints.v1).
docker_run env ORFS_EVIDENCE=1 ORFS_RECOVERY_REPS=10 sh -c \
  'go test ./internal/orbench/ -run TestCheckpointRecoveryCourt -count=1'
echo "container-go-courts: PASS — receipts in evidence/performance/, evidence/recovery/, and evidence/crash.json"
