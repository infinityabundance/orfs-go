package namespace

import (
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/view"
)

// Every mutation below is expressed as a stageFn: it derives the namespace keys
// its record set depends on (the directory-entry names it reads or writes and
// the inode ids whose mutable fields it reads or writes), and, on the final
// call, builds the records. Namespace.staged reserves those keys, rebuilds the
// records under the reservation against the current committed view, then the
// records are published as one authoritative transaction; the reservation is
// released once the transaction has been applied. Two writers validating the
// same key against a stale view therefore cannot both commit: the second blocks
// on the reservation until the first has committed, then re-validates.

// Create creates a regular file.
func (ns *Namespace) Create(cred Credential, parent uint64, name string, mode, uid, gid uint32) (*index.Inode, error) {
	var id uint64
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		if _, err := viewDir(v, cred, parent, true); err != nil {
			return r, nil, err
		}
		if err := record.ValidateName([]byte(name)); err != nil {
			return r, nil, err
		}
		if _, ok := v.Lookup(parent, name); ok {
			return r, nil, orfserr.Exists("entry %q already exists in %d", name, parent)
		}
		r.addName(parent, name)
		r.addShared(parent)
		if !final {
			return r, nil, nil
		}
		id = ns.allocID()
		tm := ns.now()
		ic := record.InodeCreate{
			InodeID: id, Generation: 1, Kind: record.KindFile,
			Mode: mode & 0o7777, UID: uid, GID: gid, Nlink: 1,
			Atime: tm, Mtime: tm, Ctime: tm,
		}
		recs := []*record.Record{
			mkRec(record.TypeInodeCreate, 0, ic.Encode()),
			mkRec(record.TypeDentryAdd, 0, record.EncodeDentryAdd(record.DentryAdd{Parent: parent, Child: id, Name: []byte(name)})),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
	if err != nil {
		return nil, err
	}
	return ns.inodeAfter(id)
}

// Mkdir creates a directory.
func (ns *Namespace) Mkdir(cred Credential, parent uint64, name string, mode, uid, gid uint32) (*index.Inode, error) {
	var id uint64
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		p, err := viewDir(v, cred, parent, true)
		if err != nil {
			return r, nil, err
		}
		if err := record.ValidateName([]byte(name)); err != nil {
			return r, nil, err
		}
		if _, ok := v.Lookup(parent, name); ok {
			return r, nil, orfserr.Exists("entry %q already exists in %d", name, parent)
		}
		r.addName(parent, name)
		// The parent's link count rises: an absolute value, so it is reserved
		// exclusively.
		r.addExcl(parent)
		if !final {
			return r, nil, nil
		}
		id = ns.allocID()
		tm := ns.now()
		ic := record.InodeCreate{
			InodeID: id, Generation: 1, Kind: record.KindDir,
			Mode: mode & 0o7777, UID: uid, GID: gid, Nlink: 2,
			Atime: tm, Mtime: tm, Ctime: tm,
		}
		recs := []*record.Record{
			mkRec(record.TypeInodeCreate, 0, ic.Encode()),
			mkRec(record.TypeDentryAdd, 0, record.EncodeDentryAdd(record.DentryAdd{Parent: parent, Child: id, Name: []byte(name)})),
			// The parent gains a subdirectory: its link count rises and its
			// timestamps change (published explicitly, in the same transaction).
			mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: parent, Nlink: p.Nlink + 1}.Encode()),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
	if err != nil {
		return nil, err
	}
	return ns.inodeAfter(id)
}

