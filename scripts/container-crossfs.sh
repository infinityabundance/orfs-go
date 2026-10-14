#!/bin/sh
# Build the court image and run the cross-FS determinism court inside a
# privileged container (loop devices + mkfs are required).
#
# Receipt: evidence/correctness/cross-fs.json (schema orfs.correctness.crossfs.v1).
# A host without docker or /dev/fuse reports SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-crossfs: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-crossfs: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/correctness"
build_court_image
docker_run_court_privileged sh /src/scripts/crossfs-court.sh
echo "container-crossfs: PASS — receipt in evidence/correctness/cross-fs.json"
