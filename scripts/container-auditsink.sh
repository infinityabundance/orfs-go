#!/bin/sh
# Build the court image and run the audit-sink court inside it.
#
# The receipt persists at the repository's evidence/audit-sink.json
# (schema orfs.audit-sink.v1). A host without docker or /dev/fuse reports
# SKIPPED rather than a false pass.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-auditsink: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-auditsink: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

mkdir -p "${REPO_ROOT}/evidence"
build_court_image
# Forward the workload knobs into the container (docker does not inherit the host env).
docker_run_court env \
  AUDIT_RECORDS="${AUDIT_RECORDS:-1500}" \
  AUDIT_PAYLOAD="${AUDIT_PAYLOAD:-4096}" \
  AUDIT_RATE="${AUDIT_RATE:-150}" \
  AUDIT_SEED="${AUDIT_SEED:-1}" \
  AUDIT_KILLS="${AUDIT_KILLS:-8}" \
  AUDIT_KILL_PAYLOAD="${AUDIT_KILL_PAYLOAD:-262144}" \
  AUDIT_CRASH_ITERS="${AUDIT_CRASH_ITERS:-120}" \
  AUDIT_CRASH_BUDGET_S="${AUDIT_CRASH_BUDGET_S:-40}" \
  SEGMENT_SIZE="${SEGMENT_SIZE:-4194304}" \
  sh /src/courts/auditsink/court.sh
echo "container-auditsink: PASS — receipt in evidence/audit-sink.json"
