#!/bin/sh
# Cross-FS determinism court — runs *inside* a privileged court container as root.
#
# For each backing filesystem in {ext4, xfs, btrfs, tmpfs} this court creates a
# loop image, mkfs, mounts it at a work dir, initialises an OrFS volume on it with
# a FIXED volume UUID and a pinned clock (ORFS_CLOCK_EPOCH), replays one identical
# deterministic operation sequence against the mount, unmounts OrFS, and hashes
# every authoritative or.log/*.seg segment.
#
# The concatenated segment digest must be IDENTICAL across every backing that
# ran: the backing store must not change the bytes authority writes. If any
# backing that ran disagrees, the court FAILs.
#
# Honesty rules:
#   * tmpfs is always available; ext4/xfs/btrfs run only when their mkfs exists
#     and the loop+mount succeeds.
#   * A backing that cannot run is named and is NOT counted as a pass.
#   * The court FAILs if fewer than two backings could be compared, because a
#     one-backed "agreement" proves nothing.
#
# Env:
#   ORFS                orfs binary        (default /out/orfs)
#   ORFS_EVIDENCE_DIR   receipt root        (default /evidence, the repo's evidence/)
#   SEGMENT_SIZE        OrFS segment bytes  (default 8388608)
#   UUID_HEX            fixed volume uuid   (default 00112233445566778899aabbccddeeff)
#   CLOCK_EPOCH         pinned clock secs   (default 1700000000)
#   IMG_MIB             loop image size     (default 512)
#   TMPFS_SIZE          tmpfs size          (default 512m)
#   BACKINGS            space-separated set (default "ext4 xfs btrfs tmpfs")
set -eu

# A fixed collation makes glob order and test paths deterministic.
LC_ALL=C
export LC_ALL

ORFS=${ORFS:-/out/orfs}
EVID=${ORFS_EVIDENCE_DIR:-/evidence}
SEGMENT_SIZE=${SEGMENT_SIZE:-8388608}
UUID_HEX=${UUID_HEX:-00112233445566778899aabbccddeeff}
CLOCK_EPOCH=${CLOCK_EPOCH:-1700000000}
IMG_MIB=${IMG_MIB:-512}
TMPFS_SIZE=${TMPFS_SIZE:-512m}
BACKINGS=${BACKINGS:-ext4 xfs btrfs tmpfs}

if [ ! -x "$ORFS" ]; then
  echo "crossfs-court: SKIPPED (missing $ORFS)"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  echo "crossfs-court: SKIPPED (go is required for the xattr probe)"
  exit 0
fi

mkdir -p "$EVID/correctness"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- the shared, identical operation sequence ------------------------------

# xattrprobe is a syscall-level setxattr/getxattr round trip, used because the
# court image ships no attr(1) tools. It is applied *inside* the OrFS mount, so
# the backing filesystem never sees it; the sequence is therefore identical on
# every backing.
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

xattr_probe() { # $1 = file
  # Run from $WORK (outside the /src module) so the probe is a plain single-file
  # program with stdlib-only imports.
  ( cd "$WORK" && go run xattrprobe.go "$1" )
}

# fs_ops is the deterministic sequence. It must not depend on the backing store
# or on wall-clock time (timestamps are pinned by ORFS_CLOCK_EPOCH). The
# parameter is deliberately named mp: POSIX shell functions have no local scope,
# so a variable here would clobber the caller's backing-mount variable.
fs_ops() { # $1 = OrFS mountpoint
  mp=$1
  mkdir "$mp/dir"              || return 1
  mkdir "$mp/dir/sub"          || return 1
  printf 'alpha\n' > "$mp/a.txt"        || return 1
  printf 'beta\n'  > "$mp/dir/b.txt"    || return 1
  printf 'gamma\n' > "$mp/dir/sub/c.txt" || return 1
  ln -s a.txt "$mp/sym"        || return 1
  ln "$mp/a.txt" "$mp/hard"   || return 1
  mv "$mp/dir/b.txt" "$mp/dir/b2.txt" || return 1
  rm "$mp/hard"                || return 1
  xattr_probe "$mp/a.txt"      || return 1
  return 0
}

