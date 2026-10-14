package index

import (
	"encoding/binary"
	"sort"

	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// Canonical index snapshot document.
//
// A snapshot is a versioned, deterministic (canonical) serialization of an
// *Index* sufficient to reconstruct it exactly at a committed root: the inode
// table, the directory-entry table, the named-snapshot table, and the recorded
// checkpoints. It is little-endian, field-explicit (no struct is reinterpreted
// over untrusted bytes), and every table is written in a fixed order (inodes by
// id, dentries by (parent, name), snapshots by name, checkpoints in replay
// order), so the same index always yields the same bytes.
//
// The document ends with a 32-byte BLAKE3 self-digest over every preceding byte.
// A reader recomputes it before trusting a single field, so a torn, truncated
// or tampered body is rejected rather than partially applied. This is the
// accelerator the INDEX_CHECKPOINT record carries; it is never load-bearing.
const (
	snapshotMagic     uint32 = 0x5358494F // "OIXS"
	snapshotVersion   uint32 = 1
	snapshotDigestLen        = 32
	snapshotMinDocLen        = 4 + 4 + 8 + 8 + 8 + 8 + 4 // header through inode count
)

// EncodeSnapshot renders the canonical snapshot document and returns it together
// with the 32-byte BLAKE3 digest of its content (the same digest recorded in the
// INDEX_CHECKPOINT payload).
func EncodeSnapshot(idx *Index) ([]byte, [32]byte, error) {
	if idx == nil {
		return nil, [32]byte{}, orfserr.Invalid("cannot encode a nil index")
	}
	b := make([]byte, 0, 64+len(idx.Inodes)*96+len(idx.Dentry)*32)
	b = binary.LittleEndian.AppendUint32(b, snapshotMagic)
	b = binary.LittleEndian.AppendUint32(b, snapshotVersion)
	b = binary.LittleEndian.AppendUint64(b, idx.Root)
	b = binary.LittleEndian.AppendUint64(b, idx.NextInodeID)
	b = binary.LittleEndian.AppendUint64(b, idx.CommittedRecords)
	b = binary.LittleEndian.AppendUint64(b, idx.MaxTxID)

	// Inodes, ascending by id.
	ids := make([]uint64, 0, len(idx.Inodes))
	for id := range idx.Inodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b = binary.LittleEndian.AppendUint32(b, uint32(len(ids)))
	for _, id := range ids {
		b = encodeSnapshotInode(b, idx.Inodes[id])
	}

	// Directory entries, ascending by (parent, name).
	keys := make([]DentryKey, 0, len(idx.Dentry))
	for k := range idx.Dentry {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Parent != keys[j].Parent {
			return keys[i].Parent < keys[j].Parent
		}
		return keys[i].Name < keys[j].Name
	})
	b = binary.LittleEndian.AppendUint32(b, uint32(len(keys)))
	for _, k := range keys {
		b = binary.LittleEndian.AppendUint64(b, k.Parent)
		b = appendSnapshotBytes(b, []byte(k.Name))
		b = binary.LittleEndian.AppendUint64(b, idx.Dentry[k])
	}

	// Named snapshots, ascending by name.
	names := make([]string, 0, len(idx.Snapshots))
	for n := range idx.Snapshots {
		names = append(names, n)
	}
	sort.Strings(names)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(names)))
	for _, n := range names {
		b = appendSnapshotBytes(b, []byte(n))
		b = binary.LittleEndian.AppendUint64(b, idx.Snapshots[n])
	}

	// Recorded checkpoints, in replay order.
	b = binary.LittleEndian.AppendUint32(b, uint32(len(idx.Checkpoints)))
	for _, c := range idx.Checkpoints {
		b = binary.LittleEndian.AppendUint64(b, c.Root)
		b = binary.LittleEndian.AppendUint64(b, c.InodeCount)
		b = binary.LittleEndian.AppendUint64(b, c.DentryCount)
		b = append(b, c.Digest[:]...)
	}

	sum := blake3.Sum256(b)
	b = append(b, sum[:]...)
	return b, sum, nil
}

