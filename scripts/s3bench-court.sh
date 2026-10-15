#!/bin/sh
# The WORM S3 court — runs *inside* the court container as root.
#
# It compares two WORM-capable S3 surfaces with ONE client and ONE operation set:
#
#   OrFS-S3      cmd/orfs-s3: an in-process S3 gateway over an append-only OrFS
#                volume. No FUSE in the data path; a PUT is an authoritative
#                append.
#   MinIO        a client/server S3 server over its own local storage (pinned
#                image), the system the world actually buys WORM with today.
#
# It also runs a WORM CORRECTNESS arm against the gateway: PUT with retention,
# attempt DELETE (must be 403), prove the object survives, and scrape the
# gateway's breach/quarantine counters.
#
# The receipt is data, not a leaderboard. Rows are keyed
# {system, mode, concurrency, object_bytes} and are never ranked across
# incomparable coordinates.
#
# Env: S3LOAD / ORFSS3 / MINIO binaries; DURATION; CONCURRENCY; OBJECT_BYTES;
#      ORFS_EVIDENCE_DIR; the shared container caps.
set -eu

S3LOAD=${S3LOAD:-/out/s3load}
ORFSS3=${ORFSS3:-/out/orfs-s3}
MINIO=${MINIO:-/out/minio}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}

DURATION=${DURATION:-5}
CONCURRENCY=${CONCURRENCY:-"1 64"}
OBJECT_BYTES=${OBJECT_BYTES:-"4096 1048576"}
GET_BYTES=${GET_BYTES:-4096}
SEED=${SEED:-1}
MAX_PUT_MIB=${MAX_PUT_MIB:-256}
SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}
MINIO_ARM=${MINIO_ARM:-1}
MINIO_NOTE=${MINIO_NOTE:-}
S3BENCH_OUT=${S3BENCH_OUT:-$EVID/s3bench.json}
# GW_MANIFEST: "on" (default) mounts the manifested read path (-manifest); "off"
# omits it, so the two configurations can be measured separately as the design
# requires (the manifested body publication is a per-PUT cost worth isolating).
GW_MANIFEST=${GW_MANIFEST:-on}

CAP_MEM_LIMIT=${CAP_MEM_LIMIT:-2g}
CAP_MEMSWAP_LIMIT=${CAP_MEMSWAP_LIMIT:-2g}
CAP_CPUS=${CAP_CPUS:-4}
CAP_PIDS_LIMIT=${CAP_PIDS_LIMIT:-1024}

GW_ADDR=127.0.0.1:9000
MINIO_ADDR=127.0.0.1:9100
GW_ACCESS=orfsbench
GW_SECRET=orfsbenchsecret
MINIO_ACCESS=minioadmin
MINIO_SECRET=minioadmin

if [ ! -x "$S3LOAD" ]; then
  echo "s3bench-court: SKIPPED (missing $S3LOAD)"
  exit 0
fi
if [ ! -x "$ORFSS3" ]; then
  echo "s3bench-court: SKIPPED (missing $ORFSS3)"
  exit 0
fi

WORK=$(mktemp -d)
# The measured payload (an append-only volume with EVERY PUT retained, plus
# manifested bodies) can run to hundreds of MiB; on hosts where Docker's data
# root is a small tmpfs, writing it under the container's overlay hits ENOSPC.
# S3WORK points at a disk-backed bind mount when the wrapper provides one.
if [ -n "${S3WORK:-}" ]; then
  WORK="$S3WORK/s3court"
  rm -rf "$WORK"
  mkdir -p "$WORK"
fi
ARMS="$WORK/arms"
mkdir -p "$ARMS" "$WORK/vol"

