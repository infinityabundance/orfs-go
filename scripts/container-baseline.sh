#!/bin/sh
# Build the court image and run the external-baseline court inside it.
#
# Receipts persist under the repository's evidence/performance/
# (baseline.json, scale.json). A host without docker or /dev/fuse reports SKIPPED
# rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-baseline: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-baseline: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/performance"
build_court_image
docker_run_court sh /src/scripts/baseline-court.sh
echo "container-baseline: PASS — receipts in evidence/performance/"
