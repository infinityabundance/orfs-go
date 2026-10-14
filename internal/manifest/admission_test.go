package manifest_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// The manifested-admission court drives a scan-heavy trace through the *live*
// manifested read path (namespace.Read → manifest.Cache.ManifestedRead) and
// compares three policies for the resident-body cache:
//
//   - s3fifo: S3-FIFO admission (the default), bounded,
//   - lru:    recency-only, bounded, and
//   - map:    the legacy unbounded map (nothing evicted) — the pre-admission
//     baseline.
//
// It measures the manifested-body cache hit rate (hits = a read answered from an
// already-open certified body; misses = a read that had to re-load and re-certify
// from the store) and, secondarily, the per-read latency. Every arm returns the
// same bytes: admission changes only whether a body is served from memory or
// re-opened.

const (
	admReadSize    = 512
	admContentSize = 1024
)

// admWorkload parameterises the scan-heavy trace. The reference configuration is
// a bounded cache of 64 bodies, a 32-inode hot set, and a scan of 480 cold
// inodes walked in bursts of 96 (five bursts).
type admWorkload struct {
	Capacity int
	Hot      int
	Scan     int
	Burst    int
	Warmup   int
}

func admReference() admWorkload {
	return admWorkload{Capacity: 64, Hot: 32, Scan: 480, Burst: 96, Warmup: 4}
}

func (w admWorkload) bodies() int { return w.Hot + w.Scan }

// admRead is one logical read in the trace: index into the file list and whether
// it is a hot-set read (the working set) or a cold scan read.
type admRead struct {
	file int
	hot  bool
}

// warmupReads primes the hot set so it settles into the cache before the scan.
func (w admWorkload) warmupReads() []admRead {
	out := make([]admRead, 0, w.Warmup*w.Hot)
	for r := 0; r < w.Warmup; r++ {
		for j := 0; j < w.Hot; j++ {
			out = append(out, admRead{file: j, hot: true})
		}
	}
	return out
}

// burstReads models a directory walk / backup scan: a burst of fresh cold inodes
// is read, then the whole hot working set. The cold burst is longer than the
// cache, which is exactly what flushes a recency-only cache.
func (w admWorkload) burstReads() []admRead {
	out := make([]admRead, 0, w.Scan+w.Scan/w.Burst*w.Hot)
	for s := 0; s < w.Scan/w.Burst; s++ {
		for i := 0; i < w.Burst; i++ {
			out = append(out, admRead{file: w.Hot + s*w.Burst + i, hot: false})
		}
		for j := 0; j < w.Hot; j++ {
			out = append(out, admRead{file: j, hot: true})
		}
	}
	return out
}

// interleavedReads is the reference court's shape: one cold inode, then the whole
// hot set. The hot set is re-referenced within the cache window, so recency alone
// protects it — the honest case where admission buys nothing over LRU.
func (w admWorkload) interleavedReads() []admRead {
	out := make([]admRead, 0, w.Scan*(1+w.Hot))
	for i := 0; i < w.Scan; i++ {
		out = append(out, admRead{file: w.Hot + i, hot: false})
		for j := 0; j < w.Hot; j++ {
			out = append(out, admRead{file: j, hot: true})
		}
	}
	return out
}

// admArm is one (policy, capacity) arm of the comparison.
type admArm struct {
	name     string
	policy   manifest.Policy
	capacity int
	note     string
}

func admArms(capacity int) []admArm {
	return []admArm{
		{
			name:     "s3fifo",
			policy:   manifest.PolicyS3FIFO,
			capacity: capacity,
			note:     "S3-FIFO admission: a first touch enters a small probationary queue, only a re-referenced body is promoted to main; a one-pass scan is evicted without displacing the hot set",
		},
		{
			name:     "lru",
			policy:   manifest.PolicyLRU,
			capacity: capacity,
			note:     "recency-only LRU: a scan burst longer than the cache evicts the whole hot set",
		},
		{
			name:     "map",
			policy:   manifest.PolicyUnbounded,
			capacity: 0,
			note:     "the legacy unbounded resident map (nothing evicted); 100% residency but unbounded growth — the pre-admission baseline",
		},
	}
}

// admArmResult is one arm's measured outcome.
type admArmResult struct {
	policy   string
	capacity int
	hits     uint64
	misses   uint64
	hotHits  uint64
	hotMiss  uint64
	resident uint64
	p50      time.Duration
	note     string
}

func (r admArmResult) hitRate() float64 { return rate(r.hits, r.hits+r.misses) }
func (r admArmResult) hotRate() float64 { return rate(r.hotHits, r.hotHits+r.hotMiss) }

