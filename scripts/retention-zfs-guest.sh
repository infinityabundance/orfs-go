#!/bin/sh
# Real-OpenZFS retention workload — runs INSIDE the KVM micro-VM guest.
#
# This is the guest half of scripts/retention-zfs-court.sh. The guest kernel is
# an Ubuntu generic kernel, whose `linux-modules-extra` ships the out-of-tree
# OpenZFS module, so `zfs`/`zpool` work *inside the guest* on a file-backed vdev
# without touching the host kernel. The same `cmd/retentionbench` driver the
# Btrfs court uses drives both arms inside this one VM, so the two systems run
# on the same virtual hardware, the same disk and the same CPU.
#
# Everything is written to the serial console, which the container captures:
#   ORFSKVM-LOG-BEGIN .. ORFSKVM-LOG-END        the human-readable log
#   ORFSKVM-SETUP-OK | ORFSKVM-SETUP-FAIL ...   whether both arms came up
#   ORFSKVM-COUNTS zfs_ran=.. zfs_failed=.. orfs_ran=.. orfs_failed=.. orfs_status=..
#   ORFSKVM-RESULTS-BEGIN .. ORFSKVM-RESULTS-END   base64 of the results tar.gz
#
# Env (all required unless a default is shown):
#   HOST_URL              base URL of the staging server (http://10.0.2.2:PORT)
#   SEGMENT_SIZE IMG_MIB POOL EPOCHS WRITERS RECORDS_PER_EPOCH RECORD_BYTES
#   APPEND_SECONDS READ_SECONDS READERS BLOCK_SIZE READ_EPOCHS SEED ORFS_ARM
set -u
LC_ALL=C
export LC_ALL

