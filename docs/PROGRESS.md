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
| 13a | Concurrency: **lock-free immutable read views**. A commit publishes a frozen namespace view (a path-copying treap for inodes + a persistent dentry treap, `internal/view`) through an `atomic.Pointer`; readers load it once per request and traverse it with **no lock**, so concurrent readers never contend with each other or with the writer, and a read still observes a whole committed transaction (never a partial one). Was: one `RWMutex` over the shared index | complete — measured at 64 readers / 8 writers (same-session before/after): `/sync/mutex/wait` down 1.5–10× (2.4× at `GOGC=100`, 10.1× at `GOGC=20`), reader throughput ~2.5×, p99.9 16.7 → 3.9 ms at `GOGC=50`, and the `RWMutex` is absent from the mutex/block profiles. Design: `docs/CONCURRENCY.md`; numbers: `docs/RUNTIME.md` §3c; receipt: `evidence/performance/gc-scaling.json` |
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
| 38 | Cross-language compatibility court: build the Rust reference (`rust:1.99.0-slim-bookworm`, capped) and drive Rust ↔ Go on the same volumes — frozen fixtures both ways, a rich Rust volume read by Go, a Go volume read by Rust, alternating mutations, recovery after a torn tail (new-segment and in-place) and after a real `SIGKILL` of the other mutator, and byte identity — publishing `evidence/correctness/crosslang.json` | complete — 12/12 matrix cases pass. The Go reader accepts every Rust-written volume; the Go *writer* defaults to a native profile (`CTIME_UPDATE` 0x0017, `MKNOD_CREATE` 0x0016) the reference cannot express, plus a new opt-in **compatibility profile** (`ORFS_COMPAT=1` / `mount --compat`) whose volumes the reference reads (its full-op case now passes on both `verify --deep` and a mount read-back). The native profile is asserted as an explicit gate (`go_native_profile_gated`) and the empirical Step-1 finding is pinned (`reference_optional_record_policy`): the reference refuses in-bracket *optional* records too, so bit 15 is not an escape hatch. A shared (Go *and* Rust) writable-open defect on an in-place torn tail remains a finding; one reader bug was fixed earlier (`TX_BEGIN` advisory `expected_record_count == 0` now means “unset”) |
| 39 | Go-runtime measurement: a mounted workload driver (`cmd/orfsrw`: 64 concurrent 4 KiB random readers + concurrent writers, exact application p50/p99/p99.9, throughput, errno-classified errors), a `--profile-dir` mount flag that makes the daemon dump windowed `runtime/metrics` (GC pauses, scheduler latency, allocation rate, mutex wait), real `VmRSS`/`VmHWM`, and pprof profiles on shutdown/SIGUSR1, a `GOGC`×`GOMEMLIMIT` sweep, and `cmd/gcsummary` merging it into `evidence/performance/gc-scaling.json` (`orfs.performance.gc-scaling.v1`), with `docs/RUNTIME.md` analysing it | complete — bounded 9-arm sweep: p99.9 ranges 14.8 ms (`GOGC=20`) to 73.6 ms (`GOGC=off`+512MiB); GC stop-the-world pauses ≤ 328 µs, so **pauses are not the tail** (the ~2 GB/s allocation rate and a single shared `RWMutex` are); `GOMEMLIMIT` is inert until it binds; `GOGC=off` with no memory limit is OOM-killed ~1.3 s into the run (recorded `daemon_died`, not fabricated). The container runtime-metrics collector (`internal/gcruntime`) carries unit tests. (The shared `RWMutex` this phase named was removed in phase 13a; the re-run and before/after are in `docs/RUNTIME.md` §3–§3c) |
| 40 | Manifested-body admission: route the resident manifested-body cache (`manifest.Cache.live`, once an unbounded map) through `internal/fcache` S3-FIFO admission keyed by the manifested identity `(inode, version, root)`, bounded and configurable (`--manifest-cache`/`--manifest-policy`), preserving exact-identity certification, the authority binding, and the negative cache; a court drives the live manifested read path over a scan-heavy trace (S3-FIFO vs LRU vs the unbounded map) and a correctness court forces constant eviction under a capacity-2 cache | complete — burst scan: S3-FIFO hot-set hit rate 1.00 in a bounded 64 bodies vs LRU 0.00 at the same bound; the unbounded map keeps 1.00 but grows to 512 residents; interleaved shape: LRU matches (no admission win); 400/400 eviction-forced reads byte-identical to authority. `evidence/performance/manifest-admission.json` |
| 41 | Audit-log sink milestone: a `cmd/orfsaudit` JSON-event writer (seeded 80/20 hot/cold corpus, configurable rate) with an inotify tail reader (poll fallback, append-to-observation lag p50/p99), a mounted immutability proof (`SIGKILL` the FUSE-client writer mid-append) plus the existing in-Go crash court, a live `tail -f`, a Prometheus exposition snapshot and a 5-panel Grafana dashboard, and the production story | complete — `evidence/audit-sink.json`; 1500 events, 150 rec/s, append ≈ 2.3 ms p50 vs barrier ≤ 1 µs (the FUSE tax), tail lag ≈ 2.4 ms p50 (inotify), amplification 1.044×, sink quarantine/breach 0; 0 prefix mismatches / 0 truncations across 8 mounted kills and 120 log-level kills (120/120 verify --deep) |

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

