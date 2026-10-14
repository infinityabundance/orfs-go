package namespace_test

import (
	"testing"

	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

func open(t *testing.T, backing string) (namespace.Credential, *namespace.Namespace) {
	t.Helper()
	v, err := volume.Open(backing, log.ReadWrite)
	if err != nil {
		t.Fatalf("open volume: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return namespace.Credential{UID: 0, GID: 0}, namespace.New(v)
}

func TestCreateWriteReadRecover(t *testing.T) {
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 64 * 1024})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	ns := namespace.New(v)
	root := record.RootInode

	file, err := ns.Create(namespace.Credential{}, root, "hello.txt", 0o644, 1000, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if file.Kind != record.KindFile || file.Nlink != 1 {
		t.Fatalf("file inode wrong: %+v", file)
	}
	n, err := ns.Write(namespace.Credential{UID: 1000, GID: 1000}, file.ID, 0, []byte("hello, orfs\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 12 {
		t.Fatalf("size after write = %d, want 12", n)
	}
	v.Close()

	// Reopen and re-read: the namespace must be byte-exact after replay.
	cred, ns2 := open(t, backing)
	got, err := ns2.Read(cred, file.ID, 0, 12)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello, orfs\n" {
		t.Fatalf("read after recovery = %q", got)
	}
	// Overwrite a range: old bytes are retained, only the view changes.
	if _, err := ns2.Write(cred, file.ID, 7, []byte("OrFS")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got2, _ := ns2.Read(cred, file.ID, 0, 12)
	if string(got2) != "hello, OrFS\n" {
		t.Fatalf("after overwrite = %q", got2)
	}
	// Reads beyond EOF are clamped.
	if b, _ := ns2.Read(cred, file.ID, 100, 10); len(b) != 0 {
		t.Fatalf("read past EOF = %q", b)
	}
}

func TestMkdirReaddirRmdir(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	dir, err := ns.Mkdir(cred, record.RootInode, "docs", 0o755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if dir.Nlink != 2 {
		t.Fatalf("new dir nlink = %d, want 2", dir.Nlink)
	}
	parent, _ := ns.Getattr(record.RootInode)
	if parent.Nlink != 3 {
		t.Fatalf("parent nlink = %d, want 3", parent.Nlink)
	}
	if _, err := ns.Create(cred, dir.ID, "a.txt", 0o644, 0, 0); err != nil {
		t.Fatalf("create in dir: %v", err)
	}
	entries, _ := ns.Readdir(cred, dir.ID)
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("readdir = %+v", entries)
	}
	if err := ns.Rmdir(cred, record.RootInode, "docs"); err == nil {
		t.Fatal("rmdir of non-empty directory should fail")
	}
	if err := ns.Unlink(cred, dir.ID, "a.txt"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if err := ns.Rmdir(cred, record.RootInode, "docs"); err != nil {
		t.Fatalf("rmdir empty: %v", err)
	}
}

func TestRenameAndLink(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	f, _ := ns.Create(cred, record.RootInode, "a", 0o644, 0, 0)
	if err := ns.Rename(cred, record.RootInode, "a", record.RootInode, "b", false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := ns.Lookup(cred, record.RootInode, "a"); err == nil {
		t.Fatal("old name should be gone")
	}
	moved, err := ns.Lookup(cred, record.RootInode, "b")
	if err != nil {
		t.Fatalf("lookup b: %v", err)
	}
	if moved.ID != f.ID {
		t.Fatalf("rename changed identity: %d != %d", moved.ID, f.ID)
	}
	if err := ns.Link(cred, record.RootInode, "c", f.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	after, _ := ns.Getattr(f.ID)
	if after.Nlink != 2 {
		t.Fatalf("nlink after link = %d, want 2", after.Nlink)
	}
	// Directory hard-links are EPERM.
	d, err := ns.Mkdir(cred, record.RootInode, "d", 0o755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := ns.Link(cred, record.RootInode, "dlink", d.ID); err == nil {
		t.Fatal("hard-linking a directory must fail")
	}
}

func TestPermissionChecks(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)

	// A root-owned 0600 file, accessed by an unrelated user.
	f, _ := ns.Create(namespace.Credential{}, record.RootInode, "secret", 0o600, 0, 0)
	other := namespace.Credential{UID: 4242, GID: 4242}
	if _, err := ns.Read(other, f.ID, 0, 1); err == nil {
		t.Fatal("read of 0600 by other should be denied")
	}
	if _, err := ns.Write(other, f.ID, 0, []byte("x")); err == nil {
		t.Fatal("write of 0600 by other should be denied")
	}
	// Group access is granted via a supplementary group.
	ns.Setxattr(namespace.Credential{}, f.ID, "user.k", []byte("v"))
	groupMember := namespace.Credential{UID: 5000, GID: 5000, Groups: []uint32{0}}
	ns.Setattr(namespace.Credential{}, f.ID, namespace.SetattrRequest{Mode: ptr(uint32(0o640))})
	if _, err := ns.Getxattr(groupMember, f.ID, "user.k"); err != nil {
		t.Fatalf("group member with supplementary group should read: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
