#!/bin/sh
# The mounted POSIX court: run *inside* the container as root. Exercises the FUSE
# path end-to-end and asserts the projection boundary and recovery.
set -eu

ORFS=${1:-/tmp/orfs}
WORK=$(mktemp -d)
mkdir -p "$WORK/vol" "$WORK/mnt"

cleanup() { umount "$WORK/mnt" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT

"$ORFS" init "$WORK/vol" --segment-size 4096 >/dev/null
"$ORFS" mount "$WORK/vol" "$WORK/mnt" >/dev/null 2>&1 &
sleep 2

# The backing directory must not be reachable through the mount.
if [ -e "$WORK/mnt/or.log" ] || [ -e "$WORK/mnt/volume.meta" ]; then
  echo "FAIL: backing directory reachable through the mount" >&2
  exit 1
fi

printf 'hello, orfs\n' > "$WORK/mnt/hello.txt"
[ "$(cat "$WORK/mnt/hello.txt")" = "hello, orfs" ] || { echo "FAIL: read" >&2; exit 1; }

mkdir "$WORK/mnt/dir"
printf 'x\n' > "$WORK/mnt/dir/x"
[ "$(cat "$WORK/mnt/dir/x")" = "x" ] || { echo "FAIL: nested write" >&2; exit 1; }

ln -s hello.txt "$WORK/mnt/link"
[ "$(cat "$WORK/mnt/link")" = "hello, orfs" ] || { echo "FAIL: symlink" >&2; exit 1; }

mv "$WORK/mnt/hello.txt" "$WORK/mnt/renamed.txt"
ln "$WORK/mnt/renamed.txt" "$WORK/mnt/hard.txt"
[ "$(stat -c %h "$WORK/mnt/renamed.txt")" = "2" ] || { echo "FAIL: hard link" >&2; exit 1; }

# A directory may not be hard-linked.
mkdir "$WORK/mnt/d2"
if ln "$WORK/mnt/d2" "$WORK/mnt/d2link" 2>/dev/null; then
  echo "FAIL: directory hard-link should be EPERM" >&2
  exit 1
fi

umount "$WORK/mnt"
sleep 1

"$ORFS" verify --deep "$WORK/vol" | grep -q '\[ok\] replay' || { echo "FAIL: verify" >&2; exit 1; }

# Remount and read back byte-exact after replay.
"$ORFS" mount "$WORK/vol" "$WORK/mnt" >/dev/null 2>&1 &
sleep 2
[ "$(cat "$WORK/mnt/renamed.txt")" = "hello, orfs" ] || { echo "FAIL: recovery" >&2; exit 1; }
umount "$WORK/mnt"

echo "mount court: PASS"
