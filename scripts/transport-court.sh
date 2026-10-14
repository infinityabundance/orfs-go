#!/bin/sh
# Transport court — runs *inside* the court container as root.
#
# Part 1 (empirical): runs fuseuringprobe, which attempts the real FUSE INIT
# handshake against the running kernel — once as a control, once requesting
# FUSE_OVER_IO_URING — and records exactly what the kernel did. This is the
# evidence for whether F2 (FUSE-over-io_uring) is reachable at all.
#
# Part 2 (measured): drives the F0 transport the way go-fuse actually serves it
# (multi-reader) and the f0-single serving loop, 4K random reads at numjobs=1
# and 64, sampling the kernel FUSE connection's pending-request backlog, and
# writes evidence/performance/transport.json (orfs.performance.transport.v1).
#
# A target that cannot run is named and skipped, never counted as a pass.
#
# Env:
#   ORFS / FIOSUMMARY / TRANSPORTSUMMARY / PROBE   binary paths
#   ORFS_EVIDENCE_DIR                              receipt root (default /evidence)
#   RUNTIME                                        fio seconds per arm (default 8)
#   FILE_MIB                                       data file size, MiB (default 64)
set -eu

ORFS=${ORFS:-/out/orfs}
TRANSPORTSUMMARY=${TRANSPORTSUMMARY:-/out/transportsummary}
PROBE=${PROBE:-/out/fuseuringprobe}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
RUNTIME=${RUNTIME:-8}
FILE_MIB=${FILE_MIB:-64}

if ! command -v fio >/dev/null 2>&1; then
  echo "transport-court: SKIPPED (fio is not installed in this image)"
  exit 0
fi
if [ ! -x "$ORFS" ] || [ ! -x "$TRANSPORTSUMMARY" ]; then
  echo "transport-court: SKIPPED (missing $ORFS or $TRANSPORTSUMMARY)"
  exit 0
fi

mkdir -p "$EVID/performance"

# The container has no fusectl mount by default, so /sys/fs/fuse/connections is
# empty; mounting it (we hold CAP_SYS_ADMIN) exposes the FUSE connection's
# pending-request backlog. Best effort: without it the counters are simply named
# as absent, never invented.
mkdir -p /sys/fs/fuse/connections
mount -t fusectl fusectl /sys/fs/fuse/connections 2>/dev/null || true

WORK=$(mktemp -d)
MNT="$WORK/orfs-mnt"
VOL="$WORK/orfs-vol"
mkdir -p "$MNT"
PROBE_OUT="$WORK/probe.json"
ARMS=""
COUNTER_ARGS=""

