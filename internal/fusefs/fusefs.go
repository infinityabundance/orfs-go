// Package fusefs projects the OrFS namespace onto FUSE.
//
// The mount is a purely logical projection: or.log/, volume.meta, quarantine/
// and .or_bloom/ are never reachable through it. All permission checks run in
// the daemon (the reference's single POSIX class rule), and a mutation is served
// by committing one authoritative transaction, so a read never observes a
// partial view.
package fusefs

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// MountOptions configures a mount.
type MountOptions struct {
	AllowOther bool
	ReadOnly   bool
	DirectIO   bool
	// SingleThreaded serialises request processing in go-fuse (one request at a
	// time). It is the f0-single serving mode: an honest, measurable point on
	// the F0 reader-concurrency curve, not a separate transport.
	SingleThreaded bool
	// AttrTTLSeconds/EntryTTLSeconds set the kernel's attribute and entry cache
	// lifetimes. A nil pointer leaves go-fuse's default; a pointer to 0 forces
	// every path walk and getattr back through the daemon, which is what the
	// POSIX conformance court relies on so a cached dentry cannot satisfy a walk
	// and skip the daemon's search-permission re-check.
	AttrTTLSeconds  *float64
	EntryTTLSeconds *float64
	FsName          string
}

// Server serves a volume over FUSE.
type Server struct {
	vol   *volume.Volume
	ns    *namespace.Namespace
	cache *manifest.Cache
	opt   MountOptions
}

// New builds a server over an opened volume, wiring the concealed read seam
// (Plane C) with an authoritative oracle so a manifested body is trusted only
// when it agrees with authority.
func New(v *volume.Volume, opt MountOptions) *Server {
	ns := namespace.New(v)
	cache := manifest.NewCache(v)
	cache.SetAuthority(func(inode, off, size uint64) ([]byte, bool) {
		b, err := ns.ReadAuthoritative(namespace.Credential{}, inode, off, size)
		if err != nil {
			return nil, false
		}
		return b, true
	})
	ns.SetManifestSource(cache)
	return &Server{vol: v, ns: ns, cache: cache, opt: opt}
}

// Mount mounts the volume at mnt and serves it until the returned server is
// unmounted. It returns the fuse.Server so the caller can control lifetime.
func (s *Server) Mount(mnt string) (*fuse.Server, error) {
	root := &node{id: record.RootInode, s: s}
	opts := &fs.Options{}
	// A file legitimately created with mode 0000 must report 0000, not the
	// package's 0644 (0755 for a directory) placeholder (pjdfstest open/26.t).
	opts.NullPermissions = true
	opts.MountOptions = fuse.MountOptions{
		Name:    "orfs",
		FsName:  s.opt.FsName,
		Options: []string{"ro"},
	}
	// As root, mount directly with mount(2) rather than shelling out to
	// fusermount(1), which may be absent in a minimal container.
	if os.Geteuid() == 0 {
		opts.MountOptions.DirectMount = true
	}
	if !s.opt.ReadOnly {
		opts.MountOptions.Options = nil
	}
	if s.opt.AllowOther {
		opts.MountOptions.AllowOther = true
	}
	opts.MountOptions.SingleThreaded = s.opt.SingleThreaded
	// The kernel must enforce the inode's DAC for every type whose open() is never
	// routed to the daemon: a FIFO, socket, or device node is opened by the
	// kernel's special-file handlers, and without default_permissions FUSE's
	// permission op is a no-op, so an otherwise-uncheckable mode would still open.
	// default_permissions makes the kernel check the mode/uid/gid the daemon
	// reports; the daemon's own checks remain authoritative for the operations it
	// does see.
	opts.MountOptions.Options = append(opts.MountOptions.Options, "default_permissions")
	if s.opt.AttrTTLSeconds != nil {
		d := seconds(*s.opt.AttrTTLSeconds)
		opts.AttrTimeout = &d
	}
	if s.opt.EntryTTLSeconds != nil {
		d := seconds(*s.opt.EntryTTLSeconds)
		opts.EntryTimeout = &d
	}
	return fs.Mount(mnt, root, opts)
}

// node is one FUSE node, identified by our inode id.
type node struct {
	fs.Inode
	id uint64
	s  *Server
}

