#!/bin/sh
# fsstress court — runs *inside* the court container as root.
#
# Proves the mounted namespace survives an adversarial, bounded operation storm
# (a native fsstress-equivalent), then that the authoritative log still verifies
# --deep. It also checks the POSIX extras the namespace claims: symlink/readlink,
# hard-link nlink, user xattrs, and EPERM on a directory hard-link. A wrong
# answer fails the court loudly; an unavailable interface is named, not hidden.
#
# Env:
#   ORFS / ORFSSTRESS   binary paths        (default /out/orfs, /out/orfsstress)
#   ORFS_EVIDENCE_DIR   receipt root        (default /evidence, the repo's evidence/)
#   WORKERS             stress workers      (default 8)
#   STRESS_SECONDS      stress duration     (default 15)
#   SEED                stress seed         (default 1)
#   SEGMENT_SIZE        OrFS segment bytes  (default 8388608)
set -eu

ORFS=${ORFS:-/out/orfs}
ORFSSTRESS=${ORFSSTRESS:-/out/orfsstress}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
WORKERS=${WORKERS:-8}
STRESS_SECONDS=${STRESS_SECONDS:-15}
SEED=${SEED:-1}
SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}

if [ ! -x "$ORFS" ]; then
  echo "fsstress-court: SKIPPED (missing $ORFS)"
  exit 0
fi
if [ ! -x "$ORFSSTRESS" ]; then
  echo "fsstress-court: SKIPPED (missing $ORFSSTRESS)"
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
    mounted=0
  fi
  if [ -n "$mount_pid" ]; then
    kill "$mount_pid" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

fail() { echo "fsstress-court: FAIL: $1" >&2; exit 1; }

"$ORFS" init "$VOL" --segment-size "$SEGMENT_SIZE" >/dev/null
"$ORFS" mount "$VOL" "$MNT" >/dev/null 2>&1 &
mount_pid=$!
sleep 2
mountpoint -q "$MNT" || fail "OrFS mount did not come up"
mounted=1

# --- POSIX extras ---------------------------------------------------------

# xattrprobe is a syscall-level setxattr/getxattr round trip, used because the
# court image ships no attr(1) tools. Written once, on demand.
write_xattr_probe() {
  if [ -f "$WORK/xattrprobe.go" ]; then
    return 0
  fi
  cat > "$WORK/xattrprobe.go" <<'EOF'
// Command xattrprobe exercises setxattr/getxattr on one path.
package main

import (
	"fmt"
	"os"
	"syscall"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: xattrprobe <path>")
		os.Exit(2)
	}
	path, name, want := os.Args[1], "user.orfs", "hello"
	if err := syscall.Setxattr(path, name, []byte(want), 0); err != nil {
		fmt.Fprintf(os.Stderr, "setxattr: %v\n", err)
		os.Exit(1)
	}
	buf := make([]byte, 128)
	n, err := syscall.Getxattr(path, name, buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "getxattr: %v\n", err)
		os.Exit(1)
	}
	if got := string(buf[:n]); got != want {
		fmt.Fprintf(os.Stderr, "xattr mismatch: got %q want %q\n", got, want)
		os.Exit(1)
	}
	_ = syscall.Removexattr(path, name)
}
EOF
}

xattr_check() { # $1 = file
  f=$1
  if command -v setfattr >/dev/null 2>&1 && command -v getfattr >/dev/null 2>&1; then
    setfattr -n user.orfs -v hello "$f" 2>/dev/null || return 1
    got=$(getfattr --only-values -n user.orfs "$f" 2>/dev/null) || return 1
    [ "$got" = "hello" ] || return 1
    setfattr -x user.orfs "$f" 2>/dev/null || true
    return 0
  fi
  if ! command -v go >/dev/null 2>&1; then
    echo "fsstress-court: WARN: no setfattr and no go; xattr check unavailable" >&2
    return 1
  fi
  write_xattr_probe
  # Run from $WORK (outside the /src module) so the probe is a plain single-file
  # program with stdlib-only imports.
  ( cd "$WORK" && go run xattrprobe.go "$f" )
}

printf 'orfs\n' > "$MNT/base" || fail "create base file"

# symlink + readlink
ln -s base "$MNT/sym" || fail "symlink create"
target=$(readlink "$MNT/sym")
[ "$target" = "base" ] || fail "readlink returned '$target', want 'base'"
[ "$(cat "$MNT/sym")" = "orfs" ] || fail "symlink does not resolve to its target"

# hard link: nlink must reach 2
ln "$MNT/base" "$MNT/hard" || fail "hard link create"
nlink=$(stat -c %h "$MNT/base")
[ "$nlink" = "2" ] || fail "hard link nlink is $nlink, want 2"

# xattr set/get
xattr_check "$MNT/base" || fail "xattr set/get"

# a directory hard-link must be refused with EPERM. coreutils `ln` refuses to
# even attempt it, so call link(2) directly via a stdlib-only probe.
cat > "$WORK/linkprobe.go" <<'EOF'
// Command linkprobe asserts that link(2) on a directory returns EPERM.
package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: linkprobe <dir> <link>")
		os.Exit(2)
	}
	err := os.Link(os.Args[1], os.Args[2])
	if err == nil {
		fmt.Fprintln(os.Stderr, "directory hard-link succeeded; want EPERM")
		os.Exit(1)
	}
	if !errors.Is(err, syscall.EPERM) {
		fmt.Fprintf(os.Stderr, "directory hard-link failed with %v; want EPERM\n", err)
		os.Exit(1)
	}
}
EOF
mkdir "$MNT/dir" || fail "mkdir for dir hard-link"
( cd "$WORK" && go run linkprobe.go "$MNT/dir" "$MNT/dirlink" ) || fail "directory hard-link did not return EPERM"

# --- bounded operation storm ---------------------------------------------

stress_rc=0
if "$ORFSSTRESS" -dir "$MNT" -workers "$WORKERS" -seconds "$STRESS_SECONDS" -seed "$SEED" \
     > "$EVID/performance/fsstress.json"; then
  echo "fsstress-court: orfsstress ok ($WORKERS workers, ${STRESS_SECONDS}s)"
else
  stress_rc=$?
  echo "fsstress-court: FAIL: orfsstress exited $stress_rc (unexpected errno); receipt kept at $EVID/performance/fsstress.json" >&2
fi

# --- clean unmount, then verify the authoritative log ---------------------

"$ORFS" unmount "$MNT" >/dev/null 2>&1 || umount "$MNT" 2>/dev/null || true
mounted=0
if [ -n "$mount_pid" ]; then
  kill "$mount_pid" 2>/dev/null || true
  mount_pid=""
fi
sleep 1

verify_rc=0
if vout=$("$ORFS" verify --deep "$VOL" 2>&1); then
  printf '%s\n' "$vout"
else
  verify_rc=$?
  printf '%s\n' "$vout"
fi
if printf '%s\n' "$vout" | grep -q '\[FAIL\]'; then
  verify_rc=1
fi

if [ "$stress_rc" -ne 0 ] || [ "$verify_rc" -ne 0 ]; then
  echo "fsstress-court: FAIL (stress=$stress_rc verify=$verify_rc)" >&2
  exit 1
fi
echo "fsstress court: PASS"
