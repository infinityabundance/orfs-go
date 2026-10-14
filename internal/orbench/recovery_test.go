package orbench_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/orbench"
)

// TestCheckpointRecoveryCourt measures index-reconstruction work and Go heap
// across exponentially increasing retained histories, comparing full replay,
// checkpoint + suffix replay, and directory-assisted discovery, and asserts the
// bounded reconstruction equals full replay at every size. With ORFS_EVIDENCE=1
// it writes evidence/recovery/checkpoints.json (schema
// orfs.recovery.checkpoints.v1).
func TestCheckpointRecoveryCourt(t *testing.T) {
	kmin, kmax := 10, 14
	if v := os.Getenv("ORFS_RECOVERY_KMIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			kmin = n
		}
	}
	if v := os.Getenv("ORFS_RECOVERY_KMAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			kmax = n
		}
	}
	evidence := os.Getenv("ORFS_EVIDENCE") == "1"
	if evidence {
		// The full published curve unless the range was pinned explicitly.
		if os.Getenv("ORFS_RECOVERY_KMIN") == "" {
			kmin = 10
		}
		if os.Getenv("ORFS_RECOVERY_KMAX") == "" {
			kmax = 20
		}
	}

	reps := 3
	if v := os.Getenv("ORFS_RECOVERY_REPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reps = n
		}
	}

	rec, err := orbench.RunRecoveryCourt(t.TempDir(), kmin, kmax, reps)
	if err != nil {
		t.Fatalf("recovery court: %v", err)
	}
	if len(rec.Rows) == 0 {
		t.Fatal("no recovery rows produced")
	}
	for _, row := range rec.Rows {
		if !row.Correct {
			t.Fatalf("%s k=%d (%d bytes): bounded reconstruction != full replay", row.Workload, row.K, row.CommittedRoot)
		}
		if !row.UsedCheckpoint {
			t.Fatalf("%s k=%d: no checkpoint was used", row.Workload, row.K)
		}
		if row.CheckpointBytesReplayed >= row.CommittedRoot {
			t.Fatalf("%s k=%d: suffix replay (%d) did not skip any committed bytes (%d)",
				row.Workload, row.K, row.CheckpointBytesReplayed, row.CommittedRoot)
		}
		t.Logf("%-9s k=%2d root=%8d full=%6dµs ckpt=%6dµs dir=%6dµs suffix=%7d/%8d open(full)=%6dµs open(ckpt)=%6dµs heap full=%d ckpt=%d",
			row.Workload, row.K, row.CommittedRoot, row.FullMicros, row.CheckpointMicros, row.DirectoryMicros,
			row.CheckpointBytesReplayed, row.CommittedRoot, row.OpenFullMicros, row.OpenCheckpointMicros,
			row.FullHeapInuse, row.CheckpointHeapInuse)
	}

	if evidence {
		writeRecoveryEvidence(t, rec)
	}
}

func writeRecoveryEvidence(t *testing.T, rec orbench.RecoveryReceipt) {
	t.Helper()
	dir := filepath.Join("..", "..", "evidence", "recovery")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoints.json"), append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