var _ = (fs.NodeLookuper)((*node)(nil))
var _ = (fs.NodeGetattrer)((*node)(nil))
var _ = (fs.NodeSetattrer)((*node)(nil))
var _ = (fs.NodeReaddirer)((*node)(nil))
var _ = (fs.NodeOpendirer)((*node)(nil))
var _ = (fs.NodeReader)((*node)(nil))
var _ = (fs.NodeWriter)((*node)(nil))
var _ = (fs.NodeOpener)((*node)(nil))
var _ = (fs.NodeCreater)((*node)(nil))
var _ = (fs.NodeMkdirer)((*node)(nil))
var _ = (fs.NodeMknoder)((*node)(nil))
var _ = (fs.NodeUnlinker)((*node)(nil))
var _ = (fs.NodeRmdirer)((*node)(nil))
var _ = (fs.NodeRenamer)((*node)(nil))
var _ = (fs.NodeSymlinker)((*node)(nil))
var _ = (fs.NodeReadlinker)((*node)(nil))
var _ = (fs.NodeLinker)((*node)(nil))
var _ = (fs.NodeGetxattrer)((*node)(nil))
var _ = (fs.NodeSetxattrer)((*node)(nil))
var _ = (fs.NodeRemovexattrer)((*node)(nil))
var _ = (fs.NodeListxattrer)((*node)(nil))
var _ = (fs.NodeStatfser)((*node)(nil))
var _ = (fs.NodeStatxer)((*node)(nil))
var _ = (fs.NodeAccesser)((*node)(nil))
var _ = (fs.NodeFsyncer)((*node)(nil))

func seconds(v float64) (d time.Duration) { return time.Duration(v * float64(time.Second)) }

// credOf extracts the caller's identity, including supplementary groups.
func credOf(ctx context.Context) namespace.Credential {
	caller, ok := fuse.FromContext(ctx)
	if !ok || caller == nil {
		return namespace.Credential{}
	}
	cred := namespace.Credential{UID: caller.Owner.Uid, GID: caller.Owner.Gid}
	cred.Groups = supplementaryGroups(caller.Pid)
	return cred
}

// supplementaryGroups reads /proc/<pid>/status Groups:, matching the reference.
// An unreadable /proc leaves groups empty, which can over-deny but never
// over-grant.
func supplementaryGroups(pid uint32) []uint32 {
	f, err := os.Open("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/status")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Groups:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Groups:"))
		var groups []uint32
		for _, fld := range fields {
			if g, err := strconv.ParseUint(fld, 10, 32); err == nil {
				groups = append(groups, uint32(g))
			}
		}
		return groups
	}
	return nil
}

func stableAttr(in *index.Inode) fs.StableAttr {
	return fs.StableAttr{Mode: modeOf(in), Ino: in.ID}
}

func modeOf(in *index.Inode) uint32 {
	var t uint32
	switch in.Kind {
	case record.KindDir:
		t = syscall.S_IFDIR
	case record.KindSymlink:
		t = syscall.S_IFLNK
	case record.KindFifo:
		t = syscall.S_IFIFO
	case record.KindChar:
		t = syscall.S_IFCHR
	case record.KindBlock:
		t = syscall.S_IFBLK
	case record.KindSocket:
		t = syscall.S_IFSOCK
	default:
		t = syscall.S_IFREG
	}
	return t | (in.Mode & 0o7777)
}

// kindOf maps a mode's file-type bits to the record inode kind. A regular file
// (no type bits) maps to KindFile so mknod(2) of a regular file falls back to a
// create.
func kindOf(mode uint32) uint8 {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFIFO:
		return record.KindFifo
	case syscall.S_IFCHR:
		return record.KindChar
	case syscall.S_IFBLK:
		return record.KindBlock
	case syscall.S_IFSOCK:
		return record.KindSocket
	}
	return record.KindFile
}

func fillAttr(out *fuse.Attr, in *index.Inode) {
	out.Ino = in.ID
	out.Mode = modeOf(in)
	out.Size = in.Size
	out.Rdev = uint32(in.Rdev)
	out.Nlink = in.Nlink
	out.Owner = fuse.Owner{Uid: in.UID, Gid: in.GID}
	out.Atime = uint64(in.Atime.Secs)
	out.Atimensec = in.Atime.Nanos
	out.Mtime = uint64(in.Mtime.Secs)
	out.Mtimensec = in.Mtime.Nanos
	out.Ctime = uint64(in.Ctime.Secs)
	out.Ctimensec = in.Ctime.Nanos
	out.Blocks = (in.Size + 511) / 512
}

func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	return orfserr.ErrnoOf(err)
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	in, err := n.s.ns.Lookup(credOf(ctx), n.id, name)
	if err != nil {
		return nil, errno(err)
	}
	child := n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in))
	fillAttr(&out.Attr, in)
	// The bridge applies the configured entry/attr timeouts when these are left
	// at zero; hardcoding one here would defeat --entry-ttl-ms 0 and let a cached
	// dentry satisfy a walk without the daemon's search-permission re-check.
	return child, 0
}

func (n *node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	in, err := n.s.ns.Getattr(n.id)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, in)
	return 0
}

// fileHandle records the open flags so a received FSETATTR can tell whether it
// arrived through an already-open writable descriptor (Linux ftruncate never
// re-checks the inode's write permission).
type fileHandle struct{ flags uint32 }

