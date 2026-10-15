#!/bin/sh
# OpenZFS retained-snapshot retention court — host wrapper.
#
# Builds the pinned KVM-court image and runs the retention court inside it. The
# host has no OpenZFS kernel module, so the court boots a KVM micro-VM whose
# *guest* kernel ships the out-of-tree OpenZFS module: real OpenZFS, measured
# with the same driver and the same axes as the Btrfs court, without ever
# touching the host kernel. The receipt persists at the repository's
# evidence/retention-zfs.json.
#
# SKIPPED (never a false pass) when: no usable docker daemon, or the host has
# neither /dev/kvm (the micro-VM path) nor /dev/zfs (a directly reachable
# OpenZFS). The court itself reports a precise SKIPPED, with the feasibility
# evidence, when neither path yields a measurement.
#
# Usage: sh scripts/container-retention-zfs.sh
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-retention-zfs: SKIPPED (no usable docker daemon)"
  exit 0
fi
if [ ! -e /dev/kvm ] && [ ! -e /dev/zfs ]; then
  echo "container-retention-zfs: SKIPPED (no /dev/kvm for the micro-VM path and no /dev/zfs for a direct path)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence"
rm -f "${REPO_ROOT}/evidence/retention-zfs.json"
build_court_image
build_zfs_kvm_image

# Bind the host's /dev/zfs read-only when it exists, so a host that *does* have
# OpenZFS can also be measured directly. On this host it is absent, and the
# court falls back to the KVM micro-VM.
ZFS_DEV=""
[ -e /dev/zfs ] && ZFS_DEV="-v /dev/zfs:/dev/zfs:ro"

set +e
# shellcheck disable=SC2086
docker run --rm \
  --platform linux/amd64 \
  --privileged \
  --device /dev/kvm \
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
  -e ZFS_KVM="${ZFS_KVM:-1}" \
  -e VM_MEM_MIB="${VM_MEM_MIB:-384}" -e VM_CPUS="${VM_CPUS:-2}" \
  -e VM_BOOT_SECONDS="${VM_BOOT_SECONDS:-600}" \
  -e ZFSVM_WORKDIR=/kvm/vmwork \
  -e ZFS_KVM_IMAGE=/kvm/ubuntu-24.04-minimal-cloudimg-amd64.img \
  -v "${REPO_ROOT}/evidence":/evidence \
  -v "${ZFSKVM_CACHE_DIR}":/kvm \
  -w /src \
  "${ZFSKVM_IMAGE}" sh /src/scripts/retention-zfs-court.sh
rc=$?
set -e

if [ "$rc" -ne 0 ]; then
  echo "container-retention-zfs: FAIL (court exit $rc) — see evidence/retention-zfs.json" >&2
  exit "$rc"
fi

if [ -f "${REPO_ROOT}/evidence/retention-zfs.json" ]; then
  if grep -q '"status": "skipped"' "${REPO_ROOT}/evidence/retention-zfs.json"; then
    echo "container-retention-zfs: SKIPPED (no reachable OpenZFS; feasibility receipt in evidence/retention-zfs.json)"
  else
    echo "container-retention-zfs: PASS — receipt in evidence/retention-zfs.json"
  fi
  exit 0
fi
echo "container-retention-zfs: SKIPPED (the court reported SKIPPED; no receipt was written)"
exit 0
