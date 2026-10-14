#!/bin/sh
# Btrfs retained-snapshots retention court — runs *inside* a privileged court
# container as root (loop devices + mkfs.btrfs are required).
#
# The court answers a RETENTION question, not an IOPS question: can a system
# retain every committed historical transition while achieving comparable tail
# latency, throughput and physical-space economics?
#
#   - OrFS retains history in its authoritative log (non-withdrawal, Law I):
#     every committed transition is kept, with no extra snapshot step.
#   - Btrfs retains it with a read-only snapshot taken after every epoch.
#
# One defined, bounded epoch workload (append 4 KiB records with write(2)+fsync(2)
# per record) drives both. After each epoch the Btrfs arm takes a read-only
# snapshot, so every committed state is retained. The court measures, per epoch:
#
#   * current-state append latency/throughput (exact p50/p95/p99/p99.9);
#   * current-state read latency/throughput;
#   * historical-state (snapshot) read latency/throughput (Btrfs only);
#   * physical vs logical bytes (`btrfs filesystem usage` + `btrfs filesystem du`);
#   * the cost of creating a snapshot (`btrfs subvolume snapshot -r`).
#
# It composes evidence/retention-btrfs.json (schema orfs.retention.btrfs.v1) via
# cmd/retentionsummary. It never ranks rows. Every axis it cannot measure the same
# way on both systems is recorded as an explicit non-comparability, never as a
# fake parity: OrFS-GO serves no named historical root in this build, so the
# historical-state read axis is Btrfs-only.
#
# SKIPPED (a precise skip, never a false pass) when mkfs.btrfs/btrfs tooling is
# absent, the loop image cannot be created/mounted, or the court is not root.
#
# Env:
#   ORFS / RETENTIONBENCH / RETENTIONSUMMARY   binary paths
#   ORFS_EVIDENCE_DIR                          receipt root   (default /evidence)
#   SEGMENT_SIZE                               OrFS segment bytes (default 8 MiB)
#   IMG_MIB                                    loop image size  (default 512)
#   EPOCHS                                     epochs / retained states (default 8)
#   WRITERS                                    concurrent writers (default 1)
#   RECORDS_PER_EPOCH                          records per writer per epoch (256)
#   RECORD_BYTES                               record size (default 4096)
#   APPEND_SECONDS                             per-epoch safety time bound (30)
#   READ_SECONDS / READERS / BLOCK_SIZE        read arm knobs (1 / 1 / 4096)
#   READ_EPOCHS                                "all" or a space list of epochs to read
#   SEED                                       deterministic seed (default 1)
#   ORFS_ARM                                   run the OrFS-GO side (default 1)
set -eu
LC_ALL=C
export LC_ALL

ORFS=${ORFS:-/out/orfs}
BENCH=${RETENTIONBENCH:-/out/retentionbench}
SUMMARY=${RETENTIONSUMMARY:-/out/retentionsummary}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}

SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}
IMG_MIB=${IMG_MIB:-512}
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

CAP_MEM_LIMIT=${CAP_MEM_LIMIT:-2g}
CAP_MEMSWAP_LIMIT=${CAP_MEMSWAP_LIMIT:-2g}
CAP_CPUS=${CAP_CPUS:-4}
CAP_PIDS_LIMIT=${CAP_PIDS_LIMIT:-1024}

# --- environment guards: a precise SKIPPED, never a false pass --------------

if [ ! -x "$BENCH" ] || [ ! -x "$SUMMARY" ]; then
  echo "retention-btrfs-court: SKIPPED (missing $BENCH or $SUMMARY)"
  exit 0
fi
if [ "$(id -u)" != 0 ]; then
  echo "retention-btrfs-court: SKIPPED (must run as root to mount loop devices)"
  exit 0
fi
if ! command -v mkfs.btrfs >/dev/null 2>&1 || ! command -v btrfs >/dev/null 2>&1; then
  echo "retention-btrfs-court: SKIPPED (btrfs-progs is not installed)"
  exit 0
fi
if [ ! -e /dev/loop-control ]; then
  echo "retention-btrfs-court: SKIPPED (no /dev/loop-control; loop devices unavailable)"
  exit 0
fi

mkdir -p "$EVID"
WORK=$(mktemp -d)
RUNS="$WORK/measures"; SPACE="$WORK/space"; SNAPS="$WORK/snaps"
mkdir -p "$RUNS" "$SPACE" "$SNAPS"

