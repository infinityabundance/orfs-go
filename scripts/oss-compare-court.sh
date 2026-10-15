#!/bin/sh
# Open-source head-to-head court, step 2 — runs *inside* the court container as
# root with /dev/fuse + CAP_SYS_ADMIN.
#
# It drives the SAME shared, deterministic workload driver (cmd/orfscompare)
# against three MOUNTED systems on the SAME machine, same container caps:
# (a) OrFS-GO (this repository's Go binary), (b) OrFS-Rust (the Rust reference,
# `.cache/orfs-rs/orfs`), and (c) SeaweedFS's FUSE client (`weed mount`, staged
# from the pinned chrislusf/seaweedfs image). OrFS-GO and OrFS-Rust are mounted
# with equal durability (one fsync per appended record) and the same `--direct-io`
# setting; SeaweedFS is mounted as it comes (no --direct-io knob) and its
# durability semantics are recorded, not assumed.
#
# It composes evidence/oss-compare.json (schema orfs.oss-compare.v1): one row per
# {system, workload, writers, readers, block_size} plus a comparability block
# that names every way the two configurations differ. It never ranks rows: the
# receipt is data, not a leaderboard. If a system cannot be mounted the same way,
# it is run as best it allows and the exact difference is recorded — a documented
# asymmetry, never a fake parity.
#
# Env:
#   ORFS / ORFS_RS / ORFSCOMPARE / OSSSUMMARY / WEED   binary paths
#   SEAWEEDFS_IMAGE / SEAWEEDFS_DIGEST                 pinned identity, recorded
#   ORFS_EVIDENCE_DIR                            receipt root   (default /evidence)
#   SEGMENT_SIZE                                 volume segment bytes (default 8 MiB)
#   APPEND_SECONDS / READ_SECONDS                bounded arm durations (8 / 4)
#   WRITERS                                      append writer counts (default "1 64")
#   READERS                                      read concurrency   (default "1 64")
#   BLOCK_SIZES                                  read block sizes   (4096 65536 1048576)
#   RECORD_BYTES / READ_MIB / MAX_APPEND_MIB     workload sizes
#   SEED                                         deterministic seed (default 1)
#   Container caps (forwarded from scripts/lib.sh): CAP_MEM_LIMIT / CAP_MEMSWAP_LIMIT /
#   CAP_CPUS / CAP_PIDS_LIMIT.
set -eu

ORFS=${ORFS:-/out/orfs}
ORFS_RS=${ORFS_RS:-/out-rs/orfs}
ORFSCOMPARE=${ORFSCOMPARE:-/out/orfscompare}
OSSSUMMARY=${OSSSUMMARY:-/out/osscomparesummary}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}

# The third comparator: SeaweedFS's FUSE client (`weed mount`). The `weed`
# binary is staged from the pinned image by the host wrapper and bind-mounted
# read-only. Its absolute path is passed in; when it is missing the SeaweedFS
# half is SKIPPED with the reason and recorded, never faked.
WEED=${WEED:-/out-sw/weed}
SEAWEEDFS_IMAGE=${SEAWEEDFS_IMAGE:-chrislusf/seaweedfs:3.99}
SEAWEEDFS_DIGEST=${SEAWEEDFS_DIGEST:-unavailable}
SW_IP=127.0.0.1
SW_MASTER_PORT=9333
SW_VOLUME_PORT=8080
SW_FILER_PORT=8888
# SeaweedFS is a multi-process stack (server + FUSE client) sharing the SAME
# 2 GiB container cap as the driver. At its default Go GC settings the client's
# heap grew past the cap and the kernel OOM-killed the volume server mid-arm
# (recorded in not_comparable). GOMEMLIMIT/GOGC are the documented Go runtime
# knobs; they bound each weed process inside the shared cap. Recorded verbatim.
SW_GOGC=${SW_GOGC:-50}
SW_GOMEMLIMIT=${SW_GOMEMLIMIT:-700MiB}

SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}
APPEND_SECONDS=${APPEND_SECONDS:-5}
READ_SECONDS=${READ_SECONDS:-4}
WRITERS=${WRITERS:-"1 64"}
READERS=${READERS:-"1 64"}
BLOCK_SIZES=${BLOCK_SIZES:-"4096 65536 1048576"}
RECORD_BYTES=${RECORD_BYTES:-4096}
READ_MIB=${READ_MIB:-64}
MAX_APPEND_MIB=${MAX_APPEND_MIB:-1024}
SEED=${SEED:-1}

CAP_MEM_LIMIT=${CAP_MEM_LIMIT:-2g}
CAP_MEMSWAP_LIMIT=${CAP_MEMSWAP_LIMIT:-2g}
CAP_CPUS=${CAP_CPUS:-4}
CAP_PIDS_LIMIT=${CAP_PIDS_LIMIT:-1024}

