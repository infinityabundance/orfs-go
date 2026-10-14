#!/bin/sh
# Build the Rust reference `orfs` binary in a pinned, resource-capped rust
# container and stage it for the cross-language court.
#
# The reference lives at .reference/OrFS (crate `orfs` v0.2.0, rust-toolchain
# 1.99.0). Building it requires the crates.io registry index and the `ekbloom`
# crate, so the container gets `--network host`. Every artifact is redirected to
# .cache/ (CARGO_TARGET_DIR), so a build never writes into .reference/.
#
# Exit codes:
#   0  built; the binary is staged at .cache/orfs-rs/orfs
#   3  SKIPPED (no usable docker daemon) — the caller must not treat this as pass
#   1  the build failed; the exact cargo output is on stderr
#
# Usage: sh scripts/container-rust-build.sh
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-rust-build: SKIPPED (no usable docker daemon)" >&2
  exit 3
fi
if [ ! -f "${REPO_ROOT}/.reference/OrFS/Cargo.toml" ]; then
  echo "container-rust-build: SKIPPED (no Rust reference at .reference/OrFS)" >&2
  exit 3
fi

docker_run_rust sh -c '
  set -eu
  cargo build --release --locked --manifest-path /src/.reference/OrFS/Cargo.toml
  cp /src/.cache/orfs-rs-target/release/orfs /src/.cache/orfs-rs/orfs
  chmod 0755 /src/.cache/orfs-rs/orfs
  echo "container-rust-build: staged $(/src/.cache/orfs-rs/orfs --version 2>&1 || true)"
'
echo "container-rust-build: PASS — $(du -h "${ORFS_RS_BIN}" 2>/dev/null | awk '{print $1}') binary at .cache/orfs-rs/orfs"
