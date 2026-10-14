package fcache_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/fcache"
)

// scratchValue is the placeholder payload the court admits: the scan-resistance
// court measures residency, not payload semantics.
type scratchValue = struct{}

// policyArm pairs a policy name and note with a fresh cache instance.
type policyArm struct {
	name  string
	note  string
	cache fcache.Cache[uint64, scratchValue]
}

// scratchArms builds one fresh instance of every policy.
func scratchArms(capacity int) []policyArm {
	return []policyArm{
		{
			name:  "s3fifo",
			note:  "S3-FIFO admission: small probationary FIFO (~10% of capacity) with a frequency bit, main FIFO (~90%), and a keys-only ghost queue; single-touch scan keys never reach main",
			cache: fcache.NewS3FIFO[uint64, scratchValue](capacity),
		},
		{
			name:  "fifo",
			note:  "insertion-ordered FIFO baseline (the pre-admission policy); a hit does not reorder the queue, so a scan flushes the hot set",
			cache: fcache.NewFIFO[uint64, scratchValue](capacity),
		},
		{
			name:  "lru",
			note:  "faithful LRU baseline; a hit promotes the key to most-recently-used and a miss evicts the least-recently-used key",
			cache: fcache.NewLRU[uint64, scratchValue](capacity),
		},
	}
}

// TestScanResistance replays one identical scan-heavy trace through the S3-FIFO
// admission cache and the FIFO and LRU baselines, asserting the point of the
// court: admission is never worse than LRU on the scan-heavy trace, and it beats
// the insertion-ordered FIFO that the scan destroys.
//
// The trace is primed with warm-up rounds first; only the scan phase is
// measured, so the reported hit rates are the scan-phase rates. When
// ORFS_EVIDENCE=1 the court writes evidence/performance/cache.json.
func TestScanResistance(t *testing.T) {
	w := fcache.ReferenceWorkload()
	warmup := w.WarmupTrace()
	scan := w.ScanTrace()

	arms := scratchArms(w.Capacity)
	stats := make(map[string]fcache.Stats, len(arms))
	for i := range arms {
		fcache.Replay(arms[i].cache, warmup, scratchValue{})        // prime the hot set
		before := arms[i].cache.Stats()                             // snapshot after warm-up
		after := fcache.Replay(arms[i].cache, scan, scratchValue{}) // measure the scan
		stats[arms[i].name] = fcache.Stats{
			Hits:   after.Hits - before.Hits,
			Misses: after.Misses - before.Misses,
		}
	}

	rate := func(name string) float64 { return stats[name].Rate() }

	t.Logf("scan-heavy trace: %d touches (%d cold + %d hot), capacity %d, hot set %d",
		len(scan), w.Scan*w.Passes, w.Scan*w.Passes*w.HotPerStep, w.Capacity, w.Hot)
	for _, a := range arms {
		s := stats[a.name]
		t.Logf("%-7s hits %6d  misses %6d  hit rate %.4f", a.name, s.Hits, s.Misses, s.Rate())
		if got := a.cache.Len(); got > w.Capacity {
			t.Fatalf("%s resident keys %d exceed capacity %d", a.name, got, w.Capacity)
		}
	}

	// The whole point: admission is scan resistant, so it is never worse than
	// LRU and strictly better than the FIFO the scan flushes.
	if rate("s3fifo")+1e-9 < rate("lru") {
		t.Fatalf("S3-FIFO hit rate %.4f < LRU %.4f on the scan-heavy trace", rate("s3fifo"), rate("lru"))
	}
	if rate("fifo")+1e-9 >= rate("s3fifo") {
		t.Fatalf("the scan did not cost FIFO more than admission (fifo %.4f >= s3fifo %.4f)",
			rate("fifo"), rate("s3fifo"))
	}

	if counters, ok := fcache.Counters(arms[0].cache); ok {
		t.Logf("s3fifo admission: promotions %d, evictions %d, ghost hits %d",
			counters.Promotions, counters.Evictions, counters.GhostHits)
		if counters.Promotions == 0 {
			t.Fatal("S3-FIFO promoted nothing into main during the scan")
		}
	}

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeEvidence(t, w, arms, stats)
	}
}

// TestDeterministicReplay proves every policy is deterministic: replaying the
// identical trace through two fresh instances yields identical stats.
func TestDeterministicReplay(t *testing.T) {
	w := fcache.ReferenceWorkload()
	trace := w.Trace()

	first := scratchArms(w.Capacity)
	second := scratchArms(w.Capacity)
	for i := range first {
		a := fcache.Replay(first[i].cache, trace, scratchValue{})
		b := fcache.Replay(second[i].cache, trace, scratchValue{})
		if a != b {
			t.Fatalf("%s is not deterministic: %+v != %+v", first[i].name, a, b)
		}
	}
}