The cross-language court found one genuine Rust↔Go incompatibility, now resolved
by an explicit compatibility profile rather than by weakening the native format.
Go's native writer emits `CTIME_UPDATE` (`0x0017`) — and, for special files,
`MKNOD_CREATE` (`0x0016`) — mandatory record types that the Rust reference's
registry does not define (it ends at `PERMISSION_CHANGE`, `0x0015`). Go emits
`CTIME_UPDATE` on hard link, rename-overwrite, chmod, and unlink (see
`internal/namespace/mutate.go`); a Rust reader treats the type as an unknown
*mandatory* record and refuses the volume, so `orfs verify --deep` and
`orfs mount` fail with `unknown mandatory record type 0x0017`.

**Empirical Step-1 result (measured against the real `orfs` 0.2.0 binary).** The
obvious remedy — move the two types into the format's *optional* space (set bit
15, which the format reserves for records a reader “may skip”) — was tested and
rejected. A volume whose transaction carried a `0x8017` record with the bracket
counts Go computes today is still refused by the reference:
`record type 0x8017 is optional and has no known semantics`. Its `verify --deep`
Plane-B index rebuild (`Index::from_log` → `apply_record`) and its `mount` both
call `Record::known_type()`, which hard-errors on an optional-unknown type; only
its log *scan* counts optional records into the `TX_COMMIT` bracket. So bit 15
is not a compatibility escape hatch, and — independently — skipping a
`MKNOD_CREATE` would leave a `DENTRY_ADD` naming an inode the reference never
created. The court reproduces this as `reference_optional_record_policy`.

**Remediation.** A new opt-in **compatibility profile** (`ORFS_COMPAT=1`, or
`orfs mount --compat`; programmatically `volume.WithCompat()`): the writer emits
only the reference registry and semantics — no `CTIME_UPDATE` (ctime is derived
from mtime on `INODE_SETATTR`, exactly as the reference does) and no
`MKNOD_CREATE` (special-file creation is refused with `EOPNOTSUPP`). The
**default stays native** (full POSIX ctime and special files); see
`docs/MIGRATION.md` §4a and `docs/ON_DISK.md` §3.3. The court now writes the Go
side of every Go→Rust case in the compatible profile (12/12 pass, including the
full-op case on both `verify --deep` and a mount read-back), and asserts the
native-profile divergence as an explicit gate (`go_native_profile_gated`) instead
of claiming compatibility.

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
