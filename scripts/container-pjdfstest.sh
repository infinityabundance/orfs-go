#!/bin/sh
# Build the court image and run the pjdfstest court inside the privileged court
# container (the runner supplies /dev/fuse and the full device surface).
#
# Receipt: evidence/performance/pjdfstest.json (schema orfs.performance.pjdfstest.v1).
# A host without docker or /dev/fuse reports SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-pjdfstest: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-pjdfstest: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/performance"
build_court_image
docker_run_court_privileged sh /src/scripts/pjdfstest-court.sh
echo "container-pjdfstest: PASS — receipt in evidence/performance/pjdfstest.json"
