#!/bin/sh
# Build the CLI inside the pinned container.
set -eu
. "$(dirname "$0")/lib.sh"
docker_run go build -o /tmp/orfs ./cmd/orfs
echo "container-build: PASS"
