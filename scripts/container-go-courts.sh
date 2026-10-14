#!/bin/sh
# Run the in-Go courts with ORFS_EVIDENCE=1 so their receipts are written into
# the repository: evidence/performance/{amplification,cache}.json and
# evidence/recovery/{capacity,quarantine}.json.
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
echo "container-go-courts: PASS — receipts in evidence/performance/ and evidence/recovery/"
