package orbench_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/orbench"
)

// TestAmplificationGrowsMonotonically runs the growth court for both arms and
// asserts the laws: retained authoritative history grows monotonically, the
// manifested arm spends at least as much, and the amplification ratios rise.
// It writes evidence/performance/amplification.json when ORFS_EVIDENCE=1.
func TestAmplificationGrowsMonotonically(t *testing.T) {
	opts := orbench.Options{Epochs: 24, Payload: 16 * 1024, SegmentSz: 4 * 1024 * 1024}

	auth, err := orbench.Run(t.TempDir(), orbench.ArmAuthoritative, opts)
	if err != nil {
		t.Fatalf("authoritative arm: %v", err)
	}
	man, err := orbench.Run(t.TempDir(), orbench.ArmManifested, opts)
	if err != nil {
		t.Fatalf("manifested arm: %v", err)
	}

	checkMonotonic := func(res orbench.AmplificationResult) {
		if len(res.Samples) != opts.Epochs {
			t.Fatalf("%s: got %d samples, want %d", res.Arm, len(res.Samples), opts.Epochs)
		}
		for i, s := range res.Samples {
			if s.LogicalBytes == 0 {
				t.Fatalf("%s epoch %d: logical bytes is zero", res.Arm, i)
			}
			if i > 0 {
				prev := res.Samples[i-1]
				if s.AuthoritativePhysicalBytes <= prev.AuthoritativePhysicalBytes {
					t.Fatalf("%s epoch %d: authoritative physical did not grow (%d <= %d): non-withdrawal requires monotonic growth",
						res.Arm, i, s.AuthoritativePhysicalBytes, prev.AuthoritativePhysicalBytes)
				}
				if s.AmplificationAuth < prev.AmplificationAuth {
					t.Fatalf("%s epoch %d: amplification decreased (%.3f < %.3f)", res.Arm, i, s.AmplificationAuth, prev.AmplificationAuth)
				}
			}
		}
	}
	checkMonotonic(auth)
	checkMonotonic(man)

	lastAuth := auth.Samples[len(auth.Samples)-1]
	lastMan := man.Samples[len(man.Samples)-1]
	if lastMan.ManifestedBytes == 0 {
		t.Fatal("manifested arm spent no manifested bytes")
	}
	if lastMan.TotalPhysicalBytes <= lastAuth.AuthoritativePhysicalBytes {
		t.Fatalf("manifested total %d must exceed the authoritative-only log %d", lastMan.TotalPhysicalBytes, lastAuth.AuthoritativePhysicalBytes)
	}
	t.Logf("authoritative: logical %d, physical %d, amp %.2f -> %.2f",
		lastAuth.LogicalBytes, lastAuth.AuthoritativePhysicalBytes,
		auth.Samples[0].AmplificationAuth, lastAuth.AmplificationAuth)
	t.Logf("manifested:    logical %d, manifested %d, total %d, amp %.2f -> %.2f",
		lastMan.LogicalBytes, lastMan.ManifestedBytes, lastMan.TotalPhysicalBytes,
		man.Samples[0].AmplificationManifested, lastMan.AmplificationManifested)

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeEvidence(t, auth, man)
	}
}

func writeEvidence(t *testing.T, arms ...orbench.AmplificationResult) {
	t.Helper()
	out := map[string]any{
		"schema": "orfs.performance.amplification.v1",
		"method": "bounded growth court: each epoch overwrites one file so retained history grows monotonically",
		"arms":   arms,
	}
	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	dir := filepath.Join("..", "..", "evidence", "performance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	path := filepath.Join(dir, "amplification.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s", path)
}