GW_PID=""
MINIO_PID=""
cleanup() {
  [ -n "$GW_PID" ] && kill "$GW_PID" 2>/dev/null || true
  [ -n "$MINIO_PID" ] && kill "$MINIO_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

json_escape() {
  sed -e 's/\x1b\[[0-9;]*[A-Za-z]//g' \
    | LC_ALL=C tr -c '[:print:]' ' ' \
    | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^ *//' -e 's/ *$//'
}

ARM_FAILS=0
: > "$WORK/failed-arms"

# --- start the OrFS-S3 gateway ----------------------------------------------

MANIFEST_FLAG="-manifest"
[ "$GW_MANIFEST" = off ] && MANIFEST_FLAG=""
# shellcheck disable=SC2086
"$ORFSS3" -init -volume "$WORK/vol" -addr "$GW_ADDR" \
  -access-key "$GW_ACCESS" -secret-key "$GW_SECRET" \
  -default-retention forever -group-commit $MANIFEST_FLAG \
  >"$WORK/gw.log" 2>&1 &
GW_PID=$!

# Bounded readiness: /metrics needs no auth by default, so a metrics scrape that
# succeeds is proof the listener is up.
gw_ready=0
i=0
while [ "$i" -lt 100 ]; do
  if "$S3LOAD" -endpoint "$GW_ADDR" -mode metrics -duration 1s >/dev/null 2>&1; then
    gw_ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.2
done
if [ "$gw_ready" != 1 ]; then
  echo "s3bench-court: FAIL: the OrFS-S3 gateway did not come up; it said:" >&2
  tail -c 1500 "$WORK/gw.log" >&2 || true
  exit 1
fi
echo "s3bench-court: OrFS-S3 gateway up at $GW_ADDR"

# run_arm <system> <endpoint> <access> <secret> <mode> <concurrency> <bytes> <label>
run_arm() {
  sys=$1; ep=$2; ak=$3; sk=$4; mode=$5; conc=$6; bytes=$7; label=$8
  out="$ARMS/$sys-$label.json"
  if "$S3LOAD" -endpoint "$ep" -access "$ak" -secret "$sk" -mode "$mode" \
      -concurrency "$conc" -object-bytes "$bytes" -duration "${DURATION}s" \
      -max-put-bytes "$((MAX_PUT_MIB * 1048576))" \
      -prefix "sbench-$sys-$label" \
      -seed "$SEED" -out "$out" >"$WORK/$sys-$label.log" 2>&1; then
    echo "s3bench-court: $sys $label ok"
    return 0
  fi
  echo "s3bench-court: WARN: arm $sys/$label FAILED; recorded as missing" >&2
  tail -3 "$WORK/$sys-$label.log" >&2 || true
  echo "$sys/$mode concurrency=$conc object_bytes=$bytes" >> "$WORK/failed-arms"
  rm -f "$out"
  ARM_FAILS=$((ARM_FAILS + 1))
  return 1
}

run_gateway() {
  for b in $OBJECT_BYTES; do
    for c in $CONCURRENCY; do
      run_arm "OrFS-S3" "$GW_ADDR" "$GW_ACCESS" "$GW_SECRET" put "$c" "$b" "put-c$c-b$b" || true
    done
  done
  for c in $CONCURRENCY; do
    run_arm "OrFS-S3" "$GW_ADDR" "$GW_ACCESS" "$GW_SECRET" get "$c" "$GET_BYTES" "get-c$c-b$GET_BYTES" || true
  done
  for c in $CONCURRENCY; do
    run_arm "OrFS-S3" "$GW_ADDR" "$GW_ACCESS" "$GW_SECRET" mixed "$c" "$GET_BYTES" "mixed-c$c-b$GET_BYTES" || true
  done
  run_arm "OrFS-S3" "$GW_ADDR" "$GW_ACCESS" "$GW_SECRET" worm 1 "$GET_BYTES" "worm" || true
  run_arm "OrFS-S3" "$GW_ADDR" "$GW_ACCESS" "$GW_SECRET" metrics 1 0 "metrics" || true
}

GW_FAILS_BEFORE=0
run_gateway
GW_FAILS=$ARM_FAILS

# --- MinIO, the real-world comparison ---------------------------------------
MINIO_STATUS="not_attempted"
MINIO_RAN=0
MINIO_FAILED=0
if [ "$MINIO_ARM" = 1 ] && [ -x "$MINIO" ]; then
  mkdir -p "$WORK/minio-data"
  "$MINIO" server "$WORK/minio-data" --address "$MINIO_ADDR" --quiet \
    >"$WORK/minio.log" 2>&1 &
  MINIO_PID=$!
  m_ready=0
  i=0
  while [ "$i" -lt 150 ]; do
    if "$S3LOAD" -endpoint "$MINIO_ADDR" -access "$MINIO_ACCESS" -secret "$MINIO_SECRET" \
        -mode put -concurrency 1 -object-bytes 4096 -duration 1s >/dev/null 2>&1; then
      m_ready=1
      break
    fi
    i=$((i + 1))
    sleep 0.2
  done
  if [ "$m_ready" = 1 ]; then
    MINIO_STATUS="mounted"
    echo "s3bench-court: MinIO up at $MINIO_ADDR"
    before=$ARM_FAILS
    for b in $OBJECT_BYTES; do
      for c in $CONCURRENCY; do
        run_arm "MinIO" "$MINIO_ADDR" "$MINIO_ACCESS" "$MINIO_SECRET" put "$c" "$b" "put-c$c-b$b" || true
      done
    done
    for c in $CONCURRENCY; do
      run_arm "MinIO" "$MINIO_ADDR" "$MINIO_ACCESS" "$MINIO_SECRET" get "$c" "$GET_BYTES" "get-c$c-b$GET_BYTES" || true
    done
    MINIO_FAILED=$((ARM_FAILS - before))
    MINIO_RAN=1
  else
    MINIO_STATUS="start_failed"
    echo "s3bench-court: NOTE: MinIO did not come up; the comparison is skipped" >&2
    tail -c 1000 "$WORK/minio.log" >&2 || true
  fi
  kill "$MINIO_PID" 2>/dev/null || true
  MINIO_PID=""
else
  MINIO_STATUS="unavailable"
fi

# --- assemble the receipt ---------------------------------------------------

NPROC=$(nproc 2>/dev/null || echo "?")
KERNEL=$(uname -sr | json_escape)
BACKING_FS=$(stat -f -c %T "$WORK" 2>/dev/null || echo "?")
GW_TAIL=$(tail -c 1200 "$WORK/gw.log" 2>/dev/null | json_escape || true)

FAILED_JSON=""
if [ -s "$WORK/failed-arms" ]; then
  FAILED_JSON=$(sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' "$WORK/failed-arms" | sed -e 's/^/  "/' -e 's/$/"/' | paste -sd, -)
fi

OUT="$S3BENCH_OUT"
HDR="$WORK/head.json"
cat > "$HDR" <<EOF
{
  "schema": "orfs.performance.s3bench.v1",
  "generated_at_unix": $(date +%s),
  "note": "One client (cmd/s3bench/s3load, its own SigV4 signer) drove the OrFS-S3 gateway and MinIO, sequentially, inside one resource-capped container on the same machine. This receipt is data, not a leaderboard: rows are keyed by {system, mode, concurrency, object_bytes} and are never ranked across incomparable coordinates.",
  "method": "A PUT is one single-shot S3 PUT of a seeded object; a GET is one S3 GET of a pre-PUT object; mixed alternates. Latencies are exact observed samples (per-worker slices merged and sorted). Bounded windows; the driver's own sample buffer is capped.",
  "comparability": {
    "same_machine": true,
    "container": {
      "caps": {"memory": "${CAP_MEM_LIMIT}", "memory_swap": "${CAP_MEMSWAP_LIMIT}", "cpus": "${CAP_CPUS}", "pids_limit": "${CAP_PIDS_LIMIT}"},
      "nproc": "${NPROC}",
      "kernel": "${KERNEL}",
      "backing_filesystem": "${BACKING_FS}"
    },
    "workload": {
      "duration_seconds": ${DURATION},
      "concurrency": [$(echo "$CONCURRENCY" | tr ' ' ',')],
      "put_object_bytes": [$(echo "$OBJECT_BYTES" | tr ' ' ',')],
      "get_object_bytes": ${GET_BYTES},
      "seed": ${SEED},
      "bounded": true
    },
    "segment_size_bytes": ${SEGMENT_SIZE},
    "systems": {
      "OrFS-S3": {
        "ran": true,
        "binary": "cmd/orfs-s3",
        "gateway": "in-process S3 over an append-only OrFS volume; no FUSE in the data path",
        "worm": "-default-retention forever; DELETE refused while locked",
        "manifested_reads": "${GW_MANIFEST} (-manifest)",
        "group_commit": "-group-commit (ORFS_GROUP_COMMIT=1)",
        "daemon_stderr": "${GW_TAIL}"
      },
      "MinIO": {
        "ran": $( [ "$MINIO_RAN" = 1 ] && echo true || echo false ),
        "status": "${MINIO_STATUS}",
        "availability_note": "${MINIO_NOTE}",
        "note": "client/server S3 server over its own local storage directory; the system most WORM is bought with today"
      }
    },
    "not_comparable": [
      "durability: MinIO is a client/server object store with its own on-disk format and fsync policy; the OrFS-S3 gateway appends to an OrFS authoritative log. The two do not offer the same durability contract and their rates are not a like-for-like durability ranking.",
      "data path: the gateway serves in-process from one local append-only volume; MinIO serves over its HTTP server from its own storage engine. Same client, same operations, different systems underneath.",
      "the gateway does NOT get batched group commit: internal/namespace holds its write mutex across every Publish, so concurrent PUTs never overlap and orfs_group_commit_batch_size stays 1. The gateway win measured here is the removed FUSE round trip, not a batched 233k-ops/s path."
    ],
    "not_ranked": true,
    "arms": {
      "OrFS-S3": {"failed": ${GW_FAILS}},
      "MinIO": {"ran": ${MINIO_RAN}, "failed": ${MINIO_FAILED}, "status": "${MINIO_STATUS}"},
      "failed": [${FAILED_JSON}]
    }
  },
  "arms": [
EOF

cp "$HDR" "$OUT"
first=1
for f in "$ARMS"/*.json; do
  [ -f "$f" ] || continue
  if [ "$first" = 1 ]; then first=0; else printf ',\n' >> "$OUT"; fi
  cat "$f" >> "$OUT"
done
printf '\n  ]\n}\n' >> "$OUT"

rows=$(grep -o '"op"' "$OUT" 2>/dev/null | wc -l)
if [ "$rows" -eq 0 ]; then
  echo "s3bench-court: FAIL: the receipt has no arms" >&2
  exit 1
fi
if [ "$GW_FAILS" -gt 0 ]; then
  echo "s3bench-court: FAIL: the OrFS-S3 gateway lost $GW_FAILS arm(s); the receipt is partial" >&2
  echo "s3bench-court: missing: $(tr '\n' ';' < "$WORK/failed-arms")" >&2
  exit 1
fi
if [ "$MINIO_RAN" = 1 ] && [ "$MINIO_FAILED" -gt 0 ]; then
  echo "s3bench-court: FAIL: MinIO lost $MINIO_FAILED arm(s); the receipt is partial" >&2
  exit 1
fi
echo "s3bench-court: PASS ($rows arm(s); MinIO status=$MINIO_STATUS)"
