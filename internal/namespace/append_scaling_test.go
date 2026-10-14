package namespace_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// appendScalingReceipt is the court's machine-readable output.
type appendScalingReceipt struct {
	Schema       string      `json:"schema"`
	RecordBytes  int         `json:"record_bytes"`
	WarmupOps    int         `json:"warmup_ops"`
	Backing      string      `json:"backing"`
	Points       []scalingPt `json:"points"`
	GrowthRatio  float64     `json:"growth_ratio"`
	Bounded      bool        `json:"bounded"`
	Note         string      `json:"note"`
	Method       string      `json:"method"`
	ExtentGrowth string      `json:"extent_growth"`
}

type scalingPt struct {
	Appends         int     `json:"appends"`
	Extents         int     `json:"extents"`
	MeanNsPerAppend float64 `json:"mean_ns_per_append"`
	TotalNs         int64   `json:"total_ns"`
}

// TestAppendScalingCourt is the structural-sharing court (Phase 27 parity). It
// appends fixed 4 KiB records at EOF to a single file, each append in its own
// authoritatively committed transaction, and measures the per-append cost as
// the file's retained extent count grows: 256, 512, 1024, 2048, 4096, 8192.
//
// Before structural sharing the extent map was a flat slice: cloning it and
// splicing a write were both O(E), so the per-append cost rose with the number
// of appends and an append run degraded quadratically — fatal for the audit/WORM
// workload, whose whole point is unbounded history. The persistent extent map
// makes clone O(1) and a write O(log E + k), so the cost must be flat.
//
// The court asserts the flatness bound and writes the measured curve to
// evidence/performance/append-scaling.json. It is a bounded laboratory
// measurement of this host, not an endurance test.
func TestAppendScalingCourt(t *testing.T) {
	const recordBytes = 4096
	const warmup = 64
	ns := []int{256, 512, 1024, 2048, 4096, 8192}

	rec := appendScalingReceipt{
		Schema:      "orfs.performance.append-scaling.v1",
		RecordBytes: recordBytes,
		WarmupOps:   warmup,
		Backing:     "container overlay (t.TempDir)",
		Method: "one 4 KiB append at EOF per transaction to a single file; equal durability " +
			"(each append is its own authoritatively committed transaction with its durability barrier); " +
			"mean is over appends after a fixed warm-up, on a fresh volume per point.",
		Note: "the per-append cost must not grow with the file's retained extent count; a rising curve " +
			"means the extent map has regressed to O(E) per write (structural sharing lost). " +
			"Measured on this host; run-to-run variance is expected.",
		ExtentGrowth: "extents grow 1:1 with appends by construction (each 4 KiB write's bytes live at a " +
			"distinct, non-contiguous log offset, so logical extents cannot coalesce); the cost must be " +
			"flat regardless.",
	}

	for _, n := range ns {
		backing := t.TempDir()
		v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 8 << 20})
		if err != nil {
			t.Fatalf("init: %v", err)
		}
		nsx := namespace.New(v)
		cred := namespace.Credential{}
		f, err := nsx.Create(cred, record.RootInode, "append.bin", 0o644, 0, 0)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		data := make([]byte, recordBytes)
		for i := 0; i < warmup && i < n; i++ {
			if _, err := nsx.Write(cred, f.ID, uint64(i)*recordBytes, data); err != nil {
				t.Fatalf("warmup write: %v", err)
			}
		}
		start := warmup
		if start > n {
			start = n
		}
		t0 := time.Now()
		for i := start; i < n; i++ {
			if _, err := nsx.Write(cred, f.ID, uint64(i)*recordBytes, data); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		el := time.Since(t0)
		ops := n - start
		if ops <= 0 {
			ops = 1
		}
		in, err := nsx.Getattr(f.ID)
		if err != nil {
			t.Fatalf("getattr: %v", err)
		}
		rec.Points = append(rec.Points, scalingPt{
			Appends:         n,
			Extents:         in.Extents.Count(),
			MeanNsPerAppend: float64(el.Nanoseconds()) / float64(ops),
			TotalNs:         el.Nanoseconds(),
		})
		if err := v.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	first := rec.Points[0].MeanNsPerAppend
	last := rec.Points[len(rec.Points)-1].MeanNsPerAppend
	rec.GrowthRatio = last / first
	rec.Bounded = rec.GrowthRatio <= 2.0

	for _, p := range rec.Points {
		t.Logf("append N=%-6d extents=%-6d mean=%-12s", p.Appends, p.Extents, time.Duration(p.MeanNsPerAppend))
	}
	t.Logf("growth ratio (N=8192 / N=256) = %.3fx (bound 2.0x)", rec.GrowthRatio)

	writeAppendScalingEvidence(t, rec)

	// Deterministic structural assertion: extents are 1:1 (the map is maximal
	// but cannot coalesce non-contiguous log bytes).
	for _, p := range rec.Points {
		if p.Extents != p.Appends {
			t.Fatalf("N=%d: %d extents, want %d (1:1)", p.Appends, p.Extents, p.Appends)
		}
	}
	// The flatness bound is the regression guard for the O(E) -> O(log E) fix.
	if !rec.Bounded {
		t.Fatalf("append cost grew %.3fx from N=256 to N=8192 (bound 2.0x): the extent map has "+
			"regressed to O(E) per write", rec.GrowthRatio)
	}
}

func writeAppendScalingEvidence(t *testing.T, rec appendScalingReceipt) {
	t.Helper()
	dir := filepath.Join("..", "..", "evidence", "performance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	path := filepath.Join(dir, "append-scaling.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
