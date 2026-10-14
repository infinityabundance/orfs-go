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
`docs/ON_DISK.md` Part II).

The Rust reference ships its own byte-frozen pair at
`.reference/OrFS/tests/fixtures/{legacy_v0,current_v1}`. The cross-language court
(`scripts/container-crosslang.sh`, `evidence/correctness/crosslang.json`) builds
the Rust `orfs` binary and now treats **those** bytes as ground truth: Go must
replay them to exactly `hello.txt` containing `hello, orfs\n`, and the Go codec
must re-encode every Rust frame byte-for-byte (both asserted). The court also
proves the Rust reader accepts these Go fixtures.

The two builders do **not** produce identical bytes for the same fixture: the Go
builder uses its own volume UUID (`4f52…6531`), segment size (64 MiB),
timestamps, `TX_BEGIN` advisory `expected_*` values, and record-header inode
stamps. That divergence is reported in the court receipt, not hidden; it is a
property of the builders' defaults, not of the codec.

`volume.meta` creation time is fixed at `1_700_000_000` and the UUID is fixed
(`4f 52 46 53 2d 47 4f 2d 66 69 78 74 75 72 65 31`) so the bytes are
reproducible.
