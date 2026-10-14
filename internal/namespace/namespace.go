// Package namespace implements ordinary POSIX semantics over the authoritative
// log. Every mutation is expressed as new records in one atomic transaction and
// published through the log; nothing here rewrites committed bytes.
//
// Concurrency: a single RWMutex guards the shared index. Readers take a read
// lock; a mutation takes the write lock for the whole check-and-publish so a
// read never observes a partially applied transaction. Public methods never call
// other public methods (which would risk re-entrant locking); the internal
// helpers assume the lock is already held.
package namespace

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// ManifestSource is the concealed read acceleration seam (Plane C). It is an
// interface so this package never imports the codec: a fault or a miss in the
// manifested plane is a fallback, never a different answer.
type ManifestSource interface {
	ManifestedRead(inode, version, root, offset, size uint64) ([]byte, bool)
}

// Namespace is a live, mutable view over a writable volume.
type Namespace struct {
	vol *volume.Volume
	now func() record.Timespec

	manifest ManifestSource

	mu sync.RWMutex
}

// New builds a namespace over a volume.
func New(v *volume.Volume) *Namespace {
	return &Namespace{vol: v, now: realNow}
}

// Volume returns the backing volume.
func (ns *Namespace) Volume() *volume.Volume { return ns.vol }

// SetManifestSource installs the concealed read seam. A nil source disables it.
func (ns *Namespace) SetManifestSource(src ManifestSource) { ns.manifest = src }

func (ns *Namespace) idx() *index.Index { return ns.vol.Index() }

// realNow returns the current time, or a fixed timestamp when ORFS_CLOCK_EPOCH
// is set. Pinning the clock (with a pinned volume UUID) makes the on-disk bytes
// reproducible, which the cross-FS determinism court relies on.
func realNow() record.Timespec {
	if v := os.Getenv("ORFS_CLOCK_EPOCH"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return record.Timespec{Secs: secs}
		}
	}
	n := time.Now()
	return record.Timespec{Secs: n.Unix(), Nanos: uint32(n.Nanosecond())}
}

func mkRec(t uint16, inodeID uint64, payload []byte) *record.Record {
	return &record.Record{
		Header:  record.Header{RecordType: t, PayloadLength: uint64(len(payload)), InodeID: inodeID},
		Payload: payload,
	}
}

// dir loads an inode and asserts it is a directory, checking search permission.
func (ns *Namespace) dir(cred Credential, id uint64, wantWrite bool) (*index.Inode, error) {
	in, ok := ns.idx().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	if in.Kind != record.KindDir {
		return nil, orfserr.NotDir("inode %d is not a directory", id)
	}
	mask := uint32(0o1)
	if wantWrite {
		mask |= 0o2
	}
	if !HasPermission(cred, in, mask) {
		return nil, orfserr.Access("search permission denied on inode %d", id)
	}
	return in, nil
}

// getattr is the unlocked inode lookup.
func (ns *Namespace) getattr(id uint64) (*index.Inode, error) {
	in, ok := ns.idx().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	return in, nil
}

// Getattr returns an inode.
func (ns *Namespace) Getattr(id uint64) (*index.Inode, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.getattr(id)
}

// checkNameLen rejects a path component longer than NAME_MAX with the
// POSIX-correct ENAMETOOLONG. The kernel splits paths and never passes "."/".."
// to a name-resolving operation, so the other structural rules are already
// guaranteed here (they remain enforced on the committed bytes by record).
func checkNameLen(name string) error {
	if len(name) > record.NameMax {
		return orfserr.NameTooLong("name length %d exceeds %d", len(name), record.NameMax)
	}
	return nil
}

// Lookup resolves a name within a directory.
func (ns *Namespace) Lookup(cred Credential, parent uint64, name string) (*index.Inode, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if err := checkNameLen(name); err != nil {
		return nil, err
	}
	if _, err := ns.dir(cred, parent, false); err != nil {
		return nil, err
	}
	in, ok := ns.idx().Lookup(parent, name)
	if !ok {
		return nil, orfserr.NotFound("no entry %q in %d", name, parent)
	}
	return in, nil
}

// ParentOf returns a node's parent directory id.
func (ns *Namespace) ParentOf(id uint64) (uint64, bool) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.idx().ParentOf(id)
}

// Readdir lists a directory's members.
func (ns *Namespace) Readdir(cred Credential, id uint64) ([]index.DirEntry, error) {
	_, entries, _, err := ns.ReaddirFull(cred, id)
	return entries, err
}

// DirEntryInfo is a directory member resolved for projection.
type DirEntryInfo struct {
	Name  string
	Ino   uint64
	Mode  uint32
	IsDir bool
}

