package namespace

import (
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// Create creates a regular file.
func (ns *Namespace) Create(cred Credential, parent uint64, name string, mode, uid, gid uint32) (*index.Inode, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(name); err != nil {
		return nil, err
	}
	if _, err := ns.dir(cred, parent, true); err != nil {
		return nil, err
	}
	if err := record.ValidateName([]byte(name)); err != nil {
		return nil, err
	}
	if _, ok := ns.idx().Lookup(parent, name); ok {
		return nil, orfserr.Exists("entry %q already exists in %d", name, parent)
	}
	id := ns.idx().NextInodeID
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
	if _, err := ns.vol.Publish(recs); err != nil {
		return nil, err
	}
	in, _ := ns.idx().Inode(id)
	return in, nil
}

// Mkdir creates a directory.
func (ns *Namespace) Mkdir(cred Credential, parent uint64, name string, mode, uid, gid uint32) (*index.Inode, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(name); err != nil {
		return nil, err
	}
	p, err := ns.dir(cred, parent, true)
	if err != nil {
		return nil, err
	}
	if err := record.ValidateName([]byte(name)); err != nil {
		return nil, err
	}
	if _, ok := ns.idx().Lookup(parent, name); ok {
		return nil, orfserr.Exists("entry %q already exists in %d", name, parent)
	}
	id := ns.idx().NextInodeID
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
	if _, err := ns.vol.Publish(recs); err != nil {
		return nil, err
	}
	in, _ := ns.idx().Inode(id)
	return in, nil
}

// Symlink creates a symbolic link.
func (ns *Namespace) Symlink(cred Credential, parent uint64, name string, target string, uid, gid uint32) (*index.Inode, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(name); err != nil {
		return nil, err
	}
	if _, err := ns.dir(cred, parent, true); err != nil {
		return nil, err
	}
	if err := record.ValidateName([]byte(name)); err != nil {
		return nil, err
	}
	if len(target) == 0 || len(target) > record.SymlinkTargetMax {
		return nil, orfserr.NameTooLong("symlink target length %d out of range", len(target))
	}
	if _, ok := ns.idx().Lookup(parent, name); ok {
		return nil, orfserr.Exists("entry %q already exists in %d", name, parent)
	}
	id := ns.idx().NextInodeID
	tm := ns.now()
	sc := record.SymlinkCreate{
		InodeID: id, Generation: 1, Mode: 0o777, UID: uid, GID: gid, Mtime: tm, Target: []byte(target),
	}
	recs := []*record.Record{
		mkRec(record.TypeSymlinkCreate, 0, sc.Encode()),
		mkRec(record.TypeDentryAdd, 0, record.EncodeDentryAdd(record.DentryAdd{Parent: parent, Child: id, Name: []byte(name)})),
		mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
	}
	if _, err := ns.vol.Publish(recs); err != nil {
		return nil, err
	}
	in, _ := ns.idx().Inode(id)
	return in, nil
}

// Write writes bytes at a logical offset and returns the new size.
func (ns *Namespace) Write(cred Credential, id, offset uint64, data []byte) (uint64, error) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	in, err := ns.getattr(id)
	if err != nil {
		return 0, err
	}
	if in.Kind == record.KindDir {
		return 0, orfserr.IsDir("inode %d is a directory", id)
	}
	if !HasPermission(cred, in, 0o2) {
		return 0, orfserr.Access("write permission denied on inode %d", id)
	}
	if len(data) == 0 {
		return in.Size, nil
	}
	de := record.DataExtent{Offset: offset, Data: data}.Encode()
	tm := ns.now()
	// A write is a content mutation: it advances the file's mtime and ctime in
	// the same authoritative transaction (docs/FUSE-SEMANTICS.md §5.1).
	recs := []*record.Record{
		mkRec(record.TypeDataExtent, id, de),
		mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()),
	}
	if _, err := ns.vol.Publish(recs); err != nil {
		return 0, err
	}
	in2, _ := ns.idx().Inode(id)
	return in2.Size, nil
}