// Mknod creates a special file: a FIFO, Unix-domain socket, or character/block
// device node. kind is a record inode kind and mode carries only the permission
// bits (the type is the kind). A non-root caller may not create a device node
// (EPERM); a FIFO or socket only needs write permission on the parent.
func (ns *Namespace) Mknod(cred Credential, parent uint64, name string, kind uint8, mode uint32, rdev uint64, uid, gid uint32) (*index.Inode, error) {
	var id uint64
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if kind < record.KindFifo || kind > record.KindSocket {
			return r, nil, orfserr.Invalid("unsupported special-file kind %d", kind)
		}
		if ns.compat {
			return r, nil, orfserr.NotSupported("special files are not supported in the Rust-reference compatibility profile")
		}
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		if _, err := viewDir(v, cred, parent, true); err != nil {
			return r, nil, err
		}
		if err := record.ValidateName([]byte(name)); err != nil {
			return r, nil, err
		}
		if _, ok := v.Lookup(parent, name); ok {
			return r, nil, orfserr.Exists("entry %q already exists in %d", name, parent)
		}
		if (kind == record.KindChar || kind == record.KindBlock) && cred.UID != 0 {
			return r, nil, orfserr.Permission("creating a device node requires root")
		}
		r.addName(parent, name)
		r.addShared(parent)
		if !final {
			return r, nil, nil
		}
		id = ns.allocID()
		tm := ns.now()
		mc := record.MknodCreate{
			InodeID: id, Generation: 1, Kind: kind,
			Mode: mode & 0o7777, UID: uid, GID: gid, Nlink: 1, Rdev: rdev,
			Atime: tm, Mtime: tm, Ctime: tm,
		}
		recs := []*record.Record{
			mkRec(record.TypeMknodCreate, 0, mc.Encode()),
			mkRec(record.TypeDentryAdd, 0, record.EncodeDentryAdd(record.DentryAdd{Parent: parent, Child: id, Name: []byte(name)})),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
	if err != nil {
		return nil, err
	}
	return ns.inodeAfter(id)
}

// Symlink creates a symbolic link.
func (ns *Namespace) Symlink(cred Credential, parent uint64, name string, target string, uid, gid uint32) (*index.Inode, error) {
	var id uint64
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		if _, err := viewDir(v, cred, parent, true); err != nil {
			return r, nil, err
		}
		if err := record.ValidateName([]byte(name)); err != nil {
			return r, nil, err
		}
		if len(target) == 0 || len(target) > record.SymlinkTargetMax {
			return r, nil, orfserr.NameTooLong("symlink target length %d out of range", len(target))
		}
		if _, ok := v.Lookup(parent, name); ok {
			return r, nil, orfserr.Exists("entry %q already exists in %d", name, parent)
		}
		r.addName(parent, name)
		r.addShared(parent)
		if !final {
			return r, nil, nil
		}
		id = ns.allocID()
		tm := ns.now()
		sc := record.SymlinkCreate{
			InodeID: id, Generation: 1, Mode: 0o777, UID: uid, GID: gid, Mtime: tm, Target: []byte(target),
		}
		recs := []*record.Record{
			mkRec(record.TypeSymlinkCreate, 0, sc.Encode()),
			mkRec(record.TypeDentryAdd, 0, record.EncodeDentryAdd(record.DentryAdd{Parent: parent, Child: id, Name: []byte(name)})),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
	if err != nil {
		return nil, err
	}
	return ns.inodeAfter(id)
}

// Write writes bytes at a logical offset and returns the new size. Concurrent
// writes to the same inode are not serialised against each other (a data extent
// is positional, so they compose); the returned size is the committed size after
// this write applied, which may include a racing write.
func (ns *Namespace) Write(cred Credential, id, offset uint64, data []byte) (uint64, error) {
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		in, err := viewGetattr(v, id)
		if err != nil {
			return r, nil, err
		}
		if in.Kind == record.KindDir {
			return r, nil, orfserr.IsDir("inode %d is a directory", id)
		}
		if !HasPermission(cred, in, 0o2) {
			return r, nil, orfserr.Access("write permission denied on inode %d", id)
		}
		r.addShared(id)
		if len(data) == 0 || !final {
			return r, nil, nil
		}
		de := record.DataExtent{Offset: offset, Data: data}.Encode()
		tm := ns.now()
		// A write is a content mutation: it advances the file's mtime and ctime in
		// the same authoritative transaction (docs/FUSE-SEMANTICS.md §5.1).
		recs := []*record.Record{
			mkRec(record.TypeDataExtent, id, de),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
	if err != nil {
		return 0, err
	}
	in2, err := ns.inodeAfter(id)
	if err != nil {
		return 0, err
	}
	return in2.Size, nil
}

// Truncate resizes a file. Shrinking hides bytes but retains them; growing
// zero-fills, so truncated bytes are never resurrected.
func (ns *Namespace) Truncate(cred Credential, id, size uint64) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		in, err := viewGetattr(v, id)
		if err != nil {
			return r, nil, err
		}
		if in.Kind == record.KindDir {
			return r, nil, orfserr.IsDir("inode %d is a directory", id)
		}
		if !HasPermission(cred, in, 0o2) {
			return r, nil, orfserr.Access("write permission denied on inode %d", id)
		}
		r.addShared(id)
		if !final {
			return r, nil, nil
		}
		fr := record.FileResize{NewSize: size}.Encode()
		tm := ns.now()
		// A truncate is a content mutation: it advances mtime and ctime (POSIX).
		recs := []*record.Record{
			mkRec(record.TypeFileResize, id, fr),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
}

// Unlink removes a name from a directory. The inode and its bytes are retained.
func (ns *Namespace) Unlink(cred Credential, parent uint64, name string) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		p, err := viewDir(v, cred, parent, true)
		if err != nil {
			return r, nil, err
		}
		child, ok := v.Lookup(parent, name)
		if !ok {
			return r, nil, orfserr.NotFound("no entry %q in %d", name, parent)
		}
		if child.Kind == record.KindDir {
			return r, nil, orfserr.IsDir("use rmdir for directory %q", name)
		}
		if err := ns.checkSticky(cred, p, child); err != nil {
			return r, nil, err
		}
		r.addName(parent, name)
		// The child's link count is emitted as an absolute value, and the record
		// type depends on it, so the child is reserved exclusively: two unlinks of
		// different names of one inode serialise.
		r.addExcl(child.ID)
		r.addShared(parent)
		if !final {
			return r, nil, nil
		}
		remaining := uint32(namesOf(v, child.ID) - 1)
		rem := record.DentryRemove{Parent: parent, Name: []byte(name)}
		var recs []*record.Record
		if remaining > 0 {
			recs = append(recs, mkRec(record.TypeHardlinkRemove, 0, record.EncodeHardlinkRemove(rem)))
		} else {
			recs = append(recs, mkRec(record.TypeDentryRemove, 0, record.EncodeDentryRemove(rem)))
		}
		recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: child.ID, Nlink: remaining}.Encode()))
		tm := ns.now()
		// Removing a name is a metadata change: the surviving inode's ctime advances.
		recs = append(recs, ns.ctimeRec(child.ID, tm)...)
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()))
		return r, recs, nil
	})
}

