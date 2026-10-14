#!/bin/sh
# Build the court image and run the fsstress court inside it.
#
# Receipts persist at the repository's evidence/performance/fsstress.json. A host
# without docker or /dev/fuse reports SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-fsstress: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-fsstress: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/performance"
build_court_image
docker_run_court sh /src/scripts/fsstress-court.sh
echo "container-fsstress: PASS — receipt in evidence/performance/fsstress.json"
