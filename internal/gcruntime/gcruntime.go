// Package gcruntime collects Go runtime telemetry so a court can attribute
// latency to the runtime — GC pauses, scheduler latency, allocation throughput,
// mutex wait, and resident memory — rather than guess.
//
// Everything here is observed, never invented. Values come from
// runtime/metrics (the runtime's own cumulative counters and histograms) and,
// for resident memory, from /proc/self/status VmRSS/VmHWM. A metric the running
// toolchain does not expose is recorded in Snapshot.Missing rather than
// reported as a silent zero. Nothing is turned into a rate until two snapshots
// are subtracted over a known window.
package gcruntime

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

// Schema is the runtime-metrics artifact schema written by WriteProfileDir.
const Schema = "orfs.runtime.metrics.v1"

// metricNames are the runtime metrics this package reads. The names are the
// stable runtime/metrics spellings; a name the toolchain does not know lands in
// Snapshot.Missing.
var metricNames = []string{
	"/gc/heap/allocs:bytes",          // cumulative bytes allocated
	"/gc/cycles/total:gc-cycles",     // cumulative GC cycles
	"/sync/mutex/wait/total:seconds", // cumulative blocked-on-mutex time
	"/gc/heap/goal:bytes",            // current heap goal
	"/gc/heap/live:bytes",            // live heap after the last cycle
	"/gc/gogc:percent",               // effective GOGC
	"/gc/gomemlimit:bytes",           // effective GOMEMLIMIT
	"/sched/gomaxprocs:threads",      // effective GOMAXPROCS
	"/sched/pauses/total/gc:seconds", // cumulative GC stop-the-world histogram
	"/sched/latencies:seconds",       // cumulative scheduler-latency histogram
}

// Histogram is a cumulative runtime histogram. Bounds are the bucket edges, in
// increasing order, with len(Bounds) == len(Counts)+1; Counts[i] is the weight
// of [Bounds[i], Bounds[i+1]). The final edge is +Inf (and the first may be
// -Inf), which JSON cannot encode as a number, so Histogram marshals non-finite
// edges as the strings "-Inf"/"+Inf" and round-trips them.
type Histogram struct {
	Bounds []float64 `json:"bounds"`
	Counts []uint64  `json:"counts"`
	Count  uint64    `json:"count"`
}

// MarshalJSON encodes non-finite bucket edges as strings, since encoding/json
// refuses NaN and ±Inf.
func (h Histogram) MarshalJSON() ([]byte, error) {
	bounds := make([]any, len(h.Bounds))
	for i, b := range h.Bounds {
		switch {
		case math.IsInf(b, 1):
			bounds[i] = "+Inf"
		case math.IsInf(b, -1):
			bounds[i] = "-Inf"
		case math.IsNaN(b):
			bounds[i] = "NaN"
		default:
			bounds[i] = b
		}
	}
	return json.Marshal(struct {
		Bounds []any    `json:"bounds"`
		Counts []uint64 `json:"counts"`
		Count  uint64   `json:"count"`
	}{bounds, h.Counts, h.Count})
}