// Rmdir removes an empty directory.
func (ns *Namespace) Rmdir(cred Credential, parent uint64, name string) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		p, err := viewDir(v, cred, parent, true)
		if err != nil {
			return r, nil, err
		}
		child, ok := v.Lookup(parent, name)
		if !ok {
			return r, nil, orfserr.NotFound("no entry %q in %d", name, parent)
		}
		if child.Kind != record.KindDir {
			return r, nil, orfserr.NotDir("%q is not a directory", name)
		}
		if child.ID == record.RootInode {
			return r, nil, orfserr.Permission("cannot remove the root directory")
		}
		if len(v.Children(child.ID)) > 0 {
			return r, nil, orfserr.NotEmpty("directory %q is not empty", name)
		}
		if err := ns.checkSticky(cred, p, child); err != nil {
			return r, nil, err
		}
		r.addName(parent, name)
		// Both link counts change as absolute values, and the emptiness check
		// must hold through the commit (a concurrent create in the child takes a
		// shared reservation on it, so it is serialised).
		r.addExcl(parent)
		r.addExcl(child.ID)
		if !final {
			return r, nil, nil
		}
		tm := ns.now()
		recs := []*record.Record{
			mkRec(record.TypeDentryRemove, 0, record.EncodeDentryRemove(record.DentryRemove{Parent: parent, Name: []byte(name)})),
			mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: child.ID, Nlink: 0}.Encode()),
			mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: parent, Nlink: p.Nlink - 1}.Encode()),
			mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
		}
		return r, recs, nil
	})
}

