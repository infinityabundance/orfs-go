package namespace_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// openTestVolume opens a fresh writable volume with group commit enabled, so the
// concurrency tests exercise the batched committer the S3 gateway uses.
func openTestVolume(t *testing.T) (*volume.Volume, *namespace.Namespace) {
	t.Helper()
	t.Setenv("ORFS_GROUP_COMMIT", "1")
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return v, namespace.New(v)
}

// TestConcurrentCreateSameName is the validation-race case: many writers create
// one name against the same committed view. Exactly one may win; every other
// must be refused with EEXIST, and the committed namespace must hold exactly one
// entry for it.
func TestConcurrentCreateSameName(t *testing.T) {
	v, ns := openTestVolume(t)
	cred := namespace.Credential{}
	const n = 48

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = ns.Create(cred, record.RootInode, "contended", 0o644, 0, 0)
		}(i)
	}
	close(start)
	wg.Wait()

	wins, exists := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, orfserr.ErrExists):
			exists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || exists != n-1 {
		t.Fatalf("wins=%d exists=%d, want 1/%d", wins, exists, n-1)
	}
	infos, _, _, err := ns.ReaddirFull(cred, record.RootInode)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	count := 0
	for _, e := range infos {
		if e.Name == "contended" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the committed namespace names %q %d times, want 1", "contended", count)
	}
	assertReplayEquivalent(t, v)
}

