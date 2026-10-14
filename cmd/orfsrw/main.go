// Command orfsrw is the Go-runtime court's workload driver.
//
// It drives a *mounted* OrFS volume with concurrent 4 KiB random readers and
// concurrent writers (interior overwrites, tail-block rewrites, and genuine
// appends past end-of-file), sampling every operation's latency continuously so
// the receipt carries application p50/p99/p99.9, throughput, and an error rate
// classified by errno.
//
// It is bounded and seeded: a duration (or a writer byte cap) always ends the
// run, so it can never spin forever. Latencies are exact: each worker keeps its
// own sample slice (no lock on the hot path) and the slices are merged and
// sorted at the end, so a p99.9 is a real observed sample, not a bucket edge.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// opClass summarises one operation class (reads, writes, or the combined view).
type opClass struct {
	Ops         uint64  `json:"ops"`
	Bytes       uint64  `json:"bytes"`
	Errors      uint64  `json:"errors"`
	IOPerSecond float64 `json:"io_per_second"`
	MiBPerSec   float64 `json:"mib_per_second"`
	MeanNs      int64   `json:"mean_ns"`
	P50Ns       int64   `json:"p50_ns"`
	P99Ns       int64   `json:"p99_ns"`
	P999Ns      int64   `json:"p99_9_ns"`
	P9999Ns     int64   `json:"p99_99_ns"`
	MaxNs       int64   `json:"max_ns"`
	Sampled     uint64  `json:"sampled"`
	Truncated   bool    `json:"sample_buffer_truncated"`
}

