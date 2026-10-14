# Transport: F0, F1, F2

**Status:** the transport story is now measured, not promised. This build serves
**F0 only**, and it says exactly why the reference's F2 transport is not
reachable through it — with a live kernel negotiation as the evidence
(`evidence/performance/transport.json`).

The law this file serves: **a change to the transport plane must never change
filesystem semantics** (`ORFS-SPEC.md` §1). Transport is how FUSE requests reach
the daemon; it is not part of what the bytes mean.

## 1. What the names mean

| Name | Meaning | This build |
| --- | --- | --- |
| **F0** | The classic Linux FUSE transport: requests and replies are exchanged as framed messages over the `/dev/fuse` character device (`read(2)` / `writev(2)`), with synchronous backing I/O. | **Yes** — the default, and the only transport served. |
| **F1** | No distinct F1 is named by this port. The reference material reachable from this repository names F0 and F2 only; this port does not invent an F1. | none |
| **F2** | FUSE-over-`io_uring`: the same FUSE protocol exchange, but request delivery and reply completion go through `io_uring` (`IORING_OP_URING_CMD` with `FUSE_IO_URING_CMD_REGISTER` / `COMMIT_AND_FETCH`), typically with one queue per CPU. | **Not reachable** — see §3. |

## 2. What this build actually does (F0)

The mount is `hanwen/go-fuse` v2 (`internal/fusefs`). Concretely:

- **Transport.** `fuse/fusefd.go` reads requests with a blocking `syscall.Read`
  on the `/dev/fuse` fd and writes replies with `writev`. There is no transport
  abstraction and no other backend.
- **Serving concurrency.** go-fuse spawns up to
  `clamp(runtime.GOMAXPROCS, 2, 16)` reader goroutines
  (`fuse/server.go:216-220`, `minMaxReaders = 2`, `maxMaxReaders = 16`), each
  blocked in `read(2)`; the request is processed inline in the reader.
- **Knobs.** `orfs mount` exposes these honestly:
  - `--transport f0` (default): the multi-reader loop above.
  - `--transport f0-single`: go-fuse `SingleThreaded` request processing plus
    `GOMAXPROCS=1` — a single serving loop.
  - `--readers N`: sets `GOMAXPROCS=N`, which sets the reader count
    (`clamp(N, 2, 16)`). This is the only F0 throughput lever, and go-fuse caps
    it at 16.
  - `--transport fuse-uring`: requests F2. It is negotiated at mount time; when
    it is not available the exact reason is printed to stderr and the mount
    continues on `f0` — a loud fallback, never a silent downgrade (§3).

## 3. Why F2 is not available here (the honest part)

Two independent conditions must hold for F2, and the second fails:

1. **The kernel supports it — true on this host.** The running kernel is
   `7.2.9-2-cachyos`; it is built with `CONFIG_FUSE_IO_URING=y` and the feature
   is enabled at runtime (`/sys/module/fuse/parameters/enable_uring = Y`). The
   ABI is present in `/usr/include/linux/fuse.h`: `FUSE_OVER_IO_URING`
   (`1ULL << 41`), `struct fuse_uring_req_header`, and
   `FUSE_IO_URING_CMD_REGISTER` / `COMMIT_AND_FETCH`.

2. **The linked FUSE library can drive it — false.** `github.com/hanwen/go-fuse/v2`
   v2.12.0 exposes **no** FUSE-over-io_uring transport. It only names the
   capability bit: `CAP_OVER_IO_URING = (1 << 41)` at `fuse/types.go:325`, printed
   at `fuse/print.go:58`. Those two sites are its entire io_uring surface —
   nothing negotiates it and nothing serves through it. Its transport is the
   `/dev/fuse` read/write loop described in §2. `golang.org/x/sys/unix` v0.49.0
   is no help either: it has no io_uring support at all (the raw syscall numbers
   `SYS_IO_URING_SETUP/ENTER/REGISTER` exist in `unix/zsysnum_linux_amd64.go:347-349`,
   but no types, rings, or SQE helpers). A pure-Go F2 would mean hand-rolling
   `io_uring` *and* replacing go-fuse's whole transport — a reimplementation of
   the FUSE server, out of scope for this port.

**The live negotiation (the evidence).** `scripts/container-transport-probe.sh`
runs `cmd/fuseuringprobe`, which mounts a raw FUSE filesystem and performs the
real INIT handshake twice. Recorded result:

- the kernel **advertises** `FUSE_OVER_IO_URING` in the INIT it sends
  (`kernel_advertises_io_uring: true`);
- in the control round (INIT replied *without* the flag) the follow-up request
  is delivered on `/dev/fuse` (`control_request_on_dev_fuse: true`);
