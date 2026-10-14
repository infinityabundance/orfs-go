#!/bin/sh
# Open-source head-to-head court, step 1 — host wrapper.
#
# Builds the Rust reference (if needed), builds the pinned court image, and runs
# the mounted court inside the capped court container (root + /dev/fuse +
# CAP_SYS_ADMIN) with the Rust binary bind-mounted read-only. The receipt
# persists at the repository's evidence/oss-compare.json.
#
# SKIPPED (never a false pass) when: no docker, no /dev/fuse, or the Rust
# reference cannot be built. If the Rust reference is absent but OrFS-GO can be
# measured, the court runs anyway and records the asymmetry as a caveat.
#
# Usage: sh scripts/container-oss-compare.sh
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-oss-compare: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-oss-compare: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

# Ensure the Rust reference is staged. A build failure is an environment
# limitation: report SKIPPED, never a pass.
if [ ! -x "${ORFS_RS_BIN}" ]; then
  if ! sh "$(dirname "$0")/container-rust-build.sh"; then
    echo "container-oss-compare: SKIPPED (could not build the Rust reference; see the cargo output above)"
    exit 0
  fi
fi

mkdir -p "${REPO_ROOT}/evidence"
build_court_image

# Forward the container caps so the receipt records them, and the workload knobs
# so the court is tunable without editing the script (docker does not inherit the
# host environment).
set +e
docker run --rm \
  --platform linux/amd64 \
  --network host ${CAPS_COMMON} \
  --device /dev/fuse --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
  -e HOME=/tmp -e TMPDIR=/tmp \
  -e "GOFLAGS=${GOFLAGS_VALUE}" -e GOCACHE=/tmp/gobuild \
  -e ORFS_EVIDENCE_DIR=/evidence \
  -e CAP_MEM_LIMIT="${MEM_LIMIT}" -e CAP_MEMSWAP_LIMIT="${MEMSWAP_LIMIT}" \
  -e CAP_CPUS="${CPUS}" -e CAP_PIDS_LIMIT="${PIDS_LIMIT}" \
  -e SEGMENT_SIZE="${SEGMENT_SIZE:-8388608}" \
  -e APPEND_SECONDS="${APPEND_SECONDS:-5}" -e READ_SECONDS="${READ_SECONDS:-4}" \
  -e WRITERS="${WRITERS:-1 64}" -e READERS="${READERS:-1 64}" \
  -e BLOCK_SIZES="${BLOCK_SIZES:-4096 65536 1048576}" \
  -e RECORD_BYTES="${RECORD_BYTES:-4096}" -e READ_MIB="${READ_MIB:-64}" \
  -e MAX_APPEND_MIB="${MAX_APPEND_MIB:-1024}" -e SEED="${SEED:-1}" \
  -v "${REPO_ROOT}/evidence":/evidence \
  -v "${ORFS_RS_DIR}":/out-rs:ro \
  -w /src \
  "${COURT_IMAGE}" sh /src/scripts/oss-compare-court.sh
rc=$?
set -e

if [ "$rc" -eq 0 ]; then
  echo "container-oss-compare: PASS — receipt in evidence/oss-compare.json"
  exit 0
fi
echo "container-oss-compare: FAIL (court exit $rc) — see evidence/oss-compare.json" >&2
exit "$rc"