# All mount/loop state is torn down on every exit path.
BTRFS_LOOP=""; BTRFS_MNT=""; BTRFS_MOUNTED=0
ORFS_LOOP=""; ORFS_BMNT=""; ORFS_MNT=""; ORFS_VOL=""; ORFS_PID=""
ORFS_BTRFS_MOUNTED=0; ORFS_DAEMON=0; ORFS_MOUNTED_OK=0

cleanup() {
  set +e
  if [ "$ORFS_DAEMON" = 1 ]; then
    "$ORFS" unmount "$ORFS_MNT" >/dev/null 2>&1 || umount -l "$ORFS_MNT" 2>/dev/null
  fi
  if [ -n "$ORFS_PID" ]; then
    kill "$ORFS_PID" 2>/dev/null
    # Wait (bounded) for the daemon to exit so its btrfs backing can be unmounted
    # before the work directory is removed.
    _w=0
    while [ "$_w" -lt 50 ] && kill -0 "$ORFS_PID" 2>/dev/null; do
      _w=$((_w + 1))
      sleep 0.1
    done
  fi
  [ "$ORFS_BTRFS_MOUNTED" = 1 ] && umount "$ORFS_BMNT" 2>/dev/null
  [ "$BTRFS_MOUNTED" = 1 ] && umount "$BTRFS_MNT" 2>/dev/null
  [ -n "$ORFS_LOOP" ] && losetup -d "$ORFS_LOOP" 2>/dev/null
  [ -n "$BTRFS_LOOP" ] && losetup -d "$BTRFS_LOOP" 2>/dev/null
  rm -rf "$WORK" 2>/dev/null
}
trap cleanup EXIT INT TERM

wait_mount() { # $1 = mountpoint; ~10s budget
  _w=0
  while [ "$_w" -lt 100 ]; do
    mountpoint -q "$1" && return 0
    _w=$((_w + 1))
    sleep 0.1
  done
  return 1
}

# bring_up_btrfs IMG MNT — create an image, mkfs.btrfs, and mount it. Echoes the
# loop device on success; returns non-zero on any failure (the caller decides
# whether that is a SKIP).
bring_up_btrfs() {
  _img=$1; _mnt=$2
  mkdir -p "$_mnt"
  dd if=/dev/zero of="$_img" bs=1048576 count="$IMG_MIB" status=none >/dev/null 2>&1 || return 1
  _loop=$(losetup -fP --show "$_img" 2>/dev/null) || return 1
  [ -n "$_loop" ] || return 1
  mkfs.btrfs -q -f "$_loop" >/dev/null 2>&1 || { losetup -d "$_loop" 2>/dev/null; return 1; }
  mount "$_loop" "$_mnt" >/dev/null 2>&1 || { losetup -d "$_loop" 2>/dev/null; return 1; }
  printf '%s' "$_loop"
  return 0
}

# --- the Btrfs arm: the comparator that retains history with snapshots ------

echo "retention-btrfs-court: bringing up the Btrfs loop image (${IMG_MIB} MiB)"
BTRFS_IMG="$WORK/btrfs.img"
BTRFS_MNT="$WORK/btrfs-mnt"
if ! BTRFS_LOOP=$(bring_up_btrfs "$BTRFS_IMG" "$BTRFS_MNT"); then
  echo "retention-btrfs-court: SKIPPED (could not create/mount a btrfs loop image; loop devices or mkfs.btrfs unavailable)"
  exit 0
fi
BTRFS_MOUNTED=1
BTRFS_LIVE="$BTRFS_MNT/live"
BTRFS_SNAPDIR="$BTRFS_MNT/snapshots"
if ! btrfs subvolume create "$BTRFS_LIVE" >/dev/null 2>&1; then
  echo "retention-btrfs-court: SKIPPED (btrfs subvolume create failed)"
  exit 0
fi
mkdir -p "$BTRFS_SNAPDIR"

BTRFS_RAN=0; BTRFS_FAILED=0
ORFS_RAN=0; ORFS_FAILED=0

# should_read EPOCH — honour READ_EPOCHS.
should_read() {
  [ "$READ_EPOCHS" = all ] && return 0
  for _e in $READ_EPOCHS; do [ "$_e" = "$1" ] && return 0; done
  return 1
}

# --- OrFS-GO arm: the same retention axes where they are comparable ---------

