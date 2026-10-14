# OrFS-GO Snapshots and Index Checkpoints

**Status:** implemented (phase 21). This documents how an open bounds its index
reconstruction to the suffix after a committed checkpoint, the derived
committed-root directory that lets discovery avoid a whole-log scan, and the
measured startup/heap curve. It mirrors the Rust reference's
[`docs/SNAPSHOTS.md`](../.reference/OrFS/docs/SNAPSHOTS.md) §6 and
[`src/checkpoint.rs`](../.reference/OrFS/src/checkpoint.rs).

## 1. The problem

Recovery replays the committed log to reconstruct the namespace index. Full
replay is `O(|L|)`, so mount latency grows with retained history (Law I: history
is never reclaimed). OrFS stores periodic index checkpoints **inside the
authoritative log** as `INDEX_CHECKPOINT` records (`0x0014`) and then reconstructs
the index by loading the newest valid checkpoint and replaying **only**
`[checkpoint_root, committed_root)`.

## 2. The `INDEX_CHECKPOINT` record

The envelope is unchanged (`docs/ON_DISK.md` §3); the payload is:

```
magic         u32    ICP1
root          u64    the committed root this snapshot reflects
inode_count   u64    asserted live inode count at root
dentry_count  u64    asserted live dentry count at root
digest        [32]   BLAKE3 of the canonical snapshot document
index_len     u32
index_bytes   [index_len]  the canonical snapshot document
crc32c        u32    over all preceding payload bytes
```

The payload's own trailing CRC32C is integrity framing; the embedded `digest` is
a content digest over the snapshot document.

### 2.1 The canonical snapshot document

`index.EncodeSnapshot` / `index.DecodeSnapshot` render a versioned, deterministic
serialization of the index at a committed root — everything a replay would
produce:

```
magic "OIXS" (u32), version (u32)
root, next_inode_id, committed_records, max_tx_id
inodes   (ascending by id):      full inode metadata, xattrs, extent map
dentries (ascending by parent,name): parent, name, child
snapshots (ascending by name):   name -> root
checkpoints (replay order):      root, inode_count, dentry_count, digest
self-digest [32] BLAKE3 over every preceding byte
```

Every table is written in a fixed order, so the same index always yields the same
bytes (canonical). `DecodeSnapshot` recomputes the self-digest **before a single
field is trusted**, so a torn, truncated, or tampered body is rejected rather
than partially applied. The design is the reference's `SnapshotFile` /
`decode_snapshot` (self-digest verified), rendered as a compact binary document
instead of the reference's JSON.

## 3. Validity — a checkpoint is tied to an exact committed root

A checkpoint is accepted only when **all** hold:

1. the record is **fully framed** and lies within the committed prefix `[0, R)`;
2. its `root` is a **real committed boundary** — the end offset of a
   `VOLUME_GENESIS` or a `TX_COMMIT` — and `root <= R`;
3. the embedded snapshot decodes and passes its **self-digest**, and that digest
   equals the payload's `digest`;
4. the snapshot's root equals the payload's `root`, and its inode/dentry counts
   equal the payload's asserted counts.

The checkpoint record is committed as **its own transaction**; its `root` names
the committed boundary the snapshot reflects (the root current at write time).
Because the record is committed *after* the state it serializes, the record
itself lands inside the committed prefix and is therefore replayed as part of the
suffix on the next mount — replaying it costs one `Append` into the checkpoint
list, never a re-decode of the body. This is exactly the reference's
`write_index_checkpoint` (which sets `root = self.index.root()` before committing
the record).

### 3.1 Incomplete is never accepted

A candidate whose frame cannot be read (short frame, bad CRC, wrong length) is
**skipped**, and the next-newest candidate is tried. A difficulty discovered by a
derived structure (a bogus offset) therefore costs only a rejected candidate.

### 3.2 Corrupt is a policy decision, never silent

A **fully framed** checkpoint whose snapshot does not decode or fails its
self-digest is handled by an explicit policy:

| Policy | Behaviour |
| --- | --- |
| `FallbackFull` (**default**) | record the rejection and rebuild the index by a full replay from genesis |
| `Refuse` | return a typed `corrupt` error (`EIO`) |

The policy is selectable (`checkpoint.Options.Corrupt`, or
`volume.WithCheckpointCorruptPolicy`). A corrupt checkpoint is **never** silently
treated as good.

