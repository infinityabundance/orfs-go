#!/bin/sh
# Build the Rust reference, build the court image, and run the cross-language
# compatibility court inside a court container (root + /dev/fuse + CAP_SYS_ADMIN).
#
# Receipt: evidence/correctness/crosslang.json (schema orfs.correctness.crosslang.v1).
#
# Honesty rules:
#   * No docker            -> SKIPPED.
#   * No /dev/fuse         -> SKIPPED.
#   * No Rust reference    -> SKIPPED.
#   * Rust build fails     -> SKIPPED, with the exact cargo failure above it.
#   * The court itself runs the matrix and exits non-zero on a real disagreement;
#     that result is propagated (never masked as a pass).
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-crosslang: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-crosslang: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi
if [ ! -d "${REPO_ROOT}/.reference/OrFS/tests/fixtures/legacy_v0" ]; then
  echo "container-crosslang: SKIPPED (no Rust reference fixtures at .reference/OrFS/tests/fixtures)"
  exit 0
fi

# 1. Build the Rust reference `orfs` binary in a capped rust container. A build
#    failure is an environment limitation here: report SKIPPED, never a pass.
if ! sh "$(dirname "$0")/container-rust-build.sh"; then
  echo "container-crosslang: SKIPPED (could not build the Rust reference; see the cargo output above)"
  exit 0
fi

# 2. Build the court image (which carries the Go `orfs` and the orfsxlang probe)
#    and run the matrix with the Rust binary and fixtures bind-mounted.
mkdir -p "${REPO_ROOT}/evidence/correctness"
build_court_image

set +e
docker run --rm \
  --platform linux/amd64 \
  --network host ${CAPS_COMMON} \
  --device /dev/fuse --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
  -e HOME=/tmp -e TMPDIR=/tmp \
  -e "GOFLAGS=${GOFLAGS_VALUE}" -e GOCACHE=/tmp/gobuild \
  -v "${REPO_ROOT}/evidence":/evidence \
  -v "${ORFS_RS_DIR}":/out-rs:ro \
  -v "${REPO_ROOT}/.reference/OrFS/tests/fixtures":/rust-fixtures:ro \
  -w /src \
  "${COURT_IMAGE}" sh /src/scripts/crosslang-court.sh
rc=$?
set -e

if [ "$rc" -eq 0 ]; then
  echo "container-crosslang: PASS — receipt in evidence/correctness/crosslang.json"
  exit 0
fi
echo "container-crosslang: FAIL (court exit $rc) — see evidence/correctness/crosslang.json" >&2
exit "$rc"
