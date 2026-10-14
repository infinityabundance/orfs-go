package retention

import (
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// defaultMaxSamplesPerWorker bounds the driver's own memory on a long arm: past
// it the worker still performs I/O but stops recording latencies.
const defaultMaxSamplesPerWorker = 400_000

// AppendConfig describes one defined append epoch. Records is the epoch size: the
// arm appends exactly Records records per writer (one write(2)+fsync(2) per
// record), so the committed state after the epoch is well defined and retained.
type AppendConfig struct {
	Dir               string
	System            string
	Prefix            string
	Writers           int
	Records           int
	RecordBytes       int
	MaxSeconds        float64 // safety time bound (0 = none)
	MaxBytesPerWriter int64   // optional byte cap (0 = none)
	MaxSamples        int
	Seed              int64
	Epoch             int
}

// Prepare creates each writer's dedicated file (empty) before the measured
// window, mirroring cmd/orfscompare prepare, so file creation is not charged to
// the first epoch's append latency.
func Prepare(dir, prefix string, writers int) error {
	if dir == "" {
		return fmt.Errorf("prepare: -dir is required")
	}
	if writers < 0 {
		return fmt.Errorf("prepare: writers must be >= 0")
	}
	for i := 0; i < writers; i++ {
		path := join(dir, fmt.Sprintf("%s-%d.bin", prefix, i))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("prepare: create %s: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("prepare: sync %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("prepare: close %s: %w", path, err)
		}
	}
	return nil
}

// RunAppend appends Records records per writer to each writer's own file with
// write(2)+fsync(2) per record — one durability barrier per record, the same
// durable-append shape as cmd/orfscompare — and returns the exact-sample
// measurement of the epoch.
func RunAppend(cfg AppendConfig) (Measure, error) {
	if cfg.Writers < 1 {
		return Measure{}, fmt.Errorf("append needs at least one writer")
	}
	if cfg.Records < 1 {
		return Measure{}, fmt.Errorf("append needs at least one record per epoch")
	}
	if cfg.RecordBytes <= 0 {
		return Measure{}, fmt.Errorf("record-bytes must be positive")
	}
	maxSamples := cfg.MaxSamples
	if maxSamples <= 0 {
		maxSamples = defaultMaxSamplesPerWorker
	}

	files := make([]*os.File, cfg.Writers)
	starts := make([]int64, cfg.Writers)
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	for w := 0; w < cfg.Writers; w++ {
		path := join(cfg.Dir, fmt.Sprintf("%s-%d.bin", cfg.Prefix, w))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0)
		if err != nil {
			return Measure{}, fmt.Errorf("open writer file %s: %w", path, err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return Measure{}, fmt.Errorf("stat writer file %s: %w", path, err)
		}
		files[w] = f
		starts[w] = info.Size()
	}

	start := time.Now()
	var deadline time.Time
	if cfg.MaxSeconds > 0 {
		deadline = start.Add(time.Duration(cfg.MaxSeconds * float64(time.Second)))
	}
	var timeCapped atomic.Bool
	var firstErr atomic.Int64
	noteError := func() { firstErr.CompareAndSwap(0, time.Now().UnixNano()) }

	results := make([]WorkerResult, cfg.Writers)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			f := files[w]
			buf := make([]byte, cfg.RecordBytes)
			off := starts[w]
			// The first record's global index is derived from the file's current
			// length, so the epoch's record contents are deterministic and encode
			// where in the history they belong.
			firstRec := off / int64(cfg.RecordBytes)
			res := WorkerResult{Errs: map[string]uint64{}, Samples: make([]int64, 0, 4096)}
			for j := 0; j < cfg.Records; j++ {
				if !deadline.IsZero() && time.Now().After(deadline) {
					timeCapped.Store(true)
					break
				}
				if cfg.MaxBytesPerWriter > 0 && off-starts[w] >= cfg.MaxBytesPerWriter {
					break
				}
				fillRecord(buf, w, firstRec+int64(j))
				t0 := time.Now()
				n, werr := f.WriteAt(buf, off)
				var ferr error
				if werr == nil {
					ferr = f.Sync()
				}
				d := time.Since(t0).Nanoseconds()
				res.Ops++
				if len(res.Samples) < maxSamples {
					res.Samples = append(res.Samples, d)
				}
				if werr != nil {
					res.Errs[ErrnoName(werr)]++
					noteError()
					continue // retry the same offset
				}
				if n > 0 {
					res.Bytes += uint64(n)
					off += int64(n)
				}
				if ferr != nil {
					res.Errs[ErrnoName(ferr)]++
					noteError()
				}
			}
			results[w] = res
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = cfg.MaxSeconds
	}

	m := Measure{
		Schema:         MeasurementSchema,
		System:         cfg.System,
		Kind:           KindAppend,
		State:          StateAppend,
		Epoch:          cfg.Epoch,
		Writers:        cfg.Writers,
		BlockSize:      cfg.RecordBytes,
		RecordBytes:    cfg.RecordBytes,
		Records:        cfg.Records,
		RequestSeconds: cfg.MaxSeconds,
		ElapsedSeconds: elapsed,
		Seed:           cfg.Seed,
		Durability:     "write+fsync per record (one durability barrier per record)",
		TimeCapped:     timeCapped.Load(),
		Note: "A defined epoch: every writer appends exactly Records records with write(2)+fsync(2) per record. " +
			"The committed state after the epoch is well defined and is what the court retains (Btrfs: a read-only " +
			"snapshot; OrFS-GO: the authoritative log). Exact p50/p95/p99/p99.9 from per-worker latency samples, " +
			"merged and sorted at the end.",
	}
	m.Stats = Summarise(results, elapsed)
	return m, nil
}

