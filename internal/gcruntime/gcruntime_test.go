package gcruntime_test

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/gcruntime"
)

// TestCaptureReadsRealMetrics proves the collector reads the running toolchain's
// metrics without a fabricated value: allocation is monotonic, GOMAXPROCS is
// positive, and a second capture after allocating bytes never moves backwards.
func TestCaptureReadsRealMetrics(t *testing.T) {
	a := gcruntime.Capture()
	if a.GOMAXPROCS == 0 {
		t.Errorf("GOMAXPROCS metric missing (Missing=%v)", a.Missing)
	}
	if a.AllocBytes == 0 {
		t.Errorf("alloc counter is zero right after allocating (Missing=%v)", a.Missing)
	}

	// Allocate something and confirm the cumulative alloc counter advanced.
	sink := make([]byte, 4<<20)
	for i := range sink {
		sink[i] = byte(i)
	}
	b := gcruntime.Capture()
	if b.AllocBytes < a.AllocBytes {
		t.Errorf("alloc counter went backwards: %d -> %d", a.AllocBytes, b.AllocBytes)
	}
	if b.UnixNanos < a.UnixNanos {
		t.Errorf("snapshot clock went backwards")
	}
}

// TestDeltaBetweenWindowsCounters proves rates describe the interval, not
// process life: a known alloc delta over a known window yields the exact rate.
func TestDeltaBetweenWindowsCounters(t *testing.T) {
	start := gcruntime.Snapshot{
		UnixNanos:  1_000_000_000,
		AllocBytes: 100,
		GCCycles:   2,
		RSSBytes:   5 << 20,
	}
	end := gcruntime.Snapshot{
		UnixNanos:  3_000_000_000, // 2s window
		AllocBytes: 1100,
		GCCycles:   7,
		RSSBytes:   9 << 20,
	}
	d := gcruntime.DeltaBetween(start, end)
	if !almost(d.DurationS, 2, 1e-9) {
		t.Errorf("duration = %v, want 2", d.DurationS)
	}
	if d.AllocBytes != 1000 {
		t.Errorf("alloc delta = %d, want 1000", d.AllocBytes)
	}
	if !almost(d.AllocBytesPerSecond, 500, 1e-9) {
		t.Errorf("alloc rate = %v, want 500", d.AllocBytesPerSecond)
	}
	if d.GCCycles != 5 {
		t.Errorf("gc cycles delta = %d, want 5", d.GCCycles)
	}
	if d.RSSBytes != 9<<20 {
		t.Errorf("rss = %d, want %d", d.RSSBytes, 9<<20)
	}
}

// TestDeltaClampsResets proves a counter that appears to reset never yields a
// negative delta or a non-finite rate.
func TestDeltaClampsResets(t *testing.T) {
	start := gcruntime.Snapshot{UnixNanos: 10, AllocBytes: 1000, MutexWaitSeconds: 5}
	end := gcruntime.Snapshot{UnixNanos: 20, AllocBytes: 100, MutexWaitSeconds: 1}
	d := gcruntime.DeltaBetween(start, end)
	if d.AllocBytes != 0 || d.MutexWaitSeconds != 0 {
		t.Errorf("reset not clamped: alloc=%d mutex=%v", d.AllocBytes, d.MutexWaitSeconds)
	}
}

// TestHistogramDiffAndQuantile exercises the interval histogram: two cumulative
// snapshots whose difference concentrates all weight in one bucket must yield
// that bucket's upper edge for p50 and p99.
func TestHistogramDiffAndQuantile(t *testing.T) {
	bounds := []float64{0, 1e-6, 1e-5, 1e-4, 1e-3, math.Inf(1)}
	// start: everything already in the first buckets (noise before the window).
	start := gcruntime.Histogram{Bounds: bounds, Counts: []uint64{3, 0, 0, 0, 0}}
	start.Count = 3
	// end: window added 100 samples into bucket index 2 ([1e-5, 1e-4)).
	end := gcruntime.Histogram{Bounds: bounds, Counts: []uint64{3, 0, 100, 0, 0}}
	end.Count = 103

	w := gcruntime.DiffHistograms(start, end)
	if w.Count != 100 {
		t.Fatalf("window count = %d, want 100", w.Count)
	}
	if got := gcruntime.Quantile(w, 0.50); !almost(got, 1e-4, 1e-15) {
		t.Errorf("p50 = %v, want upper edge 1e-4", got)
	}
	if got := gcruntime.Quantile(w, 0.99); !almost(got, 1e-4, 1e-15) {
		t.Errorf("p99 = %v, want upper edge 1e-4", got)
	}
	if got := gcruntime.Quantile(w, 0); !almost(got, 1e-4, 1e-15) {
		t.Errorf("p0 = %v, want upper edge 1e-4", got)
	}
}