// ReaddirFull lists a directory resolved to entries plus the parent id.
func (ns *Namespace) ReaddirFull(cred Credential, id uint64) ([]DirEntryInfo, []index.DirEntry, uint64, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if _, err := ns.dir(cred, id, false); err != nil {
		return nil, nil, 0, err
	}
	children := ns.idx().Children(id)
	infos := make([]DirEntryInfo, 0, len(children))
	for _, e := range children {
		in, ok := ns.idx().Inode(e.Child)
		if !ok {
			continue
		}
		infos = append(infos, DirEntryInfo{
			Name:  e.Name,
			Ino:   e.Child,
			Mode:  in.Mode,
			IsDir: in.Kind == record.KindDir,
		})
	}
	parent, _ := ns.idx().ParentOf(id)
	if parent == 0 {
		parent = id
	}
	return infos, children, parent, nil
}

// Read reads a file's bytes. When a manifested body is certified for the exact
// (inode, version, committed root), it is served from there; otherwise the read
// is served from authority. Both paths yield identical bytes.
func (ns *Namespace) Read(cred Credential, id, offset, size uint64) ([]byte, error) {
	ns.mu.RLock()
	in, err := ns.getattr(id)
	if err != nil {
		ns.mu.RUnlock()
		return nil, err
	}
	if !HasPermission(cred, in, 0o4) {
		ns.mu.RUnlock()
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	if in.Kind != record.KindFile {
		ns.mu.RUnlock()
		return nil, orfserr.IsDir("inode %d is not a regular file", id)
	}
	version := in.Version
	src := ns.manifest
	root := ns.vol.Log.CommittedRoot()
	ns.mu.RUnlock()

	if src != nil {
		if b, ok := src.ManifestedRead(id, version, root, offset, size); ok {
			return b, nil
		}
	}
	return ns.ReadAuthoritative(cred, id, offset, size)
}

// ReadAuthoritative reads a file's bytes from authority, never consulting the
// manifested plane. It is the oracle a manifested body must agree with.
func (ns *Namespace) ReadAuthoritative(cred Credential, id, offset, size uint64) ([]byte, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	in, err := ns.getattr(id)
	if err != nil {
		return nil, err
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	return ns.readUnchecked(in, offset, size)
}

// ReadUnchecked reads without a permission check, under the read lock.
func (ns *Namespace) ReadUnchecked(in *index.Inode, offset, size uint64) ([]byte, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.readUnchecked(in, offset, size)
}

func (ns *Namespace) readUnchecked(in *index.Inode, offset, size uint64) ([]byte, error) {
	if in.Kind != record.KindFile {
		return nil, orfserr.IsDir("inode %d is not a regular file", in.ID)
	}
	if offset >= in.Size || size == 0 {
		return []byte{}, nil
	}
	want := size
	if offset+want > in.Size {
		want = in.Size - offset
	}
	out := make([]byte, want)
	pieces := in.Extents.PlanRead(offset, want)
	var at uint64
	for _, p := range pieces {
		dst := out[at : at+p.Length]
		if !p.Zero {
			if _, err := ns.vol.Reader().ReadAt(p.DataOffset, dst); err != nil {
				return nil, err
			}
		}
		at += p.Length
	}
	return out, nil
}

// ReadAll reads a file's entire content (used to build a manifested body).
func (ns *Namespace) ReadAll(cred Credential, id uint64) ([]byte, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	in, err := ns.getattr(id)
	if err != nil {
		return nil, err
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	return ns.readUnchecked(in, 0, in.Size)
}

// Readlink returns a symlink's target under the read lock.
func (ns *Namespace) Readlink(id uint64) ([]byte, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	in, err := ns.getattr(id)
	if err != nil {
		return nil, err
	}
	if in.Kind != record.KindSymlink {
		return nil, orfserr.Invalid("inode %d is not a symbolic link", id)
	}
	return append([]byte(nil), in.Target...), nil
}

// Listxattr returns an inode's extended-attribute names under the read lock.
func (ns *Namespace) Listxattr(cred Credential, id uint64) ([]string, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	in, err := ns.getattr(id)
	if err != nil {
		return nil, err
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	names := make([]string, 0, len(in.Xattrs))
	for name := range in.Xattrs {
		names = append(names, name)
	}
	return names, nil
}

// namesOf counts the directory entries naming an inode.
func (ns *Namespace) namesOf(child uint64) int {
	n := 0
	for _, e := range ns.idx().Dentry {
		if e == child {
			n++
		}
	}
	return n
}
