#!/bin/sh
# Real-OpenZFS retention court: the KVM micro-VM orchestrator (container side).
#
# Runs INSIDE the privileged court container, which has /dev/kvm. It boots a
# pinned Ubuntu cloud image whose generic kernel ships the out-of-tree OpenZFS
# module, serves the shared driver and the guest workload over HTTP, runs the
# guest, and reads the guest's per-arm measurement JSON back over the serial
# console. The host kernel is never touched: OpenZFS runs *inside the guest*.
#
# Usage: sh scripts/retention-zfs-vm.sh <vm-work-dir>
# Reads the workload knobs from the environment (see scripts/retention-zfs-court.sh).
# Writes into <vm-work-dir>: results/*.json, facts, serial.log, guest.log.
#
# Exit codes:
#   0  the guest ran and its results were extracted
#   2  the KVM path is unavailable here (no /dev/kvm, no qemu/cloud-localds/
#      busybox, or the pinned image could not be fetched) -> the caller SKIPs
#   3  the guest booted but could not bring up the comparison (verbatim in
#      guest.log) -> the caller SKIPs with that evidence
#   1  the guest reported success but its results were not usable -> FAIL
set -eu
LC_ALL=C
export LC_ALL

VMDIR=${1:?usage: retention-zfs-vm.sh <vm-work-dir>}
mkdir -p "$VMDIR"

SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}
IMG_MIB=${IMG_MIB:-512}
POOL=${POOL:-orfsret}
EPOCHS=${EPOCHS:-8}
WRITERS=${WRITERS:-1}
RECORDS_PER_EPOCH=${RECORDS_PER_EPOCH:-256}
RECORD_BYTES=${RECORD_BYTES:-4096}
APPEND_SECONDS=${APPEND_SECONDS:-30}
READ_SECONDS=${READ_SECONDS:-1}
READERS=${READERS:-1}
BLOCK_SIZE=${BLOCK_SIZE:-4096}
READ_EPOCHS=${READ_EPOCHS:-all}
SEED=${SEED:-1}
ORFS_ARM=${ORFS_ARM:-1}

# The pinned guest image (Ubuntu 24.04 minimal cloud image). The URL and digest
# are recorded in the receipt; a mismatched or missing image is re-fetched, so
# the guest is reproducible from this script alone.
ZFS_KVM_IMAGE=${ZFS_KVM_IMAGE:-/kvm/ubuntu-24.04-minimal-cloudimg-amd64.img}
ZFS_KVM_IMAGE_URL=${ZFS_KVM_IMAGE_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img}
ZFS_KVM_IMAGE_SHA256=${ZFS_KVM_IMAGE_SHA256:-2f119f50bf29fe40e73b69ae2a736e8210d8146796f036e0324315b6b3a777df}
VM_MEM_MIB=${VM_MEM_MIB:-1024}
VM_CPUS=${VM_CPUS:-2}
VM_PORT=${VM_PORT:-8080}
VM_BOOT_SECONDS=${VM_BOOT_SECONDS:-600}

ORFS_BIN_DIR=${ORFS_BIN_DIR:-/out}
SRC=${SRC:-/src}
GUEST_SCRIPT="$SRC/scripts/retention-zfs-guest.sh"

unavailable() { echo "retention-zfs-vm: unavailable: $1" >&2; exit 2; }
setup_failed() {
  echo "retention-zfs-vm: the guest could not bring up the comparison: $1" >&2
  [ -f "$VMDIR/guest.log" ] && sed -n '/ORFSKVM-LOG-BEGIN/,/ORFSKVM-LOG-END/p' "$VMDIR/guest.log" >&2
  exit 3
}

# --- preconditions ----------------------------------------------------------
[ -e /dev/kvm ] || unavailable "no /dev/kvm"
command -v qemu-system-x86_64 >/dev/null 2>&1 || unavailable "qemu-system-x86_64 not installed"
command -v cloud-localds >/dev/null 2>&1 || unavailable "cloud-localds (cloud-image-utils) not installed"
command -v busybox >/dev/null 2>&1 || unavailable "busybox (static) not installed"
[ -x "$ORFS_BIN_DIR/retentionbench" ] || unavailable "$ORFS_BIN_DIR/retentionbench not built"
[ -f "$GUEST_SCRIPT" ] || unavailable "$GUEST_SCRIPT not found"

