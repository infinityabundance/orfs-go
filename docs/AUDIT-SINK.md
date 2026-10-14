# OrFS as an audit-log sink — the production story

**Scope.** This document turns the WORM case study ([`WORM.md`](WORM.md)) into the
production story a storage engineer actually deploys: mounting OrFS at
`/var/log/audit` and driving it as an **append-only audit-log sink** (Loki /
Manifold style) for systemd-journald or a Kubernetes audit policy. It records the
workload, the immutability argument, the metrics and dashboard, the measured
numbers, how to mount it, and — explicitly — what it does **not** claim.

Receipt: [`../evidence/audit-sink.json`](../evidence/audit-sink.json) (schema
`orfs.audit-sink.v1`). Court: [`../courts/auditsink/`](../courts/auditsink/)
(driven by [`../scripts/container-auditsink.sh`](../scripts/container-auditsink.sh)).
Driver: `cmd/orfsaudit`. Dashboard:
[`../courts/auditsink/dashboard.json`](../courts/auditsink/dashboard.json).

## 1. The workload

A single sequential writer appends **structured JSON audit events** to a mounted
file and `fsync`s each one (the OrFS publish barrier):

```json
{"user":"root","action":"exec","resource":"/usr/bin/sudo","time":"2026-10-14T20:41:13.226233268Z"}
```

| Knob | Default |
| --- | --- |
| Events | 1500 |
| Record | 4096 B — a JSONL line padded to the size, newline-terminated |
| Corpus | seeded **80/20 hot/cold skew**: 8 hot users / 6 hot actions / 8 hot resources chosen 80% of the time, a long cold tail the other 20% |
| Appends | sequential `write(2)` + `fsync(2)` per event |
| Rate | configurable; the court runs at 150 events/s |
| Reader | one concurrent tail reader, following the file with **inotify** |
| Mount | FUSE (go-fuse, direct mount), transport `f0`, group commit enabled |

The corpus is deliberately **skewed, not uniform**: a handful of service
identities, actions, and paths dominate realistic audit traffic, and that is what
the tail reader and the index actually see. A uniform draw would be a
less-faithful, easier workload.

Records are fixed-length on purpose: one client write is exactly one FUSE write
and one authoritative transaction, so the appended-event count and the daemon's
transaction count agree. A non-aligned record size can be split across a 4 KiB
page boundary by the FUSE layer (measured in [`WORM.md`](WORM.md) §2).

Run it:

```sh
sh scripts/container-auditsink.sh
AUDIT_RECORDS=4096 AUDIT_RATE=0 sh scripts/container-auditsink.sh
```

The tail reader uses **real inotify** (`golang.org/x/sys/unix` `InotifyInit1` plus
`AddWatch` on the containing directory and on the file). When inotify could not
be armed — or delivered no event, e.g. because the file was written without a
kernel notification — the reader falls back to a bounded `poll(2)` and reports
`method=poll`. The receipt never claims inotify when it was not used.

## 2. The immutability argument

OrFS is built around **Law I — non-withdrawal**: committed authoritative history is
never overwritten, truncated, reused, or garbage-collected. An audit sink is the
workload that law is *designed* for, and the court proves it two ways.

**Mounted proof (the FUSE path).** The court `SIGKILL`s the mounted audit writer
mid-append, repeatedly (`AUDIT_KILLS`, default 8). Because each client append is
an acknowledged authoritative transaction, a kill can only lose *unacknowledged*
work:

- the **committed prefix is never lost or rewritten** — the court hashes the
  prefix before and after each kill and requires the byte-identical hash, and
  requires the file size to only grow (`prefix_mismatches = 0`,
  `truncations = 0`, `growth_monotonic = true`);
- after the storm, `orfs verify --deep` on the backing passes.

**Log-level proof (the existing court).** The court also invokes the repository's
real-process crash-consistency court, `TestCrashConsistencyCourt`
([`../internal/log/crash_court_test.go`](../internal/log/crash_court_test.go)): it
`SIGKILL`s the `cmd/orfscrashtest` log writer at a random point mid-append, reopens
read-write, and proves the committed prefix is byte-identical and `verify --deep`
passes **every iteration**. Its receipt is redirected with `ORFS_CRASH_EVIDENCE`
into the court's work directory so a run never clobbers the published
`evidence/crash.json`.

Uncommitted abandoned tails that this court finds are **moved** into
`or.log/quarantine/` — never truncated (that is the recovery contract, and it is
why `crash_court.quarantined_count` is non-zero and correct). The sink volume's
own `orfs_quarantine_total` and `orfs_breach_total` are **0**: a clean sink run
abandons no tail and is never refused growth.

## 3. Metrics and dashboard

`orfs metrics <backing>` renders the Prometheus text exposition for the volume
(see [`WORM.md`](WORM.md) §4); the court captures a live snapshot into the
receipt's `prometheus` string. There is **no HTTP scrape endpoint** — the scrape
model is a textfile collector or a cron job:

```sh
orfs metrics /var/lib/orfs/audit > /var/lib/node_exporter/textfile/orfs.prom
```

[`../courts/auditsink/dashboard.json`](../courts/auditsink/dashboard.json) is a
5-panel Grafana dashboard referencing only real metric names:

| Panel | Series |
| --- | --- |
| (a) bytes + amplification | `orfs_physical_bytes`, `orfs_manifested_bytes`, `orfs_logical_bytes`, `orfs_amplification_ratio` |
| (b) barrier latency | `orfs_fsync_latency_seconds{quantile="0.5"}`, `…{quantile="0.99"}` |
| (c) group commit | `orfs_group_commit_batch_size{stat="last"}`, `…{stat="avg"}` |
| (d) breach & quarantine rate | `rate(orfs_breach_total[5m])`, `rate(orfs_quarantine_total[5m])` |
| (e) verify failures | `orfs_verify_deep_failures` |

