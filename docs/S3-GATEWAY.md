# `orfs-s3` — a minimal WORM S3 gateway over OrFS

**Scope.** `cmd/orfs-s3` serves a deliberately small, WORM-only subset of the S3
API over one or more OrFS volumes. It does **not** mount FUSE: it opens the
volume read-write and appends through the library, so an object PUT is an
ordinary authoritative OrFS append (`internal/volume` → `internal/log`), and an
object GET goes through the manifested read path (`internal/manifest`, the
S3-FIFO admission policy in `internal/fcache`).

The gateway exists because the audit-log sink pays a FUSE round trip per append
(≈ 2.3 ms p50 end-to-end, see [`AUDIT-SINK.md`](AUDIT-SINK.md) §4). S3 clients
are how log sinks are actually written to (Loki, Fluent Bit, systemd), so the
gateway offers that interface without the FUSE tax.

Code: [`internal/s3gw`](../internal/s3gw) (the logic),
[`cmd/orfs-s3`](../cmd/orfs-s3) (a thin `main`).

## 1. What is implemented

| Operation | Notes |
| --- | --- |
| `PUT /bucket/key` | single-shot, `Content-Length` body. The bytes become an OrFS `DATA_EXTENT` append; the object is WORM-locked per §3. Returns the `ETag`. |
| `GET /bucket/key` | served through the manifested read path; supports a single `Range` (`bytes=a-b`, `bytes=a-`, `bytes=-n`). |
| `HEAD /bucket/key` | same headers, no body. |
| `DELETE /bucket/key` | `403` while the object is under retention or legal hold (§3). `204` for an unlocked object (the name is withdrawn; the bytes stay in the log) and `204` for a key that does not exist, as S3 does. |
| `GET /bucket?list-type=2` and `GET /bucket` (v1) | prefix, delimiter, `max-keys`, `continuation-token` / `start-after` / `marker`, `encoding-type=url`. |
| `PUT /bucket` | creates the bucket; idempotent when it exists (S3 answers `BucketAlreadyOwnedByYou`; this gateway treats it as success so an unconditional create in a client is harmless — a documented deviation). |
| `HEAD /bucket` | `200` / `404`. |
| `GET /bucket?location` | the configured region (empty for `us-east-1`, as S3 does). |
| `GET /bucket?object-lock` | `ObjectLockEnabled=Enabled` plus the default retention rule when the default is finite. |
| `GET /bucket?versioning` | an empty `VersioningConfiguration` (unversioned). |
| `GET /` | `ListBuckets`. |
| `DELETE /bucket` | refused with `403` (§3). |
| `GET /metrics` | Prometheus text (§5). |

Auth: **minimal AWS Signature V4, header-based** (§4). Unsigned requests are
refused.

## 2. The object → OrFS mapping

- A **bucket** is either a directory at the default volume's root (created on
  first use) or, when mapped with `-bucket NAME=PATH`, an entire OrFS volume
  whose root inode is the bucket root.
- An **object key** is a path: each `/`-separated component is a directory and
  the final component is a regular file. A key ending in `/` maps onto a
  directory — the marker `s3fs` creates — so clients that materialise
  directories keep working.
- Per-object S3 metadata (`ETag`, `Content-Type`, the lock state, the creation
  time) lives in **one extended attribute**, `user.orfs.s3.meta`, written as its
  own authoritative transaction. It therefore survives recovery like any other
  commit.

Documented consequences of a tree mapping:

- the keys `a` and `a/b` cannot coexist (one name cannot be both a file and a
  directory); the second write is refused;
- `/metrics` is a reserved path, so a bucket literally named `metrics` is not
  addressable;
- a `%2F` in the request path is decoded before splitting and is treated as a
  separator.

## 3. The WORM model

OrFS's **Law I** already means committed bytes are never overwritten,
truncated, reused, or garbage-collected. The gateway adds the **name-level**
guarantee: a lock state, recorded per object, enforced on write and delete.