# segments_digest LOGDIR — sha256 of the concatenation of every *.seg file in
# ascending (zero-padded) name order. A segment's name is its logical offset, so
# lexicographic order is canonical order.
segments_digest() {
  logdir=$1
  set -- "$logdir"/*.seg
  [ -e "$1" ] || return 1
  cat "$@" | sha256sum | awk '{print $1}'
}

# --- one backing ------------------------------------------------------------

# run_backing FS — carries out the full sequence on one backing store and leaves
# its outcome in $WORK/FS/{status,digest,note}. It runs in a subshell so its
# teardown trap fires on every exit path (success, skip, or error).
run_backing() {
  fs=$1
  d="$WORK/$fs"
  mkdir -p "$d"
  printf 'skip\n' > "$d/status"
  : > "$d/digest"
  : > "$d/note"

  (
    set +e
    mnt="$d/mntfs"; omnt="$d/orfs"; vol="$mnt/vol"; img="$d/img"
    mkdir -p "$mnt" "$omnt"
    loop=""; mounted=0; omounted=0; mount_pid=""

    teardown() {
      if [ "$omounted" = 1 ]; then
        "$ORFS" unmount "$omnt" >/dev/null 2>&1 || umount "$omnt" 2>/dev/null || true
      fi
      if [ -n "$mount_pid" ]; then kill "$mount_pid" 2>/dev/null || true; fi
      if [ "$mounted" = 1 ]; then umount "$mnt" 2>/dev/null || true; fi
      if [ -n "$loop" ]; then losetup -d "$loop" 2>/dev/null || true; fi
    }
    trap teardown EXIT INT TERM

    record() { # status digest note
      printf '%s\n' "$1" > "$d/status"
      printf '%s\n' "$2" > "$d/digest"
      printf '%s'   "$3" > "$d/note"
    }
    skip_out() { record skip "" "$1"; exit 0; }

    # --- backing store ------------------------------------------------------
    if [ "$fs" = tmpfs ]; then
      mount -t tmpfs -o "size=$TMPFS_SIZE" tmpfs "$mnt" >/dev/null 2>&1 \
        || skip_out "skipped: tmpfs mount failed"
      mounted=1
    else
      command -v "mkfs.$fs" >/dev/null 2>&1 \
        || skip_out "skipped: mkfs.$fs is not installed in this image"
      dd if=/dev/zero of="$img" bs=1048576 count="$IMG_MIB" status=none >/dev/null 2>&1 \
        || skip_out "skipped: could not create the backing image"
      loop=$(losetup -fP --show "$img" 2>/dev/null) || skip_out "skipped: losetup failed"
      [ -n "$loop" ] || skip_out "skipped: no free loop device"
      case "$fs" in
        ext4)  mkfs.ext4  -q    "$loop" >/dev/null 2>&1 ;;
        xfs)   mkfs.xfs   -q -f "$loop" >/dev/null 2>&1 ;;
        btrfs) mkfs.btrfs -q -f "$loop" >/dev/null 2>&1 ;;
      esac || skip_out "skipped: mkfs.$fs failed"
      mount "$loop" "$mnt" >/dev/null 2>&1 || skip_out "skipped: mount of $loop failed"
      mounted=1
    fi

    # --- OrFS volume with pinned identity and clock -------------------------
    ORFS_CLOCK_EPOCH=$CLOCK_EPOCH "$ORFS" init "$vol" \
      --segment-size "$SEGMENT_SIZE" --uuid "$UUID_HEX" >/dev/null 2>&1 \
      || skip_out "skipped: orfs init failed on $fs"

    ORFS_CLOCK_EPOCH=$CLOCK_EPOCH "$ORFS" mount "$vol" "$omnt" >/dev/null 2>&1 &
    mount_pid=$!
    sleep 2
    if ! mountpoint -q "$omnt"; then
      kill "$mount_pid" 2>/dev/null || true; mount_pid=""
      skip_out "skipped: OrFS mount did not come up on $fs"
    fi
    omounted=1

    # --- one identical deterministic sequence -------------------------------
    fs_ops "$omnt" || skip_out "skipped: the operation sequence failed on $fs"

    # --- clean unmount, then hash authority ---------------------------------
    ORFS_CLOCK_EPOCH=$CLOCK_EPOCH "$ORFS" unmount "$omnt" >/dev/null 2>&1 \
      || umount "$omnt" 2>/dev/null || true
    omounted=0
    kill "$mount_pid" 2>/dev/null || true; mount_pid=""
    sleep 1

    dg=$(segments_digest "$vol/or.log") || skip_out "skipped: no segment files were written on $fs"
    [ -n "$dg" ] || skip_out "skipped: empty segment digest on $fs"
    record ran "$dg" ""
    exit 0
  )
  return 0
}

# --- drive every backing and collect ----------------------------------------

write_xattr_probe
for fs in $BACKINGS; do
  echo "crossfs-court: $fs ..."
  run_backing "$fs"
done

REF=""
RAN=0
for fs in $BACKINGS; do
  st=$(cat "$WORK/$fs/status" 2>/dev/null || echo skip)
  dg=$(cat "$WORK/$fs/digest" 2>/dev/null || echo "")
  if [ "$st" = ran ]; then
    RAN=$((RAN + 1))
    [ -n "$REF" ] || REF="$dg"
  fi
done

AGREED=true
[ "$RAN" -ge 2 ] || AGREED=false
for fs in $BACKINGS; do
  st=$(cat "$WORK/$fs/status" 2>/dev/null || echo skip)
  dg=$(cat "$WORK/$fs/digest" 2>/dev/null || echo "")
  [ "$st" = ran ] || continue
  [ "$dg" = "$REF" ] || AGREED=false
done

# --- receipts ---------------------------------------------------------------

write_receipt() {
  out=$1
  {
    printf '{\n'
    printf '  "schema": "orfs.correctness.crossfs.v1",\n'
    printf '  "uuid": "%s",\n' "$UUID_HEX"
    printf '  "clock_epoch": %s,\n' "$CLOCK_EPOCH"
    printf '  "segment_size": %s,\n' "$SEGMENT_SIZE"
    printf '  "reference_sha256": "%s",\n' "$REF"
    printf '  "backings_ran": %s,\n' "$RAN"
    printf '  "agreed": %s,\n' "$AGREED"
    printf '  "backings": [\n'
    first=1
    for fs in $BACKINGS; do
      st=$(cat "$WORK/$fs/status" 2>/dev/null || echo skip)
      dg=$(cat "$WORK/$fs/digest" 2>/dev/null || echo "")
      nt=$(tr -d '"\\' < "$WORK/$fs/note" 2>/dev/null | tr '\n' ' ')
      ag=false
      if [ "$st" = ran ] && [ "$dg" = "$REF" ]; then ag=true; fi
      if [ "$first" = 1 ]; then first=0; else printf ',\n'; fi
      printf '    {"fs": "%s", "segment_sha256": "%s", "agreed": %s, "note": "%s"}' \
        "$fs" "$dg" "$ag" "$nt"
    done
    printf '\n  ]\n}\n'
  } > "$out"
}

write_log() {
  out=$1
  {
    echo "cross-fs determinism court"
    echo "uuid=$UUID_HEX clock_epoch=$CLOCK_EPOCH segment_size=$SEGMENT_SIZE"
    echo "reference_sha256=$REF backings_ran=$RAN agreed=$AGREED"
    for fs in $BACKINGS; do
      st=$(cat "$WORK/$fs/status" 2>/dev/null || echo skip)
      dg=$(cat "$WORK/$fs/digest" 2>/dev/null || echo "")
      nt=$(cat "$WORK/$fs/note" 2>/dev/null || echo "")
      printf '%-6s %-4s %s %s\n' "$fs" "$st" "$dg" "$nt"
    done
  } > "$out"
}

write_receipt "$EVID/correctness/cross-fs.json"
write_log "$EVID/correctness/cross-fs.log"

echo "crossfs-court: --- segment digests ---"
for fs in $BACKINGS; do
  st=$(cat "$WORK/$fs/status" 2>/dev/null || echo skip)
  dg=$(cat "$WORK/$fs/digest" 2>/dev/null || echo "")
  nt=$(cat "$WORK/$fs/note" 2>/dev/null || echo "")
  printf 'crossfs-court:   %-6s %-4s %s %s\n' "$fs" "$st" "$dg" "$nt"
done
echo "crossfs-court: reference=$REF backings_ran=$RAN"

if [ "$AGREED" = true ]; then
  echo "crossfs court: PASS ($RAN backing(s) produced an identical segment digest)"
  exit 0
fi

if [ "$RAN" -lt 2 ]; then
  echo "crossfs court: FAIL (only $RAN backing(s) ran; cross-fs agreement cannot be established)" >&2
else
  echo "crossfs court: FAIL (backings that ran did not agree)" >&2
fi
exit 1
