# Security and Threat Model

**Status:** implemented as described. This mirrors the Rust reference's
`docs/SECURITY.md` and `docs/THREAT-MODEL.md`; where a claim is not enforced by
this build it is stated plainly.

## 1. The single most important statement

**The on-disk checks are integrity, not authentication.** The record envelope
uses CRC32C (`header_check`, `record_check`); the commit bracket uses a BLAKE3
content digest. There is **no MAC, no signature, and no encryption** anywhere in
the log or the envelope.

CRC32C and BLAKE3 detect accidental corruption and casual tampering; they are
**not** a message-authentication code. An adversary with write access to the
backing directory can rewrite history and recompute the digests.

Non-withdrawal (Law I) is therefore a **design law enforced by the
implementation**, not a cryptographic guarantee. It constrains OrFS's own
behaviour — the code never overwrites, truncates, reuses, or garbage-collects
committed bytes — and is verified by tests. It does not defend against a
privileged attacker.

## 2. Threat model

| Adversary | Capability | Answered by |
| --- | --- | --- |
| Hostile backing data | arbitrary bytes in `or.log/*.seg`; a forged but correctly-framed record; a tampered/truncated committed segment; an unsupported version | `internal/record`, `internal/segment`, `internal/log`, every payload decoder |
| Hostile FUSE client | arbitrary opcodes, node ids, names, offsets, sizes, xattr names, masks | `internal/fusefs`, `internal/namespace`, `internal/index` |
| Unprivileged local user | mounts (possibly with `default_permissions`), issues ordinary POSIX ops | the shared class rule, supplementary groups, sticky-bit checks |
| Hostile backing directory contents | plants files/symlinks in the backing dir | namespace ↔ backend separation |
| Process crash / torn write | killed mid-write | recovery (`internal/log`) |

Explicitly **out of scope:** a compromised kernel; a privileged attacker with raw
device access; power-loss/device-cache reordering (a separate simulated proof
obligation); **confidentiality** (no encryption — anyone who can read the backing
directory can read the content); host-level DoS.

## 3. The mount is the boundary

Mounting requires `CAP_SYS_ADMIN` and `/dev/fuse`. There is no setuid helper and
no setuid mount path. A process that cannot mount cannot reach a mounted view.
Once mounted, requests are routed to the daemon, which runs with the mounting
user's privileges.

**A person who can mount is trusted with the backing store.** Mounting exposes
every object the daemon can read; there is no additional per-mount secret.
**Protect the backing directory more strictly than the mount** — the mount can be
shared with ordinary users (e.g. `--allow-other`), the backing store cannot.

## 4. Within a mounted view

The single permission rule is the POSIX class rule (`namespace.HasPermission`),
mirroring the reference's `inode::has_permission_grouped`:

- root bypasses read/write; execute on a regular file needs at least one `x` bit,
  and is always allowed on a directory;
- otherwise the class is owner (uid match), else group (gid match **or any
  supplementary group**), else other, and the class bits must contain the mask.

Supplementary groups come from `/proc/<pid>/status` (`Groups:`). An unreadable
`/proc` leaves groups empty, which can over-deny but never over-grant.

- Every mutating handler checks permission before acting.
- Reading an xattr requires read permission (`0o4`); setting/removing requires
  write permission (`0o2`) with no special privilege for `security.*`,
  `trusted.*`, or `system.*`.
- The sticky bit is honoured on `unlink`/`rmdir` (owner of the entry, owner of
  the directory, or root).
- A directory hard-link returns `EPERM`.
- A read-only mount rejects every mutation with `EROFS` regardless of identity.

## 5. Namespace ↔ backend separation

The mount is a purely logical projection. `or.log/`, `volume.meta`,
`quarantine/`, and `.or_bloom/` are never reachable through the mount. This is
asserted by the mounted court (`scripts/mount-court.sh`).

## 6. Hardening by construction

- Field-by-field little-endian decode; **no struct-over-bytes cast**, no `unsafe`
  over untrusted length. (The Go port uses no `unsafe` at all.)
- `MAX_PAYLOAD_LEN` (64 MiB) is checked *before* any payload buffer is allocated.
- Checked arithmetic: overflow yields a controlled error.
- Names ≤ 255 bytes, no `/`, no NUL, not `.`/`..`. xattr name ≤ 255 (no NUL),
  value ≤ 64 KiB. Symlink target ≤ 4096.
- No allocation driven by an untrusted length before validation.

## 7. TOCTOU

OrFS-GO does **not** claim to eliminate races against a writer with
backing-store access; its checks are not authentication. The analogous concern is
addressed structurally:

- the authoritative log is the source of truth, and a derived structure is only
  ever trusted when re-verified against authority **as it exists now**, not a
  cached claim;
- the FUSE projection never follows a host path from user input — it resolves
  names against the in-memory index, so there is no path-based time-of-check/
  time-of-use window on the backing directory.

## 8. What this build does not (yet) implement

- **Passwords, keys, encryption:** none, by design.
- **Keyed authentication of the log:** none, by design.
- **FUSE passthrough / kernel-served snapshots:** not wired; all reads are
  daemon-served with identical bytes.
- A compromised kernel or a privileged attacker is out of scope for every FUSE
  filesystem.

## 9. Left open

- Logical deletion is **not** secure erasure: `unlink`/`rmdir`/truncate/rename
  change only the visible namespace; the bytes remain forever (Law I). OrFS is
  inappropriate for workflows requiring reliable physical deletion unless a
  separately specified retention and key-destruction policy is applied on top.
- Power-loss durability is tested with a simulated device, not real hardware
  (see `docs/LIMITATIONS.md`).
