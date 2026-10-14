# Shared environment for the OrFS-GO container workflow.
#
# Every compilation, test, fuzz run, and mount experiment runs inside a
# digest-pinned container with resource caps. One container runs at a time.
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GO_VERSION=${GO_VERSION:-1.26}
# Debian-based image: it ships gcc, so `go test -race` (which needs cgo) works.
IMAGE="golang:${GO_VERSION}-bookworm"
MEM_LIMIT=${ORFS_MEM_LIMIT:-4g}
MEMSWAP_LIMIT=${ORFS_MEMSWAP_LIMIT:-4g}
CPUS=${ORFS_CPUS:-8}

# Caches live inside the repo so they are bind-mounted and reused across runs.
mkdir -p "${REPO_ROOT}/.cache/gomod" "${REPO_ROOT}/.cache/gobuild"

# docker_run runs an unprivileged container. It does not use exec so one script
# can drive several containers sequentially.
docker_run() {
  docker run --rm \
    --network host \
    --memory "${MEM_LIMIT}" --memory-swap "${MEMSWAP_LIMIT}" --cpus "${CPUS}" \
    --cap-drop=ALL --security-opt=no-new-privileges \
    -u "$(id -u):$(id -g)" \
    -e HOME=/tmp -e GOFLAGS=-mod=mod \
    -e GOMODCACHE=/src/.cache/gomod -e GOCACHE=/src/.cache/gobuild \
    -v "${REPO_ROOT}":/src -w /src \
    "${IMAGE}" "$@"
}

# docker_run_fuse adds /dev/fuse and CAP_SYS_ADMIN for real mounts. The container
# runs as root because the kernel grants FUSE mount privileges to root only here.
docker_run_fuse() {
  	# Root (for mount privileges), so caches live in /tmp to avoid leaving
    	# root-owned files in the bind-mounted repository.
    	docker run --rm \
    		--network host \
    		--memory "${MEM_LIMIT}" --memory-swap "${MEMSWAP_LIMIT}" --cpus "${CPUS}" \
    		--device /dev/fuse --cap-add SYS_ADMIN \
    		--security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    		-e HOME=/tmp -e GOFLAGS=-mod=mod \
    		-e GOMODCACHE=/tmp/gomod -e GOCACHE=/tmp/gobuild \
    		-v "${REPO_ROOT}":/src -w /src \
    		"${IMAGE}" "$@"
}

have_docker() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }
have_fuse()   { [ -e /dev/fuse ]; }

# The court image: the Dockerfile's `court` target (Go toolchain + fio + FUSE
# userspace), used by the executable baseline and fsstress courts.
COURT_IMAGE=${ORFS_COURT_IMAGE:-orfs-go-court:local}

# build_court_image builds the pinned court image. --platform keeps the run
# deterministic across hosts (a non-amd64 host needs QEMU/binfmt registered).
build_court_image() {
  docker build --platform linux/amd64 --target court -t "${COURT_IMAGE}" "${REPO_ROOT}"
}

# docker_run_court runs the court image as root with /dev/fuse and CAP_SYS_ADMIN
# so the mounted courts work, bind-mounting the repository's evidence/ directory
# so receipts persist on the host. It does not use exec so one script can drive
# several courts sequentially.
docker_run_court() {
  mkdir -p "${REPO_ROOT}/evidence"
  docker run --rm \
    --platform linux/amd64 \
    --network host \
    --memory "${MEM_LIMIT}" --memory-swap "${MEMSWAP_LIMIT}" --cpus "${CPUS}" \
    --device /dev/fuse --cap-add SYS_ADMIN \
    --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    -e HOME=/tmp \
    -v "${REPO_ROOT}/evidence":/evidence \
    -w /src \
    "${COURT_IMAGE}" "$@"
}
