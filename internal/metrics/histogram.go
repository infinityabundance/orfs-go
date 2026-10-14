package metrics

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// FsyncBounds are the histogram bucket upper bounds (seconds) for the durability
// barrier. They span 100 ns to sixty seconds so an fsync on anything from a
// fast NVMe (sub-microsecond) to a slow spindle (tens of milliseconds) lands in a
// meaningful bucket. The bounds are finite so the histogram serializes; an
// observation beyond the last bound is counted in the final bucket (a bounded
// estimate, never an infinitude).
var FsyncBounds = []float64{
	1e-7, 2.5e-7, 5e-7, 1e-6, 2e-6, 5e-6, 1e-5, 2e-5, 5e-5,
	1e-4, 2e-4, 5e-4, 1e-3, 2e-3, 5e-3,
	1e-2, 2e-2, 5e-2, 1e-1, 2e-1, 5e-1,
	1, 2, 5, 10, 30, 60,
}

// Histogram is a fixed-bucket latency histogram with a bounded quantile estimate.
// It is safe for concurrent use.
type Histogram struct {
	mu     sync.Mutex
	bounds []float64
	counts []uint64
	count  uint64
	sum    float64
}

// NewHistogram builds a histogram over the given strictly-increasing bounds. The
// bounds slice is copied.
func NewHistogram(bounds []float64) *Histogram {
	b := append([]float64(nil), bounds...)
	return &Histogram{bounds: b, counts: make([]uint64, len(b))}
}

// Observe records one observation (seconds).
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += v
	for i, ub := range h.bounds {
		if v <= ub {
			h.counts[i]++
			return
		}
	}
	// No finite bound exceeded: the final bucket always absorbs it.
	h.counts[len(h.counts)-1]++
}

// Quantile returns the bounded estimate of the q-quantile (0..1): the upper edge
// of the bucket the rank falls in. When the rank lands in a +Inf bucket the
// largest finite bound is returned so the estimate stays finite and honest.
func (h *Histogram) Quantile(q float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return quantileFrom(h.bounds, h.counts, h.count, q)
}

// Count is the number of observations.
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Sum is the sum of observations.
func (h *Histogram) Sum() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sum
}

// Snapshot renders the histogram for persistence.
func (h *Histogram) Snapshot() HistogramSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HistogramSnapshot{
		Bounds: append([]float64(nil), h.bounds...),
		Counts: append([]uint64(nil), h.counts...),
		Count:  h.count,
		Sum:    h.sum,
	}
}

// HistogramSnapshot is a serializable histogram.
type HistogramSnapshot struct {
	Bounds []float64 `json:"bounds"`
	Counts []uint64  `json:"counts"`
	Count  uint64    `json:"count"`
	Sum    float64   `json:"sum"`
}

// Quantile estimates the q-quantile from the snapshot.
func (s HistogramSnapshot) Quantile(q float64) float64 {
	return quantileFrom(s.Bounds, s.Counts, s.Count, q)
}

func quantileFrom(bounds []float64, counts []uint64, count uint64, q float64) float64 {
	if count == 0 || len(bounds) == 0 || len(counts) == 0 {
		return 0
	}
	rank := uint64(q * float64(count))
	if rank < 1 {
		rank = 1
	}
	var cum uint64
	for i, c := range counts {
		cum += c
		if cum >= rank {
			if math.IsInf(bounds[i], 1) {
				return lastFinite(bounds)
			}
			return bounds[i]
		}
	}
	return lastFinite(bounds)
}

func lastFinite(bounds []float64) float64 {
	for i := len(bounds) - 1; i >= 0; i-- {
		if !math.IsInf(bounds[i], 1) {
			return bounds[i]
		}
	}
	return 0
}

// Runtime is the process-scoped set of counters a live Log accumulates: capacity
// refusals, the durability-barrier histogram, and group-commit batch sizes. It is
// what a mounted daemon persists so an offline `orfs metrics` can report a real
// p50/p99 rather than a zero.
type Runtime struct {
	Breach     map[string]uint64  `json:"breach_total,omitempty"`
	Fsync      *HistogramSnapshot `json:"fsync,omitempty"`
	BatchLast  uint64             `json:"group_commit_batch_last"`
	BatchCount uint64             `json:"group_commit_batch_count"`
	BatchSum   uint64             `json:"group_commit_batch_sum"`
}

// FsyncQuantile is the bounded q-quantile of the barrier latency, in seconds.
func (r Runtime) FsyncQuantile(q float64) float64 {
	if r.Fsync == nil {
		return 0
	}
	return r.Fsync.Quantile(q)
}

// FsyncCount is the number of barrier observations.
func (r Runtime) FsyncCount() uint64 {
	if r.Fsync == nil {
		return 0
	}
	return r.Fsync.Count
}

// FsyncSum is the sum of barrier observations, in seconds.
func (r Runtime) FsyncSum() float64 {
	if r.Fsync == nil {
		return 0
	}
	return r.Fsync.Sum
}

// BatchAvg is the mean group-commit batch size (0 when no batch was observed).
func (r Runtime) BatchAvg() float64 {
	if r.BatchCount == 0 {
		return 0
	}
	return float64(r.BatchSum) / float64(r.BatchCount)
}

// BreachTotal sums every refusal reason.
func (r Runtime) BreachTotal() uint64 {
	var total uint64
	for _, v := range r.Breach {
		total += v
	}
	return total
}

// SnapshotFileName is the derived, non-authoritative metrics sidecar a mounted
// daemon writes so its runtime counters survive the process. It sits beside
// volume.meta; it is never read by recovery and deleting it changes nothing.
const SnapshotFileName = "metrics.json"

// SnapshotSchema is the sidecar schema.
const SnapshotSchema = "orfs.metrics.snapshot.v1"

// Snapshot is the persisted runtime state.
type Snapshot struct {
	Schema     string  `json:"schema"`
	UUID       string  `json:"uuid,omitempty"`
	CapturedAt uint64  `json:"captured_at_unix,omitempty"`
	Runtime    Runtime `json:"runtime"`
}

// SaveSnapshot writes the sidecar atomically (temp file + rename).
func SaveSnapshot(backing string, snap Snapshot) error {
	if snap.Schema == "" {
		snap.Schema = SnapshotSchema
	}
	blob, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	path := filepath.Join(backing, SnapshotFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot reads the sidecar, returning ok=false when it is absent.
func LoadSnapshot(backing string) (*Snapshot, bool) {
	blob, err := os.ReadFile(filepath.Join(backing, SnapshotFileName))
	if err != nil {
		return nil, false
	}
	var snap Snapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		return nil, false
	}
	return &snap, true
}
