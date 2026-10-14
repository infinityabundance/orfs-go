# OrFS-GO Specification

**Status:** normative for the bytes this build writes and accepts. This Go
edition is wire-compatible with the Rust `OrFS` reference (`docs/ON_DISK.md`
Part II). Where the two disagree, the reference's byte layout governs; this
document records only where the Go port deviates, which it does nowhere in the
committed format.

All multi-byte integers are **little-endian**. Fields are encoded and decoded
explicitly, field by field; no Go struct is ever reinterpreted over untrusted
bytes.

## 1. Authority and planes

`or.log` (the authoritative append-only log) is the source of truth. EKBLOOM is
the outward acceleration engine, consumed as an independent, pure-Go dependency
(`github.com/infinityabundance/ekbloom-go`). FUSE is the Linux interface.

Four planes:

| Plane | Name | Responsibility |
| --- | --- | --- |
| A | Authoritative history | permanent records, transaction ordering, committed roots, replay/recovery |
| B | Namespace interpretation | ordinary FS semantics reconstructed from the log: inodes, directories, extents |
| C | Manifested acceleration | read/query structures that never alter authoritative history |
| D | Transport & storage | the FUSE transport and backing I/O |

Plane-separation invariants: a fault in the manifested plane must never corrupt
the authoritative plane; a change to the transport plane must never change
filesystem semantics.

## 2. The two laws

**Law I — Non-withdrawal.** Committed authoritative history is never
overwritten, truncated, reused, or garbage-collected. Every committed transition
extends the previous history:

```
L_{n+1} = L_n ⊕ Δ_n        (L_0 ⪯ L_1 ⪯ L_2 ⪯ …)
```

Logical deletion, truncation, and rename are new committed events, not in-place
mutation of prior records. A root is a *position* (committed end offset) in a
volume's authoritative record sequence, not a content hash.

**Law II — Concealed expansion.** Additional operational structures are
deliberately manifested *behind* the ordinary logical namespace, spending
storage, memory, and construction work to accelerate reads. The manifested body
may be larger than the logical contents and is not exempt from physical
accounting.

## 3. The record envelope

```
RECORD HEADER   (60 bytes)
PAYLOAD         (payload_length bytes)
RECORD FOOTER   (16 bytes)
```

See `docs/ON_DISK.md` §2–§7 for the byte-exact header, footer, CRC coverage,
record-type registry, and the typed payloads. The Go implementation lives in
`internal/record`.

## 4. Transactions and the committed root

Records between a `TX_BEGIN` and its matching `TX_COMMIT` are one atomic unit.
Only a complete, valid `VOLUME_GENESIS` or `TX_COMMIT` advances the committed
root `R`. `TX_ABORT` closes a bracket without publishing.

Sequence numbers are contiguous from `0` across the whole canonical stream.
Transaction ids are allocated strictly above the greatest id ever observed, so a
consumed id is never reused — across reopen.

Every non-genesis record in format version 1 lies inside a matching transaction;
only `VOLUME_GENESIS` is standalone.

## 5. Recovery

Recovery reconstructs, from `or.log/` alone:

- the validated committed prefix `L[0:R]`;
- the boundary list;
- the committed record count (hence the next sequence number);
- the greatest transaction id;
- the namespace index, by replaying `L[0:R]`.

The committed root is always a complete committed boundary. Bytes beyond `R`
(torn or well-framed-but-uncommitted) are **retained**, never truncated; on a
writable open the segments wholly at or beyond `R` are moved to
`or.log/quarantine/` and the stream continues in a fresh segment. A malformed
*committed* record is a hard error, never skipped. Nothing is silently repaired.

Recovery time is `Θ(retained history)`: without a usable checkpoint the whole
committed log is replayed. The Go port does **not** claim constant-time
recovery.

## 6. Capacity

Two spaces are kept strictly separate: the authoritative space (`or.log/*.seg`
plus `commit.watermark`) and the optional manifestation space. An authoritative
admission is a whole framed transaction, checked *before the first byte is
written*. Optional-plane failure is throttled/skipped and never fails an
authoritative commit.

The capacity guard only ever *refuses* growth; it never reclaims space (Law I),
so the backing will eventually fill. Exhaustion is an orderly, typed, read-only
transition, not a mid-commit `ENOSPC`.

## 7. Port deviations

None in the committed format. The Go port additionally:

- exposes `orfs` with the same subcommand vocabulary as the reference, rejecting
  not-yet-wired commands with their owning phase;
- consumes EKBLOOM as a pure-Go module rather than the Rust crate, and wires it
  as a **live** manifested read path (Plane C) with exact-identity certification
  and an authoritative fallback (`docs/MANIFESTATION.md`);
- probes the F2 (FUSE-over-`io_uring`) transport live but does not serve it:
  this host's kernel supports it, the linked Go FUSE library exposes none, so the
  port serves F0 and reports the exact reason it cannot serve F2
  (`docs/TRANSPORT.md`, `docs/LIMITATIONS.md` §6);
- uses no `unsafe` and no CGO.

## 8. Package map

| Concern | Package |
| --- | --- |
| envelope, registry, payloads | `internal/record` |
| segments and contiguity | `internal/segment` |
| the log | `internal/log` |
| the watermark | `internal/wm` |
| volume identity and init/open | `internal/volume` |
| namespace model and replay | `internal/index` |
| POSIX operations | `internal/namespace` |
| capacity guard | `internal/capacity` |
| verification | `internal/fsck` |
| operator report/probe | `internal/doctor` |
| FUSE projection | `internal/fusefs` |
| concealed expansion (Plane C) | `internal/manifest` |
| sealed dense-ordinal inode spine | `internal/inodex` |
| simulated power-loss device | `internal/powerloss` |
| compatibility fixtures | `internal/fixture` |
| typed errors | `internal/orfserr` |

See `docs/README.md` for the full documentation map.