## 4. Suffix-only replay

`index.ReplayRange` starts at a committed boundary and decodes no byte before it
(`log.ReadRecordsRange` seeks straight to `start`), so the decoded span is exactly
`committed_root - checkpoint_root`, independent of how much history precedes it.
A `start` that does not sit on a record frame is refused rather than silently
applying a garbled record. The result is verified to land exactly on
`committed_root`; the bounded reconstruction is **byte-identical** to a full
replay (asserted at every size by the court).

## 5. The committed-root directory (derived)

Under `index/` (beside `or.log/`) the writable head maintains a small,
atomically-updated, CRC32C-framed directory:

```text
<volume>/index/committed-root.bin
  magic "ORD1", version, volume uuid, scanned_root,
  count, [checkpoint_offset] (ascending), crc32c
```

It records the committed root a scan covered (`scanned_root`) and the ascending
offsets of the checkpoints it found in `[0, scanned_root)`, so discovery can
avoid a whole-log scan. It is **derived and rebuildable**: deleting it changes
nothing recoverable. It is **distrusted** — and the log is scanned authoritatively
— when it is missing, fails its CRC, has the wrong version, names a **foreign
volume uuid**, names an offset `>= scanned_root`, is not strictly ascending (a
hole), or **over-claims** (`scanned_root` beyond the surviving committed root).
When `scanned_root < committed_root` (the log grew), only the **suffix**
`[scanned_root, committed_root)` is scanned and merged, so discovery stays cheap
as history grows.

Publishing is **writable-open only**; a read-only open consults the directory but
writes not a single byte. A directory entry is only ever a *hint*: every candidate
offset it names is read from the log and fully validated, and the suffix is
replayed, so a corrupt or stale directory can only ever cost a fallback — never a
wrong result.

`orfs inspect` reports how the index was obtained (`index source`), the
checkpoint root, and the bytes/records replayed.

## 6. Taking checkpoints

Checkpointing is **opt-in** (off by default). It writes a checkpoint as a normal
committed transaction (so it never rewrites committed bytes):

```sh
orfs checkpoint <backing>                     # take one now
ORFS_CHECKPOINT_EVERY=10000 orfs mount ...    # every 10k committed transactions
```

| Knob | Meaning |
| --- | --- |
| `volume.WithCheckpointEvery(n)` / `ORFS_CHECKPOINT_EVERY` | every `n` committed transactions |
| `volume.WithCheckpointEveryBytes(m)` / `ORFS_CHECKPOINT_BYTES` | every `m` committed bytes since the last checkpoint |

A checkpoint whose serialized payload would exceed the 64 MiB record limit is
refused with a typed error; the automatic policy is best-effort and never fails
the commit it followed.

## 7. Accelerator, not authority

Deleting **every** checkpoint (and the derived directory) changes nothing
recoverable: a full replay yields a **byte-identical** index. The court asserts
`checkpoint mount == full replay` at every size, and that deleting `index/`
leaves the reconstruction unchanged. A checkpoint may only let you *skip replay
work*; it can never change **which** root is committed (the committed root is
established exactly as in the full path, and the mount's final root is verified
to equal it).

## 8. Mount cost — measured, not claimed

Mount is **not constant-time**. Two costs remain:

* **discovery:** `O(#records)` header reads — or a directory lookup plus a suffix
  scan when the derived directory is trusted;
* **replay:** `O(bytes after the newest valid checkpoint)`.

`log.Open` still scans and validates the authoritative log in **every** arm to
establish the committed root; the checkpoint bounds only the index
reconstruction. Both arms therefore keep the authoritative scan; the checkpoint
removes the `O(|L|)` payload decode-and-apply.

### The measured curve

Court: `internal/orbench` (`TestCheckpointRecoveryCourt`), receipt
`evidence/recovery/checkpoints.json` (schema `orfs.recovery.checkpoints.v1`).
Two workloads isolate the two costs; each size is `2^k` committed bytes; each
timed arm is the best of 10 runs in the pinned container. Reported heap is Go
`HeapInuse` (not process RSS). Absolute microseconds vary run-to-run with host
load; the shape is the stable result.

**`overwrite`** — the log grows while the index stays constant (rewrite one
file). This is the shape the milestone targets: the checkpoint decodes a
constant index and the directory removes discovery, so the replayed suffix is a
constant 852 bytes no matter how large the log.

