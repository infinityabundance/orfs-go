# OrFS-GO On-Disk Format

**Status:** implemented. This documents the bytes this build writes and accepts;
it is wire-compatible with the Rust reference's `docs/ON_DISK.md` Part II.

All multi-byte integers are **little-endian**. No struct is reinterpreted over
untrusted bytes; every field is decoded explicitly (`internal/record`).

## 1. Volume directory

```text
<volume>/
├── volume.meta          volume identity (fixed size, checksummed)
├── or.log/              the authoritative log
│   ├── 0000000000000000.seg
│   ├── ...
│   └── quarantine/      retained abandoned segment tails (never canonical)
└── .or_bloom/           concealed acceleration structures (Plane C; derived)
```

`.or_bloom/` is **derived**: it is not authoritative, is not checksummed by the
log, and deleting it changes nothing recoverable. Its layout is specified in
`docs/MANIFESTATION.md` §2.

## 2. Segments

| Property | Value |
| --- | --- |
| Suffix | `.seg` |
| Name | `NNNNNNNNNNNNNNNN.seg` — exactly 16 zero-padded decimal digits |
| Segment id | the first logical byte offset the segment covers |
| Logical→physical | for canonical logical offset `L` in segment `S`: `L - S` |
| Contiguity | `extents[i+1].id == extents[i].end()`; a gap or overlap is corruption |
| First id | must be `0` |
| Max id | `9_999_999_999_999_999` |

Segments are append-only and a record never straddles two segments (the writer
rolls over before writing). Discovery ignores every non-segment entry, sorts ids,
and reports duplicate ids as corruption.

## 3. Record envelope

```text
RECORD HEADER (60) || PAYLOAD (N) || RECORD FOOTER (16)      total = 76 + N
```

### 3.1 Header (60 bytes)

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 4 | `magic` = `0x5346524F` (`"ORFS"`) |
| 4 | 2 | `format_version` = `1` |
| 6 | 2 | `record_type` |
| 8 | 4 | `header_length` = `60` |
| 12 | 8 | `payload_length` (≤ 64 MiB) |
| 20 | 8 | `sequence_number` |
| 28 | 8 | `transaction_id` |
| 36 | 8 | `inode_id` |
| 44 | 8 | `previous_inode_offset` |
| 52 | 4 | `flags` (reserved, `0`) |
| 56 | 4 | `header_check` = CRC32C over `[0, 56)` |

Decode order (checksum first, so a partial header is a *torn tail*):
fewer than 60 bytes → torn; bad `header_check` → torn; bad magic → corrupt;
`header_length != 60` → corrupt; `format_version != 1` → compatibility;
`payload_length > 64 MiB` → corrupt, checked before any allocation.

### 3.2 Footer (16 bytes)

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | `total_record_length` = `60 + N + 16` |
| 8 | 4 | `record_check` = CRC32C over header `||` payload |
| 12 | 4 | `end_marker` = `0x21444E45` (`"END!"`) |

A footer whose length or checksum disagrees is **torn**; a bad end marker (with
an otherwise valid record check) is **corrupt**.

### 3.3 Record type registry (format version 1)

`0x0001` `VOLUME_GENESIS`, `0x0002` `TX_BEGIN`, `0x0003` `TX_COMMIT`,
`0x0004` `TX_ABORT`, `0x0005` `INODE_CREATE`, `0x0006` `INODE_METADATA`,
`0x0007` `INODE_SETATTR`, `0x0008` `DENTRY_ADD`, `0x0009` `DENTRY_REMOVE`,
`0x000A` `DENTRY_RENAME`, `0x000B` `DATA_EXTENT`, `0x000C` `DATA_ZERO_RANGE`,
`0x000D` `FILE_RESIZE`, `0x000E` `SYMLINK_CREATE`, `0x000F` `HARDLINK_ADD`,
`0x0010` `HARDLINK_REMOVE`, `0x0011` `XATTR_SET`, `0x0012` `XATTR_REMOVE`,
`0x0013` `SNAPSHOT_ROOT`, `0x0014` `INDEX_CHECKPOINT`, `0x0015`
`PERMISSION_CHANGE`.

Bit 15 (`0x8000`) marks a type *optional*: an unknown optional type may be
skipped; an unknown type with bit 15 clear is *mandatory* and stops the reader
with a compatibility error.

## 4. Typed payloads

Every payload begins with a 4-byte magic and ends with a 4-byte trailing CRC32C
over all preceding payload bytes. The last character of each magic is a revision
digit; there is no separate version field.