func rate(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// admFixture is a built volume with one manifested body per file.
type admFixture struct {
	vol   *volume.Volume
	ns    *namespace.Namespace
	cred  namespace.Credential
	files []uint64
}

// buildAdmFixture creates w.bodies() files, writes deterministic content into
// each, and publishes a manifested body for every one under the final committed
// root. Bodies are published through the package-level Publish (not a Cache
// method) so every arm loads them lazily through the same certified path.
func buildAdmFixture(t *testing.T, w admWorkload) *admFixture {
	t.Helper()
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 8 * 1024 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	ns := namespace.New(v)
	cred := namespace.Credential{}

	bodies := w.bodies()
	files := make([]uint64, bodies)
	content := make([][]byte, bodies)
	for i := 0; i < bodies; i++ {
		name := fmt.Sprintf("f%04d.bin", i)
		file, err := ns.Create(cred, record.RootInode, name, 0o644, 0, 0)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		// Deterministic, compressible but per-file-distinct content.
		data := make([]byte, 0, admContentSize)
		tag := fmt.Appendf(nil, "orfs-manifested-%04d ", i)
		for len(data) < admContentSize {
			data = append(data, tag...)
		}
		data = data[:admContentSize]
		if _, err := ns.Write(cred, file.ID, 0, data); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		files[i] = file.ID
		content[i] = data
	}

	root := v.Log.CommittedRoot()
	for i := 0; i < bodies; i++ {
		in, err := ns.Getattr(files[i])
		if err != nil {
			t.Fatalf("getattr %d: %v", files[i], err)
		}
		if _, err := manifest.Publish(v, files[i], in.Version, root, content[i]); err != nil {
			t.Fatalf("publish %d: %v", files[i], err)
		}
	}
	return &admFixture{vol: v, ns: ns, cred: cred, files: files}
}

// runAdmArm installs a fresh admission cache for one arm, warms the hot set,
// replays the measured trace through namespace.Read, and reports the body-cache
// hit/miss accounting (hits = served from a resident certified body) and the
// per-read p50.
func runAdmArm(t *testing.T, f *admFixture, warmup, reads []admRead, arm admArm) admArmResult {
	t.Helper()
	cache := manifest.NewCache(f.vol,
		manifest.WithAdmissionCapacity(arm.capacity),
		manifest.WithPolicy(arm.policy),
	)
	cache.SetAuthority(func(inode, off, size uint64) ([]byte, bool) {
		b, err := f.ns.ReadAuthoritative(f.cred, inode, off, size)
		if err != nil {
			return nil, false
		}
		return b, true
	})
	f.ns.SetManifestSource(cache)

	for _, r := range warmup {
		if _, err := f.ns.Read(f.cred, f.files[r.file], 0, admReadSize); err != nil {
			t.Fatalf("%s warmup read: %v", arm.name, err)
		}
	}
	h0, m0, _ := cache.BodyStats()

	samples := make([]time.Duration, 0, len(reads))
	var hotHits, hotMiss uint64
	for _, r := range reads {
		hb, mb, _ := cache.BodyStats()
		start := time.Now()
		got, err := f.ns.Read(f.cred, f.files[r.file], 0, admReadSize)
		samples = append(samples, time.Since(start))
		if err != nil {
			t.Fatalf("%s read: %v", arm.name, err)
		}
		if len(got) != admReadSize {
			t.Fatalf("%s read returned %d bytes, want %d", arm.name, len(got), admReadSize)
		}
		ha, ma, _ := cache.BodyStats()
		if r.hot {
			hotHits += ha - hb
			hotMiss += ma - mb
		}
	}
	h1, m1, resident := cache.BodyStats()

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return admArmResult{
		policy:   arm.name,
		capacity: arm.capacity,
		hits:     h1 - h0,
		misses:   m1 - m0,
		hotHits:  hotHits,
		hotMiss:  hotMiss,
		resident: resident,
		p50:      percentile(samples, 0.50),
		note:     arm.note,
	}
}

// admShapeResult is one trace shape's measured outcome.
type admShapeResult struct {
	name  string
	note  string
	reads []admRead
	arms  []admArmResult
}

// TestManifestAdmissionScanResistance is the manifested-admission court. It
// demonstrates the admission win on a realistic scan-heavy OrFS trace (a burst
// directory walk over many cold inodes with a hot working set) and is honest
// about the shape where there is no win (the hot set re-referenced within the
// cache window).
func TestManifestAdmissionScanResistance(t *testing.T) {
	w := admReference()
	f := buildAdmFixture(t, w)
	warmup := w.warmupReads()

	shapes := []admShapeResult{
		{
			name:  "burst",
			note:  "directory-walk burst: a cold burst longer than the cache, then the hot set (the scan-pollution case)",
			reads: w.burstReads(),
		},
		{
			name:  "interleaved",
			note:  "reference shape: one cold inode then the whole hot set; the hot set stays within the cache window, so recency alone protects it",
			reads: w.interleavedReads(),
		},
	}

	results := make([]admShapeResult, len(shapes))
	for si, shape := range shapes {
		results[si] = admShapeResult{name: shape.name, note: shape.note, reads: shape.reads}
		for _, arm := range admArms(w.Capacity) {
			res := runAdmArm(t, f, warmup, shape.reads, arm)
			results[si].arms = append(results[si].arms, res)
			t.Logf("%-9s %-7s resident %4d  hits %5d  misses %5d  hit rate %.4f  hot hit rate %.4f  p50 %v",
				shape.name, res.policy, res.resident, res.hits, res.misses, res.hitRate(), res.hotRate(), res.p50)
		}
	}

	// Every bounded arm stays within its capacity; the unbounded map does not.
	for _, shape := range results {
		for _, a := range shape.arms {
			if a.capacity > 0 && a.resident > uint64(a.capacity) {
				t.Fatalf("%s/%s resident bodies %d exceed capacity %d", shape.name, a.policy, a.resident, a.capacity)
			}
		}
	}

	byName := func(shape admShapeResult, name string) admArmResult {
		for _, a := range shape.arms {
			if a.policy == name {
				return a
			}
		}
		t.Fatalf("%s: arm %q missing", shape.name, name)
		return admArmResult{}
	}

	// The admission win: on the burst scan the hot working set survives under
	// S3-FIFO at a bounded capacity, while the recency-only LRU is flushed.
	burst := results[0]
	s3, lru, mp := byName(burst, "s3fifo"), byName(burst, "lru"), byName(burst, "map")
	if s3.hotRate() < 0.95 {
		t.Fatalf("burst: S3-FIFO hot-set hit rate %.4f, want >= 0.95", s3.hotRate())
	}
	if lru.hotRate() > 0.05 {
		t.Fatalf("burst: LRU hot-set hit rate %.4f, want <= 0.05 (the scan should flush it)", lru.hotRate())
	}
	if s3.hotRate() <= lru.hotRate() {
		t.Fatalf("burst: no admission win: s3fifo %.4f <= lru %.4f", s3.hotRate(), lru.hotRate())
	}
	if s3.misses >= lru.misses {
		t.Fatalf("burst: admission should re-certify less than LRU: s3fifo %d >= lru %d", s3.misses, lru.misses)
	}
	// The unbounded map retains the hot set too — but without a bound.
	if mp.hotRate() < 0.95 {
		t.Fatalf("burst: map hot-set hit rate %.4f, want >= 0.95", mp.hotRate())
	}
	if mp.resident <= uint64(w.Capacity) {
		t.Fatalf("burst: map resident %d should exceed the bounded capacity %d", mp.resident, w.Capacity)
	}

	// Honesty: on the interleaved shape there is no win over LRU — all three
	// retain the hot set because it is re-referenced within the cache window.
	inter := results[1]
	if iS3, iLRU := byName(inter, "s3fifo"), byName(inter, "lru"); iS3.hotRate() < 0.95 || iLRU.hotRate() < 0.95 {
		t.Fatalf("interleaved: expected both s3fifo (%.4f) and lru (%.4f) to retain the hot set",
			iS3.hotRate(), iLRU.hotRate())
	}

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeAdmissionEvidence(t, w, results)
	}
}