ORFS_STATUS="not_attempted"
if [ "$ORFS_ARM" = 1 ] && [ -x "$ORFS" ]; then
  echo "retention-btrfs-court: bringing up the OrFS-GO arm (loop btrfs backing)"
  ORFS_IMG="$WORK/orfs.img"
  ORFS_BMNT="$WORK/orfs-btrfs-mnt"
  ORFS_MNT="$WORK/orfs-mnt"
  ORFS_VOL="$ORFS_BMNT/vol"
  mkdir -p "$ORFS_MNT"
  if ORFS_LOOP=$(bring_up_btrfs "$ORFS_IMG" "$ORFS_BMNT"); then
    ORFS_BTRFS_MOUNTED=1
    if ! "$ORFS" init "$ORFS_VOL" --segment-size "$SEGMENT_SIZE" >/dev/null 2>"$WORK/orfs-init.err"; then
      ORFS_STATUS="init_failed"
      echo "retention-btrfs-court: NOTE: OrFS-GO init failed; the OrFS-GO half is skipped" >&2
      cat "$WORK/orfs-init.err" >&2 || true
    else
      # Group commit OFF: one durability barrier per record, as in the btrfs arm.
      ORFS_GROUP_COMMIT= "$ORFS" mount "$ORFS_VOL" "$ORFS_MNT" --direct-io >"$WORK/orfs-mount.err" 2>&1 &
      ORFS_PID=$!
      if wait_mount "$ORFS_MNT"; then
        ORFS_DAEMON=1
        ORFS_MOUNTED_OK=1
        ORFS_STATUS="mounted"
        echo "retention-btrfs-court: OrFS-GO mounted at $ORFS_MNT"
      else
        ORFS_STATUS="mount_failed"
        echo "retention-btrfs-court: NOTE: OrFS-GO mount did not come up; the OrFS-GO half is skipped" >&2
        cat "$WORK/orfs-mount.err" >&2 || true
        kill -9 "$ORFS_PID" 2>/dev/null || true
        ORFS_PID=""
      fi
    fi
  else
    ORFS_STATUS="loop_unavailable"
    echo "retention-btrfs-court: NOTE: could not bring up the OrFS-GO btrfs backing; the OrFS-GO half is skipped" >&2
  fi
fi

# Prepare writer files outside the measured windows.
if ! "$BENCH" prepare -dir "$BTRFS_LIVE" -prefix append -writers "$WRITERS" >/dev/null 2>&1; then
  echo "retention-btrfs-court: FAIL: prepare on the Btrfs arm failed" >&2
  exit 1
fi
if [ "$ORFS_DAEMON" = 1 ]; then
  "$BENCH" prepare -dir "$ORFS_MNT" -prefix append -writers "$WRITERS" >/dev/null 2>&1 || true
fi

# --- the epoch loop ---------------------------------------------------------