| Type | Magic | Layout |
| --- | --- | --- |
| `TX_BEGIN` | `TBN1` | fixed 28: `tx_id:u64`, `expected_byte_length:u64`, `expected_record_count:u32`, crc |
| `TX_COMMIT` | `TCM1` | fixed 72: `tx_id:u64`, `record_count:u64`, `bracketed_byte_length:u64`, `commit_sequence_number:u64`, `bracket_checksum:[32]` (BLAKE3), crc |
| `TX_ABORT` | `TBA1` | prefix 20: `tx_id:u64`, `reason:u16`, `reserved:u16`, `msg_len:u32`, message, crc |
| `INODE_CREATE` | `ICR1` | fixed 88 |
| `SYMLINK_CREATE` | `SLK1` | `inode_id:u64`, `generation:u64`, `mode:u32`, `uid:u32`, `gid:u32`, `mtime:12`, `target_len:u32`, target, crc |
| `INODE_METADATA` | `IMT1` | fixed 20: `inode_id:u64`, `nlink:u32`, crc |
| `PERMISSION_CHANGE` | `PCH1` | fixed 32: `inode_id:u64`, `mode_present:u8`, `uid_present:u8`, `gid_present:u8`, pad, `mode:u32`, `uid:u32`, `gid:u32`, crc |
| `INODE_SETATTR` | `ISA1` | fixed 44: `inode_id:u64`, `atime_present:u8`, `mtime_present:u8`, pad, `atime:12`, `mtime:12`, crc |
| `XATTR_SET` | `XAS1` | `inode_id:u64`, `name_len:u32`, name, `value_len:u32`, value, crc |
| `XATTR_REMOVE` | `XAR1` | `inode_id:u64`, `name_len:u32`, name, crc |
| `DENTRY_ADD` / `HARDLINK_ADD` | `DAD1` / `HLA1` | `parent:u64`, `child:u64`, `name_len:u32`, name, crc |
| `DENTRY_REMOVE` / `HARDLINK_REMOVE` | `DRM1` / `HLR1` | `parent:u64`, `name_len:u32`, name, crc |
| `DENTRY_RENAME` | `DRN1` | `flags:u8`, pad3, `old_parent:u64`, `new_parent:u64`, `old_name_len:u32`, old_name, `new_name_len:u32`, new_name, crc |
| `DATA_EXTENT` | `DEX1` | `offset:u64`, `data_len:u64`, data, crc (data at payload offset 20) |
| `DATA_ZERO_RANGE` | `DZR1` | fixed 24: `offset:u64`, `len:u64`, crc |
| `FILE_RESIZE` | `FRS1` | fixed 16: `new_size:u64`, crc |
| `SNAPSHOT_ROOT` | `SNR1` | `root:u64`, `name_len:u32`, name, crc |
| `INDEX_CHECKPOINT` | `ICP1` | `root:u64`, `inode_count:u64`, `dentry_count:u64`, `digest:[32]`, `index_len:u32`, index_bytes, crc |

A fixed `time` field is `Timespec{secs:i64, nanos:u32}` (nanosecond granularity).

**Subject-inode rule.** For a payload that carries `inode_id` (`INODE_*`,
`XATTR_*`, `SYMLINK_CREATE`), the subject is the payload id when non-zero, else
the header id; the result must be non-zero, and if both are non-zero and differ
the record is corrupt. `DATA_EXTENT`, `DATA_ZERO_RANGE`, and `FILE_RESIZE` carry
no payload inode id and take the header's `inode_id`.

**Name rule.** A committed name is non-empty, ≤ 255 bytes, contains no `/` and
no NUL, and is not `.` or `..`.

## 5. `volume.meta` (44 bytes)

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 4 | `magic` = `0x5646524F` (`"ORFV"`) |
| 4 | 2 | `format_version` = `1` |
| 6 | 2 | `reserved` = `0` |
| 8 | 16 | `uuid` |
| 24 | 8 | `created_at_unix_secs` |
| 32 | 8 | `segment_size` |
| 40 | 4 | `meta_check` = CRC32C over `[0, 40)` |

Written atomically (`tmp → fsync → rename → dir fsync`) **after** the genesis
record, so a crash during `init` leaves an orphan log, not a volume without
authoritative content.

## 6. Genesis payload (24 or 26 bytes)

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 16 | volume `uuid` |
| 16 | 8 | `segment_size` |
| 24 | 2 | `format_version` (v1 only) |

A 24-byte payload has no version field (version = `0`, legacy). A ≥ 26-byte
payload carries the little-endian version at offset 24. A 25-byte payload is
malformed. A version greater than the current one is refused
(`EPROTONOSUPPORT`), never guessed.

## 7. Commit watermark (36 bytes)

`or.log/commit.watermark`:

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 4 | `magic` = `0x5746524F` (`"ORFW"`) |
| 4 | 2 | `format_version` |
| 6 | 2 | `reserved` |
| 8 | 8 | `committed_root` |
| 16 | 8 | `segment_id` |
| 24 | 8 | `segment_valid_len` |
| 32 | 4 | CRC32C over `[0, 32)` |

Written only *after* the bytes it describes are durable. Recovery refuses an
open when the watermark certifies a committed root beyond the surviving history
(a trailing committed segment is missing or truncated) rather than silently
recovering shorter history. A missing watermark leaves the volume *unvalidated*;
a present-but-corrupt watermark is an error, never ignored.

## 8. Integrity model

`header_check` and `record_check` are CRC32C framing/integrity checks; the
`bracket_checksum` is a BLAKE3 content digest. **These are integrity, not
authentication.** There is no MAC, no signature, and no encryption: an adversary
with write access to the backing directory can rewrite history and recompute the
digests. Non-withdrawal is a design law enforced by the implementation, not a
cryptographic guarantee. See `docs/SECURITY.md`.
