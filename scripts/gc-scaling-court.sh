#!/bin/sh
# Go-runtime GC-scaling court — runs *inside* the court container as root.
#
# It answers "what does the Go runtime cost, and under which runtime knobs is
# the port a reasonable operational alternative?" by sweeping GOGC and
# GOMEMLIMIT and measuring, per configuration, both the *application* view
# (cmd/orfsrw: 64 concurrent 4 KiB random readers plus concurrent writers,
# p50/p99/p99.9, throughput, errno-classified errors) and the *Go-runtime* view
# (the mount daemon's --profile-dir dump: windowed GC pauses, scheduler latency,
# allocation rate, real VmRSS/VmHWM, mutex wait). cmd/gcsummary merges the two
# into evidence/performance/gc-scaling.json.
#
# Design choice (see docs/RUNTIME.md): the workload drives a *mounted* volume,
# and the runtime metrics are collected *from the mount daemon* via
# `--profile-dir`. This keeps the FUSE integration inside the measurement — the
# question explicitly includes how the FUSE boundary affects tail latency — and
# it is the artifact an operator can reproduce. Driving internal/namespace
# in-process was rejected because it removes the very FUSE boundary whose cost
# is part of the question.
#
# Env:
#   ORFS / ORFSRW / GCSUMMARY   binary paths       (default /out/orfs etc.)
#   ORFS_EVIDENCE_DIR           receipt root       (default /evidence)
#   ARM_SECONDS                 sweep arm duration (default 5)
#   TRACE_SECONDS               trace arm duration (default 3)
#   READERS / WRITERS           driver concurrency (default 64 / 8)
#   DATA_MIB / WRITE_MIB        seeded file sizes  (default 32 / 8)
#   MAX_APPEND_MIB              writer append cap  (default 32)
#   SEGMENT_SIZE                OrFS segment bytes (default 8388608)
set -eu

ORFS=${ORFS:-/out/orfs}
ORFSRW=${ORFSRW:-/out/orfsrw}
GCSUMMARY=${GCSUMMARY:-/out/gcsummary}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}

ARM_SECONDS=${ARM_SECONDS:-5}
TRACE_SECONDS=${TRACE_SECONDS:-3}
READERS=${READERS:-64}
WRITERS=${WRITERS:-8}
DATA_MIB=${DATA_MIB:-32}
WRITE_MIB=${WRITE_MIB:-8}
MAX_APPEND_MIB=${MAX_APPEND_MIB:-32}
SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}

if [ ! -e /dev/fuse ]; then
  echo "gc-scaling-court: SKIPPED (no /dev/fuse)"
  exit 0
fi
for b in "$ORFS" "$ORFSRW" "$GCSUMMARY"; do
  if [ ! -x "$b" ]; then
    echo "gc-scaling-court: SKIPPED (missing $b)"
    exit 0
  fi
done

mkdir -p "$EVID/performance"

WORK=$(mktemp -d)
ARMS="$WORK/arms"
mkdir -p "$ARMS"

