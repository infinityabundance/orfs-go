package namespace_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// allocTarget is the achieved steady-state authoritative read allocation floor.
// Read returns a []byte the caller owns, so one allocation (the destination) is
// unavoidable; the read planner must add none. A regression above this fails the
// court.
const allocTarget = 1

// allocBefore is the allocation count measured on the unoptimised read path: the
// destination plus the fresh []Piece that PlanRead allocated on every read (a
// 4 KiB read accounted 4096 + 24 = 4120 B/op). Recorded from the measurement
// taken before the caller-buffer change, not invented.
const allocBefore = 2

// allocsReceipt is evidence/performance/allocs.json (schema
// orfs.performance.allocs.v1).
type allocsReceipt struct {
	Schema     string `json:"schema"`
	Before     int64  `json:"before"`
	After      int64  `json:"after"`
	NsPerOp    int64  `json:"ns_per_op"`
	BytesPerOp int64  `json:"bytes_per_op"`
	Note       string `json:"note"`
}

// TestAuthoritativeReadAllocs is the per-read allocation court. It measures the
// steady-state authoritative 4 KiB read over a 1 MiB single-extent file and fails
// if the per-read allocation count regresses above allocTarget, so a change that
// reintroduces a per-read planner allocation fails here. With ORFS_EVIDENCE=1 it
// writes evidence/performance/allocs.json.
func TestAuthoritativeReadAllocs(t *testing.T) {
	res := testing.Benchmark(BenchmarkAuthoritativeRead4K)
	allocs := res.AllocsPerOp()
	t.Logf("authoritative 4 KiB read: %s (before: %d allocs/op, %d B/op)",
		res.String(), allocBefore, 4120)

	if allocs > allocTarget {
		t.Fatalf("authoritative 4 KiB read allocates %d allocs/op, want <= %d: %s",
			allocs, allocTarget, res.String())
	}
	if allocs < 1 {
		t.Fatalf("authoritative 4 KiB read reported %d allocs/op; the returned []byte must be one allocation", allocs)
	}

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		if err := writeAllocsEvidence(res, allocBefore, allocs); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
	}
}

func writeAllocsEvidence(res testing.BenchmarkResult, before, after int64) error {
	out := allocsReceipt{
		Schema:     "orfs.performance.allocs.v1",
		Before:     before,
		After:      after,
		NsPerOp:    res.NsPerOp(),
		BytesPerOp: res.AllocedBytesPerOp(),
		Note: "Steady-state authoritative 4 KiB read (no manifest registered) over a 1 MiB single-extent file on " +
			"a filesystem-backed volume, reads walked across the whole file. before=2 allocs/op: the destination " +
			"[]byte plus the fresh []Piece that PlanRead allocated (4120 B/op). after>=1 alloc: the returned " +
			"[]byte alone (4096 B/op); ExtentMap.PlanReadInto now fills a small caller-owned array, so the common " +
			"single-extent read allocates nothing for the plan. bytes_per_op is allocated bytes per op (the " +
			"benchmark's B/op), not bytes read. The reference reached 2 allocs/read; this reaches 1.",
	}
	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join("..", "..", "evidence", "performance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "allocs.json"), append(blob, '\n'), 0o644)
}
