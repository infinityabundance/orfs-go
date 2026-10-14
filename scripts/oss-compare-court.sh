#!/bin/sh
# Open-source head-to-head court, step 1 — runs *inside* the court container as
# root with /dev/fuse + CAP_SYS_ADMIN.
#
# It drives the SAME shared, deterministic workload driver (cmd/orfscompare)
# against two MOUNTED OrFS implementations on the SAME machine, same container
# caps: (a) OrFS-GO (this repository's Go binary) and (b) OrFS-Rust (the Rust
# reference, `.cache/orfs-rs/orfs`). Both are mounted with equal durability (one
# fsync per appended record) and the same `--direct-io` setting.
#
# It composes evidence/oss-compare.json (schema orfs.oss-compare.v1): one row per
# {system, workload, writers, readers, block_size} plus a comparability block
# that names every way the two configurations differ. It never ranks rows: the
# receipt is data, not a leaderboard. If a system cannot be mounted the same way,
# it is run as best it allows and the exact difference is recorded — a documented
# asymmetry, never a fake parity.
#
# Env:
#   ORFS / ORFS_RS / ORFSCOMPARE / OSSSUMMARY   binary paths
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

mkdir -p "$EVID"
WORK=$(mktemp -d)
RUNS="$WORK/runs"
mkdir -p "$RUNS"

GO_VOL="$WORK/go-vol"; GO_MNT="$WORK/go-mnt"
RS_VOL="$WORK/rs-vol"; RS_MNT="$WORK/rs-mnt"
mkdir -p "$GO_MNT" "$RS_MNT"

GO_PID=""; RS_PID=""
GO_MOUNTED=0; RS_MOUNTED=0

cleanup() {
  if [ "$GO_MOUNTED" = 1 ]; then
    "$ORFS" unmount "$GO_MNT" >/dev/null 2>&1 || umount -l "$GO_MNT" 2>/dev/null || true
  fi
  if [ "$RS_MOUNTED" = 1 ]; then
    "$ORFS_RS" unmount "$RS_MNT" >/dev/null 2>&1 || umount -l "$RS_MNT" 2>/dev/null || true
  fi
  [ -n "$GO_PID" ] && kill "$GO_PID" 2>/dev/null || true
  [ -n "$RS_PID" ] && kill "$RS_PID" 2>/dev/null || true
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

prepare_on() { # $1 = mountpoint, $2 = label
  mnt=$1; label=$2
  echo "oss-compare-court: preparing workload files on $label"
  if ! "$ORFSCOMPARE" prepare -dir "$mnt" -writers "$PREP_WRITERS" -append-prefix append \
      -read-file data.bin -read-bytes "$((READ_MIB * 1024 * 1024))" -seed "$SEED" \
      >"$WORK/prepare-$label.json" 2>&1; then
    echo "oss-compare-court: FAIL: prepare failed on $label" >&2
    cat "$WORK/prepare-$label.json" >&2 || true
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
        echo "oss-compare-court: WARN: arm $sys/$wl writers=$w failed; skipping" >&2
        cat "$out.log" >&2 || true
        return 1
      fi
      ;;
    read)
      if ! "$ORFSCOMPARE" run -dir "$mnt" -system "$sys" -workload read \
          -readers "$r" -read-file data.bin -bs "$bs" \
          -seconds "$READ_SECONDS" -seed "$SEED" -out "$out" >"$out.log" 2>&1; then
        echo "oss-compare-court: WARN: arm $sys/$wl readers=$r bs=$bs failed; skipping" >&2
        cat "$out.log" >&2 || true
        return 1
      fi
      ;;
  esac
  rm -f "$out.log"
  return 0
}

run_system() { # $1 = system label, $2 = mountpoint
  sys=$1; mnt=$2
  for w in $WRITERS; do
    echo "oss-compare-court: $sys append writers=$w (${APPEND_SECONDS}s)"
    run_arm "$sys" "$mnt" append "$w" 0 0 || true
  done
  for bs in $BLOCK_SIZES; do
    for r in $READERS; do
      echo "oss-compare-court: $sys read bs=$bs readers=$r (${READ_SECONDS}s)"
      run_arm "$sys" "$mnt" read 0 "$r" "$bs" || true
    done
  done
}

run_system "OrFS-GO" "$GO_MNT"
if [ "$RS_MOUNTED" = 1 ]; then
  run_system "OrFS-Rust" "$RS_MNT"
fi

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
      "mounted": $( [ "$RS_MOUNTED" = 1 ] && echo true || echo false ),
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
    }
  },
  "differences": [
    "FUSE userspace library differs: go-fuse/v2 (Go) vs the fuser crate (Rust). Both speak classical /dev/fuse with one request per operation, but the daemons are independent implementations.",
    "Transport shape differs: Go serves its default f0 multi-reader loop; Rust serves its classical fuse transport. Same wire protocol, different daemon concurrency shape.",
    "Backing I/O: the Go daemon writes synchronously (pwrite/pread); Rust was pinned to --backing-io sync to match, because its default on this host is io_uring and the Go build has no io_uring backing path.",
    "Both are mounted with --direct-io (FOPEN_DIRECT_IO) and, for the append arm, one fsync per 4 KiB record: durability is equal even though the implementations differ.",
    "Both volumes use the same segment size and the same seeded 4 KiB record payload; the read file is the same size and the same deterministic content.",
    "Container caps are shared (the same container enforces them for both mounts), so the two systems cannot be given different resources.",
    "Host-level variance (page cache state, kernel scheduler, backing device write cache) is not controlled beyond the shared container: run-to-run variance is expected and unquantified here."
  ],
  "payload_size_bytes": ${RECORD_BYTES},
  "not_ranked": true,
  "note": "Two different implementations of OrFS, measured on the same machine and the same device with the same driver and equal durability. Differences above are the comparability caveats; no row is a ranking. A system that could not be run the same way is recorded as an asymmetry, not a parity claim."
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
if "$OSSSUMMARY" -comparability "$COMP" -o "$OUT" "$@"; then
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
if [ "$RS_MOUNTED" = 1 ]; then
  echo "oss-compare-court: PASS ($rows row(s); OrFS-GO and OrFS-Rust both mounted)"
else
  echo "oss-compare-court: PASS ($rows row(s); OrFS-Rust NOT mounted — see the comparability caveat)"
fi
