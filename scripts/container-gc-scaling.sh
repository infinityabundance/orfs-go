#!/bin/sh
# Build the court image and run the Go-runtime GC-scaling court inside it.
#
# Receipt: evidence/performance/gc-scaling.json (schema
# orfs.performance.gc-scaling.v1). A host without docker or /dev/fuse reports
# SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-gc-scaling: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-gc-scaling: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence/performance"
build_court_image
# COURT_CONFIGS / ARM_SECONDS / TRACE_SECONDS are forwarded only when set; the
# court script treats an empty value as "use the default".
docker_run_court env \
  COURT_CONFIGS="${COURT_CONFIGS:-}" \
  ARM_SECONDS="${ARM_SECONDS:-}" \
  TRACE_SECONDS="${TRACE_SECONDS:-}" \
  sh /src/scripts/gc-scaling-court.sh
echo "container-gc-scaling: PASS — receipt in evidence/performance/gc-scaling.json"
