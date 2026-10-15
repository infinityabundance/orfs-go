#!/bin/sh
# OpenZFS retained-snapshot retention court — host wrapper.
#
# Builds the pinned court image and runs the retention court inside a privileged
# court container (root + /dev/fuse + loop devices + mkfs.btrfs + the shared
# scripts/lib.sh caps). The court image ships zfsutils-linux, so the container can
# run `zpool`/`zfs`; the host's /dev/zfs is bind-mounted read-only *when it
# exists*. When it does not exist (no OpenZFS kernel module on the host), the
# court runs anyway, records the feasibility evidence, writes a SKIPPED receipt to
# evidence/retention-zfs.json, and exits 0 — loading the kernel module would be a
# host change and is out of scope. The receipt persists at the repository's
# evidence/retention-zfs.json.
#
# SKIPPED (never a false pass) when: no docker, no /dev/fuse, or no
# /dev/loop-control on the host.
#
# Usage: sh scripts/container-retention-zfs.sh
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-retention-zfs: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-retention-zfs: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi
if [ ! -e /dev/loop-control ]; then
  echo "container-retention-zfs: SKIPPED (no /dev/loop-control; loop devices unavailable)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence"
rm -f "${REPO_ROOT}/evidence/retention-zfs.json"
build_court_image

# Bind the host's /dev/zfs read-only when it exists, so a host that *does* have
# OpenZFS gives the container access to the pool. The path is absent on a host
# with no OpenZFS kernel module; the court then records that as its finding.
ZFS_DEV=""
if [ -e /dev/zfs ]; then
  ZFS_DEV="-v /dev/zfs:/dev/zfs:ro"
fi

set +e
# shellcheck disable=SC2086
docker run --rm \
  --platform linux/amd64 \
  --privileged \
  --network host ${CAPS_COMMON} \
  ${ZFS_DEV} \
  -e HOME=/tmp -e TMPDIR=/tmp \
  -e "GOFLAGS=${GOFLAGS_VALUE}" -e GOCACHE=/tmp/gobuild \
  -e ORFS_EVIDENCE_DIR=/evidence \
  -e CAP_MEM_LIMIT="${MEM_LIMIT}" -e CAP_MEMSWAP_LIMIT="${MEMSWAP_LIMIT}" \
  -e CAP_CPUS="${CPUS}" -e CAP_PIDS_LIMIT="${PIDS_LIMIT}" \
  -e SEGMENT_SIZE="${SEGMENT_SIZE:-8388608}" \
  -e IMG_MIB="${IMG_MIB:-512}" \
  -e POOL="${POOL:-orfsret}" \
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
  "${COURT_IMAGE}" sh /src/scripts/retention-zfs-court.sh
rc=$?
set -e

if [ "$rc" -ne 0 ]; then
  echo "container-retention-zfs: FAIL (court exit $rc) — see evidence/retention-zfs.json" >&2
  exit "$rc"
fi

if [ -f "${REPO_ROOT}/evidence/retention-zfs.json" ]; then
  if grep -q '"status": "skipped"' "${REPO_ROOT}/evidence/retention-zfs.json"; then
    echo "container-retention-zfs: SKIPPED (OpenZFS unavailable; feasibility receipt in evidence/retention-zfs.json)"
  else
    echo "container-retention-zfs: PASS — receipt in evidence/retention-zfs.json"
  fi
  exit 0
fi
echo "container-retention-zfs: SKIPPED (the court reported SKIPPED; no receipt was written)"
exit 0
