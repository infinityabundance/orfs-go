// Command orfscompare is the shared, deterministic workload driver for the
// open-source head-to-head court (courts/oss-compare). It drives a *mounted*
// directory — one mounted filesystem at a time, on the same machine, under the
// same container caps — with the two workloads a storage engineer compares:
//
//   - append: every writer owns its own (preallocated) file and appends fixed
//     4 KiB records with write(2)+fsync(2) per record — one durability barrier
//     per record requested of the mount — at 1 and 64 concurrent writers, for a
//     bounded time. This is the durable 4 KiB append figure. It is *not* the
//     zero-payload batched-publish number in evidence/performance/groupcommit.json.
//     The driver issues the same syscalls on every system, but a system is only
//     equal-durability if its fsync is a durable barrier; that is a per-system
//     property the court records, not one this driver can assert.
//   - read: random reads at 4 KiB, 64 KiB and 1 MiB block sizes, reporting
//     IOPS/throughput and exact p50/p95/p99/p99.9.
//
// It is bounded and seeded: a duration always ends the run (and a per-writer
// byte cap bounds appended growth, since OrFS retains appended history), so it
// can never spin forever. Latencies are exact: each worker keeps its own sample
// slice (no lock on the hot path) and the slices are merged and sorted at the
// end, so a p99.9 is a real observed sample, not a bucket edge. Errors are
// classified by errno class.
//
// Usage:
//
//	orfscompare prepare -dir <mnt> [-writers N] [-prealloc-bytes B] [-read-file F] [-read-bytes B]
//	orfscompare run -dir <mnt> -system <label> -workload append|read [flags]
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/infinityabundance/orfs-go/internal/osscompare"
)

// maxSamplesPerWorker bounds the driver's own memory on a long run: past it the
// worker still performs I/O but stops recording latencies.
const maxSamplesPerWorker = 400_000

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "prepare":
		os.Exit(runPrepare(os.Args[2:]))
	case "run":
		os.Exit(runRun(os.Args[2:]))
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "orfscompare: unknown command %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `orfscompare — shared deterministic workload driver (courts/oss-compare)

usage:
  orfscompare prepare -dir <mnt> [-writers N] [-prealloc-bytes B] [-read-file F] [-read-bytes B]
  orfscompare run -dir <mnt> -system <label> -workload append|read [flags]

run flags:
  -writers N              append arm: concurrent writers, each its own file
  -readers N              read arm: concurrent readers
  -append-prefix P         writer file prefix (append-<i>.bin)
  -read-file F             read arm file
  -bs B                    read arm block size (4096 | 65536 | 1048576)
  -record-bytes B          append record size (default 4096)
  -seconds S               bounded run duration
  -max-bytes-per-writer B  bound each writer's appended bytes
  -seed N                  deterministic seed
  -out FILE                receipt path (default stdout)