// TestConcurrentUnlinkLinkCount runs many unlinks (of distinct names of one
// inode) and links concurrently. The absolute link count each unlink emits must
// be serialised against the others, so the surviving count is exact and the live
// index stays a pure function of the committed prefix.
func TestConcurrentUnlinkLinkCount(t *testing.T) {
	v, ns := openTestVolume(t)
	cred := namespace.Credential{}
	f, err := ns.Create(cred, record.RootInode, "n0", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const links = 16
	for i := 1; i < links; i++ {
		if err := ns.Link(cred, record.RootInode, fmt.Sprintf("n%d", i), f.ID); err != nil {
			t.Fatalf("link %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < links; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := ns.Unlink(cred, record.RootInode, fmt.Sprintf("n%d", i)); err != nil {
				t.Errorf("unlink %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if n := ns.Volume().View().NameCount(f.ID); n != 0 {
		t.Fatalf("inode %d still has %d linked names, want 0", f.ID, n)
	}
	in, err := ns.Getattr(f.ID)
	if err != nil {
		t.Fatalf("getattr: %v", err)
	}
	if in.Nlink != 0 {
		t.Fatalf("link count %d, want 0", in.Nlink)
	}
	assertReplayEquivalent(t, v)
}

// TestConcurrentUnlinkWithOpen writes to and reads a file while it is unlinked
// from its only name: the open inode must stay readable (its bytes are retained)
// and the namespace must stay consistent.
func TestConcurrentUnlinkWithOpen(t *testing.T) {
	v, ns := openTestVolume(t)
	cred := namespace.Credential{}
	f, err := ns.Create(cred, record.RootInode, "open", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	payload := []byte("retained bytes")
	if _, err := ns.Write(cred, f.ID, 0, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			b, err := ns.Read(cred, f.ID, 0, uint64(len(payload)))
			if err != nil {
				t.Errorf("read after unlink: %v", err)
				return
			}
			if string(b) != string(payload) {
				t.Errorf("read returned %q, want %q", b, payload)
				return
			}
		}
		close(stop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ns.Unlink(cred, record.RootInode, "open"); err != nil {
			t.Errorf("unlink: %v", err)
		}
	}()
	wg.Wait()
	<-stop
	assertReplayEquivalent(t, v)
}

// TestConcurrentRenameContended renames one source name to two different targets
// concurrently: exactly one rename may win, the source name must be gone either
// way, and the index must stay replay-equivalent.
func TestConcurrentRenameContended(t *testing.T) {
	v, ns := openTestVolume(t)
	cred := namespace.Credential{}
	for round := 0; round < 8; round++ {
		src := fmt.Sprintf("s%d", round)
		if _, err := ns.Create(cred, record.RootInode, src, 0o644, 0, 0); err != nil {
			t.Fatalf("create: %v", err)
		}
		targets := []string{fmt.Sprintf("t%da", round), fmt.Sprintf("t%db", round)}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, len(targets))
		for i, dst := range targets {
			wg.Add(1)
			go func(i int, dst string) {
				defer wg.Done()
				<-start
				errs[i] = ns.Rename(cred, record.RootInode, src, record.RootInode, dst, false)
			}(i, dst)
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
				continue
			}
			if !errors.Is(err, orfserr.ErrNotFound) {
				t.Fatalf("unexpected rename error: %v", err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d renames won, want 1 (errs=%v)", round, wins, errs)
		}
		if _, err := ns.Lookup(cred, record.RootInode, src); !errors.Is(err, orfserr.ErrNotFound) {
			t.Fatalf("round %d: source name still present (%v)", round, err)
		}
	}
	assertReplayEquivalent(t, v)
}

// TestGroupCommitBatchesThroughNamespace proves the writer no longer serialises
// the durability barrier: concurrent, non-conflicting mutations reach the log's
// group committer together, so its observed batch size exceeds one.
func TestGroupCommitBatchesThroughNamespace(t *testing.T) {
	v, ns := openTestVolume(t)
	cred := namespace.Credential{}
	const (
		writers = 32
		each    = 24
	)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				name := fmt.Sprintf("w%02d-%04d", w, i)
				if _, err := ns.Create(cred, record.RootInode, name, 0o644, 0, 0); err != nil {
					t.Errorf("create %s: %v", name, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	rt := v.Log.Runtime()
	if rt.BatchCount == 0 {
		t.Fatal("no group-commit batch was recorded")
	}
	if rt.BatchSum != writers*each {
		t.Fatalf("batch sum %d, want %d (batch avg %.2f)", rt.BatchSum, writers*each, rt.BatchAvg())
	}
	if rt.BatchAvg() <= 1 {
		t.Fatalf("group commit did not batch through the namespace: avg batch %.2f, count %d", rt.BatchAvg(), rt.BatchCount)
	}
	assertReplayEquivalent(t, v)
}

// TestConcurrentMixedWorkloadReplayEquivalent drives many mutations of every kind
// concurrently and then asserts the live index is byte-identical to a
// from-scratch replay of the committed prefix — the invariant the whole design
// rests on, independent of which publish finished first.
func TestConcurrentMixedWorkloadReplayEquivalent(t *testing.T) {
	v, ns := openTestVolume(t)
	cred := namespace.Credential{}
	if _, err := ns.Mkdir(cred, record.RootInode, "dir", 0o755, 0, 0); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const workers = 24
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				name := fmt.Sprintf("f%02d-%02d", w, i)
				f, err := ns.Create(cred, record.RootInode, name, 0o644, 0, 0)
				if err != nil {
					// A concurrent rename may have taken the name; that is fine.
					continue
				}
				_, _ = ns.Write(cred, f.ID, 0, []byte("data"))
				_ = ns.Setxattr(cred, f.ID, "user.k", []byte("v"))
				if err := ns.Link(cred, record.RootInode, name+"-l", f.ID); err == nil {
					_ = ns.Unlink(cred, record.RootInode, name+"-l")
				}
				_ = ns.Rename(cred, record.RootInode, name, record.RootInode, name+"-r", false)
				_ = ns.Unlink(cred, record.RootInode, name+"-r")
			}
		}(w)
	}
	wg.Wait()
	assertReplayEquivalent(t, v)
}

// assertReplayEquivalent proves the live index equals a from-scratch replay of
// the committed prefix. Callers must have quiesced the writers.
func assertReplayEquivalent(t *testing.T, v *volume.Volume) {
	t.Helper()
	full, err := index.Replay(log.Dir(v.Backing), v.Log.Extents(), v.Log.CommittedRoot())
	if err != nil {
		t.Fatalf("full replay: %v", err)
	}
	if !v.Index().Equal(full) {
		t.Fatal("the live index is not a pure function of the committed prefix")
	}
}
