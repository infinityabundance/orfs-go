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

// ExtentMap invariantly covers exactly [0, size) with ordered, disjoint,
// maximal extents.
//
// # Structural sharing (structural-sharing parity with the reference's Phase 27)
//
// The map is a persistent (path-copying) treap keyed by Start, so cloning it —
// which happens once per inode clone on the publish path — is a single pointer
// copy and never deep-copies the extent list, and a write path-copies only the
// O(log E) nodes on the affected range plus the extents it splices. Write
// latency is therefore O(log E + k) rather than O(E), so an append-only file
// does not degrade quadratically as its history grows. This mirrors the Rust
// reference (`.reference/OrFS/src/extent.rs`, "Structural sharing (Phase 27)"),
// which keys the map in a persistent `PMap` for exactly this reason; the
// observable extent sequence is identical to a flat ordered list, so the
// canonical snapshot encoding and every fixture are unaffected.
//
// The live writer mutates its map in place (assigning the new root), while a
// published read view holds a map that shares nodes with it. Because nodes are
// never mutated — a mutation allocates a fresh path — a frozen view can never
// observe a later write.
type ExtentMap struct {
	root *extNode
	size uint64
}

// extNode is one immutable treap node. Its fields are written once, when the
// node is created, and never mutated; a path copy replaces the nodes on the
// root-to-key path and shares every untouched subtree with the prior version.
type extNode struct {
	ext         Extent
	prio        uint64
	left, right *extNode
}

// extPriority is a deterministic hash of the key (splitmix64). Using a
// key-derived priority gives a treap whose shape depends only on the key set,
// not on insertion order, so the tree — and therefore the extent sequence — is
// reproducible and independent of the commit history that produced it.
func extPriority(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// setExt returns a new subtree in which ext is bound to key ext.Start, copying
// only the nodes on the root-to-key path. The receiver is unchanged.
func setExt(n *extNode, ext Extent) *extNode {
	if n == nil {
		return &extNode{ext: ext, prio: extPriority(ext.Start)}
	}
	switch {
	case ext.Start < n.ext.Start:
		l := setExt(n.left, ext)
		nn := &extNode{ext: n.ext, prio: n.prio, left: l, right: n.right}
		if l.prio > nn.prio {
			return rotRight(nn)
		}
		return nn
	case ext.Start > n.ext.Start:
		r := setExt(n.right, ext)
		nn := &extNode{ext: n.ext, prio: n.prio, left: n.left, right: r}
		if r.prio > nn.prio {
			return rotLeft(nn)
		}
		return nn
	default:
		return &extNode{ext: ext, prio: n.prio, left: n.left, right: n.right}
	}
}

// delExt returns a new subtree without the key start. A missing key returns the
// receiver unchanged.
func delExt(n *extNode, start uint64) *extNode {
	if n == nil {
		return nil
	}
	switch {
	case start < n.ext.Start:
		l := delExt(n.left, start)
		if l == n.left {
			return n
		}
		return &extNode{ext: n.ext, prio: n.prio, left: l, right: n.right}
	case start > n.ext.Start:
		r := delExt(n.right, start)
		if r == n.right {
			return n
		}
		return &extNode{ext: n.ext, prio: n.prio, left: n.left, right: r}
	default:
		return mergeExt(n.left, n.right)
	}
}

// mergeExt joins two treaps whose key ranges are disjoint and ordered.
func mergeExt(a, b *extNode) *extNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if a.prio > b.prio {
		return &extNode{ext: a.ext, prio: a.prio, left: a.left, right: mergeExt(a.right, b)}
	}
	return &extNode{ext: b.ext, prio: b.prio, left: mergeExt(a, b.left), right: b.right}
}

func rotRight(n *extNode) *extNode {
	l := n.left
	return &extNode{
		ext: l.ext, prio: l.prio, left: l.left,
		right: &extNode{ext: n.ext, prio: n.prio, left: l.right, right: n.right},
	}
}

func rotLeft(n *extNode) *extNode {
	r := n.right
	return &extNode{
		ext: r.ext, prio: r.prio, right: r.right,
		left: &extNode{ext: n.ext, prio: n.prio, left: n.left, right: r.left},
	}
}

// walkExt visits every extent in ascending Start order; fn returns false to stop.
func walkExt(n *extNode, fn func(Extent) bool) bool {
	if n == nil {
		return true
	}
	if !walkExt(n.left, fn) {
		return false
	}
	if !fn(n.ext) {
		return false
	}
	return walkExt(n.right, fn)
}

// floorExt returns the extent with the greatest Start <= key, or nil.
func floorExt(n *extNode, key uint64) *extNode {
	var best *extNode
	for n != nil {
		if n.ext.Start <= key {
			best = n
			n = n.right
		} else {
			n = n.left
		}
	}
	return best
}

// geNode returns the least node with Start >= key, or nil. It allocates
// nothing, so it is safe on the read hot path.
func geNode(n *extNode, key uint64) *extNode {
	var best *extNode
	for n != nil {
		if n.ext.Start >= key {
			best = n
			n = n.left
		} else {
			n = n.right
		}
	}
	return best
}

// walkRange visits, in ascending Start order, every extent that intersects
// [start, end). It prunes the subtrees that cannot intersect, so it is
// O(log E + k) and — crucially for the read path — allocates nothing.
//
// Pruning relies on the map invariant (extents are ordered, disjoint, and cover
// [0, size)): at most one extent contains any point, so if a node's Start is not
// greater than `start`, no key in its left subtree can intersect [start, end).
func walkRange(n *extNode, start, end uint64, fn func(Extent) bool) bool {
	if n == nil {
		return true
	}
	if n.ext.Start > start {
		if !walkRange(n.left, start, end, fn) {
			return false
		}
	}
	if n.ext.End() > start && n.ext.Start < end {
		if !fn(n.ext) {
			return false
		}
	}
	if n.ext.Start < end {
		if !walkRange(n.right, start, end, fn) {
			return false
		}
	}
	return true
}