e=0
while [ "$e" -lt "$EPOCHS" ]; do
  echo "retention-btrfs-court: btrfs epoch $e (records=$RECORDS_PER_EPOCH writers=$WRITERS)"

  # 1. the defined append epoch, on the Btrfs arm.
  if "$BENCH" append -dir "$BTRFS_LIVE" -system Btrfs -prefix append \
      -writers "$WRITERS" -records "$RECORDS_PER_EPOCH" -record-bytes "$RECORD_BYTES" \
      -seconds "$APPEND_SECONDS" -epoch "$e" -seed "$SEED" \
      -out "$RUNS/btrfs-append-e$e.json" >/dev/null 2>&1; then
    BTRFS_RAN=$((BTRFS_RAN + 1))
  else
    BTRFS_FAILED=$((BTRFS_FAILED + 1))
    echo "retention-btrfs-court: WARN: btrfs append epoch $e failed; recorded as missing" >&2
  fi

  # 2. physical vs logical bytes of the current committed state.
  "$BENCH" space -kind btrfs -system Btrfs -mnt "$BTRFS_MNT" -subvol "$BTRFS_LIVE" \
    -epoch "$e" -state current -out "$SPACE/btrfs-current-e$e.json" >/dev/null 2>&1 \
    || echo "retention-btrfs-court: WARN: btrfs current space epoch $e failed" >&2

  # 3. retain the committed state as a read-only snapshot, timed.
  sync
  if "$BENCH" snapshot -live "$BTRFS_LIVE" -dest "$BTRFS_SNAPDIR/snap-$e" \
      -system Btrfs -epoch "$e" -out "$SNAPS/btrfs-snap-e$e.json" >/dev/null 2>&1; then
    :
  else
    BTRFS_FAILED=$((BTRFS_FAILED + 1))
    echo "retention-btrfs-court: WARN: btrfs snapshot epoch $e failed; recorded as missing" >&2
  fi

  # 4. physical vs logical bytes of the retained snapshot.
  "$BENCH" space -kind btrfs -system Btrfs -mnt "$BTRFS_MNT" -subvol "$BTRFS_SNAPDIR/snap-$e" \
    -epoch "$e" -state snapshot -out "$SPACE/btrfs-snapshot-e$e.json" >/dev/null 2>&1 \
    || echo "retention-btrfs-court: WARN: btrfs snapshot space epoch $e failed" >&2

  # 5. current-state and historical-state reads.
  if should_read "$e"; then
    if "$BENCH" read -dir "$BTRFS_LIVE" -system Btrfs -state current -epoch "$e" \
        -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
        -seed "$SEED" -out "$RUNS/btrfs-read-current-e$e.json" >/dev/null 2>&1; then
      BTRFS_RAN=$((BTRFS_RAN + 1))
    else
      BTRFS_FAILED=$((BTRFS_FAILED + 1))
      echo "retention-btrfs-court: WARN: btrfs current read epoch $e failed" >&2
    fi
    if "$BENCH" read -dir "$BTRFS_SNAPDIR/snap-$e" -system Btrfs -state snapshot -epoch "$e" \
        -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
        -seed "$SEED" -out "$RUNS/btrfs-read-snapshot-e$e.json" >/dev/null 2>&1; then
      BTRFS_RAN=$((BTRFS_RAN + 1))
    else
      BTRFS_FAILED=$((BTRFS_FAILED + 1))
      echo "retention-btrfs-court: WARN: btrfs snapshot read epoch $e failed" >&2
    fi
  fi

  # 6. the OrFS-GO side of the same axes where comparable: current append, the
  # authoritative physical bytes, and a current-state read. No snapshot step:
  # OrFS retains every committed transition in the log (that is the point).
  if [ "$ORFS_DAEMON" = 1 ]; then
    if "$BENCH" append -dir "$ORFS_MNT" -system OrFS-GO -prefix append \
        -writers "$WRITERS" -records "$RECORDS_PER_EPOCH" -record-bytes "$RECORD_BYTES" \
        -seconds "$APPEND_SECONDS" -epoch "$e" -seed "$SEED" \
        -out "$RUNS/orfs-append-e$e.json" >/dev/null 2>&1; then
      ORFS_RAN=$((ORFS_RAN + 1))
    else
      ORFS_FAILED=$((ORFS_FAILED + 1))
      echo "retention-btrfs-court: WARN: OrFS-GO append epoch $e failed" >&2
    fi
    "$BENCH" space -kind dir -system OrFS-GO -subvol "$ORFS_MNT" -physical-dir "$ORFS_VOL/or.log" \
      -epoch "$e" -state current -out "$SPACE/orfs-current-e$e.json" >/dev/null 2>&1 \
      || echo "retention-btrfs-court: WARN: OrFS-GO space epoch $e failed" >&2
    if should_read "$e"; then
      if "$BENCH" read -dir "$ORFS_MNT" -system OrFS-GO -state current -epoch "$e" \
          -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
          -seed "$SEED" -out "$RUNS/orfs-read-current-e$e.json" >/dev/null 2>&1; then
        ORFS_RAN=$((ORFS_RAN + 1))
      else
        ORFS_FAILED=$((ORFS_FAILED + 1))
        echo "retention-btrfs-court: WARN: OrFS-GO read epoch $e failed" >&2
      fi
    fi
  fi

  e=$((e + 1))
done

# Release the OrFS daemon from its mount before composing the receipt; the daemon
# process is still killed (and its backing unmounted) by cleanup.
if [ "$ORFS_DAEMON" = 1 ]; then
  "$ORFS" unmount "$ORFS_MNT" >/dev/null 2>&1 || umount -l "$ORFS_MNT" 2>/dev/null || true
  ORFS_DAEMON=0
fi

# --- comparability block ----------------------------------------------------

