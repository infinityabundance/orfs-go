package index

import (
	"math/rand"
	"testing"
)

// refMap is the original flat extent-map implementation, kept here as the
// reference model for a differential test against the persistent (path-copying)
// ExtentMap. The persistent map must be observably identical: same size, same
// ordered extent sequence, same maximality, same read plan.
type refMap struct {
	extents []Extent
	size    uint64
}

func (m *refMap) Size() uint64 { return m.size }

func (m *refMap) Extents() []Extent { return append([]Extent(nil), m.extents...) }

func (m *refMap) appendZero(length uint64) {
	start := m.size
	if n := len(m.extents); n > 0 && m.extents[n-1].Kind == ExtentZero {
		m.extents[n-1].Length += length
	} else {
		m.extents = append(m.extents, Extent{Start: start, Length: length, Kind: ExtentZero})
	}
}

func (m *refMap) replace(start, end uint64, kind ExtentKind, dataOffset uint64) {
	if start >= end {
		return
	}
	var out []Extent
	for _, e := range m.extents {
		if e.End() <= start || e.Start >= end {
			out = append(out, e)
			continue
		}
		if e.Start < start {
			out = append(out, Extent{Start: e.Start, Length: start - e.Start, Kind: e.Kind, DataOffset: e.DataOffset})
		}
		if e.End() > end {
			right := Extent{Start: end, Length: e.End() - end, Kind: e.Kind}
			if e.Kind == ExtentData {
				right.DataOffset = e.DataOffset + (end - e.Start)
			}
			out = append(out, right)
		}
	}
	out = append(out, Extent{Start: start, Length: end - start, Kind: kind, DataOffset: dataOffset})
	m.extents = mergeAdjacent(refSortExtents(out))
}

func (m *refMap) Write(offset, dataOffset, length uint64) {
	if length == 0 {
		return
	}
	if offset > m.size {
		m.appendZero(offset - m.size)
	}
	end := offset + length
	if end > m.size {
		m.size = end
	}
	m.replace(offset, end, ExtentData, dataOffset)
}

func (m *refMap) Zero(offset, length uint64) {
	if length == 0 {
		return
	}
	end := offset + length
	if offset > m.size {
		m.appendZero(offset - m.size)
	}
	if end > m.size {
		m.size = end
	}
	m.replace(offset, end, ExtentZero, 0)
}

func (m *refMap) Resize(newSize uint64) {
	if newSize == m.size {
		return
	}
	if newSize > m.size {
		m.appendZero(newSize - m.size)
		m.size = newSize
		return
	}
	m.size = newSize
	var out []Extent
	for _, e := range m.extents {
		if e.Start >= m.size {
			continue
		}
		if e.End() > m.size {
			e.Length = m.size - e.Start
		}
		out = append(out, e)
	}
	m.extents = mergeAdjacent(out)
}

func refSortExtents(in []Extent) []Extent {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].Start > in[j].Start; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
	return in
}

func refPieces(m *refMap, offset, length uint64) []Piece {
	if length == 0 || offset >= m.size {
		return nil
	}
	end := offset + length
	if end > m.size {
		end = m.size
	}
	var out []Piece
	for _, e := range m.extents {
		if e.End() <= offset || e.Start >= end {
			continue
		}
		lo := maxu(e.Start, offset)
		hi := minu(e.End(), end)
		p := Piece{Zero: e.Kind == ExtentZero, Length: hi - lo}
		if e.Kind == ExtentData {
			p.DataOffset = e.DataOffset + (lo - e.Start)
		}
		out = append(out, p)
	}
	return out
}

