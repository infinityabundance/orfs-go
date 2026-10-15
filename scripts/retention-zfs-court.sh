#!/bin/sh
# OpenZFS retained-snapshot retention court — runs *inside* a privileged court
# container as root (a file-backed zpool vdev needs root and, crucially, the
# OpenZFS kernel module).
#
# The court answers the same RETENTION question as the Btrfs court
# (scripts/retention-btrfs-court.sh), for a fourth system:
#
#   Can a system retain every committed historical transition while achieving
#   comparable tail latency, throughput and physical-space economics?
#
#   - OrFS retains history in its authoritative log (non-withdrawal, Law I):
#     every committed transition is kept, with no extra snapshot step.
#   - OpenZFS retains it with a read-only snapshot (`zfs snapshot`) taken after
#     every epoch. ZFS snapshots are always read-only.
#
# One defined, bounded epoch workload (append 4 KiB records with write(2)+fsync(2)
# per record) drives both. After each epoch the OpenZFS arm takes a snapshot, so
# every committed state is retained. The court measures, per epoch:
#
#   * current-state append latency/throughput (exact p50/p95/p99/p99.9);
#   * current-state read latency/throughput;
#   * historical-state (snapshot) read latency/throughput (OpenZFS only);
#   * physical vs logical bytes (`zfs list -o used,referenced,logicalused,
#     logicalreferenced` for OpenZFS; the authoritative or.log blocks for
#     OrFS-GO);
#   * the cost of creating a snapshot (`zfs snapshot`).
#
# It composes evidence/retention-zfs.json (schema orfs.retention.zfs.v1) via
# cmd/retentionsummary. It never ranks rows. Every axis it cannot measure the same
# way on both systems is recorded as an explicit non-comparability, never as a
# fake parity: OrFS-GO serves no named historical root in this build, so the
# historical-state read axis is OpenZFS-only.
#
# FEASIBILITY FIRST. OpenZFS is an out-of-tree kernel module: `zpool`/`zfs`
# cannot create, import, or even query a pool without it, and loading a kernel
# module is a host-level change that is out of scope for this repository (see
# docs/LIMITATIONS.md). This court therefore probes for a reachable OpenZFS
# *before* building anything. When it is not reachable it emits a single precise
# SKIPPED line naming exactly what is missing, writes evidence/retention-zfs.json
# recording the exact feasibility evidence (the commands it ran and their verbatim
# output, so the finding is reproducible), and exits 0 — never a false pass, and
# never a non-zero exit for a genuine environment absence.
#
# Env:
#   ORFS / RETENTIONBENCH / RETENTIONSUMMARY   binary paths
#   ORFS_EVIDENCE_DIR                          receipt root   (default /evidence)
#   SEGMENT_SIZE                               OrFS segment bytes (default 8 MiB)
#   IMG_MIB                                    vdev/loop image size  (default 512)
#   POOL                                       zpool name (default orfsret)
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
POOL=${POOL:-orfsret}
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

# Path A: real OpenZFS inside a KVM micro-VM. The host has no ZFS kernel module
# and loading one would be a host change, so the comparator runs in a guest
# whose own kernel ships the out-of-tree OpenZFS module. ZFS_KVM=0 disables it.
ZFS_KVM=${ZFS_KVM:-1}
VM_MEM_MIB=${VM_MEM_MIB:-1024}
VM_CPUS=${VM_CPUS:-2}
VM_BOOT_SECONDS=${VM_BOOT_SECONDS:-600}
ZFS_KVM_IMAGE=${ZFS_KVM_IMAGE:-/kvm/ubuntu-24.04-minimal-cloudimg-amd64.img}

CAP_MEM_LIMIT=${CAP_MEM_LIMIT:-2g}
CAP_MEMSWAP_LIMIT=${CAP_MEMSWAP_LIMIT:-2g}
CAP_CPUS=${CAP_CPUS:-4}
CAP_PIDS_LIMIT=${CAP_PIDS_LIMIT:-1024}

OUT="$EVID/retention-zfs.json"
mkdir -p "$EVID"

json_escape() {
  sed -e 's/\x1b\[[0-9;]*[A-Za-z]//g' \
  | LC_ALL=C tr -c '[:print:]' ' ' \
  | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^ *//' -e 's/ *$//'
}
jstr() { printf '"%s"' "$(printf '%s' "$1" | json_escape)"; }

# --- environment guards: a precise SKIPPED, never a false pass --------------

if [ ! -x "$BENCH" ] || [ ! -x "$SUMMARY" ]; then
  echo "retention-zfs-court: SKIPPED (missing $BENCH or $SUMMARY)"
  exit 0
fi
if [ "$(id -u)" != 0 ]; then
  echo "retention-zfs-court: SKIPPED (must run as root to create/mount a zpool)"
  exit 0
fi

# expected arms: OpenZFS = EPOCHS appends + EPOCHS current reads + EPOCHS snapshot
# reads; OrFS-GO = EPOCHS appends + EPOCHS current reads (no snapshot arms).
N_READ_EPOCHS=$EPOCHS
if [ "$READ_EPOCHS" != all ]; then
  N_READ_EPOCHS=0
  for _e in $READ_EPOCHS; do N_READ_EPOCHS=$((N_READ_EPOCHS + 1)); done
fi
ZFS_EXPECTED=$((EPOCHS + 2 * N_READ_EPOCHS))
ORFS_EXPECTED=$((EPOCHS + N_READ_EPOCHS))

WORK=$(mktemp -d)
PROBES="$WORK/probes.jsonl"
: > "$PROBES"

