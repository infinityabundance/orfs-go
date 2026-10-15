package retention_test

import (
	"encoding/json"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/retention"
)

// The Btrfs court's receipt schema and note must be unchanged by the ZFS
// generalisation: Merge (no schema/comparator) is the Btrfs court's exact path.
func TestMergeBtrfsDefaultsUnchanged(t *testing.T) {
	rec, err := retention.Merge([]retention.Measure{
		measure("Btrfs", "append", "append", 0, 1, 0, 4096, 5),
	}, nil, nil, json.RawMessage(`{"same_machine":true}`), 7)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if rec.Schema != retention.ReceiptSchema {
		t.Errorf("schema = %q, want %q", rec.Schema, retention.ReceiptSchema)
	}
	want := "A shared, deterministic driver (cmd/retentionbench) drove two systems on the same machine, same " +
		"device, same container caps: Btrfs with a read-only snapshot after every epoch, and OrFS-GO, which " +
		"retains every committed transition in its authoritative log with no extra snapshots. This receipt is " +
		"data, not a leaderboard: rows are keyed by {system, kind, state, epoch, writers, readers, block_size} " +
		"and are never ranked across incomparable coordinates. Read the comparability block before drawing any " +
		"conclusion."
	if rec.Note != want {
		t.Errorf("note changed:\n got: %q\nwant: %q", rec.Note, want)
	}
}

// MergeFor stamps the ZFS court's schema and comparator and is otherwise the
// same merge.
func TestMergeForZFS(t *testing.T) {
	rec, err := retention.MergeFor([]retention.Measure{
		measure("OpenZFS", "append", "append", 0, 1, 0, 4096, 5),
	}, nil, nil, json.RawMessage(`{"same_machine":true}`), retention.ZFSReceiptSchema, "OpenZFS", 7)
	if err != nil {
		t.Fatalf("MergeFor: %v", err)
	}
	if rec.Schema != retention.ZFSReceiptSchema {
		t.Errorf("schema = %q, want %q", rec.Schema, retention.ZFSReceiptSchema)
	}
	if !contains(rec.Note, "OpenZFS with a read-only snapshot after every epoch") {
		t.Errorf("note does not name OpenZFS: %q", rec.Note)
	}
	if len(rec.Rows) != 1 || rec.Rows[0].System != "OpenZFS" {
		t.Fatalf("rows = %+v", rec.Rows)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestParseZFSList(t *testing.T) {
	used, ref, lu, lr, err := retention.ParseZFSList("1175552\t1048576\t1048576\t1048576")
	if err != nil {
		t.Fatalf("ParseZFSList: %v", err)
	}
	if used != 1175552 || ref != 1048576 || lu != 1048576 || lr != 1048576 {
		t.Fatalf("used=%d ref=%d lu=%d lr=%d", used, ref, lu, lr)
	}
	// A trailing newline (zfs list always emits one) is tolerated.
	if _, _, _, _, err := retention.ParseZFSList("1\t2\t3\t4\n"); err != nil {
		t.Fatalf("ParseZFSList with newline: %v", err)
	}
	// A `-` figure (a value ZFS does not report for that dataset type) reads 0.
	used, ref, lu, lr, err = retention.ParseZFSList("8192\t4096\t-\t0")
	if err != nil {
		t.Fatalf("ParseZFSList with dash: %v", err)
	}
	if used != 8192 || ref != 4096 || lu != 0 || lr != 0 {
		t.Fatalf("dash: used=%d ref=%d lu=%d lr=%d", used, ref, lu, lr)
	}
	if _, _, _, _, err := retention.ParseZFSList(""); err == nil {
		t.Fatal("expected an error for empty output")
	}
	if _, _, _, _, err := retention.ParseZFSList("8192\t4096"); err == nil {
		t.Fatal("expected an error for too few fields")
	}
	if _, _, _, _, err := retention.ParseZFSList("x\ty\tz\tw"); err == nil {
		t.Fatal("expected an error for non-numeric fields")
	}
}