// Truncate resizes a file. Shrinking hides bytes but retains them; growing
// zero-fills, so truncated bytes are never resurrected.
func (ns *Namespace) Truncate(cred Credential, id, size uint64) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	in, err := ns.getattr(id)
	if err != nil {
		return err
	}
	if in.Kind == record.KindDir {
		return orfserr.IsDir("inode %d is a directory", id)
	}
	if !HasPermission(cred, in, 0o2) {
		return orfserr.Access("write permission denied on inode %d", id)
	}
	fr := record.FileResize{NewSize: size}.Encode()
	tm := ns.now()
	// A truncate is a content mutation: it advances mtime and ctime (POSIX).
	recs := []*record.Record{
		mkRec(record.TypeFileResize, id, fr),
		mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()),
	}
	_, err = ns.vol.Publish(recs)
	return err
}

// Unlink removes a name from a directory. The inode and its bytes are retained.
func (ns *Namespace) Unlink(cred Credential, parent uint64, name string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(name); err != nil {
		return err
	}
	p, err := ns.dir(cred, parent, true)
	if err != nil {
		return err
	}
	child, ok := ns.idx().Lookup(parent, name)
	if !ok {
		return orfserr.NotFound("no entry %q in %d", name, parent)
	}
	if child.Kind == record.KindDir {
		return orfserr.IsDir("use rmdir for directory %q", name)
	}
	if err := ns.checkSticky(cred, p, child); err != nil {
		return err
	}
	remaining := uint32(ns.namesOf(child.ID) - 1)
	rem := record.DentryRemove{Parent: parent, Name: []byte(name)}
	var recs []*record.Record
	if remaining > 0 {
		recs = append(recs, mkRec(record.TypeHardlinkRemove, 0, record.EncodeHardlinkRemove(rem)))
	} else {
		recs = append(recs, mkRec(record.TypeDentryRemove, 0, record.EncodeDentryRemove(rem)))
	}
	recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: child.ID, Nlink: remaining}.Encode()))
	tm := ns.now()
	recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()))
	_, err = ns.vol.Publish(recs)
	return err
}

// Rmdir removes an empty directory.
func (ns *Namespace) Rmdir(cred Credential, parent uint64, name string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(name); err != nil {
		return err
	}
	p, err := ns.dir(cred, parent, true)
	if err != nil {
		return err
	}
	child, ok := ns.idx().Lookup(parent, name)
	if !ok {
		return orfserr.NotFound("no entry %q in %d", name, parent)
	}
	if child.Kind != record.KindDir {
		return orfserr.NotDir("%q is not a directory", name)
	}
	if child.ID == record.RootInode {
		return orfserr.Permission("cannot remove the root directory")
	}
	if len(ns.idx().Children(child.ID)) > 0 {
		return orfserr.NotEmpty("directory %q is not empty", name)
	}
	if err := ns.checkSticky(cred, p, child); err != nil {
		return err
	}
	tm := ns.now()
	recs := []*record.Record{
		mkRec(record.TypeDentryRemove, 0, record.EncodeDentryRemove(record.DentryRemove{Parent: parent, Name: []byte(name)})),
		mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: child.ID, Nlink: 0}.Encode()),
		mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: parent, Nlink: p.Nlink - 1}.Encode()),
		mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: parent, Mtime: &tm}.Encode()),
	}
	_, err = ns.vol.Publish(recs)
	return err
}

