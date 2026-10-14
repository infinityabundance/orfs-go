package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

func TestManifestRoundTripAndFallback(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 256 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	content := []byte("the quick brown fox jumps over the lazy dog, repeatedly. ")
	full := append([]byte(nil), content...)
	for i := 0; i < 6; i++ {
		full = append(full, content...)
	}
	file, err := ns.Create(cred, record.RootInode, "f", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ns.Write(cred, file.ID, 0, full); err != nil {
		t.Fatalf("write: %v", err)
	}

	in, _ := ns.Getattr(file.ID)
	root := v.Log.CommittedRoot()
	m, err := manifest.Publish(v, file.ID, in.Version, root, full)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if m.PhysicalBytes == 0 {
		t.Fatal("manifested body is empty")
	}

	// A certified manifested read returns the exact bytes.
	got, ok := manifest.TryRead(v, file.ID, in.Version, root, 7, 40)
	if !ok {
		t.Fatal("certified read should be served from the manifested body")
	}
	if string(got) != string(full[7:47]) {
		t.Fatalf("manifested read = %q, want %q", got, full[7:47])
	}

	// A different (inode, version, root) is not certified: fall back.
	if _, ok := manifest.TryRead(v, file.ID, in.Version+1, root, 0, 16); ok {
		t.Fatal("a stale version must not be served")
	}
	if _, ok := manifest.TryRead(v, file.ID, in.Version, root+1, 0, 16); ok {
		t.Fatal("a different root must not be served")
	}

	// Tampering the body must be detected by the self-digest.
	body := filepath.Join(backing, manifest.BloomDirName, "ekbloom")
	entries, _ := os.ReadDir(body)
	if len(entries) != 1 {
		t.Fatalf("expected one body, found %d", len(entries))
	}
	bp := filepath.Join(body, entries[0].Name())
	raw, _ := os.ReadFile(bp)
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(bp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.TryRead(v, file.ID, in.Version, root, 0, 16); ok {
		t.Fatal("a tampered body must not be served")
	}
}
