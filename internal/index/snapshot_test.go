package index

import (
	"encoding/binary"
	"testing"

	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/record"
)

// buildTestIndex builds a small but non-trivial index exercising every field
// the snapshot must carry: files with extents and xattrs, a directory, a
// symlink, dentries, named snapshots, and recorded checkpoints.
func buildTestIndex() *Index {
	idx := New()
	idx.Root = 4096
	idx.NextInodeID = 4
	idx.CommittedRecords = 42
	idx.MaxTxID = 7

	idx.Inodes[1] = &Inode{
		ID: 1, Kind: record.KindDir, Mode: 0o755, Nlink: 2, Live: true, Version: 1,
		Xattrs: map[string][]byte{}, Extents: NewExtentMap(),
	}
	ext := NewExtentMap()
	ext.Write(0, 1000, 10)
	ext.Zero(20, 5)
	idx.Inodes[2] = &Inode{
		ID: 2, Kind: record.KindFile, Mode: 0o644, UID: 7, GID: 8, Nlink: 1,
		Size: ext.Size(), Live: true, Version: 3, Generation: 2,
		LatestRecordOffset: 999, CreationRoot: 100, Extents: ext,
		Atime:  record.Timespec{Secs: 5, Nanos: 6},
		Mtime:  record.Timespec{Secs: 7, Nanos: 8},
		Ctime:  record.Timespec{Secs: 9, Nanos: 10},
		Xattrs: map[string][]byte{"user.a": []byte("v"), "user.b": {1, 2, 3}},
	}
	idx.Inodes[3] = &Inode{
		ID: 3, Kind: record.KindSymlink, Mode: 0o777, Nlink: 1, Size: 3,
		Live: true, Version: 1, Target: []byte("abc"),
		Xattrs: map[string][]byte{}, Extents: NewExtentMap(),
	}
	idx.Dentry[DentryKey{Parent: 1, Name: "a"}] = 2
	idx.Dentry[DentryKey{Parent: 1, Name: "b"}] = 3
	idx.Snapshots["first"] = 100
	idx.Snapshots["second"] = 2048
	idx.Checkpoints = append(idx.Checkpoints,
		Checkpoint{Root: 100, InodeCount: 2, DentryCount: 1, Digest: [32]byte{9}},
		Checkpoint{Root: 2048, InodeCount: 3, DentryCount: 2, Digest: [32]byte{0xaa}},
	)
	return idx
}

func TestSnapshotRoundTrip(t *testing.T) {
	idx := buildTestIndex()
	doc, sum, err := EncodeSnapshot(idx)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, gotSum, err := DecodeSnapshot(doc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotSum != sum {
		t.Fatalf("digest mismatch: %x != %x", gotSum, sum)
	}
	if !got.Equal(idx) {
		t.Fatal("decoded index is not equal to the original")
	}
	if got.Root != idx.Root || got.NextInodeID != idx.NextInodeID ||
		got.CommittedRecords != idx.CommittedRecords || got.MaxTxID != idx.MaxTxID {
		t.Fatal("scalar fields did not round-trip")
	}
	// The extent map must reconstruct exactly (not merely observe equal bytes).
	if got.Inodes[2].Extents.Size() != idx.Inodes[2].Extents.Size() {
		t.Fatal("extent size did not round-trip")
	}
	if got.Inodes[2].Extents.Count() != 2 {
		t.Fatalf("want 2 extents, got %d", got.Inodes[2].Extents.Count())
	}
}

func TestSnapshotSelfDigestRejectsTamper(t *testing.T) {
	idx := buildTestIndex()
	doc, _, err := EncodeSnapshot(idx)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Flip a content byte (not the trailing digest).
	tampered := append([]byte(nil), doc...)
	tampered[len(tampered)/2] ^= 0xff
	if _, _, err := DecodeSnapshot(tampered); err == nil {
		t.Fatal("a tampered snapshot must be rejected")
	}
	// Truncate the document.
	if _, _, err := DecodeSnapshot(doc[:len(doc)-1]); err == nil {
		t.Fatal("a truncated snapshot must be rejected")
	}
	// A too-short document.
	if _, _, err := DecodeSnapshot(doc[:8]); err == nil {
		t.Fatal("a short snapshot must be rejected")
	}
}

func TestSnapshotHugeCountRejected(t *testing.T) {
	// Overwrite the inode-count field with a huge value and repair the self-digest
	// so the adversarial count is what is tested, not the digest.
	idx := buildTestIndex()
	doc, _, err := EncodeSnapshot(idx)
	if err != nil {
		t.Fatal(err)
	}
	content := append([]byte(nil), doc[:len(doc)-snapshotDigestLen]...)
	const inodeCountOff = 4 + 4 + 8 + 8 + 8 + 8 // magic..max_tx_id
	binary.LittleEndian.PutUint32(content[inodeCountOff:], 0xffffffff)
	sum := blake3.Sum256(content)
	content = append(content, sum[:]...)
	if _, _, err := DecodeSnapshot(content); err == nil {
		t.Fatal("a huge table count must be rejected")
	}
}

func TestSnapshotCanonicalDeterministic(t *testing.T) {
	// The same logical index, built by inserting map entries in a different
	// order, must serialize to identical bytes.
	a := New()
	a.Root = 10
	a.Inodes[0] = &Inode{ID: 0, Kind: record.KindFile, Xattrs: map[string][]byte{"z": {1}, "a": {2}}, Extents: NewExtentMap()}
	a.Inodes[1] = &Inode{ID: 1, Kind: record.KindFile, Xattrs: map[string][]byte{}, Extents: NewExtentMap()}
	a.Dentry[DentryKey{1, "y"}] = 0
	a.Dentry[DentryKey{1, "x"}] = 1
	a.Snapshots["b"] = 1
	a.Snapshots["a"] = 2

	b := New()
	b.Root = 10
	b.Inodes[1] = &Inode{ID: 1, Kind: record.KindFile, Xattrs: map[string][]byte{}, Extents: NewExtentMap()}
	b.Inodes[0] = &Inode{ID: 0, Kind: record.KindFile, Xattrs: map[string][]byte{"a": {2}, "z": {1}}, Extents: NewExtentMap()}
	b.Dentry[DentryKey{1, "x"}] = 1
	b.Dentry[DentryKey{1, "y"}] = 0
	b.Snapshots["a"] = 2
	b.Snapshots["b"] = 1

	da, _, err := EncodeSnapshot(a)
	if err != nil {
		t.Fatal(err)
	}
	db, _, err := EncodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(da) != string(db) {
		t.Fatal("snapshot encoding is not canonical")
	}
}
