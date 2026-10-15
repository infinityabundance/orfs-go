# The `orfs-s3` WORM gateway court

This court measures the S3 gateway (`cmd/orfs-s3`) against the FUSE-mounted
append path and against a stock S3 server on the same machine, the same device,
and the same container caps. It is **not** implemented as a script yet: this
document is the design the later run must follow, so the numbers it produces
mean what they say.

Code under test: [`internal/s3gw`](../../internal/s3gw) and
[`cmd/orfs-s3`](../../cmd/orfs-s3); the protocol surface and its honest limits
are in [`docs/S3-GATEWAY.md`](../../docs/S3-GATEWAY.md).

## 1. The question

The audit-log sink pays a FUSE round trip per append (≈ 2.3 ms p50 end-to-end;
[`docs/AUDIT-SINK.md`](../../docs/AUDIT-SINK.md) §4). The gateway appends through
the same library (`internal/volume` → `internal/log`) with no FUSE in the path.
The court answers: **what does an S3 PUT and an S3 GET cost against OrFS, and
how does that compare to the mounted append and to a stock S3 server on the same
host?**

## 2. Arms (strictly sequential, one at a time)

| Arm | What runs | Why |
| --- | --- | --- |
| `A: s3-gateway` | `cmd/orfs-s3` over a fresh OrFS volume | the thing under test |
| `B: fuse-append` | the existing mounted append (`cmd/orfsaudit` or `cmd/orfsrw`) on the same backing | the baseline the gateway is meant to replace; reuses an existing court rather than re-measuring it |
| `C: minio` | a pinned `minio` image/server on the same device | an upper-bound reference point for the HTTP+S3 layer, **not** a like-for-like durability comparison (§6) |

Arms run **sequentially** and each arm's server/volume is released before the
next starts: the shared 2 GiB cap (`scripts/lib.sh`) cannot hold two servers and
a volume cache at once (an earlier court OOM-killed a daemon mid-arm).

## 3. Drivers

Two, in order of preference:

