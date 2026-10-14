package log

import (
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/capacity"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// TestRuntimeCountsBarriersAndBatches proves the log records one fsync-latency
// observation per durability barrier and the group-commit batch sizes, which is
// what `orfs metrics` exposes as orfs_fsync_latency_seconds and
// orfs_group_commit_batch_size.
func TestRuntimeCountsBarriersAndBatches(t *testing.T) {
	const n = 32
	backing := t.TempDir()
	l, err := Create(backing, testGenesis(), WithGroupCommit(n, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer l.Close()

	_, errs := publishConcurrent(t, l, n)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("writer %d: %v", i, e)
		}
	}

	rt := l.Runtime()
	if rt.FsyncCount() == 0 {
		t.Fatal("no durability barrier was observed")
	}
	if rt.BatchCount == 0 {
		t.Fatal("no group-commit batch was recorded")
	}
	if rt.BatchSum != n {
		t.Fatalf("batch sum = %d, want %d", rt.BatchSum, n)
	}
	if rt.BatchAvg() < 1 {
		t.Fatalf("batch avg = %v, want >= 1", rt.BatchAvg())
	}
	// One barrier per batch, so the barrier count equals the batch count.
	if rt.FsyncCount() != rt.BatchCount {
		t.Fatalf("barriers %d != batches %d", rt.FsyncCount(), rt.BatchCount)
	}
	if rt.FsyncQuantile(0.5) <= 0 {
		t.Fatalf("p50 barrier latency = %v, want > 0", rt.FsyncQuantile(0.5))
	}
}

// TestRuntimeCountsBreach proves a capacity refusal is counted in the log's
// runtime snapshot (the source of orfs_breach_total).
func TestRuntimeCountsBreach(t *testing.T) {
	backing := t.TempDir()
	l, err := Create(backing, testGenesis())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer l.Close()

	free, ok := capacity.BackingAvailableBytes(Dir(backing))
	if !ok {
		t.Skip("backing free space unmeasurable")
	}
	// Reserve far above free space: zero headroom, so the next publish is refused.
	l.Capacity().SetPolicy(capacity.Policy{ReserveBytes: free + (1 << 20)}, Dir(backing), l.CommittedRoot())

	if _, err := l.Publish([]*record.Record{inodeCreate(9)}); err == nil {
		t.Fatal("expected a capacity refusal")
	}
	if got := l.Runtime().BreachTotal(); got != 1 {
		t.Fatalf("breach total = %d, want 1", got)
	}
}