// UnmarshalJSON is the inverse of MarshalJSON, accepting either a number or the
// "-Inf"/"+Inf"/"NaN" strings for an edge.
func (h *Histogram) UnmarshalJSON(blob []byte) error {
	var raw struct {
		Bounds []any    `json:"bounds"`
		Counts []uint64 `json:"counts"`
		Count  uint64   `json:"count"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return err
	}
	h.Bounds = make([]float64, len(raw.Bounds))
	for i, v := range raw.Bounds {
		switch t := v.(type) {
		case float64:
			h.Bounds[i] = t
		case string:
			switch t {
			case "+Inf":
				h.Bounds[i] = math.Inf(1)
			case "-Inf":
				h.Bounds[i] = math.Inf(-1)
			default:
				h.Bounds[i] = math.NaN()
			}
		}
	}
	h.Counts = raw.Counts
	h.Count = raw.Count
	return nil
}

// Snapshot is the runtime state at one instant.
type Snapshot struct {
	UnixNanos int64 `json:"unix_nanos"`

	// RSSBytes is the process's current resident set size, read from
	// /proc/self/status VmRSS. PeakRSSBytes is VmHWM (the peak since start).
	// Both are 0 when /proc is unreadable; the receipt note says so.
	RSSBytes     uint64 `json:"rss_bytes"`
	PeakRSSBytes uint64 `json:"peak_rss_bytes"`

	AllocBytes       uint64  `json:"alloc_bytes"`        // cumulative
	GCCycles         uint64  `json:"gc_cycles"`          // cumulative
	MutexWaitSeconds float64 `json:"mutex_wait_seconds"` // cumulative
	HeapGoalBytes    uint64  `json:"heap_goal_bytes"`
	HeapLiveBytes    uint64  `json:"heap_live_bytes"`
	GOGCPercent      uint64  `json:"gogc_percent"`
	GOMemLimitBytes  uint64  `json:"gomemlimit_bytes"`
	GOMAXPROCS       uint64  `json:"gomaxprocs"`

	GCPauses       Histogram `json:"gc_pauses"`
	SchedLatencies Histogram `json:"sched_latencies"`

	Missing []string `json:"missing_metrics,omitempty"`
}

// Capture reads the runtime metrics and the process's resident memory.
func Capture() Snapshot {
	samples := make([]metrics.Sample, len(metricNames))
	for i, n := range metricNames {
		samples[i].Name = n
	}
	metrics.Read(samples)

	s := Snapshot{UnixNanos: time.Now().UnixNano()}
	var missing []string
	for i, sm := range samples {
		name := metricNames[i]
		switch name {
		case "/gc/heap/allocs:bytes":
			s.AllocBytes = uint64Of(&missing, name, sm.Value)
		case "/gc/cycles/total:gc-cycles":
			s.GCCycles = uint64Of(&missing, name, sm.Value)
		case "/gc/heap/goal:bytes":
			s.HeapGoalBytes = uint64Of(&missing, name, sm.Value)
		case "/gc/heap/live:bytes":
			s.HeapLiveBytes = uint64Of(&missing, name, sm.Value)
		case "/gc/gogc:percent":
			s.GOGCPercent = uint64Of(&missing, name, sm.Value)
		case "/gc/gomemlimit:bytes":
			s.GOMemLimitBytes = uint64Of(&missing, name, sm.Value)
		case "/sched/gomaxprocs:threads":
			s.GOMAXPROCS = uint64Of(&missing, name, sm.Value)
		case "/sync/mutex/wait/total:seconds":
			if sm.Value.Kind() == metrics.KindFloat64 {
				s.MutexWaitSeconds = sm.Value.Float64()
			} else {
				missing = append(missing, name)
			}
		case "/sched/pauses/total/gc:seconds":
			if h, ok := histogramOf(sm.Value); ok {
				s.GCPauses = h
			} else {
				missing = append(missing, name)
			}
		case "/sched/latencies:seconds":
			if h, ok := histogramOf(sm.Value); ok {
				s.SchedLatencies = h
			} else {
				missing = append(missing, name)
			}
		default:
			missing = append(missing, name)
		}
	}
	s.RSSBytes, s.PeakRSSBytes = readRSS()
	s.Missing = missing
	return s
}

func uint64Of(missing *[]string, name string, v metrics.Value) uint64 {
	if v.Kind() == metrics.KindUint64 {
		return v.Uint64()
	}
	*missing = append(*missing, name)
	return 0
}

func histogramOf(v metrics.Value) (Histogram, bool) {
	if v.Kind() != metrics.KindFloat64Histogram {
		return Histogram{}, false
	}
	src := v.Float64Histogram()
	h := Histogram{
		Bounds: append([]float64(nil), src.Buckets...),
		Counts: append([]uint64(nil), src.Counts...),
	}
	for _, c := range h.Counts {
		h.Count += c
	}
	return h, true
}

// readRSS parses VmRSS and VmHWM (kB) from /proc/self/status. It returns
// (0, 0) when the file is unreadable (e.g. a non-Linux host), which the caller
// surfaces as "RSS unavailable" rather than a fabricated number.
func readRSS() (rss, peak uint64) {
	blob, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(blob), "\n") {
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			rss = parseKB(line)
		case strings.HasPrefix(line, "VmHWM:"):
			peak = parseKB(line)
		}
	}
	return rss, peak
}

func parseKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	kb, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// Delta is the windowed runtime cost between two snapshots.
type Delta struct {
	StartUnixNanos int64   `json:"start_unix_nanos"`
	EndUnixNanos   int64   `json:"end_unix_nanos"`
	DurationS      float64 `json:"duration_seconds"`

	AllocBytes          uint64  `json:"alloc_bytes"`
	AllocBytesPerSecond float64 `json:"alloc_bytes_per_second"`
	GCCycles            uint64  `json:"gc_cycles"`
	MutexWaitSeconds    float64 `json:"mutex_wait_seconds"`

	GCPauseCount      uint64  `json:"gc_pause_count"`
	GCPauseP50Seconds float64 `json:"gc_pause_p50_seconds"`
	GCPauseP99Seconds float64 `json:"gc_pause_p99_seconds"`

	SchedP50Seconds float64 `json:"sched_latency_p50_seconds"`
	SchedP99Seconds float64 `json:"sched_latency_p99_seconds"`

	// Resident memory: RSS is the end snapshot's VmRSS; PeakRSSBytes is the
	// VmHWM high-water mark (a true peak since process start, not a sampled
	// maximum). Both are 0 when /proc is unreadable.
	RSSBytes     uint64 `json:"rss_bytes"`
	PeakRSSBytes uint64 `json:"peak_rss_bytes"`

	HeapLiveBytes uint64 `json:"heap_live_bytes"`
	HeapGoalBytes uint64 `json:"heap_goal_bytes"`

	GOGCPercent     uint64 `json:"gogc_percent"`
	GOMemLimitBytes uint64 `json:"gomemlimit_bytes"`
	GOMAXPROCS      uint64 `json:"gomaxprocs"`
}

// DeltaBetween subtracts two snapshots, windowing the cumulative counters and
// histograms so rates and percentiles describe the interval, not process life.
func DeltaBetween(start, end Snapshot) Delta {
	d := Delta{
		StartUnixNanos:   start.UnixNanos,
		EndUnixNanos:     end.UnixNanos,
		AllocBytes:       sub64(end.AllocBytes, start.AllocBytes),
		GCCycles:         sub64(end.GCCycles, start.GCCycles),
		MutexWaitSeconds: subf(end.MutexWaitSeconds, start.MutexWaitSeconds),
		RSSBytes:         end.RSSBytes,
		PeakRSSBytes:     max64(start.PeakRSSBytes, end.PeakRSSBytes),
		HeapLiveBytes:    end.HeapLiveBytes,
		HeapGoalBytes:    end.HeapGoalBytes,
		GOGCPercent:      end.GOGCPercent,
		GOMemLimitBytes:  end.GOMemLimitBytes,
		GOMAXPROCS:       end.GOMAXPROCS,
	}
	if end.UnixNanos > start.UnixNanos {
		d.DurationS = float64(end.UnixNanos-start.UnixNanos) / 1e9
	}
	if d.DurationS > 0 {
		d.AllocBytesPerSecond = float64(d.AllocBytes) / d.DurationS
	}
	gp := DiffHistograms(start.GCPauses, end.GCPauses)
	d.GCPauseCount = gp.Count
	d.GCPauseP50Seconds = Quantile(gp, 0.50)
	d.GCPauseP99Seconds = Quantile(gp, 0.99)
	sl := DiffHistograms(start.SchedLatencies, end.SchedLatencies)
	d.SchedP50Seconds = Quantile(sl, 0.50)
	d.SchedP99Seconds = Quantile(sl, 0.99)
	return d
}

// DiffHistograms subtracts one cumulative histogram from a later one, yielding
// the interval's distribution. Counts that went backwards (a reset) clamp to 0.
func DiffHistograms(a, b Histogram) Histogram {
	n := len(b.Counts)
	if len(a.Counts) < n {
		n = len(a.Counts)
	}
	h := Histogram{Bounds: b.Bounds, Counts: make([]uint64, n)}
	if h.Bounds == nil {
		h.Bounds = a.Bounds
	}
	for i := 0; i < n; i++ {
		if b.Counts[i] >= a.Counts[i] {
			h.Counts[i] = b.Counts[i] - a.Counts[i]
		}
	}
	h.Count = sub64(b.Count, a.Count)
	return h
}

// Quantile estimates the q-quantile from a cumulative histogram's bucket edges.
// It returns the upper edge of the bucket the rank falls in; when the rank
// lands in the open-ended top bucket the bucket's finite lower edge is returned
// (a lower bound, never +Inf, which JSON cannot carry).
func Quantile(h Histogram, q float64) float64 {
	if h.Count == 0 || len(h.Counts) == 0 || len(h.Bounds) < 2 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(h.Count)))
	if rank < 1 {
		rank = 1
	}
	var cum uint64
	for i, c := range h.Counts {
		cum += c
		if cum >= rank {
			return bucketUpper(h.Bounds, i)
		}
	}
	return bucketUpper(h.Bounds, len(h.Counts)-1)
}

func bucketUpper(bounds []float64, i int) float64 {
	if i+1 < len(bounds) && !math.IsInf(bounds[i+1], 1) {
		return finite(bounds[i+1])
	}
	if i < len(bounds) && !math.IsInf(bounds[i], 1) {
		return finite(bounds[i])
	}
	return 0
}

// Options configures a profile dump.
type Options struct {
	// Locks requests the mutex and block pprof profiles. They are only
	// meaningful when EnableLockProfiles was called before the workload.
	Locks bool
	Note  string
}

// Document is the runtime-metrics.json artifact.
type Document struct {
	Schema string   `json:"schema"`
	Note   string   `json:"note,omitempty"`
	Start  Snapshot `json:"start"`
	End    Snapshot `json:"end"`
	Delta  Delta    `json:"delta"`
}

// EnableLockProfiles turns on mutex and block profiling for the process. It
// must be called before the workload so contention is sampled.
func EnableLockProfiles() {
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
}

// WriteProfileDir writes runtime-metrics.json plus pprof profiles (heap and
// goroutine always; mutex and block when opt.Locks) into dir. It falls back to
// an empty file when a named profile is unavailable, so a partial toolchain
// never fails the daemon.
func WriteProfileDir(dir string, start, end Snapshot, opt Options) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	doc := Document{Schema: Schema, Note: opt.Note, Start: start, End: end, Delta: DeltaBetween(start, end)}
	doc.Delta.sanitize()
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	if err := os.WriteFile(filepath.Join(dir, "runtime-metrics.json"), blob, 0o644); err != nil {
		return err
	}
	if err := writeProfile(dir, "heap.prof", "heap"); err != nil {
		return err
	}
	if err := writeProfile(dir, "goroutine.prof", "goroutine"); err != nil {
		return err
	}
	if opt.Locks {
		if err := writeProfile(dir, "mutex.prof", "mutex"); err != nil {
			return err
		}
		if err := writeProfile(dir, "block.prof", "block"); err != nil {
			return err
		}
	}
	return nil
}

func writeProfile(dir, file, name string) error {
	p := pprof.Lookup(name)
	if p == nil {
		return nil
	}
	f, err := os.Create(filepath.Join(dir, file))
	if err != nil {
		return err
	}
	defer f.Close()
	return p.WriteTo(f, 0)
}

// finite clamps a non-finite float to 0 so the JSON artifact always marshals.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// sanitize clamps every float in the windowed delta, since json.Marshal refuses
// NaN and ±Inf and an artifact that cannot be written is worse than a zero.
func (d *Delta) sanitize() {
	d.DurationS = finite(d.DurationS)
	d.AllocBytesPerSecond = finite(d.AllocBytesPerSecond)
	d.MutexWaitSeconds = finite(d.MutexWaitSeconds)
	d.GCPauseP50Seconds = finite(d.GCPauseP50Seconds)
	d.GCPauseP99Seconds = finite(d.GCPauseP99Seconds)
	d.SchedP50Seconds = finite(d.SchedP50Seconds)
	d.SchedP99Seconds = finite(d.SchedP99Seconds)
}

func sub64(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return 0
}

func subf(a, b float64) float64 {
	if a >= b {
		return a - b
	}
	return 0
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