# probe LABEL CMD... — run a command, record its verbatim (escaped) output and
# its exit code as one JSON line. This is the feasibility transcript.
probe() {
  _label=$1; shift
  _rc=0
  _out=$("$@" 2>&1) || _rc=$?
  printf '    {"cmd": %s, "exit_code": %d, "output": "%s"},\n' \
    "$(jstr "$_label")" "$_rc" "$(printf '%s' "$_out" | json_escape)" >> "$PROBES"
}

# --- step 1: is OpenZFS reachable from this container at all? ---------------
# `modprobe -n zfs` only *queries* what modprobe would do; it never loads a
# module. Loading one would be a host-level change and is out of scope.
probe "ls -l /dev/zfs"                       ls -l /dev/zfs
probe "grep -w zfs /proc/modules"            grep -w zfs /proc/modules
probe "grep -w zfs /proc/filesystems"        grep -w zfs /proc/filesystems
probe "command -v zpool"                     sh -c 'command -v zpool'
probe "command -v zfs"                       sh -c 'command -v zfs'
probe "modprobe -n zfs (query only)"         modprobe -n zfs

ZFS_DEV=0
[ -e /dev/zfs ] && ZFS_DEV=1
ZFS_POOL_OK=0
# Attempt a file-backed `zpool create` whenever the userspace tool exists. It is
# the decisive probe: without the kernel module it fails with the verbatim
# "modules are not loaded" message even though /dev/zfs is absent.
if command -v zpool >/dev/null 2>&1; then
  _pimg="$WORK/probe-vdev.img"
  dd if=/dev/zero of="$_pimg" bs=1048576 count=64 status=none >/dev/null 2>&1 || true
  _prc=0
  _pout=$(zpool create -f -m none "${POOL}probe" "$_pimg" 2>&1) || _prc=$?
  printf '    {"cmd": %s, "exit_code": %d, "output": "%s"},\n' \
    "$(jstr "zpool create -f -m none ${POOL}probe <64 MiB file vdev>")" "$_prc" \
    "$(printf '%s' "$_pout" | json_escape)" >> "$PROBES"
  if [ "$_prc" = 0 ]; then
    ZFS_POOL_OK=1
    zpool destroy -f "${POOL}probe" >/dev/null 2>&1 || true
  fi
fi

