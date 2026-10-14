package index

import "testing"

func TestExtentMapWriteOverwrite(t *testing.T) {
	m := NewExtentMap()
	m.Write(0, 1000, 12) // "hello, orfs\n" lives at log offset 1000
	if m.Size() != 12 {
		t.Fatalf("size = %d", m.Size())
	}
	m.Write(7, 5000, 4) // overwrite "orfs" with 4 bytes at log offset 5000
	if m.Size() != 12 {
		t.Fatalf("size after overwrite = %d", m.Size())
	}
	pieces := m.PlanRead(0, 12)
	var total uint64
	for _, p := range pieces {
		total += p.Length
	}
	if total != 12 {
		t.Fatalf("plan covers %d bytes: %+v", total, pieces)
	}
	// The pieces must be, in order: [0,7)@1000, [7,11)@5000, [11,12)@1011.
	want := []Piece{{DataOffset: 1000, Length: 7}, {DataOffset: 5000, Length: 4}, {DataOffset: 1011, Length: 1}}
	if len(pieces) != len(want) {
		t.Fatalf("pieces = %+v", pieces)
	}
	for i := range want {
		if pieces[i] != want[i] {
			t.Fatalf("piece %d = %+v, want %+v", i, pieces[i], want[i])
		}
	}
}

func TestExtentMapGrowShrink(t *testing.T) {
	m := NewExtentMap()
	m.Write(0, 100, 10)
	m.Resize(20) // grow: zero-fill
	if m.Size() != 20 {
		t.Fatalf("size = %d", m.Size())
	}
	p := m.PlanRead(10, 10)
	if len(p) != 1 || !p[0].Zero || p[0].Length != 10 {
		t.Fatalf("grown region = %+v", p)
	}
	m.Resize(5) // shrink
	if m.Size() != 5 {
		t.Fatalf("size = %d", m.Size())
	}
	p = m.PlanRead(4, 10)
	if len(p) != 1 || p[0].Length != 1 || p[0].DataOffset != 104 {
		t.Fatalf("clamped read = %+v", p)
	}
}