HOST_URL=${HOST_URL:-http://10.0.2.2:8080}
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
# Bound the ZFS ARC so the guest stays inside its (small) memory allocation.
ARC_MAX=${ARC_MAX:-134217728}

# Everything below goes to the serial console, which is how the container reads
# the log, the counts and the results back out of the VM.
exec >>/dev/ttyS0 2>&1
# Keep kernel chatter off the serial console so the results block below cannot be
# interleaved with unrelated kernel log lines (only emergencies reach it now).
dmesg -n 1 2>/dev/null || true

echo "ORFSKVM-LOG-BEGIN"
echo "ORFSKVM-GUEST-KERNEL $(uname -r)"

ORFS=/usr/local/bin/orfs
BENCH=/usr/local/bin/retentionbench

# Fail fast and power off, so a broken guest never lingers until the boot
# timeout: the container sees SETUP-FAIL and reports a precise SKIPPED.
finish_fail() {
  echo "ORFSKVM-SETUP-FAIL $1"
  sync
  poweroff -f
  exit 1
}

# Fetch the shared driver (and the OrFS-GO binary) from the container's staging
# server over the QEMU user-mode network, so the guest runs the SAME
# cmd/retentionbench the Btrfs court uses.
mkdir -p /usr/local/bin
for _b in retentionbench orfs; do
  curl -sf -m 120 -o "/usr/local/bin/$_b" "$HOST_URL/$_b" || finish_fail "could not fetch $_b from $HOST_URL"
  chmod 0755 "/usr/local/bin/$_b"
done
echo "ORFSKVM-FETCH-OK"

WORK=/work
LIVE_MNT="$WORK/zfs-live"
LIVE_DS="$POOL/live"
SNAPDIR_REL=".zfs/snapshot"
ZFS_IMG="$WORK/zfs-vdev.img"
ORFS_IMG="$WORK/orfs.img"
ORFS_BMNT="$WORK/orfs-btrfs"
ORFS_MNT="$WORK/orfs-mnt"
ORFS_VOL="$ORFS_BMNT/vol"
RESULTS="$WORK/results"

mkdir -p "$WORK" "$LIVE_MNT" "$RESULTS"

command -v zpool >/dev/null 2>&1 || finish_fail "zpool missing"
command -v zfs   >/dev/null 2>&1 || finish_fail "zfs missing"
modprobe zfs >/dev/null 2>&1 || true
if ! grep -qw zfs /proc/modules; then
  finish_fail "zfs kernel module not loaded"
fi
echo "ORFSKVM-ZFS-VERSION $(zpool version 2>&1 | tr '\n' ' ' | sed 's/  */ /g')"

# Bound the ARC so a 1 GiB guest cannot spend most of its RAM on cache.
[ -w /sys/module/zfs/parameters/zfs_arc_max ] && echo "$ARC_MAX" > /sys/module/zfs/parameters/zfs_arc_max

# --- bring up the OpenZFS arm (file-backed vdev; no host involvement) --------
if ! dd if=/dev/zero of="$ZFS_IMG" bs=1M count="$IMG_MIB" status=none; then
  finish_fail "could not create the vdev image"
fi
if ! zpool create -f -m none "$POOL" "$ZFS_IMG"; then
  finish_fail "zpool create failed"
fi
if ! zfs create -o mountpoint="$LIVE_MNT" "$LIVE_DS"; then
  finish_fail "zfs create failed"
fi
echo "ORFSKVM-POOL-VERSION $(zpool get -H -o value version "$POOL" 2>/dev/null || echo unknown)"

# --- bring up the OrFS-GO arm (loop-mounted btrfs backing, as in the Btrfs ---
# court, so the OrFS-GO rows are produced the same way) -----------------------
ORFS_STATUS="not_attempted"
ORFS_LOOP=""
if [ "$ORFS_ARM" = 1 ] && [ -x "$ORFS" ]; then
  mkdir -p "$ORFS_BMNT" "$ORFS_MNT"
  if dd if=/dev/zero of="$ORFS_IMG" bs=1M count="$IMG_MIB" status=none \
     && ORFS_LOOP=$(losetup -fP --show "$ORFS_IMG" 2>/dev/null) \
     && [ -n "$ORFS_LOOP" ] \
     && mkfs.btrfs -q -f "$ORFS_LOOP" >/dev/null 2>&1 \
     && mount "$ORFS_LOOP" "$ORFS_BMNT" >/dev/null 2>&1; then
    if ! "$ORFS" init "$ORFS_VOL" --segment-size "$SEGMENT_SIZE" >"$WORK/orfs-init.err" 2>&1; then
      ORFS_STATUS="init_failed"
    else
      ORFS_GROUP_COMMIT= "$ORFS" mount "$ORFS_VOL" "$ORFS_MNT" --direct-io >"$WORK/orfs-mount.err" 2>&1 &
      ORFS_PID=$!
      _w=0
      while [ "$_w" -lt 100 ]; do mountpoint -q "$ORFS_MNT" && break; _w=$((_w + 1)); sleep 0.1; done
      if mountpoint -q "$ORFS_MNT"; then
        ORFS_STATUS="mounted"
      else
        ORFS_STATUS="mount_failed"
        kill -9 "$ORFS_PID" 2>/dev/null || true
      fi
    fi
  else
    ORFS_STATUS="loop_unavailable"
  fi
fi

echo "ORFSKVM-SETUP-OK orfs_status=$ORFS_STATUS"

ZFS_RAN=0; ZFS_FAILED=0
ORFS_RAN=0; ORFS_FAILED=0

should_read() {
  [ "$READ_EPOCHS" = all ] && return 0
  for _e in $READ_EPOCHS; do [ "$_e" = "$1" ] && return 0; done
  return 1
}

# --- prepare writer files outside the measured windows ----------------------
if ! "$BENCH" prepare -dir "$LIVE_MNT" -prefix append -writers "$WRITERS" >/dev/null 2>&1; then
  finish_fail "prepare on the OpenZFS arm failed"
fi
[ "$ORFS_STATUS" = mounted ] && "$BENCH" prepare -dir "$ORFS_MNT" -prefix append -writers "$WRITERS" >/dev/null 2>&1

# --- the epoch loop (identical in shape to scripts/retention-btrfs-court.sh) --
e=0
while [ "$e" -lt "$EPOCHS" ]; do
  echo "ORFSKVM-EPOCH $e"

  if "$BENCH" append -dir "$LIVE_MNT" -system OpenZFS -prefix append \
      -writers "$WRITERS" -records "$RECORDS_PER_EPOCH" -record-bytes "$RECORD_BYTES" \
      -seconds "$APPEND_SECONDS" -epoch "$e" -seed "$SEED" \
      -out "$RESULTS/zfs-append-e$e.json" >/dev/null 2>&1; then
    ZFS_RAN=$((ZFS_RAN + 1))
  else
    ZFS_FAILED=$((ZFS_FAILED + 1))
  fi

  "$BENCH" space -kind zfs -system OpenZFS -dataset "$LIVE_DS" \
    -epoch "$e" -state current -out "$RESULTS/zfs-current-e$e.json" >/dev/null 2>&1 \
    || echo "ORFSKVM-WARN zfs current space $e failed"

  sync
  if "$BENCH" snapshot -kind zfs -live "$LIVE_DS" -dest "snap-$e" \
      -system OpenZFS -epoch "$e" -out "$RESULTS/zfs-snap-e$e.json" >/dev/null 2>&1; then
    :
  else
    ZFS_FAILED=$((ZFS_FAILED + 1))
  fi

  "$BENCH" space -kind zfs -system OpenZFS -dataset "$LIVE_DS@snap-$e" \
    -epoch "$e" -state snapshot -out "$RESULTS/zfs-snapshot-e$e.json" >/dev/null 2>&1 \
    || echo "ORFSKVM-WARN zfs snapshot space $e failed"

  if should_read "$e"; then
    if "$BENCH" read -dir "$LIVE_MNT" -system OpenZFS -state current -epoch "$e" \
        -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
        -seed "$SEED" -out "$RESULTS/zfs-read-current-e$e.json" >/dev/null 2>&1; then
      ZFS_RAN=$((ZFS_RAN + 1))
    else
      ZFS_FAILED=$((ZFS_FAILED + 1))
    fi
    if "$BENCH" read -dir "$LIVE_MNT/$SNAPDIR_REL/snap-$e" -system OpenZFS -state snapshot -epoch "$e" \
        -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
        -seed "$SEED" -out "$RESULTS/zfs-read-snapshot-e$e.json" >/dev/null 2>&1; then
      ZFS_RAN=$((ZFS_RAN + 1))
    else
      ZFS_FAILED=$((ZFS_FAILED + 1))
    fi
  fi

  if [ "$ORFS_STATUS" = mounted ]; then
    if "$BENCH" append -dir "$ORFS_MNT" -system OrFS-GO -prefix append \
        -writers "$WRITERS" -records "$RECORDS_PER_EPOCH" -record-bytes "$RECORD_BYTES" \
        -seconds "$APPEND_SECONDS" -epoch "$e" -seed "$SEED" \
        -out "$RESULTS/orfs-append-e$e.json" >/dev/null 2>&1; then
      ORFS_RAN=$((ORFS_RAN + 1))
    else
      ORFS_FAILED=$((ORFS_FAILED + 1))
    fi
    "$BENCH" space -kind dir -system OrFS-GO -subvol "$ORFS_MNT" -physical-dir "$ORFS_VOL/or.log" \
      -epoch "$e" -state current -out "$RESULTS/orfs-current-e$e.json" >/dev/null 2>&1 \
      || echo "ORFSKVM-WARN orfs space $e failed"
    if should_read "$e"; then
      if "$BENCH" read -dir "$ORFS_MNT" -system OrFS-GO -state current -epoch "$e" \
          -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
          -seed "$SEED" -out "$RESULTS/orfs-read-current-e$e.json" >/dev/null 2>&1; then
        ORFS_RAN=$((ORFS_RAN + 1))
      else
        ORFS_FAILED=$((ORFS_FAILED + 1))
      fi
    fi
  fi

  e=$((e + 1))
done

[ "$ORFS_STATUS" = mounted ] && { "$ORFS" unmount "$ORFS_MNT" >/dev/null 2>&1 || umount -l "$ORFS_MNT" 2>/dev/null; }
zpool export "$POOL" >/dev/null 2>&1 || true

echo "ORFSKVM-COUNTS zfs_ran=$ZFS_RAN zfs_failed=$ZFS_FAILED orfs_ran=$ORFS_RAN orfs_failed=$ORFS_FAILED orfs_status=$ORFS_STATUS"
echo "ORFSKVM-RESULTS-BEGIN"
dmesg -n 1 2>/dev/null || true
# Each base64 line is prefixed with R: so the container can pick exactly the
# result lines out of the serial transcript even if anything else is logged.
tar -C "$RESULTS" -czf - . 2>/dev/null | base64 | sed 's/^/R:/'
echo "ORFSKVM-RESULTS-END"
echo "ORFSKVM-LOG-END"
sync
poweroff -f
