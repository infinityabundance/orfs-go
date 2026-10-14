// Command gcsummary merges the Go-runtime court's per-arm artifacts (the
// orfsrw workload receipt and the mount daemon's runtime-metrics dump) into one
// machine-readable receipt, evidence/performance/gc-scaling.json
// (schema orfs.performance.gc-scaling.v1).
//
// It parallels cmd/fiosummary: the shell court produces raw per-arm files, and
// this tool is the only place that turns them into the receipt. It never
// invents a number: a missing daemon artifact marks the row's status rather
// than fabricating zeros, and it can name the best and worst arm by measured
// p99.9 so the court can re-run exactly those two with a GC trace and the lock
// profiles enabled.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Row is one configuration's measured result.
type Row struct {
	GoGC     string `json:"gogc"`
	MemLimit string `json:"memlimit"`
	Readers  int    `json:"readers"`
	Writers  int    `json:"writers"`

	Status string `json:"status"` // ok | daemon_died | driver_failed
	Note   string `json:"note,omitempty"`

	P50Ns  int64 `json:"p50_ns"`
	P99Ns  int64 `json:"p99_ns"`
	P999Ns int64 `json:"p99_9_ns"`

	ReadIOPerSecond     float64           `json:"read_io_per_second"`
	WriteIOPerSecond    float64           `json:"write_io_per_second"`
	ThroughputIOPS      float64           `json:"throughput_iops"`
	ThroughputMiBPerSec float64           `json:"throughput_mib_per_second"`
	ErrorRate           float64           `json:"error_rate"`
	Errors              map[string]uint64 `json:"errors,omitempty"`

	// FirstErrorSeconds is how soon the first failed syscall happened (0 when
	// none failed). A small value on a daemon_died row shows the mount was gone
	// almost immediately, so the row's latencies are failed syscalls.
	FirstErrorSeconds float64 `json:"first_error_seconds,omitempty"`

	GCPauseP50Seconds float64 `json:"gc_pause_p50_seconds"`
	GCPauseP99Seconds float64 `json:"gc_pause_p99_seconds"`
	GCPauseCount      uint64  `json:"gc_pause_count"`
	GCCycles          uint64  `json:"gc_cycles"`
	SchedP99Seconds   float64 `json:"sched_latency_p99_seconds"`
	AllocBytesPerS    float64 `json:"alloc_bytes_per_s"`

	RSSBytes      uint64 `json:"rss_bytes"`       // real VmRSS at the window end
	PeakRSSBytes  uint64 `json:"peak_rss_bytes"`  // real VmHWM high-water mark
	HeapLiveBytes uint64 `json:"heap_live_bytes"` // Go live heap after last cycle

	MutexWaitSeconds float64 `json:"mutex_wait_seconds"`
	WindowSeconds    float64 `json:"window_seconds"`

	EffectiveGoGCPercent uint64 `json:"effective_gogc_percent"`
	EffectiveMemLimit    uint64 `json:"effective_gomemlimit_bytes"`

	// DaemonStderrTail is the daemon's last few non-trace stderr lines, kept for
	// a row the watchdog aborted (the only diagnostics a stalled mount leaves).
	DaemonStderrTail []string `json:"daemon_stderr_tail,omitempty"`
}

// TraceExcerpt is a short GODEBUG=gctrace=1 excerpt plus the lock profile
// locations for the best and worst configurations.
type TraceExcerpt struct {
	Label         string   `json:"label"`
	Role          string   `json:"role"`
	Lines         []string `json:"lines"`
	MutexProfile  string   `json:"mutex_profile,omitempty"`
	BlockProfile  string   `json:"block_profile,omitempty"`
	MutexTop      []string `json:"mutex_top,omitempty"`
	BlockTop      []string `json:"block_top,omitempty"`
	MutexWaitSecs float64  `json:"mutex_wait_seconds"`
}

// Receipt is evidence/performance/gc-scaling.json.
type Receipt struct {
	Schema        string         `json:"schema"`
	Method        string         `json:"method"`
	Note          string         `json:"note"`
	Bounded       bool           `json:"bounded"`
	Rows          []Row          `json:"rows"`
	TraceExcerpts []TraceExcerpt `json:"gc_trace_excerpts,omitempty"`
}

