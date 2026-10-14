# Concealed Expansion (Manifested Read Path)

**Status:** implemented as a live read path. This is Plane C: a manifested body
never alters authoritative history, and a fault or miss in it is a fallback, not
an error.

## 1. The law

**Law II — Concealed expansion.** Additional operational structures are
deliberately manifested *behind* the ordinary logical namespace, spending
storage, memory, and construction work to accelerate reads. The manifested body
may be larger than the logical contents and is not exempt from physical
accounting.

Authority establishes truth; a concealed structure only accelerates observation
of it.

## 2. Identity: what a body is bound to

A manifested body is an EKBLOOM projection of a file's **immutable** bytes under
a root profile, written under `.or_bloom/`:

```text
.or_bloom/
├── ekbloom/   <inode>-v<version>-r<root>.ekb
└── manifests/ <inode>-v<version>-r<root>.json
```

The manifest binds the body to an exact identity:

| Field | Meaning |
| --- | --- |
| `inode` | inode id |
| `inode_version` | the inode's version at publication |
| `root` | the committed root at publication |
| `logical_len` | the file's logical length |
| `physical_bytes` | the manifested body's size |
| `body_blake3` | BLAKE3 of the body file |
| `logical_blake3` | BLAKE3 of the file's logical content |
| `ekbloom_version` | the codec identity (`ekbloom-go`) |

A read is served from the body only for an **exact** `(inode, version, root)`
match. Because extents are immutable (Law I), a given `(inode, version)` names
one byte-string forever; a later write bumps the version and the old body is
simply stale — never wrong.

## 3. Certification

`manifest.Cache` trusts a body only when all of these hold:

1. **Identity** — `(inode, version, root)` matches the request.
2. **Codec identity** — `ekbloom_version` matches this build.
3. **Self-digest** — `body_blake3` equals the body's recomputed BLAKE3.
4. **Logical length** — the opened reader's `LogicalLen()` equals `logical_len`.
5. **Authority binding** — when an authoritative oracle is installed, the
   file's logical bytes read from `or.log` **now** hash to `logical_blake3`.

Rule 5 is what defeats a tampered *pair*: an attacker who rewrites the body
**and** its manifest to agree with each other still disagrees with authority and
is refused. Certification happens once per identity; thereafter reads are served
directly from the opened codec.

Misses are **negative-cached**, so a volume with no manifested bodies pays no
per-read filesystem lookup.

## 4. Plane separation

`namespace.Namespace` reaches Plane C through a one-method interface
(`ManifestSource`) and never imports the codec. `read(2)` consults the seam and,
on any miss or refusal, calls `ReadAuthoritative`, which never consults Plane C.
A manifested byte is therefore *never* the only source of an answer: the
authoritative extent read always exists and always yields the same bytes.

## 5. Profiles

| Profile | State |
| --- | --- |
| `root` (full history) | implemented — the default; amplifies ~1.00× |
| `sparse`/`dense`/`bloom`/`regional`/`path`/`max-speed` | the EKBLOOM codec supports them; OrFS-GO does not yet select one by measured worth |

Choosing a profile by expected access pattern (spending more space for more
speed) is the follow-up. No superiority is claimed for any profile not measured.

## 6. Using it

```sh
orfs manifest <backing> --inode <id>          # publish a body for one file
orfs manifest <backing> --inode <id> --json   # the manifest as JSON
```

A mounted volume installs the seam automatically; any read of a manifested inode
is served from Plane C with an authoritative fallback.

## 7. Measured results (this host, unoptimised test build, warm cache)

Court: `internal/manifest/planner_test.go`; mounted round trip:
`scripts/mount-court.sh`.

| Property | Result |
| --- | --- |
| Correctness | 200 random ranges byte-identical to authority |
| Selection | reads served from the body; zero misses/refusals on a certified body |
| Stale identity | falls back to authority (a miss, not a served body) |
| Amplification | 1 MiB logical → root-profile body ~1.00× |
| Latency (1 MiB file, 4 KiB random reads) | manifested **p50 ~2× better** (~240 ns vs ~480 ns); **p99 within noise, not claimed** |
| Mounted round trip | 270 KB file manifested, remounted, read back byte-exact (`md5`), `verify --deep` clean |

## 8. Transport and lifecycle

`orfs mount` installs the seam with the authoritative oracle wired in. It serves
the F0 (`/dev/fuse`) transport; the transport story — including why the reference's
F2 is not reachable here — is in [`TRANSPORT.md`](TRANSPORT.md).
`orfs verify --deep` validates the authoritative log regardless of Plane C; the
manifested plane is derived and deleting `.or_bloom/` changes nothing
recoverable (Law I).

> A manifested failure never propagates to an authoritative commit: the two
> error planes are kept strictly separate (`docs/ORFS-SPEC.md` §6).