# How many arms a fully measured system is expected to contribute. A mounted
# system that loses an arm (its daemon died, an I/O error, ...) leaves a partial
# receipt; that is reported, never hidden behind a PASS.
N_WRITERS=$(echo "$WRITERS" | wc -w)
N_READERS=$(echo "$READERS" | wc -w)
N_BLOCKS=$(echo "$BLOCK_SIZES" | wc -w)
ARMS_PER_SYS=$((N_WRITERS + N_READERS * N_BLOCKS))

if [ ! -e /dev/fuse ]; then
  echo "oss-compare-court: SKIPPED (no /dev/fuse)"
  exit 0
fi
for b in "$ORFS" "$ORFSCOMPARE" "$OSSSUMMARY"; do
  if [ ! -x "$b" ]; then
    echo "oss-compare-court: SKIPPED (missing $b)"
    exit 0
  fi
done
# The Rust half is optional: without it the court still measures OrFS-GO and
# records the absence as a caveat, never as a parity claim.
RS_AVAILABLE=1
if [ ! -x "$ORFS_RS" ]; then
  RS_AVAILABLE=0
  echo "oss-compare-court: NOTE: no Rust reference at $ORFS_RS; only OrFS-GO will be measured" >&2
fi
# The SeaweedFS half is optional the same way: without the `weed` binary the
# court still measures the OrFS pair and records SeaweedFS as SKIPPED (with the
# reason), never as a measured row.
SW_AVAILABLE=1
if [ ! -x "$WEED" ]; then
  SW_AVAILABLE=0
  echo "oss-compare-court: SKIPPED SeaweedFS (no weed binary at $WEED; stage it with scripts/container-oss-compare.sh)" >&2
fi

mkdir -p "$EVID"
WORK=$(mktemp -d)
RUNS="$WORK/runs"
mkdir -p "$RUNS"

GO_VOL="$WORK/go-vol"; GO_MNT="$WORK/go-mnt"
RS_VOL="$WORK/rs-vol"; RS_MNT="$WORK/rs-mnt"
SW_ROOT="$WORK/sw"; SW_MNT="$WORK/sw-mnt"
mkdir -p "$GO_MNT" "$RS_MNT" "$SW_MNT"

GO_PID=""; RS_PID=""
GO_MOUNTED=0; RS_MOUNTED=0

# SeaweedFS state. SW_STATUS is the honest per-system verdict string; it is one
# of not_attempted | server_failed | mount_failed | prepare_failed | mounted, and
# is recorded verbatim.
SW_SERVER_PID=""; SW_MOUNT_PID=""
SW_SERVER_UP=0; SW_MOUNTED=0; SW_STATUS="not_attempted"

# Arm bookkeeping: a failed arm is counted and named so the receipt can say
# exactly which coordinates are missing instead of quietly shrinking.
ARM_FAILS=0
SYS_FAILS=0
: > "$WORK/failed-arms"

