# Migrating `or.log` v0 → v1

**Status:** implemented. The frozen fixtures under `tests/fixtures/` pin both
directions, and `internal/fixture` is the byte-identity court.

## 1. The wire difference

The format version lives in the **genesis payload** — the payload of the first
committed record, `VOLUME_GENESIS`. A reader decides compatibility from the log
itself, never from a rebuildable sidecar.

Genesis payload, little-endian:

| Offset | Width | Field | Present |
| ---: | ---: | --- | --- |
| 0 | 16 | volume `uuid` | always |
| 16 | 8 | `segment_size` | always |
| 24 | 2 | `format_version` | **only** in the versioned (≥ 26-byte) layout |

- **v0** — a **24-byte** genesis (`uuid ‖ segment_size`), version inferred `0`.
- **v1** — a **26-byte** genesis (`uuid ‖ segment_size ‖ format_version`),
  version `1`.

The only difference is the trailing 2-byte version field; **every transaction
record is byte-identical between the two**, so a v1 committed root is exactly
**2 bytes larger** than the equivalent v0 root. This is asserted by
`TestVersionedGenesisIsExactlyTwoBytesLarger`.

## 2. Reader algorithm

```
len = payload.length
if len < 24:     reject (corrupt: too short)
if len == 24:    format_version = 0                 # legacy: field absent
elif len >= 26:  format_version = u16_le(payload[24..26])
else:            reject (corrupt: length 25)
if format_version > LOG_FORMAT_VERSION:
    reject (UnsupportedFormatVersion)               # never guess
accept                                              # 0 <= version <= current
```

Enforced at open (`volume.Open` parses the genesis before returning). The same
rule applies to `volume.meta`'s `format_version`: a known version ≤ current is
accepted; a higher one is refused.

## 3. Compatibility matrix

| Frozen log | v0 reader | v1 reader (current) |
| --- | --- | --- |
| v0 (24-byte genesis, version `0`) | reads | **reads** — replays, verifies, reports `format_version = 0` |
| v1 (26-byte genesis, version `1`) | must not guess — treat as newer and refuse | **reads** — reports `format_version = 1` |
| v>1 (unknown) | — | **refuses** `UnsupportedFormatVersion` |

A v1 reader must never interpret a versioned genesis as if the payload ended at
24 bytes, must never fold the trailing bytes into `segment_size`, and must never
truncate, rewrite, or "repair" a record, nor mount on a guess.

## 4. Fixtures

Location: `tests/fixtures/{legacy_v0,current_v1}/`. Each is a minimal but
semantically valid volume: `volume.meta` plus a single
`or.log/0000000000000000.seg` containing a `VOLUME_GENESIS` followed by two
committed transactions (create `hello.txt`; write `hello, orfs\n`). Only the
committed bytes are included; the derived sidecars (`index/`, `.or_bloom/`,
`commit.watermark`) are deliberately absent — they are rebuildable, not part of
the frozen identity.

Regenerate (they are never edited by hand):

```sh
ORFS_GENERATE_FIXTURES=1 go test ./internal/fixture/ -run TestGenerateFixtures
```

The court (`internal/fixture/format_compat_test.go`) asserts: byte-identity
against a fresh build; replay equivalence (the logical content is
`hello, orfs\n`); the reported `format_version`; the exact 2-byte delta between
v0 and v1; and the unknown-higher-version refusal.

> **Provenance.** These fixtures are produced by the deterministic Go builder,
> not by the Rust reference. Cross-generating the same pair with the Rust
> `orfs` crate and proving the two agree byte-for-byte is the next verification
> step (see `docs/PROGRESS.md`). Until then, the wire layout is asserted against
> the documented reference layout, not against Rust-produced bytes.

## 5. Adding a future version

1. Register it in `docs/ON_DISK.md` §6 (and this file).
2. Bump `record.FormatVersion`, keeping all older versions accepted.
3. Add a fixture variant; generate bytes; extend the court.
4. Commit the frozen bytes and update the provenance note above.

There is no destructive upgrade path, by design: a newer binary reads old bytes
and, if it writes, appends new records at the committed root. The old bytes stay
authoritative.