`orfs_fsync_latency_seconds` is a Prometheus **summary**, not a histogram, so
`histogram_quantile()` does not apply — panel (b) reads the summary's own
quantile series. The values are a bounded fixed-bucket estimate: a reported value
is the bucket's upper edge, so `1e-06` means "≤ 1 µs".

## 4. The measured numbers

One representative container run (fast NVMe backing, overlay upper layer). These
move with the backing; the *structure* is what to expect.

| Quantity | Value |
| --- | --- |
| Events / bytes appended | 1500 / 6,144,000 B (6.0 MiB) |
| Sustained throughput | 149.7 events/s · 613 KB/s (at a configured 150 events/s) |
| **End-to-end append + fsync** p50 / p99 / mean | **2.32 ms / 3.58 ms / 2.39 ms** — the **FUSE tax** |
| **Durability barrier** p50 / p99 (`orfs_fsync_latency_seconds`) | **≤ 1 µs / ≤ 2 µs** |
| **Tail lag** p50 / p99 (`inotify`) | **2.39 ms / 6.52 ms** |
| Tail reader | method `inotify`, 3002 inotify events, 1500/1500 events observed, 0 parse failures, growth monotonic |
| `orfs_logical_bytes` | 17,022,976 B (sink 6,144,000 + kill-proof appends) |
| `orfs_physical_bytes` (authoritative retained) | 17,770,939 B |
| `orfs_manifested_bytes` (`.or_bloom/`) | 17,023,630 B |
| `orfs_amplification_ratio` (R / logical) | **1.044×** |
| `orfs_group_commit_batch_size` last / avg | 1 / 1 |
| `orfs_quarantine_total` · `orfs_breach_total` · `orfs_verify_deep_failures` | 0 · 0 · 0 |
| Mounted immutability | 8 SIGKILLs, 0 prefix mismatches, 0 truncations, `verify --deep` passes |
| Log-level crash court | 120 iterations, 38 uncommitted tails moved to quarantine, 0 torn, 0 truncations / 0 prefix mismatches / 0 recover failures, 120/120 `verify --deep`, 120/120 continued commits |

**The headline honesty.** The barrier is cheap (≤ 1 µs) and the append is not
(≈ 2.3 ms): almost all of the difference is the FUSE round trip plus transaction
framing, the watermark write, and index replay — **the FUSE tax**, not the
barrier. The **tail lag is in the same regime**: a reader only observes a record
after the writer's transaction is durable and the FUSE read round trip completes,
so the ~2.4 ms tail lag is the FUSE publish/read path, not the barrier. Do not
read either the barrier or the tail lag as the append cost.

The amplification of **1.044×** is the framing/retention tax of an append-only
log with 4 KiB events (per-record `TX_BEGIN`/`TX_COMMIT` brackets, an
`INODE_SETATTR` per write, and the `commit.watermark`). Smaller records raise it
(512 B records measured ≈ 1.96× in [`WORM.md`](WORM.md)); it is a property of the
record size, not a defect.

## 5. How to mount OrFS as an audit sink

```sh
# 1. Create the volume once, on a dedicated backing.
orfs init /var/lib/orfs/audit --segment-size 64MiB

# 2. Mount it where the auditor writes. Group commit is the default acquisition
#    path for concurrent publishers; -o allow-other lets journald (not root) write.
orfs mount /var/lib/orfs/audit /var/log/audit --allow-other &

# 3. Point the producer at it. For systemd-journald, set the audit log path in
#    /etc/systemd/journald.conf or a drop-in; for Kubernetes, set the audit
#    policy's log path to /var/log/audit/audit.log.
#    Producers must APPEND, never rewrite in place.

# 4. Scrape it (textfile collector or cron). There is no HTTP endpoint.
orfs metrics /var/lib/orfs/audit > /var/lib/node_exporter/textfile/orfs.prom

# 5. Verify integrity read-only, any time.
orfs verify --deep /var/lib/orfs/audit
```

Readers tail the file: because it only grows, a reader that reads forward only
ever sees more bytes, never a changed one.

## 6. Honest limits

- **Append-only WORM, not a general-purpose filesystem.** No in-place rewrite,
  hole-punching, reclamation, or space-bounded growth. Committed bytes are
  retained forever (Law I); **retention is unbounded** — plan capacity with
  `orfs doctor --capacity`, and remember the sink cannot shrink.
- **The FUSE tax dominates.** The barrier (≤ 1 µs here) is not the append cost;
  the end-to-end append is ≈ 2.3 ms p50 and the tail lag ≈ 2.4 ms p50 under this
  mount. The broader FUSE tax is published in [`BENCH.md`](BENCH.md).
- **Single writer.** The court's sink is one sequential writer, so
  `orfs_group_commit_batch_size` is 1/1; the group committer is exercised but does
  not batch. Concurrent-writer batching is measured separately in
  [`../evidence/performance/groupcommit.json`](../evidence/performance/groupcommit.json).
- **Quantiles are bounded.** `orfs_fsync_latency_seconds` quantiles are a fixed
  bucket estimate (a reported value is the bucket's upper edge), and the summary
  is read from the persisted per-run snapshot, not a live Prometheus histogram.
- **One backing.** These numbers are one container run on one backing; a different
  backing (tmpfs, a slow spindle) moves the barrier and the append, but the shape
  — cheap barrier, expensive FUSE round trip, > 1 amplification — is what to
  expect.
- **`quarantine_total` is not zero in the log-level crash court.** That court
  *moves* uncommitted abandoned tails into `or.log/quarantine/`; the sink volume
  itself reports 0. The two are reported separately and are not the same number.