# --- the pinned guest image (bounded download, digest-verified) -------------
image_ok() {
  [ -f "$ZFS_KVM_IMAGE" ] || return 1
  _got=$(sha256sum "$ZFS_KVM_IMAGE" | cut -d' ' -f1)
  [ "$_got" = "$ZFS_KVM_IMAGE_SHA256" ]
}
if ! image_ok; then
  echo "retention-zfs-vm: fetching the pinned guest image (~252 MiB) into $ZFS_KVM_IMAGE"
  mkdir -p "$(dirname "$ZFS_KVM_IMAGE")"
  command -v curl >/dev/null 2>&1 || unavailable "curl not installed (needed to fetch the guest image)"
  rm -f "$ZFS_KVM_IMAGE.tmp"
  curl -fSL --max-time 900 -o "$ZFS_KVM_IMAGE.tmp" "$ZFS_KVM_IMAGE_URL" \
    || unavailable "could not download the guest image from $ZFS_KVM_IMAGE_URL"
  _got=$(sha256sum "$ZFS_KVM_IMAGE.tmp" | cut -d' ' -f1)
  [ "$_got" = "$ZFS_KVM_IMAGE_SHA256" ] || { rm -f "$ZFS_KVM_IMAGE.tmp"; unavailable "guest image digest mismatch (got $_got)"; }
  mv -f "$ZFS_KVM_IMAGE.tmp" "$ZFS_KVM_IMAGE"
fi