// Rename atomically moves or exchanges a directory entry. Identity (the inode
// id) is preserved.
func (ns *Namespace) Rename(cred Credential, oldParent uint64, oldName string, newParent uint64, newName string, exchange bool) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(oldName); err != nil {
			return r, nil, err
		}
		if err := checkNameLen(newName); err != nil {
			return r, nil, err
		}
		op, err := viewDir(v, cred, oldParent, true)
		if err != nil {
			return r, nil, err
		}
		np, err := viewDir(v, cred, newParent, true)
		if err != nil {
			return r, nil, err
		}
		child, ok := v.Lookup(oldParent, oldName)
		if !ok {
			return r, nil, orfserr.NotFound("no entry %q in %d", oldName, oldParent)
		}
		if child.Kind == record.KindDir && isSelfOrDescendantView(v, newParent, child.ID) {
			return r, nil, orfserr.Invalid("rename would create a directory cycle")
		}
		// Sticky-bit rule: in a sticky directory only the entry's owner, the
		// directory's owner, or root may rename an entry out of it, or rename another
		// entry over one (POSIX). Linux reports EPERM/EACCES for both.
		if err := ns.checkSticky(cred, op, child); err != nil {
			return r, nil, err
		}
		if dest, ok := v.Lookup(newParent, newName); ok && dest.ID != child.ID {
			if err := ns.checkSticky(cred, np, dest); err != nil {
				return r, nil, err
			}
		}
		// Moving a directory to a different parent rewrites its ".." entry, which
		// needs write permission on the directory itself (Linux). A same-parent
		// rename does not touch it.
		if child.Kind == record.KindDir && newParent != oldParent && !HasPermission(cred, child, 0o2) {
			return r, nil, orfserr.Permission("rename of a directory across parents requires write permission on it")
		}

		// Both names and both containing directories are reserved: the names fix
		// the moved entry and any entry it overwrites, and the directories' link
		// counts are emitted as absolute values.
		r.addName(oldParent, oldName)
		r.addName(newParent, newName)
		r.addExcl(oldParent)
		r.addExcl(newParent)

		if exchange {
			other, ok := v.Lookup(newParent, newName)
			if !ok {
				return r, nil, orfserr.NotFound("no entry %q in %d", newName, newParent)
			}
			if (child.Kind == record.KindDir) != (other.Kind == record.KindDir) {
				return r, nil, orfserr.Invalid("exchange requires matching types")
			}
			if !final {
				return r, nil, nil
			}
			dr := record.DentryRename{Flags: record.RenameExchange, OldParent: oldParent, NewParent: newParent, OldName: []byte(oldName), NewName: []byte(newName)}
			return r, []*record.Record{mkRec(record.TypeDentryRename, 0, dr.Encode())}, nil
		}

		destIsDir := false
		hasDest := false
		var dest *index.Inode
		if d, ok := v.Lookup(newParent, newName); ok {
			if d.ID == child.ID {
				// Same file: a successful no-op (no records).
				return r, nil, nil
			}
			if d.Kind == record.KindDir && len(v.Children(d.ID)) > 0 {
				return r, nil, orfserr.NotEmpty("destination directory %q is not empty", newName)
			}
			if child.Kind == record.KindDir && d.Kind != record.KindDir {
				return r, nil, orfserr.IsDir("cannot overwrite a file with a directory")
			}
			if child.Kind != record.KindDir && d.Kind == record.KindDir {
				return r, nil, orfserr.IsDir("cannot overwrite a directory with a file")
			}
			destIsDir = d.Kind == record.KindDir
			hasDest = true
			dest = d
		}
		if !final {
			return r, nil, nil
		}
		var recs []*record.Record
		if hasDest {
			// Overwrite is a preceding removal in the same transaction. The
			// destination loses exactly one name: a directory becomes unlinked
			// (nlink 0), a multiply-linked file keeps its remaining links.
			destNlink := uint32(0)
			if !destIsDir && dest.Nlink > 0 {
				destNlink = dest.Nlink - 1
			}
			recs = append(recs, mkRec(record.TypeDentryRemove, 0, record.EncodeDentryRemove(record.DentryRemove{Parent: newParent, Name: []byte(newName)})))
			recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: dest.ID, Nlink: destNlink}.Encode()))
			// Removing the overwritten name updates the destination inode's ctime.
			recs = append(recs, ns.ctimeRec(dest.ID, ns.now())...)
		}
		dr := record.DentryRename{Flags: record.RenameMove, OldParent: oldParent, NewParent: newParent, OldName: []byte(oldName), NewName: []byte(newName)}
		recs = append(recs, mkRec(record.TypeDentryRename, 0, dr.Encode()))
		tm := ns.now()
		// A directory's link count tracks the subdirectories that point at it with
		// "..": moving one out of oldParent and into newParent adjusts both. The
		// new parent also loses a subdirectory when an existing one is overwritten.
		if child.Kind == record.KindDir {
			oldNlink := op.Nlink
			if oldNlink > 0 {
				oldNlink--
			}
			if newParent != oldParent {
				recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: oldParent, Nlink: oldNlink}.Encode()))
			}
			newNlink := np.Nlink
			if newParent != oldParent {
				newNlink++
			}
			if destIsDir && newNlink > 0 {
				newNlink--
			}
			if newNlink != np.Nlink {
				recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: newParent, Nlink: newNlink}.Encode()))
			}
		} else if destIsDir {
			if np.Nlink > 0 {
				recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: newParent, Nlink: np.Nlink - 1}.Encode()))
			}
		}
		// A rename marks both containing directories' mtime and ctime for update.
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: oldParent, Mtime: &tm}.Encode()))
		if newParent != oldParent {
			recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: newParent, Mtime: &tm}.Encode()))
		}
		return r, recs, nil
	})
}

