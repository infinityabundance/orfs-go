package metrics

import (
	"math"
	"testing"
)

// TestHistogramQuantiles checks the bounded quantile estimate: the rank falls in
// a bucket and the estimate is that bucket's upper edge.
func TestHistogramQuantiles(t *testing.T) {
	bounds := []float64{0.001, 0.01, 0.1, 1, math.Inf(1)}
	h := NewHistogram(bounds)
	// 100 observations: 50 in (0.001,0.01], 50 in (0.1,1].
	for i := 0; i < 50; i++ {
		h.Observe(0.005)
	}
	for i := 0; i < 50; i++ {
		h.Observe(0.5)
	}

	if h.Count() != 100 {
		t.Fatalf("count = %d, want 100", h.Count())
	}
	if got := h.Quantile(0.5); got != 0.01 {
		t.Fatalf("p50 = %v, want 0.01", got)
	}
	if got := h.Quantile(0.99); got != 1 {
		t.Fatalf("p99 = %v, want 1", got)
	}
	if got := h.Quantile(1.0); got != 1 {
		t.Fatalf("p100 = %v, want 1", got)
	}
}

// TestHistogramQuantileNeverInfinite proves the +Inf bucket is reported as the
// largest finite edge, so a quantile is never emitted as +Inf.
func TestHistogramQuantileNeverInfinite(t *testing.T) {
	h := NewHistogram([]float64{0.001, 0.01, math.Inf(1)})
	for i := 0; i < 10; i++ {
		h.Observe(100)
	}
	if got := h.Quantile(0.99); math.IsInf(got, 1) {
		t.Fatalf("quantile is +Inf, want the largest finite edge")
	}
	if got := h.Quantile(0.99); got != 0.01 {
		t.Fatalf("p99 = %v, want 0.01", got)
	}
}

// TestHistogramEmpty proves an empty histogram reports 0 rather than a lie.
func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram(FsyncBounds)
	if h.Count() != 0 || h.Sum() != 0 || h.Quantile(0.99) != 0 {
		t.Fatalf("empty histogram is not zero: count=%d sum=%v", h.Count(), h.Sum())
	}
}

// TestHistogramSnapshotRoundTrip proves the persisted form reproduces the same
// quantiles and totals.
func TestHistogramSnapshotRoundTrip(t *testing.T) {
	h := NewHistogram([]float64{0.0001, 0.001, 0.01, math.Inf(1)})
	for i := 0; i < 1000; i++ {
		h.Observe(0.0005)
	}
	for i := 0; i < 5; i++ {
		h.Observe(0.5)
	}
	snap := h.Snapshot()
	if snap.Count != 1005 {
		t.Fatalf("snapshot count = %d", snap.Count)
	}
	if got, want := snap.Quantile(0.5), h.Quantile(0.5); got != want {
		t.Fatalf("snapshot p50 = %v, live %v", got, want)
	}
	if got, want := snap.Quantile(0.99), h.Quantile(0.99); got != want {
		t.Fatalf("snapshot p99 = %v, live %v", got, want)
	}
}

// TestRuntimeHelpers checks the derived runtime aggregates used by the exposition.
func TestRuntimeHelpers(t *testing.T) {
	h := NewHistogram(FsyncBounds)
	h.Observe(0.002)
	h.Observe(0.004)
	snap := h.Snapshot()
	rt := Runtime{
		Breach:     map[string]uint64{"backing_reserve": 2, "authoritative": 1},
		Fsync:      &snap,
		BatchLast:  1,
		BatchCount: 4,
		BatchSum:   8,
	}
	if rt.BreachTotal() != 3 {
		t.Fatalf("breach total = %d, want 3", rt.BreachTotal())
	}
	if rt.FsyncCount() != 2 {
		t.Fatalf("fsync count = %d, want 2", rt.FsyncCount())
	}
	if rt.BatchAvg() != 2 {
		t.Fatalf("batch avg = %v, want 2", rt.BatchAvg())
	}
}

// TestSnapshotSidecarRoundTrip proves the persisted snapshot survives a write and
// read, and that a missing sidecar is reported absent rather than zero-filled.
func TestSnapshotSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := LoadSnapshot(dir); ok {
		t.Fatal("LoadSnapshot found a snapshot where none exists")
	}
	h := NewHistogram(FsyncBounds)
	h.Observe(0.003)
	snap := h.Snapshot()
	in := Snapshot{
		Schema:     SnapshotSchema,
		UUID:       "abcd",
		CapturedAt: 12345,
		Runtime:    Runtime{Breach: map[string]uint64{"hard_quota": 1}, Fsync: &snap, BatchLast: 3, BatchCount: 2, BatchSum: 5},
	}
	if err := SaveSnapshot(dir, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, ok := LoadSnapshot(dir)
	if !ok {
		t.Fatal("LoadSnapshot did not find the snapshot")
	}
	if out.Schema != SnapshotSchema || out.UUID != "abcd" || out.CapturedAt != 12345 {
		t.Fatalf("snapshot identity lost: %+v", out)
	}
	if out.Runtime.Breach["hard_quota"] != 1 || out.Runtime.FsyncCount() != 1 {
		t.Fatalf("snapshot runtime lost: %+v", out.Runtime)
	}
}
