package log

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/wm"
)

// publishConcurrent starts n goroutines that each publish one distinct
// single-record transaction as simultaneously as possible, and returns their
// roots and errors indexed by writer.
func publishConcurrent(t *testing.T, l *Log, n int) ([]uint64, []error) {
	t.Helper()
	roots := make([]uint64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := l.Publish([]*record.Record{inodeCreate(uint64(1000 + i))})
			roots[i] = r
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	return roots, errs
}

// readCommittedPrefix reads the exact committed bytes [0, root) from the log's
// segments.
func readCommittedPrefix(t *testing.T, backing string, root uint64) []byte {
	t.Helper()
	dir := Dir(backing)
	ids, err := segment.DiscoverSegmentIDs(dir)
	if err != nil {
		t.Fatalf("discover segments: %v", err)
	}
	var out []byte
	for _, id := range ids {
		if id >= root {
			break
		}
		b, err := os.ReadFile(filepath.Join(dir, segment.SegmentName(id)))
		if err != nil {
			t.Fatalf("read segment %d: %v", id, err)
		}
		remain := root - id
		if uint64(len(b)) > remain {
			b = b[:remain]
		}
		out = append(out, b...)
		if uint64(len(b)) == remain {
			break
		}
	}
	return out
}

// TestGroupCommitOptionAndEnv proves group commit is opt-in: it is off by
// default, enabled by WithGroupCommit, and enabled by ORFS_GROUP_COMMIT=1.
func TestGroupCommitOptionAndEnv(t *testing.T) {
	t.Setenv("ORFS_GROUP_COMMIT", "")

	def, err := Create(t.TempDir(), testGenesis())
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	if def.group != nil {
		t.Fatal("group commit must be off by default")
	}
	def.Close()

	opt, err := Create(t.TempDir(), testGenesis(), WithGroupCommit(4, time.Millisecond))
	if err != nil {
		t.Fatalf("create with option: %v", err)
	}
	if opt.group == nil {
		t.Fatal("WithGroupCommit did not enable group commit")
	}
	if opt.group.maxBatch != 4 || opt.group.maxWait != time.Millisecond {
		t.Fatalf("option not applied: maxBatch=%d maxWait=%s", opt.group.maxBatch, opt.group.maxWait)
	}
	opt.Close()

	t.Setenv("ORFS_GROUP_COMMIT", "1")
	env, err := Create(t.TempDir(), testGenesis())
	if err != nil {
		t.Fatalf("create with env: %v", err)
	}
	if env.group == nil {
		t.Fatal("ORFS_GROUP_COMMIT=1 did not enable group commit")
	}
	if env.group.maxBatch != DefaultGroupMaxBatch || env.group.maxWait != DefaultGroupMaxWait {
		t.Fatalf("env defaults wrong: maxBatch=%d maxWait=%s", env.group.maxBatch, env.group.maxWait)
	}
	env.Close()
}

// TestGroupCommitConcurrentDistinctContiguousRoots proves that n concurrent
// publishes coalesced into batches all succeed, each with its own distinct,
// contiguous, order-preserving root, and that the committed log scans cleanly
// with exactly the records a sequential run would have written.
func TestGroupCommitConcurrentDistinctContiguousRoots(t *testing.T) {
	const n = 48
	backing := t.TempDir()
	l, err := Create(backing, testGenesis(), WithGroupCommit(n, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	genesisRoot := l.CommittedRoot()

	roots, errs := publishConcurrent(t, l, n)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("writer %d: %v", i, e)
		}
	}

	// Every root is distinct.
	seen := make(map[uint64]int, n)
	for i, r := range roots {
		if prev, ok := seen[r]; ok {
			t.Fatalf("writers %d and %d share committed root %d", prev, i, r)
		}
		seen[r] = i
	}
	// Ordering writers by root yields strictly increasing, equal-gap (contiguous)
	// roots: every transaction is exactly one single-record frame set.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return roots[order[a]] < roots[order[b]] })
	gap := roots[order[0]] - genesisRoot
	for k := 1; k < n; k++ {
		if roots[order[k]] <= roots[order[k-1]] {
			t.Fatalf("roots not strictly increasing at position %d", k)
		}
		if roots[order[k]]-roots[order[k-1]] != gap {
			t.Fatalf("roots not contiguous at position %d: gap %d != %d", k, roots[order[k]]-roots[order[k-1]], gap)
		}
	}
	finalRoot := roots[order[n-1]]
	if l.CommittedRoot() != finalRoot {
		t.Fatalf("committed root %d != greatest transaction root %d", l.CommittedRoot(), finalRoot)
	}
	if got := len(l.Boundaries()); got != n+1 {
		t.Fatalf("boundaries = %d, want %d (genesis + %d transactions)", got, n+1, n)
	}
	if l.CommittedRecordCount() != uint64(1+3*n) {
		t.Fatalf("committed records = %d, want %d", l.CommittedRecordCount(), 1+3*n)
	}
	l.Close()

	scan, err := ScanLog(Dir(backing))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.CommittedRoot != finalRoot {
		t.Fatalf("scan root %d != %d", scan.CommittedRoot, finalRoot)
	}
	if want := uint64(1 + 3*n); scan.CommittedRecordCount != want {
		t.Fatalf("scan records %d, want %d", scan.CommittedRecordCount, want)
	}
	if scan.Tail != nil {
		t.Fatalf("unexpected tail after a clean batch: %+v", scan.Tail)
	}
}

// TestGroupCommitByteIdenticalToSequential proves the committed prefix produced
// by concurrent group commit is byte-identical to a sequential run that replays
// the same records in the same order. The commit order is recovered from the
// returned (distinct, increasing) roots, so the comparison isolates the effect
// of the barrier, not of scheduling.
func TestGroupCommitByteIdenticalToSequential(t *testing.T) {
	const n = 32

	backingA := t.TempDir()
	la, err := Create(backingA, testGenesis(), WithGroupCommit(n, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("create concurrent: %v", err)
	}
	roots, errs := publishConcurrent(t, la, n)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("writer %d: %v", i, e)
		}
	}
	rootA := la.CommittedRoot()
	la.Close()

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return roots[order[a]] < roots[order[b]] })

	backingB := t.TempDir()
	lb, err := Create(backingB, testGenesis())
	if err != nil {
		t.Fatalf("create sequential: %v", err)
	}
	for _, w := range order {
		if _, err := lb.Publish([]*record.Record{inodeCreate(uint64(1000 + w))}); err != nil {
			t.Fatalf("sequential publish: %v", err)
		}
	}
	rootB := lb.CommittedRoot()
	lb.Close()

	if rootA != rootB {
		t.Fatalf("final committed roots differ: %d != %d", rootA, rootB)
	}
	a := readCommittedPrefix(t, backingA, rootA)
	b := readCommittedPrefix(t, backingB, rootB)
	if uint64(len(a)) != rootA {
		t.Fatalf("read %d bytes of a %d-byte prefix", len(a), rootA)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("committed prefix is not byte-identical to the sequential run (%d vs %d bytes)", len(a), len(b))
	}
}