1. **`warp`** (MinIO's S3 load generator). It reports ops/s, bytes/s and
   `p50/p90/p99` per operation, uses unique keys, and is the shape the
   comparison needs. The pinned image may need `warp` added by the integrator
   (the court image is not owned by this workstream).
2. **`aws-cli` + `xargs -P`** as the fallback for a compatibility smoke test
   and a coarse concurrency sweep. `aws s3api put-object` / `get-object` with a
   pre-generated body file, invoked at 1 and 64 concurrent processes.

If neither is available the court reports `SKIPPED` (see §7) rather than
substituting a number. A third option — a Go driver in this directory using
`internal/s3gw.SignRequest` so no extra binary is needed — is explicitly
*welcome* but is not required.

**A driver that reuses keys will be refused by the gateway.** With the shipped
default (`-default-retention forever`) every object is WORM-locked, so a second
PUT to the same key is a legitimate `403`. Benchmarks therefore run the gateway
with `-default-retention 0` (retention-free objects; §3 of the gateway doc), and
the WORM behaviour is exercised by its own correctness arm (§5) and by the
package tests. A driver must still use unique keys, or the arm's numbers are
measuring refusals.

## 4. The matrix

| Axis | Values |
| --- | --- |
| Concurrency | **1** and **64** |
| Object size | **4 KiB** (the audit-record size) and **1 MiB** |
| Operation | `PUT` only; `GET` only (over pre-PUT objects); mixed |
| Duration | a bounded window per point (e.g. 10 s), never a fixed count |

Reported per point: **ops/s**, **MiB/s**, and **p50 / p99 / p99.9** latency, plus
the error rate. Excluded from the window: bucket creation and the pre-PUT of the
objects a `GET` arm reads.

The gateway is run twice per size, because the manifested read path is a
deliberate space/speed trade:

- `-manifest=false` — the pure append sink;
- `-manifest=true` (the default) — a manifested body is published per PUT.

The GET arms make the second case's read path visible; the PUT arms make its
cost visible. Report both; do not average them.

## 5. What else is recorded (so the receipt is honest)

Scraped from `GET /metrics` before and after every arm, from the **live**
process:

| Metric | Why it is recorded |
| --- | --- |
| `orfs_physical_bytes`, `orfs_authoritative_bytes`, `orfs_logical_bytes`, `orfs_manifested_bytes`, `orfs_amplification_ratio` | the retention/framing tax of what the arm actually wrote |
| `orfs_group_commit_batch_size{stat="last"\|"avg"}` | whether the log's group committer batched. The gateway's writes go through `namespace.Namespace`, whose write-side mutex spans each `Publish`, so the expected value is **1** — the court must report what it measures, including that |
| `orfs_breach_total`, `orfs_quarantine_total`, `orfs_degraded` | a clean run is `0`; a capacity refusal is a real result, not a retry |
| `orfs_s3_requests_total{op}`, `orfs_s3_worm_refusals_total{reason}` | the gateway's own accounting; refusals must be 0 in the throughput arms |

A **WORM correctness arm** runs once and is recorded verbatim:

1. PUT an object with `--object-lock-retain-until-date <future>` → expect `200`;
2. `DELETE` it → expect **`403`** with `x-amz-worm-refusal: retention`;
3. `GET` it → expect `200` and the exact bytes;
4. PUT it again → expect **`403`**;
5. PUT an object with `--object-lock-legal-hold ON`, `DELETE` → expect **`403`**
   with `x-amz-worm-refusal: legal-hold`;
6. `orfs verify --deep` on the volume → expect clean.

The package tests (`internal/s3gw`) already cover 1–5 plus LIST, SigV4
accept/reject, Range, directory markers, bucket ops and the metrics endpoint;
this arm exists to prove the same over a real client.

## 6. Comparability rules

Follow the same rules as
[`courts/oss-compare/README.md`](../oss-compare/README.md):

1. **Same machine, same device, same caps.** One container enforces the caps for
   every arm; record them.
2. **Name every asymmetry.** In particular:
   - **Durability.** The gateway acknowledges a PUT after the OrFS transaction
     commits and the log's barrier returns. MinIO's default write policy is not
     the same durability point; record MinIO's configuration and mark the
     append-durability axis `not_comparable` unless it is pinned to `fsync`-on-
     commit.
   - **The read path.** OrFS GETs are served from the manifested body when it
     certifies, else from the authoritative extent, and the response is
     built per request; MinIO has its own cache/index behaviour. Name it.
   - **The clients.** `warp` and `aws-cli` are different clients; run the same
     driver against every arm, or record which arm used which.
3. **No ranking.** The receipt is data. It carries no winner.
4. **No silent holes.** An arm that did not run is a named `SKIPPED` with the
   reason, and a partial receipt is a FAIL.

## 7. Caps, and what is `SKIPPED` rather than faked

- Everything runs inside the pinned, resource-capped container
  (`sh scripts/run.sh …`, caps in [`scripts/lib.sh`](../../scripts/lib.sh) —
  2 GiB / 4 cpus / pids 1024). The host is never used directly.
- The MinIO arm is `SKIPPED` when its image cannot be pulled; the FUSE arm is
  `SKIPPED` when `/dev/fuse` is unavailable. Neither is ever replaced by a
  number measured some other way.
- The driver arm is `SKIPPED` when neither `warp` nor `aws-cli` is present in the
  image: a `SKIPPED` with the reason is a success.
- One container runs at a time. Do not run this court concurrently with another
  measurement on the same machine — both would be contaminated.

## 8. The receipt

`evidence/performance/s3-gateway.json`, schema `orfs.s3-gateway.v1`:

```jsonc
{
  "schema": "orfs.s3-gateway.v1",
  "generated_at_unix": 0,
  "caps": { "memory": "2g", "cpus": 4, "pids": 1024 },
  "arms": [
    { "name": "A:s3-gateway", "status": "ok|skipped", "reason": "",
      "config": { "manifest": true, "default_retention": "0",
                  "segment_size": 67108864, "group_commit": true },
      "points": [
        { "op": "put|get|mixed", "concurrency": 1, "object_bytes": 4096,
          "duration_seconds": 10, "ops_per_second": 0, "bytes_per_second": 0,
          "p50_seconds": 0, "p99_seconds": 0, "p999_seconds": 0,
          "errors": 0 }
      ],
      "metrics_before": {}, "metrics_after": {} }
  ],
  "worm_correctness": { "retain_put": 200, "retain_delete": 403,
                        "legal_hold_delete": 403, "verify_deep": "ok" },
  "not_comparable": ["append durability", "read-path caching shape"],
  "notes": []
}
```

Every number in the receipt is one the driver or `/metrics` measured. A missing
point is a `SKIPPED` entry with its reason, never a zero.