// Link adds a hard link. Directories cannot be hard-linked (EPERM).
func (ns *Namespace) Link(cred Credential, parent uint64, name string, target uint64) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		if _, err := viewDir(v, cred, parent, true); err != nil {
			return r, nil, err
		}
		targetIn, err := viewGetattr(v, target)
		if err != nil {
			return r, nil, err
		}
		if targetIn.Kind == record.KindDir {
			return r, nil, orfserr.Permission("hard-linking a directory is not permitted")
		}
		if _, ok := v.Lookup(parent, name); ok {
			return r, nil, orfserr.Exists("entry %q already exists in %d", name, parent)
		}
		r.addName(parent, name)
		// The target's link count is emitted as an absolute value.
		r.addExcl(target)
		r.addShared(parent)
		if !final {
			return r, nil, nil
		}
		tm := ns.now()
		recs := []*record.Record{
			mkRec(record.TypeHardlinkAdd, 0, record.EncodeHardlinkAdd(record.DentryAdd{Parent: parent, Child: target, Name: []byte(name)})),
			mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: target, Nlink: targetIn.Nlink + 1}.Encode()),
		}
		// Adding a link is a metadata change: the target's ctime advances, and the
		// containing directory gains an entry (its mtime/ctime advance).
		recs = append(recs, ns.ctimeRec(target, tm)...)
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()))
		return r, recs, nil
	})
}

// SetattrRequest is a partial attribute change. An omitted timestamp (all four
// time fields zero) is utimensat's UTIME_OMIT; AtimeNow/MtimeNow are UTIME_NOW.
type SetattrRequest struct {
	Mode  *uint32
	UID   *uint32
	GID   *uint32
	Atime *record.Timespec
	Mtime *record.Timespec
	// AtimeNow/MtimeNow request the current time (utimensat UTIME_NOW).
	AtimeNow bool
	MtimeNow bool
	Size     *uint64
	// WritableHandle reports that the request arrives through an already-open
	// writable descriptor (FUSE FATTR_FH): a size change then needs no fresh
	// write-permission check, exactly as Linux ftruncate never re-checks.
	WritableHandle bool
	// KernelMode reports a mode change originated by the kernel to clear
	// S_ISUID/S_ISGID after a write (FUSE FATTR_KILL_SUIDGID). It is applied
	// without an ownership check, because the kernel already decided it.
	KernelMode bool
}

