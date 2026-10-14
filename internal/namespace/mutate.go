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
	if _, err := ns.dir(cred, parent, true); err != nil {
		return nil, err
	}
	if err := record.ValidateName([]byte(name)); err != nil {
		return nil, err
	}
	if len(target) == 0 || len(target) > record.SymlinkTargetMax {
		return nil, orfserr.Invalid("symlink target length %d out of range", len(target))
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
		return 0, orfserr.Permission("write permission denied on inode %d", id)
	}
	if len(data) == 0 {
		return in.Size, nil
	}
	de := record.DataExtent{Offset: offset, Data: data}.Encode()
	if _, err := ns.vol.Publish([]*record.Record{mkRec(record.TypeDataExtent, id, de)}); err != nil {
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
		return orfserr.Permission("write permission denied on inode %d", id)
	}
	fr := record.FileResize{NewSize: size}.Encode()
	_, err = ns.vol.Publish([]*record.Record{mkRec(record.TypeFileResize, id, fr)})
	return err
}

// Unlink removes a name from a directory. The inode and its bytes are retained.
func (ns *Namespace) Unlink(cred Credential, parent uint64, name string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
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
	recs = append(recs, mkRec(record.TypeInodeSetattr, 0, record.InodeSetattr{InodeID: newParent, Mtime: &tm}.Encode()))
	_, err := ns.vol.Publish(recs)
	return err
}

// Link adds a hard link. Directories cannot be hard-linked (EPERM).
func (ns *Namespace) Link(cred Credential, parent uint64, name string, target uint64) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
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

// SetattrRequest is a partial attribute change.
type SetattrRequest struct {
	Mode  *uint32
	UID   *uint32
	GID   *uint32
	Atime *record.Timespec
	Mtime *record.Timespec
	Size  *uint64
}

// Setattr applies a partial attribute change.
func (ns *Namespace) Setattr(cred Credential, id uint64, req SetattrRequest) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	in, err := ns.getattr(id)
	if err != nil {
		return err
	}
	// Permission: chmod/chown require ownership (or root); timestamp and size
	// changes require write permission (or ownership).
	if req.Mode != nil || req.UID != nil || req.GID != nil {
		if !IsOwner(cred, in) {
			return orfserr.Permission("only the owner may change mode or ownership")
		}
	}
	if req.Atime != nil || req.Mtime != nil || req.Size != nil {
		if !HasPermission(cred, in, 0o2) {
			return orfserr.Permission("write permission denied on inode %d", id)
		}
	}
	var recs []*record.Record
	var pc record.PermissionChange
	pc.InodeID = id
	hasPerm := false
	if req.Mode != nil {
		m := *req.Mode & 0o7777
		pc.Mode = &m
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
	if req.Atime != nil || req.Mtime != nil {
		sa := record.InodeSetattr{InodeID: id, Atime: req.Atime}
		if req.Mtime != nil {
			sa.Mtime = req.Mtime
		} else if req.Atime != nil {
			// setting only atime still needs a ctime bump; use atime as mtime is
			// not implied, so publish with the current mtime.
			m := in.Mtime
			sa.Mtime = &m
		}
		recs = append(recs, mkRec(record.TypeInodeSetattr, 0, sa.Encode()))
	}
	if req.Size != nil {
		if in.Kind == record.KindDir {
			return orfserr.IsDir("cannot resize a directory")
		}
		recs = append(recs, mkRec(record.TypeFileResize, id, record.FileResize{NewSize: *req.Size}.Encode()))
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
		return orfserr.Permission("write permission denied on inode %d", id)
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
		return orfserr.Permission("write permission denied on inode %d", id)
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
		return nil, orfserr.Permission("read permission denied on inode %d", id)
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
