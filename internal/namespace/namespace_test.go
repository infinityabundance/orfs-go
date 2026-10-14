package namespace_test

import (
	"testing"
	"time"

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

func TestMknodSpecialFiles(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	ns := namespace.New(v)
	root := record.RootInode
	cred := namespace.Credential{}

	// Make the root world-writable so a non-root caller may create in it, as the
	// pjdfstest suite does with `chmod . 0777`.
	if err := ns.Setattr(cred, root, namespace.SetattrRequest{Mode: ptr(uint32(0o777))}); err != nil {
		t.Fatalf("chmod root: %v", err)
	}

	// A FIFO and a socket are creatable by any user with write on the parent.
	fifo, err := ns.Mknod(cred, root, "f", record.KindFifo, 0o644, 0, 0, 0)
	if err != nil {
		t.Fatalf("mknod fifo: %v", err)
	}
	if fifo.Kind != record.KindFifo || fifo.Mode != 0o644 || fifo.Nlink != 1 {
		t.Fatalf("fifo inode wrong: %+v", fifo)
	}
	other := namespace.Credential{UID: 4242, GID: 4242}
	sock, err := ns.Mknod(other, root, "s", record.KindSocket, 0o755, 0, 4242, 4242)
	if err != nil {
		t.Fatalf("mknod socket as non-root: %v", err)
	}
	if sock.Kind != record.KindSocket || sock.UID != 4242 {
		t.Fatalf("socket inode wrong: %+v", sock)
	}

	// A device node records its device number and is root-only.
	dev, err := ns.Mknod(cred, root, "b", record.KindBlock, 0o600, 0x0102, 0, 0)
	if err != nil {
		t.Fatalf("mknod block: %v", err)
	}
	if dev.Kind != record.KindBlock || dev.Rdev != 0x0102 {
		t.Fatalf("block inode wrong: %+v", dev)
	}
	if _, err := ns.Mknod(other, root, "c", record.KindChar, 0o600, 0x0102, 4242, 4242); err == nil {
		t.Fatal("non-root device mknod must be EPERM")
	}

	// Duplicate names are EEXIST.
	if _, err := ns.Mknod(cred, root, "f", record.KindFifo, 0o644, 0, 0, 0); err == nil {
		t.Fatal("duplicate mknod must be EEXIST")
	}

	v.Close()

	// Kind, mode, and device number survive replay.
	_, ns2 := open(t, backing)
	got, err := ns2.Getattr(dev.ID)
	if err != nil {
		t.Fatalf("getattr after replay: %v", err)
	}
	if got.Kind != record.KindBlock || got.Rdev != 0x0102 || got.Mode != 0o600 {
		t.Fatalf("device not preserved across replay: %+v", got)
	}
	f2, err := ns2.Lookup(namespace.Credential{}, root, "f")
	if err != nil || f2.Kind != record.KindFifo || f2.Mode != 0o644 {
		t.Fatalf("fifo not preserved across replay: %+v, %v", f2, err)
	}
}

func TestMknodPermissionDenied(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)

	d, err := ns.Mkdir(namespace.Credential{}, record.RootInode, "ro", 0o555, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	other := namespace.Credential{UID: 4242, GID: 4242}
	if _, err := ns.Mknod(other, d.ID, "f", record.KindFifo, 0o644, 0, 4242, 4242); err == nil {
		t.Fatal("mknod without write permission on the parent must be EACCES")
	}
}

func TestRenameDirectoryUpdatesParentNlink(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	src, _ := ns.Mkdir(cred, record.RootInode, "src", 0o755, 0, 0)
	dst, _ := ns.Mkdir(cred, record.RootInode, "dst", 0o755, 0, 0)
	if _, err := ns.Mkdir(cred, src.ID, "sub", 0o755, 0, 0); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	srcBefore, _ := ns.Getattr(src.ID)
	dstBefore, _ := ns.Getattr(dst.ID)
	if srcBefore.Nlink != 3 || dstBefore.Nlink != 2 {
		t.Fatalf("initial nlinks src=%d dst=%d, want 3,2", srcBefore.Nlink, dstBefore.Nlink)
	}
	if err := ns.Rename(cred, src.ID, "sub", dst.ID, "sub", false); err != nil {
		t.Fatalf("rename dir across parents: %v", err)
	}
	srcAfter, _ := ns.Getattr(src.ID)
	dstAfter, _ := ns.Getattr(dst.ID)
	if srcAfter.Nlink != 2 || dstAfter.Nlink != 3 {
		t.Fatalf("nlinks after rename src=%d dst=%d, want 2,3", srcAfter.Nlink, dstAfter.Nlink)
	}
}

func TestRenameOverwriteDecrementsNlink(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	src, _ := ns.Create(cred, record.RootInode, "src", 0o644, 0, 0)
	dst, _ := ns.Create(cred, record.RootInode, "dst", 0o644, 0, 0)
	if err := ns.Link(cred, record.RootInode, "dstlnk", dst.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if d, _ := ns.Getattr(dst.ID); d.Nlink != 2 {
		t.Fatalf("dst nlink = %d, want 2", d.Nlink)
	}
	if err := ns.Rename(cred, record.RootInode, "src", record.RootInode, "dst", false); err != nil {
		t.Fatalf("rename over hardlinked dst: %v", err)
	}
	// The old destination inode survives under its other name with one fewer link.
	if d, _ := ns.Getattr(dst.ID); d.Nlink != 1 {
		t.Fatalf("overwritten dst nlink = %d, want 1", d.Nlink)
	}
	lnk, err := ns.Lookup(cred, record.RootInode, "dstlnk")
	if err != nil || lnk.ID != dst.ID {
		t.Fatalf("dstlnk should still name the old inode: %+v, %v", lnk, err)
	}
	// The overwritten name now resolves to the source inode.
	moved, _ := ns.Lookup(cred, record.RootInode, "dst")
	if moved.ID != src.ID {
		t.Fatalf("dst should name src inode %d, got %d", src.ID, moved.ID)
	}
}

func TestRenameStickyBit(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	root := record.RootInode
	rootCred := namespace.Credential{}

	sticky, err := ns.Mkdir(rootCred, root, "sticky", 0o1777, 0, 0)
	if err != nil {
		t.Fatalf("mkdir sticky: %v", err)
	}
	dst, err := ns.Mkdir(rootCred, root, "dst", 0o777, 0, 0)
	if err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}

	// An entry's owner may rename it out of a sticky directory it does not own.
	if _, err := ns.Create(rootCred, sticky.ID, "a", 0o644, 4242, 4242); err != nil {
		t.Fatalf("create a: %v", err)
	}
	owner := namespace.Credential{UID: 4242, GID: 4242}
	if err := ns.Rename(owner, sticky.ID, "a", dst.ID, "a", false); err != nil {
		t.Fatalf("owner rename out of a sticky directory: %v", err)
	}

	// A third party who owns neither the directory nor the entry may not.
	if _, err := ns.Create(rootCred, sticky.ID, "b", 0o644, 0, 0); err != nil {
		t.Fatalf("create b: %v", err)
	}
	third := namespace.Credential{UID: 4243, GID: 4243}
	if err := ns.Rename(third, sticky.ID, "b", dst.ID, "b", false); err == nil {
		t.Fatal("third-party rename out of a sticky directory must be denied")
	}

	// A third party may also not rename an entry over one in a sticky directory.
	if err := ns.Setattr(rootCred, dst.ID, namespace.SetattrRequest{Mode: ptr(uint32(0o1777))}); err != nil {
		t.Fatalf("chmod dst: %v", err)
	}
	if _, err := ns.Create(rootCred, dst.ID, "c", 0o644, 0, 0); err != nil {
		t.Fatalf("create c: %v", err)
	}
	if _, err := ns.Create(rootCred, sticky.ID, "d", 0o644, 4243, 4243); err != nil {
		t.Fatalf("create d: %v", err)
	}
	if err := ns.Rename(third, sticky.ID, "d", dst.ID, "c", false); err == nil {
		t.Fatal("rename over an entry in a sticky directory must be denied for a non-owner")
	}
}

func TestChmodClearsSetgidForNonGroupCaller(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)

	f, err := ns.Create(namespace.Credential{}, record.RootInode, "f", 0o755, 4242, 4242)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Owner, but in a different group: S_ISGID must be cleared.
	if err := ns.Setattr(namespace.Credential{UID: 4242, GID: 9999}, f.ID, namespace.SetattrRequest{Mode: ptr(uint32(0o2755))}); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got, _ := ns.Getattr(f.ID)
	if got.Mode != 0o755 {
		t.Fatalf("setgid not cleared for a non-group caller: mode %o, want 0755", got.Mode)
	}
	// Owner in the file's group: S_ISGID is preserved.
	if err := ns.Setattr(namespace.Credential{UID: 4242, GID: 4242}, f.ID, namespace.SetattrRequest{Mode: ptr(uint32(0o2755))}); err != nil {
		t.Fatalf("chmod 2: %v", err)
	}
	got, _ = ns.Getattr(f.ID)
	if got.Mode != 0o2755 {
		t.Fatalf("setgid dropped for a group member: mode %o, want 02755", got.Mode)
	}
}

