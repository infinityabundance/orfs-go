# Frozen compatibility fixtures

Each directory is a minimal but semantically valid OrFS volume: `volume.meta`
plus a single `or.log/0000000000000000.seg` holding a `VOLUME_GENESIS` followed
by two committed transactions (create `hello.txt`; write `hello, orfs\n`).

Only the committed bytes are included. The derived sidecars (`index/`,
`.or_bloom/`, `commit.watermark`) are deliberately absent: they are rebuildable,
not part of the frozen identity.

| Directory | Format | Genesis payload | Version |
| --- | --- | --- | --- |
| `legacy_v0/` | pre-versioning | 24 bytes (`uuid ‖ segment_size`) | inferred `0` |
| `current_v1/` | current | 26 bytes (`uuid ‖ segment_size ‖ format_version`) | `1` |

The two differ by exactly the trailing 2-byte version field; every transaction
record is byte-identical (`TestVersionedGenesisIsExactlyTwoBytesLarger`).

## Regeneration

A fixture is never edited by hand and never regenerated silently. If a change
stops reading a frozen fixture, `internal/fixture/format_compat_test.go` is
expected to fail.

```sh
ORFS_GENERATE_FIXTURES=1 go test ./internal/fixture/ -run TestGenerateFixtures
```

## Provenance

These fixtures are produced by the deterministic Go builder
(`internal/fixture`), which encodes the byte layout documented in
`docs/ON_DISK.md` and `docs/MIGRATION.md` (matching the Rust reference's
`docs/ON_DISK.md` Part II). Cross-generating the same pair with the Rust `orfs`
crate and proving byte-for-byte agreement is the tracked follow-up
(`docs/PROGRESS.md` phase 25).

`volume.meta` creation time is fixed at `1_700_000_000` and the UUID is fixed
(`4f 52 46 53 2d 47 4f 2d 66 69 78 74 75 72 65 31`) so the bytes are
reproducible.