func encodeSnapshotInode(b []byte, in *Inode) []byte {
	b = binary.LittleEndian.AppendUint64(b, in.ID)
	b = binary.LittleEndian.AppendUint64(b, in.Generation)
	b = binary.LittleEndian.AppendUint64(b, in.Version)
	b = binary.LittleEndian.AppendUint64(b, in.LatestRecordOffset)
	b = binary.LittleEndian.AppendUint64(b, in.CreationRoot)
	b = append(b, in.Kind)
	if in.Live {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = binary.LittleEndian.AppendUint32(b, in.Mode)
	b = binary.LittleEndian.AppendUint32(b, in.UID)
	b = binary.LittleEndian.AppendUint32(b, in.GID)
	b = binary.LittleEndian.AppendUint32(b, in.Nlink)
	b = binary.LittleEndian.AppendUint64(b, in.Size)
	b = binary.LittleEndian.AppendUint64(b, in.Rdev)
	b = appendSnapshotTimespec(b, in.Atime)
	b = appendSnapshotTimespec(b, in.Mtime)
	b = appendSnapshotTimespec(b, in.Ctime)
	b = appendSnapshotBytes(b, in.Target)

	// Extended attributes, ascending by name.
	xnames := make([]string, 0, len(in.Xattrs))
	for n := range in.Xattrs {
		xnames = append(xnames, n)
	}
	sort.Strings(xnames)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(xnames)))
	for _, n := range xnames {
		b = appendSnapshotBytes(b, []byte(n))
		b = appendSnapshotBytes(b, in.Xattrs[n])
	}

	// Extent map: the logical size plus the ordered, disjoint extents.
	var exts []Extent
	var size uint64
	if in.Extents != nil {
		exts = in.Extents.extents
		size = in.Extents.size
	}
	b = binary.LittleEndian.AppendUint64(b, size)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(exts)))
	for _, e := range exts {
		b = binary.LittleEndian.AppendUint64(b, e.Start)
		b = binary.LittleEndian.AppendUint64(b, e.Length)
		b = append(b, byte(e.Kind))
		b = binary.LittleEndian.AppendUint64(b, e.DataOffset)
	}
	return b
}

func appendSnapshotTimespec(b []byte, t record.Timespec) []byte {
	b = binary.LittleEndian.AppendUint64(b, uint64(t.Secs))
	b = binary.LittleEndian.AppendUint32(b, t.Nanos)
	return b
}

func appendSnapshotBytes(b, p []byte) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(p)))
	return append(b, p...)
}

// DecodeSnapshot parses a canonical snapshot document, verifying its self-digest
// before a single field is trusted. It returns the reconstructed index and the
// verified digest. The index is valid only for the committed root it names;
// callers must confirm that against the authoritative log.
func DecodeSnapshot(doc []byte) (*Index, [32]byte, error) {
	if len(doc) < snapshotMinDocLen+snapshotDigestLen {
		return nil, [32]byte{}, orfserr.Corrupt("index snapshot is %d bytes, too short", len(doc))
	}
	content := doc[:len(doc)-snapshotDigestLen]
	wantSum := blake3.Sum256(content)
	var recorded [32]byte
	copy(recorded[:], doc[len(doc)-snapshotDigestLen:])
	if wantSum != recorded {
		return nil, [32]byte{}, orfserr.Corrupt("index snapshot self-digest mismatch (torn, truncated, or tampered)")
	}

	r := &snapshotReader{b: content}
	if r.u32() != snapshotMagic {
		return nil, [32]byte{}, orfserr.Corrupt("index snapshot magic mismatch")
	}
	if v := r.u32(); v != snapshotVersion {
		return nil, [32]byte{}, orfserr.Compatibility("index snapshot version %d is not implemented (%d)", v, snapshotVersion)
	}
	idx := New()
	idx.Root = r.u64()
	idx.NextInodeID = r.u64()
	idx.CommittedRecords = r.u64()
	idx.MaxTxID = r.u64()

	inodeCount := r.count(minInodeEntry)
	for i := uint32(0); i < inodeCount && r.err == nil; i++ {
		in, err := decodeSnapshotInode(r)
		if err != nil {
			return nil, [32]byte{}, err
		}
		idx.Inodes[in.ID] = in
	}

	dentryCount := r.count(minDentryEntry)
	for i := uint32(0); i < dentryCount && r.err == nil; i++ {
		parent := r.u64()
		name := r.bytes(nameUpperBound)
		child := r.u64()
		idx.Dentry[DentryKey{Parent: parent, Name: string(name)}] = child
	}

	snapCount := r.count(minSnapshotEntry)
	for i := uint32(0); i < snapCount && r.err == nil; i++ {
		name := r.bytes(nameUpperBound)
		root := r.u64()
		idx.Snapshots[string(name)] = root
	}

	cpCount := r.count(minCheckpointEntry)
	for i := uint32(0); i < cpCount && r.err == nil; i++ {
		var c Checkpoint
		c.Root = r.u64()
		c.InodeCount = r.u64()
		c.DentryCount = r.u64()
		if r.need(snapshotDigestLen) {
			copy(c.Digest[:], r.b[r.off:r.off+snapshotDigestLen])
			r.off += snapshotDigestLen
		}
		idx.Checkpoints = append(idx.Checkpoints, c)
	}
	if err := r.finish(); err != nil {
		return nil, [32]byte{}, err
	}
	return idx, recorded, nil
}

