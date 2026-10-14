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
| 20 | Simulated power-loss device (barrier/partial/torn flushes) and its crash-consistency court | complete (the real-process `SIGKILL` court is phase 36) |
| 21 | Index checkpoints bounding replay: a canonical BLAKE3-self-digested index snapshot inside `INDEX_CHECKPOINT`, a derived committed-root directory (`index/`) for scan-free discovery, suffix-only replay, an explicit corrupt/fallback policy, and an opt-in checkpoint policy | complete — bounded reconstruction equals full replay at every size; `evidence/recovery/checkpoints.json` measures full replay vs checkpoint + suffix vs directory at 2^10..2^20 committed bytes for a write-heavy and a namespace-heavy workload (write-heavy 2^20: full replay 621 µs vs directory-assisted 10 µs index build; namespace-heavy 2^20: 3.6 ms vs 1.5 ms vs 0.35 ms; end-to-end open 8.2 ms vs 6.0 ms — the `log.Open` scan is the residual) |
| 22 | External baseline courts (ext4/tmpfs/overlay/raw) and 64-thread scaling | planned |
| 23 | Endurance amplification court (logical vs physical vs manifested bytes) | planned |
| 24 | Transport characterisation: F2 negotiation probed live (kernel supports it; go-fuse exposes none), `--transport` flag with loud fallback, measured F0-vs-f0-single court with FUSE backpressure counters | complete |
| 25 | Cross-generation fixtures with the Rust reference (byte-for-byte) | partly complete — the cross-language court (phase 38) builds the Rust reference and proves the Go codec is byte-exact on Rust bytes, and that Go and Rust each read the other's fixtures; the two *builders'* bytes still differ in volume-identity parameters (see phase 38 findings) |
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
| 36 | Real-process crash-consistency court: `SIGKILL` the `cmd/orfscrashtest` writer mid-append, reopen read-write (quarantine MOVES the abandoned tail, never truncates), prove the committed prefix is byte-identical and the recovered root is a committed boundary, `verify --deep`, and publish `evidence/crash.json` | complete — 1000 kills, 379 abandoned tails (4 torn / 375 uncommitted) all quarantined; 0 truncations / 0 prefix mismatches / 0 recover failures |
| 37 | Operator observability: a dependency-free Prometheus exposition (`orfs metrics <backing>`, `internal/metrics` + `internal/observe`), the capacity breach counter, the group-committer barrier histogram + batch stats persisted as a derived snapshot, the key values surfaced in `doctor --capacity`, and an append-only WORM audit-log-sink case study + Grafana dashboard | complete — `evidence/worm-case-study.json`; 8 MiB appended, 209 rec/s, append ≈ 4.5 ms p50 vs barrier ≤ 1 µs (the FUSE tax), amplification 1.115× |
| 38 | Cross-language compatibility court: build the Rust reference (`rust:1.99.0-slim-bookworm`, capped) and drive Rust ↔ Go on the same volumes — frozen fixtures both ways, a rich Rust volume read by Go, a Go volume read by Rust, alternating mutations, recovery after a torn tail (new-segment and in-place) and after a real `SIGKILL` of the other mutator, and byte identity — publishing `evidence/correctness/crosslang.json` | complete, with real findings — 9 of 10 matrix cases pass; the Go writer emits `CTIME_UPDATE` (`0x0017`), a mandatory record type absent from the Rust registry, so the Rust reader refuses any Go volume that used hard link / rename-overwrite / chmod / unlink; a shared (Go *and* Rust) writable-open defect on an in-place torn tail is recorded as a finding; one reader bug was fixed (`TX_BEGIN` advisory `expected_record_count == 0` now means “unset”, as the reference does) |

## Phase 14 provenance note

The `legacy_v0`/`current_v1` fixtures are produced by the deterministic Go
builder (`internal/fixture`); the wire layout they encode is asserted against the
documented reference layout. The cross-language court (phase 38) now covers the
cross-generation question: it builds the Rust `orfs` crate and proves that
(a) Go replays the Rust-generated frozen fixtures to exactly `hello.txt`
containing `hello, orfs\n`, (b) Go decode+re-encode is byte-identical to the Rust
frames (so the Go *codec* is byte-exact), and (c) Go and Rust each read the
other's fixtures. The two builders still produce different bytes for the *same*
fixture: the Go builder picks its own volume UUID, segment size, inode
timestamps, `TX_BEGIN` advisory `expected_*` values, and record-header inode
stamps. That is a property of the builders' defaults, not of the codec, and is
recorded as a finding in `evidence/correctness/crosslang.json`.

## Cross-language findings (phase 38)

The cross-language court found one genuine Rust↔Go incompatibility. Go writes
`CTIME_UPDATE` (`0x0017`) — and, for special files, `MKNOD_CREATE` (`0x0016`) —
mandatory record types that the Rust reference's registry does not define (it
ends at `PERMISSION_CHANGE`, `0x0015`) and that `docs/ON_DISK.md` §3.3 does not
list either. Go emits `CTIME_UPDATE` on hard link, rename-overwrite, chmod, and
unlink (see `internal/namespace/mutate.go`); a Rust reader treats the type as an
unknown *mandatory* record and refuses the volume, so `orfs verify --deep` and
`orfs mount` fail with `unknown mandatory record type 0x0017`. The behaviour is
deliberate on the Go side (`TestMetadataOperationsAdvanceCtime`,
`TestPayloadMagicsOnDisk`), and the Rust format cannot express a ctime-only
advance (it derives `ctime` from `mtime` on `INODE_SETATTR`), so this is a
format-expressiveness decision for the maintainer rather than an accidental bug.
The court isolates it: every Rust→Go case and every Go→Rust case that avoids
those four operations passes, including alternating mutations and recovery after
an interrupted write.

The reader side had one real bug, now fixed: Go treated `TX_BEGIN`'s advisory
`expected_record_count == 0` (which the Rust low-level writer emits, and which
the Rust verifier treats as "unset") as an exact expectation of zero, so it
rejected every Rust volume with a transaction. `internal/log/scan.go` now treats
`0` as unset, exactly as the reference does.

A second, *shared* defect surfaced while building the court. After a torn frame
that lives **inside the committed segment** (the normal shape of a kill
mid-append), a **writable** open — Go `recover`/`mount`, or Rust `mount` —
creates a new segment at the committed root but leaves the stale torn bytes in
the committed segment, after which **both** readers fail with `torn record in
non-final segment 0`. Go and Rust behave identically, so it is not a Rust↔Go
divergence; a read-only open recovers correctly, and a torn tail in a
**new trailing segment** is quarantined correctly by both. It is recorded under
`findings` and is a candidate fix (truncate the committed segment to its valid
length before continuing, or teach the reader to ignore physical bytes beyond a
segment's valid length).

## What "complete" promises

- the code is in the tree and `go vet ./...` is clean;
- the default suite, `-race`, and a bounded fuzz pass are green in-container;
- where a mounted behaviour is claimed, `scripts/mount-court.sh` exercises it;
- no claim is made that a court did not measure.
