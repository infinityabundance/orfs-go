package powerloss_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/powerloss"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// validImage builds a real committed log segment image and returns its bytes and
// the fully-flushed committed root.
func validImage(t *testing.T) ([]byte, uint64) {
	t.Helper()
	dir := t.TempDir()
	v, err := volume.Init(dir, volume.InitOptions{SegmentSize: 64 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	// Commit enough transactions that a partial flush can cut between them.
	for i := 0; i < 6; i++ {
		payload := record.InodeCreate{InodeID: uint64(10 + i), Generation: 1, Kind: record.KindFile, Mode: 0o644, Nlink: 1}.Encode()
		rec := &record.Record{Header: record.Header{RecordType: record.TypeInodeCreate, PayloadLength: uint64(len(payload))}, Payload: payload}
		if _, err := v.Log.Publish([]*record.Record{rec}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	root := v.Log.CommittedRoot()
	img, err := os.ReadFile(filepath.Join(dir, "or.log", segment.SegmentName(0)))
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	v.Close()
	return img, root
}

// scanImage writes an image to a fresh log directory and scans it.
func scanImage(t *testing.T, img []byte) (*log.ScanResult, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "or.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "or.log", segment.SegmentName(0)), img, 0o644); err != nil {
		t.Fatal(err)
	}
	return log.ScanLog(filepath.Join(dir, "or.log"))
}

// TestBarrierTearTailRecoversToBoundary models an honest barrier device that
// loses its final (unflushed) sector: recovery must reach a complete committed
// boundary or refuse, never expose uncommitted data.
func TestBarrierTearTailRecoversToBoundary(t *testing.T) {
	img, fullRoot := validImage(t)
	d := powerloss.New(powerloss.Barrier, powerloss.TearTail, 1)
	d.Write(img) // all bytes written, but not yet synced
	// Sync everything (a completed barrier), then fail with a tail tear.
	d.Sync()
	survivor := d.PowerFail()

	scan, err := scanImage(t, survivor)
	if err != nil {
		// A refusal is an acceptable, honest outcome.
		return
	}
	if scan.CommittedRoot == 0 || scan.CommittedRoot > fullRoot {
		t.Fatalf("recovered root %d out of range (full %d)", scan.CommittedRoot, fullRoot)
	}
	// The recovered root must be a committed boundary.
	isBoundary := false
	for _, b := range scan.Boundaries {
		if b == scan.CommittedRoot {
			isBoundary = true
		}
	}
	if !isBoundary {
		t.Fatalf("recovered root %d is not a committed boundary", scan.CommittedRoot)
	}
}

// TestPartialFlushNeverExposesUncommittedData models a contract-violating device
// that promotes only some sectors on sync. Recovery must still reach a valid
// committed boundary or refuse.
func TestPartialFlushNeverExposesUncommittedData(t *testing.T) {
	img, fullRoot := validImage(t)
	for seed := uint64(1); seed <= 40; seed++ {
		d := powerloss.New(powerloss.Partial, powerloss.NoDamage, seed)
		// Feed the image in 100-byte writes with a sync after each, so a partial
		// flush can leave holes.
		for off := 0; off < len(img); off += 100 {
			end := off + 100
			if end > len(img) {
				end = len(img)
			}
			d.Write(img[off:end])
			d.Sync()
		}
		survivor := d.PowerFail()
		scan, err := scanImage(t, survivor)
		if err != nil {
			continue // an honest refusal
		}
		if scan.CommittedRoot > fullRoot {
			t.Fatalf("seed %d: recovered root %d beyond the fully-committed %d", seed, scan.CommittedRoot, fullRoot)
		}
		// Any surviving record after the recovered root must not be exposed as
		// authority: the scan reports a tail whose abandoned bytes begin at or
		// after the committed root, rather than silently accepting them.
		if scan.Tail != nil && scan.Tail.BoundaryOffset < scan.CommittedRoot {
			t.Fatalf("seed %d: tail begins at %d, before the committed root %d", seed, scan.Tail.BoundaryOffset, scan.CommittedRoot)
		}
	}
}
