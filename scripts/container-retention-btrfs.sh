#!/bin/sh
# Btrfs retained-snapshots retention court — host wrapper.
#
# Builds the pinned court image and runs the retention court inside a privileged
# court container (root + /dev/fuse + loop devices + mkfs.btrfs + the shared
# scripts/lib.sh caps). The receipt persists at the repository's
# evidence/retention-btrfs.json.
#
# SKIPPED (never a false pass) when: no docker, no /dev/fuse, or no
# /dev/loop-control on the host. The court itself also reports a precise SKIPPED
# when mkfs.btrfs or the loop mount is unavailable inside the container; in that
# case this wrapper reports SKIPPED too (no receipt is written).
#
# Usage: sh scripts/container-retention-btrfs.sh
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-retention-btrfs: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-retention-btrfs: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi
if [ ! -e /dev/loop-control ]; then
  echo "container-retention-btrfs: SKIPPED (no /dev/loop-control; loop devices unavailable)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence"
rm -f "${REPO_ROOT}/evidence/retention-btrfs.json"
build_court_image

# Forward the container caps so the receipt records them, and the workload knobs
# so the court is tunable without editing the script (docker does not inherit the
# host environment).
set +e
docker run --rm \
  --platform linux/amd64 \
  --privileged \
  --network host ${CAPS_COMMON} \
  -e HOME=/tmp -e TMPDIR=/tmp \
  -e "GOFLAGS=${GOFLAGS_VALUE}" -e GOCACHE=/tmp/gobuild \
  -e ORFS_EVIDENCE_DIR=/evidence \
  -e CAP_MEM_LIMIT="${MEM_LIMIT}" -e CAP_MEMSWAP_LIMIT="${MEMSWAP_LIMIT}" \
  -e CAP_CPUS="${CPUS}" -e CAP_PIDS_LIMIT="${PIDS_LIMIT}" \
  -e SEGMENT_SIZE="${SEGMENT_SIZE:-8388608}" \
  -e IMG_MIB="${IMG_MIB:-512}" \
  -e EPOCHS="${EPOCHS:-8}" \
  -e WRITERS="${WRITERS:-1}" \
  -e RECORDS_PER_EPOCH="${RECORDS_PER_EPOCH:-256}" \
  -e RECORD_BYTES="${RECORD_BYTES:-4096}" \
  -e APPEND_SECONDS="${APPEND_SECONDS:-30}" \
  -e READ_SECONDS="${READ_SECONDS:-1}" -e READERS="${READERS:-1}" \
  -e BLOCK_SIZE="${BLOCK_SIZE:-4096}" -e READ_EPOCHS="${READ_EPOCHS:-all}" \
  -e SEED="${SEED:-1}" -e ORFS_ARM="${ORFS_ARM:-1}" \
  -v "${REPO_ROOT}/evidence":/evidence \
  -w /src \
  "${COURT_IMAGE}" sh /src/scripts/retention-btrfs-court.sh
rc=$?
set -e

if [ "$rc" -eq 0 ]; then
  if [ -f "${REPO_ROOT}/evidence/retention-btrfs.json" ]; then
    echo "container-retention-btrfs: PASS — receipt in evidence/retention-btrfs.json"
  else
    echo "container-retention-btrfs: SKIPPED (the court reported SKIPPED; no receipt was written)"
  fi
  exit 0
fi
echo "container-retention-btrfs: FAIL (court exit $rc) — see evidence/retention-btrfs.json" >&2
exit "$rc"