// TestManifestAdmissionPreservesBytes proves the correctness invariant directly:
// with a tiny admission capacity (2) that forces constant eviction and
// re-certification, every manifested read is byte-identical to authority — for
// both the admission and eviction paths.
func TestManifestAdmissionPreservesBytes(t *testing.T) {
	w := admReference()
	w.Hot, w.Scan = 6, 12
	f := buildAdmFixture(t, w)

	cache := manifest.NewCache(f.vol, manifest.WithAdmissionCapacity(2))
	cache.SetAuthority(func(inode, off, size uint64) ([]byte, bool) {
		b, err := f.ns.ReadAuthoritative(f.cred, inode, off, size)
		if err != nil {
			return nil, false
		}
		return b, true
	})
	f.ns.SetManifestSource(cache)

	rng := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < 400; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		idx := int((rng >> 16) % uint64(len(f.files)))
		off := (rng >> 8) % (admContentSize - 1)
		size := ((rng >> 4) % 256) + 1
		got, err := f.ns.Read(f.cred, f.files[idx], off, size)
		if err != nil {
			t.Fatalf("manifested read: %v", err)
		}
		want, err := f.ns.ReadAuthoritative(f.cred, f.files[idx], off, size)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("inode %d range [%d,+%d): manifested %d bytes != authoritative %d bytes",
				f.files[idx], off, size, len(got), len(want))
		}
	}
	if _, _, resident := cache.BodyStats(); resident > 2 {
		t.Fatalf("tiny-capacity admission cache holds %d bodies, want <= 2", resident)
	}
}

