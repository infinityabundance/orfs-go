#!/bin/sh
# Run an arbitrary command inside the pinned, resource-capped container.
#
# This is the ONLY supported way to run builds, tests, benchmarks, fuzzers, or
# tools for this project: the host must never run them uncapped (an uncapped Go
# build/test/bench has OOM-killed the editor). The container is capped by
# scripts/lib.sh (ORFS_MEM_LIMIT, ORFS_MEMSWAP_LIMIT, ORFS_CPUS, ORFS_PIDS_LIMIT).
#
# Usage: sh scripts/run.sh go test ./...
set -eu
. "$(dirname "$0")/lib.sh"
if ! have_docker; then
  echo "run: no usable docker daemon" >&2
  exit 1
fi
docker_run "$@"