// TestCacheBasics exercises the small interface contract directly.
func TestCacheBasics(t *testing.T) {
	ctors := []struct {
		name string
		ctor func(int) fcache.Cache[uint64, scratchValue]
	}{
		{"s3fifo", fcache.NewS3FIFO[uint64, scratchValue]},
		{"fifo", fcache.NewFIFO[uint64, scratchValue]},
		{"lru", fcache.NewLRU[uint64, scratchValue]},
	}
	for _, tc := range ctors {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.ctor(2)
			if _, ok := c.Get(7); ok {
				t.Fatal("empty cache reported a hit")
			}
			c.Put(7, scratchValue{})
			if _, ok := c.Get(7); !ok {
				t.Fatal("resident key reported a miss")
			}
			if c.Len() != 1 {
				t.Fatalf("Len = %d, want 1", c.Len())
			}
			c.Put(8, scratchValue{})
			c.Put(9, scratchValue{}) // exceeds capacity 2, one key must go
			if c.Len() != 2 {
				t.Fatalf("Len = %d, want 2 after overflow", c.Len())
			}
			s := c.Stats()
			if s.Total() != 2 {
				t.Fatalf("Total lookups = %d, want 2", s.Total())
			}
		})
	}
	// A zero-capacity cache never stores and always misses.
	c := fcache.NewS3FIFO[uint64, scratchValue](0)
	c.Put(1, scratchValue{})
	if _, ok := c.Get(1); ok || c.Len() != 0 {
		t.Fatal("zero-capacity cache stored a key")
	}
}

// receipt types, written only under ORFS_EVIDENCE=1.

type policyReceipt struct {
	Policy      string  `json:"policy"`
	Capacity    int     `json:"capacity"`
	TraceLength int     `json:"trace_length"`
	Hits        uint64  `json:"hits"`
	Misses      uint64  `json:"misses"`
	HitRate     float64 `json:"hit_rate"`
	Note        string  `json:"note"`
	Promotions  uint64  `json:"promotions,omitempty"`
	Evictions   uint64  `json:"evictions,omitempty"`
	GhostHits   uint64  `json:"ghost_hits,omitempty"`
}

type workloadReceipt struct {
	Capacity        int `json:"capacity"`
	HotObjects      int `json:"hot_objects"`
	ScanObjects     int `json:"scan_objects"`
	WarmupRounds    int `json:"warmup_rounds"`
	HotReadsPerStep int `json:"hot_reads_per_scan_step"`
	ScanPasses      int `json:"scan_passes"`
	TraceLength     int `json:"trace_length"`
}

type cacheReceipt struct {
	Schema   string          `json:"schema"`
	Workload workloadReceipt `json:"workload"`
	Policies []policyReceipt `json:"policies"`
	Note     string          `json:"note"`
}

// writeEvidence marshals the court's receipt to evidence/performance/cache.json.
func writeEvidence(t *testing.T, w fcache.Workload, arms []policyArm, stats map[string]fcache.Stats) {
	t.Helper()

	traceLen := len(w.ScanTrace())
	doc := cacheReceipt{
		Schema: "orfs.performance.cache.v1",
		Workload: workloadReceipt{
			Capacity:        w.Capacity,
			HotObjects:      w.Hot,
			ScanObjects:     w.Scan * w.Passes,
			WarmupRounds:    w.Warmup,
			HotReadsPerStep: w.HotPerStep,
			ScanPasses:      w.Passes,
			TraceLength:     traceLen,
		},
		Note: "Single-run, single-host hit-rate court over one identical scan-heavy key trace; " +
			"only the eviction policy differs. This is a hit-RATE measurement, not a latency one: " +
			"a higher hit rate is not a latency win. The reference court this reproduces measured " +
			"admission 95.2% vs FIFO 88.0% vs LRU 95.2% on the same trace (capacity 32, 20-key hot " +
			"set, 512-key scan). Admission matches LRU here because the hot set is re-referenced " +
			"every scan step; the win is over the insertion-ordered FIFO, which the scan flushes.",
	}
	for _, a := range arms {
		s := stats[a.name]
		pr := policyReceipt{
			Policy:      a.name,
			Capacity:    w.Capacity,
			TraceLength: traceLen,
			Hits:        s.Hits,
			Misses:      s.Misses,
			HitRate:     s.Rate(),
			Note:        a.note,
		}
		if counters, ok := fcache.Counters(a.cache); ok {
			pr.Promotions = counters.Promotions
			pr.Evictions = counters.Evictions
			pr.GhostHits = counters.GhostHits
		}
		doc.Policies = append(doc.Policies, pr)
	}

	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	dir := filepath.Join("..", "..", "evidence", "performance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	path := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s", path)
}