// receipt is the driver's result document (schema orfs.performance.rw.v1).
type receipt struct {
	Schema         string  `json:"schema"`
	Dir            string  `json:"dir"`
	Readers        int     `json:"readers"`
	Writers        int     `json:"writers"`
	BlockSize      int     `json:"block_size"`
	WriteMode      string  `json:"write_mode"`
	RequestSeconds float64 `json:"request_seconds"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	ReadFile       string  `json:"read_file"`
	ReadFileBytes  uint64  `json:"read_file_bytes"`
	WriteFile      string  `json:"write_file"`
	Read           opClass `json:"read"`
	Write          opClass `json:"write"`
	Combined       opClass `json:"combined"`
	AppendedBytes  uint64  `json:"appended_bytes"`
	WriterCapped   bool    `json:"writer_capped"`
	// FirstErrorSeconds is the time from workload start to the first failed
	// syscall (0 when none failed). It shows how early a daemon that died took
	// the mount with it.
	FirstErrorSeconds float64           `json:"first_error_seconds"`
	Errors            map[string]uint64 `json:"errors"`
	Note              string            `json:"note"`
}

// workerResult is one worker's private tally, written without a lock (each
// worker owns a distinct slice index) and aggregated single-threaded at the end.
type workerResult struct {
	ops     uint64
	bytes   uint64
	samples []int64
	errs    map[string]uint64
}

// maxSamplesPerWorker bounds the driver's own memory on a long run: past it the
// worker still performs I/O but stops recording latencies.
const maxSamplesPerWorker = 400_000

func main() {
	dir := flag.String("dir", "", "mounted directory to drive (required)")
	readers := flag.Int("readers", 64, "concurrent random readers")
	writers := flag.Int("writers", 8, "concurrent writers")
	seconds := flag.Float64("seconds", 5, "run duration in seconds")
	seed := flag.Int64("seed", 1, "base seed")
	bs := flag.Int("bs", 4096, "block size in bytes (4 KiB random I/O)")
	readFile := flag.String("read-file", "data.bin", "file (under -dir) to read randomly")
	writeFile := flag.String("write-file", "hot.bin", "file (under -dir) to overwrite/append")
	writeMode := flag.String("write-mode", "mixed", "mixed | overwrite | append")
	maxAppendMiB := flag.Int("max-append-mib", 64, "cap on bytes writers append past EOF, MiB")
	out := flag.String("out", "", "receipt path (default stdout)")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "orfsrw: -dir is required")
		os.Exit(2)
	}
	if *readers < 0 || *writers < 0 || (*readers == 0 && *writers == 0) {
		fmt.Fprintln(os.Stderr, "orfsrw: need at least one reader or writer")
		os.Exit(2)
	}
	if *bs <= 0 {
		fmt.Fprintln(os.Stderr, "orfsrw: -bs must be positive")
		os.Exit(2)
	}
	switch *writeMode {
	case "mixed", "overwrite", "append":
	default:
		fmt.Fprintf(os.Stderr, "orfsrw: unknown -write-mode %q\n", *writeMode)
		os.Exit(2)
	}

	rf, err := os.OpenFile(join(*dir, *readFile), os.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsrw: open read file: %v\n", err)
		os.Exit(1)
	}
	defer rf.Close()
	rinfo, err := rf.Stat()
	if err != nil || rinfo.Size() < int64(*bs) {
		fmt.Fprintf(os.Stderr, "orfsrw: read file %s must be at least %d bytes: %v\n", *readFile, *bs, err)
		os.Exit(1)
	}
	readSize := uint64(rinfo.Size())

	var wf *os.File
	var writeSize uint64
	if *writers > 0 {
		wf, err = os.OpenFile(join(*dir, *writeFile), os.O_RDWR, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orfsrw: open write file: %v\n", err)
			os.Exit(1)
		}
		defer wf.Close()
		winfo, err := wf.Stat()
		if err != nil || winfo.Size() < int64(*bs) {
			fmt.Fprintf(os.Stderr, "orfsrw: write file %s must be preallocated to at least %d bytes: %v\n", *writeFile, *bs, err)
			os.Exit(1)
		}
		writeSize = uint64(winfo.Size())
	}

	start := time.Now()
	deadline := start.Add(time.Duration(*seconds * float64(time.Second)))

	var appended atomic.Uint64
	var firstErr atomic.Int64
	noteError := func() {
		now := time.Now().UnixNano()
		firstErr.CompareAndSwap(0, now)
	}
	maxAppend := uint64(*maxAppendMiB) * 1024 * 1024
	appendEnd := atomic.Uint64{}
	appendEnd.Store(writeSize)
	var writerCapped atomic.Bool

	readResults := make([]workerResult, *readers)
	writeResults := make([]workerResult, *writers)

	var wg sync.WaitGroup
	for w := 0; w < *readers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + int64(w)))
			buf := make([]byte, *bs)
			maxOff := readSize - uint64(*bs)
			res := workerResult{errs: map[string]uint64{}, samples: make([]int64, 0, 4096)}
			for time.Now().Before(deadline) {
				off := rng.Uint64() % (maxOff + 1)
				t0 := time.Now()
				n, err := rf.ReadAt(buf, int64(off))
				d := time.Since(t0).Nanoseconds()
				res.ops++
				if len(res.samples) < maxSamplesPerWorker {
					res.samples = append(res.samples, d)
				}
				if err != nil {
					res.errs[errnoName(err)]++
					noteError()
				} else if n != *bs {
					res.errs["SHORT_READ"]++
					noteError()
				} else {
					res.bytes += uint64(n)
				}
			}
			readResults[w] = res
		}(w)
	}

	for w := 0; w < *writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + 977 + int64(w)))
			buf := make([]byte, *bs)
			for i := range buf {
				buf[i] = byte(w*31 + i)
			}
			maxOff := writeSize - uint64(*bs)
			res := workerResult{errs: map[string]uint64{}, samples: make([]int64, 0, 4096)}
			for time.Now().Before(deadline) {
				off, doAppend := pickOffset(rng, *writeMode, maxOff, writeSize, uint64(*bs), &appended, maxAppend, &appendEnd, &writerCapped)
				t0 := time.Now()
				n, err := wf.WriteAt(buf, int64(off))
				d := time.Since(t0).Nanoseconds()
				res.ops++
				if len(res.samples) < maxSamplesPerWorker {
					res.samples = append(res.samples, d)
				}
				if err != nil {
					res.errs[errnoName(err)]++
					noteError()
				} else if n != *bs {
					res.errs["SHORT_WRITE"]++
					noteError()
				} else {
					res.bytes += uint64(n)
					if doAppend {
						appended.Add(uint64(*bs))
					}
				}
			}
			writeResults[w] = res
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = *seconds
	}

	res := receipt{
		Schema:         "orfs.performance.rw.v1",
		Dir:            *dir,
		Readers:        *readers,
		Writers:        *writers,
		BlockSize:      *bs,
		WriteMode:      *writeMode,
		RequestSeconds: *seconds,
		ElapsedSeconds: elapsed,
		ReadFile:       *readFile,
		ReadFileBytes:  readSize,
		WriteFile:      *writeFile,
		AppendedBytes:  appended.Load(),
		WriterCapped:   writerCapped.Load(),
		Errors:         map[string]uint64{},
		Note: "Exact p50/p99/p99.9 from per-worker latency samples (lock-free on the hot path, merged and sorted at the end). " +
			"Reads are 4 KiB pread at uniform-random offsets; writes are 4 KiB pwrite: a bounded mix of interior overwrites, " +
			"tail-block rewrites, and genuine appends past EOF (capped by -max-append-mib, since OrFS retains appended history). " +
			"Latency is measured around the syscall only.",
	}

	res.Read = summarise(readResults, *bs, elapsed, &res.Errors)
	res.Write = summarise(writeResults, *bs, elapsed, &res.Errors)
	res.Combined = combine(res.Read, res.Write, elapsed)
	if fe := firstErr.Load(); fe > 0 && fe > start.UnixNano() {
		res.FirstErrorSeconds = float64(fe-start.UnixNano()) / 1e9
	}

	blob, _ := json.MarshalIndent(res, "", "  ")
	blob = append(blob, '\n')
	if *out == "" {
		os.Stdout.Write(blob)
	} else if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "orfsrw: write receipt: %v\n", err)
		os.Exit(1)
	}

	if res.Combined.Ops == 0 {
		fmt.Fprintln(os.Stderr, "orfsrw: no operations ran")
		os.Exit(1)
	}
}

// pickOffset chooses the next write offset. In mixed mode roughly one in eight
// writes appends past EOF (bounded), the rest recycle the tail block or an
// interior offset so the file never grows without bound.
func pickOffset(rng *rand.Rand, mode string, maxOff, writeSize, bs uint64, appended *atomic.Uint64, maxAppend uint64, appendEnd *atomic.Uint64, capped *atomic.Bool) (uint64, bool) {
	doAppend := mode == "append"
	if mode == "mixed" {
		doAppend = rng.Intn(8) == 0
	}
	if doAppend {
		if appended.Load() >= maxAppend {
			capped.Store(true)
			// Fall back to a tail-block rewrite rather than growing forever.
			return writeSize - bs, false
		}
		return appendEnd.Add(bs) - bs, true
	}
	// Half tail-block rewrites (append-like locality), half interior overwrites.
	if rng.Intn(2) == 0 {
		return writeSize - bs, false
	}
	return rng.Uint64() % (maxOff + 1), false
}

// summarise turns merged worker results plus an elapsed window into a class.
func summarise(results []workerResult, bs int, elapsed float64, allErrs *map[string]uint64) opClass {
	var ops, bytes, errs uint64
	var samples []int64
	for _, r := range results {
		ops += r.ops
		bytes += r.bytes
		for k, v := range r.errs {
			errs += v
			(*allErrs)[k] += v
		}
		samples = append(samples, r.samples...)
	}
	c := opClass{Ops: ops, Bytes: bytes, Errors: errs, Sampled: uint64(len(samples))}
	if elapsed > 0 {
		c.IOPerSecond = float64(ops) / elapsed
		c.MiBPerSec = float64(bytes) / elapsed / (1024 * 1024)
	}
	if len(samples) == 0 {
		return c
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var sum int64
	for _, s := range samples {
		sum += s
	}
	c.MeanNs = sum / int64(len(samples))
	c.P50Ns = quantile(samples, 0.50)
	c.P99Ns = quantile(samples, 0.99)
	c.P999Ns = quantile(samples, 0.999)
	c.P9999Ns = quantile(samples, 0.9999)
	c.MaxNs = samples[len(samples)-1]
	return c
}

// combine merges the read and write classes into one application view. The
// combined percentiles are op-count-weighted averages of the two class
// percentiles (a headline); the per-class numbers are exact.
func combine(r, w opClass, elapsed float64) opClass {
	c := opClass{
		Ops:    r.Ops + w.Ops,
		Bytes:  r.Bytes + w.Bytes,
		Errors: r.Errors + w.Errors,
	}
	if elapsed > 0 {
		c.IOPerSecond = float64(c.Ops) / elapsed
		c.MiBPerSec = float64(c.Bytes) / elapsed / (1024 * 1024)
	}
	c.P50Ns = weighted(r.P50Ns, w.P50Ns, r.Ops, w.Ops)
	c.P99Ns = weighted(r.P99Ns, w.P99Ns, r.Ops, w.Ops)
	c.P999Ns = weighted(r.P999Ns, w.P999Ns, r.Ops, w.Ops)
	c.P9999Ns = weighted(r.P9999Ns, w.P9999Ns, r.Ops, w.Ops)
	c.MeanNs = weighted(r.MeanNs, w.MeanNs, r.Ops, w.Ops)
	if r.MaxNs > w.MaxNs {
		c.MaxNs = r.MaxNs
	} else {
		c.MaxNs = w.MaxNs
	}
	return c
}

func weighted(a, b int64, na, nb uint64) int64 {
	total := na + nb
	if total == 0 {
		return 0
	}
	return (a*int64(na) + b*int64(nb)) / int64(total)
}

// quantile returns the exact q-quantile (nearest-rank) of a sorted slice.
func quantile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	if dir[len(dir)-1] == '/' {
		return dir + name
	}
	return dir + "/" + name
}

func errnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOENT:
			return "ENOENT"
		case syscall.EEXIST:
			return "EEXIST"
		case syscall.ENOSPC:
			return "ENOSPC"
		case syscall.EROFS:
			return "EROFS"
		case syscall.ENOTCONN:
			return "ENOTCONN"
		case syscall.EIO:
			return "EIO"
		case syscall.EINTR:
			return "EINTR"
		case syscall.EAGAIN:
			return "EAGAIN"
		case syscall.EPERM:
			return "EPERM"
		case syscall.EINVAL:
			return "EINVAL"
		case syscall.EBADF:
			return "EBADF"
		case syscall.ENFILE:
			return "ENFILE"
		case syscall.EMFILE:
			return "EMFILE"
		}
		return errno.Error()
	}
	return "OTHER:" + err.Error()
}