func (h *fileHandle) writable() bool {
	return h != nil && h.flags&uint32(syscall.O_ACCMODE) != uint32(syscall.O_RDONLY)
}

func (n *node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	cred := credOf(ctx)
	req := namespace.SetattrRequest{}
	if v, ok := in.GetMode(); ok {
		req.Mode = &v
		// The kernel clears S_ISUID/S_ISGID itself when it does not delegate
		// killpriv to the filesystem; that mode change is the kernel's decision
		// and is applied without a fresh ownership check.
		if in.Valid&fuse.FATTR_KILL_SUIDGID != 0 {
			req.KernelMode = true
		}
	}
	if v, ok := in.GetUID(); ok {
		req.UID = &v
	}
	if v, ok := in.GetGID(); ok {
		req.GID = &v
	}
	if in.Valid&fuse.FATTR_ATIME != 0 {
		if in.Valid&fuse.FATTR_ATIME_NOW != 0 {
			req.AtimeNow = true
		} else if v, ok := in.GetATime(); ok {
			ts := record.Timespec{Secs: v.Unix(), Nanos: uint32(v.Nanosecond())}
			req.Atime = &ts
		}
	}
	if in.Valid&fuse.FATTR_MTIME != 0 {
		if in.Valid&fuse.FATTR_MTIME_NOW != 0 {
			req.MtimeNow = true
		} else if v, ok := in.GetMTime(); ok {
			ts := record.Timespec{Secs: v.Unix(), Nanos: uint32(v.Nanosecond())}
			req.Mtime = &ts
		}
	}
	if v, ok := in.GetSize(); ok {
		req.Size = &v
	}
	if _, ok := in.GetFh(); ok {
		if h, ok := f.(*fileHandle); ok {
			req.WritableHandle = h.writable()
		}
	}
	if err := n.s.ns.Setattr(cred, n.id, req); err != nil {
		return errno(err)
	}
	return n.Getattr(ctx, f, out)
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	infos, _, parent, err := n.s.ns.ReaddirFull(credOf(ctx), n.id)
	if err != nil {
		return nil, errno(err)
	}
	list := make([]fuse.DirEntry, 0, len(infos)+2)
	list = append(list, fuse.DirEntry{Name: ".", Mode: syscall.S_IFDIR, Ino: n.id})
	list = append(list, fuse.DirEntry{Name: "..", Mode: syscall.S_IFDIR, Ino: parent})
	for _, e := range infos {
		var mode uint32 = syscall.S_IFREG
		if e.IsDir {
			mode = syscall.S_IFDIR
		}
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: mode | (e.Mode & 0o7777), Ino: e.Ino})
	}
	return fs.NewListDirStream(list), 0
}

// Opendir enforces the read permission a directory iteration requires, so an
// opendir(2)/readdir(2) of a directory without read permission is EACCES
// (POSIX). The kernel routes directory opens to OPENDIR, not OPEN.
func (n *node) Opendir(ctx context.Context) syscall.Errno {
	in, err := n.s.ns.Getattr(n.id)
	if err != nil {
		return errno(err)
	}
	if !namespace.HasPermission(credOf(ctx), in, 0o4) {
		return syscall.EACCES
	}
	return 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	// POSIX open(2) checks the requested access mode against the inode before
	// handing back a descriptor, so a read-only open of a file without read
	// permission is EACCES here, not at the first read.
	mask := uint32(0o4)
	switch flags & uint32(syscall.O_ACCMODE) {
	case uint32(syscall.O_WRONLY):
		mask = 0o2
	case uint32(syscall.O_RDWR):
		mask = 0o6
	}
	in, err := n.s.ns.Getattr(n.id)
	if err != nil {
		return nil, 0, errno(err)
	}
	if !namespace.HasPermission(credOf(ctx), in, mask) {
		return nil, 0, syscall.EACCES
	}
	// We do not set FOPEN_KEEP_CACHE: a short attribute/entry TTL already bounds
	// reuse, and a cached page could otherwise outlive a concurrent overwrite.
	// When requested, FOPEN_DIRECT_IO bypasses the page cache so a benchmark
	// reads the daemon, not the cache.
	var fuseFlags uint32
	if n.s.opt.DirectIO {
		fuseFlags |= fuse.FOPEN_DIRECT_IO
	}
	return &fileHandle{flags: flags}, fuseFlags, 0
}

func (n *node) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := n.s.ns.Read(credOf(ctx), n.id, uint64(off), uint64(len(dest)))
	if err != nil {
		return nil, errno(err)
	}
	return fuse.ReadResultData(data), 0
}

func (n *node) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	nw, err := n.s.ns.Write(credOf(ctx), n.id, uint64(off), data)
	if err != nil {
		return 0, errno(err)
	}
	_ = nw
	return uint32(len(data)), 0
}

