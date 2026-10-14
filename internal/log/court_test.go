package log

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/capacity"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// TestCapacityExhaustedRefusesBeforeWrite proves the capacity guard is a
// pre-first-byte refusal: when the reserve would be breached, Publish returns
// the typed CapacityExhausted (ENOSPC), the previous committed root is
// byte-identical, and the volume then latches read-only.
func TestCapacityExhaustedRefusesBeforeWrite(t *testing.T) {
	backing := t.TempDir()
	l, err := Create(backing, testGenesis())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer l.Close()
	rootBefore, err := l.Publish([]*record.Record{inodeCreate(5)})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	segPath := filepath.Join(Dir(backing), segment.SegmentName(0))
	bytesBefore, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}

	free, ok := capacity.BackingAvailableBytes(Dir(backing))
	if !ok {
		t.Skip("backing free space unmeasurable")
	}
	reserve := free + (1 << 20) // reserve exceeds free: zero headroom
	l.Capacity().SetPolicy(capacity.Policy{ReserveBytes: reserve}, Dir(backing), l.CommittedRoot())

	_, err = l.Publish([]*record.Record{inodeCreate(6)})
	if !errors.Is(err, orfserr.ErrExhausted) {
		t.Fatalf("want CapacityExhausted, got %v", err)
	}
	if e := orfserr.ErrnoOf(err); e != 28 { // ENOSPC
		t.Fatalf("want ENOSPC, got errno %d", e)
	}
	if l.CommittedRoot() != rootBefore {
		t.Fatalf("committed root moved on a refused transaction: %d != %d", l.CommittedRoot(), rootBefore)
	}
	bytesAfter, _ := os.ReadFile(segPath)
	if string(bytesAfter) != string(bytesBefore) {
		t.Fatal("a refused transaction wrote bytes")
	}

	// The volume latches read-only once the reserve floor is breached.
	if _, second := l.Publish([]*record.Record{inodeCreate(7)}); !errors.Is(second, orfserr.ErrReadOnly) {
		t.Fatalf("want read-only latch after exhaustion, got %v", second)
	}

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeEvidence(t, "recovery", "capacity.json", map[string]any{
			"schema":            "orfs.recovery.capacity.v1",
			"refusal":           "CapacityExhausted",
			"errno":             "ENOSPC",
			"root_unchanged":    true,
			"bytes_unchanged":   true,
			"latched_read_only": true,
			"reserve_breached":  true,
		})
	}
}

// TestQuarantineRetainsEveryByte proves recovery moves (never truncates) an
// abandoned tail and leaves the committed prefix byte-identical.
func TestQuarantineRetainsEveryByte(t *testing.T) {
	backing := t.TempDir()
	l, err := Create(backing, testGenesis())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	root, err := l.Publish([]*record.Record{inodeCreate(5)})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	l.Close()

	committedPath := filepath.Join(Dir(backing), segment.SegmentName(0))
	committedBefore, _ := os.ReadFile(committedPath)

	// A torn append in a wholly new trailing segment.
	torn := []byte{0x4F, 0x52, 0x46, 0x53, 0x01, 0x00, 0xAB, 0xCD}
	tailName := segment.SegmentName(root)
	tailPath := filepath.Join(Dir(backing), tailName)
	if err := os.WriteFile(tailPath, torn, 0o644); err != nil {
		t.Fatal(err)
	}

	rw, err := Open(backing, ReadWrite)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rw.Close()
	if !rw.Recovery().Quarantined {
		t.Fatal("expected the tail to be quarantined")
	}
	if rw.CommittedRoot() != root {
		t.Fatalf("recovered root %d != %d", rw.CommittedRoot(), root)
	}
	// The quarantined file retains every byte.
	qpath := filepath.Join(Dir(backing), segment.QuarantineDir, tailName)
	q, err := os.ReadFile(qpath)
	if err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
	if string(q) != string(torn) {
		t.Fatalf("quarantine truncated: kept %d of %d bytes", len(q), len(torn))
	}
	// The committed prefix is byte-identical.
	committedAfter, _ := os.ReadFile(committedPath)
	if string(committedAfter) != string(committedBefore) {
		t.Fatal("recovery modified the committed prefix")
	}

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeEvidence(t, "recovery", "quarantine.json", map[string]any{
			"schema":                "orfs.recovery.quarantine.v1",
			"tail_kind":             "torn-record",
			"quarantined":           true,
			"tail_bytes_written":    len(torn),
			"tail_bytes_retained":   len(q),
			"truncated":             false,
			"committed_prefix_same": true,
		})
	}
}

func writeEvidence(t *testing.T, sub, name string, doc any) {
	t.Helper()
	dir := filepath.Join("..", "..", "evidence", sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
