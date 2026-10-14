#!/bin/sh
# External-baseline court — runs *inside* the court container as root.
#
# Compares OrFS (mounted with --direct-io) against two references the kernel
# offers for free: the container filesystem (ext4/overlay) and a 512 MiB tmpfs.
# It drives fio 4K random reads at iodepth=1 for a single job, then repeats the
# same workload at numjobs=64 for the scaling receipt, and writes two labelled
# receipts with /out/fiosummary. Every stage reports honestly: a target that
# cannot run is named and skipped rather than silently counted as a pass.
#
# Env:
#   ORFS / FIOSUMMARY   binary paths         (default /out/orfs, /out/fiosummary)
#   ORFS_EVIDENCE_DIR   receipt root         (default /evidence, the repo's evidence/)
#   RUNTIME             fio seconds per arm  (default 8)
#   FILE_MIB            data file size, MiB  (default 64)
#   TMPFS_SIZE          tmpfs size           (default 512m)
set -eu

ORFS=${ORFS:-/out/orfs}
FIOSUMMARY=${FIOSUMMARY:-/out/fiosummary}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
RUNTIME=${RUNTIME:-8}
FILE_MIB=${FILE_MIB:-64}
TMPFS_SIZE=${TMPFS_SIZE:-512m}

if ! command -v fio >/dev/null 2>&1; then
  echo "baseline-court: SKIPPED (fio is not installed in this image)"
  exit 0
fi
if [ ! -x "$ORFS" ]; then
  echo "baseline-court: SKIPPED (missing $ORFS)"
  exit 0
fi
if [ ! -x "$FIOSUMMARY" ]; then
  echo "baseline-court: SKIPPED (missing $FIOSUMMARY)"
  exit 0
fi

mkdir -p "$EVID/performance"

WORK=$(mktemp -d)
MNT="$WORK/orfs-mnt"
VOL="$WORK/orfs-vol"
EXT4="$WORK/ext4"
TMPFS="$WORK/tmpfs"
mkdir -p "$MNT" "$EXT4" "$TMPFS"