func (n *node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	cred := credOf(ctx)
	in, err := n.s.ns.Create(cred, n.id, name, mode, cred.UID, cred.GID)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	fillAttr(&out.Attr, in)
	child := n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in))
	return child, &fileHandle{flags: flags}, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cred := credOf(ctx)
	in, err := n.s.ns.Mkdir(cred, n.id, name, mode, cred.UID, cred.GID)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, in)
	child := n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in))
	return child, 0
}

// Mknod creates a special file (FIFO, socket, or device node). The kernel applies
// the umask before the request, and provides the type through mode's S_IFMT bits.
func (n *node) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cred := credOf(ctx)
	kind := kindOf(mode)
	if kind == record.KindFile {
		in, err := n.s.ns.Create(cred, n.id, name, mode&0o7777, cred.UID, cred.GID)
		if err != nil {
			return nil, errno(err)
		}
		fillAttr(&out.Attr, in)
		return n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in)), 0
	}
	in, err := n.s.ns.Mknod(cred, n.id, name, kind, mode&0o7777, uint64(rdev), cred.UID, cred.GID)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, in)
	child := n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in))
	return child, 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	return errno(n.s.ns.Unlink(credOf(ctx), n.id, name))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errno(n.s.ns.Rmdir(credOf(ctx), n.id, name))
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	if flags != 0 {
		// Rename flags (NOREPLACE/EXCHANGE) are handled by the kernel for the
		// common cases; an explicit exchange is not wired here.
		return syscall.ENOTSUP
	}
	return errno(n.s.ns.Rename(credOf(ctx), n.id, name, np.id, newName, false))
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cred := credOf(ctx)
	in, err := n.s.ns.Symlink(cred, n.id, name, target, cred.UID, cred.GID)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, in)
	child := n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in))
	return child, 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.s.ns.Readlink(n.id)
	if err != nil {
		return nil, errno(err)
	}
	return target, 0
}

func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	t, ok := target.(*node)
	if !ok {
		return nil, syscall.EXDEV
	}
	if err := n.s.ns.Link(credOf(ctx), n.id, name, t.id); err != nil {
		return nil, errno(err)
	}
	in, err := n.s.ns.Getattr(t.id)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, in)
	child := n.NewInode(ctx, &node{id: in.ID, s: n.s}, stableAttr(in))
	return child, 0
}

func (n *node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	v, err := n.s.ns.Getxattr(credOf(ctx), n.id, attr)
	if err != nil {
		return 0, errno(err)
	}
	if len(dest) == 0 {
		return uint32(len(v)), 0
	}
	if len(v) > len(dest) {
		return 0, syscall.ERANGE
	}
	copy(dest, v)
	return uint32(len(v)), 0
}

func (n *node) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	return errno(n.s.ns.Setxattr(credOf(ctx), n.id, attr, data))
}

func (n *node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	return errno(n.s.ns.Removexattr(credOf(ctx), n.id, attr))
}

func (n *node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	names, err := n.s.ns.Listxattr(credOf(ctx), n.id)
	if err != nil {
		return 0, errno(err)
	}
	var total int
	for _, name := range names {
		total += len(name) + 1
	}
	if len(dest) == 0 {
		return uint32(total), 0
	}
	if total > len(dest) {
		return 0, syscall.ERANGE
	}
	off := 0
	for _, name := range names {
		off += copy(dest[off:], name)
		dest[off] = 0
		off++
	}
	return uint32(total), 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	// Physical-safe: report the retained physical footprint, never the logical.
	if phys, err := n.s.vol.Log.PhysicalBytes(); err == nil {
		out.Blocks = phys / 4096
	}
	out.Bsize = 4096
	out.Frsize = 4096
	out.NameLen = 255
	return 0
}

func (n *node) Statx(ctx context.Context, f fs.FileHandle, flags uint32, mask uint32, out *fuse.StatxOut) syscall.Errno {
	in, err := n.s.ns.Getattr(n.id)
	if err != nil {
		return errno(err)
	}
	out.Mask = 0x7ff // STATX_BASIC_STATS
	out.Ino = in.ID
	out.Mode = uint16(modeOf(in))
	out.Nlink = in.Nlink
	out.Uid = in.UID
	out.Gid = in.GID
	out.Size = in.Size
	out.Blocks = (in.Size + 511) / 512
	return 0
}

func (n *node) Access(ctx context.Context, mask uint32) syscall.Errno {
	in, err := n.s.ns.Getattr(n.id)
	if err != nil {
		return errno(err)
	}
	if !namespace.HasPermission(credOf(ctx), in, mask&0o7) {
		return syscall.EACCES
	}
	return 0
}

func (n *node) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	// Every mutation is committed (fsynced) at Publish, so a fsync is a no-op.
	return 0
}