cleanup() {
  if [ "$GO_MOUNTED" = 1 ]; then
    "$ORFS" unmount "$GO_MNT" >/dev/null 2>&1 || umount -l "$GO_MNT" 2>/dev/null || true
  fi
  if [ "$RS_MOUNTED" = 1 ]; then
    "$ORFS_RS" unmount "$RS_MNT" >/dev/null 2>&1 || umount -l "$RS_MNT" 2>/dev/null || true
  fi
  if [ "$SW_MOUNTED" = 1 ]; then
    umount -l "$SW_MNT" 2>/dev/null || true
  fi
  [ -n "$GO_PID" ] && kill "$GO_PID" 2>/dev/null || true
  [ -n "$RS_PID" ] && kill "$RS_PID" 2>/dev/null || true
  [ -n "$SW_MOUNT_PID" ] && kill "$SW_MOUNT_PID" 2>/dev/null || true
  [ -n "$SW_SERVER_PID" ] && kill "$SW_SERVER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# --- bounded waits (never an unbounded loop) -------------------------------

wait_mount() { # $1 = mountpoint; ~10s budget
  _w=0
  while [ "$_w" -lt 100 ]; do
    mountpoint -q "$1" && return 0
    _w=$((_w + 1))
    sleep 0.1
  done
  return 1
}

# wait_tcp waits for a TCP port to accept a connection, ~30s budget. POSIX sh has
# no /dev/tcp, so it uses the container's bash; this is how the court waits for
# the SeaweedFS master/volume/filer to listen before mounting.
wait_tcp() { # $1 = host, $2 = port
  _w=0
  while [ "$_w" -lt 300 ]; do
    bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null && return 0
    _w=$((_w + 1))
    sleep 0.1
  done
  return 1
}

# --- mount both systems ----------------------------------------------------

echo "oss-compare-court: building volumes (segment-size=$SEGMENT_SIZE)"

if ! "$ORFS" init "$GO_VOL" --segment-size "$SEGMENT_SIZE" >/dev/null 2>"$WORK/go-init.err"; then
  echo "oss-compare-court: FAIL: Go orfs init failed" >&2
  cat "$WORK/go-init.err" >&2 || true
  exit 1
fi

# Go: a plain mount has group commit OFF, so every publish takes its own
# durability barrier (one fsync per record). Pin ORFS_GROUP_COMMIT empty so the
# equal-durability claim is explicit and not inherited from the environment.
ORFS_GROUP_COMMIT= "$ORFS" mount "$GO_VOL" "$GO_MNT" --direct-io >"$WORK/go-mount.err" 2>&1 &
GO_PID=$!
if wait_mount "$GO_MNT"; then
  GO_MOUNTED=1
else
  echo "oss-compare-court: FAIL: OrFS-GO mount did not come up; daemon said:" >&2
  cat "$WORK/go-mount.err" >&2 || true
  kill -9 "$GO_PID" 2>/dev/null || true
  exit 1
fi
echo "oss-compare-court: OrFS-GO mounted at $GO_MNT"

RS_STATUS="not_attempted"
if [ "$RS_AVAILABLE" = 1 ]; then
  if ! "$ORFS_RS" init "$RS_VOL" --segment-size "$SEGMENT_SIZE" >/dev/null 2>"$WORK/rs-init.err"; then
    RS_STATUS="init_failed"
    echo "oss-compare-court: NOTE: Rust orfs init failed; the Rust half is skipped" >&2
    cat "$WORK/rs-init.err" >&2 || true
  else
    # Equal durability: --durability sync (fsync per transaction). Backing I/O is
    # pinned to sync to match the Go daemon's synchronous pwrite/pread.
    "$ORFS_RS" mount "$RS_VOL" "$RS_MNT" --direct-io --durability sync --backing-io sync \
      >"$WORK/rs-mount.err" 2>&1 &
    RS_PID=$!
    if wait_mount "$RS_MNT"; then
      RS_MOUNTED=1
      RS_STATUS="mounted"
      echo "oss-compare-court: OrFS-Rust mounted at $RS_MNT"
    else
      RS_STATUS="mount_failed"
      echo "oss-compare-court: NOTE: OrFS-Rust mount did not come up; the Rust half is skipped" >&2
      cat "$WORK/rs-mount.err" >&2 || true
      kill -9 "$RS_PID" 2>/dev/null || true
      RS_PID=""
    fi
  fi
fi

# --- prepare the workload files (outside the measured window) --------------

PREP_WRITERS=1
for w in $WRITERS; do
  [ "$w" -gt "$PREP_WRITERS" ] && PREP_WRITERS=$w
done

# prepare_try prepares the workload files on a mount and returns non-zero on
# failure (without exiting), so a system that cannot be prepared is recorded as
# SKIPPED rather than tearing the whole court down.
prepare_try() { # $1 = mountpoint, $2 = label
  mnt=$1; label=$2
  echo "oss-compare-court: preparing workload files on $label"
  "$ORFSCOMPARE" prepare -dir "$mnt" -writers "$PREP_WRITERS" -append-prefix append \
      -read-file data.bin -read-bytes "$((READ_MIB * 1024 * 1024))" -seed "$SEED" \
      >"$WORK/prepare-$label.json" 2>&1
}

prepare_on() { # $1 = mountpoint, $2 = label; fatal on failure
  if ! prepare_try "$1" "$2"; then
    echo "oss-compare-court: FAIL: prepare failed on $2" >&2
    cat "$WORK/prepare-$2.json" >&2 || true
    exit 1
  fi
}

prepare_on "$GO_MNT" go
[ "$RS_MOUNTED" = 1 ] && prepare_on "$RS_MNT" rust

# --- run the matrix --------------------------------------------------------

run_arm() { # $1=system $2=mnt $3=workload $4=writers $5=readers $6=bs
  sys=$1; mnt=$2; wl=$3; w=$4; r=$5; bs=$6
  out="$RUNS/$sys-$wl-w$w-r$r-bs$bs.json"
  case "$wl" in
    append)
      if ! "$ORFSCOMPARE" run -dir "$mnt" -system "$sys" -workload append \
          -writers "$w" -append-prefix append -record-bytes "$RECORD_BYTES" \
          -seconds "$APPEND_SECONDS" -max-bytes-per-writer "$((MAX_APPEND_MIB * 1024 * 1024))" \
          -seed "$SEED" -out "$out" >"$out.log" 2>&1; then
        echo "oss-compare-court: WARN: arm $sys/$wl writers=$w FAILED; recorded as missing" >&2
        cat "$out.log" >&2 || true
        echo "$sys/$wl writers=$w" >> "$WORK/failed-arms"
        ARM_FAILS=$((ARM_FAILS + 1))
        return 1
      fi
      ;;
    read)
      if ! "$ORFSCOMPARE" run -dir "$mnt" -system "$sys" -workload read \
          -readers "$r" -read-file data.bin -bs "$bs" \
          -seconds "$READ_SECONDS" -seed "$SEED" -out "$out" >"$out.log" 2>&1; then
        echo "oss-compare-court: WARN: arm $sys/$wl readers=$r bs=$bs FAILED; recorded as missing" >&2
        cat "$out.log" >&2 || true
        echo "$sys/$wl readers=$r bs=$bs" >> "$WORK/failed-arms"
        ARM_FAILS=$((ARM_FAILS + 1))
        return 1
      fi
      ;;
  esac
  rm -f "$out.log"
  return 0
}

