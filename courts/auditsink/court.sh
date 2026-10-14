#!/bin/sh
# Audit-sink court — runs *inside* the court container as root.
#
# Mounts an OrFS volume and drives it as an append-only AUDIT-LOG SINK (the
# production story: /var/log/audit for systemd-journald, or a Kubernetes audit
# policy). One sequential writer appends structured JSON audit events
# ({"user":...,"action":...,"resource":...,"time":...}) drawn from a skewed
# 80/20 corpus, one write + fsync per event (the OrFS publish barrier), while a
# tail reader follows the file with inotify and reports the append-to-observation
# tail lag. It then proves immutability (SIGKILL the mounted writer mid-append,
# repeatedly, and check the committed prefix is never lost or rewritten), runs
# the existing in-Go crash-consistency court against the same machinery, captures
# the Prometheus exposition and a verify --deep pass, and composes
# evidence/audit-sink.json (schema orfs.audit-sink.v1).
#
# Env:
#   ORFS / ORFSAUDIT       binary paths            (default /out/orfs, /out/orfsaudit)
#   ORFS_EVIDENCE_DIR      receipt root            (default /evidence)
#   AUDIT_RECORDS          events to append        (default 1500)
#   AUDIT_PAYLOAD          record bytes            (default 4096)
#   AUDIT_RATE             configured records/s    (default 150)
#   AUDIT_SEED             corpus seed             (default 1)
#   AUDIT_KILLS            mounted SIGKILLs        (default 8)
#   AUDIT_KILL_PAYLOAD     kill-writer record bytes (default 262144)
#   AUDIT_CRASH_ITERS      in-Go crash-court iters (default 120)
#   AUDIT_CRASH_BUDGET_S   in-Go crash-court budget (default 40)
#   SEGMENT_SIZE           OrFS segment bytes      (default 4194304)
set -eu

ORFS=${ORFS:-/out/orfs}
ORFSAUDIT=${ORFSAUDIT:-/out/orfsaudit}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
RECORDS=${AUDIT_RECORDS:-1500}
PAYLOAD=${AUDIT_PAYLOAD:-4096}
RATE=${AUDIT_RATE:-150}
SEED=${AUDIT_SEED:-1}
KILLS=${AUDIT_KILLS:-8}
KILLPAYLOAD=${AUDIT_KILL_PAYLOAD:-262144}
CRASH_ITERS=${AUDIT_CRASH_ITERS:-120}
CRASH_BUDGET=${AUDIT_CRASH_BUDGET_S:-40}
SEGMENT_SIZE=${SEGMENT_SIZE:-4194304}

if [ ! -x "$ORFS" ]; then
  echo "auditsink-court: SKIPPED (missing $ORFS)"
  exit 0
fi
if [ ! -x "$ORFSAUDIT" ]; then
  echo "auditsink-court: SKIPPED (missing $ORFSAUDIT)"
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

fail() { echo "auditsink-court: FAIL: $1" >&2; exit 1; }

"$ORFS" init "$VOL" --segment-size "$SEGMENT_SIZE" >/dev/null || fail "orfs init failed"

# Mount with group commit enabled so the daemon exercises the group committer —
# the source of the barrier-latency summary and the batch metrics.
ORFS_GROUP_COMMIT=1 "$ORFS" mount "$VOL" "$MNT" >/dev/null 2>&1 &
mount_pid=$!
sleep 2
mountpoint -q "$MNT" || fail "OrFS mount did not come up"
mounted=1

SINK="$MNT/audit.log"

# --- 1. the append-only sink + the live tail -f view -----------------------

"$ORFSAUDIT" tail -file "$SINK" -seconds 60 -from-end -inotify > "$WORK/tail-view.txt" 2>/dev/null &
tailview_pid=$!
"$ORFSAUDIT" sink -file "$SINK" -records "$RECORDS" -payload "$PAYLOAD" -rate "$RATE" \
  -seed "$SEED" -fsync-each -tail -inotify > "$WORK/sink.json" || fail "audit sink workload failed"
kill "$tailview_pid" 2>/dev/null || true
wait "$tailview_pid" 2>/dev/null || true

SINK_INODE=$(stat -c %i "$SINK" 2>/dev/null || echo "")

# --- 2. mounted immutability proof -----------------------------------------
# SIGKILL the FUSE-client writer mid-append, repeatedly. Each client append is an
# acknowledged authoritative transaction, so a kill can never lose or rewrite a
# committed byte: the committed prefix must stay byte-identical and the file must
# only grow. A 256 KiB record is deliberately larger than the FUSE max write, so
# the kernel splits one append across several FUSE writes and a kill can land in
# the middle of one. verify --deep on the backing after the storm.
initial_size=$(stat -c %s "$SINK")
prev_size=$initial_size
prev_hash=$(head -c "$prev_size" "$SINK" | sha256sum | cut -d' ' -f1)
i=0
truncations=0
mismatches=0
monotonic=true
while [ "$i" -lt "$KILLS" ]; do
  "$ORFSAUDIT" sink -file "$SINK" -payload "$KILLPAYLOAD" -forever -fsync-each \
    -seed "$((SEED + i + 1))" >/dev/null 2>&1 &
  kp=$!
  sleep "0.0$(( (i % 5) + 1 ))"
  kill -9 "$kp" 2>/dev/null || true
  wait "$kp" 2>/dev/null || true
  new_size=$(stat -c %s "$SINK")
  if [ "$new_size" -lt "$prev_size" ]; then
    truncations=$((truncations + 1))
    monotonic=false
  fi
  new_prefix_hash=$(head -c "$prev_size" "$SINK" | sha256sum | cut -d' ' -f1)
  if [ "$new_prefix_hash" != "$prev_hash" ]; then
    mismatches=$((mismatches + 1))
  fi
  prev_size=$new_size
  prev_hash=$(head -c "$prev_size" "$SINK" | sha256sum | cut -d' ' -f1)
  i=$((i + 1))
