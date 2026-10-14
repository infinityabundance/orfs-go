#!/bin/sh
# Build the court image and run the transport court inside it.
#
# Receipts persist at the repository's evidence/performance/transport.json. A
# host without docker or /dev/fuse reports SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-transport: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-transport: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/performance"
build_court_image
docker_run_court sh /src/scripts/transport-court.sh
echo "container-transport: PASS — receipt in evidence/performance/transport.json"