| Lock | Source | Effect |
| --- | --- | --- |
| Retention (instant) | `x-amz-object-lock-retain-until-date` (must be in the future) | `PUT` over the object and `DELETE` are refused `403` until the instant passes |
| Retention (indefinite) | the bucket default (`-default-retention forever`, the **default**) | the object is never withdrawable |
| Legal hold | `x-amz-object-lock-legal-hold: ON` (or `-default-legal-hold`) | the object is never withdrawable |

- **Default to WORM.** `-default-retention` defaults to `forever`, so an object
  PUT without any lock header is retained indefinitely and its DELETE is
  refused. An operator who wants S3-style time-boxed retention sets
  `-default-retention 24h`; an operator who wants **retention-free** objects
  says so explicitly with `-default-retention 0`, and only then does `DELETE`
  withdraw the name.
- **Refusal, not concealment.** A refused `DELETE` is a `403 AccessDenied`
  naming the object and the reason, with `x-amz-worm-refusal: retention` or
  `legal-hold`. It is counted in `orfs_s3_worm_refusals_total` (§5).
- **An indefinite lock reports `x-amz-object-lock-retain-until-date:
  indefinite`.** S3 Object Lock has no "no expiry" concept, so the header value
  is a gateway extension rather than an RFC3339 date: a client that parses it as
  a date will see an unparsable value, and the 403 on `DELETE` is the guarantee
  that matters. A finite lock always reports a real RFC3339 date.
- A key ending in `/` (a directory) is never withdrawn either; `DELETE` on it is
  refused `403`.
- A **legal hold cannot be removed** through this surface (there is no
  `PUT ?legal-hold` sub-resource): with no IAM and one fixed policy, the honest
  answer is that the release path is out of scope.
- `x-amz-object-lock-mode` is accepted and recorded, but every lock is enforced
  compliance-style: there is no bypass.

**Unlocked deletion is not erasure.** With `-default-retention 0`, `DELETE`
unlinks the name — a `DENTRY_REMOVE` transaction. The data extents remain
committed in `or.log` forever (Law I). §7 of
[`LIMITATIONS.md`](LIMITATIONS.md) applies unchanged.

## 4. What the signature verifier does and does not do

`verifyRequest` checks: the `Authorization` header is present and
`AWS4-HMAC-SHA256`; the access key equals the single configured one; the
credential scope's region, `s3` service, and `aws4_request` terminator; the
credential date matches the request timestamp; the request time is within 15
minutes of the gateway clock; and the recomputed signature equals the presented
one (constant-time compare). For a PUT it additionally checks the body's
SHA-256 against the declared `x-amz-content-sha256`, and `Content-MD5` when the
client sends it.

