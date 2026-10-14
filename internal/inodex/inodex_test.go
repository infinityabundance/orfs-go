package inodex

import (
	"testing"
	"unsafe"

	"github.com/infinityabundance/orfs-go/internal/index"
)

func TestSpineLookupAndSeal(t *testing.T) {
	s := New()
	s.Put(1, 1, 3, 100)
	s.Put(5, 0, 7, 900)
	s.Put(5, 0, 8, 950) // update
	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2", s.Len())
	}
	if _, _, _, ok := s.Get(3); ok {
		t.Fatal("absent id should not be present")
	}
	kind, ver, off, ok := s.Get(5)
	if !ok || kind != 0 || ver != 8 || off != 950 {
		t.Fatalf("get(5) = %d,%d,%d,%v", kind, ver, off, ok)
	}
	// Ids skipped by density are still absent.
	if _, _, _, ok := s.Get(2); ok {
		t.Fatal("dense gap should read absent")
	}
	s.Seal()
	s.Put(9, 0, 1, 1)
	if _, _, _, ok := s.Get(9); ok {
		t.Fatal("sealed spine must reject writes")
	}
}

// TestSpineIsMuchSmallerThanIndex measures the packed spine against the live
// index's inode struct: the historical role can be a flat 16-byte-per-inode
// spine instead of a pointer-rich struct per inode.
func TestSpineIsMuchSmallerThanIndex(t *testing.T) {
	const n = 100_000
	s := New()
	for i := uint64(1); i <= n; i++ {
		s.Put(i, 0, 1, i*100)
	}
	s.Seal()

	packed := s.Bytes()
	spinePer := float64(packed) / float64(n)
	inodeSize := float64(unsafe.Sizeof(index.Inode{}))
	ratio := inodeSize / spinePer
	t.Logf("spine: %d bytes total, %.1f bytes/inode; index.Inode struct %d bytes; ratio %.1fx",
		packed, spinePer, uint64(inodeSize), ratio)
	if packed != n*EntryBytes {
		t.Fatalf("spine packed size = %d, want %d", packed, n*EntryBytes)
	}
	if ratio < 4 {
		t.Fatalf("spine is only %.1fx smaller than a full inode struct; expected the sealed spine to be much smaller", ratio)
	}
}

func BenchmarkSpineGet(b *testing.B) {
	s := New()
	for i := uint64(1); i <= 1_000_000; i++ {
		s.Put(i, 0, 1, i)
	}
	s.Seal()
	b.ResetTimer()
	var ok bool
	for i := 0; i < b.N; i++ {
		_, _, _, ok = s.Get(uint64(i%1_000_000) + 1)
	}
	_ = ok
}

func BenchmarkMapGet(b *testing.B) {
	m := make(map[uint64]*index.Inode, 1_000_000)
	for i := uint64(1); i <= 1_000_000; i++ {
		m[i] = &index.Inode{ID: i, Version: 1}
	}
	b.ResetTimer()
	var ok bool
	for i := 0; i < b.N; i++ {
		_, ok = m[uint64(i%1_000_000)+1]
	}
	_ = ok
}
