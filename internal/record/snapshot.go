package record

import "github.com/infinityabundance/orfs-go/internal/orfserr"

// SnapshotRoot publishes a named historical root. On replay it sets
// snapshots[name] = root (an alias table; no inode impact).
type SnapshotRoot struct {
	Root uint64
	Name []byte
}

// Encode renders the SNAPSHOT_ROOT payload.
func (t SnapshotRoot) Encode() []byte {
	b := make([]byte, 4+8+4+len(t.Name))
	putU32(b[0:4], MagicSnapshotRoot)
	putU64(b[4:12], t.Root)
	putU32(b[12:16], uint32(len(t.Name)))
	copy(b[16:], t.Name)
	return appendCRC(b)
}

// DecodeSnapshotRoot parses a SNAPSHOT_ROOT payload.
func DecodeSnapshotRoot(b []byte) (SnapshotRoot, error) {
	var t SnapshotRoot
	r := &payloadReader{b: b}
	if r.u32() != MagicSnapshotRoot {
		return t, orfserr.Corrupt("SNAPSHOT_ROOT magic mismatch")
	}
	t.Root = r.u64()
	nameLen := int(r.u32())
	if r.err == nil && nameLen > NameMax {
		return t, orfserr.Corrupt("SNAPSHOT_ROOT name length %d exceeds %d", nameLen, NameMax)
	}
	if r.need(nameLen) {
		t.Name = append([]byte(nil), r.b[r.off:r.off+nameLen]...)
		r.off += nameLen
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	if err := validateName(t.Name); err != nil {
		return t, err
	}
	return t, nil
}

// IndexCheckpoint is the variable INDEX_CHECKPOINT payload. The embedded index
// document is an accelerator, never load-bearing.
type IndexCheckpoint struct {
	Root        uint64
	InodeCount  uint64
	DentryCount uint64
	Digest      [32]byte
	IndexBytes  []byte
}

// Encode renders the INDEX_CHECKPOINT payload.
func (t IndexCheckpoint) Encode() []byte {
	b := make([]byte, IndexCheckpointFixedLen+len(t.IndexBytes))
	putU32(b[0:4], MagicIndexCheckpoint)
	putU64(b[4:12], t.Root)
	putU64(b[12:20], t.InodeCount)
	putU64(b[20:28], t.DentryCount)
	copy(b[28:60], t.Digest[:])
	putU32(b[60:64], uint32(len(t.IndexBytes)))
	copy(b[64:], t.IndexBytes)
	return appendCRC(b)
}

// DecodeIndexCheckpoint parses an INDEX_CHECKPOINT payload.
func DecodeIndexCheckpoint(b []byte) (IndexCheckpoint, error) {
	var t IndexCheckpoint
	r := &payloadReader{b: b}
	if r.u32() != MagicIndexCheckpoint {
		return t, orfserr.Corrupt("INDEX_CHECKPOINT magic mismatch")
	}
	t.Root = r.u64()
	t.InodeCount = r.u64()
	t.DentryCount = r.u64()
	if r.need(32) {
		copy(t.Digest[:], r.b[r.off:r.off+32])
		r.off += 32
	}
	idxLen := int(r.u32())
	if r.err == nil && idxLen > 64*1024*1024 {
		return t, orfserr.Corrupt("INDEX_CHECKPOINT index length %d exceeds bound", idxLen)
	}
	if r.need(idxLen) {
		t.IndexBytes = append([]byte(nil), r.b[r.off:r.off+idxLen]...)
		r.off += idxLen
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	return t, nil
}