func decodeSnapshotInode(r *snapshotReader) (*Inode, error) {
	var in Inode
	in.ID = r.u64()
	in.Generation = r.u64()
	in.Version = r.u64()
	in.LatestRecordOffset = r.u64()
	in.CreationRoot = r.u64()
	in.Kind = r.u8()
	in.Live = r.u8() != 0
	in.Mode = r.u32()
	in.UID = r.u32()
	in.GID = r.u32()
	in.Nlink = r.u32()
	in.Size = r.u64()
	in.Rdev = r.u64()
	in.Atime = r.timespec()
	in.Mtime = r.timespec()
	in.Ctime = r.timespec()
	in.Target = r.bytes(symlinkUpperBound)

	xattrCount := r.count(minXattrEntry)
	in.Xattrs = make(map[string][]byte, capped(xattrCount, 64))
	for i := uint32(0); i < xattrCount && r.err == nil; i++ {
		name := r.bytes(xattrNameUpperBound)
		val := r.bytes(xattrValueUpperBound)
		in.Xattrs[string(name)] = val
	}

	extentSize := r.u64()
	extentCount := r.count(minExtentEntry)
	m := &ExtentMap{size: extentSize}
	if extentCount > 0 {
		m.extents = make([]Extent, 0, capped(extentCount, 4096))
	}
	for i := uint32(0); i < extentCount && r.err == nil; i++ {
		var e Extent
		e.Start = r.u64()
		e.Length = r.u64()
		e.Kind = ExtentKind(r.u8())
		e.DataOffset = r.u64()
		m.extents = append(m.extents, e)
	}
	in.Extents = m
	if r.err != nil {
		return nil, r.err
	}
	return &in, nil
}

// Minimum on-the-wire sizes of one table entry, used to bound a table count by
// the bytes that remain so an adversarial prefix cannot force an unbounded loop
// or allocation. They are lower bounds (each entry is at least this many bytes),
// so they are safe for the bound.
const (
	minInodeEntry      = 120
	minDentryEntry     = 20
	minSnapshotEntry   = 12
	minCheckpointEntry = 56
	minXattrEntry      = 8
	minExtentEntry     = 25
)

// Upper bounds used only to keep an adversarial length prefix from driving an
// unbounded allocation; they mirror the committed format limits.
const (
	nameUpperBound          = 255
	symlinkUpperBound       = 4096
	xattrNameUpperBound     = 255
	xattrValueUpperBound    = 65536
	snapshotMaxTableEntries = 1 << 30
)

// snapshotReader is a bounds-checked cursor over a snapshot document.
type snapshotReader struct {
	b   []byte
	off int
	err error
}

// capped bounds a speculative pre-allocation by an untrusted count.
func capped(n uint32, limit int) int {
	if uint64(n) > uint64(limit) {
		return limit
	}
	return int(n)
}

func (r *snapshotReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if n < 0 || r.off+n > len(r.b) {
		r.err = orfserr.Corrupt("index snapshot truncated at offset %d", r.off)
		return false
	}
	return true
}

func (r *snapshotReader) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}

func (r *snapshotReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v
}

func (r *snapshotReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v
}

func (r *snapshotReader) timespec() record.Timespec {
	return record.Timespec{Secs: int64(r.u64()), Nanos: r.u32()}
}

func (r *snapshotReader) bytes(max int) []byte {
	n := int(r.u32())
	if r.err != nil {
		return nil
	}
	if n < 0 || n > max {
		r.err = orfserr.Corrupt("index snapshot field length %d exceeds bound %d", n, max)
		return nil
	}
	if !r.need(n) {
		return nil
	}
	out := make([]byte, n)
	copy(out, r.b[r.off:r.off+n])
	r.off += n
	return out
}

// count reads a table count, bounding it so a corrupt prefix cannot request a
// huge allocation or loop. The remaining bytes must be able to hold every entry
// at the given minimum entry size.
func (r *snapshotReader) count(minEntry int) uint32 {
	n := r.u32()
	if r.err != nil {
		return 0
	}
	remaining := len(r.b) - r.off
	if n > snapshotMaxTableEntries || (minEntry > 0 && uint64(n) > uint64(remaining)/uint64(minEntry)) {
		r.err = orfserr.Corrupt("index snapshot table count %d exceeds the remaining %d byte(s)", n, remaining)
		return 0
	}
	return n
}

func (r *snapshotReader) finish() error {
	if r.err != nil {
		return r.err
	}
	if r.off != len(r.b) {
		return orfserr.Corrupt("index snapshot has %d trailing byte(s)", len(r.b)-r.off)
	}
	return nil
}

// Equal reports whether two indices are the same reconstructed namespace: every
// field that a replay produces, compared through the canonical snapshot
// encoding. A nil operand is never equal to a non-nil one.
func (idx *Index) Equal(other *Index) bool {
	if idx == nil || other == nil {
		return idx == other
	}
	a, _, err := EncodeSnapshot(idx)
	if err != nil {
		return false
	}
	b, _, err := EncodeSnapshot(other)
	if err != nil {
		return false
	}
	return string(a) == string(b)
}
