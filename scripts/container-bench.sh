#!/bin/sh
# Run the micro-benchmarks in the pinned container and tee a receipt.
set -eu
. "$(dirname "$0")/lib.sh"
mkdir -p "${REPO_ROOT}/evidence/performance"
OUT="${REPO_ROOT}/evidence/performance/bench.txt"
docker_run sh -c 'go test -run "^$" -bench . -benchtime=200ms -benchmem ./...' | tee "$OUT"
echo "container-bench: wrote ${OUT}"