# --- stage the driver + guest workload for the HTTP server ------------------
STAGE="$VMDIR/stage"
rm -rf "$STAGE"; mkdir -p "$STAGE"
cp "$ORFS_BIN_DIR/retentionbench" "$STAGE/retentionbench"
[ -x "$ORFS_BIN_DIR/orfs" ] && cp "$ORFS_BIN_DIR/orfs" "$STAGE/orfs"
cp "$GUEST_SCRIPT" "$STAGE/retention-zfs-guest.sh"
chmod 0755 "$STAGE"/*

# --- the guest's cloud-init: install ZFS + FUSE + btrfs, then run the workload
cat > "$VMDIR/user-data" <<EOF
#cloud-config
package_update: true
package_upgrade: false
packages:
  - zfsutils-linux
  - fuse3
  - btrfs-progs
  - curl
runcmd:
  - |
    curl -sf -m 120 -o /tmp/rguest.sh http://10.0.2.2:${VM_PORT}/retention-zfs-guest.sh
    HOST_URL=http://10.0.2.2:${VM_PORT} SEGMENT_SIZE=${SEGMENT_SIZE} IMG_MIB=${IMG_MIB} POOL=${POOL} \
      EPOCHS=${EPOCHS} WRITERS=${WRITERS} RECORDS_PER_EPOCH=${RECORDS_PER_EPOCH} RECORD_BYTES=${RECORD_BYTES} \
      APPEND_SECONDS=${APPEND_SECONDS} READ_SECONDS=${READ_SECONDS} READERS=${READERS} BLOCK_SIZE=${BLOCK_SIZE} \
      READ_EPOCHS="${READ_EPOCHS}" SEED=${SEED} ORFS_ARM=${ORFS_ARM} sh /tmp/rguest.sh
EOF
cat > "$VMDIR/meta-data" <<EOF
instance-id: orfs-retention-zfs-kvm
local-hostname: orfs-zfs-kvm
EOF

# --- the guest overlay (copy-on-write over the pinned base image) -----------
rm -f "$VMDIR/overlay.qcow2"
qemu-img create -f qcow2 -b "$ZFS_KVM_IMAGE" -F qcow2 "$VMDIR/overlay.qcow2" >/dev/null
cloud-localds "$VMDIR/seed.img" "$VMDIR/user-data" "$VMDIR/meta-data"

# --- serve the staged binaries/workload to the guest (slirp: 10.0.2.2) ------
busybox httpd -f -p "$VM_PORT" -h "$STAGE" >"$VMDIR/httpd.log" 2>&1 &
HTTPD_PID=$!
cleanup() { kill "$HTTPD_PID" 2>/dev/null || true; rm -rf "$STAGE" 2>/dev/null || true; }
trap cleanup EXIT INT TERM
sleep 1

# --- boot the micro-VM ------------------------------------------------------
QEMU_VERSION=$(qemu-system-x86_64 --version 2>/dev/null | head -1)
rm -f "$VMDIR/serial.log"
echo "retention-zfs-vm: booting the guest (${VM_MEM_MIB} MiB, ${VM_CPUS} vcpu, image sha256 ${ZFS_KVM_IMAGE_SHA256})"

set +e
timeout "$VM_BOOT_SECONDS" qemu-system-x86_64 \
  -accel kvm -cpu host -smp "$VM_CPUS" -m "$VM_MEM_MIB" \
  -drive file="$VMDIR/overlay.qcow2",if=virtio,format=qcow2,cache=none \
  -drive file="$VMDIR/seed.img",if=virtio,format=raw,readonly=on \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  -display none -serial "file:$VMDIR/serial.log" -no-reboot
QEMU_RC=$?
set -e
kill "$HTTPD_PID" 2>/dev/null || true
trap - EXIT INT TERM

# Normalise the transcript ONCE, on capture: the serial console emits CRLF line
# endings, and a stray \r inside an extracted value corrupts both the base64
# results payload and any receipt field built from it (it is an invalid JSON
# control character). Everything downstream reads the CR-free guest.log.
tr -d '\r' < "$VMDIR/serial.log" > "$VMDIR/guest.log" 2>/dev/null || true
echo "retention-zfs-vm: qemu exited rc=$QEMU_RC"

if ! grep -q 'ORFSKVM-SETUP-OK' "$VMDIR/guest.log" 2>/dev/null; then
  if [ "$QEMU_RC" = 124 ]; then
    setup_failed "the guest did not finish within ${VM_BOOT_SECONDS}s"
  fi
  setup_failed "the guest never reported ORFSKVM-SETUP-OK (boot or ZFS bring-up failed)"
fi

# --- extract the per-arm results from the serial transcript -----------------
RESULTS="$VMDIR/results"
rm -rf "$RESULTS"; mkdir -p "$RESULTS"
# The serial console emits CRLF line endings; strip BOTH \r and \n, or the
# carriage returns land inside the base64 stream and corrupt the payload.
sed -n '/ORFSKVM-RESULTS-BEGIN/,/ORFSKVM-RESULTS-END/p' "$VMDIR/guest.log" \
  | sed -n 's/^R://p' | tr -d '\r\n' > "$VMDIR/results.b64"
if ! base64 -d < "$VMDIR/results.b64" | tar -xzf - -C "$RESULTS" 2>/dev/null; then
  echo "retention-zfs-vm: FAIL: the guest reported success but its results block did not decode" >&2
  exit 1
fi
N=$(ls "$RESULTS" 2>/dev/null | wc -l | tr -d ' ')
[ "$N" -gt 0 ] || { echo "retention-zfs-vm: FAIL: the results block was empty" >&2; exit 1; }

# --- facts for the receipt --------------------------------------------------
GUEST_KERNEL=$(sed -n 's/^ORFSKVM-GUEST-KERNEL //p' "$VMDIR/guest.log" | tail -1)
ZFS_VERSION=$(sed -n 's/^ORFSKVM-ZFS-VERSION //p' "$VMDIR/guest.log" | tail -1)
POOL_VERSION=$(sed -n 's/^ORFSKVM-POOL-VERSION //p' "$VMDIR/guest.log" | tail -1)
COUNTS=$(sed -n 's/^ORFSKVM-COUNTS //p' "$VMDIR/guest.log" | tail -1)
cat > "$VMDIR/facts" <<EOF
GUEST_KERNEL='$GUEST_KERNEL'
ZFS_VERSION='$ZFS_VERSION'
POOL_VERSION='$POOL_VERSION'
COUNTS='$COUNTS'
QEMU_VERSION='$QEMU_VERSION'
IMAGE_URL='$ZFS_KVM_IMAGE_URL'
IMAGE_SHA256='$ZFS_KVM_IMAGE_SHA256'
VM_MEM_MIB='$VM_MEM_MIB'
VM_CPUS='$VM_CPUS'
EOF

echo "retention-zfs-vm: guest produced $N result file(s); counts: ${COUNTS:-none}"
exit 0
