# Audit-sink court

The **audit-sink** court turns the WORM case study into the production story a
storage engineer actually runs: mounting OrFS at `/var/log/audit` and driving it
as an append-only **audit-log sink** (Loki / Manifold style) for
systemd-journald or a Kubernetes audit policy.

It is one end-to-end court. It appends structured JSON audit events to a mounted
file, tails it with **real inotify**, proves immutability under repeated
`SIGKILL` of the mounted writer, captures the Prometheus exposition, and composes
the receipt [`../../evidence/audit-sink.json`](../../evidence/audit-sink.json)
(schema `orfs.audit-sink.v1`).

## Layout

| Path | What it is |
| --- | --- |
| `court.sh` | The court entry point — runs *inside* the court container as root. |
| `dashboard.json` | A 5-panel Grafana dashboard over the `orfs metrics` exposition. |
| `../../cmd/orfsaudit` | The driver/composer: `sink`, `tail`, `summary`. |
| `../../scripts/container-auditsink.sh` | The host wrapper (court container, capped). |
| `../../evidence/audit-sink.json` | The receipt (written by the court). |

## What the court does

1. **JSON audit-event writer.** `orfsaudit sink` appends fixed-length JSONL events
   `{"user":...,"action":...,"resource":...,"time":...}` drawn from a seeded
   **80/20 hot/cold corpus** (8 hot users / 6 hot actions / 8 hot resources, each
   chosen 80% of the time; a long cold tail the other 20%), one `write(2)` +
   `fsync(2)` per event, at a configurable rate. It reports records/s and bytes/s.
2. **Tail reader with growth observation.** A reader follows the file using
   `golang.org/x/sys/unix` **inotify** (`InotifyInit1` + `AddWatch` on the
   containing directory and on the file). It reports the **append-to-observation
   tail lag p50/p99** and that growth is monotonic. If inotify delivers no event
   it falls back to a bounded `poll(2)` and reports `method=poll` honestly. A
   plain `orfsaudit tail -file PATH -from-end` is the live `tail -f` view.
3. **Immutability proof.** Two independent proofs:
   - **Mounted:** `SIGKILL` the FUSE-client writer mid-append, `AUDIT_KILLS`
     times. Each append is an acknowledged authoritative transaction, so the
     committed prefix stays byte-identical and the file only grows; after the
     storm `orfs verify --deep` passes. Recorded under `mounted_immutability`.
   - **Log-level:** the existing in-Go court `TestCrashConsistencyCourt`
     (`internal/log`) is invoked with `ORFS_CRASH_EVIDENCE` redirected into the
     court's work directory, so it never clobbers the published
     `evidence/crash.json`. Recorded under `crash_court`.
4. **Prometheus scrape → Grafana.** The live sink's exposition is captured into
   the receipt's `prometheus` string; `dashboard.json` renders it.

## Run it

```sh
sh scripts/container-auditsink.sh
```

Knobs (all optional, forwarded by the wrapper):

| Env | Default | Meaning |
| --- | --- | --- |
| `AUDIT_RECORDS` | 1500 | events appended by the sink |
| `AUDIT_PAYLOAD` | 4096 | record bytes (padded JSONL line) |
| `AUDIT_RATE` | 150 | configured events/s |
| `AUDIT_SEED` | 1 | corpus seed |
| `AUDIT_KILLS` | 8 | mounted `SIGKILL`s of the client writer |
| `AUDIT_KILL_PAYLOAD` | 262144 | kill-writer record bytes (split across FUSE writes) |
| `AUDIT_CRASH_ITERS` | 120 | iterations of the in-Go crash court |
| `SEGMENT_SIZE` | 4194304 | OrFS segment size |

The receipt persists at `evidence/audit-sink.json`. A host without docker or
`/dev/fuse` reports `SKIPPED` rather than a false pass. The court is wired into
`scripts/container-verify.sh` (SKIPPED-aware).

## Honest notes

- **Append-only WORM, not a general-purpose filesystem.** No in-place rewrite,
  hole-punching, reclamation, or space-bounded growth.
- **The FUSE tax dominates the append; the barrier does not.** The daemon barrier
  (`orfs_fsync_latency_seconds`) is sub-microsecond, while the end-to-end append
  is a few milliseconds. The tail lag is in the same regime: a reader observes a
  record only after the writer's transaction is durable and the FUSE read round
  trip completes.
- **`quarantine_total` and `breach_total` in the receipt are the sink volume's
  own counters and are 0.** The log-level crash court *moves* uncommitted
  abandoned tails into quarantine (never truncates) — that is not data loss and
  is reported separately under `crash_court`.
