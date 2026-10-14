package namespace_test

import (
	"sync"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// TestPublishedViewIsImmutable proves that a commit publishes a *new* view and
// leaves the previously published one frozen: after an exchange of "a" and "b",
// the old view still maps a->A / b->B while the new view maps a->B / b->A.
func TestPublishedViewIsImmutable(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 256 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	a, err := ns.Create(cred, record.RootInode, "a", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := ns.Create(cred, record.RootInode, "b", 0o600, 0, 0)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	v0 := ns.Volume().View()
	if v0 == nil {
		t.Fatal("no view published at open")
	}
	if err := ns.Rename(cred, record.RootInode, "a", record.RootInode, "b", true); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	v1 := ns.Volume().View()
	if v0 == v1 {
		t.Fatal("a commit did not publish a new read view")
	}

	if in, ok := v0.Lookup(record.RootInode, "a"); !ok || in.ID != a.ID {
		t.Fatalf("old view a -> %v,%v want %d", in, ok, a.ID)
	}
	if in, ok := v0.Lookup(record.RootInode, "b"); !ok || in.ID != b.ID {
		t.Fatalf("old view b -> %v,%v want %d", in, ok, b.ID)
	}
	if in, ok := v1.Lookup(record.RootInode, "a"); !ok || in.ID != b.ID {
		t.Fatalf("new view a -> %v,%v want %d", in, ok, b.ID)
	}
	if in, ok := v1.Lookup(record.RootInode, "b"); !ok || in.ID != a.ID {
		t.Fatalf("new view b -> %v,%v want %d", in, ok, a.ID)
	}
}

// TestConcurrentReaderSeesWholeTransaction proves a reader never observes a
// partially applied transaction. The writer repeatedly exchanges two directory
// entries; that is one authoritative transaction that moves two dentry keys, and
// the two inodes have distinct modes. A reader that saw a half-applied
// transaction would see both entries carrying the same mode within one
// ReaddirFull listing (which is answered from a single loaded view). Run under
// -race, it also proves the read path is race-free against the writer.
func TestConcurrentReaderSeesWholeTransaction(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 512 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	a, err := ns.Create(cred, record.RootInode, "a", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := ns.Create(cred, record.RootInode, "b", 0o600, 0, 0)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	valid := map[uint64]bool{a.ID: true, b.ID: true}

	const readers = 6
	const flips = 400
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				infos, _, _, err := ns.ReaddirFull(cred, record.RootInode)
				if err != nil {
					t.Errorf("readdir: %v", err)
					return
				}
				var ma, mb uint32
				var oka, okb bool
				for _, e := range infos {
					switch e.Name {
					case "a":
						ma, oka = e.Mode, true
					case "b":
						mb, okb = e.Mode, true
						if !valid[e.Ino] {
							t.Errorf("entry b -> unexpected inode %d", e.Ino)
							return
						}
					}
				}
				if oka && okb && ma == mb {
					t.Errorf("torn view: a and b both report mode %o", ma)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < flips; i++ {
			if err := ns.Rename(cred, record.RootInode, "a", record.RootInode, "b", true); err != nil {
				t.Errorf("exchange: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// TestConcurrentReadsDoNotBlockOnWriter is a -race stress that runs the full
// read surface against a writer mutating a directory tree: readers must stay
// correct (no races, no missing inode) with no lock taken.
func TestConcurrentReadsDoNotBlockOnWriter(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 512 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	dir, err := ns.Mkdir(cred, record.RootInode, "d", 0o755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file, err := ns.Create(cred, dir.ID, "f", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ns.Write(cred, file.ID, 0, make([]byte, 4096)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := ns.Setxattr(cred, file.ID, "user.k", []byte("v")); err != nil {
		t.Fatalf("setxattr: %v", err)
	}

	const iterations = 300
	var wg sync.WaitGroup

	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if b, err := ns.Read(cred, file.ID, 0, 128); err != nil || len(b) != 128 {
					t.Errorf("read: %v (%d bytes)", err, len(b))
					return
				}
				if in, err := ns.Getattr(file.ID); err != nil || in.ID != file.ID {
					t.Errorf("getattr: %v", err)
					return
				}
				if _, err := ns.Lookup(cred, dir.ID, "f"); err != nil {
					t.Errorf("lookup: %v", err)
					return
				}
				if names, err := ns.Listxattr(cred, file.ID); err != nil || len(names) != 1 {
					t.Errorf("listxattr: %v %v", names, err)
					return
				}
				if v, err := ns.Getxattr(cred, file.ID, "user.k"); err != nil || string(v) != "v" {
					t.Errorf("getxattr: %q %v", v, err)
					return
				}
				if _, _, _, err := ns.ReaddirFull(cred, dir.ID); err != nil {
					t.Errorf("readdir: %v", err)
					return
				}
				if _, ok := ns.ParentOf(file.ID); !ok {
					t.Error("parentof: not found")
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := ns.Write(cred, file.ID, uint64(i%4096), []byte{byte(i)}); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			name := "scratch"
			if _, err := ns.Create(cred, dir.ID, name, 0o644, 0, 0); err == nil {
				if err := ns.Unlink(cred, dir.ID, name); err != nil {
					t.Errorf("unlink: %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
}
