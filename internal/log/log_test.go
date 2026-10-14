package log

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/wm"
)

func testGenesis() record.Genesis {
	return record.Genesis{
		UUID:        [16]byte{0xaa, 0xbb, 0xcc, 0xdd},
		SegmentSize: record.DefaultSegmentSize,
	}
}

func inodeCreate(id uint64) *record.Record {
	p := record.InodeCreate{InodeID: id, Generation: 1, Kind: record.KindFile, Mode: 0o644, Nlink: 1}.Encode()
	return &record.Record{Header: record.Header{RecordType: record.TypeInodeCreate, PayloadLength: uint64(len(p))}, Payload: p}
}

func TestCreatePublishScan(t *testing.T) {
	backing := t.TempDir()
	l, err := Create(backing, testGenesis())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	genesisRoot := l.CommittedRoot()
	if genesisRoot != record.EnvelopeOverhead+record.GenesisPayloadLen {
		t.Fatalf("genesis root = %d", genesisRoot)
	}
	root, err := l.Publish([]*record.Record{inodeCreate(5)})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if root <= genesisRoot {
		t.Fatalf("root did not advance: %d", root)
	}
	l.Close()

	scan, err := ScanLog(Dir(backing))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.CommittedRoot != root {
		t.Fatalf("scan root = %d, want %d", scan.CommittedRoot, root)
	}
	// genesis + TX_BEGIN + one data record + TX_COMMIT = 4.
	if scan.CommittedRecordCount != 4 {
		t.Fatalf("committed records = %d, want 4", scan.CommittedRecordCount)
	}
	if scan.Tail != nil {
		t.Fatalf("unexpected tail: %+v", scan.Tail)
	}
	if !scan.HasGenesis || scan.Genesis.UUID != testGenesis().UUID {
		t.Fatalf("genesis identity lost: %+v", scan.Genesis)
	}
	// Reopen read-write and publish again.
	l2, err := Open(backing, ReadWrite)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer l2.Close()
	if l2.CommittedRoot() != root {
		t.Fatalf("reopened root = %d, want %d", l2.CommittedRoot(), root)
	}
	root2, err := l2.Publish([]*record.Record{inodeCreate(6)})
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if root2 <= root {
		t.Fatal("second root did not advance")
	}
}

func TestTornTailRecovery(t *testing.T) {
	backing := t.TempDir()
	l, _ := Create(backing, testGenesis())
	root, _ := l.Publish([]*record.Record{inodeCreate(5)})
	l.Close()

	// Simulate a crash: a wholly new trailing segment holding a torn frame.
	// Its id is the committed root, so quarantine moves it (a segment that also
	// holds committed bytes would instead be retained in place, untouched).
	tailName := segment.SegmentName(root)
	tailPath := filepath.Join(Dir(backing), tailName)
	if err := os.WriteFile(tailPath, []byte{0x4F, 0x52, 0x46, 0x53, 0x01, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}

	scan, err := ScanLog(Dir(backing))
	if err != nil {
		t.Fatalf("scan after tear: %v", err)
	}
	if scan.Tail == nil || scan.Tail.Kind != TailTornRecord {
		t.Fatalf("want torn tail, got %+v", scan.Tail)
	}
	if scan.CommittedRoot != root {
		t.Fatalf("committed root moved: %d != %d", scan.CommittedRoot, root)
	}

	// A read-only open must not touch a byte.
	ro, err := Open(backing, ReadOnly)
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	ro.Close()
	if got := fileSize(t, tailPath); got != 6 {
		t.Fatalf("read-only open modified the tail segment: size %d", got)
	}

	// A writable open quarantines the tail, retaining every byte.
	rw, err := Open(backing, ReadWrite)
	if err != nil {
		t.Fatalf("read-write open: %v", err)
	}
	defer rw.Close()
	disp := rw.Recovery()
	if !disp.Quarantined || disp.Tail == nil {
		t.Fatalf("want quarantined tail, got %+v", disp)
	}
	if rw.CommittedRoot() != root {
		t.Fatalf("recovered root = %d, want %d", rw.CommittedRoot(), root)
	}
	qpath := filepath.Join(Dir(backing), segment.QuarantineDir, tailName)
	if got := fileSize(t, qpath); got != 6 {
		t.Fatalf("quarantined bytes = %d, want 6", got)
	}
	// Continue in a fresh segment at the committed root.
	if got := fileSize(t, tailPath); got != 0 {
		t.Fatalf("fresh continuation segment size = %d, want 0", got)
	}
	if _, err := rw.Publish([]*record.Record{inodeCreate(7)}); err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}
}

func TestWatermarkAheadRefused(t *testing.T) {
	backing := t.TempDir()
	l, _ := Create(backing, testGenesis())
	l.Publish([]*record.Record{inodeCreate(5)})
	l.Close()

	// Forge a watermark that certifies more history than survives.
	writeWatermarkAhead(t, backing)
	_, err := Open(backing, ReadOnly)
	if !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("want corrupt refusal, got %v", err)
	}
}

func TestTornInNonFinalSegmentIsCorrupt(t *testing.T) {
	backing := t.TempDir()
	l, _ := Create(backing, testGenesis())
	// Force a rollover by publishing enough to exceed a tiny segment size.
	l.Close()

	dir := Dir(backing)
	// Create an explicitly torn non-final segment (id 0) and a later segment.
	// Replace segment 0 with a torn frame and add a segment 1.
	seg0 := filepath.Join(dir, segment.SegmentName(0))
	os.Remove(seg0)
	if err := os.WriteFile(seg0, []byte{0x4F, 0x52, 0x46, 0x53}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, segment.SegmentName(1)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanLog(dir); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("want corrupt, got %v", err)
	}
}

func writeWatermarkAhead(t *testing.T, backing string) {
	t.Helper()
	dir := Dir(backing)
	cur, ok, err := wm.Read(dir)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if !ok {
		t.Fatal("expected a watermark")
	}
	cur.CommittedRoot += 4096
	if err := wm.Write(dir, cur); err != nil {
		t.Fatalf("write watermark: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
