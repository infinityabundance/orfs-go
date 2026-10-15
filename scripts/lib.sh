# Shared environment for the OrFS-GO container workflow.
#
# EVERYTHING runs inside a pinned, resource-capped container. Nothing heavy is
# ever run directly on the host: a host run of an uncapped Go build/test/bench
# has OOM-killed the editor before. One container runs at a time.
#
# Note on -buildvcs=false: the mounted courts run as root over a repository owned
# by the host user, and `go build` would otherwise ask git for VCS status and
# fail with "dubious ownership". We never want VCS stamping in an image anyway.
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GO_VERSION=${GO_VERSION:-1.26}
# Debian-based image: it ships gcc, so `go test -race` (which needs cgo) works.
IMAGE="golang:${GO_VERSION}-bookworm"

# Resource caps. These protect the host; override via the ORFS_* environment.
MEM_LIMIT=${ORFS_MEM_LIMIT:-2g}
MEMSWAP_LIMIT=${ORFS_MEMSWAP_LIMIT:-2g}
CPUS=${ORFS_CPUS:-4}
PIDS_LIMIT=${ORFS_PIDS_LIMIT:-1024}

CAPS_COMMON="--memory ${MEM_LIMIT} --memory-swap ${MEMSWAP_LIMIT} --cpus ${CPUS} --pids-limit ${PIDS_LIMIT} --ulimit nofile=4096:4096"
GOFLAGS_VALUE=${ORFS_GOFLAGS:-"-mod=mod -buildvcs=false"}

# Caches live inside the repo so they are bind-mounted and reused across runs.
mkdir -p "${REPO_ROOT}/.cache/gomod" "${REPO_ROOT}/.cache/gobuild"

# docker_run runs an unprivileged container as the host user. It does not use
# exec so one script can drive several containers sequentially.
docker_run() {
  docker run --rm \
    --network host ${CAPS_COMMON} \
    --cap-drop=ALL --security-opt=no-new-privileges \
    -u "$(id -u):$(id -g)" \
    -e HOME=/tmp -e "GOFLAGS=${GOFLAGS_VALUE}" \
    -e GOMODCACHE=/src/.cache/gomod -e GOCACHE=/src/.cache/gobuild \
    -v "${REPO_ROOT}":/src -w /src \
    "${IMAGE}" "$@"
}

# docker_run_fuse adds /dev/fuse and CAP_SYS_ADMIN for real mounts. The container
# runs as root because the kernel grants FUSE mount privileges to root here, so
# caches live in /tmp to avoid leaving root-owned files in the repository.
docker_run_fuse() {
  docker run --rm \
    --network host ${CAPS_COMMON} \
    --device /dev/fuse --cap-add SYS_ADMIN \
    --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    -e HOME=/tmp -e "GOFLAGS=${GOFLAGS_VALUE}" \
    -e GOMODCACHE=/tmp/gomod -e GOCACHE=/tmp/gobuild \
    -v "${REPO_ROOT}":/src -w /src \
    "${IMAGE}" "$@"
}

# --- Rust reference container -------------------------------------------------
# The Rust reference (`OrFS`) is the compatibility court's source of truth. It is
# built and run in the same pinned, resource-capped way as everything else: never
# on the host. The image matches the reference's own `rust-toolchain.toml`
# (1.99.0). Cargo's registry and target caches live under the repo's gitignored
# .cache/ so a rebuild reuses downloads, and every build artifact is redirected
# away from `.reference/` (CARGO_TARGET_DIR) so a build never writes into the
# reference tree.
RUST_IMAGE=${ORFS_RUST_IMAGE:-rust:1.99.0-slim-bookworm}
# Where the built reference binary is staged for the cross-language court.
ORFS_RS_DIR=${REPO_ROOT}/.cache/orfs-rs
ORFS_RS_BIN=${ORFS_RS_DIR}/orfs
mkdir -p "${REPO_ROOT}/.cache/cargo" "${REPO_ROOT}/.cache/orfs-rs-target" "${ORFS_RS_DIR}"

# docker_run_rust runs an unprivileged rust container as the host user. The
# platform is pinned to linux/amd64 so the staged binary runs in the (amd64)
# court image even when the host has a cached arm64 variant of the tag.
docker_run_rust() {
  docker run --rm \
    --platform linux/amd64 \
    --network host ${CAPS_COMMON} \
    --cap-drop=ALL --security-opt=no-new-privileges \
    -u "$(id -u):$(id -g)" \
    -e HOME=/tmp -e TMPDIR=/tmp \
    -e CARGO_HOME=/src/.cache/cargo \
    -e CARGO_TARGET_DIR=/src/.cache/orfs-rs-target \
    -e CARGO_TERM_COLOR=never -e RUSTUP_TOOLCHAIN=1.99.0 \
    -v "${REPO_ROOT}":/src -w /src \
    "${RUST_IMAGE}" "$@"
}