run_system() { # $1 = system label, $2 = mountpoint
  sys=$1; mnt=$2
  _before=$ARM_FAILS
  # Reads first, on a quiet volume: the append arms retain their whole history
  # (non-withdrawal), so running them first would evict the read file from the
  # backing page cache and make with --direct-io every read a backing I/O. All
  # systems are measured in the same order, so the comparison is symmetric.
  for bs in $BLOCK_SIZES; do
    for r in $READERS; do
      echo "oss-compare-court: $sys read bs=$bs readers=$r (${READ_SECONDS}s)"
      run_arm "$sys" "$mnt" read 0 "$r" "$bs" || true
    done
  done
  for w in $WRITERS; do
    echo "oss-compare-court: $sys append writers=$w (${APPEND_SECONDS}s)"
    run_arm "$sys" "$mnt" append "$w" 0 0 || true
  done
  SYS_FAILS=$((ARM_FAILS - _before))
}

GO_FAILED=0; RS_FAILED=0; SW_FAILED=0
run_system "OrFS-GO" "$GO_MNT"
GO_FAILED=$SYS_FAILS

# Release the finished system before the next one's arms. The arms are strictly
# sequential, so this changes no measurement; it stops a daemon (and its volume's
# page cache) from competing for the single shared memory cap, which is what made
# a long Rust append arm get OOM-killed on a 2 GiB cap. The receipt records this.
if [ "$GO_MOUNTED" = 1 ]; then
  "$ORFS" unmount "$GO_MNT" >/dev/null 2>&1 || umount -l "$GO_MNT" 2>/dev/null || true
  GO_MOUNTED=0
  rm -rf "$GO_VOL"
fi

if [ "$RS_MOUNTED" = 1 ]; then
  run_system "OrFS-Rust" "$RS_MNT"
  RS_FAILED=$SYS_FAILS
  # Release the Rust daemon and its volume before the SeaweedFS cluster starts, so
  # no two systems compete for the shared memory cap.
  "$ORFS_RS" unmount "$RS_MNT" >/dev/null 2>&1 || umount -l "$RS_MNT" 2>/dev/null || true
  RS_MOUNTED=0
  rm -rf "$RS_VOL"
fi

# --- SeaweedFS: single-node cluster (master + volume + filer), then the FUSE
# client (`weed mount`). Brought up exactly as the pinned image's own `server`
# entrypoint does, on loopback, then measured with the same driver. Every daemon
# is released (mount unmounted, cluster killed) before the comparability block.

if [ "$SW_AVAILABLE" = 1 ]; then
  echo "oss-compare-court: starting SeaweedFS (master+volume+filer on $SW_IP)"
  mkdir -p "$SW_ROOT/data" "$SW_ROOT/cache"
  env GOGC="$SW_GOGC" GOMEMLIMIT="$SW_GOMEMLIMIT" "$WEED" server -dir="$SW_ROOT/data" -filer=true -ip="$SW_IP" \
      -master.port="$SW_MASTER_PORT" -volume.port="$SW_VOLUME_PORT" -filer.port="$SW_FILER_PORT" \
      -master.volumeSizeLimitMB=1024 -volume.max=0 -master.electionTimeout=2s \
      >"$WORK/sw-server.err" 2>&1 &
  SW_SERVER_PID=$!
  if wait_tcp "$SW_IP" "$SW_MASTER_PORT" && wait_tcp "$SW_IP" "$SW_VOLUME_PORT" && wait_tcp "$SW_IP" "$SW_FILER_PORT"; then
    SW_SERVER_UP=1
    echo "oss-compare-court: SeaweedFS cluster up (master :$SW_MASTER_PORT, volume :$SW_VOLUME_PORT, filer :$SW_FILER_PORT)"
  else
    SW_STATUS="server_failed"
    echo "oss-compare-court: SKIPPED SeaweedFS (cluster did not come up within the bounded wait); the SeaweedFS half is NOT measured" >&2
    tail -n 40 "$WORK/sw-server.err" >&2 || true
    kill -9 "$SW_SERVER_PID" 2>/dev/null || true
    SW_SERVER_PID=""
  fi
fi

if [ "$SW_SERVER_UP" = 1 ]; then
  echo "oss-compare-court: mounting SeaweedFS via weed mount at $SW_MNT"
  env GOGC="$SW_GOGC" GOMEMLIMIT="$SW_GOMEMLIMIT" "$WEED" mount -filer="$SW_IP:$SW_FILER_PORT" -dir="$SW_MNT" -cacheDir="$SW_ROOT/cache" \
      -filer.path=/ -concurrentWriters=128 \
      >"$WORK/sw-mount.err" 2>&1 &
  SW_MOUNT_PID=$!
  if wait_mount "$SW_MNT"; then
    SW_MOUNTED=1
    SW_STATUS="mounted"
    echo "oss-compare-court: SeaweedFS mounted at $SW_MNT"
  else
    SW_STATUS="mount_failed"
    echo "oss-compare-court: SKIPPED SeaweedFS (weed mount did not come up within the bounded wait); the SeaweedFS half is NOT measured" >&2
    cat "$WORK/sw-mount.err" >&2 || true
    kill -9 "$SW_MOUNT_PID" 2>/dev/null || true
    SW_MOUNT_PID=""
  fi
