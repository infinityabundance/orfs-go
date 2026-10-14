# Progress

Ordered phases. A phase is `complete` only when its code, tests, and (where
applicable) documentation are in the tree and the container gate passes. A
command that is part of the design but not yet wired rejects the request with
its owning phase rather than pretending to work.

| Phase | Area | State |
| ---: | --- | --- |
| 1 | Record envelope, registry, all typed payloads (byte-exact) | complete |
| 2 | Segments, naming, contiguity locator, discovery | complete |
| 3 | The authoritative log: scan, transactions, bracket verification | complete |
| 4 | Genesis identity and `format_version` (`v0`/`v1`) | complete |
| 5 | Commit watermark | complete |
| 6 | Volume init/open, identity cross-check | complete |
| 7 | Namespace model (inodes, dentries, extents) and replay | complete |
| 8 | POSIX operations as transactions | complete |
| 9 | Permissions: class rule, supplementary groups, sticky bit, dir-link `EPERM` | complete |
| 10 | Capacity guard: policy, `CapacityExhausted`, `doctor --capacity` | complete |
| 11 | Read-only verification (`verify`, `verify --deep`, `inspect`) | complete |
| 12 | Recovery: newest valid committed generation; quarantine-not-truncate | complete |
| 13 | FUSE mount and the mounted POSIX court | complete |
| 13a | Concurrency: an RWMutex guards the shared index (readers run in parallel; a reader never sees a partial transaction) | complete; lock-free immutable read views are the optimization |
| 14 | Frozen `legacy_v0`/`current_v1` fixtures + byte-identity court | complete |
| 15 | Typed errors with errno mapping; honest CLI | complete |
| 16 | Fuzzing (frame + payload decoders); `-race` on the shared index | complete |
| 17 | One-command container verification (`scripts/container-verify.sh`) | complete |
| 18 | Sealed dense-ordinal inode spine for the historical role (10.5× smaller, ~40× faster lookup than a map) | complete |
| 19 | Concealed manifestation on the EKBLOOM codec: manifested bodies, exact-identity certification (body self-digest + authority binding), honest fallback, live `read(2)` selection, `orfs manifest`, and a selection/correctness/latency court | complete — measured p50 ~2× better, p99 not claimed, amplification ~1.00× (root profile) |
| 20 | Simulated power-loss device (barrier/partial/torn flushes) and its crash-consistency court | complete |
| 21 | Named historical snapshots and index checkpoints bounding replay | planned |
| 22 | External baseline courts (ext4/tmpfs/overlay/raw) and 64-thread scaling | planned |
| 23 | Endurance amplification court (logical vs physical vs manifested bytes) | planned |
| 24 | Transport characterisation: F2 negotiation probed live (kernel supports it; go-fuse exposes none), `--transport` flag with loud fallback, measured F0-vs-f0-single court with FUSE backpressure counters | complete |
| 25 | Cross-generation fixtures with the Rust reference (byte-for-byte) | planned |
| 26 | Growth/amplification court (non-withdrawal) + receipts | complete |
| 27 | External baseline court (`fio` vs ext4/tmpfs, honest FUSE tax) + 64-thread scaling | complete |
| 28 | fsstress-equivalent court (bounded op storm + POSIX extras + `verify --deep`) | complete |
| 29 | Capacity `CapacityExhausted` + quarantine-never-truncates courts + receipts | complete |
| 30 | Multi-arch publish workflow + tagged release | complete |
| 31 | Scan-resistant admission court (S3-FIFO vs FIFO vs LRU) | complete |
| 32 | A raw pure-Go F2 transport (blocked: no Go FUSE library exposes io_uring — see phase 24); per-read allocs court | F2 blocked (phase 24); allocs court complete — authoritative 4 KiB read 2 -> 1 allocs/op (`PlanReadInto`, destination `[]byte` is the only allocation) |
| 33 | Cross-FS determinism (identical authoritative log bytes across ext4/xfs/btrfs/tmpfs, fixed uuid + pinned clock) | complete |
| 34 | pjdfstest court (upstream POSIX syscall suite vs the mount; deviations published, not allowlisted) | complete — 8798/8798 assertions pass, 0 deviations |
| 35 | Group commit: an opt-in `Log` coordinator coalescing concurrent publishes behind one durability barrier (own root + contiguous sequence numbers per transaction; all-or-nothing on a failed fsync), with correctness courts and an OFF-vs-ON receipt | complete — 4.31× throughput, 4.49× better p50 on the container backing |

## Phase 14 provenance note

The `legacy_v0`/`current_v1` fixtures are produced by the deterministic Go
builder (`internal/fixture`); the wire layout they encode is asserted against the
documented reference layout. Cross-generating the same pair with the Rust `orfs`
crate and proving byte-for-byte agreement is phase 25.

## What "complete" promises

- the code is in the tree and `go vet ./...` is clean;
- the default suite, `-race`, and a bounded fuzz pass are green in-container;
- where a mounted behaviour is claimed, `scripts/mount-court.sh` exercises it;
- no claim is made that a court did not measure.