// Setattr applies a partial attribute change with the reference's permission
// model (docs/FUSE-SEMANTICS.md §5, §5.1, §5.2): chmod/chown and an explicit
// utimes require owner or root; a utimensat(UTIME_NOW) needs only write
// permission for a non-owner; changing the owner requires root; changing the
// group requires root or an owner that belongs to the target group; a size
// change requires write permission unless it carries an open writable handle.
func (ns *Namespace) Setattr(cred Credential, id uint64, req SetattrRequest) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		in, err := viewGetattr(v, id)
		if err != nil {
			return r, nil, err
		}
		isRoot := cred.UID == 0
		ownerOrRoot := isRoot || cred.UID == in.UID
		metaChange := req.Mode != nil || req.UID != nil || req.GID != nil
		timeChange := req.Atime != nil || req.Mtime != nil || req.AtimeNow || req.MtimeNow
		// Linux ATTR_TOUCH: utimensat with both fields UTIME_NOW is the one time
		// update a non-owner may perform, provided it has write permission.
		touchLike := !metaChange && req.AtimeNow && req.MtimeNow
		// go-fuse cannot negotiate FUSE_HANDLE_KILLPRIV, so the kernel does not
		// delegate the post-write setid clearing to the daemon. Instead it sends a
		// mode-only SETATTR carrying the already-cleared mode (S_ISUID/S_ISGID
		// removed). Recognise exactly that pattern, for a caller that could have
		// written the file, and apply it as the kernel would.
		kernelKillSetID := req.Mode != nil && req.UID == nil && req.GID == nil &&
			!timeChange && req.Size == nil && in.Mode&0o6000 != 0 &&
			(*req.Mode&0o7777) == in.Mode&^0o6000 &&
			(ownerOrRoot || HasPermission(cred, in, 0o2))
		if !req.KernelMode {
			switch {
			case kernelKillSetID:
				// The kernel already decided; no ownership re-check.
			case touchLike:
				if !ownerOrRoot && !HasPermission(cred, in, 0o2) {
					return r, nil, orfserr.Access("timestamps require ownership or write permission")
				}
			case (metaChange || timeChange) && !ownerOrRoot:
				return r, nil, orfserr.Permission("only the owner or root may change mode, ownership or an explicit timestamp")
			}
			if req.UID != nil && *req.UID != in.UID && !isRoot {
				return r, nil, orfserr.Permission("changing the owner requires root")
			}
			if req.GID != nil && *req.GID != in.GID && !isRoot {
				if !ownerOrRoot || !inGroup(cred, *req.GID) {
					return r, nil, orfserr.Permission("changing the group requires root or membership in the target group")
				}
			}
			if req.Size != nil && !req.WritableHandle && !HasPermission(cred, in, 0o2) {
				return r, nil, orfserr.Access("write permission denied on inode %d", id)
			}
		}
		// A truncate by a caller without CAP_FSETID clears S_ISUID/S_ISGID. The
		// kernel does not surface FATTR_KILL_SUIDGID here, so approximate CAP_FSETID
		// as uid 0 and clear the bits with the resize, in one transaction.
		mode := req.Mode
		if mode != nil {
			m := *mode & 0o7777
			// POSIX: an unprivileged chmod clears S_ISGID when the inode's group is
			// neither the caller's effective group nor a supplementary group (Linux
			// applies this to every inode type).
			if !isRoot && m&0o2000 != 0 && !inGroup(cred, in.GID) {
				m &^= 0o2000
			}
			mode = &m
		}
		if req.Size != nil && !isRoot && in.Mode&0o6000 != 0 {
			cleared := (in.Mode &^ 0o6000) & 0o7777
			if mode != nil {
				c := *mode &^ 0o6000
				cleared = c
			}
			mode = &cleared
		}
		r.addShared(id)
		if !final {
			return r, nil, nil
		}
		var recs []*record.Record
		var pc record.PermissionChange
		pc.InodeID = id
		hasPerm := false
		if mode != nil {
			pc.Mode = mode
			hasPerm = true
		}
		if req.UID != nil {
			pc.UID = req.UID
			hasPerm = true
		}
		if req.GID != nil {
			pc.GID = req.GID
			hasPerm = true
		}
		if hasPerm {
			recs = append(recs, mkRec(record.TypePermissionChange, 0, pc.Encode()))
			// A mode/owner change is a metadata change: ctime advances, mtime does not.
			recs = append(recs, ns.ctimeRec(id, ns.now())...)
		}
		if timeChange {
			sa := record.InodeSetattr{InodeID: id, Atime: req.Atime}
			if req.AtimeNow {
				t := ns.now()
				sa.Atime = &t
			}
			sa.Mtime = req.Mtime
			if req.MtimeNow {
				t := ns.now()
				sa.Mtime = &t
			}
			recs = append(recs, mkRec(record.TypeInodeSetattr, 0, sa.Encode()))
		}
		if req.Size != nil {
			if in.Kind == record.KindDir {
				return r, nil, orfserr.IsDir("cannot resize a directory")
			}
			recs = append(recs, mkRec(record.TypeFileResize, id, record.FileResize{NewSize: *req.Size}.Encode()))
			// A size change is a content mutation: it advances mtime/ctime unless an
			// explicit time was supplied in the same request (which already set them).
			if !timeChange {
				tm := ns.now()
				recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()))
			}
		}
		return r, recs, nil
	})
}