// TestGroupCommitFsyncFailureFailsWholeBatch proves that when the batch barrier
// fails, every transaction in the batch fails, no waiter is acknowledged, and
// the previous committed root (and its watermark) is left intact.
func TestGroupCommitFsyncFailureFailsWholeBatch(t *testing.T) {
	const n = 16
	backing := t.TempDir()
	l, err := Create(backing, testGenesis(), WithGroupCommit(n, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer l.Close()

	root0, err := l.Publish([]*record.Record{inodeCreate(1)})
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	records0 := l.CommittedRecordCount()

	// Install the barrier seam: every subsequent fsync fails.
	boom := errors.New("injected fsync failure")
	l.syncHook = func() error { return boom }

	roots, errs := publishConcurrent(t, l, n)
	for i := range errs {
		if !errors.Is(errs[i], boom) {
			t.Fatalf("writer %d: err = %v, want the injected failure", i, errs[i])
		}
		if roots[i] != 0 {
			t.Fatalf("writer %d: root = %d, want 0 on a failed barrier", i, roots[i])
		}
	}
	if l.CommittedRoot() != root0 {
		t.Fatalf("committed root moved on a failed batch: %d != %d", l.CommittedRoot(), root0)
	}
	if l.CommittedRecordCount() != records0 {
		t.Fatalf("committed record count moved on a failed batch: %d != %d", l.CommittedRecordCount(), records0)
	}
	// The watermark is written only after a barrier, so it must still certify
	// the previous root: nothing was partially published.
	w, ok, err := wm.Read(Dir(backing))
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if !ok {
		t.Fatal("expected a watermark")
	}
	if w.CommittedRoot != root0 {
		t.Fatalf("watermark advanced to %d on a failed batch, want %d", w.CommittedRoot, root0)
	}
}