// Rename atomically moves or exchanges a directory entry. Identity (the inode
// id) is preserved.
func (ns *Namespace) Rename(cred Credential, oldParent uint64, oldName string, newParent uint64, newName string, exchange bool) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(oldName); err != nil {
		return err
	}
	if err := checkNameLen(newName); err != nil {
		return err
	}
	if _, err := ns.dir(cred, oldParent, true); err != nil {
		return err
	}
	if _, err := ns.dir(cred, newParent, true); err != nil {
		return err
	}
	child, ok := ns.idx().Lookup(oldParent, oldName)
	if !ok {
		return orfserr.NotFound("no entry %q in %d", oldName, oldParent)
	}
	if child.Kind == record.KindDir && exchange {
		// Exchange of a directory with a non-directory is rejected below by the
		// type check; cycles are rejected regardless.
	}
	if child.Kind == record.KindDir && ns.isSelfOrDescendant(newParent, child.ID) {
		return orfserr.Invalid("rename would create a directory cycle")
	}

	if exchange {
		other, ok := ns.idx().Lookup(newParent, newName)
		if !ok {
			return orfserr.NotFound("no entry %q in %d", newName, newParent)
		}
		if (child.Kind == record.KindDir) != (other.Kind == record.KindDir) {
			return orfserr.Invalid("exchange requires matching types")
		}
		dr := record.DentryRename{Flags: record.RenameExchange, OldParent: oldParent, NewParent: newParent, OldName: []byte(oldName), NewName: []byte(newName)}
		_, err := ns.vol.Publish([]*record.Record{mkRec(record.TypeDentryRename, 0, dr.Encode())})
		return err
	}

	var recs []*record.Record
	if dest, ok := ns.idx().Lookup(newParent, newName); ok {
		if dest.ID == child.ID {
			return nil // same file, no-op
		}
		if dest.Kind == record.KindDir && len(ns.idx().Children(dest.ID)) > 0 {
			return orfserr.NotEmpty("destination directory %q is not empty", newName)
		}
		if child.Kind == record.KindDir && dest.Kind != record.KindDir {
			return orfserr.IsDir("cannot overwrite a file with a directory")
		}
		if child.Kind != record.KindDir && dest.Kind == record.KindDir {
			return orfserr.IsDir("cannot overwrite a directory with a file")
		}
		// Overwrite is a preceding removal in the same transaction.
		recs = append(recs, mkRec(record.TypeDentryRemove, 0, record.EncodeDentryRemove(record.DentryRemove{Parent: newParent, Name: []byte(newName)})))
		recs = append(recs, mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: dest.ID, Nlink: 0}.Encode()))
	}
	dr := record.DentryRename{Flags: record.RenameMove, OldParent: oldParent, NewParent: newParent, OldName: []byte(oldName), NewName: []byte(newName)}
	recs = append(recs, mkRec(record.TypeDentryRename, 0, dr.Encode()))
	tm := ns.now()
	// A rename marks both containing directories' mtime and ctime for update.
	recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: oldParent, Mtime: &tm}.Encode()))
	if newParent != oldParent {
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: newParent, Mtime: &tm}.Encode()))
	}
	_, err := ns.vol.Publish(recs)
	return err
}