// ReadConfig describes one read arm over a directory (the current state or a
// retained snapshot).
type ReadConfig struct {
	Dir        string
	System     string
	ReadFile   string
	Readers    int
	BlockSize  int
	Seconds    float64
	MaxSamples int
	Seed       int64
	Epoch      int
	State      string // current | snapshot
}

// RunRead performs block-aligned uniform-random preads over ReadFile at the
// given block size and concurrency, over the current state or a retained
// snapshot, and returns the exact-sample measurement.
func RunRead(cfg ReadConfig) (Measure, error) {
	if cfg.Readers < 1 {
		return Measure{}, fmt.Errorf("read needs at least one reader")
	}
	if cfg.BlockSize <= 0 {
		return Measure{}, fmt.Errorf("bs must be positive")
	}
	if cfg.Seconds <= 0 {
		return Measure{}, fmt.Errorf("seconds must be positive")
	}
	maxSamples := cfg.MaxSamples
	if maxSamples <= 0 {
		maxSamples = defaultMaxSamplesPerWorker
	}
	state := cfg.State
	if state == "" {
		state = StateCurrent
	}

	f, err := os.Open(join(cfg.Dir, cfg.ReadFile))
	if err != nil {
		return Measure{}, fmt.Errorf("open read file %s: %w", cfg.ReadFile, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Measure{}, fmt.Errorf("stat read file %s: %w", cfg.ReadFile, err)
	}
	size := info.Size()
	if size < int64(cfg.BlockSize) {
		return Measure{}, fmt.Errorf("read file %s is %d bytes, need at least block size %d", cfg.ReadFile, size, cfg.BlockSize)
	}
	blocks := size / int64(cfg.BlockSize)

	start := time.Now()
	deadline := start.Add(time.Duration(cfg.Seconds * float64(time.Second)))
	var firstErr atomic.Int64
	noteError := func() { firstErr.CompareAndSwap(0, time.Now().UnixNano()) }

	results := make([]WorkerResult, cfg.Readers)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Readers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(cfg.Seed + int64(w)))
			buf := make([]byte, cfg.BlockSize)
			res := WorkerResult{Errs: map[string]uint64{}, Samples: make([]int64, 0, 4096)}
			for time.Now().Before(deadline) {
				off := (rng.Int63() % blocks) * int64(cfg.BlockSize)
				t0 := time.Now()
				n, rerr := f.ReadAt(buf, off)
				d := time.Since(t0).Nanoseconds()
				res.Ops++
				if len(res.Samples) < maxSamples {
					res.Samples = append(res.Samples, d)
				}
				if rerr != nil {
					res.Errs[ErrnoName(rerr)]++
					noteError()
				} else if n != cfg.BlockSize {
					res.Errs["SHORT_READ"]++
					noteError()
				} else {
					res.Bytes += uint64(n)
				}
			}
			results[w] = res
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = cfg.Seconds
	}

	m := Measure{
		Schema:         MeasurementSchema,
		System:         cfg.System,
		Kind:           KindRead,
		State:          state,
		Epoch:          cfg.Epoch,
		Readers:        cfg.Readers,
		BlockSize:      cfg.BlockSize,
		RequestSeconds: cfg.Seconds,
		ElapsedSeconds: elapsed,
		Seed:           cfg.Seed,
		Durability:     "none (reads)",
		Note: "Block-aligned uniform-random pread at the given block size. state=current reads the live committed " +
			"state; state=snapshot reads a retained historical state (a Btrfs read-only snapshot). Exact " +
			"p50/p95/p99/p99.9 from per-worker latency samples, merged and sorted at the end.",
	}
	m.Stats = Summarise(results, elapsed)
	return m, nil
}

// fillRecord writes a deterministic record body that encodes the writer and the
// record's global index in its history, so two runs of the same workload produce
// byte-identical data and a retained snapshot's content is reproducible.
func fillRecord(buf []byte, writer int, rec int64) {
	for i := range buf {
		buf[i] = byte((rec*7 + int64(writer)*31 + int64(i)) & 0xff)
	}
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	if len(dir) > 0 && dir[len(dir)-1] == '/' {
		return dir + name
	}
	return dir + "/" + name
}
