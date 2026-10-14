#!/bin/sh
# The mounted FUSE court inside the pinned container (root + /dev/fuse).
set -eu
. "$(dirname "$0")/lib.sh"
if ! have_fuse; then
  echo "container-fuse-test: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi
docker_run_fuse sh -c 'go build -o /tmp/orfs ./cmd/orfs && sh scripts/mount-court.sh /tmp/orfs'
echo "container-fuse-test: PASS"
