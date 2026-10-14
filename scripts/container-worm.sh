#!/bin/sh
# Build the court image and run the WORM case-study court inside it.
#
# The receipt persists at the repository's evidence/worm-case-study.json. A host
# without docker or /dev/fuse reports SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-worm: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-worm: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence"
build_court_image
# Forward the workload knobs into the container (docker does not inherit the host env).
docker_run_court env \
  WORM_RECORDS="${WORM_RECORDS:-2048}" \
  WORM_PAYLOAD="${WORM_PAYLOAD:-4080}" \
  SEGMENT_SIZE="${SEGMENT_SIZE:-4194304}" \
  sh /src/scripts/worm-court.sh
echo "container-worm: PASS — receipt in evidence/worm-case-study.json"