// Link adds a hard link. Directories cannot be hard-linked (EPERM).
func (ns *Namespace) Link(cred Credential, parent uint64, name string, target uint64) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if err := checkNameLen(name); err != nil {
		return err
	}
	if _, err := ns.dir(cred, parent, true); err != nil {
		return err
	}
	targetIn, err := ns.getattr(target)
	if err != nil {
		return err
	}
	if targetIn.Kind == record.KindDir {
		return orfserr.Permission("hard-linking a directory is not permitted")
	}
	if _, ok := ns.idx().Lookup(parent, name); ok {
		return orfserr.Exists("entry %q already exists in %d", name, parent)
	}
	recs := []*record.Record{
		mkRec(record.TypeHardlinkAdd, 0, record.EncodeHardlinkAdd(record.DentryAdd{Parent: parent, Child: target, Name: []byte(name)})),
		mkRec(record.TypeInodeMetadata, 0, record.InodeMetadata{InodeID: target, Nlink: targetIn.Nlink + 1}.Encode()),
	}
	_, err = ns.vol.Publish(recs)
	return err
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
	ns.mu.Lock()
	defer ns.mu.Unlock()
	in, err := ns.getattr(id)
	if err != nil {
		return err
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
				return orfserr.Access("timestamps require ownership or write permission")
			}
		case (metaChange || timeChange) && !ownerOrRoot:
			return orfserr.Permission("only the owner or root may change mode, ownership or an explicit timestamp")
		}
		if req.UID != nil && *req.UID != in.UID && !isRoot {
			return orfserr.Permission("changing the owner requires root")
		}
		if req.GID != nil && *req.GID != in.GID && !isRoot {
			if !ownerOrRoot || !inGroup(cred, *req.GID) {
				return orfserr.Permission("changing the group requires root or membership in the target group")
			}
		}
		if req.Size != nil && !req.WritableHandle && !HasPermission(cred, in, 0o2) {
			return orfserr.Access("write permission denied on inode %d", id)
		}
	}
	// A truncate by a caller without CAP_FSETID clears S_ISUID/S_ISGID. The
	// kernel does not surface FATTR_KILL_SUIDGID here, so approximate CAP_FSETID
	// as uid 0 and clear the bits with the resize, in one transaction.
	mode := req.Mode
	if mode != nil {
		m := *mode & 0o7777
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
	}
	if hasPerm {
		recs = append(recs, mkRec(record.TypePermissionChange, 0, pc.Encode()))
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
			return orfserr.IsDir("cannot resize a directory")
		}
		recs = append(recs, mkRec(record.TypeFileResize, id, record.FileResize{NewSize: *req.Size}.Encode()))
		// A size change is a content mutation: it advances mtime/ctime unless an
		// explicit time was supplied in the same request (which already set them).
		if !timeChange {
			tm := ns.now()
			recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: id, Mtime: &tm}.Encode()))
		}
	}
	if len(recs) == 0 {
		return nil
	}
	_, err = ns.vol.Publish(recs)
	return err
}

// Setxattr sets an extended attribute.
func (ns *Namespace) Setxattr(cred Credential, id uint64, name string, value []byte) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	in, err := ns.getattr(id)
	if err != nil {
		return err
	}
	if !HasPermission(cred, in, 0o2) {
		return orfserr.Access("write permission denied on inode %d", id)
	}
	if len(name) == 0 || len(name) > record.XattrNameMax {
		return orfserr.Invalid("xattr name length %d out of range", len(name))
	}
	if len(value) > record.XattrValueMax {
		return orfserr.Invalid("xattr value length %d exceeds %d", len(value), record.XattrValueMax)
	}
	xs := record.XattrSet{InodeID: id, Name: []byte(name), Value: value}.Encode()
	_, err = ns.vol.Publish([]*record.Record{mkRec(record.TypeXattrSet, 0, xs)})
	return err
}

// Removexattr removes an extended attribute.
func (ns *Namespace) Removexattr(cred Credential, id uint64, name string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	in, err := ns.getattr(id)
	if err != nil {
		return err
	}
	if !HasPermission(cred, in, 0o2) {
		return orfserr.Access("write permission denied on inode %d", id)
	}
	xr := record.XattrRemove{InodeID: id, Name: []byte(name)}.Encode()
	_, err = ns.vol.Publish([]*record.Record{mkRec(record.TypeXattrRemove, 0, xr)})
	return err
}

// Getxattr reads an extended attribute. Reading requires read permission.
func (ns *Namespace) Getxattr(cred Credential, id uint64, name string) ([]byte, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	in, err := ns.getattr(id)
	if err != nil {
		return nil, err
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

// isSelfOrDescendant reports whether candidate is dir or lies beneath it.
func (ns *Namespace) isSelfOrDescendant(candidate, dir uint64) bool {
	if candidate == dir {
		return true
	}
	cur := candidate
	for cur != record.RootInode {
		p, ok := ns.idx().ParentOf(cur)
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