// Setxattr sets an extended attribute.
func (ns *Namespace) Setxattr(cred Credential, id uint64, name string, value []byte) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		in, err := viewGetattr(v, id)
		if err != nil {
			return r, nil, err
		}
		if !HasPermission(cred, in, 0o2) {
			return r, nil, orfserr.Access("write permission denied on inode %d", id)
		}
		if len(name) == 0 || len(name) > record.XattrNameMax {
			return r, nil, orfserr.Invalid("xattr name length %d out of range", len(name))
		}
		if len(value) > record.XattrValueMax {
			return r, nil, orfserr.Invalid("xattr value length %d exceeds %d", len(value), record.XattrValueMax)
		}
		r.addShared(id)
		if !final {
			return r, nil, nil
		}
		xs := record.XattrSet{InodeID: id, Name: []byte(name), Value: value}.Encode()
		return r, []*record.Record{mkRec(record.TypeXattrSet, 0, xs)}, nil
	})
}

// Removexattr removes an extended attribute.
func (ns *Namespace) Removexattr(cred Credential, id uint64, name string) error {
	return ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		in, err := viewGetattr(v, id)
		if err != nil {
			return r, nil, err
		}
		if !HasPermission(cred, in, 0o2) {
			return r, nil, orfserr.Access("write permission denied on inode %d", id)
		}
		r.addShared(id)
		if !final {
			return r, nil, nil
		}
		xr := record.XattrRemove{InodeID: id, Name: []byte(name)}.Encode()
		return r, []*record.Record{mkRec(record.TypeXattrRemove, 0, xr)}, nil
	})
}

// Getxattr reads an extended attribute. Reading requires read permission, and
// the inode is taken lock-free from the current immutable view.
func (ns *Namespace) Getxattr(cred Credential, id uint64, name string) ([]byte, error) {
	in, ok := ns.vol.View().Inode(id)
	if !ok {
		return nil, orfserr.NotFound("inode %d does not exist", id)
	}
	if !HasPermission(cred, in, 0o4) {
		return nil, orfserr.Access("read permission denied on inode %d", id)
	}
	v, ok := in.Xattrs[name]
	if !ok {
		return nil, orfserr.NotFound("xattr %q not set on inode %d", name, id)
	}
	return v, nil
}

// CreateFileData creates a regular file at (parent, name), writes data into it,
// and (when xattrName is non-empty) sets one extended attribute on the new
// inode — all in one authoritative transaction. It is the single-barrier form
// of Create + Write (+ Setxattr): the same records, committed atomically, so a
// single PUT costs one durability barrier instead of three while the committed
// state is byte-identical to the sequential form.
func (ns *Namespace) CreateFileData(cred Credential, parent uint64, name string, mode, uid, gid uint32, data []byte, xattrName string, xattrValue []byte) (*index.Inode, error) {
	var id uint64
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		if err := checkNameLen(name); err != nil {
			return r, nil, err
		}
		if _, err := viewDir(v, cred, parent, true); err != nil {
			return r, nil, err
		}
		if err := record.ValidateName([]byte(name)); err != nil {
			return r, nil, err
		}
		if _, ok := v.Lookup(parent, name); ok {
			return r, nil, orfserr.Exists("entry %q already exists in %d", name, parent)
		}
		if xattrName != "" {
			if len(xattrName) > record.XattrNameMax {
				return r, nil, orfserr.Invalid("xattr name length %d out of range", len(xattrName))
			}
			if len(xattrValue) > record.XattrValueMax {
				return r, nil, orfserr.Invalid("xattr value length %d exceeds %d", len(xattrValue), record.XattrValueMax)
			}
		}
		r.addName(parent, name)
		r.addShared(parent)
		if !final {
			return r, nil, nil
		}
		id = ns.allocID()
		tm := ns.now()
		ic := record.InodeCreate{
			InodeID: id, Generation: 1, Kind: record.KindFile,
			Mode: mode & 0o7777, UID: uid, GID: gid, Nlink: 1,
			Atime: tm, Mtime: tm, Ctime: tm,
		}
		recs := []*record.Record{
			mkRec(record.TypeInodeCreate, 0, ic.Encode()),
			mkRec(record.TypeDentryAdd, 0, record.EncodeDentryAdd(record.DentryAdd{Parent: parent, Child: id, Name: []byte(name)})),
		}
		if len(data) > 0 {
			recs = append(recs, mkRec(record.TypeDataExtent, id, record.DataExtent{Offset: 0, Data: data}.Encode()))
			recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()))
		}
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()))
		if xattrName != "" {
			recs = append(recs, mkRec(record.TypeXattrSet, 0, record.XattrSet{InodeID: id, Name: []byte(xattrName), Value: xattrValue}.Encode()))
		}
		return r, recs, nil
	})
	if err != nil {
		return nil, err
	}
	return ns.inodeAfter(id)
}