| k | committed root | txns | full replay | ckpt + suffix | directory | open (full) | open (ckpt) |
| --: | --: | --: | --: | --: | --: | --: | --: |
| 10 | 6 113 | 1 | 7 µs | 9 µs | 10 µs | 37 µs | 39 µs |
| 14 | 20 669 | 4 | 17 µs | 12 µs | 11 µs | 105 µs | 73 µs |
| 16 | 70 917 | 15 | 44 µs | 19 µs | 10 µs | 238 µs | 172 µs |
| 18 | 267 341 | 58 | 144 µs | 47 µs | 11 µs | 818 µs | 562 µs |
| 20 | 1 053 037 | 230 | 621 µs | 158 µs | 10 µs | 3 041 µs | 2 149 µs |

**`namespace`** — the index grows with the log (a new file per transaction), so
the checkpoint must decode an index that grows.

| k | committed root | txns | full replay | ckpt + suffix | directory | open (full) | open (ckpt) |
| --: | --: | --: | --: | --: | --: | --: | --: |
| 10 | 2 040 | 2 | 6 µs | 9 µs | 8 µs | 25 µs | 25 µs |
| 14 | 20 062 | 26 | 52 µs | 36 µs | 16 µs | 173 µs | 143 µs |
| 16 | 76 865 | 106 | 215 µs | 105 µs | 29 µs | 547 µs | 395 µs |
| 18 | 301 795 | 422 | 832 µs | 382 µs | 81 µs | 2 218 µs | 1 482 µs |
| 20 | 1 204 346 | 1 690 | 3 586 µs | 1 544 µs | 352 µs | 8 238 µs | 5 979 µs |

Reading the curve honestly:

* **`overwrite` (constant index): full replay is `Θ(committed bytes)` while the
  directory-assisted reconstruction is flat.** Full replay grows 7 µs → 621 µs
  across `2^10`–`2^20` (~90×); the directory arm stays at 10–11 µs because the
  decoded index is constant and the replayed suffix is a constant 852 bytes. This
  is the milestone's target shape.
* **The header-scan checkpoint arm still grows** (9 µs → 158 µs) because
  discovery reads every record header; the directory removes exactly that cost.
* **`namespace` (index grows with the log):** both arms grow (the index itself is
  `Θ(history)`), but a checkpoint replaces per-record frame-decode-and-apply with
  one snapshot decode, so the gap widens (full/ckpt ≈ 2.3× at `2^20`; adding the
  directory, ≈10×).
* **Small logs pay a tax.** At `2^10`–`2^12` the checkpoint's decode + validation
  can cost as much as replaying a tiny log, as the reference also reports; the
  checkpoint pays off once the retained log dwarfs the index.
* **The replayed suffix includes the checkpoint record's own body.** Since the
  checkpoint is written *after* the state it captures, its record is in
  `[checkpoint_root, committed_root)`; replaying it is one list append, not a
  re-decode. `checkpoint_bytes_replayed` counts the bytes read honestly (a
  constant 852 bytes in `overwrite`; ≈13% of the committed root in `namespace`
  at `2^20`).
* **End-to-end open is still scan-bound.** `log.Open` scans and validates the
  whole log in every arm — the checkpoint removes the *replay*, not the safety
  scan — so `open (ckpt)` is generally below `open (full)` (e.g. 5 979 µs vs
  8 238 µs at `namespace` 2^20) but not flat, and the two are within noise on the
  smallest logs.

The receipt has every `k` from 10 to 20 for **both** workloads, with bytes and
records replayed, Go heap, end-to-end open times, and the per-row
`correct_equals_full_replay` flag.

## 9. What is not claimed

* Mount is **not** constant-time: the authoritative scan in `log.Open` remains
  `Θ(|L|)`. The checkpoint bounds the *replay*, not the safety scan; the flat
  directory curve above is the **index reconstruction** and excludes the scan.
  A quick-open that trusts the watermark plus a persisted accelerator would bound
  the scan too; it is not implemented here.
* Absolute times are single-host, warm-cache, and Go-build-sensitive; the
  **shape** (full replay `Θ(history)` vs a flat bounded replay on a constant
  index) and the **bytes-replayed ratio** are the load-independent results.
* No checkpoint superiority is claimed for trivially small logs.