json_escape() {
  sed -e 's/\x1b\[[0-9;]*[A-Za-z]//g' \
  | LC_ALL=C tr -c '[:print:]' ' ' \
  | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^ *//' -e 's/ *$//'
}
stderr_tail() { [ -f "$1" ] && tail -c 2000 "$1" | json_escape || true; }
sha256_of() { [ -f "$1" ] && sha256sum "$1" | cut -d' ' -f1 || echo "unavailable"; }

BENCH_SHA=$(sha256_of "$BENCH")
ORFS_SHA=$(sha256_of "$ORFS")
ORFS_STDERR=$(stderr_tail "$WORK/orfs-mount.err")
KERNEL=$(uname -sr | json_escape)
NPROC=$(nproc 2>/dev/null || echo "?")
BACKING_FS=$(stat -f -c %T "$WORK" 2>/dev/null || echo "?")
BTRFS_SNAPS_CREATED=$(ls "$SNAPS" 2>/dev/null | wc -l | tr -d ' ')

# expected arms: Btrfs = EPOCHS appends + EPOCHS current reads + EPOCHS snapshot
# reads; OrFS-GO = EPOCHS appends + EPOCHS current reads (no snapshot arms).
N_READ_EPOCHS=$EPOCHS
if [ "$READ_EPOCHS" != all ]; then
  N_READ_EPOCHS=0
  for _e in $READ_EPOCHS; do N_READ_EPOCHS=$((N_READ_EPOCHS + 1)); done
fi
BTRFS_EXPECTED=$((EPOCHS + 2 * N_READ_EPOCHS))
ORFS_EXPECTED=$((EPOCHS + N_READ_EPOCHS))

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
    "backing_filesystem": "${BACKING_FS}",
    "comparator_filesystem": "btrfs (loop-mounted ${IMG_MIB} MiB image)"
  },
  "question": "Can a system retain every committed historical transition while achieving comparable tail latency, throughput and physical-space economics?",
  "workload": {
    "epochs": ${EPOCHS},
    "writers": ${WRITERS},
    "records_per_epoch_per_writer": ${RECORDS_PER_EPOCH},
    "record_bytes": ${RECORD_BYTES},
    "append_durability": "write(2)+fsync(2) per record (one durability barrier per record)",
    "append_seconds_safety_bound": ${APPEND_SECONDS},
    "read_seconds_per_arm": ${READ_SECONDS},
    "readers": ${READERS},
    "block_size": ${BLOCK_SIZE},
    "seed": ${SEED},
    "bounded": true
  },
  "segment_size_bytes": ${SEGMENT_SIZE},
  "systems": {
    "Btrfs": {
      "ran": true,
      "retention": "read-only snapshot after every epoch (btrfs subvolume snapshot -r)",
      "binary_sha256": "${BENCH_SHA}",
      "snapshots_expected": ${EPOCHS},
      "snapshots_created": ${BTRFS_SNAPS_CREATED}
    },
    "OrFS-GO": {
      "mounted": $( [ "$ORFS_MOUNTED_OK" = 1 ] && echo true || echo false ),
      "status": "${ORFS_STATUS}",
      "retention": "every committed transition retained in the authoritative log (or.log); no snapshot step",
      "mount_command": "orfs mount <vol> <mnt> --direct-io  (ORFS_GROUP_COMMIT unset)",
      "fuse_library": "hanwen/go-fuse/v2 v2.12.0",
      "binary_sha256": "${ORFS_SHA}",
      "snapshots_created": 0,
      "daemon_stderr": "${ORFS_STDERR}"
    }
  },
  "comparable_axes": [
    "current-state append: the same defined epoch (one write(2)+fsync(2) per record) over a loop-mounted btrfs backing, measured at the syscall level on both systems.",
    "current-state read: block-aligned uniform-random pread of the current committed state; OrFS-GO adds its FUSE round trip.",
    "physical vs logical bytes: retained physical bytes per logical byte of data. Btrfs physical is the btrfs device Used; OrFS-GO physical is the authoritative or.log bytes."
  ],
  "not_comparable": [
    "historical-state read: Btrfs serves it from a read-only snapshot; OrFS-GO does not serve named historical roots in this build (docs/LIMITATIONS.md), so this axis is Btrfs-only. The OrFS-GO current-state read serves the same newest committed bytes.",
    "snapshot-creation cost: OrFS-GO has no snapshot step; it retains every committed transition at append time. The Btrfs snapshot-creation row has no OrFS-GO counterpart.",
    "snapshot extent sharing: Btrfs snapshots share unchanged extents, so a retained snapshot's exclusive bytes can be near zero. OrFS-GO's retained bytes are its appended log, which never shares extents; the two physical-bytes accounting schemes are therefore not the same computation even though both are measured.",
    "read latency is a native-vs-FUSE comparison: the Btrfs read is a native local filesystem read served from the page cache (no FUSE, no daemon), while the OrFS-GO read is a FUSE request plus a daemon pread on the backing. The Btrfs read IOPS therefore does not rank the OrFS-GO read; the gap is the cost of the FUSE data path, not a retention cost, and neither row is a comparison of retention mechanisms."
  ],
  "differences": [
    "The Btrfs arm writes directly to a loop-mounted btrfs subvolume; the OrFS-GO arm writes through its FUSE mount to a volume backed by its own loop-mounted btrfs image. Both backings are btrfs loop images of the same size.",
    "The OrFS-GO append arm pays the FUSE round trip and the daemon's durability barrier; the Btrfs arm pays only the kernel btrfs write+fsync. This difference is inherent to comparing a userspace filesystem against a kernel filesystem and is recorded, not removed.",
    "A btrfs snapshot is not free in principle: it costs a transaction commit and its metadata. The measured creation time is reported per epoch; a snapshot is only cheap because unchanged extents are shared.",
    "Host-level variance (page cache state, kernel scheduler, the backing device's write cache) is not controlled beyond the shared container; run-to-run variance is expected and unquantified here."
  ],
  "not_ranked": true,
  "arms": {
    "Btrfs": {"expected": ${BTRFS_EXPECTED}, "ran": ${BTRFS_RAN}, "failed": ${BTRFS_FAILED}},
    "OrFS-GO": {"expected": ${ORFS_EXPECTED}, "ran": ${ORFS_RAN}, "failed": ${ORFS_FAILED}, "mounted": $( [ "$ORFS_MOUNTED_OK" = 1 ] && echo true || echo false )}
  },
  "note": "One defined, bounded epoch workload drives a Btrfs subvolume that is snapshotted read-only after every epoch and an OrFS-GO volume that retains every committed transition in its log. The receipt is data, not a leaderboard. The comparable_axes and not_comparable lists above say exactly which axes can be compared and which cannot; no row is a ranking. Missing arms are named so a partial receipt is never a pass."
}
EOF

