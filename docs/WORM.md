# OrFS as a WORM audit-log sink — case study

**Scope.** A single OrFS-GO volume, mounted inside the pinned container, driven as
an **append-only (WORM) audit-log sink** in the Loki / Manifold style: one
sequential writer appends framed log records and `fsync`s each (the OrFS publish
barrier), never rewriting or reclaiming them, while a reader tails the growing
file. This document records what that costs and what it does **not** claim.

Receipt: [`../evidence/worm-case-study.json`](../evidence/worm-case-study.json)
(schema `orfs.worm.case-study.v1`). Court:
[`../scripts/worm-court.sh`](../scripts/worm-court.sh) (driven by
[`../scripts/container-worm.sh`](../scripts/container-worm.sh)).

## 1. What "WORM" means here

OrFS is built around **Law I — non-withdrawal**: committed authoritative history
is never overwritten, truncated, reused, or garbage-collected. An append-only
audit-log sink is the workload that law is *designed* for:

- **Append, then fsync.** A record is appended at the current end offset and the
  barrier is taken before it is acknowledged. Records are never rewritten in
  place — an append is a new `DATA_EXTENT` at a fresh offset.
- **Readers tail.** The file only grows, so a reader that reads forward only ever
  sees more bytes; it never sees a byte change.
- **Nothing is reclaimed.** There is no unlink-and-reuse story: the retained
  history *is* the point.

This is **not** a general-purpose filesystem claim. A WORM sink gives up
in-place rewrite, hole-punching, reclamation, and space-bounded growth; the
authoritative footprint grows monotonically. See
[`LIMITATIONS.md`](LIMITATIONS.md).

## 2. The workload

| Knob | Default |
| --- | --- |
| Records | 2048 |
| Record size | 4096 B (4080 B payload + a 16 B frame header) |
| Appends | sequential `write(2)` + `fsync(2)` after each record |
| Reader | one concurrent tail reader, reading forward to EOF |
| Mount | FUSE (go-fuse, direct mount), transport `f0`, group commit enabled |

Record size is 4 KiB **page-aligned** on purpose: one client write is then exactly
one FUSE write and one authoritative transaction, so the appended-record count
and the daemon's transaction count agree. A non-aligned record size can be split
across a 4 KiB page boundary by the FUSE layer, which shows up as *more*
transactions than appends (measured; see §5).

Run it:

```sh
sh scripts/container-worm.sh                 # 2048 records × 4 KiB, 4 MiB segments
WORM_RECORDS=512 WORM_PAYLOAD=4080 sh scripts/container-worm.sh
```

The appended, mounted size equals the appended bytes exactly
(`appended_bytes == logical_bytes == mounted st_size`).

## 3. The measured numbers

One representative container run (fast NVMe backing, overlay upper layer):

| Quantity | Value |
| --- | --- |
| Appended records / bytes | 2048 / 8,388,608 B (8 MiB) |
| Wall time | 9.25 s |
| Sustained append throughput | 221 records/s · 0.91 MB/s |
| Append+fsync latency p50 / p99 / mean | 4.44 ms / 7.12 ms / 4.51 ms |
| Tail reader | 3675 reads, 905 growth observations, 8,388,608 B read, monotonic |
| `orfs_logical_bytes` | 8,388,608 B |
| `orfs_physical_bytes` (authoritative retained) | 9,356,051 B |
| `orfs_authoritative_bytes` (committed root R) | 9,356,015 B |
| `orfs_manifested_bytes` (`.or_bloom/`) | 8,389,259 B (1.0008× the logical content) |
| `orfs_amplification_ratio` (R / logical) | **1.115×** |
| `orfs_fsync_latency_seconds` p50 / p99 (daemon barrier) | ≤ 1 µs / ≤ 2 µs (mean 0.64 µs) |
| `orfs_group_commit_batch_size` last / avg | 1 / 1 |
| `orfs_breach_total` · `orfs_quarantine_total` · `orfs_verify_deep_failures` | 0 · 0 · 0 |
| `verify --deep` | every check ok (framing, transactions, watermark, replay) |