// NewExtentMap returns an empty map.
func NewExtentMap() *ExtentMap { return &ExtentMap{} }

// Size is the logical file size.
func (m *ExtentMap) Size() uint64 { return m.size }

// Count is the number of extents in the map.
func (m *ExtentMap) Count() int {
	n := 0
	walkExt(m.root, func(Extent) bool { n++; return true })
	return n
}

// Extents returns a copy of the extent list in ascending logical order.
func (m *ExtentMap) Extents() []Extent {
	var out []Extent
	walkExt(m.root, func(e Extent) bool { out = append(out, e); return true })
	return out
}

// Clone returns an independent map. Because nodes are immutable and shared,
// this is O(1): a later mutation of either map path-copies only the nodes it
// changes and leaves the other map untouched (structural sharing, Phase 27).
func (m *ExtentMap) Clone() *ExtentMap {
	return &ExtentMap{root: m.root, size: m.size}
}

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

// lastExt returns the rightmost extent, which by invariant ends at m.size.
func (m *ExtentMap) lastExt() *Extent {
	n := m.root
	if n == nil {
		return nil
	}
	for n.right != nil {
		n = n.right
	}
	return &n.ext
}

func (m *ExtentMap) appendZero(length uint64) {
	if length == 0 {
		return
	}
	start := m.size
	if last := m.lastExt(); last != nil && last.Kind == ExtentZero {
		e := *last
		e.Length += length
		m.root = setExt(m.root, e)
		return
	}
	m.root = setExt(m.root, Extent{Start: start, Length: length, Kind: ExtentZero})
}

// replace overwrites [start, end) with a single extent of the given kind,
// preserving and re-splitting the surrounding pieces. It touches only the
// overlapping extents and the two neighbours that can merge with the result, so
// it is O(log E + k) rather than a rebuild of the whole map. The result is
// identical to a full sort-and-merge of the extent list.
func (m *ExtentMap) replace(start, end uint64, kind ExtentKind, dataOffset uint64) {
	if start >= end {
		return
	}
	var removes []uint64
	var firstExt, lastExt Extent
	found := false
	walkRange(m.root, start, end, func(e Extent) bool {
		removes = append(removes, e.Start)
		if !found {
			firstExt = e
			found = true
		}
		lastExt = e
		return true
	})

	// The new pieces, in ascending order.
	var pieces []Extent
	if found && firstExt.Start < start {
		pieces = append(pieces, Extent{Start: firstExt.Start, Length: start - firstExt.Start, Kind: firstExt.Kind, DataOffset: firstExt.DataOffset})
	}
	pieces = append(pieces, Extent{Start: start, Length: end - start, Kind: kind, DataOffset: dataOffset})
	if found && lastExt.End() > end {
		right := Extent{Start: end, Length: lastExt.End() - end, Kind: lastExt.Kind}
		if lastExt.Kind == ExtentData {
			right.DataOffset = lastExt.DataOffset + (end - lastExt.Start)
		}
		pieces = append(pieces, right)
	}

	// The left neighbour merges only if it ends exactly where the first piece
	// begins; the right neighbour only if it begins exactly where the last ends.
	var left *Extent
	if pieces[0].Start > 0 {
		if l := floorExt(m.root, pieces[0].Start-1); l != nil && l.ext.End() == pieces[0].Start {
			c := l.ext
			left = &c
		}
	}
	var right *Extent
	if n := geNode(m.root, pieces[len(pieces)-1].End()); n != nil && n.ext.Start == pieces[len(pieces)-1].End() {
		c := n.ext
		right = &c
	}

	for _, s := range removes {
		m.root = delExt(m.root, s)
	}
	if left != nil {
		m.root = delExt(m.root, left.Start)
	}
	if right != nil {
		m.root = delExt(m.root, right.Start)
	}

	seq := pieces
	if left != nil {
		seq = append([]Extent{*left}, seq...)
	}
	if right != nil {
		seq = append(seq, *right)
	}
	for _, e := range mergeAdjacent(seq) {
		m.root = setExt(m.root, e)
	}
}

func (m *ExtentMap) trimToSize() {
	var kept []Extent
	walkExt(m.root, func(e Extent) bool {
		if e.Start >= m.size {
			return true
		}
		if e.End() > m.size {
			e.Length = m.size - e.Start
		}
		kept = append(kept, e)
		return true
	})
	m.root = nil
	for _, e := range mergeAdjacent(kept) {
		m.root = setExt(m.root, e)
	}
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
// and returns the new slice; the caller must use the returned slice. It visits
// only the extents that intersect the range (O(log E + pieces)).
func (m *ExtentMap) PlanReadInto(offset, length uint64, dst []Piece) []Piece {
	if length == 0 || offset >= m.size {
		return dst[:0]
	}
	end := offset + length
	if end > m.size {
		end = m.size
	}
	dst = dst[:0]
	walkRange(m.root, offset, end, func(e Extent) bool {
		lo := maxu(e.Start, offset)
		hi := minu(e.End(), end)
		p := Piece{Zero: e.Kind == ExtentZero, Length: hi - lo}
		if e.Kind == ExtentData {
			p.DataOffset = e.DataOffset + (lo - e.Start)
		}
		dst = append(dst, p)
		return true
	})
	return dst
}

// mergeAdjacent folds an ascending, non-overlapping extent sequence into its
// maximal form, joining neighbours that are contiguous, of the same kind, and
// (for data) contiguous in the log.
func mergeAdjacent(in []Extent) []Extent {
	out := in[:0]
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
