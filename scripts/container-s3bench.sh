#!/bin/sh
# The WORM S3 court — host wrapper.
#
# Builds the pinned court image (which now bakes cmd/orfs-s3 and the s3load
# driver) and runs scripts/s3bench-court.sh inside the resource-capped court
# container. The receipt persists at evidence/s3bench.json.
#
# The MinIO comparator is staged best-effort: MinIO's public images are no longer
# pullable (see scripts/lib.sh), in which case the court records the MinIO arm as
# `unavailable` with the reason and still measures the gateway. A SKIPPED is
# reported rather than a false comparison.
#
# Usage: sh scripts/container-s3bench.sh
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-s3bench: SKIPPED (no usable docker daemon)"
  exit 0
fi

MINIO_STAGED=0
if stage_minio; then
  MINIO_STAGED=1
  MINIO_NOTE="staged from ${MINIO_IMAGE}"
else
  MINIO_NOTE="MinIO public images refuse access (docker.io/minio/minio and quay.io/minio/minio both answer with a registry access denial); no MinIO binary was obtainable, so no MinIO comparison was run"
  echo "container-s3bench: NOTE: MinIO is unavailable (its public images refuse access); the S3 court will record that and compare nothing against it"
fi

mkdir -p "${REPO_ROOT}/evidence"
# The measured payload is disk-hungry (an append-only volume that retains every
# PUT, plus manifested bodies); keep it on the repository's disk-backed cache
# rather than the container's overlay, which is a small tmpfs on this host.
S3WORK_DIR=${REPO_ROOT}/.cache/s3work
mkdir -p "${S3WORK_DIR}"
build_court_image

set +e
docker run --rm \
  --platform linux/amd64 \
  --network host ${CAPS_COMMON} \
  -e HOME=/tmp -e TMPDIR=/tmp \
  -e "GOFLAGS=${GOFLAGS_VALUE}" -e GOCACHE=/tmp/gobuild \
  -e ORFS_EVIDENCE_DIR=/evidence \
  -e CAP_MEM_LIMIT="${MEM_LIMIT}" -e CAP_MEMSWAP_LIMIT="${MEMSWAP_LIMIT}" \
  -e CAP_CPUS="${CPUS}" -e CAP_PIDS_LIMIT="${PIDS_LIMIT}" \
  -e SEGMENT_SIZE="${SEGMENT_SIZE:-8388608}" \
  -e DURATION="${DURATION:-5}" \
  -e CONCURRENCY="${CONCURRENCY:-1 64}" \
  -e OBJECT_BYTES="${OBJECT_BYTES:-4096 1048576}" \
  -e GET_BYTES="${GET_BYTES:-4096}" \
  -e SEED="${SEED:-1}" \
  -e "S3WORK=/s3work" \
  -e "S3BENCH_OUT=${S3BENCH_OUT:-/evidence/s3bench.json}" \
  -e "GW_MANIFEST=${GW_MANIFEST:-on}" \
  -e MAX_PUT_MIB="${MAX_PUT_MIB:-256}" \
  -e MINIO_ARM="${MINIO_ARM:-1}" \
  -e "MINIO_NOTE=${MINIO_NOTE}" \
  -e "MINIO=${MINIO:-/out/minio}" \
  -v "${REPO_ROOT}/evidence":/evidence \
  -v "${S3WORK_DIR}":/s3work \
  -v "$(dirname "${MINIO_BIN}")":/out-minio:ro \
  -w /src \
  "${COURT_IMAGE}" sh /src/scripts/s3bench-court.sh
rc=$?
set -e

if [ "$rc" -ne 0 ]; then
  echo "container-s3bench: FAIL (court exit $rc) — see evidence/s3bench.json" >&2
  exit "$rc"
fi
if [ "${MINIO_STAGED}" = 0 ]; then
  echo "container-s3bench: PASS — receipt in evidence/s3bench.json (MinIO unavailable, recorded)"
else
  echo "container-s3bench: PASS — receipt in evidence/s3bench.json"
fi