mount_pid=""
mounted=0
cleanup() {
  if [ "$mounted" = 1 ]; then
    "$ORFS" unmount "$MNT" >/dev/null 2>&1 || umount "$MNT" 2>/dev/null || true
    mounted=0
  fi
  if [ -n "$mount_pid" ]; then
    kill "$mount_pid" 2>/dev/null || true
  fi
  umount "$TMPFS" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# --- targets --------------------------------------------------------------

# tmpfs reference. This is the one target that may refuse O_DIRECT (see run_one).
if ! mount -t tmpfs -o "size=$TMPFS_SIZE" tmpfs "$TMPFS" 2>/dev/null; then
  echo "baseline-court: WARN: tmpfs mount failed; the tmpfs arm will be skipped" >&2
  TMPFS=""
fi

# OrFS volume, mounted with --direct-io.
if "$ORFS" init "$VOL" --segment-size 8388608 >/dev/null 2>&1; then
  "$ORFS" mount "$VOL" "$MNT" --direct-io >/dev/null 2>&1 &
  mount_pid=$!
  sleep 2
  if mountpoint -q "$MNT"; then
    mounted=1
  else
    echo "baseline-court: WARN: OrFS mount did not come up; the orfs arm will be skipped" >&2
    kill "$mount_pid" 2>/dev/null || true
    mount_pid=""
  fi
else
  echo "baseline-court: WARN: orfs init failed; the orfs arm will be skipped" >&2
fi

seed() { # $1 = directory to hold data.bin
  if dd if=/dev/zero of="$1/data.bin" bs=1048576 count="$FILE_MIB" conv=fsync >/dev/null 2>&1; then
    return 0
  fi
  echo "baseline-court: WARN: could not seed $1/data.bin" >&2
  return 1
}
if [ "$mounted" = 1 ]; then seed "$MNT" || true; fi
seed "$EXT4" || true
if [ -n "$TMPFS" ]; then seed "$TMPFS" || true; fi

# --- fio workload ---------------------------------------------------------

# fio_randread OUT FILE NUMJOBS DIRECT — 4K random reads, iodepth=1, time_based.
fio_randread() {
  fio --name=randread --filename="$2" --rw=randread --bs=4k \
      --iodepth=1 --numjobs="$3" --direct="$4" --ioengine=psync \
      --runtime="$RUNTIME" --time_based --group_reporting \
      --output-format=json --output="$1" >/dev/null 2>&1
}

# run_one ARMSFILE LABEL FILE NUMJOBS OUT — run one arm and, on success, record
# the receipt as "label=out" in ARMSFILE. O_DIRECT the backing store refuses
# (tmpfs) is retried buffered, and that fallback is printed, never hidden.
run_one() {
  arms=$1
  label=$2
  file=$3
  jobs=$4
  out=$5
  if [ ! -e "$file" ]; then
    echo "baseline-court: WARN: $label: no data file; skipping arm" >&2
    return 0
  fi
  if ! fio_randread "$out" "$file" "$jobs" 1; then
    if fio_randread "$out" "$file" "$jobs" 0; then
      echo "baseline-court: NOTE: $label: O_DIRECT refused by the backing store; ran buffered I/O" >&2
    else
      echo "baseline-court: WARN: $label: fio failed (direct and buffered); skipping arm" >&2
      return 0
    fi
  fi
  if [ -s "$out" ]; then
    printf '%s=%s\n' "$label" "$out" >> "$arms"
  else
    echo "baseline-court: WARN: $label: fio produced no receipt" >&2
  fi
}

# summarise ARMSFILE SCHEMA OUT — turn the collected fio reports into a receipt.
summarise() {
  arms=$1
  schema=$2
  out=$3
  if [ ! -s "$arms" ]; then
    echo "baseline-court: WARN: no fio receipts for $schema; not writing $out" >&2
    return 0
  fi
  # Each line is a "label=path" argument; mktemp paths contain no whitespace, so
  # word splitting is intended here.
  if "$FIOSUMMARY" -schema "$schema" -o "$out" $(cat "$arms"); then
    echo "baseline-court: wrote $out"
  else
    echo "baseline-court: WARN: fiosummary failed for $schema" >&2
  fi
}

# Single-job baseline (numjobs=1).
: > "$WORK/baseline-arms"
run_one "$WORK/baseline-arms" orfs  "$MNT/data.bin"   1 "$WORK/fio-orfs-1.json"
run_one "$WORK/baseline-arms" ext4  "$EXT4/data.bin"  1 "$WORK/fio-ext4-1.json"
if [ -n "$TMPFS" ]; then
  run_one "$WORK/baseline-arms" tmpfs "$TMPFS/data.bin" 1 "$WORK/fio-tmpfs-1.json"
fi

# Scaling receipt (numjobs=64).
: > "$WORK/scale-arms"
run_one "$WORK/scale-arms" orfs  "$MNT/data.bin"   64 "$WORK/fio-orfs-64.json"
run_one "$WORK/scale-arms" ext4  "$EXT4/data.bin"  64 "$WORK/fio-ext4-64.json"
if [ -n "$TMPFS" ]; then
  run_one "$WORK/scale-arms" tmpfs "$TMPFS/data.bin" 64 "$WORK/fio-tmpfs-64.json"
fi

summarise "$WORK/baseline-arms" orfs.performance.baseline.v1 "$EVID/performance/baseline.json"
summarise "$WORK/scale-arms"    orfs.performance.scale.v1    "$EVID/performance/scale.json"

wrote=0
if [ -s "$EVID/performance/baseline.json" ]; then wrote=$((wrote + 1)); fi
if [ -s "$EVID/performance/scale.json" ]; then wrote=$((wrote + 1)); fi
if [ "$wrote" -eq 0 ]; then
  echo "baseline-court: FAIL: no fio arm produced a receipt" >&2
  exit 1
fi
echo "baseline-court: PASS ($wrote receipt(s) under $EVID/performance)"