It does **not** implement: query-string (presigned URL) authentication; session
credentials / `x-amz-security-token`; chunked
(`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) payload signatures; multiple credentials
(there is exactly one access-key/secret-key pair); or any authorization beyond
the signature — no IAM, buckets policies, or ACLs.

`internal/s3gw.SignRequest` is provided so a Go driver or test can sign a
request without the AWS SDK.

## 5. Metrics

`GET /metrics` renders the unchanged OrFS exposition from `internal/observe`
over `internal/metrics` for the **default** volume — the same families
`orfs metrics` prints, read from the **live** process rather than a persisted
snapshot:

`orfs_physical_bytes`, `orfs_authoritative_bytes`, `orfs_logical_bytes`,
`orfs_manifested_bytes`, `orfs_amplification_ratio`, `orfs_degraded`,
`orfs_breach_total{reason}`, `orfs_quarantine_total{unit}`,
`orfs_verify_deep_failures`, `orfs_fsync_latency_seconds{quantile}`,
`orfs_group_commit_batch_size{stat}`.

So the existing dashboards and the `rate(orfs_breach_total[5m])` /
`rate(orfs_quarantine_total[5m])` queries keep working unchanged. Two gateway
families are added:

| Metric | Meaning |
| --- | --- |
| `orfs_s3_requests_total{op}` | `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `ListObjects` |
| `orfs_s3_worm_refusals_total{reason}` | a write or withdraw against a locked object, by `retention` / `legal-hold` |

`orfs_verify_deep_failures` is reported as 0 with its HELP string saying no
verify pass was run — the gateway does not run `verify --deep` on the request
path. `/metrics` is unauthenticated by default (the Prometheus scrape model);
`-metrics-require-auth` requires SigV4 on it too.

## 6. Run it

```sh
# Create the volume once (or pass -init on the first start).
orfs init /var/lib/orfs/s3 --segment-size 64MiB

export ORFS_S3_ACCESS_KEY=...
export ORFS_S3_SECRET_KEY=...
orfs-s3 -addr :9000 -volume /var/lib/orfs/s3 -default-retention forever

# Standard clients, path-style addressing.
aws --endpoint-url http://127.0.0.1:9000 s3 mb s3://audit
aws --endpoint-url http://127.0.0.1:9000 s3api put-object \
    --bucket audit --key 2026/host.log --body ./record.json \
    --object-lock-retain-until-date 2027-01-01T00:00:00Z

# s3fs: use_path_request_style, because only path-style is supported.
s3fs audit /mnt/audit -o use_path_request_style,url=http://127.0.0.1:9000
```

Flags: `-addr`, `-volume`, `-init`, `-segment-size`, `-bucket NAME=PATH` (repeatable),
`-access-key`/`-secret-key` (or `ORFS_S3_ACCESS_KEY`/`ORFS_S3_SECRET_KEY`),
`-region`, `-default-retention`, `-default-legal-hold`, `-manifest`,
`-manifest-cache`, `-manifest-policy`, `-checkpoint-every`, `-group-commit`,
`-metrics-require-auth`, `-max-object-bytes`, `-uid`, `-gid`.

## 7. Honest limits

- **Not an S3 clone.** No multipart (a PUT body is bounded by
  `-max-object-bytes`, default 64 MiB, and is read into memory), no versioning,
  no lifecycle, no SSE, no ACLs or bucket policies, no presigned URLs, no
  multi-object delete, no object tagging, no `x-amz-meta-*` user metadata.
- **Path-style addressing only.** Virtual-hosted-style (`bucket.host`) is not
  recognised; configure clients for path style.
- **One credential, no IAM.** Every request must be signed by the single
  configured key; authorization is all-or-nothing.
- **`ETag` is the single-part MD5** of the object, quoted, and is not verified
  against the backing by clients in any special way. There is no multipart ETag
  shape (`-N` suffix) because there is no multipart.
- **LIST is `O(all keys in the bucket)`.** It walks the immutable read view and
  sorts; it is not an index lookup. The namespace read view's directory lookup
  is a tree walk, so a large volume makes LIST slow — measure before relying on
  it for a hot path.
- **The manifested read path is root-bound.** A manifested body is served only
  for an exact `(inode, version, committed root)`; a subsequent write anywhere
  on the volume moves the root and the body becomes a miss (a fallback to the
  authoritative extent read, which is a direct byte read, not a scan). Under a
  continuous write stream most GETs therefore fall back to authority even with
  `-manifest` on, and each PUT pays one EKBLOOM encode plus two files under
  `.or_bloom/`. Set `-manifest=false` for a pure append sink that never re-reads.
- **Group commit is enabled but does not batch here.** `-group-commit` sets
  `ORFS_GROUP_COMMIT=1` (the log's opt-in group committer). Because every write
  goes through `namespace.Namespace`, which holds a write-side mutex across its
  `Publish`, concurrent `Publish` calls never overlap and the committer's batch
  is 1. The measured gateway win, if any, is the removed FUSE round trip, not a
  shared barrier — `orfs_group_commit_batch_size` on `/metrics` will show it.
- **The gateway does not verify the volume.** It serves whatever the committed
  log says; run `orfs verify --deep` out of band.
- **No durability claim beyond OrFS's own.** A PUT is acknowledged after its
  transaction commits and the log's barrier returns; that is the same guarantee
  the FUSE path gives.