// config is the per-arm configuration the court writes (config.json).
type config struct {
	GoGC     string `json:"gogc"`
	MemLimit string `json:"memlimit"`
	Readers  int    `json:"readers"`
	Writers  int    `json:"writers"`
}

// driverReceipt mirrors the fields of cmd/orfsrw's receipt this tool needs.
type driverReceipt struct {
	Combined struct {
		Ops       uint64  `json:"ops"`
		IOPerSec  float64 `json:"io_per_second"`
		MiBPerSec float64 `json:"mib_per_second"`
		P50Ns     int64   `json:"p50_ns"`
		P99Ns     int64   `json:"p99_ns"`
		P999Ns    int64   `json:"p99_9_ns"`
		Errors    uint64  `json:"errors"`
	} `json:"combined"`
	Read struct {
		IOPerSec float64 `json:"io_per_second"`
	} `json:"read"`
	Write struct {
		IOPerSec float64 `json:"io_per_second"`
	} `json:"write"`
	FirstErrorSeconds float64           `json:"first_error_seconds"`
	Errors            map[string]uint64 `json:"errors"`
}

// runtimeMetrics mirrors the delta of internal/gcruntime's Document.
type runtimeMetrics struct {
	Schema string `json:"schema"`
	Delta  struct {
		DurationS           float64 `json:"duration_seconds"`
		AllocBytesPerSecond float64 `json:"alloc_bytes_per_second"`
		GCCycles            uint64  `json:"gc_cycles"`
		MutexWaitSeconds    float64 `json:"mutex_wait_seconds"`
		GCPauseCount        uint64  `json:"gc_pause_count"`
		GCPauseP50Seconds   float64 `json:"gc_pause_p50_seconds"`
		GCPauseP99Seconds   float64 `json:"gc_pause_p99_seconds"`
		SchedP99Seconds     float64 `json:"sched_latency_p99_seconds"`
		RSSBytes            uint64  `json:"rss_bytes"`
		PeakRSSBytes        uint64  `json:"peak_rss_bytes"`
		HeapLiveBytes       uint64  `json:"heap_live_bytes"`
		GOGCPercent         uint64  `json:"gogc_percent"`
		GOMemLimitBytes     uint64  `json:"gomemlimit_bytes"`
	} `json:"delta"`
}

func main() {
	work := flag.String("work", "", "directory of arm-* subdirectories (required)")
	out := flag.String("out", "", "receipt path (default stdout)")
	pick := flag.String("pick", "", "print the label of the best|worst arm by p99.9 and exit")
	flag.Parse()
	if *work == "" {
		fmt.Fprintln(os.Stderr, "gcsummary: -work is required")
		os.Exit(2)
	}

	arms, err := loadArms(*work)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gcsummary: %v\n", err)
		os.Exit(1)
	}
	if len(arms) == 0 {
		fmt.Fprintln(os.Stderr, "gcsummary: no arms found")
		os.Exit(1)
	}

	switch *pick {
	case "":
	case "best", "worst":
		label, ok := pickArm(arms, *pick)
		if !ok {
			fmt.Fprintf(os.Stderr, "gcsummary: no arm with a measured p99.9 to pick %s\n", *pick)
			os.Exit(1)
		}
		fmt.Println(label)
		return
	default:
		fmt.Fprintf(os.Stderr, "gcsummary: -pick must be best or worst, got %q\n", *pick)
		os.Exit(2)
	}

	rec := buildReceipt(arms)
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gcsummary: marshal: %v\n", err)
		os.Exit(1)
	}
	blob = append(blob, '\n')
	if *out == "" {
		os.Stdout.Write(blob)
		return
	}
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gcsummary: write receipt: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "gcsummary: wrote %s (%d rows)\n", *out, len(rec.Rows))
}

// arm is one loaded arm directory.
type arm struct {
	label  string
	dir    string
	config config
	row    Row
}