**The headline honesty:** the durability barrier itself is cheap here
(≈ 0.6 µs mean, because the backing's `fsync` is fast), while the **end-to-end
append** costs ≈ 4.4 ms p50. Almost all of that difference is the FUSE round trip
plus transaction framing, the watermark write, and index replay — **the FUSE
tax**, not the barrier. Do not read the 1 µs barrier as the sink's latency.

The amplification of **1.115×** is the framing/retention tax of an append-only
log with 4 KiB records: per-record `TX_BEGIN` / `TX_COMMIT` bracket frames, an
`INODE_SETATTR` (mtime) per write, and the `commit.watermark`. Smaller records
raise it (512 B records measured ≈ 1.96×); it is a property of the record size,
not a defect.

## 4. How to scrape `orfs metrics`

`orfs metrics <backing>` renders the Prometheus text exposition (version 0.0.4)
for a volume, or `--json` for `orfs.metrics.v1`:

```sh
orfs metrics /var/lib/orfs/demo            # Prometheus text to stdout
orfs metrics /var/lib/orfs/demo --json     # structured
orfs metrics /var/lib/orfs/demo --skip-verify
```

It opens the volume **read-only** and modifies not a single byte. It runs a
`verify --deep` pass to source `orfs_verify_deep_failures` (skip it with
`--skip-verify`).

**There is no HTTP scrape endpoint.** `orfs metrics` is a one-shot producer, so
the scrape model is a textfile collector (or a cron job writing a `.prom` file):

```sh
# node_exporter textfile collector, or any cron/Prometheus pushgateway feed
orfs metrics /var/lib/orfs/demo > /var/lib/node_exporter/textfile/orfs.prom
```

**Runtime counters and the snapshot sidecar.** Two classes of metric are
*sourced differently*:

- **On-disk facts** (recomputed on every invocation): `orfs_physical_bytes`,
  `orfs_authoritative_bytes`, `orfs_logical_bytes`, `orfs_manifested_bytes`,
  `orfs_amplification_ratio`, `orfs_quarantine_total`, `orfs_degraded`,
  `orfs_verify_deep_failures`.
- **Process-scoped runtime counters** (capacity refusals, the barrier histogram,
  and group-commit batch sizes): a **read-only inspection of another process
  cannot observe them**, so `orfs mount` persists a small derived snapshot to
  `metrics.json` beside `volume.meta` when it shuts down, and `orfs metrics`
  reads it. When no snapshot is present the runtime families are emitted as an
  **explicit 0** whose `# HELP` says so — never a fabricated value.

| Metric | Type | Source |
| --- | --- | --- |
| `orfs_physical_bytes` | gauge | `or.log/*.seg` + `commit.watermark` (excludes `.or_bloom/`) |
| `orfs_authoritative_bytes` | gauge | committed root `R` |
| `orfs_logical_bytes` | gauge | Σ live regular-file sizes from the replayed index |
| `orfs_manifested_bytes` | gauge | bytes under `.or_bloom/` |
| `orfs_amplification_ratio` | gauge | `R / logical` (0 when there is no logical content) |
| `orfs_degraded` | gauge | the read-only capacity latch (0/1) |
| `orfs_breach_total{reason}` | counter | capacity-guard refusals by reason (`backing_reserve`, `authoritative`, `hard_quota`) |
| `orfs_quarantine_total{unit}` | counter | retained abandoned-tail segments/bytes in `or.log/quarantine/` |
| `orfs_verify_deep_failures` | gauge | failed checks from the `verify --deep` pass |
| `orfs_fsync_latency_seconds{quantile}` | summary | the group committer / writer barrier histogram (`_sum`, `_count` too) |
| `orfs_group_commit_batch_size{stat}` | gauge | last/avg transactions per barrier |
| `orfs_metrics_snapshot_timestamp_seconds` | gauge | when the persisted runtime snapshot was captured (only when a snapshot exists) |

`orfs metrics` is also surfaced (the key values) in
`orfs doctor --capacity --backing <dir>`.

## 5. The Grafana dashboard

[`grafana/orfs-worm-dashboard.json`](grafana/orfs-worm-dashboard.json) is a
minimal dashboard referencing exactly the metric names above: physical /
manifested bytes, amplification, barrier p50/p99, batch size, breaches,
quarantined tails, verify failures, the degraded latch, and the snapshot
timestamp.

Import it into Grafana and pick a Prometheus data source (it takes a
`DS_PROMETHEUS` datasource variable). Point that Prometheus at the textfile feed
from §4.

## 6. Honest limits

- **Append-only WORM, not a general-purpose FS.** No in-place rewrite,
  hole-punching, reclamation, or space-bounded growth. Committed bytes are
  retained forever (Law I).
- **The FUSE tax dominates.** The barrier (measured here ≤ 1 µs at p50) is not
  the append cost; the end-to-end append is ≈ 4.4 ms p50 under this mount. The
  broader FUSE tax is published in [`BENCH.md`](BENCH.md).
- **Single writer.** The sink is one sequential writer, so
  `orfs_group_commit_batch_size` is 1/1 — the group committer is exercised but
  does not batch. Concurrent-writer batching is measured separately in
  [`../evidence/performance/groupcommit.json`](../evidence/performance/groupcommit.json).
- **Quantiles are bounded.** `orfs_fsync_latency_seconds` quantiles are a fixed
  bucket estimate: a reported value is the bucket's upper edge, so `1e-06` means
  "≤ 1 µs". The histogram is persisted, not a live Prometheus histogram.
- **Transaction count vs appends.** With page-aligned records they are equal
  (`records + 1` create). With a non-aligned record size the FUSE layer may split
  a write at a page boundary, so `transaction_count` can exceed the append count;
  the sink's `appended_bytes` and the mounted size remain exact.
- **One backing.** The numbers above are one container run on one backing; a
  different backing (tmpfs, a slow spindle) moves the barrier cost, but the
  structure — cheap barrier, expensive FUSE round trip, > 1 amplification — is
  the shape to expect.
- **`.or_bloom/` is derived.** The manifested body accelerates reads and is
  reported separately; deleting it changes nothing recoverable.