# docker_run_rust_fuse runs the rust reference as root with /dev/fuse and
# CAP_SYS_ADMIN so `orfs mount` works, with caches in /tmp to avoid leaving
# root-owned files in the repository.
docker_run_rust_fuse() {
  docker run --rm \
    --platform linux/amd64 \
    --network host ${CAPS_COMMON} \
    --device /dev/fuse --cap-add SYS_ADMIN \
    --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    -e HOME=/tmp -e TMPDIR=/tmp \
    -e CARGO_HOME=/tmp/cargo -e CARGO_TARGET_DIR=/tmp/orfs-rs-target \
    -e CARGO_TERM_COLOR=never -e RUSTUP_TOOLCHAIN=1.99.0 \
    -v "${REPO_ROOT}":/src -w /src \
    "${RUST_IMAGE}" "$@"
}

# --- SeaweedFS (third comparator: its FUSE client, `weed mount`) -------------
# The head-to-head court's third system is a single-node SeaweedFS (master +
# volume + filer) served through its FUSE client. It is not built from source
# here: the pinned upstream image is pulled with an explicit --platform (the host
# may cache an arm64 variant of the tag) and the single static `weed` binary is
# extracted with docker create/docker cp. The image is never *run* on the host;
# the binary is bind-mounted read-only into the capped court container.
SEAWEEDFS_IMAGE=${ORFS_SEAWEEDFS_IMAGE:-chrislusf/seaweedfs:3.99}
SEAWEEDFS_DIR=${REPO_ROOT}/.cache/seaweedfs
SEAWEEDFS_BIN=${SEAWEEDFS_DIR}/weed
mkdir -p "${SEAWEEDFS_DIR}"

# seaweedfs_image_digest prints the pinned repo digest (manifest list) of the
# image, or "unavailable". It is recorded verbatim in the court receipt.
seaweedfs_image_digest() {
  docker image inspect --format '{{index .RepoDigests 0}}' "${SEAWEEDFS_IMAGE}" 2>/dev/null \
    || echo unavailable
}

# stage_seaweedfs extracts the pinned `weed` binary into .cache/seaweedfs/weed.
# It is idempotent and returns non-zero when the binary cannot be staged (no
# docker, no network to pull, ...). It never runs the image on the host.
stage_seaweedfs() {
  [ -x "${SEAWEEDFS_BIN}" ] && return 0
  have_docker || return 1
  docker pull --platform linux/amd64 "${SEAWEEDFS_IMAGE}" >/dev/null 2>&1 || true
  _cid=$(docker create --platform linux/amd64 "${SEAWEEDFS_IMAGE}" 2>/dev/null) || return 1
  if ! docker cp "${_cid}:/usr/bin/weed" "${SEAWEEDFS_BIN}" >/dev/null 2>&1; then
    docker rm "${_cid}" >/dev/null 2>&1 || true
    return 1
  fi
  docker rm "${_cid}" >/dev/null 2>&1 || true
  chmod +x "${SEAWEEDFS_BIN}"
  [ -x "${SEAWEEDFS_BIN}" ]
}

have_docker() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }
have_fuse()   { [ -e /dev/fuse ]; }

# The court image: the Dockerfile's `court` target (Go toolchain + fio + FUSE
# userspace + fs tools), used by the executable mounted courts.
COURT_IMAGE=${ORFS_COURT_IMAGE:-orfs-go-court:local}

# build_court_image builds the pinned court image. --platform keeps the run
# deterministic across hosts (a non-amd64 host needs QEMU/binfmt registered).
build_court_image() {
  docker build --platform linux/amd64 --target court -t "${COURT_IMAGE}" "${REPO_ROOT}"
}

# docker_run_court runs the court image as root with /dev/fuse and CAP_SYS_ADMIN
# so the mounted courts work, bind-mounting the repository's evidence/ directory
# so receipts persist on the host.
docker_run_court() {
  mkdir -p "${REPO_ROOT}/evidence"
  docker run --rm \
    --platform linux/amd64 \
    --network host ${CAPS_COMMON} \
    --device /dev/fuse --cap-add SYS_ADMIN \
    --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    -e HOME=/tmp -e "GOFLAGS=${GOFLAGS_VALUE}" \
    -v "${REPO_ROOT}/evidence":/evidence \
    -w /src \
    "${COURT_IMAGE}" "$@"
}

# docker_run_court_privileged is docker_run_court plus --privileged, for courts
# that create and mount loop devices (the cross-FS court's mkfs.ext4/xfs/btrfs).
docker_run_court_privileged() {
  mkdir -p "${REPO_ROOT}/evidence"
  docker run --rm \
    --platform linux/amd64 \
    --privileged \
    --network host ${CAPS_COMMON} \
    -e HOME=/tmp -e "GOFLAGS=${GOFLAGS_VALUE}" \
    -v "${REPO_ROOT}/evidence":/evidence \
    -w /src \
    "${COURT_IMAGE}" "$@"
}