- in the test round (INIT replied *with* `FUSE_OVER_IO_URING`) the follow-up
  request **does not** arrive on `/dev/fuse`
  (`uring_reply_request_on_dev_fuse: false`).

That contrast is the empirical signature of the kernel switching the connection
to the io_uring transport — the kernel side of F2 is live here; the userspace
side is what this build lacks. The probe does **not** complete an io_uring
`REGISTER` (which would need `unsafe` pointer syscalls the port avoids), so it
records the negotiation, not a full F2 round trip.

Net: **F2 is not reachable from pure Go via go-fuse.** That is a finding, not a
deferral.

## 4. Measured: F0 vs the closest honest alternative

F2 being out of reach, the closest honest comparison is **inside F0**: the
default multi-reader loop versus the single serving loop. Court:
`scripts/transport-court.sh` (4K random reads, `iodepth=1`, `--direct-io`, 64 MiB
file, ~8 s/arm), receipt `evidence/performance/transport.json`
(schema `orfs.performance.transport.v1`).

| mode | numjobs | p50 | p95 | p99 | p99.9 | IOPS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `f0` (multi-reader) | 1 | 8,512 ns | 13,376 ns | 31,872 ns | 62,208 ns | 98,582 |
| `f0-single` | 1 | 8,256 ns | 11,712 ns | 29,312 ns | 50,944 ns | 103,612 |
| `f0` (multi-reader) | 64 | 148,480 ns | 354,304 ns | 1,712,128 ns | 9,633,792 ns | 271,855 |
| `f0-single` | 64 | 366,592 ns | 536,576 ns | 22,151,168 ns | 69,730,304 ns | 85,096 |

(One committed run; on a shared host these vary run to run — `REPRO.md` §6.)

Reading it honestly:

- **At one reader the two are within noise** (p50 ~8.3–8.5 µs, p99 ~29–32 µs).
  A single reader is a serialized workload; the serving loop's shape barely
  matters.
- **At 64 readers the multi-reader loop wins on everything**: ~3.2× the IOPS
  (272k vs 85k), ~2.5× better p50, and ~13× better p99 (1.71 ms vs 22.15 ms).
  The single loop serializes: its pending-request backlog reaches the full 64
  outstanding requests and stays there.

**Backpressure counters.** `evidence/performance/transport.json` carries the
kernel FUSE connection's signals from
`/sys/fs/fuse/connections/<id>/{max_background,congestion_threshold,waiting}`
(the court mounts `fusectl`, which a plain container lacks):

| mode | numjobs | max_background | congestion_threshold | waiting max | waiting mean |
| --- | ---: | ---: | ---: | ---: | ---: |
| `f0` | 1 | 12 | 9 | 1 | 0.80 |
| `f0` | 64 | 12 | 9 | 64 | 50.8 |
| `f0-single` | 1 | 12 | 9 | 1 | 0.71 |
| `f0-single` | 64 | 12 | 9 | 64 | 51.0 |

**SQ/CQ counters were not obtained**, and the receipt says so: they belong to
the io_uring transport, which is not in use. They are not invented.

## 5. Recommendation

- **Latency:** use `f0` (the default). It matches `f0-single` on single-reader
  p50/p99 and does not give anything up, while not collapsing under concurrency.
- **Throughput:** the only transport available is F0, and its only throughput
  lever is reader concurrency (`--readers N`, capped at 16 by go-fuse). The
  reference recommends **F2 for throughput**; **this build cannot offer F2**, so
  the honest guidance is: scale F0 readers up to the cap, and treat io_uring as
  unavailable until a Go FUSE library exposes it (or the port grows a raw
  transport).
- **Do not use `f0-single`** unless a single-threaded daemon is specifically
  wanted: at 64 readers it costs ~3× the throughput and ~13× the p99.

## 6. Reproduce

```sh
sh scripts/container-transport-probe.sh   # the FUSE INIT negotiation probe
sh scripts/container-transport.sh         # the measured court -> transport.json
```

Both are container-only (`/dev/fuse` + `CAP_SYS_ADMIN`); a host without either
reports `SKIPPED`, never a false pass. See [`REPRO.md`](REPRO.md) and
[`BENCH.md`](BENCH.md) §6b.

## 7. Non-claims

- No transport superiority is claimed beyond the measured F0 comparison above.
- No io_uring path exists in this build; `--transport fuse-uring` always reports
  the reason above and serves F0.
- The probe records the INIT negotiation and the plain-path delivery contrast; it
  does not complete an io_uring `REGISTER`, so "F2 reachable" is asserted of the
  *kernel*, and *denied* of the linked Go library — not demonstrated end to end.