cleanup() {
  # Best-effort teardown; a stale mount from a killed daemon is lazily detached.
  for m in "$WORK"/mnt-*; do
    [ -d "$m" ] && umount -l "$m" 2>/dev/null || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# --- bounded waits (never an unbounded loop) ------------------------------

wait_mount() { # $1 = mountpoint; ~5s budget
  _w=0
  while [ "$_w" -lt 50 ]; do
    mountpoint -q "$1" && return 0
    _w=$((_w + 1))
    sleep 0.1
  done
  return 1
}

wait_gone() { # $1 = pid; ~10s budget
  _w=0
  while [ "$_w" -lt 100 ]; do
    kill -0 "$1" 2>/dev/null || return 0
    _w=$((_w + 1))
    sleep 0.1
  done
  return 1
}

# --- helpers --------------------------------------------------------------

# build_volume VOL MNT STDERR — init a fresh volume and seed the read and write
# files through a plain mount, then unmount. Seeding outside the measured mount
# keeps the daemon's runtime window free of seed-write cost.
build_volume() {
  vol=$1
  mnt=$2
  serr=$3
  rm -rf "$vol" "$mnt"
  mkdir -p "$mnt"
  "$ORFS" init "$vol" --segment-size "$SEGMENT_SIZE" >/dev/null
  "$ORFS" mount "$vol" "$mnt" >"$serr" 2>&1 &
  sp=$!
  if ! wait_mount "$mnt"; then
    echo "gc-scaling-court: WARN: seed mount did not come up; daemon said:" >&2
    head -n 20 "$serr" >&2 || true
    kill -9 "$sp" 2>/dev/null || true
    return 1
  fi
  dd if=/dev/zero of="$mnt/data.bin" bs=1048576 count="$DATA_MIB" conv=fsync >/dev/null 2>&1 || true
  dd if=/dev/zero of="$mnt/hot.bin" bs=1048576 count="$WRITE_MIB" conv=fsync >/dev/null 2>&1 || true
  "$ORFS" unmount "$mnt" >/dev/null 2>&1 || umount "$mnt" 2>/dev/null || true
  kill "$sp" 2>/dev/null || true
  wait_gone "$sp" || kill -9 "$sp" 2>/dev/null || true
  return 0
}

# run_arm LABEL VOL MNT GOGC MEMLIM DUR PROFDIR STDERR LOCKS DRIVEROUT
# Mount the (already built) volume for LABEL with the runtime knobs, drive it,
# and shut the daemon down so it writes its profile dump. Returns the driver's
# exit status (0 unless the driver could not start at all).
run_arm() {
  label=$1; vol=$2; mnt=$3; gogc=$4; memlim=$5; dur=$6; pdir=$7; serr=$8; locks=$9; dout=${10}
  # GOMEMLIMIT=off is the runtime's spelling for "no limit" (math.MaxInt64);
  # GOMEMLIMIT=unlimited is malformed and panics the daemon at startup.
  memlim_env="$memlim"
  [ "$memlim" = "unlimited" ] && memlim_env="off"
  locks_flag=""
  [ "$locks" = "1" ] && locks_flag="--profile-locks"
  if [ "$locks" = "1" ]; then
    GOGC="$gogc" GOMEMLIMIT="$memlim_env" GODEBUG=gctrace=1 \
      sh -c 'echo 800 > /proc/self/oom_score_adj 2>/dev/null; exec "$0" "$@"' \
      "$ORFS" mount "$vol" "$mnt" --direct-io --profile-dir "$pdir" $locks_flag >"$serr" 2>&1 &
  else
    GOGC="$gogc" GOMEMLIMIT="$memlim_env" \
      sh -c 'echo 800 > /proc/self/oom_score_adj 2>/dev/null; exec "$0" "$@"' \
      "$ORFS" mount "$vol" "$mnt" --direct-io --profile-dir "$pdir" $locks_flag >"$serr" 2>&1 &
  fi
  pid=$!
  if ! wait_mount "$mnt"; then
    echo "gc-scaling-court: WARN: $label: arm mount did not come up; daemon said:" >&2
    head -n 20 "$serr" >&2 || true
    kill -9 "$pid" 2>/dev/null || true
    return 1
  fi

  drc=0
  # Run the driver in the background under a bounded watchdog. The driver stops
  # issuing at its own deadline, but a mount that stalls (e.g. a backing-store
  # I/O hiccup beneath the writer path) leaves a blocked pread that ignores that
  # deadline; without a bound the arm's window could blow up to whatever the
  # stall lasts. If the driver overruns, SIGTERM the daemon, which aborts the
  # FUSE connection and unblocks the reads, and record the arm as stalled.
  "$ORFSRW" -dir "$mnt" -readers "$READERS" -writers "$WRITERS" -seconds "$dur" \
    -bs 4096 -read-file data.bin -write-file hot.bin -write-mode mixed \
    -max-append-mib "$MAX_APPEND_MIB" -out "$dout" &
  drv=$!
  limit=$((dur + 8))
  _w=0
  while [ "$_w" -lt "$limit" ]; do
    kill -0 "$drv" 2>/dev/null || break
    _w=$((_w + 1))
    sleep 1
  done
  stalled=0
  if kill -0 "$drv" 2>/dev/null; then
    stalled=1
    echo "gc-scaling-court: WARN: $label: driver exceeded ${limit}s (stalled mount); aborting it to unblock" >&2
    kill -TERM "$pid" 2>/dev/null || true
  fi
  if wait "$drv"; then :; else
    drc=$?
    echo "gc-scaling-court: WARN: $label: driver exited $drc" >&2
  fi
  sdir=$(dirname "$serr")
  if [ "$stalled" = "1" ]; then
    printf '{"status":"stalled","wall_limit_seconds":%s}\n' "$limit" > "$sdir/status.json"
  fi

  # Graceful shutdown so the daemon writes runtime-metrics.json + profiles.
  kill -TERM "$pid" 2>/dev/null || true
  wait_gone "$pid" || kill -9 "$pid" 2>/dev/null || true
  umount -l "$mnt" 2>/dev/null || true
  return "$drc"
}

# --- bounded sweep --------------------------------------------------------

# A bounded 9-arm spread covering both axes: every GOGC value at unlimited
# GOMEMLIMIT, the GOMEMLIMIT axis at the default GOGC, and the two bounded
# GOGC=off cells. This is not the full 5x3 cross product (see docs/RUNTIME.md).
# The GOGC=off + unlimited arm runs last, since it is unbounded by construction.
CONFIGS=${COURT_CONFIGS:-"400:unlimited 100:unlimited 50:unlimited 20:unlimited 100:256MiB 100:512MiB off:256MiB off:512MiB off:unlimited"}

echo "gc-scaling-court: sweeping $(echo "$CONFIGS" | wc -w) configurations (${ARM_SECONDS}s each, ${READERS} readers / ${WRITERS} writers)"

i=0
for cfg in $CONFIGS; do
  i=$((i + 1))
  gogc=${cfg%%:*}
  memlim=${cfg##*:}
  label=$(printf 'arm-%02d-gogc%s-mem%s' "$i" "$gogc" "$memlim")
  adir="$ARMS/$label"
  mkdir -p "$adir"
  printf '{"gogc":"%s","memlimit":"%s","readers":%s,"writers":%s}\n' \
    "$gogc" "$memlim" "$READERS" "$WRITERS" > "$adir/config.json"

  if build_volume "$WORK/vol-$label" "$WORK/mnt-$label" "$WORK/seed-$label.stderr"; then
    # The GOGC=off + unlimited arm is unbounded by construction: it is expected
    # to OOM-kill the daemon inside the 2 GiB container. It runs last (it is the
    # final config) so it cannot poison the others, and its row is recorded with
    # status=daemon_died rather than a fabricated number.
    run_arm "$label" "$WORK/vol-$label" "$WORK/mnt-$label" "$gogc" "$memlim" \
      "$ARM_SECONDS" "$adir" "$adir/daemon.stderr" 0 "$adir/driver.json" || true
    echo "gc-scaling-court: $label done"
  else
    echo "gc-scaling-court: WARN: $label: volume build failed; skipping" >&2
  fi
  rm -rf "$WORK/vol-$label" "$WORK/mnt-$label"
done

# --- GC trace + lock profiles for the best and worst configurations --------

BEST=$("$GCSUMMARY" -work "$ARMS" -pick best 2>/dev/null || true)
WORST=$("$GCSUMMARY" -work "$ARMS" -pick worst 2>/dev/null || true)
echo "gc-scaling-court: best p99.9 arm: ${BEST:-<none>}; worst p99.9 arm: ${WORST:-<none>}"

trace_one() { # $1 = arm label, $2 = role
  label=$1; role=$2
  [ -n "$label" ] || return 0
  adir="$ARMS/$label"
  [ -d "$adir" ] || return 0
  gogc=$(sed -n 's/.*"gogc":"\([^"]*\)".*/\1/p' "$adir/config.json")
  memlim=$(sed -n 's/.*"memlimit":"\([^"]*\)".*/\1/p' "$adir/config.json")
  [ -n "$gogc" ] || gogc=100
  [ -n "$memlim" ] || memlim=unlimited

  echo "gc-scaling-court: GC-trace + lock-profile run for $role arm $label (GOGC=$gogc GOMEMLIMIT=$memlim)"
  mkdir -p "$adir/profile"
  if build_volume "$WORK/vol-$label-t" "$WORK/mnt-$label-t" "$WORK/seed-$label-t.stderr"; then
    run_arm "$label" "$WORK/vol-$label-t" "$WORK/mnt-$label-t" "$gogc" "$memlim" \
      "$TRACE_SECONDS" "$adir/profile" "$adir/profile/daemon.stderr" 1 "$adir/profile/driver.json" || true
    # Human-readable lock profiles (best effort; the .prof files are the record).
    ( cd "$adir/profile" && go tool pprof -top -nodecount=15 mutex.prof > mutex-top.txt 2>/dev/null ) || true
    ( cd "$adir/profile" && go tool pprof -top -nodecount=15 block.prof > block-top.txt 2>/dev/null ) || true
  else
    echo "gc-scaling-court: WARN: $role trace volume build failed" >&2
  fi
  rm -rf "$WORK/vol-$label-t" "$WORK/mnt-$label-t"
}

trace_one "$BEST" best
if [ "$WORST" != "$BEST" ]; then
  trace_one "$WORST" worst
fi

# --- merge into the receipt -----------------------------------------------

OUT="$EVID/performance/gc-scaling.json"
if "$GCSUMMARY" -work "$ARMS" -out "$OUT"; then
  echo "gc-scaling-court: wrote $OUT"
else
  echo "gc-scaling-court: FAIL: gcsummary could not write the receipt" >&2
  exit 1
fi

rows=$(grep -o '"gogc"' "$OUT" 2>/dev/null | wc -l)
if [ "$rows" -eq 0 ]; then
  echo "gc-scaling-court: FAIL: receipt has no rows" >&2
  exit 1
fi
echo "gc-scaling-court: PASS ($rows arm(s) measured under $EVID/performance)"