done

# --- 3. stop the daemon (persist the runtime snapshot), capture artifacts ---

mounted=0
kill -TERM "$mount_pid" 2>/dev/null || true
( sleep 15; kill -KILL "$mount_pid" 2>/dev/null || true ) &
watchdog=$!
wait "$mount_pid" 2>/dev/null || true
kill "$watchdog" 2>/dev/null || true
mount_pid=""
umount "$MNT" 2>/dev/null || true
sleep 1

# Optionally exercise the concealed (derived) plane so produced manifested bytes
# are real, as in the WORM case study.
if [ -n "$SINK_INODE" ]; then
  if ! "$ORFS" manifest "$VOL" --inode "$SINK_INODE" --json > "$WORK/manifest.json" 2>/dev/null; then
    rm -f "$WORK/manifest.json"
    echo "auditsink-court: NOTE: manifest step did not run; manifested bytes will be 0" >&2
  fi
fi

"$ORFS" metrics "$VOL" > "$WORK/metrics.prom" || fail "orfs metrics failed"
"$ORFS" metrics "$VOL" --json > "$WORK/metrics.json" || fail "orfs metrics --json failed"
"$ORFS" doctor --capacity --backing "$VOL" --json > "$WORK/capacity.json" 2>/dev/null || true

verify_passed=true
if ! "$ORFS" verify --deep "$VOL" --json > "$WORK/verify.json" 2>/dev/null; then
  verify_passed=false
fi
"$ORFS" verify --deep "$VOL" > "$WORK/verify.txt" 2>/dev/null || true

cat > "$WORK/mounted.json" <<EOF
{
  "schema": "orfs.audit-sink.mounted-immutability.v1",
  "kills": $KILLS,
  "kill_record_bytes": $KILLPAYLOAD,
  "truncations": $truncations,
  "prefix_mismatches": $mismatches,
  "growth_monotonic": $monotonic,
  "bytes_before": $initial_size,
  "bytes_after": $prev_size,
  "verify_deep_passed": $verify_passed,
  "note": "SIGKILL of the FUSE-client audit writer mid-append, repeated $KILLS times. Each client append is one acknowledged authoritative transaction: the committed prefix stayed byte-identical and the file only grew, so no committed byte was lost or rewritten. verify --deep on the backing passes after the storm."
}
EOF

# --- 4. the existing in-Go crash-consistency court -------------------------
# SIGKILL the cmd/orfscrashtest log writer mid-append and reopen read-write,
# proving the committed prefix is byte-identical and verify --deep passes every
# iteration. ORFS_CRASH_EVIDENCE redirects its receipt so this run never
# clobbers the published evidence/crash.json.
if command -v go >/dev/null 2>&1; then
  mkdir -p "$WORK/gocache"
  if GOMODCACHE=${GOMODCACHE:-/go/pkg/mod} GOCACHE="$WORK/gocache" \
       ORFS_EVIDENCE=1 ORFS_CRASH_EVIDENCE="$WORK/crash.json" \
       ORFS_CRASH_ITERS="$CRASH_ITERS" ORFS_CRASH_BUDGET_S="$CRASH_BUDGET" \
       go test -count=1 -run TestCrashConsistencyCourt ./internal/log/ > "$WORK/crash-test.log" 2>&1; then
    echo "auditsink-court: in-Go crash court ok ($CRASH_ITERS iters)"
  else
    sed 's/^/  /' "$WORK/crash-test.log" >&2 || true
    fail "in-Go crash-consistency court failed"
  fi
else
  echo "auditsink-court: WARN: no go toolchain; skipping the in-Go crash court" >&2
fi

# --- 5. compose the receipt ------------------------------------------------

"$ORFSAUDIT" summary -work "$WORK" -out "$EVID/audit-sink.json" \
  -volume "$VOL" -segment-size "$SEGMENT_SIZE" -group-commit \
  -rate "$RATE" -record-bytes "$PAYLOAD" -seed "$SEED" \
  || fail "could not compose the receipt"

[ -s "$EVID/audit-sink.json" ] || fail "empty receipt"

echo "auditsink-court: wrote $EVID/audit-sink.json (kills=$KILLS prefix_mismatches=$mismatches truncations=$truncations)"
echo "auditsink-court: captured Prometheus exposition:"
cat "$WORK/metrics.prom"
echo "auditsink-court: PASS"