func TestMetadataOperationsAdvanceCtime(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}
	root := record.RootInode

	ctimeOf := func(id uint64) record.Timespec {
		in, err := ns.Getattr(id)
		if err != nil {
			t.Fatalf("getattr: %v", err)
		}
		return in.Ctime
	}

	f, err := ns.Create(cred, root, "f", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c0 := ctimeOf(f.ID)
	m0 := func() record.Timespec { in, _ := ns.Getattr(f.ID); return in.Mtime }()

	// chmod advances ctime and leaves mtime alone.
	time.Sleep(2 * time.Millisecond)
	if err := ns.Setattr(cred, f.ID, namespace.SetattrRequest{Mode: ptr(uint32(0o600))}); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if ctimeOf(f.ID) == c0 {
		t.Fatal("chmod did not advance ctime")
	}
	if in, _ := ns.Getattr(f.ID); in.Mtime != m0 {
		t.Fatal("chmod must not change mtime")
	}

	// link and unlink advance ctime as well.
	time.Sleep(2 * time.Millisecond)
	c2 := ctimeOf(f.ID)
	if err := ns.Link(cred, root, "g", f.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if ctimeOf(f.ID) == c2 {
		t.Fatal("link did not advance ctime")
	}
	time.Sleep(2 * time.Millisecond)
	c4 := ctimeOf(f.ID)
	if err := ns.Unlink(cred, root, "g"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if ctimeOf(f.ID) == c4 {
		t.Fatal("unlink did not advance ctime")
	}
}

func TestLinkUpdatesParentTimestamps(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	dir, err := ns.Mkdir(cred, record.RootInode, "d", 0o755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := ns.Create(cred, dir.ID, "f", 0o644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, _ := ns.Getattr(dir.ID)
	mtime0, ctime0 := before.Mtime, before.Ctime

	time.Sleep(2 * time.Millisecond)
	if err := ns.Link(cred, dir.ID, "g", f.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	after, _ := ns.Getattr(dir.ID)
	if after.Mtime == mtime0 || after.Ctime == ctime0 {
		t.Fatalf("link did not advance the parent's mtime/ctime: %+v -> %+v", before, after)
	}
}

func TestRenameDirectoryAcrossParentsNeedsWrite(t *testing.T) {
	backing := t.TempDir()
	v, _ := volume.Init(backing, volume.InitOptions{})
	defer v.Close()
	ns := namespace.New(v)
	root := record.RootInode
	rootCred := namespace.Credential{}

	// Both parents are writable by the caller; the moved directory is not.
	src, err := ns.Mkdir(rootCred, root, "src", 0o777, 4242, 4242)
	if err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	dst, err := ns.Mkdir(rootCred, root, "dst", 0o777, 4242, 4242)
	if err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if _, err := ns.Mkdir(rootCred, src.ID, "sub", 0o755, 0, 0); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	caller := namespace.Credential{UID: 4242, GID: 4242}
	if err := ns.Rename(caller, src.ID, "sub", dst.ID, "sub", false); err == nil {
		t.Fatal("moving a directory the caller cannot write must be denied")
	}
	// A same-parent rename does not rewrite ".." and is allowed.
	if err := ns.Rename(caller, src.ID, "sub", src.ID, "sub2", false); err != nil {
		t.Fatalf("same-parent directory rename: %v", err)
	}
}
