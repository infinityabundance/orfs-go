package index

// ExtentKind is the kind of a logical extent.
type ExtentKind uint8

const (
	// ExtentZero is a logically zeroed range.
	ExtentZero ExtentKind = iota
	// ExtentData points at immutable bytes at an absolute log offset.
	ExtentData
)

// Extent is one ordered, disjoint piece of a file's logical range. Data bytes
// are never rewritten: an overwrite appends new bytes to the log and repoints
// the logical range at them (Law I).
type Extent struct {
	Start      uint64
	Length     uint64
	Kind       ExtentKind
	DataOffset uint64
}

// End is the exclusive logical end.
func (e Extent) End() uint64 { return e.Start + e.Length }

// ExtentMap invariantly covers exactly [0, size) with ordered, disjoint
// extents.
type ExtentMap struct {
	extents []Extent
	size    uint64
}

// NewExtentMap returns an empty map.
func NewExtentMap() *ExtentMap { return &ExtentMap{} }

// Size is the logical file size.
func (m *ExtentMap) Size() uint64 { return m.size }

// Extents returns a copy of the extent list.
func (m *ExtentMap) Extents() []Extent { return append([]Extent(nil), m.extents...) }

// Write records that [offset, offset+len(data)) now reads the immutable bytes at
// dataOffset, zero-filling any gap between the old end and offset.
func (m *ExtentMap) Write(offset, dataOffset, length uint64) {
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

// Zero logically zeroes [offset, offset+length), extending the file if needed.
func (m *ExtentMap) Zero(offset, length uint64) {
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

// Resize changes the logical size. Shrinking drops visible extents beyond the
// new size but retains the underlying log bytes; growing zero-fills the newly
// exposed region, so truncated bytes are never resurrected.
func (m *ExtentMap) Resize(newSize uint64) {
	if newSize == m.size {
		return
	}
	if newSize > m.size {
		m.appendZero(newSize - m.size)
		m.size = newSize
		return
	}
	m.size = newSize
	m.trimToSize()
}

func (m *ExtentMap) appendZero(length uint64) {
	start := m.size
	if n := len(m.extents); n > 0 && m.extents[n-1].Kind == ExtentZero {
		m.extents[n-1].Length += length
	} else {
		m.extents = append(m.extents, Extent{Start: start, Length: length, Kind: ExtentZero})
	}
}

// replace overwrites [start, end) with a single extent of the given kind,
// preserving and re-splitting the surrounding pieces.
func (m *ExtentMap) replace(start, end uint64, kind ExtentKind, dataOffset uint64) {
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
			// A data extent split on the right loses its byte alignment; keep the
			// offset advanced by the consumed prefix so reads stay exact.
			rightLen := e.End() - end
			right := Extent{Start: end, Length: rightLen, Kind: e.Kind}
			if e.Kind == ExtentData {
				right.DataOffset = e.DataOffset + (end - e.Start)
			}
			out = append(out, right)
		}
	}
	out = append(out, Extent{Start: start, Length: end - start, Kind: kind, DataOffset: dataOffset})
	m.extents = mergeAdjacent(sortExtents(out))
}

func (m *ExtentMap) trimToSize() {
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

// Piece is a resolved read: bytes to read from the log, or a zero run.
type Piece struct {
	Zero       bool
	DataOffset uint64
	Length     uint64
}

// PlanRead resolves [offset, offset+length) into ordered pieces, clipped to the
// file size. A read beyond EOF yields no piece for that range.
func (m *ExtentMap) PlanRead(offset, length uint64) []Piece {
	return m.PlanReadInto(offset, length, nil)
}

// PlanReadInto is PlanRead into a caller-provided destination slice. The result
// is appended to dst[:0], so a caller that passes a small fixed-size array (its
// backing array on the stack) pays no heap allocation for the common, mostly
// single-extent read. If dst lacks capacity for every piece, append grows it
// and returns the new slice; the caller must use the returned slice.
func (m *ExtentMap) PlanReadInto(offset, length uint64, dst []Piece) []Piece {
	if length == 0 || offset >= m.size {
		return dst[:0]
	}
	end := offset + length
	if end > m.size {
		end = m.size
	}
	dst = dst[:0]
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
		dst = append(dst, p)
	}
	return dst
}

func sortExtents(in []Extent) []Extent {
	// insertion sort: extent lists are small (per-file segment counts are low).
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].Start > in[j].Start; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
	return in
}

func mergeAdjacent(in []Extent) []Extent {
	var out []Extent
	for _, e := range in {
		if e.Length == 0 {
			continue
		}
		if n := len(out); n > 0 {
			prev := &out[n-1]
			contiguous := prev.End() == e.Start
			sameKind := prev.Kind == e.Kind
			sameData := prev.Kind != ExtentData || prev.DataOffset+prev.Length == e.DataOffset
			if contiguous && sameKind && sameData {
				prev.Length += e.Length
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func maxu(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minu(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