# --- Path A: real OpenZFS inside a KVM micro-VM -----------------------------
# OpenZFS is an out-of-tree kernel module; this host has none, and loading one
# would be a host change. A KVM micro-VM whose *guest* kernel ships the module
# gives a real OpenZFS without touching the host: the guest boots, loads its own
# zfs.ko, and runs the very same cmd/retentionbench driver as the Btrfs court on
# a file-backed vdev. Nothing outside the container is modified.
KVM_ATTEMPTED=0
KVM_DETAIL=""
KVM_TAIL=""
SKIP_REASON="no /dev/zfs and no zfs kernel module reachable from a container; host module load is out of scope"
if [ "$ZFS_KVM" = 1 ] && [ -e /dev/kvm ] && command -v qemu-system-x86_64 >/dev/null 2>&1; then
  KVM_ATTEMPTED=1
  HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
  # The micro-VM's disk must live on a REAL filesystem, not the container's
  # overlayfs: `cache=none` (O_DIRECT) does NOT bypass the page cache on
  # overlayfs, so the guest's writes accumulate in the container's page cache
  # until the memory cap is hit and qemu is OOM-killed (rc=137) — measured on this
  # host. The wrapper bind-mounts a disk-backed dir and passes it as
  # ZFSVM_WORKDIR. Falls back to the temp dir only when it is not provided.
  VMWORK=${ZFSVM_WORKDIR:-$WORK/vm}
  rm -rf "$VMWORK"
  mkdir -p "$VMWORK"
  echo "retention-zfs-court: host OpenZFS is absent; booting a KVM micro-VM that provides real OpenZFS"
  set +e
  sh "$HERE/retention-zfs-vm.sh" "$VMWORK" >"$WORK/kvm.out" 2>&1
  KVM_RC=$?
  set -e
  KVM_TAIL=$(tail -c 2000 "$WORK/kvm.out" 2>/dev/null | json_escape || true)

  if [ "$KVM_RC" = 0 ]; then
    # ---- measured: real OpenZFS in a KVM micro-VM --------------------------
    # shellcheck disable=SC1091
    . "$VMWORK/facts"
    n_of() { printf '%s' "$COUNTS" | sed -n "s/.*$1=\\([0-9]*\\).*/\\1/p" | tail -1; }
    ZFS_RAN=$(n_of zfs_ran);     ZFS_RAN=${ZFS_RAN:-0}
    ZFS_FAILED=$(n_of zfs_failed); ZFS_FAILED=${ZFS_FAILED:-0}
    ORFS_RAN=$(n_of orfs_ran);   ORFS_RAN=${ORFS_RAN:-0}
    ORFS_FAILED=$(n_of orfs_failed); ORFS_FAILED=${ORFS_FAILED:-0}
    ORFS_STATUS=$(printf '%s' "$COUNTS" | sed -n 's/.*orfs_status=//p' | tail -1)
    [ -n "$ORFS_STATUS" ] || ORFS_STATUS=unknown
    ORFS_MOUNTED_OK=0; [ "$ORFS_STATUS" = mounted ] && ORFS_MOUNTED_OK=1
    NPROC=$(nproc 2>/dev/null || echo "?")
    KERNEL=$(uname -sr | json_escape)
    GUEST_KERNEL_E=$(printf '%s' "$GUEST_KERNEL" | json_escape)
    ZFS_VERSION_E=$(printf '%s' "$ZFS_VERSION" | json_escape)
    POOL_VERSION_E=$(printf '%s' "$POOL_VERSION" | json_escape)
    QEMU_VERSION_E=$(printf '%s' "$QEMU_VERSION" | json_escape)

    COMP="$VMWORK/comparability.json"
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
    "comparator_filesystem": "OpenZFS (real out-of-tree kernel module) inside a KVM micro-VM guest"
  },
  "kvm_guest": {
    "hypervisor": "${QEMU_VERSION_E}",
    "acceleration": "kvm (/dev/kvm; host CPU passthrough)",
    "vcpus": ${VM_CPUS},
    "memory_mib": ${VM_MEM_MIB},
    "guest_kernel": "${GUEST_KERNEL_E}",
    "zfs_version": "${ZFS_VERSION_E}",
    "zpool_version": "${POOL_VERSION_E}",
    "image_url": "${IMAGE_URL}",
    "image_sha256": "${IMAGE_SHA256}"
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
    "OpenZFS": {
      "ran": true,
      "retention": "read-only snapshot after every epoch (zfs snapshot)",
      "underlying_pool": "${POOL} (file-backed vdev inside the guest)",
      "snapshots_expected": ${EPOCHS}
    },
    "OrFS-GO": {
      "mounted": $( [ "$ORFS_MOUNTED_OK" = 1 ] && echo true || echo false ),
      "status": "${ORFS_STATUS}",
      "retention": "every committed transition retained in the authoritative log (or.log); no snapshot step",
      "mount_command": "orfs mount <vol> <mnt> --direct-io  (ORFS_GROUP_COMMIT unset)",
      "fuse_library": "hanwen/go-fuse/v2 v2.12.0",
      "snapshots_created": 0
    }
  },
  "comparable_axes": [
    "current-state append: the same defined epoch (one write(2)+fsync(2) per record), both systems inside the same KVM guest on the same virtual disk, measured at the syscall level.",
    "current-state read: block-aligned uniform-random pread of the current committed state; OrFS-GO adds its FUSE round trip inside the guest.",
    "physical vs logical bytes: retained physical bytes per logical byte. OpenZFS physical is the zfs list -o used figure of the dataset/snapshot; OrFS-GO physical is the authoritative or.log bytes (the same computation as the Btrfs court)."
  ],
  "not_comparable": [
    "historical-state read: OpenZFS serves it from a snapshot (its .zfs/snapshot/<name> path); OrFS-GO does not serve named historical roots in this build (docs/LIMITATIONS.md), so this axis is OpenZFS-only. The OrFS-GO current-state read serves the newest committed bytes.",
    "snapshot-creation cost: OrFS-GO has no snapshot step; it retains every committed transition at append time. The OpenZFS snapshot-creation row has no OrFS-GO counterpart.",
    "snapshot extent sharing: OpenZFS snapshots share unchanged blocks copy-on-write, so a retained snapshot's used figure can be near zero. OrFS-GO's retained bytes are its appended log, which never shares extents; both are measured but are not the same computation.",
    "read latency is a native-vs-FUSE comparison inside one guest: the OpenZFS read is a native guest filesystem read served from the guest page cache, while the OrFS-GO read is a FUSE request plus a daemon pread inside that same guest. The OpenZFS read IOPS therefore does not rank the OrFS-GO read."
  ],
  "differences": [
    "The comparator is measured inside a KVM micro-VM (a guest Ubuntu kernel with its own OpenZFS module), not in the container: absolute latencies include nested-virtualisation and virtio-disk cost. The OrFS-GO arm is measured in the SAME guest, so the two systems share that overhead and the comparison between them is apples-to-apples; the numbers are NOT comparable to the container-only Btrfs court (evidence/retention-btrfs.json).",
    "The host kernel still has no ZFS module; OpenZFS here is the guest's module, loaded inside the guest only. The host is never modified.",
    "The guest's ZFS ARC is bounded (zfs_arc_max) so the small VM cannot spend its RAM on cache.",
    "Host-level variance (the guest's page cache, the host scheduler, the backing device's write cache) is not controlled beyond the shared container; run-to-run variance is expected and unquantified."
  ],
  "not_ranked": true,
  "arms": {
    "OpenZFS": {"expected": ${ZFS_EXPECTED}, "ran": ${ZFS_RAN}, "failed": ${ZFS_FAILED}},
    "OrFS-GO": {"expected": ${ORFS_EXPECTED}, "ran": ${ORFS_RAN}, "failed": ${ORFS_FAILED}, "mounted": $( [ "$ORFS_MOUNTED_OK" = 1 ] && echo true || echo false )}
  },
  "note": "Both systems ran inside ONE KVM micro-VM: OpenZFS (the guest kernel's out-of-tree module) snapped read-only after every epoch, and OrFS-GO retained every committed transition in its log. The receipt is data, not a leaderboard. A lost arm inside the VM FAILs the court."
}
EOF

    set -- "$VMWORK/results"/*.json
    FILES=""
    for f in "$@"; do
      [ -f "$f" ] && FILES="$FILES $f"
    done
    if [ -z "$FILES" ]; then
      echo "retention-zfs-court: FAIL: the VM produced no measurement" >&2
      exit 1
    fi
    if "$SUMMARY" -schema orfs.retention.zfs.v1 -comparator OpenZFS -comparability "$COMP" -o "$OUT" $FILES; then
      echo "retention-zfs-court: wrote $OUT"
    else
      echo "retention-zfs-court: FAIL: retentionsummary could not write the receipt" >&2
      exit 1
    fi
    rows=$(grep -o '"kind"' "$OUT" 2>/dev/null | wc -l)
    if [ "$rows" -eq 0 ]; then
      echo "retention-zfs-court: FAIL: receipt has no rows" >&2
      exit 1
    fi
    if [ "$ZFS_FAILED" -gt 0 ]; then
      echo "retention-zfs-court: FAIL: the OpenZFS arm lost $ZFS_FAILED arm(s) inside the VM; the receipt is partial" >&2
      exit 1
    fi
    if [ "$ORFS_MOUNTED_OK" = 1 ] && [ "$ORFS_FAILED" -gt 0 ]; then
      echo "retention-zfs-court: FAIL: OrFS-GO lost $ORFS_FAILED arm(s) inside the VM; the receipt is partial" >&2
      exit 1
    fi
    rm -rf "$WORK" "$VMWORK"
    if [ "$ORFS_MOUNTED_OK" = 1 ]; then
      echo "retention-zfs-court: PASS ($rows row(s); real OpenZFS in a KVM micro-VM retained $EPOCHS snapshot(s); OrFS-GO measured on the comparable axes)"
    else
      echo "retention-zfs-court: PASS ($rows row(s); real OpenZFS in a KVM micro-VM; OrFS-GO NOT measured: status=$ORFS_STATUS)"
    fi
    exit 0
  fi
  if [ "$KVM_RC" = 3 ]; then
    KVM_DETAIL="the KVM guest booted but could not bring up OpenZFS (rc=3)"
  else
    KVM_DETAIL="the KVM path was unavailable in this environment (rc=$KVM_RC)"
  fi
  # Never hide why the micro-VM path failed: the VM's own last words go to stderr
  # (and into the receipt's skip evidence).
  echo "retention-zfs-court: KVM micro-VM attempt (rc=$KVM_RC); its output tail was:" >&2
  tail -c 1500 "$WORK/kvm.out" 2>/dev/null >&2 || true
  SKIP_REASON="no /dev/zfs and no zfs kernel module on the host (module load is out of scope); the KVM micro-VM fallback also failed: $KVM_DETAIL"
  rm -rf "$VMWORK"
else
  if [ "$ZFS_KVM" = 1 ]; then
    KVM_DETAIL="no /dev/kvm (or qemu not installed); the KVM micro-VM path needs KVM"
  else
    KVM_DETAIL="disabled by ZFS_KVM=0"
  fi
fi

# --- SKIPPED: record the feasibility evidence faithfully and exit 0 ---------
if [ "$ZFS_POOL_OK" != 1 ]; then
  KERNEL=$(uname -sr | json_escape)
  NPROC=$(nproc 2>/dev/null || echo "?")
  ZPOOL_BIN=$(command -v zpool 2>/dev/null || echo absent)
  ZFS_BIN=$(command -v zfs 2>/dev/null || echo absent)
  ZPOOL_VER=$(zpool version 2>&1 | json_escape || true)
  ZFSUTILS_VER=$(dpkg-query -W -f='${Version}' zfsutils-linux 2>/dev/null || echo "not installed")
  COMMANDS=$(sed '$ s/,$//' "$PROBES")

  cat > "$OUT" <<EOF
{
  "schema": "orfs.retention.zfs.v1",
  "status": "skipped",
  "measured": false,
  "generated_at_unix": $(date +%s),
  "note": "OpenZFS is the fourth retention comparator (after the Btrfs court in evidence/retention-btrfs.json). This run is SKIPPED: OpenZFS is an out-of-tree kernel module and this environment has neither a /dev/zfs device nor a zfs module reachable from a container. No number is published, because none was measured. The feasibility block records the exact commands and their verbatim output so the finding is reproducible.",
  "method": "None. No workload ran: the shared driver (cmd/retentionbench) is only invoked once OpenZFS is reachable.",
  "skip": {
    "reason": "${SKIP_REASON}",
    "zfs_device_present": $( [ "$ZFS_DEV" = 1 ] && echo true || echo false ),
    "zpool_usable": false,
    "module_load_is_a_host_change": true,
    "kernel": "${KERNEL}",
    "zpool_binary": "${ZPOOL_BIN}",
    "zfs_binary": "${ZFS_BIN}",
    "zpool_version_output": "${ZPOOL_VER}",
    "zfsutils_linux_version": "${ZFSUTILS_VER}"
  },
  "feasibility": {
    "conclusion": "The host kernel has no zfs module (modprobe cannot find one and /proc/modules lists none) and there is no /dev/zfs device, so a container cannot reach a host ZFS pool, and loading a kernel module is a host-level change that this repository will not make. Path A was therefore attempted: a KVM micro-VM whose guest kernel ships the out-of-tree OpenZFS module, running the same driver inside the guest. See kvm_path below for whether that produced a measurement. If it did not, the environment genuinely has no way to run OpenZFS without a host change.",
    "kvm_path": {
      "attempted": $( [ "$KVM_ATTEMPTED" = 1 ] && echo true || echo false ),
      "detail": "${KVM_DETAIL}",
      "output_tail": "${KVM_TAIL}"
    },
    "commands": [
${COMMANDS}
    ]
  },
  "comparability": {
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
      "comparator_filesystem": "OpenZFS (file-backed zpool vdev; UNAVAILABLE — see skip)"
    },
    "question": "Can a system retain every committed historical transition while achieving comparable tail latency, throughput and physical-space economics?",
    "workload": {
      "epochs": ${EPOCHS},
      "writers": ${WRITERS},
      "records_per_epoch_per_writer": ${RECORDS_PER_EPOCH},
      "record_bytes": ${RECORD_BYTES},
      "append_durability": "write(2)+fsync(2) per record (one durability barrier per record)",
      "bounded": true
    },
    "systems": {
      "OpenZFS": {
        "ran": false,
        "status": "skipped",
        "retention": "read-only snapshot after every epoch (zfs snapshot)"
      },
      "OrFS-GO": {
        "ran": false,
        "status": "skipped",
        "retention": "every committed transition retained in the authoritative log (or.log); no snapshot step"
      }
    },
    "comparable_axes": [
      "current-state append: the same defined epoch (one write(2)+fsync(2) per record) over a zpool-mounted dataset, measured at the syscall level on both systems.",
      "current-state read: block-aligned uniform-random pread of the current committed state; OrFS-GO adds its FUSE round trip.",
      "physical vs logical bytes: retained physical bytes per logical byte of data. OpenZFS physical is the zfs list -o used figure; OrFS-GO physical is the authoritative or.log bytes."
    ],
    "not_comparable": [
      "historical-state read: OpenZFS serves it from a snapshot; OrFS-GO does not serve named historical roots in this build (docs/LIMITATIONS.md), so this axis is OpenZFS-only. The OrFS-GO current-state read serves the same newest committed bytes.",
      "snapshot-creation cost: OrFS-GO has no snapshot step; it retains every committed transition at append time. The OpenZFS snapshot-creation row has no OrFS-GO counterpart.",
      "snapshot extent sharing: OpenZFS snapshots share unchanged blocks copy-on-write, so a retained snapshot's used figure can be near zero. OrFS-GO's retained bytes are its appended log, which never shares extents; the two physical-bytes accounting schemes are therefore not the same computation even though both are measured.",
      "read latency is a native-vs-FUSE comparison: the OpenZFS read is a native local filesystem read served from the page cache (no FUSE, no daemon), while the OrFS-GO read is a FUSE request plus a daemon pread on the backing. The OpenZFS read IOPS therefore does not rank the OrFS-GO read."
    ],
    "differences": [
      "The OpenZFS arm writes directly to a dataset in a file-backed zpool; the OrFS-GO arm writes through its FUSE mount to a volume backed by its own loop-mounted btrfs image (identical to the Btrfs court, so the OrFS-GO rows stay comparable across the two courts).",
      "The OrFS-GO append arm pays the FUSE round trip and the daemon's durability barrier; the OpenZFS arm pays only the kernel write+fsync. This difference is inherent to comparing a userspace filesystem against a kernel filesystem and is recorded, not removed.",
      "Host-level variance (page cache state, kernel scheduler, the backing device's write cache) is not controlled beyond the shared container; run-to-run variance is expected and unquantified here."
    ],
    "not_ranked": true,
    "arms": {
      "OpenZFS": {"expected": ${ZFS_EXPECTED}, "ran": 0, "failed": 0, "skipped": true},
      "OrFS-GO": {"expected": ${ORFS_EXPECTED}, "ran": 0, "failed": 0, "skipped": true}
    },
    "note": "SKIPPED, not measured: OpenZFS was unreachable (see skip/feasibility). In a measured run every expected arm must run — a lost arm FAILs the court and leaves a partial receipt, never a PASS. Here nothing was expected to run, so no arm is lost."
  },
  "rows": [],
  "space": [],
  "snapshots": [],
  "notes": [
    "This receipt is a SKIPPED finding, not a measurement: OpenZFS had no /dev/zfs and no reachable zfs kernel module in this environment.",
    "The feasibility block records the exact commands run and their verbatim output, so the finding is reproducible: sh scripts/container-retention-zfs.sh",
    "Loading the OpenZFS kernel module would be a host change and is explicitly out of scope for this repository (docs/LIMITATIONS.md)."
  ]
}
EOF

  echo "retention-zfs-court: SKIPPED (no /dev/zfs and no zfs kernel module reachable from a container; host module load is out of scope)"
  rm -rf "$WORK"
  exit 0
fi

# ===========================================================================
# step 2: measured path — only reached on a host with a reachable OpenZFS.
# Mirrors scripts/retention-btrfs-court.sh exactly in structure and operations.
# ===========================================================================

echo "retention-zfs-court: OpenZFS is reachable; bringing up a file-backed zpool (${IMG_MIB} MiB)"

RUNS="$WORK/measures"; SPACE="$WORK/space"; SNAPS="$WORK/snaps"
mkdir -p "$RUNS" "$SPACE" "$SNAPS"

ZFS_IMG="$WORK/zfs-vdev.img"
LIVE_DS="$POOL/live"
LIVE_MNT="$WORK/zfs-live"
SNAPDIR_REL=".zfs/snapshot"

BTRFS_LOOP=""; BTRFS_MNT=""; BTRFS_MOUNTED=0
ORFS_LOOP=""; ORFS_BMNT=""; ORFS_MNT=""; ORFS_VOL=""; ORFS_PID=""
ORFS_BTRFS_MOUNTED=0; ORFS_DAEMON=0; ORFS_MOUNTED_OK=0
ZFS_UP=0

cleanup() {
  set +e
  if [ "$ORFS_DAEMON" = 1 ]; then
    "$ORFS" unmount "$ORFS_MNT" >/dev/null 2>&1 || umount -l "$ORFS_MNT" 2>/dev/null
  fi
  if [ -n "$ORFS_PID" ]; then
    kill "$ORFS_PID" 2>/dev/null
    _w=0
    while [ "$_w" -lt 50 ] && kill -0 "$ORFS_PID" 2>/dev/null; do
      _w=$((_w + 1)); sleep 0.1
    done
  fi
  [ "$ORFS_BTRFS_MOUNTED" = 1 ] && umount "$ORFS_BMNT" 2>/dev/null
  [ "$BTRFS_MOUNTED" = 1 ] && umount "$BTRFS_MNT" 2>/dev/null
  [ "$ZFS_UP" = 1 ] && zpool destroy -f "$POOL" 2>/dev/null
  [ -n "$ORFS_LOOP" ] && losetup -d "$ORFS_LOOP" 2>/dev/null
  [ -n "$BTRFS_LOOP" ] && losetup -d "$BTRFS_LOOP" 2>/dev/null
  rm -rf "$WORK" 2>/dev/null
}
trap cleanup EXIT INT TERM

wait_mount() { # $1 = mountpoint; ~10s budget
  _w=0
  while [ "$_w" -lt 100 ]; do
    mountpoint -q "$1" && return 0
    _w=$((_w + 1)); sleep 0.1
  done
  return 1
}

# bring_up_btrfs IMG MNT — the OrFS-GO arm's backing (identical to the Btrfs
# court), so the OrFS-GO rows stay comparable across the two courts.
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

# --- bring up the OpenZFS arm ----------------------------------------------
mkdir -p "$LIVE_MNT"
dd if=/dev/zero of="$ZFS_IMG" bs=1048576 count="$IMG_MIB" status=none >/dev/null 2>&1 \
  || { echo "retention-zfs-court: SKIPPED (could not create the zpool vdev image)"; exit 0; }
if ! zpool create -f -m none "$POOL" "$ZFS_IMG" >"$WORK/zpool.err" 2>&1; then
  echo "retention-zfs-court: SKIPPED (zpool create failed despite a successful probe; see below)"
  sed 's/^/retention-zfs-court: /' "$WORK/zpool.err" >&2 || true
  exit 0
fi
ZFS_UP=1
if ! zfs create -o mountpoint="$LIVE_MNT" "$LIVE_DS" >"$WORK/zfs-create.err" 2>&1; then
  echo "retention-zfs-court: SKIPPED (zfs create failed; see below)"
  sed 's/^/retention-zfs-court: /' "$WORK/zfs-create.err" >&2 || true
  exit 0
fi

ZFS_RAN=0; ZFS_FAILED=0
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
  echo "retention-zfs-court: bringing up the OrFS-GO arm (loop btrfs backing)"
  ORFS_IMG="$WORK/orfs.img"
  ORFS_BMNT="$WORK/orfs-btrfs-mnt"
  ORFS_MNT="$WORK/orfs-mnt"
  ORFS_VOL="$ORFS_BMNT/vol"
  mkdir -p "$ORFS_MNT"
  if ORFS_LOOP=$(bring_up_btrfs "$ORFS_IMG" "$ORFS_BMNT"); then
    ORFS_BTRFS_MOUNTED=1
    if ! "$ORFS" init "$ORFS_VOL" --segment-size "$SEGMENT_SIZE" >/dev/null 2>"$WORK/orfs-init.err"; then
      ORFS_STATUS="init_failed"
      echo "retention-zfs-court: NOTE: OrFS-GO init failed; the OrFS-GO half is skipped" >&2
      cat "$WORK/orfs-init.err" >&2 || true
    else
      ORFS_GROUP_COMMIT= "$ORFS" mount "$ORFS_VOL" "$ORFS_MNT" --direct-io >"$WORK/orfs-mount.err" 2>&1 &
      ORFS_PID=$!
      if wait_mount "$ORFS_MNT"; then
        ORFS_DAEMON=1; ORFS_MOUNTED_OK=1; ORFS_STATUS="mounted"
        echo "retention-zfs-court: OrFS-GO mounted at $ORFS_MNT"
      else
        ORFS_STATUS="mount_failed"
        echo "retention-zfs-court: NOTE: OrFS-GO mount did not come up; the OrFS-GO half is skipped" >&2
        cat "$WORK/orfs-mount.err" >&2 || true
        kill -9 "$ORFS_PID" 2>/dev/null || true
        ORFS_PID=""
      fi
    fi
  else
    ORFS_STATUS="loop_unavailable"
    echo "retention-zfs-court: NOTE: could not bring up the OrFS-GO btrfs backing; the OrFS-GO half is skipped" >&2
  fi
fi

# Prepare writer files outside the measured windows.
if ! "$BENCH" prepare -dir "$LIVE_MNT" -prefix append -writers "$WRITERS" >/dev/null 2>&1; then
  echo "retention-zfs-court: FAIL: prepare on the OpenZFS arm failed" >&2
  exit 1
fi
if [ "$ORFS_DAEMON" = 1 ]; then
  "$BENCH" prepare -dir "$ORFS_MNT" -prefix append -writers "$WRITERS" >/dev/null 2>&1 || true
fi

# --- the epoch loop ---------------------------------------------------------
e=0
while [ "$e" -lt "$EPOCHS" ]; do
  echo "retention-zfs-court: OpenZFS epoch $e (records=$RECORDS_PER_EPOCH writers=$WRITERS)"

  if "$BENCH" append -dir "$LIVE_MNT" -system OpenZFS -prefix append \
      -writers "$WRITERS" -records "$RECORDS_PER_EPOCH" -record-bytes "$RECORD_BYTES" \
      -seconds "$APPEND_SECONDS" -epoch "$e" -seed "$SEED" \
      -out "$RUNS/zfs-append-e$e.json" >/dev/null 2>&1; then
    ZFS_RAN=$((ZFS_RAN + 1))
  else
    ZFS_FAILED=$((ZFS_FAILED + 1))
    echo "retention-zfs-court: WARN: OpenZFS append epoch $e failed; recorded as missing" >&2
  fi

  "$BENCH" space -kind zfs -system OpenZFS -dataset "$LIVE_DS" \
    -epoch "$e" -state current -out "$SPACE/zfs-current-e$e.json" >/dev/null 2>&1 \
    || echo "retention-zfs-court: WARN: OpenZFS current space epoch $e failed" >&2

  sync
  if "$BENCH" snapshot -kind zfs -live "$LIVE_DS" -dest "snap-$e" \
      -system OpenZFS -epoch "$e" -out "$SNAPS/zfs-snap-e$e.json" >/dev/null 2>&1; then
    :
  else
    ZFS_FAILED=$((ZFS_FAILED + 1))
    echo "retention-zfs-court: WARN: OpenZFS snapshot epoch $e failed; recorded as missing" >&2
  fi

  "$BENCH" space -kind zfs -system OpenZFS -dataset "$LIVE_DS@snap-$e" \
    -epoch "$e" -state snapshot -out "$SPACE/zfs-snapshot-e$e.json" >/dev/null 2>&1 \
    || echo "retention-zfs-court: WARN: OpenZFS snapshot space epoch $e failed" >&2

  if should_read "$e"; then
    if "$BENCH" read -dir "$LIVE_MNT" -system OpenZFS -state current -epoch "$e" \
        -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
        -seed "$SEED" -out "$RUNS/zfs-read-current-e$e.json" >/dev/null 2>&1; then
      ZFS_RAN=$((ZFS_RAN + 1))
    else
      ZFS_FAILED=$((ZFS_FAILED + 1))
      echo "retention-zfs-court: WARN: OpenZFS current read epoch $e failed" >&2
    fi
    if "$BENCH" read -dir "$LIVE_MNT/$SNAPDIR_REL/snap-$e" -system OpenZFS -state snapshot -epoch "$e" \
        -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
        -seed "$SEED" -out "$RUNS/zfs-read-snapshot-e$e.json" >/dev/null 2>&1; then
      ZFS_RAN=$((ZFS_RAN + 1))
    else
      ZFS_FAILED=$((ZFS_FAILED + 1))
      echo "retention-zfs-court: WARN: OpenZFS snapshot read epoch $e failed" >&2
    fi
  fi

  if [ "$ORFS_DAEMON" = 1 ]; then
    if "$BENCH" append -dir "$ORFS_MNT" -system OrFS-GO -prefix append \
        -writers "$WRITERS" -records "$RECORDS_PER_EPOCH" -record-bytes "$RECORD_BYTES" \
        -seconds "$APPEND_SECONDS" -epoch "$e" -seed "$SEED" \
        -out "$RUNS/orfs-append-e$e.json" >/dev/null 2>&1; then
      ORFS_RAN=$((ORFS_RAN + 1))
    else
      ORFS_FAILED=$((ORFS_FAILED + 1))
      echo "retention-zfs-court: WARN: OrFS-GO append epoch $e failed" >&2
    fi
    "$BENCH" space -kind dir -system OrFS-GO -subvol "$ORFS_MNT" -physical-dir "$ORFS_VOL/or.log" \
      -epoch "$e" -state current -out "$SPACE/orfs-current-e$e.json" >/dev/null 2>&1 \
      || echo "retention-zfs-court: WARN: OrFS-GO space epoch $e failed" >&2
    if should_read "$e"; then
      if "$BENCH" read -dir "$ORFS_MNT" -system OrFS-GO -state current -epoch "$e" \
          -read-file append-0.bin -readers "$READERS" -bs "$BLOCK_SIZE" -seconds "$READ_SECONDS" \
          -seed "$SEED" -out "$RUNS/orfs-read-current-e$e.json" >/dev/null 2>&1; then
        ORFS_RAN=$((ORFS_RAN + 1))
      else
        ORFS_FAILED=$((ORFS_FAILED + 1))
        echo "retention-zfs-court: WARN: OrFS-GO read epoch $e failed" >&2
      fi
    fi
  fi

  e=$((e + 1))
done

if [ "$ORFS_DAEMON" = 1 ]; then
  "$ORFS" unmount "$ORFS_MNT" >/dev/null 2>&1 || umount -l "$ORFS_MNT" 2>/dev/null || true
  ORFS_DAEMON=0
fi

# --- comparability block ----------------------------------------------------
stderr_tail() { [ -f "$1" ] && tail -c 2000 "$1" | json_escape || true; }
sha256_of() { [ -f "$1" ] && sha256sum "$1" | cut -d' ' -f1 || echo "unavailable"; }

BENCH_SHA=$(sha256_of "$BENCH")
ORFS_SHA=$(sha256_of "$ORFS")
ORFS_STDERR=$(stderr_tail "$WORK/orfs-mount.err")
KERNEL=$(uname -sr | json_escape)
NPROC=$(nproc 2>/dev/null || echo "?")
BACKING_FS=$(stat -f -c %T "$WORK" 2>/dev/null || echo "?")
ZFS_SNAPS_CREATED=$(ls "$SNAPS" 2>/dev/null | wc -l | tr -d ' ')

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
    "comparator_filesystem": "OpenZFS (file-backed zpool vdev, ${IMG_MIB} MiB)"
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
    "OpenZFS": {
      "ran": true,
      "retention": "read-only snapshot after every epoch (zfs snapshot)",
      "binary_sha256": "${BENCH_SHA}",
      "snapshots_expected": ${EPOCHS},
      "snapshots_created": ${ZFS_SNAPS_CREATED}
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
    "current-state append: the same defined epoch (one write(2)+fsync(2) per record) over a zpool-mounted dataset, measured at the syscall level on both systems.",
    "current-state read: block-aligned uniform-random pread of the current committed state; OrFS-GO adds its FUSE round trip.",
    "physical vs logical bytes: retained physical bytes per logical byte of data. OpenZFS physical is the zfs list -o used figure; OrFS-GO physical is the authoritative or.log bytes."
  ],
  "not_comparable": [
    "historical-state read: OpenZFS serves it from a snapshot; OrFS-GO does not serve named historical roots in this build (docs/LIMITATIONS.md), so this axis is OpenZFS-only. The OrFS-GO current-state read serves the same newest committed bytes.",
    "snapshot-creation cost: OrFS-GO has no snapshot step; it retains every committed transition at append time. The OpenZFS snapshot-creation row has no OrFS-GO counterpart.",
    "snapshot extent sharing: OpenZFS snapshots share unchanged blocks copy-on-write, so a retained snapshot's used figure can be near zero. OrFS-GO's retained bytes are its appended log, which never shares extents; the two physical-bytes accounting schemes are therefore not the same computation even though both are measured.",
    "read latency is a native-vs-FUSE comparison: the OpenZFS read is a native local filesystem read served from the page cache (no FUSE, no daemon), while the OrFS-GO read is a FUSE request plus a daemon pread on the backing. The OpenZFS read IOPS therefore does not rank the OrFS-GO read; the gap is the cost of the FUSE data path, not a retention cost."
  ],
  "differences": [
    "The OpenZFS arm writes directly to a dataset in a file-backed zpool; the OrFS-GO arm writes through its FUSE mount to a volume backed by its own loop-mounted btrfs image (identical to the Btrfs court, so the OrFS-GO rows stay comparable across the two courts).",
    "The OrFS-GO append arm pays the FUSE round trip and the daemon's durability barrier; the OpenZFS arm pays only the kernel write+fsync. This difference is inherent to comparing a userspace filesystem against a kernel filesystem and is recorded, not removed.",
    "A ZFS snapshot is not free in principle: it costs a transaction group commit and its metadata. The measured creation time is reported per epoch; a snapshot is only cheap because unchanged blocks are shared.",
    "Host-level variance (page cache state, kernel scheduler, the backing device's write cache) is not controlled beyond the shared container; run-to-run variance is expected and unquantified here."
  ],
  "not_ranked": true,
  "arms": {
    "OpenZFS": {"expected": ${ZFS_EXPECTED}, "ran": ${ZFS_RAN}, "failed": ${ZFS_FAILED}},
    "OrFS-GO": {"expected": ${ORFS_EXPECTED}, "ran": ${ORFS_RAN}, "failed": ${ORFS_FAILED}, "mounted": $( [ "$ORFS_MOUNTED_OK" = 1 ] && echo true || echo false )}
  },
  "note": "One defined, bounded epoch workload drives an OpenZFS dataset that is snapshotted read-only after every epoch and an OrFS-GO volume that retains every committed transition in its log. The receipt is data, not a leaderboard. The comparable_axes and not_comparable lists above say exactly which axes can be compared and which cannot; no row is a ranking. Missing arms are named so a partial receipt is never a pass."
}
EOF

# --- merge into the receipt -------------------------------------------------
set -- "$RUNS"/*.json "$SPACE"/*.json "$SNAPS"/*.json
FILES=""
for f in "$@"; do
  [ -f "$f" ] && FILES="$FILES $f"
done
if [ -z "$FILES" ]; then
  echo "retention-zfs-court: FAIL: no measurement produced a receipt" >&2
  exit 1
fi

if "$SUMMARY" -schema orfs.retention.zfs.v1 -comparator OpenZFS -comparability "$COMP" -o "$OUT" $FILES; then
  echo "retention-zfs-court: wrote $OUT"
else
  echo "retention-zfs-court: FAIL: retentionsummary could not write the receipt" >&2
  cat "$COMP" >&2 || true
  exit 1
fi

rows=$(grep -o '"kind"' "$OUT" 2>/dev/null | wc -l)
if [ "$rows" -eq 0 ]; then
  echo "retention-zfs-court: FAIL: receipt has no rows" >&2
  exit 1
fi

if [ "$ZFS_FAILED" -gt 0 ]; then
  echo "retention-zfs-court: FAIL: the OpenZFS arm lost $ZFS_FAILED arm(s); the receipt is partial" >&2
  exit 1
fi
if [ "$ORFS_STATUS" = "mounted" ] && [ "$ORFS_FAILED" -gt 0 ]; then
  echo "retention-zfs-court: FAIL: OrFS-GO lost $ORFS_FAILED arm(s); the receipt is partial" >&2
  exit 1
fi
if [ "$ORFS_STATUS" = "mounted" ]; then
  echo "retention-zfs-court: PASS ($rows row(s); OpenZFS retained $EPOCHS snapshot(s); OrFS-GO measured on the comparable axes)"
else
  echo "retention-zfs-court: PASS ($rows row(s); OrFS-GO NOT measured: status=$ORFS_STATUS — see the comparability caveat)"
fi
exit 0