func sameExtents(a, b []Extent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func samePieces(a, b []Piece) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPersistentExtentMapMatchesReference drives a long, deterministic random
// sequence of Write/Zero/Resize operations through both the persistent map and
// the original flat reference model and asserts they are observably identical
// after every single step. It is the equivalence proof for the Phase 27
// structural-sharing rewrite.
func TestPersistentExtentMapMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(0x0f5a))
	pm := NewExtentMap()
	rm := &refMap{}

	// frontier biases offsets toward the write frontier (appends) and the tail
	// of the file, where the splice/merge logic is most exercised.
	frontier := func() uint64 {
		if rm.size == 0 || rng.Intn(2) == 0 {
			return uint64(rng.Intn(4096))
		}
		return rm.size - uint64(rng.Intn(int(minu(rm.size, 64)))) + uint64(rng.Intn(64))
	}

	check := func(step int) {
		t.Helper()
		if pm.Size() != rm.Size() {
			t.Fatalf("step %d: size %d != %d", step, pm.Size(), rm.Size())
		}
		if !sameExtents(pm.Extents(), rm.Extents()) {
			t.Fatalf("step %d: extents differ\n got %+v\nwant %+v", step, pm.Extents(), rm.Extents())
		}
		if pm.Count() != len(rm.Extents()) {
			t.Fatalf("step %d: count %d != %d", step, pm.Count(), len(rm.Extents()))
		}
		if rm.Size() > 0 {
			off := uint64(rng.Intn(int(rm.Size()) + 1))
			l := uint64(rng.Intn(256))
			got := pm.PlanRead(off, l)
			want := refPieces(rm, off, l)
			if !samePieces(got, want) {
				t.Fatalf("step %d: read plan differ at off=%d len=%d\n got %+v\nwant %+v", step, off, l, got, want)
			}
		}
	}

	const steps = 20000
	for i := 0; i < steps; i++ {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4, 5:
			off := frontier()
			l := uint64(1 + rng.Intn(512))
			do := uint64(rng.Intn(1 << 20))
			pm.Write(off, do, l)
			rm.Write(off, do, l)
		case 6, 7:
			off := frontier()
			l := uint64(1 + rng.Intn(256))
			pm.Zero(off, l)
			rm.Zero(off, l)
		case 8:
			var ns uint64
			if rm.size == 0 || rng.Intn(2) == 0 {
				ns = rm.size + uint64(rng.Intn(4096))
			} else {
				ns = uint64(rng.Intn(int(rm.size) + 1))
			}
			pm.Resize(ns)
			rm.Resize(ns)
		default:
			// read-only: fall through to the invariant check
		}
		check(i)
	}
}

// TestPlanReadIntoZeroAlloc pins the read hot path's allocation contract: a read
// plan over a heavily fragmented file must resolve into a caller-provided stack
// buffer with no heap allocation, however many extents the file accumulated. The
// allocation-free overlap walk is what keeps the 4 KiB random-read tail flat as
// a file's history grows.
func TestPlanReadIntoZeroAlloc(t *testing.T) {
	m := NewExtentMap()
	// 16k alternating data/zero extents: a fragmented file with a large E.
	for i := 0; i < 8192; i++ {
		m.Write(uint64(i)*8192, uint64(i)*16384, 4096)
	}
	if m.Count() < 8192 {
		t.Fatalf("want a fragmented map, got %d extents", m.Count())
	}
	var plan [4]Piece
	off := uint64(4000 * 8192)
	if got := len(m.PlanReadInto(off, 4096, plan[:0])); got != 1 {
		t.Fatalf("plan has %d pieces, want 1", got)
	}
	allocs := testing.AllocsPerRun(2000, func() {
		_ = m.PlanReadInto(off, 4096, plan[:0])
	})
	if allocs != 0 {
		t.Fatalf("PlanReadInto allocated %v times per call over %d extents, want 0", allocs, m.Count())
	}
}

func BenchmarkPlanReadIntoFragmented(b *testing.B) {
	m := NewExtentMap()
	for i := 0; i < 8192; i++ {
		m.Write(uint64(i)*8192, uint64(i)*16384, 4096)
	}
	var plan [4]Piece
	off := uint64(8000 * 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.PlanReadInto(off, 4096, plan[:0])
	}
}

// BenchmarkExtentMapAppend models the audit/WORM workload: a contiguous 4 KiB
// append at EOF (offset == size), so each write inserts exactly one extent and
// the cost must stay O(log E) however long the file's history grows.
func BenchmarkExtentMapAppend(b *testing.B) {
	m := NewExtentMap()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Write(uint64(i)*4096, uint64(i)*8192, 4096)
	}
}
