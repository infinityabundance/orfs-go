# OrFS-GO Threat Model: the `or.log` AUTH Story

**Status:** implemented as described. This is the companion to
[`SECURITY.md`](SECURITY.md), which is the normative summary; this file states
the authentication story plainly.

## 1. The single most important statement

**There is no message-authentication code, no signature, and no encryption on the
authoritative log.** Nothing in `or.log` or its record envelope authenticates a
writer.

What exists is framing and integrity:

| Mechanism | Where | Detects |
| --- | --- | --- |
| CRC32C `header_check` | record header `[0, 56)` | accidental corruption / torn header |
| CRC32C `record_check` | footer over header + payload | accidental corruption / torn record |
| BLAKE3 `bracket_checksum` | `TX_COMMIT` payload | a transaction bracket that does not hash to its declared commit |

These detect **accidental corruption and casual tampering**. They are **not** a
MAC: an adversary with write access to the backing directory can rewrite history
and recompute every CRC32C and BLAKE3 digest. See [`SECURITY.md`](SECURITY.md) §1
and [`ON_DISK.md`](ON_DISK.md) §8.

## 2. Non-withdrawal is a law, not a cryptographic guarantee

Law I (non-withdrawal) is **enforced by the implementation**, not by
cryptography: the code never overwrites, truncates, reuses, or garbage-collects
committed bytes, and tests verify it. It constrains OrFS's own behaviour; it does
not defend against a privileged attacker. Someone who can write the backing store
can `dd` over `or.log`.

## 3. Adversaries

| Adversary | Capability | Answered by |
| --- | --- | --- |
| Hostile backing data | arbitrary bytes in `or.log/*.seg`; a forged but well-framed record; a tampered or truncated committed segment; an unsupported version | `internal/record`, `internal/segment`, `internal/log`, every payload decoder (integrity, not authentication) |
| Hostile FUSE client | arbitrary opcodes, node ids, names, offsets, sizes, xattr names, masks | `internal/fusefs`, `internal/namespace`, `internal/index` |
| Unprivileged local user | issues ordinary POSIX ops through a mount | the shared class rule, supplementary groups, sticky-bit checks |
| Tampered derived structures | rewrites `.or_bloom/` bodies or manifests | Plane C certification re-verifies against authority as it exists now |
| TOCTOU | races a name between check and use | names resolve in an in-memory index; no host path from user input is followed |
| Process crash / torn write | killed mid-write | recovery (`internal/log`) |

Explicitly **out of scope:** a compromised kernel; a privileged attacker with raw
device access; **confidentiality** (no encryption — anyone who can read the
backing directory can read the content); host-level DoS. See
[`SECURITY.md`](SECURITY.md) §2.

## 4. The mount is the trust boundary

Mounting requires `CAP_SYS_ADMIN` and `/dev/fuse`. There is no setuid helper and
no setuid mount path. A process that cannot mount cannot reach a mounted view.
Once mounted, requests are routed to the daemon, which runs with the mounting
user's privileges.

**A person who can mount is trusted with the backing store.** Mounting exposes
every object the daemon can read; there is no additional per-mount secret.
**Protect the backing directory more strictly than the mount** — the mount can be
shared with ordinary users (e.g. `--allow-other`), the backing store cannot.

## 5. TOCTOU

OrFS-GO does **not** claim to eliminate races against a writer with backing-store
access; its checks are not authentication. The analogous concern is addressed
structurally:

- the authoritative log is the source of truth, and a derived structure is
  trusted only when re-verified against authority **as it exists now**, not a
  cached claim;
- the FUSE projection never follows a host path from user input. It resolves
  names against the in-memory index, so there is no path-based
  time-of-check/time-of-use window on the backing directory.

## 6. Tampered derived structures (Plane C)

`.or_bloom/` is derived. A tampered manifested body **and** its manifest can
agree with each other and still be refused: certification binds the pair to the
bytes authority holds *now* (the `logical_blake3` authority-binding check, rule 5
of [`MANIFESTATION.md`](MANIFESTATION.md) §3). A miss or refusal is a fallback to
authoritative bytes, never a wrong answer. Deleting `.or_bloom/` changes nothing
recoverable.

## 7. EKBLOOM's keyed AUTH mode is not used on `or.log`

The EKBLOOM codec offers an **opt-in keyed AUTH mode**: `OpenAuthenticated` /
`VerifyAuthenticated` require a 32-byte key and validate a BLAKE3 keyed MAC (an
`AUTH` section) over the newest commit; a missing, stripped, or mismatching tag
is a typed error that never falls back to an unkeyed read.

**OrFS-GO does not use it on `or.log`.** The authoritative log is not an EKBLOOM
image and carries no key, so there is nothing to key the AUTH tag with. If a
deployment needs authentication of authoritative history, it must be layered
**above** OrFS — for example a signed or encrypted backing store, or an external
attestation of committed roots — and that layer, not OrFS, would own the key.

## 8. What this build does not implement

- Passwords, keys, or encryption: none, by design.
- Keyed authentication of the log: none, by design.
- FUSE passthrough / kernel-served snapshots: not wired; all reads are
  daemon-served with identical bytes.
- A compromised kernel or a privileged attacker is out of scope for every FUSE
  filesystem.

## See also

- [`SECURITY.md`](SECURITY.md) — the normative security summary.
- [`ON_DISK.md`](ON_DISK.md) §8 — the integrity model.
- [`MANIFESTATION.md`](MANIFESTATION.md) §3 — Plane C certification.
- [`LIMITATIONS.md`](LIMITATIONS.md) §7-§8 — deletion is not erasure; integrity is
  not authentication.