mount_pid=""
mounted=0
cleanup() {
  if [ "$mounted" = 1 ]; then
    umount "$MNT" 2>/dev/null || true
    mounted=0
  fi
  if [ -n "$mount_pid" ]; then
    kill "$mount_pid" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# --- Part 1: the FUSE-over-io_uring negotiation probe ----------------------

if [ -x "$PROBE" ]; then
  echo "transport-court: running FUSE INIT negotiation probe"
  if "$PROBE" -o "$PROBE_OUT" 2>"$WORK/probe.err"; then
    cat "$PROBE_OUT"
  else
    echo "transport-court: NOTE: probe reported an error (recorded):" >&2
    cat "$WORK/probe.err" >&2 || true
    [ -s "$PROBE_OUT" ] && cat "$PROBE_OUT"
  fi
else
  echo "transport-court: WARN: $PROBE missing; the probe section will be absent" >&2
fi

# --- Part 2: measured F0 vs f0-single --------------------------------------

fio_randread() { # OUT FILE NUMJOBS DIRECT
  fio --name=randread --filename="$2" --rw=randread --bs=4k \
      --iodepth=1 --numjobs="$3" --direct="$4" --ioengine=psync \
      --runtime="$RUNTIME" --time_based --group_reporting \
      --output-format=json --output="$1" >/dev/null 2>&1
}

run_fio() { # OUT FILE NUMJOBS — O_DIRECT, with the same named buffered fallback
  if fio_randread "$1" "$2" "$3" 1; then return 0; fi
  if fio_randread "$1" "$2" "$3" 0; then
    echo "transport-court: NOTE: arm O_DIRECT refused by the backing store; ran buffered I/O" >&2
    return 0
  fi
  return 1
}

# conn_id MNT — the FUSE connection id (sysfs name) for the mount at MNT. FUSE
# reports connection id as the minor of an anonymous device in mountinfo.
conn_id() {
  id=$(awk -v mnt="$1" '$5 == mnt { split($3, a, ":"); print a[2]; exit }' /proc/self/mountinfo)
  if [ -n "$id" ] && [ -d "/sys/fs/fuse/connections/$id" ]; then
    printf '%s\n' "$id"
    return 0
  fi
  return 1
}

# sample_waiting CONNDIR OUT — sample the pending-request backlog for RUNTIME+2s.
sample_waiting() {
  : > "$2"
  end=$(( $(date +%s) + RUNTIME + 2 ))
  while [ "$(date +%s)" -lt "$end" ]; do
    if [ -r "$1/waiting" ]; then cat "$1/waiting" >> "$2"; fi
    sleep 0.1
  done
}

# counters CONNDIR WAITING OUT — write key=value backpressure counters.
counters() {
  conn=$1; waiting=$2; out=$3
  {
    if [ -r "$conn/max_background" ]; then printf 'max_background=%s\n' "$(cat "$conn/max_background")"; fi
    if [ -r "$conn/congestion_threshold" ]; then printf 'congestion_threshold=%s\n' "$(cat "$conn/congestion_threshold")"; fi
    if [ -s "$waiting" ]; then
      printf 'waiting_max=%s\n' "$(sort -n "$waiting" | tail -n 1)"
      printf 'waiting_mean=%s\n' "$(awk '{ s += $1; n++ } END { if (n) printf "%.3f", s/n; else print "0" }' "$waiting")"
      printf 'samples=%s\n' "$(wc -l < "$waiting" | tr -d ' ')"
    fi
  } > "$out"
}

arm() { # MODE NUMJOBS
  mode=$1; nj=$2
  [ "$mounted" = 1 ] || return 0
  out="$WORK/fio-$mode-$nj.json"
  if [ ! -e "$MNT/data.bin" ]; then
    echo "transport-court: WARN: $mode/$nj: no data file; skipping arm" >&2
    return 0
  fi
  wf="$WORK/wait-$mode-$nj"
  if conn=$(conn_id "$MNT") 2>/dev/null; then
    sample_waiting "/sys/fs/fuse/connections/$conn" "$wf" &
    sampler=$!
  else
    sampler=""
    echo "transport-court: NOTE: $mode/$nj: FUSE connection id not found; counters will be absent" >&2
  fi
  if ! run_fio "$out" "$MNT/data.bin" "$nj"; then
    echo "transport-court: WARN: $mode/$nj: fio failed; skipping arm" >&2
    [ -n "$sampler" ] && wait "$sampler" 2>/dev/null || true
    return 0
  fi
  [ -n "$sampler" ] && wait "$sampler" 2>/dev/null || true
  if [ -s "$out" ]; then
    ARMS="$ARMS -arm $mode/$nj=$out"
    if [ -n "$sampler" ]; then
      cf="$WORK/counters-$mode-$nj.txt"
      counters "/sys/fs/fuse/connections/$conn" "$wf" "$cf"
      [ -s "$cf" ] && COUNTER_ARGS="$COUNTER_ARGS -counter $mode/$nj=$cf"
    fi
  else
    echo "transport-court: WARN: $mode/$nj: fio produced no receipt" >&2
  fi
}

if ! "$ORFS" init "$VOL" --segment-size 8388608 >/dev/null 2>&1; then
  echo "transport-court: FAIL: could not initialise a volume" >&2
  exit 1
fi

for mode in f0 f0-single; do
  echo "transport-court: mounting with --transport $mode"
  "$ORFS" mount "$VOL" "$MNT" --direct-io --transport "$mode" >/dev/null 2>&1 &
  mount_pid=$!
  sleep 2
  if ! mountpoint -q "$MNT"; then
    echo "transport-court: WARN: $mode mount did not come up; skipping its arms" >&2
    kill "$mount_pid" 2>/dev/null || true
    mount_pid=""
    continue
  fi
  mounted=1
  dd if=/dev/zero of="$MNT/data.bin" bs=1048576 count="$FILE_MIB" conv=fsync >/dev/null 2>&1 || true
  arm "$mode" 1
  arm "$mode" 64
  umount "$MNT" 2>/dev/null || "$ORFS" unmount "$MNT" >/dev/null 2>&1 || true
  mounted=0
  kill "$mount_pid" 2>/dev/null || true
  # Reap the daemon before the next mount: a FUSE connection that is still
  # tearing down makes the following mount(2) hang.
  wait "$mount_pid" 2>/dev/null || true
  mount_pid=""
  sleep 1
done

if [ -z "$ARMS" ]; then
  echo "transport-court: FAIL: no fio arm produced a receipt" >&2
  exit 1
fi

# --- compose the receipt ----------------------------------------------------

OUT="$EVID/performance/transport.json"
PROBE_ARG=""
[ -s "$PROBE_OUT" ] && PROBE_ARG="-probe $PROBE_OUT"
# ARMS/COUNTER_ARGS are constructed with no whitespace in paths (mktemp), so
# word splitting is intended here.
# shellcheck disable=SC2086
if "$TRANSPORTSUMMARY" -o "$OUT" $PROBE_ARG $ARMS $COUNTER_ARGS; then
  echo "transport-court: wrote $OUT"
else
  echo "transport-court: FAIL: could not compose $OUT" >&2
  exit 1
fi

echo "transport-court: PASS"
