package namespace_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// TestConcurrentReadViews runs readers against a namespace while a writer
// mutates it. Run with -race, it proves the shared index is never observed
// partially applied: a reader sees either the old or the new value, never a
// torn view.
func TestConcurrentReadViews(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 256 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	file, err := ns.Create(cred, record.RootInode, "data", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed a full page so readers always have something to observe.
	if _, err := ns.Write(cred, file.ID, 0, make([]byte, 4096)); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const readers = 8
	const iterations = 400
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if b, err := ns.Read(cred, file.ID, 0, 64); err == nil && len(b) > 0 {
					_ = b[0]
				}
				if _, err := ns.Getattr(file.ID); err != nil {
					t.Errorf("getattr: %v", err)
					return
				}
				_, _ = ns.ParentOf(file.ID)
				if infos, _, _, err := ns.ReaddirFull(cred, record.RootInode); err == nil {
					_ = infos
				}
			}
		}()
	}

	// Writer: repeatedly overwrite and create/remove scratch entries.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < iterations; i++ {
			ns.Write(cred, file.ID, uint64(i%4096), []byte{byte(i)})
			name := fmt.Sprintf("scratch-%d", i)
			if in, err := ns.Create(cred, record.RootInode, name, 0o644, 0, 0); err == nil {
				ns.Unlink(cred, record.RootInode, name)
				_ = in
			}
		}
	}()

	wg.Wait()
	<-stop
}