// receipt types, written only under ORFS_EVIDENCE=1.

type admissionArmReceipt struct {
	Policy         string  `json:"policy"`
	Capacity       int     `json:"capacity"`
	ResidentBodies uint64  `json:"resident_bodies"`
	Hits           uint64  `json:"hits"`
	Misses         uint64  `json:"misses"`
	HitRate        float64 `json:"hit_rate"`
	HotHits        uint64  `json:"hot_hits"`
	HotMisses      uint64  `json:"hot_misses"`
	HotHitRate     float64 `json:"hot_hit_rate"`
	P50Ns          int64   `json:"p50_ns"`
	Note           string  `json:"note,omitempty"`
}

type admissionShapeReceipt struct {
	Shape string                `json:"shape"`
	Reads int                   `json:"reads"`
	Note  string                `json:"note"`
	Arms  []admissionArmReceipt `json:"arms"`
}

type admissionWorkloadReceipt struct {
	Capacity       int `json:"capacity"`
	HotObjects     int `json:"hot_objects"`
	ScanObjects    int `json:"scan_objects"`
	ColdBurst      int `json:"cold_burst"`
	ScanSteps      int `json:"scan_steps"`
	WarmupRounds   int `json:"warmup_rounds"`
	ContentBytes   int `json:"content_bytes"`
	ReadSizeBytes  int `json:"read_size_bytes"`
	ManifestedBody int `json:"manifested_bodies"`
}

type admissionReceipt struct {
	Schema   string                   `json:"schema"`
	Workload admissionWorkloadReceipt `json:"workload"`
	Shapes   []admissionShapeReceipt  `json:"shapes"`
	Note     string                   `json:"note"`
}

func writeAdmissionEvidence(t *testing.T, w admWorkload, shapes []admShapeResult) {
	t.Helper()

	doc := admissionReceipt{
		Schema: "orfs.performance.manifest-admission.v1",
		Workload: admissionWorkloadReceipt{
			Capacity:       w.Capacity,
			HotObjects:     w.Hot,
			ScanObjects:    w.Scan,
			ColdBurst:      w.Burst,
			ScanSteps:      w.Scan / w.Burst,
			WarmupRounds:   w.Warmup,
			ContentBytes:   admContentSize,
			ReadSizeBytes:  admReadSize,
			ManifestedBody: w.bodies(),
		},
		Note: "Single-run, single-host court driving the LIVE manifested read path " +
			"(namespace.Read -> manifest.Cache.ManifestedRead) over one identical scan-heavy " +
			"trace; only the resident-body admission policy differs. This is a HIT-RATE " +
			"measurement of the manifested-body cache (hits = served from an already-open " +
			"certified body; misses = re-loaded and re-certified from the store), not a " +
			"latency claim; p50_ns is reported but host- and timing-dependent. Admission " +
			"never changes the bytes returned (see TestManifestAdmissionPreservesBytes). The " +
			"burst shape shows the win: S3-FIFO, bounded, retains the hot set under a cold " +
			"burst that flushes LRU; the unbounded map retains it too but without a bound. " +
			"The interleaved shape shows the honest counter-case: when the hot set is " +
			"re-referenced within the cache window, recency alone protects it and admission " +
			"buys nothing over LRU.",
	}
	for _, shape := range shapes {
		sr := admissionShapeReceipt{
			Shape: shape.name,
			Reads: len(shape.reads),
			Note:  shape.note,
		}
		for _, a := range shape.arms {
			sr.Arms = append(sr.Arms, admissionArmReceipt{
				Policy:         a.policy,
				Capacity:       a.capacity,
				ResidentBodies: a.resident,
				Hits:           a.hits,
				Misses:         a.misses,
				HitRate:        a.hitRate(),
				HotHits:        a.hotHits,
				HotMisses:      a.hotMiss,
				HotHitRate:     a.hotRate(),
				P50Ns:          a.p50.Nanoseconds(),
				Note:           a.note,
			})
		}
		doc.Shapes = append(doc.Shapes, sr)
	}

	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	dir := filepath.Join("..", "..", "evidence", "performance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	path := filepath.Join(dir, "manifest-admission.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s", path)
}
