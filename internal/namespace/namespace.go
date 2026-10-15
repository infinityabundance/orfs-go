// Package namespace implements ordinary POSIX semantics over the authoritative
// log. Every mutation is expressed as new records in one atomic transaction and
// published through the log; nothing here rewrites committed bytes.
//
// Concurrency: reads are lock-free. The writer mutates the live index in place
// under a write-side mutex and, once a transaction is committed, publishes a new
// immutable read view through volume.Volume's atomic pointer. A reader loads the
// current view once per request and traverses it with no lock, so concurrent
// readers never contend with each other or with the writer. Because the view is
// replaced only after the whole transaction has been applied, a read observes a
// consistent committed state and never a partially applied one.
//
// Public methods never call other public methods (which would risk re-entrant
// locking); the internal helpers assume the writer's lock is already held.
package namespace

import (
	"os"
	"strconv"
	"time"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/view"
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

	// compat is the Rust-reference compatibility profile. When set, mutations
	// never emit a record type outside the reference's format-1 registry and
	// semantics: no CTIME_UPDATE (ctime is derived from mtime, as the reference
	// does) and no MKNOD_CREATE (special files are refused). It is a *writer*
	// policy; reads are unaffected. See docs/MIGRATION.md §4a.
	compat bool

	manifest ManifestSource

	// res serialises staged mutations by the namespace keys they touch. A
	// mutation reserves its keys, releases the write lock across the durability
	// barrier, and releases the reservation only once the transaction has
	// committed and been applied. Conflicting writers therefore wait for a commit
	// (and its index apply) rather than for the barrier itself, while
	// non-conflicting writers run in parallel and their barriers batch in the
	// log's group committer.
	res *reservations
}

// New builds a namespace over a volume.
func New(v *volume.Volume) *Namespace {
	return &Namespace{vol: v, now: realNow, compat: v.Compat(), res: newReservations(v.Index().NextInodeID)}
}

// Volume returns the backing volume.
func (ns *Namespace) Volume() *volume.Volume { return ns.vol }

// Compat reports whether this namespace is in the Rust-reference compatibility
// profile.
func (ns *Namespace) Compat() bool { return ns.compat }

// ctimeRec returns the records that advance an inode's ctime for a metadata-only
// change. In the compatibility profile it returns nothing: the reference cannot
// express a ctime-only advance and derives ctime from mtime, so a compatible
// writer omits the record and lets the same derive-on-setattr rule apply.
func (ns *Namespace) ctimeRec(id uint64, tm record.Timespec) []*record.Record {
	if ns.compat {
		return nil
	}
	return []*record.Record{mkRec(record.TypeCtimeUpdate, 0, record.CtimeUpdate{InodeID: id, Ctime: tm}.Encode())}
}

// SetManifestSource installs the concealed read seam. A nil source disables it.
func (ns *Namespace) SetManifestSource(src ManifestSource) { ns.manifest = src }

// allocID reserves an inode id for a new inode.
func (ns *Namespace) allocID() uint64 { return ns.res.allocID() }

// stageFn is one mutation's planner and builder. On the first (final=false) call
// it derives the reservation only; on the final call it also builds the record
// set (allocating an inode id when it needs one). Both calls see a committed
// view, and the final call runs with the reservation held.
type stageFn func(v *view.View, final bool) (reservation, []*record.Record, error)

// staged reserves a mutation's keys and rebuilds it under the reservation
// against the current committed view. It returns the records and the held
// reservation, which the caller must release once the commit has been applied.
//
// The reservation is re-derived from the freshly loaded view under the lock: if
// the committed state moved while the transaction waited (so the keys it would
// need changed), it releases and retries rather than committing a record set
// built against stale state.
func (ns *Namespace) staged(fn stageFn) (reservation, []*record.Record, error) {
	for {
		v := ns.vol.View()
		res, _, err := fn(v, false)
		if err != nil {
			return reservation{}, nil, err
		}
		ns.res.acquire(res)
		v2 := ns.vol.View()
		res2, recs, err := fn(v2, true)
		if err != nil {
			ns.res.release(res)
			return reservation{}, nil, err
		}
		if !res.same(res2) {
			ns.res.release(res)
			continue
		}
		return res, recs, nil
	}
}

// publishStaged stages fn, publishes its record set as one committed
// transaction, and releases the reservation once the transaction has been
// applied. An empty record set is not published.
func (ns *Namespace) publishStaged(fn stageFn) error {
	res, recs, err := ns.staged(fn)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		ns.res.release(res)
		return nil
	}
	_, perr := ns.vol.Publish(recs)
	ns.res.release(res)
	return perr
}

// inodeAfter returns a just-committed inode from the published view.
func (ns *Namespace) inodeAfter(id uint64) (*index.Inode, error) {
	if in, ok := ns.vol.View().Inode(id); ok {
		return in, nil
	}
	return nil, orfserr.NotFound("inode %d does not exist", id)
}

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

