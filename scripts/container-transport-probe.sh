#!/bin/sh
# Run just the FUSE-over-io_uring negotiation probe in the capped, /dev/fuse
# container. Cheap: it builds only the probe, not the court image.
#
# The probe attempts the real FUSE INIT handshake (control, then requesting
# FUSE_OVER_IO_URING) and prints what the kernel did. It is the empirical
# evidence behind docs/TRANSPORT.md; the full measured court is
# scripts/container-transport.sh.
set -eu
. "$(dirname "$0")/lib.sh"

if ! have_docker; then
  echo "container-transport-probe: SKIPPED (no usable docker daemon)"
  exit 0
fi
if ! have_fuse; then
  echo "container-transport-probe: SKIPPED (no /dev/fuse on the host)"
  exit 0
fi

docker_run_fuse sh -c 'go build -o /tmp/fuseuringprobe ./cmd/fuseuringprobe && /tmp/fuseuringprobe'
echo "container-transport-probe: PASS"