fi

if [ "$SW_MOUNTED" = 1 ]; then
  if prepare_try "$SW_MNT" seaweedfs; then
    run_system "SeaweedFS" "$SW_MNT"
    SW_FAILED=$SYS_FAILS
  else
    SW_STATUS="prepare_failed"
    SW_MOUNTED=0
    echo "oss-compare-court: SKIPPED SeaweedFS (prepare failed on the weed mount); the SeaweedFS half is NOT measured" >&2
    cat "$WORK/prepare-seaweedfs.json" >&2 || true
  fi
fi

# Snapshot the SeaweedFS daemon logs *before* teardown, so the recorded stderr
# describes the measured run and not the shutdown (the receipt is built later).
cp "$WORK/sw-server.err" "$WORK/sw-server.snap" 2>/dev/null || true
cp "$WORK/sw-mount.err" "$WORK/sw-mount.snap" 2>/dev/null || true

# Teardown in reverse order (mount, then cluster), releasing the shared cap.
if [ "$SW_MOUNTED" = 1 ]; then
  umount -l "$SW_MNT" 2>/dev/null || true
  SW_MOUNTED=0
fi
[ -n "$SW_MOUNT_PID" ] && kill "$SW_MOUNT_PID" 2>/dev/null || true
[ -n "$SW_SERVER_PID" ] && kill "$SW_SERVER_PID" 2>/dev/null || true
rm -rf "$SW_ROOT"

# --- comparability block ---------------------------------------------------

# json_escape makes an arbitrary shell string safe to embed in a JSON string: it
# first drops ANSI colour sequences, maps every remaining non-printable byte
# (control chars, high bytes, stray newlines) to a space, trims the ends, then
# escapes backslashes and double quotes. This keeps a captured daemon message
# from ever producing invalid JSON.
json_escape() {
  sed -e 's/\x1b\[[0-9;]*[A-Za-z]//g' \
  | LC_ALL=C tr -c '[:print:]' ' ' \
  | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^ *//' -e 's/ *$//'
}

# stderr_tail returns a bounded, JSON-safe tail of a daemon's stderr.
stderr_tail() {
  if [ -f "$1" ]; then tail -c 2000 "$1" | json_escape; fi
}

sha256_of() { [ -f "$1" ] && sha256sum "$1" | cut -d' ' -f1 || echo "unavailable"; }

GO_SHA=$(sha256_of "$ORFS")
RS_SHA="unavailable"; RS_VERSION="unavailable"
if [ "$RS_AVAILABLE" = 1 ]; then
  RS_SHA=$(sha256_of "$ORFS_RS")
  RS_VERSION=$("$ORFS_RS" --version 2>&1 | head -n 1 | json_escape)
fi
GO_STDERR=$(stderr_tail "$WORK/go-mount.err")
RS_STDERR=$(stderr_tail "$WORK/rs-mount.err")
NPROC=$(nproc 2>/dev/null || echo "?")
KERNEL=$(uname -sr | json_escape)
BACKING_FS=$(stat -f -c %T "$WORK" 2>/dev/null || echo "?")
READ_MIB_JSON=$READ_MIB
MAX_APPEND_MIB_JSON=$MAX_APPEND_MIB

# SeaweedFS identity (version pinned by the image) and captured stderr.
SW_VERSION="unavailable"; SW_STDERR=""
if [ "$SW_AVAILABLE" = 1 ]; then
  SW_VERSION=$("$WEED" version 2>/dev/null | awk '/^version/{print; exit}' | json_escape)
  [ -n "$SW_VERSION" ] || SW_VERSION="unavailable"
  SW_SERVER_STDERR=$(stderr_tail "$WORK/sw-server.snap")
  SW_MOUNT_STDERR=$(stderr_tail "$WORK/sw-mount.snap")
  [ -n "$SW_SERVER_STDERR$SW_MOUNT_STDERR" ] || { SW_SERVER_STDERR=$(stderr_tail "$WORK/sw-server.err"); SW_MOUNT_STDERR=$(stderr_tail "$WORK/sw-mount.err"); }
  SW_STDERR="server: ${SW_SERVER_STDERR} | mount: ${SW_MOUNT_STDERR}"
fi

# The SeaweedFS arms entry: a measured system reports ran/failed; an unmeasured
# system reports measured=false with its exact status and no implied rows.
SW_ARMS_JSON="{\"measured\": false, \"status\": \"${SW_STATUS}\", \"ran\": 0, \"failed\": 0}"
if [ "$SW_STATUS" = "mounted" ]; then
  SW_ARMS_JSON="{\"measured\": true, \"status\": \"mounted\", \"ran\": $((ARMS_PER_SYS - SW_FAILED)), \"failed\": ${SW_FAILED}}"
