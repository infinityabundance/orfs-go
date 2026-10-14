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