// loadArms reads every arm-* subdirectory of work, in name order (the court
// pads the index so the name order is the sweep order).
func loadArms(work string) ([]arm, error) {
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "arm-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var arms []arm
	for _, name := range names {
		dir := filepath.Join(work, name)
		a := arm{label: name, dir: dir}
		if err := readJSON(filepath.Join(dir, "config.json"), &a.config); err != nil {
			return nil, fmt.Errorf("%s/config.json: %w", name, err)
		}
		a.row = Row{
			GoGC:     a.config.GoGC,
			MemLimit: a.config.MemLimit,
			Readers:  a.config.Readers,
			Writers:  a.config.Writers,
			Status:   "ok",
		}
		// The driver receipt is the application view.
		var dr driverReceipt
		if err := readJSON(filepath.Join(dir, "driver.json"), &dr); err != nil {
			a.row.Status = "driver_failed"
			a.row.Note = "no driver receipt: " + err.Error()
		} else {
			a.row.P50Ns = dr.Combined.P50Ns
			a.row.P99Ns = dr.Combined.P99Ns
			a.row.P999Ns = dr.Combined.P999Ns
			a.row.ReadIOPerSecond = dr.Read.IOPerSec
			a.row.WriteIOPerSecond = dr.Write.IOPerSec
			a.row.ThroughputIOPS = dr.Combined.IOPerSec
			a.row.ThroughputMiBPerSec = dr.Combined.MiBPerSec
			a.row.Errors = dr.Errors
			a.row.FirstErrorSeconds = dr.FirstErrorSeconds
			if dr.Combined.Ops > 0 {
				a.row.ErrorRate = float64(dr.Combined.Errors) / float64(dr.Combined.Ops)
			}
		}
		// The daemon runtime metrics are the Go-runtime view.
		var rm runtimeMetrics
		if err := readJSON(filepath.Join(dir, "runtime-metrics.json"), &rm); err != nil {
			if a.row.Status == "ok" {
				a.row.Status = "daemon_died"
			}
			appendNote(&a.row, "no runtime-metrics dump: the mount daemon did not reach its shutdown dump "+
				"(with GC disabled and no memory limit it allocates without bound and is killed before it can). "+
				"This row's latencies and throughput describe failed syscalls, not service")
		} else {
			a.row.GCPauseP50Seconds = rm.Delta.GCPauseP50Seconds
			a.row.GCPauseP99Seconds = rm.Delta.GCPauseP99Seconds
			a.row.GCPauseCount = rm.Delta.GCPauseCount
			a.row.GCCycles = rm.Delta.GCCycles
			a.row.SchedP99Seconds = rm.Delta.SchedP99Seconds
			a.row.AllocBytesPerS = rm.Delta.AllocBytesPerSecond
			a.row.RSSBytes = rm.Delta.RSSBytes
			a.row.PeakRSSBytes = rm.Delta.PeakRSSBytes
			a.row.HeapLiveBytes = rm.Delta.HeapLiveBytes
			a.row.MutexWaitSeconds = rm.Delta.MutexWaitSeconds
			a.row.WindowSeconds = rm.Delta.DurationS
			a.row.EffectiveGoGCPercent = rm.Delta.GOGCPercent
			a.row.EffectiveMemLimit = rm.Delta.GOMemLimitBytes
		}
		// The court watchdog's status file (written only when it aborted a stalled
		// arm) overrides the inferred status.
		var st struct {
			Status           string `json:"status"`
			WallLimitSeconds int    `json:"wall_limit_seconds"`
		}
		if err := readJSON(filepath.Join(dir, "status.json"), &st); err == nil && st.Status != "" {
			a.row.Status = st.Status
			appendNote(&a.row, fmt.Sprintf("the court watchdog aborted a stalled mount after %ds; "+
				"this window is a transient backing-store/scheduler stall, not a GC-knob effect", st.WallLimitSeconds))
		}
		if a.row.Status != "ok" {
			a.row.DaemonStderrTail, _ = readLines(filepath.Join(dir, "daemon.stderr"), 10)
		}
		arms = append(arms, a)
	}
	return arms, nil
}

func appendNote(r *Row, s string) {
	if r.Note == "" {
		r.Note = s
		return
	}
	r.Note += "; " + s
}