fi

# failed_arms renders the missing coordinates as a JSON string array (empty if
# every expected arm ran).
FAILED_JSON=""
if [ -s "$WORK/failed-arms" ]; then
  FAILED_JSON=$(sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' "$WORK/failed-arms" | sed -e 's/^/  "/' -e 's/$/"/' | paste -sd, -)
fi

COMP="$WORK/comparability.json"
cat > "$COMP" <<EOF
{
  "same_machine": true,
  "container": {
    "caps": {
      "memory": "${CAP_MEM_LIMIT}",
      "memory_swap": "${CAP_MEMSWAP_LIMIT}",
      "cpus": "${CAP_CPUS}",
      "pids_limit": "${CAP_PIDS_LIMIT}"
    },
    "nproc": "${NPROC}",
    "kernel": "${KERNEL}",
    "backing_filesystem": "${BACKING_FS}"
  },
  "workload": {
    "append_record_bytes": ${RECORD_BYTES},
    "append_durability": "write(2)+fsync(2) per record (one durability barrier per record)",
    "append_seconds_per_arm": ${APPEND_SECONDS},
    "append_writers": [$(echo "$WRITERS" | tr ' ' ',')],
    "read_seconds_per_arm": ${READ_SECONDS},
    "read_readers": [$(echo "$READERS" | tr ' ' ',')],
    "read_block_sizes": [$(echo "$BLOCK_SIZES" | tr ' ' ',')],
    "read_file_mib": ${READ_MIB_JSON},
    "max_append_mib_per_writer": ${MAX_APPEND_MIB_JSON},
    "seed": ${SEED},
    "bounded": true
  },
  "segment_size_bytes": ${SEGMENT_SIZE},
  "systems": {
    "OrFS-GO": {
      "mounted": true,
      "binary_sha256": "${GO_SHA}",
      "mount_command": "orfs mount <vol> <mnt> --direct-io  (ORFS_GROUP_COMMIT unset)",
      "fuse_library": "hanwen/go-fuse/v2 v2.12.0",
      "transport": "f0 (default multi-reader go-fuse loop)",
      "direct_io": true,
      "fsync_policy": "one durability barrier per publish; group commit OFF (plain mount)",
      "backing_io": "synchronous pwrite/pread",
      "daemon_stderr": "${GO_STDERR}"
    },
    "OrFS-Rust": {
      "mounted": $( [ "$RS_STATUS" = "mounted" ] && echo true || echo false ),
      "status": "${RS_STATUS}",
      "version": "${RS_VERSION}",
      "binary_sha256": "${RS_SHA}",
      "mount_command": "orfs mount <vol> <mnt> --direct-io --durability sync --backing-io sync",
      "fuse_library": "fuser crate (classical /dev/fuse)",
      "transport": "fuse (classical)",
      "direct_io": true,
      "fsync_policy": "--durability sync: fsync per transaction",
      "backing_io": "synchronous (--backing-io sync; the Rust default on this host is io_uring)",
      "daemon_stderr": "${RS_STDERR}"
    },
    "SeaweedFS": {
      "mounted": $( [ "$SW_STATUS" = "mounted" ] && echo true || echo false ),
      "status": "${SW_STATUS}",
      "version": "${SW_VERSION}",
      "image": "${SEAWEEDFS_IMAGE}",
      "image_digest": "${SEAWEEDFS_DIGEST}",
      "server_command": "weed server -dir <data> -filer=true -ip=127.0.0.1 -master.port=9333 -volume.port=8080 -filer.port=8888 -master.volumeSizeLimitMB=1024 -volume.max=0 -master.electionTimeout=2s",
      "mount_command": "weed mount -filer=127.0.0.1:8888 -dir <mnt> -cacheDir <dir> -filer.path=/ -concurrentWriters=128",
      "go_runtime_env": "GOGC=${SW_GOGC} GOMEMLIMIT=${SW_GOMEMLIMIT} on both weed processes (bounds each inside the shared ${CAP_MEM_LIMIT} cap; at default GC the client heap exceeded the cap and the volume server was OOM-killed mid-arm)",
      "fuse_library": "hanwen/go-fuse/v2 (the same FUSE userspace library OrFS-GO uses)",
      "transport": "f0-classical /dev/fuse (go-fuse; FUSE-over-io_uring not negotiated)",
      "direct_io": false,
      "fsync_policy": "weed mount's Fsync flushes the client's dirty pages to the co-located volume server (HTTP PUT over loopback) and commits filer metadata over gRPC; it does not fsync the volume .dat or the filer store, so it is NOT a durable per-record barrier (see not_comparable).",
      "data_path": "client/server over loopback: FUSE client -> volume server (HTTP) + filer (gRPC); warm reads are served from the client's in-process chunk cache (default -cacheCapacityMB 128 >= the 64 MiB read file)",
      "backing_io": "the SeaweedFS volume server writes its own .dat/.idx files under -dir; no per-write fsync is performed",
      "daemon_stderr": "${SW_STDERR}"
    }
  },
  "differences": [
    "FUSE userspace library: OrFS-GO and SeaweedFS both use hanwen/go-fuse/v2; OrFS-Rust uses the fuser crate. All three speak classical /dev/fuse with one request per operation, but they are independent daemons.",
    "Transport shape differs: Go serves its default f0 multi-reader loop; Rust serves its classical fuse transport; SeaweedFS serves go-fuse. Same wire protocol, different daemon concurrency shapes.",
    "Backing I/O: the Go daemon writes synchronously (pwrite/pread); Rust was pinned to --backing-io sync to match, because its default on this host is io_uring; SeaweedFS's FUSE client PUTs data over loopback HTTP to a co-located volume server that writes its own .dat files.",
    "Durability is equal for the OrFS pair (both take one fsync per 4 KiB record) but NOT for SeaweedFS, whose Fsync is a flush-to-co-located-server, not a stable-storage barrier. See not_comparable.",
    "OrFS-GO and OrFS-Rust are both mounted --direct-io (FOPEN_DIRECT_IO); weed mount exposes no such knob. Recorded, not removed.",
    "All three use the same segment/record geometry and the same seeded 4 KiB record payload; the read file is the same size and the same deterministic content.",
    "Container caps are shared (the same container enforces them for all mounts), so no system can be given different resources. The systems' arms run strictly sequentially and a finished system's daemon and volume are released before the next system's arms, so no two systems compete for the shared memory cap.",
    "Within each system the read arms run before the append arms, on a quiet volume: the OrFS append arms retain unbounded history (non-withdrawal), so measuring appends first would evict the read file from the backing page cache and make every --direct-io read a backing I/O. All three systems are measured in the same order.",
    "Host-level variance (page cache state, kernel scheduler, backing device write cache) is not controlled beyond the shared container: run-to-run variance is expected and unquantified here."
  ],
  "not_comparable": [
    {
      "systems": ["SeaweedFS", "OrFS-GO", "OrFS-Rust"],
      "axis": "append durability",
      "reason": "The driver issues write(2)+fsync(2) per 4 KiB record on every system, but the systems do not honour it equally. OrFS-GO and OrFS-Rust implement fsync as a durable barrier (one fsync of the authoritative log per record). SeaweedFS 3.99's FUSE client implements Fsync as weed/mount/weedfs_file_sync.go: Fsync -> doFlush -> dirtyPages.FlushData, i.e. an HTTP PUT of the client's dirty pages to the co-located volume server plus a gRPC filer CreateEntry; it performs no fsync of the volume .dat file or the filer store, so it offers no documented per-record durability barrier. The SeaweedFS append rate is therefore NOT equal-durability with the OrFS rows and must not be read as such."
    },
    {
      "systems": ["SeaweedFS", "OrFS-GO", "OrFS-Rust"],
      "axis": "data path",
      "reason": "SeaweedFS is a client/server filesystem: even co-located, the FUSE client's data crosses the loopback network stack to the volume server (HTTP) and its metadata crosses gRPC to the filer, unlike the local in-process OrFS daemons. Its read path also serves warm reads from the client's in-process chunk cache, so a warm read never leaves the client process."
    },
    {
      "systems": ["SeaweedFS", "OrFS-GO", "OrFS-Rust"],
      "axis": "read-path caching shape",
      "reason": "The identical pread workload produced a very different shape for SeaweedFS: its 4 KiB random reads ran ~2k IOPS while its 64 KiB random reads ran ~0.3-1.3M IOPS (a ~100-600x gap), and a preceding warm 64 KiB pass did not change the 4 KiB rate (checked directly). The rows are published as measured; they are comparable as syscalls, but SeaweedFS's FUSE client serves some read sizes from an in-process chunk cache and others with a backing round trip, which is not the same data path as the OrFS daemons."
    },
    {
      "systems": ["SeaweedFS", "OrFS-GO", "OrFS-Rust"],
      "axis": "mount knobs (-direct-io)",
      "reason": "OrFS-GO and OrFS-Rust are both mounted --direct-io (FOPEN_DIRECT_IO); weed mount exposes no direct-I/O knob, so the direct-I/O parity the OrFS pair shares is unavailable for SeaweedFS. Recorded as a caveat rather than silently differing."
    },
    {
      "systems": ["SeaweedFS", "OrFS-GO", "OrFS-Rust"],
      "axis": "container memory budget",
      "reason": "The shared 2 GiB cap holds one OrFS daemon plus the driver, but must hold SeaweedFS's whole stack (server + FUSE client) plus the same driver. At SeaweedFS's default Go GC settings its client heap grew past the cap and the kernel OOM-killed the volume server mid-arm on an earlier run. The SeaweedFS processes are therefore run under GOGC=${SW_GOGC} GOMEMLIMIT=${SW_GOMEMLIMIT}; this is an environment-fit knob applied to SeaweedFS only and is recorded, not hidden."
    }
  ],
  "payload_size_bytes": ${RECORD_BYTES},
  "not_ranked": true,
  "arms": {
    "expected_per_system": ${ARMS_PER_SYS},
    "OrFS-GO": {"ran": $((ARMS_PER_SYS - GO_FAILED)), "failed": ${GO_FAILED}},
    "OrFS-Rust": {"ran": $((ARMS_PER_SYS - RS_FAILED)), "failed": ${RS_FAILED}},
    "SeaweedFS": ${SW_ARMS_JSON},
    "failed": [${FAILED_JSON}]
  },
  "note": "Three mounted storage systems driven by the same shared driver on the same machine and the same device: OrFS-GO, OrFS-Rust, and SeaweedFS's FUSE client. Durability is equal for the OrFS pair (one fsync per 4 KiB record) but not for SeaweedFS, whose Fsync is a flush-to-server, not a stable-storage barrier (see not_comparable) — its append rows are published but must not be read as equal-durability. The systems' arms run strictly sequentially and a finished system's daemon and volume are released before the next system's arms, so no two compete for the shared memory cap (this changes no measured arm). Differences above are the comparability caveats; no row is a ranking. A system that could not be run the same way is recorded as an asymmetry or a named SKIPPED, never a parity claim. The arms block names every coordinate that is missing for a system that mounted; a system that did not mount reports measured=false with its status."
}
EOF

# --- merge into the receipt ------------------------------------------------

# POSIX sh: the runs dir holds only succeeded arm receipts; an unmatched glob
# stays literal and is caught below.
set -- "$RUNS"/*.json
if [ ! -e "$1" ]; then
  echo "oss-compare-court: FAIL: no arm produced a run receipt" >&2
  exit 1
fi

OUT="$EVID/oss-compare.json"

# The systems the merge must see rows for: every system that MOUNTED. A system
# that did not mount is deliberately NOT in this list (it is recorded as
# measured=false / SKIPPED), so the guard catches a mounted system whose rows are
# missing — a silent hole — not a system that was never measured.
EXPECT="OrFS-GO"
[ "$RS_STATUS" = "mounted" ] && EXPECT="$EXPECT,OrFS-Rust"
[ "$SW_STATUS" = "mounted" ] && EXPECT="$EXPECT,SeaweedFS"

if "$OSSSUMMARY" -comparability "$COMP" -expect-systems "$EXPECT" -o "$OUT" "$@"; then
  echo "oss-compare-court: wrote $OUT"
else
  echo "oss-compare-court: FAIL: osscomparesummary could not write the receipt; comparability block was:" >&2
  cat "$COMP" >&2 || true
  exit 1
fi

rows=$(grep -o '"system"' "$OUT" 2>/dev/null | wc -l)
if [ "$rows" -eq 0 ]; then
  echo "oss-compare-court: FAIL: receipt has no rows" >&2
  exit 1
fi

# Honest verdict. A mounted system that lost an arm means the receipt is
# incomplete at known coordinates; that is a FAIL, never a PASS.
if [ "$GO_FAILED" -gt 0 ]; then
  echo "oss-compare-court: FAIL: OrFS-GO lost $GO_FAILED/$ARMS_PER_SYS arm(s); the receipt is partial" >&2
  echo "oss-compare-court: missing: $(tr '\n' ';' < "$WORK/failed-arms")" >&2
  exit 1
fi
if [ "$RS_STATUS" = "mounted" ] && [ "$RS_FAILED" -gt 0 ]; then
  echo "oss-compare-court: FAIL: OrFS-Rust lost $RS_FAILED/$ARMS_PER_SYS arm(s) (its daemon likely died mid-run); the receipt is partial" >&2
  echo "oss-compare-court: missing: $(tr '\n' ';' < "$WORK/failed-arms")" >&2
  exit 1
fi
if [ "$SW_STATUS" = "mounted" ] && [ "$SW_FAILED" -gt 0 ]; then
  echo "oss-compare-court: FAIL: SeaweedFS lost $SW_FAILED/$ARMS_PER_SYS arm(s); the receipt is partial" >&2
  echo "oss-compare-court: missing: $(tr '\n' ';' < "$WORK/failed-arms")" >&2
  exit 1
fi

_measured="OrFS-GO"
[ "$RS_STATUS" = "mounted" ] && _measured="$_measured, OrFS-Rust"
[ "$SW_STATUS" = "mounted" ] && _measured="$_measured, SeaweedFS"
echo "oss-compare-court: PASS ($rows row(s); $ARMS_PER_SYS/$ARMS_PER_SYS arms on each of: $_measured)"
if [ "$SW_STATUS" != "mounted" ]; then
  echo "oss-compare-court: SKIPPED SeaweedFS (status=$SW_STATUS) — recorded in the comparability block, NOT measured" >&2
fi
if [ "$RS_STATUS" != "mounted" ]; then
  echo "oss-compare-court: NOTE: OrFS-Rust was not mounted (status=$RS_STATUS) — recorded as a caveat"
fi