`)
}

// prepare creates each writer's dedicated file (preallocated to -prealloc-bytes)
// and, if -read-bytes > 0, a seeded read file. Doing this before the measured
// run keeps file creation out of the append+fsync number and gives every writer
// its own file.
func runPrepare(args []string) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	dir := fs.String("dir", "", "mounted directory (required)")
	writers := fs.Int("writers", 64, "writer files to create")
	prefix := fs.String("append-prefix", "append", "writer file prefix")
	prealloc := fs.Int64("prealloc-bytes", 0, "bytes to preallocate in each writer file")
	readFile := fs.String("read-file", "data.bin", "read file name")
	readBytes := fs.Int64("read-bytes", 64<<20, "bytes of read file to seed (0 = skip)")
	chunk := fs.Int("chunk-bytes", 1<<20, "seed write chunk size")
	seed := fs.Int64("seed", 1, "seed for the read file contents")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "orfscompare prepare: -dir is required")
		return 2
	}
	if *writers < 0 || *readBytes < 0 || *prealloc < 0 || *chunk <= 0 {
		fmt.Fprintln(os.Stderr, "orfscompare prepare: writers/read-bytes/prealloc must be >= 0 and chunk-bytes > 0")
		return 2
	}

	created := 0
	for i := 0; i < *writers; i++ {
		path := join(*dir, fmt.Sprintf("%s-%d.bin", *prefix, i))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orfscompare prepare: create %s: %v\n", path, err)
			return 1
		}
		if *prealloc > 0 {
			if err := f.Truncate(*prealloc); err != nil {
				f.Close()
				fmt.Fprintf(os.Stderr, "orfscompare prepare: preallocate %s: %v\n", path, err)
				return 1
			}
		}
		if err := f.Sync(); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "orfscompare prepare: sync %s: %v\n", path, err)
			return 1
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "orfscompare prepare: close %s: %v\n", path, err)
			return 1
		}
		created++
	}

	seeded := int64(0)
	if *readBytes > 0 {
		if err := seedReadFile(join(*dir, *readFile), *readBytes, int64(*chunk), *seed); err != nil {
			fmt.Fprintf(os.Stderr, "orfscompare prepare: seed read file: %v\n", err)
			return 1
		}
		seeded = *readBytes
	}

	out := map[string]any{
		"schema":      "orfs.oss-compare.prepare.v1",
		"dir":         *dir,
		"writers":     created,
		"prealloc":    *prealloc,
		"read_file":   *readFile,
		"read_bytes":  seeded,
		"chunk_bytes": *chunk,
		"seed":        *seed,
	}
	blob, _ := json.MarshalIndent(out, "", "  ")
	blob = append(blob, '\n')
	os.Stdout.Write(blob)
	return 0
}

// seedReadFile writes readBytes of deterministic, seeded content and fsyncs once.
func seedReadFile(path string, readBytes, chunk int64, seed int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, chunk)
	rng := rand.New(rand.NewSource(seed))
	if _, err := rng.Read(buf); err != nil {
		return err
	}
	var off int64
	for off < readBytes {
		n := int64(len(buf))
		if rem := readBytes - off; rem < n {
			n = rem
		}
		if _, err := f.WriteAt(buf[:n], off); err != nil {
			return err
		}
		off += n
	}
	return f.Sync()
}

// workerResult is one worker's private tally, written without a lock (each
// worker owns a distinct slice index) and aggregated single-threaded at the end.
type workerResult struct {
	ops     uint64
	bytes   uint64
	samples []int64
	errs    map[string]uint64
}

func runRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("dir", "", "mounted directory (required)")
	system := fs.String("system", "unknown", "system label for the receipt row")
	workload := fs.String("workload", "", "append | read (required)")
	writers := fs.Int("writers", 1, "append arm: concurrent writers")
	readers := fs.Int("readers", 64, "read arm: concurrent readers")
	prefix := fs.String("append-prefix", "append", "writer file prefix")
	readFile := fs.String("read-file", "data.bin", "read arm file")
	bs := fs.Int("bs", 4096, "read arm block size")
	recordBytes := fs.Int("record-bytes", 4096, "append record size")
	seconds := fs.Float64("seconds", 5, "bounded run duration")
	maxPerWriter := fs.Int64("max-bytes-per-writer", 8<<20, "bound each writer's appended bytes")
	maxSamples := fs.Int("max-samples-per-worker", maxSamplesPerWorker, "per-worker latency sample cap")
	seed := fs.Int64("seed", 1, "deterministic seed")
	out := fs.String("out", "", "receipt path (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "orfscompare run: -dir is required")
		return 2
	}
	if *seconds <= 0 {
		fmt.Fprintln(os.Stderr, "orfscompare run: -seconds must be positive")
		return 2
	}
	if *maxSamples <= 0 {
		*maxSamples = maxSamplesPerWorker
	}

	var run osscompare.Run
	var err error
	switch *workload {
	case "append":
		run, err = runAppend(*dir, *system, *prefix, *writers, *recordBytes, *seconds, *maxPerWriter, *maxSamples, *seed)
	case "read":
		run, err = runRead(*dir, *system, *readFile, *readers, *bs, *seconds, *maxSamples, *seed)
	default:
		fmt.Fprintf(os.Stderr, "orfscompare run: -workload must be append or read (got %q)\n", *workload)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfscompare run: %v\n", err)
		return 1
	}

	blob, _ := json.MarshalIndent(run, "", "  ")
	blob = append(blob, '\n')
	if *out == "" {
		os.Stdout.Write(blob)
	} else if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "orfscompare run: write receipt: %v\n", err)
		return 1
	}
	if run.Stats.Ops == 0 {
		fmt.Fprintln(os.Stderr, "orfscompare run: no operations ran")
		return 1
	}
	return 0
}

// runAppend appends fixed records with write+fsync per record at `writers`
// concurrency. Each writer owns a precreated file.
func runAppend(dir, system, prefix string, writers, recordBytes int, seconds float64, maxPerWriter int64, maxSamples int, seed int64) (osscompare.Run, error) {
	if writers < 1 {
		return osscompare.Run{}, fmt.Errorf("append needs at least one writer")
	}
	if recordBytes <= 0 {
		return osscompare.Run{}, fmt.Errorf("record-bytes must be positive")
	}
	// Open every writer's file before the timed window so a missing file is a
	// setup error, not a measurement artifact.
	files := make([]*os.File, writers)
	starts := make([]uint64, writers)
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	for w := 0; w < writers; w++ {
		path := join(dir, fmt.Sprintf("%s-%d.bin", prefix, w))
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return osscompare.Run{}, fmt.Errorf("open writer file %s: %w", path, err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return osscompare.Run{}, fmt.Errorf("stat writer file %s: %w", path, err)
		}
		files[w] = f
		starts[w] = uint64(info.Size())
	}

	start := time.Now()
	deadline := start.Add(time.Duration(seconds * float64(time.Second)))
	var firstErr atomic.Int64
	noteError := func() { firstErr.CompareAndSwap(0, time.Now().UnixNano()) }
	var capped atomic.Bool

	results := make([]workerResult, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			f := files[w]
			buf := make([]byte, recordBytes)
			for i := range buf {
				buf[i] = byte(w*31 + i)
			}
			off := starts[w]
			var appended uint64
			res := workerResult{errs: map[string]uint64{}, samples: make([]int64, 0, 4096)}
			for time.Now().Before(deadline) {
				if appended >= uint64(maxPerWriter) {
					capped.Store(true)
					break
				}
				t0 := time.Now()
				n, werr := f.WriteAt(buf, int64(off))
				var ferr error
				if werr == nil {
					ferr = f.Sync()
				}
				d := time.Since(t0).Nanoseconds()
				res.ops++
				if len(res.samples) < maxSamples {
					res.samples = append(res.samples, d)
				}
				if werr != nil {
					res.errs[errnoName(werr)]++
					noteError()
					continue // retry the same offset
				}
				if n > 0 {
					res.bytes += uint64(n)
					appended += uint64(n)
					off += uint64(n)
				}
				if ferr != nil {
					res.errs[errnoName(ferr)]++
					noteError()
				}
			}
			results[w] = res
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = seconds
	}

	run := osscompare.Run{
		Schema:         osscompare.RunSchema,
		System:         system,
		Workload:       "append",
		Dir:            dir,
		Writers:        writers,
		BlockSize:      recordBytes,
		RecordBytes:    recordBytes,
		RequestSeconds: seconds,
		ElapsedSeconds: elapsed,
		Seed:           seed,
		Durability:     "write+fsync per record (one durability barrier per record requested)",
		Note: "Every writer owns its own precreated file and appends fixed records with write(2)+fsync(2) per " +
			"record: this is the append figure with one durability barrier per record requested of the mount. " +
			"Whether the mount honours it is a per-system property the court records, not one this driver can " +
			"assert. Latency is measured around the write+fsync pair. Exact p50/p95/p99/p99.9 from per-worker " +
			"latency samples, merged and sorted at the end. Bounded by duration and by -max-bytes-per-writer.",
	}
	run.Stats = summarise(results, elapsed)
	run.WriterCapped = capped.Load()
	run.FirstErrorSeconds = firstErrorSeconds(firstErr.Load(), start)
	return run, nil
}

// runRead performs random reads at the given block size at `readers`
// concurrency over a preexisting file.
func runRead(dir, system, readFile string, readers, bs int, seconds float64, maxSamples int, seed int64) (osscompare.Run, error) {
	if readers < 1 {
		return osscompare.Run{}, fmt.Errorf("read needs at least one reader")
	}
	if bs <= 0 {
		return osscompare.Run{}, fmt.Errorf("bs must be positive")
	}
	f, err := os.Open(join(dir, readFile))
	if err != nil {
		return osscompare.Run{}, fmt.Errorf("open read file %s: %w", readFile, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return osscompare.Run{}, fmt.Errorf("stat read file %s: %w", readFile, err)
	}
	size := info.Size()
	if size < int64(bs) {
		return osscompare.Run{}, fmt.Errorf("read file %s is %d bytes, need at least block size %d", readFile, size, bs)
	}
	blocks := size / int64(bs)

	start := time.Now()
	deadline := start.Add(time.Duration(seconds * float64(time.Second)))
	var firstErr atomic.Int64
	noteError := func() { firstErr.CompareAndSwap(0, time.Now().UnixNano()) }

	results := make([]workerResult, readers)
	var wg sync.WaitGroup
	for w := 0; w < readers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(w)))
			buf := make([]byte, bs)
			res := workerResult{errs: map[string]uint64{}, samples: make([]int64, 0, 4096)}
			for time.Now().Before(deadline) {
				off := (rng.Int63() % blocks) * int64(bs)
				t0 := time.Now()
				n, err := f.ReadAt(buf, off)
				d := time.Since(t0).Nanoseconds()
				res.ops++
				if len(res.samples) < maxSamples {
					res.samples = append(res.samples, d)
				}
				if err != nil {
					res.errs[errnoName(err)]++
					noteError()
				} else if n != bs {
					res.errs["SHORT_READ"]++
					noteError()
				} else {
					res.bytes += uint64(n)
				}
			}
			results[w] = res
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = seconds
	}

	run := osscompare.Run{
		Schema:         osscompare.RunSchema,
		System:         system,
		Workload:       "read",
		Dir:            dir,
		Readers:        readers,
		BlockSize:      bs,
		RequestSeconds: seconds,
		ElapsedSeconds: elapsed,
		Seed:           seed,
		Durability:     "none (reads)",
		Note: "Block-aligned uniform-random pread at the given block size over a seeded file. Exact " +
			"p50/p95/p99/p99.9 from per-worker latency samples, merged and sorted at the end. Bounded by duration.",
	}
	run.Stats = summarise(results, elapsed)
	run.FirstErrorSeconds = firstErrorSeconds(firstErr.Load(), start)
	return run, nil
}

// summarise merges worker results plus an elapsed window into the receipt stats.
func summarise(results []workerResult, elapsed float64) osscompare.Stats {
	var ops, bytes, errs uint64
	var samples []int64
	classes := map[string]uint64{}
	for _, r := range results {
		ops += r.ops
		bytes += r.bytes
		for k, v := range r.errs {
			errs += v
			classes[k] += v
		}
		samples = append(samples, r.samples...)
	}
	s := osscompare.Stats{
		Ops:          ops,
		Bytes:        bytes,
		Errors:       errs,
		Sampled:      uint64(len(samples)),
		Truncated:    ops > uint64(len(samples)),
		ErrorClasses: classes,
	}
	if elapsed > 0 {
		s.OpsPerSecond = float64(ops) / elapsed
		s.BytesPerSecond = float64(bytes) / elapsed
	}
	if ops > 0 {
		s.ErrorRate = float64(errs) / float64(ops)
	}
	if len(samples) == 0 {
		return s
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var sum int64
	for _, v := range samples {
		sum += v
	}
	s.MeanNs = sum / int64(len(samples))
	s.P50Ns = quantile(samples, 0.50)
	s.P95Ns = quantile(samples, 0.95)
	s.P99Ns = quantile(samples, 0.99)
	s.P999Ns = quantile(samples, 0.999)
	s.MaxNs = samples[len(samples)-1]
	return s
}

// firstErrorSeconds is the time from workload start to the first failed syscall
// (0 when none failed).
func firstErrorSeconds(ns int64, start time.Time) float64 {
	if ns > start.UnixNano() {
		return float64(ns-start.UnixNano()) / 1e9
	}
	return 0
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
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// errnoName classifies an error by its errno, so the receipt's error classes
// match the kernel vocabulary a storage engineer expects.
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