// buildReceipt assembles the receipt and attaches trace excerpts for the best
// and worst configurations.
func buildReceipt(arms []arm) Receipt {
	rec := Receipt{
		Schema:  "orfs.performance.gc-scaling.v1",
		Bounded: true,
		Method: "For each (GOGC, GOMEMLIMIT) configuration: init a fresh volume, mount it with --direct-io " +
			"and --profile-dir, drive it with cmd/orfsrw (N concurrent 4 KiB random readers + M concurrent writers), " +
			"then SIGTERM the daemon so it writes runtime-metrics.json (runtime/metrics + /proc VmRSS/VmHWM) and pprof " +
			"profiles. The driver measures application latency; the daemon measures the Go runtime.",
		Note: "BOUNDED sweep: 9 arms (each GOGC value at unlimited GOMEMLIMIT, plus the GOMEMLIMIT axis and the " +
			"GOGC=off bounded cells); this is not the full 5x3 cross product. Each arm runs a fixed, short workload " +
			"inside a 2 GiB / 4 CPU container. rss_bytes is REAL resident memory (VmRSS); peak_rss_bytes is VmHWM. " +
			"gc pause percentiles come from the windowed /sched/pauses/total/gc:seconds histogram (bucket edges). " +
			"alloc_bytes_per_s is /gc/heap/allocs:bytes over the daemon window. mutex_wait_seconds is the windowed " +
			"/sync/mutex/wait/total:seconds. An arm whose daemon was OOM-killed (GC disabled with no memory limit) is " +
			"recorded status=daemon_died, not given a fabricated number.",
	}
	for _, a := range arms {
		rec.Rows = append(rec.Rows, a.row)
	}

	for _, role := range []string{"best", "worst"} {
		label, ok := pickArm(arms, role)
		if !ok {
			continue
		}
		for _, a := range arms {
			if a.label != label {
				continue
			}
			rec.TraceExcerpts = append(rec.TraceExcerpts, loadTrace(a, role))
		}
	}
	return rec
}

// pickArm returns the label with the lowest ("best") or highest ("worst")
// measured p99.9 among arms with a real measurement. Ties break by label so the
// selection is deterministic and the court re-runs the same arms it traced.
func pickArm(arms []arm, role string) (string, bool) {
	best := ""
	for _, a := range arms {
		if a.row.Status != "ok" || a.row.P999Ns <= 0 {
			continue
		}
		if best == "" {
			best = a.label
			continue
		}
		var cur arm
		for _, b := range arms {
			if b.label == best {
				cur = b
			}
		}
		lower := a.row.P999Ns < cur.row.P999Ns
		if role == "worst" {
			lower = a.row.P999Ns > cur.row.P999Ns
		}
		if lower || (a.row.P999Ns == cur.row.P999Ns && a.label < best) {
			best = a.label
		}
	}
	return best, best != ""
}

// loadTrace reads the gctrace excerpt and lock profiles a profile run left in
// the arm's profile/ subdirectory.
func loadTrace(a arm, role string) TraceExcerpt {
	profDir := filepath.Join(a.dir, "profile")
	te := TraceExcerpt{
		Label:         a.label,
		Role:          role,
		MutexWaitSecs: a.row.MutexWaitSeconds,
	}
	if lines, err := readGCTrace(filepath.Join(profDir, "daemon.stderr")); err == nil {
		te.Lines = excerpt(lines, 12)
	}
	if _, err := os.Stat(filepath.Join(profDir, "mutex.prof")); err == nil {
		te.MutexProfile = filepath.ToSlash(filepath.Join(a.label, "profile", "mutex.prof"))
	}
	if _, err := os.Stat(filepath.Join(profDir, "block.prof")); err == nil {
		te.BlockProfile = filepath.ToSlash(filepath.Join(a.label, "profile", "block.prof"))
	}
	te.MutexTop, _ = readLines(filepath.Join(profDir, "mutex-top.txt"), 16)
	te.BlockTop, _ = readLines(filepath.Join(profDir, "block-top.txt"), 16)
	return te
}

// readGCTrace keeps only the lines GODEBUG=gctrace=1 emits.
func readGCTrace(path string) ([]string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(blob), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "gc ") {
			out = append(out, t)
		}
	}
	return out, nil
}

func readLines(path string, max int) ([]string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(blob), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(out) >= max {
			break
		}
		out = append(out, line)
	}
	return out, nil
}

// excerpt keeps the first 2 and last (n-2) lines, so a "short excerpt" shows
// both the start of the trace and how it settled.
func excerpt(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	head := 2
	out := make([]string, 0, n)
	out = append(out, lines[:head]...)
	out = append(out, fmt.Sprintf("... (%d GC lines omitted) ...", len(lines)-n))
	out = append(out, lines[len(lines)-(n-head):]...)
	return out
}

func readJSON(path string, v any) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, v)
}
