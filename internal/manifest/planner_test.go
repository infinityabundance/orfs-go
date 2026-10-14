package manifest_test

import (
	"bytes"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// buildCorpus returns a deterministic, compressible body.
func buildCorpus(n int) []byte {
	out := make([]byte, 0, n)
	line := []byte("the quick brown fox jumps over the lazy dog; ")
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// TestPlannerSelectionAndCorrectness proves the live read path selects a
// certified manifested body, returns byte-identical results to authority over
// many random ranges, and falls back when the identity is stale.
func TestPlannerSelectionAndCorrectness(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 4 * 1024 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	corpus := buildCorpus(256 * 1024)
	file, err := ns.Create(cred, record.RootInode, "corpus.bin", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ns.Write(cred, file.ID, 0, corpus); err != nil {
		t.Fatalf("write: %v", err)
	}

	cache := manifest.NewCache(v)
	cache.SetAuthority(func(inode, off, size uint64) ([]byte, bool) {
		b, err := ns.ReadAuthoritative(namespace.Credential{}, inode, off, size)
		if err != nil {
			return nil, false
		}
		return b, true
	})
	ns.SetManifestSource(cache)

	in, _ := ns.Getattr(file.ID)
	root := v.Log.CommittedRoot()
	m, err := cache.Publish(file.ID, in.Version, root, corpus)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Correctness: manifested reads must equal authoritative reads everywhere.
	rng := uint64(12345)
	for i := 0; i < 200; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		off := (rng >> 16) % uint64(len(corpus))
		size := ((rng >> 8) % 8192) + 1
		got, err := ns.Read(cred, file.ID, off, size)
		if err != nil {
			t.Fatalf("manifested read: %v", err)
		}
		want, err := ns.ReadAuthoritative(cred, file.ID, off, size)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("range [%d,+%d): manifested %d bytes != authoritative %d bytes", off, size, len(got), len(want))
		}
	}

	manifested, missed, refused := cache.Stats()
	if manifested == 0 {
		t.Fatal("no read was served from the manifested body: selection failed")
	}
	if refused != 0 {
		t.Fatalf("expected no refusals on a fresh, certified body; got %d", refused)
	}
	if missed != 0 {
		t.Fatalf("expected no misses on a certified body; got %d", missed)
	}

	// Amplification is honest: the manifested body may be larger than the
	// logical content.
	t.Logf("amplification: logical %d bytes -> manifested %d bytes (%.2fx)",
		m.LogicalLen, m.PhysicalBytes, float64(m.PhysicalBytes)/float64(m.LogicalLen))

	// Staleness: a different version/root must fall back to authority (a miss,
	// not a refusal: there is simply no manifested body for that identity).
	beforeM, beforeMissed, _ := cache.Stats()
	if _, ok := cache.ManifestedRead(file.ID, in.Version+1, root, 0, 64); ok {
		t.Fatal("a stale version must not be served")
	}
	if _, ok := cache.ManifestedRead(file.ID, in.Version, root+1, 0, 64); ok {
		t.Fatal("a different root must not be served")
	}
	afterM, afterMissed, refusedAfter := cache.Stats()
	if afterM != beforeM || afterMissed < beforeMissed+2 {
		t.Fatalf("stale identities must be misses: manifested %d->%d, missed %d->%d", beforeM, afterM, beforeMissed, afterMissed)
	}
	if refusedAfter != 0 {
		t.Fatalf("a plain miss must not count as a refusal; refused=%d", refusedAfter)
	}
}

// TestPlannerLatency measures the manifested versus authoritative read path over
// the same history. The numbers are reported honestly; no superiority is
// claimed, and the test only asserts correctness, not speed.
func TestPlannerLatency(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 8 * 1024 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	const logical = 1 << 20 // 1 MiB
	corpus := buildCorpus(logical)
	file, _ := ns.Create(cred, record.RootInode, "big.bin", 0o644, 0, 0)
	if _, err := ns.Write(cred, file.ID, 0, corpus); err != nil {
		t.Fatalf("write: %v", err)
	}
	cache := manifest.NewCache(v)
	cache.SetAuthority(func(inode, off, size uint64) ([]byte, bool) {
		b, err := ns.ReadAuthoritative(namespace.Credential{}, inode, off, size)
		if err != nil {
			return nil, false
		}
		return b, true
	})
	ns.SetManifestSource(cache)
	in, _ := ns.Getattr(file.ID)
	if _, err := cache.Publish(file.ID, in.Version, v.Log.CommittedRoot(), corpus); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Warm the certification.
	if _, ok := cache.ManifestedRead(file.ID, in.Version, v.Log.CommittedRoot(), 0, 4096); !ok {
		t.Fatal("warm-up manifested read failed")
	}

	const iters = 3000
	const reqSize = 4096
	measure := func(manifested bool) []time.Duration {
		samples := make([]time.Duration, 0, iters)
		off := uint64(0)
		for i := 0; i < iters; i++ {
			off = (off*2654435761 + 40503) % (logical - reqSize)
			var b []byte
			var err error
			start := time.Now()
			if manifested {
				b, err = ns.Read(cred, file.ID, off, reqSize)
			} else {
				b, err = ns.ReadAuthoritative(cred, file.ID, off, reqSize)
			}
			samples = append(samples, time.Since(start))
			if err != nil || len(b) != reqSize {
				t.Fatalf("read failed (manifested=%v): err=%v len=%d", manifested, err, len(b))
			}
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples
	}

	auth := measure(false)
	man := measure(true)

	report := func(name string, s []time.Duration) {
		fmt.Printf("%s: p50=%v p99=%v\n", name, percentile(s, 0.50), percentile(s, 0.99))
	}
	report("authoritative 4K", auth)
	report("manifested 4K   ", man)
	t.Logf("authoritative p50=%v p99=%v", percentile(auth, 0.50), percentile(auth, 0.99))
	t.Logf("manifested    p50=%v p99=%v", percentile(man, 0.50), percentile(man, 0.99))
}
