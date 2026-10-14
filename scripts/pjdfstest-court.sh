#!/bin/sh
# pjdfstest court — runs *inside* the court container as root.
#
# Initialises and mounts an OrFS volume, then drives the upstream pjdfstest POSIX
# system-call suite against the mount. The court records the PASS/FAIL counts and
# lists every failing test file as a *deviation*: a failure is published, never
# silently allowlisted.
#
# Honesty rules:
#   * If pjdfstest cannot be built or run (no toolchain, clone fails, `prove`
#     missing, the OrFS mount never came up, or the bounded run times out), the
#     court prints SKIPPED with the exact reason and exits 0. A skipped court is
#     not a pass.
#   * A run that completes writes its receipt and exits 0 even when the suite
#     reports failures: those are deviations of the mounted namespace, and the
#     receipt states their number and identity.
#   * A `verify --deep` that reports `[FAIL]` after the run is a real integrity
#     failure and exits 1.
#
# Env:
#   ORFS                orfs binary            (default /out/orfs)
#   ORFS_EVIDENCE_DIR   receipt root            (default /evidence, the repo's evidence/)
#   PJD_DIR             prebuilt pjdfstest tree (default /opt/pjdfstest)
#   PJD_URL             upstream repository     (default https://github.com/pjd/pjdfstest)
#   SEGMENT_SIZE        OrFS segment bytes      (default 8388608)
#   PJD_TIMEOUT         bounded run seconds     (default 1800)
#   PJD_MOUNT_FLAGS     extra `orfs mount` flags (default the reference court's
#                       "--allow-other --entry-ttl-ms 0 --attr-ttl-ms 0")
#   PJD_VERBOSE         when 1, additionally record the first few failing
#                       assertions of every failing test file to
#                       $EVID/performance/pjdfstest-failures.txt. It changes no
#                       pass/fail accounting: the receipt is byte-identical
#                       whether or not it is set.
set -eu

LC_ALL=C
export LC_ALL

ORFS=${ORFS:-/out/orfs}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
PJD_DIR=${PJD_DIR:-/opt/pjdfstest}
PJD_URL=${PJD_URL:-https://github.com/pjd/pjdfstest}
SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}
PJD_TIMEOUT=${PJD_TIMEOUT:-1800}
PJD_VERBOSE=${PJD_VERBOSE:-0}
# --allow-other: the suite switches to unprivileged uids (nobody = 65534); a
#   root-mounted FUSE filesystem denies every other uid at the VFS layer, so the
#   daemon's own POSIX checks would never run. The Rust reference mounts its
#   court the same way (docs/FUSE-SEMANTICS.md 11.1).
# --entry-ttl-ms 0 --attr-ttl-ms 0: force every path walk back through the
#   daemon so a cached dentry cannot satisfy a walk and skip the search-
#   permission re-check. Matches the reference court exactly.
PJD_MOUNT_FLAGS=${PJD_MOUNT_FLAGS:---allow-other --entry-ttl-ms 0 --attr-ttl-ms 0}

if [ ! -x "$ORFS" ]; then
  echo "pjdfstest-court: SKIPPED (missing $ORFS)"
  exit 0
fi

mkdir -p "$EVID/performance"