// TestQuantileTopBucketStaysFinite proves the open-ended top bucket is reported
// at its finite lower edge, never +Inf (which JSON cannot encode).
func TestQuantileTopBucketStaysFinite(t *testing.T) {
	h := gcruntime.Histogram{
		Bounds: []float64{0, 1, 2, math.Inf(1)},
		Counts: []uint64{0, 0, 50},
		Count:  50,
	}
	got := gcruntime.Quantile(h, 0.99)
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Fatalf("quantile = %v, must be finite", got)
	}
	if got != 2 {
		t.Errorf("quantile = %v, want finite lower edge 2", got)
	}
}

// TestQuantileEmptyIsZero proves an empty window reports 0 rather than a panic.
func TestQuantileEmptyIsZero(t *testing.T) {
	if got := gcruntime.Quantile(gcruntime.Histogram{}, 0.99); got != 0 {
		t.Errorf("empty quantile = %v, want 0", got)
	}
}

// TestHistogramJSONRoundTrip proves the ±Inf bucket edges survive a JSON round
// trip, since a real runtime histogram bounds array carries them and
// encoding/json would otherwise refuse the document.
func TestHistogramJSONRoundTrip(t *testing.T) {
	in := gcruntime.Histogram{
		Bounds: []float64{math.Inf(-1), 0, 1e-6, math.Inf(1)},
		Counts: []uint64{0, 1, 2},
		Count:  3,
	}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out gcruntime.Histogram
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Bounds) != len(in.Bounds) {
		t.Fatalf("bounds length %d, want %d", len(out.Bounds), len(in.Bounds))
	}
	if !math.IsInf(out.Bounds[0], -1) || !math.IsInf(out.Bounds[3], 1) {
		t.Errorf("infinities lost: %v", out.Bounds)
	}
	if out.Bounds[1] != 0 || out.Bounds[2] != 1e-6 {
		t.Errorf("finite edges changed: %v", out.Bounds)
	}
}

// TestWriteProfileDirMarshals proves the full artifact (with our own runtime as
// the subject) marshals cleanly, carries the schema, and produces the profiles.
func TestWriteProfileDirMarshals(t *testing.T) {
	start := gcruntime.Capture()
	time.Sleep(5 * time.Millisecond)
	// Churn the heap so the window has something to describe.
	sink := make([][]byte, 0, 64)
	for i := 0; i < 64; i++ {
		sink = append(sink, make([]byte, 64*1024))
	}
	end := gcruntime.Capture()

	dir := t.TempDir()
	if err := gcruntime.WriteProfileDir(dir, start, end, gcruntime.Options{Note: "test"}); err != nil {
		t.Fatalf("WriteProfileDir: %v", err)
	}

	blob, err := os.ReadFile(dir + "/runtime-metrics.json")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	var doc struct {
		Schema string `json:"schema"`
		Delta  struct {
			DurationS           float64 `json:"duration_seconds"`
			AllocBytesPerSecond float64 `json:"alloc_bytes_per_second"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("artifact is not valid JSON: %v", err)
	}
	if doc.Schema != gcruntime.Schema {
		t.Errorf("schema = %q, want %q", doc.Schema, gcruntime.Schema)
	}
	if doc.Delta.DurationS <= 0 {
		t.Errorf("duration = %v, want > 0", doc.Delta.DurationS)
	}
	if math.IsNaN(doc.Delta.AllocBytesPerSecond) || math.IsInf(doc.Delta.AllocBytesPerSecond, 0) {
		t.Errorf("alloc rate is not finite: %v", doc.Delta.AllocBytesPerSecond)
	}
	if _, err := os.ReadFile(dir + "/heap.prof"); err != nil {
		t.Errorf("heap.prof missing: %v", err)
	}
	_ = sink
}

func almost(a, b, eps float64) bool { return math.Abs(a-b) <= eps }