// dir loads an inode from a committed view and asserts it is a directory,
// checking search permission.
func viewDir(v *view.View, cred Credential, id uint64, wantWrite bool) (*index.Inode, error) {
	in, ok := v.Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	return checkDir(cred, id, in, wantWrite)
}

func checkDir(cred Credential, id uint64, in *index.Inode, wantWrite bool) (*index.Inode, error) {
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

// getattr is the inode lookup against a committed view.
func viewGetattr(v *view.View, id uint64) (*index.Inode, error) {
	in, ok := v.Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	return in, nil
}

// Getattr returns an inode from the current immutable read view.
func (ns *Namespace) Getattr(id uint64) (*index.Inode, error) {
	in, ok := ns.vol.View().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	return in, nil
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

// Lookup resolves a name within a directory from the current read view.
func (ns *Namespace) Lookup(cred Credential, parent uint64, name string) (*index.Inode, error) {
	if err := checkNameLen(name); err != nil {
		return nil, err
	}
	v := ns.vol.View()
	if _, err := viewDir(v, cred, parent, false); err != nil {
		return nil, err
	}
	in, ok := v.Lookup(parent, name)
	if !ok {
		return nil, orfserr.NotFound("no entry %q in %d", name, parent)
	}
	return in, nil
}

// ParentOf returns a node's parent directory id from the current read view.
func (ns *Namespace) ParentOf(id uint64) (uint64, bool) {
	return ns.vol.View().ParentOf(id)
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

// ReaddirFull lists a directory resolved to entries plus the parent id. The
// whole listing is answered from one loaded view, so the entries and their inodes
// are mutually consistent even while a writer commits.
func (ns *Namespace) ReaddirFull(cred Credential, id uint64) ([]DirEntryInfo, []index.DirEntry, uint64, error) {
	v := ns.vol.View()
	if _, err := viewDir(v, cred, id, false); err != nil {
		return nil, nil, 0, err
	}
	children := v.Children(id)
	infos := make([]DirEntryInfo, 0, len(children))
	for _, e := range children {
		in, ok := v.Inode(e.Child)
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
	parent, _ := v.ParentOf(id)
	if parent == 0 {
		parent = id
	}
	return infos, children, parent, nil
}

// Read reads a file's bytes. When a manifested body is certified for the exact
// (inode, version, committed root), it is served from there; otherwise the read
// is served from authority. Both paths yield identical bytes. The inode is taken
// from the current immutable view, so no lock is held.
func (ns *Namespace) Read(cred Credential, id, offset, size uint64) ([]byte, error) {
	v := ns.vol.View()
	in, ok := v.Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	if in.Kind != record.KindFile {
		return nil, orfserr.IsDir("inode %d is not a regular file", id)
	}
	version := in.Version
	src := ns.manifest
	// The committed root is taken from the same view as the inode, so the
	// manifested-identity key is internally consistent, and the read never touches
	// the writer's live log state.
	root := v.Root()

	if src != nil {
		if b, ok := src.ManifestedRead(id, version, root, offset, size); ok {
			return b, nil
		}
	}
	return ns.readUnchecked(in, offset, size)
}

// ReadAuthoritative reads a file's bytes from authority, never consulting the
// manifested plane. It is the oracle a manifested body must agree with, served
// lock-free from the current immutable view.
func (ns *Namespace) ReadAuthoritative(cred Credential, id, offset, size uint64) ([]byte, error) {
	in, ok := ns.vol.View().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	return ns.readUnchecked(in, offset, size)
}

// ReadUnchecked reads without a permission check. in must come from a published
// read view; the inode is immutable, so no lock is taken.
func (ns *Namespace) ReadUnchecked(in *index.Inode, offset, size uint64) ([]byte, error) {
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
	// The common read lands in a single contiguous extent; a small caller-owned
	// array keeps plan resolution from allocating a fresh []Piece every read.
	var plan [4]index.Piece
	pieces := in.Extents.PlanReadInto(offset, want, plan[:0])
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

// ReadAll reads a file's entire content (used to build a manifested body), from
// the current read view.
func (ns *Namespace) ReadAll(cred Credential, id uint64) ([]byte, error) {
	in, ok := ns.vol.View().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	return ns.readUnchecked(in, 0, in.Size)
}

// Readlink returns a symlink's target from the current read view.
func (ns *Namespace) Readlink(id uint64) ([]byte, error) {
	in, ok := ns.vol.View().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	if in.Kind != record.KindSymlink {
		return nil, orfserr.Invalid("inode %d is not a symbolic link", id)
	}
	return append([]byte(nil), in.Target...), nil
}

// Listxattr returns an inode's extended-attribute names from the current read
// view.
func (ns *Namespace) Listxattr(cred Credential, id uint64) ([]string, error) {
	in, ok := ns.vol.View().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
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

// namesOf counts the directory entries naming an inode, from a committed view.
func namesOf(v *view.View, child uint64) int { return v.NameCount(child) }