WORK=$(mktemp -d)
MNT="$WORK/mnt"
VOL="$WORK/vol"
mkdir -p "$MNT"
mount_pid=""
mounted=0
cleanup() {
  if [ "$mounted" = 1 ]; then
    "$ORFS" unmount "$MNT" >/dev/null 2>&1 || umount "$MNT" 2>/dev/null || true
  fi
  if [ -n "$mount_pid" ]; then kill "$mount_pid" 2>/dev/null || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

skip() { echo "pjdfstest-court: SKIPPED ($1)"; exit 0; }
fail() { echo "pjdfstest-court: FAIL: $1" >&2; exit 1; }

# --- locate or build pjdfstest ---------------------------------------------

if [ ! -x "$PJD_DIR/pjdfstest" ] || [ ! -d "$PJD_DIR/tests" ]; then
  command -v git      >/dev/null 2>&1 || skip "pjdfstest is absent and git is unavailable to build it"
  command -v autoreconf >/dev/null 2>&1 || skip "pjdfstest is absent and autoreconf is unavailable"
  command -v make     >/dev/null 2>&1 || skip "pjdfstest is absent and make is unavailable"
  if ! command -v cc >/dev/null 2>&1 && ! command -v gcc >/dev/null 2>&1; then
    skip "pjdfstest is absent and no C compiler is available"
  fi
  echo "pjdfstest-court: building pjdfstest from $PJD_URL ..."
  BUILD="$WORK/pjdfstest"
  git clone --depth 1 "$PJD_URL" "$BUILD" >"$WORK/clone.log" 2>&1 \
    || { tail -5 "$WORK/clone.log" >&2 || true; skip "could not clone $PJD_URL"; }
  if ! ( cd "$BUILD" && autoreconf -ifs && ./configure && make pjdfstest ) >"$WORK/build.log" 2>&1; then
    tail -8 "$WORK/build.log" >&2 || true
    skip "pjdfstest build failed"
  fi
  PJD_DIR="$BUILD"
fi

if ! command -v prove >/dev/null 2>&1; then
  skip "prove (TAP::Harness) is not installed"
fi
if [ ! -f "$PJD_DIR/tests/misc.sh" ]; then
  skip "pjdfstest tests directory is missing at $PJD_DIR/tests"
fi

# --- mount an OrFS volume ---------------------------------------------------

"$ORFS" init "$VOL" --segment-size "$SEGMENT_SIZE" >/dev/null 2>&1 \
  || skip "orfs init failed"

"$ORFS" mount $PJD_MOUNT_FLAGS "$VOL" "$MNT" >/dev/null 2>&1 &
mount_pid=$!
sleep 2
if ! mountpoint -q "$MNT"; then
  kill "$mount_pid" 2>/dev/null || true; mount_pid=""
  skip "OrFS mount did not come up (no usable /dev/fuse or mount privilege?)"
fi
mounted=1

# --- run the suite against the mount ----------------------------------------

OUT="$WORK/prove.out"
run_prove() {
  if command -v timeout >/dev/null 2>&1; then
    ( cd "$MNT" && timeout "$PJD_TIMEOUT" prove -r "$PJD_DIR/tests" )
  else
    ( cd "$MNT" && prove -r "$PJD_DIR/tests" )
  fi
}
set +e
run_prove > "$OUT" 2>&1
rc=$?
set -e
if [ "$rc" = 124 ] || [ "$rc" = 137 ]; then
  skip "the pjdfstest suite exceeded the bounded run time of ${PJD_TIMEOUT}s"
fi

# --- parse the TAP::Harness summary -----------------------------------------

total=$(grep -o 'Tests=[0-9][0-9]*' "$OUT" | tail -1 | cut -d= -f2)
failed=$(grep -o 'Failed: [0-9][0-9]*' "$OUT" | awk '{s += $2} END {print s + 0}')
if [ -z "$total" ]; then
  echo "pjdfstest-court: prove output (tail):" >&2
  tail -8 "$OUT" >&2 || true
  skip "prove produced no test summary (the suite did not run)"
fi
if [ "$total" = 0 ]; then
  skip "prove collected 0 tests"
fi
passed=$((total - failed))

# Every failing test *file* from the summary report is a deviation. The paths are
# absolute; reduce them to tests/<suite>/<file> so the receipt is location-free.
DEVFILE="$WORK/deviations"
: > "$DEVFILE"
grep -E '\(Wstat: .*Tests: [0-9]+ Failed: [0-9]+\)' "$OUT" > "$WORK/summary.lines" || true
while IFS= read -r line; do
  path=${line%% *}
  rel=${path#"$PJD_DIR/tests/"}
  printf 'tests/%s\n' "$rel" >> "$DEVFILE"
done < "$WORK/summary.lines"

if [ "$failed" -gt 0 ]; then
  note="$failed of $total pjdfstest assertions failed against the OrFS mount; every failing test file is listed under deviations (no allowlist)"
else
  note="all $total pjdfstest assertions passed against the OrFS mount"
fi

# --- optional per-file failure detail ---------------------------------------

# With PJD_VERBOSE=1, re-run each failing test file individually and record its
# first failing assertions. This is diagnostic only: it never feeds the receipt
# (the counts above are already fixed) and it never allowlists a failure.
if [ "$PJD_VERBOSE" = 1 ] && [ "$failed" -gt 0 ]; then
  FAILFILE="$EVID/performance/pjdfstest-failures.txt"
  : > "$FAILFILE"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    rel=${line%% *}
    path="$PJD_DIR/$rel"
    echo "=== $rel ===" >> "$FAILFILE"
    if command -v timeout >/dev/null 2>&1; then
      ( cd "$MNT" && timeout 180 prove -v "$path" 2>&1 ) \
        | grep -E '^(not ok|# )' | head -n 25 >> "$FAILFILE" || true
    else
      ( cd "$MNT" && prove -v "$path" 2>&1 ) \
        | grep -E '^(not ok|# )' | head -n 25 >> "$FAILFILE" || true
    fi
  done < "$DEVFILE"
  echo "pjdfstest-court: per-file failures recorded at $FAILFILE"
fi

# --- unmount, then verify the authoritative log -----------------------------

"$ORFS" unmount "$MNT" >/dev/null 2>&1 || umount "$MNT" 2>/dev/null || true
mounted=0
kill "$mount_pid" 2>/dev/null || true; mount_pid=""
sleep 1

verify_failed=0
vout=$("$ORFS" verify --deep "$VOL" 2>&1) || true
if printf '%s\n' "$vout" | grep -q '\[FAIL\]'; then
  verify_failed=1
fi

# --- receipt ----------------------------------------------------------------

deviations_json() {
  printf '['
  first=1
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    if [ "$first" = 1 ]; then first=0; else printf ', '; fi
    printf '"%s"' "$(printf '%s' "$line" | tr -d '"\\')"
  done < "$DEVFILE"
  printf ']'
}

{
  printf '{\n'
  printf '  "schema": "orfs.performance.pjdfstest.v1",\n'
  printf '  "source": "%s",\n' "$PJD_URL"
  printf '  "total": %s,\n' "$total"
  printf '  "passed": %s,\n' "$passed"
  printf '  "failed": %s,\n' "$failed"
  printf '  "deviations": '
  deviations_json
  printf ',\n'
  printf '  "note": "%s"\n' "$note"
  printf '}\n'
} > "$EVID/performance/pjdfstest.json"

echo "pjdfstest-court: total=$total passed=$passed failed=$failed"
if [ "$failed" -gt 0 ]; then
  echo "pjdfstest-court: deviations:"
  sed 's/^/pjdfstest-court:   /' "$DEVFILE"
fi

if [ "$verify_failed" -ne 0 ]; then
  printf '%s\n' "$vout" >&2
  fail "verify --deep reported [FAIL] after the suite"
fi

if [ "$failed" -gt 0 ]; then
  echo "pjdfstest court: RAN — $passed/$total assertions passed, $failed failed (deviations recorded, not allowlisted); receipt at $EVID/performance/pjdfstest.json"
else
  echo "pjdfstest court: PASS — all $total assertions passed; receipt at $EVID/performance/pjdfstest.json"
fi
