package observe

import (
	"strings"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/fsck"
	"github.com/infinityabundance/orfs-go/internal/metrics"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

func hasFamily(set *metrics.Set, name string) bool {
	for _, f := range set.Families() {
		if f.Name == name {
			return true
		}
	}
	return false
}

// TestCollectFreshVolume proves an offline collection reports the on-disk facts
// and an explicit absence for the runtime families.
func TestCollectFreshVolume(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()

	snap, err := Collect(v, Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.PhysicalBytes == 0 {
		t.Fatal("physical bytes should be non-zero for a genesis volume")
	}
	if snap.CommittedRoot == 0 {
		t.Fatal("committed root should be non-zero for a genesis volume")
	}
	if snap.LogicalBytes != 0 {
		t.Fatalf("fresh volume has no regular files, logical = %d", snap.LogicalBytes)
	}
	if snap.Amplification != nil {
		t.Fatalf("amplification should be undefined with no logical bytes, got %v", *snap.Amplification)
	}
	if snap.RuntimeSource != "absent" {
		t.Fatalf("runtime source = %q, want absent", snap.RuntimeSource)
	}

	set := snap.Set()
	for _, name := range []string{
		"orfs_physical_bytes", "orfs_authoritative_bytes", "orfs_logical_bytes",
		"orfs_manifested_bytes", "orfs_amplification_ratio", "orfs_degraded",
		"orfs_breach_total", "orfs_quarantine_total", "orfs_verify_deep_failures",
		"orfs_fsync_latency_seconds", "orfs_group_commit_batch_size",
	} {
		if !hasFamily(set, name) {
			t.Fatalf("exposition is missing family %s", name)
		}
	}
	// A missing runtime source must be documented, never silently zero.
	if !strings.Contains(set.Text(), "Source: none present") {
		t.Fatal("exposition does not disclose the absent runtime source")
	}
}

// TestCollectMergesRuntimeAndVerify proves a live runtime snapshot and a deep
// verify report are folded into the collection.
func TestCollectMergesRuntimeAndVerify(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()

	h := metrics.NewHistogram(metrics.FsyncBounds)
	h.Observe(0.005)
	h.Observe(0.007)
	hs := h.Snapshot()
	rt := metrics.Runtime{
		Breach:     map[string]uint64{"backing_reserve": 2, "hard_quota": 1},
		Fsync:      &hs,
		BatchLast:  4,
		BatchCount: 3,
		BatchSum:   12,
	}
	rep := &fsck.Report{Checks: []fsck.Check{{Name: "ok", OK: true}, {Name: "bad", OK: false}}}

	snap, err := Collect(v, Options{Runtime: &rt, Verify: rep})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.RuntimeSource != "live" {
		t.Fatalf("runtime source = %q, want live", snap.RuntimeSource)
	}
	if snap.BreachTotal != 3 {
		t.Fatalf("breach total = %d, want 3", snap.BreachTotal)
	}
	if snap.FsyncCount != 2 || snap.FsyncP50 == 0 {
		t.Fatalf("fsync not folded in: count=%d p50=%v", snap.FsyncCount, snap.FsyncP50)
	}
	if snap.BatchLast != 4 || snap.BatchAvg != 4 {
		t.Fatalf("batch not folded in: last=%d avg=%v", snap.BatchLast, snap.BatchAvg)
	}
	if snap.VerifyDeepFailures != 1 || !snap.VerifyDeepRan {
		t.Fatalf("verify not folded in: failures=%d ran=%v", snap.VerifyDeepFailures, snap.VerifyDeepRan)
	}
	if !strings.Contains(snap.Set().Text(), "Source: this process's live counters.") {
		t.Fatal("exposition does not disclose the live runtime source")
	}
}

// TestCollectReadsSnapshotSidecar proves an offline inspection reads the
// persisted runtime snapshot and flags it as such.
func TestCollectReadsSnapshotSidecar(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()

	h := metrics.NewHistogram(metrics.FsyncBounds)
	h.Observe(0.002)
	hs := h.Snapshot()
	if err := metrics.SaveSnapshot(backing, metrics.Snapshot{
		Schema:     metrics.SnapshotSchema,
		CapturedAt: 42,
		Runtime:    metrics.Runtime{Fsync: &hs, BatchLast: 1, BatchCount: 5, BatchSum: 5},
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	snap, err := Collect(v, Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.RuntimeSource != "snapshot" {
		t.Fatalf("runtime source = %q, want snapshot", snap.RuntimeSource)
	}
	if snap.SnapshotCapturedAt != 42 {
		t.Fatalf("captured at = %d, want 42", snap.SnapshotCapturedAt)
	}
	if !strings.Contains(snap.Set().Text(), "Source: persisted snapshot") {
		t.Fatal("exposition does not disclose the snapshot runtime source")
	}
}
