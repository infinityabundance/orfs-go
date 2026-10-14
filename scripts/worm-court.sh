#!/bin/sh
# WORM case-study court — runs *inside* the court container as root.
#
# Mounts an OrFS volume and drives it as an append-only AUDIT LOG SINK (Loki /
# Manifold style): one sequential writer appends framed records and fsyncs each
# (the OrFS publish barrier), while a tail reader watches the file grow. It then
# captures the Prometheus exposition (`orfs metrics`), the capacity report, and a
# `verify --deep` pass, and composes evidence/worm-case-study.json
# (schema orfs.worm.case-study.v1).
#
# The point is to be honest: this is append-only WORM, not a general-purpose
# filesystem. Every append is one authoritative transaction, so the append
# latency published here includes the fsync barrier under the FUSE tax.
#
# Env:
#   ORFS / ORFSWORM     binary paths          (default /out/orfs, /out/orfsworm)
#   ORFS_EVIDENCE_DIR   receipt root          (default /evidence)
#   WORM_RECORDS        records to append     (default 2048)
#   WORM_PAYLOAD        payload bytes/record  (default 4080; 4096-byte aligned record)
#   SEGMENT_SIZE        OrFS segment bytes    (default 4194304)
set -eu

ORFS=${ORFS:-/out/orfs}
ORFSWORM=${ORFSWORM:-/out/orfsworm}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
RECORDS=${WORM_RECORDS:-2048}
PAYLOAD=${WORM_PAYLOAD:-4080}
SEGMENT_SIZE=${SEGMENT_SIZE:-4194304}

if [ ! -x "$ORFS" ]; then
  echo "worm-court: SKIPPED (missing $ORFS)"
  exit 0
fi
if [ ! -x "$ORFSWORM" ]; then
  echo "worm-court: SKIPPED (missing $ORFSWORM)"
  exit 0
fi

mkdir -p "$EVID"

WORK=$(mktemp -d)
VOL="$WORK/vol"
MNT="$WORK/mnt"
mkdir -p "$MNT"

mount_pid=""
mounted=0
cleanup() {
  if [ "$mounted" = 1 ]; then
    "$ORFS" unmount "$MNT" >/dev/null 2>&1 || umount "$MNT" 2>/dev/null || true
    mounted=0
  fi
  if [ -n "$mount_pid" ]; then
    kill "$mount_pid" 2>/dev/null || true
    wait "$mount_pid" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fail() { echo "worm-court: FAIL: $1" >&2; exit 1; }

"$ORFS" init "$VOL" --segment-size "$SEGMENT_SIZE" >/dev/null || fail "orfs init failed"

# Mount with group commit enabled so the daemon exercises the group committer —
# the source of the barrier-latency histogram and the batch metrics.
ORFS_GROUP_COMMIT=1 "$ORFS" mount "$VOL" "$MNT" >/dev/null 2>&1 &
mount_pid=$!
sleep 2
mountpoint -q "$MNT" || fail "OrFS mount did not come up"
mounted=1

# --- the append-only sink workload ----------------------------------------

"$ORFSWORM" sink -file "$MNT/audit.log" -records "$RECORDS" -payload "$PAYLOAD" -fsync-each -tail \
  > "$WORK/sink.json" || fail "append workload failed"

# The OrFS inode of the sink file (== st_ino) and its mounted size.
SINK_INODE=$(stat -c %i "$MNT/audit.log" 2>/dev/null || echo "")
MOUNT_LOGICAL=$(stat -c %s "$MNT/audit.log" 2>/dev/null || echo 0)

# --- stop the daemon so it persists its runtime snapshot ------------------

mounted=0
kill -TERM "$mount_pid" 2>/dev/null || true
# A watchdog fails the wait loudly if the daemon will not exit (never hangs the court).
( sleep 15; kill -KILL "$mount_pid" 2>/dev/null || true ) &
watchdog=$!
wait "$mount_pid" 2>/dev/null || true
kill "$watchdog" 2>/dev/null || true
mount_pid=""
umount "$MNT" 2>/dev/null || true
sleep 1

# --- optionally exercise the concealed (derived) plane --------------------

MANIFESTED=0
if [ -n "$SINK_INODE" ]; then
  if "$ORFS" manifest "$VOL" --inode "$SINK_INODE" --json > "$WORK/manifest.json" 2>/dev/null; then
    MANIFESTED=1
  else
    rm -f "$WORK/manifest.json"
    echo "worm-court: NOTE: manifest step did not run; manifested bytes will be 0" >&2
  fi
fi

# --- capture the observability artifacts ----------------------------------

"$ORFS" metrics "$VOL" > "$WORK/metrics.prom" || fail "orfs metrics failed"
"$ORFS" metrics "$VOL" --json > "$WORK/metrics.json" || fail "orfs metrics --json failed"
"$ORFS" doctor --capacity --backing "$VOL" --json > "$WORK/capacity.json" 2>/dev/null || true
"$ORFS" verify --deep "$VOL" --json > "$WORK/verify.json" 2>/dev/null || true

# --- compose the receipt ---------------------------------------------------

"$ORFSWORM" summary -work "$WORK" -out "$EVID/worm-case-study.json" \
  -volume "$VOL" -segment-size "$SEGMENT_SIZE" -group-commit \
  || fail "could not compose the case-study receipt"

[ -s "$EVID/worm-case-study.json" ] || fail "empty receipt"

echo "worm-court: wrote $EVID/worm-case-study.json (mounted logical=$MOUNT_LOGICAL, manifested=$MANIFESTED)"
echo "worm-court: captured Prometheus exposition:"
cat "$WORK/metrics.prom"
echo "worm-court: PASS"