# --- merge into the receipt -------------------------------------------------

set -- "$RUNS"/*.json "$SPACE"/*.json "$SNAPS"/*.json
# An unmatched glob stays literal in POSIX sh; drop any literal that is not a file.
FILES=""
for f in "$@"; do
  [ -f "$f" ] && FILES="$FILES $f"
done
if [ -z "$FILES" ]; then
  echo "retention-btrfs-court: FAIL: no measurement produced a receipt" >&2
  exit 1
fi

OUT="$EVID/retention-btrfs.json"
# shellcheck disable=SC2086
if "$SUMMARY" -comparability "$COMP" -o "$OUT" $FILES; then
  echo "retention-btrfs-court: wrote $OUT"
else
  echo "retention-btrfs-court: FAIL: retentionsummary could not write the receipt" >&2
  cat "$COMP" >&2 || true
  exit 1
fi

rows=$(grep -o '"kind"' "$OUT" 2>/dev/null | wc -l)
if [ "$rows" -eq 0 ]; then
  echo "retention-btrfs-court: FAIL: receipt has no rows" >&2
  exit 1
fi

if [ "$BTRFS_FAILED" -gt 0 ]; then
  echo "retention-btrfs-court: FAIL: the Btrfs arm lost $BTRFS_FAILED arm(s); the receipt is partial" >&2
  exit 1
fi
if [ "$ORFS_STATUS" = "mounted" ] && [ "$ORFS_FAILED" -gt 0 ]; then
  echo "retention-btrfs-court: FAIL: OrFS-GO lost $ORFS_FAILED arm(s); the receipt is partial" >&2
  exit 1
fi
if [ "$ORFS_STATUS" = "mounted" ]; then
  echo "retention-btrfs-court: PASS ($rows row(s); Btrfs retained $EPOCHS snapshot(s); OrFS-GO measured on the comparable axes)"
else
  echo "retention-btrfs-court: PASS ($rows row(s); OrFS-GO NOT measured: status=$ORFS_STATUS — see the comparability caveat)"
fi
exit 0