// ReplaceFileData replaces an existing regular file's contents with data and
// (when xattrName is non-empty) sets one extended attribute, in one
// authoritative transaction: the single-barrier form of Truncate(0) + Write (+
// Setxattr).
func (ns *Namespace) ReplaceFileData(cred Credential, id uint64, data []byte, xattrName string, xattrValue []byte) (*index.Inode, error) {
	err := ns.publishStaged(func(v *view.View, final bool) (reservation, []*record.Record, error) {
		var r reservation
		in, err := viewGetattr(v, id)
		if err != nil {
			return r, nil, err
		}
		if in.Kind == record.KindDir {
			return r, nil, orfserr.IsDir("inode %d is a directory", id)
		}
		if !HasPermission(cred, in, 0o2) {
			return r, nil, orfserr.Access("write permission denied on inode %d", id)
		}
		if xattrName != "" {
			if len(xattrName) > record.XattrNameMax {
				return r, nil, orfserr.Invalid("xattr name length %d out of range", len(xattrName))
			}
			if len(xattrValue) > record.XattrValueMax {
				return r, nil, orfserr.Invalid("xattr value length %d exceeds %d", len(xattrValue), record.XattrValueMax)
			}
		}
		r.addShared(id)
		if !final {
			return r, nil, nil
		}
		tm := ns.now()
		recs := []*record.Record{
			mkRec(record.TypeFileResize, id, record.FileResize{NewSize: 0}.Encode()),
		}
		if len(data) > 0 {
			recs = append(recs, mkRec(record.TypeDataExtent, id, record.DataExtent{Offset: 0, Data: data}.Encode()))
		}
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()))
		if xattrName != "" {
			recs = append(recs, mkRec(record.TypeXattrSet, 0, record.XattrSet{InodeID: id, Name: []byte(xattrName), Value: xattrValue}.Encode()))
		}
		return r, recs, nil
	})
	if err != nil {
		return nil, err
	}
	return ns.inodeAfter(id)
}

// checkSticky enforces the sticky-bit rule on unlink/rmdir: when the parent has
// the sticky bit, only the owner of the entry, the owner of the directory, or
// root may remove it.
func (ns *Namespace) checkSticky(cred Credential, parent, child *index.Inode) error {
	if parent.Mode&0o1000 == 0 {
		return nil
	}
	if cred.UID == 0 || cred.UID == parent.UID || cred.UID == child.UID {
		return nil
	}
	return orfserr.Permission("sticky directory: not the owner of the entry or the directory")
}

// isSelfOrDescendantView reports whether candidate is dir or lies beneath it in
// a committed view.
func isSelfOrDescendantView(v *view.View, candidate, dir uint64) bool {
	if candidate == dir {
		return true
	}
	cur := candidate
	for cur != record.RootInode {
		p, ok := v.ParentOf(cur)
		if !ok {
			return false
		}
		if p == dir {
			return true
		}
		cur = p
	}
	return false
}
