// Package index reconstructs ordinary filesystem semantics from the
// authoritative log: inodes, directories, extents, and snapshots.
//
// The index is a pure function of the committed byte prefix [0, R): replaying
// the log from scratch yields byte-for-byte the same namespace a live handle
// holds. Nothing here ever mutates the log; mutations are expressed as new
// records.
package index

import (
	"sort"

	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// Inode is a reconstructed filesystem object.
type Inode struct {
	ID      uint64
	Kind    uint8
	Mode    uint32 // permission bits
	UID     uint32
	GID     uint32
	Nlink   uint32
	Size    uint64
	Atime   record.Timespec
	Mtime   record.Timespec
	Ctime   record.Timespec
	Xattrs  map[string][]byte
	Extents *ExtentMap
	Target  []byte // symlink target
	Rdev    uint64 // device number for a char/block special file

	Generation         uint64
	Version            uint64
	LatestRecordOffset uint64
	CreationRoot       uint64
	Live               bool
}

// DentryKey identifies a directory entry by parent inode and name.
type DentryKey struct {
	Parent uint64
	Name   string
}

// DirEntry is a resolved directory member.
type DirEntry struct {
	Name  string
	Child uint64
}

// Index is the reconstructed namespace at a committed root.
type Index struct {
	Root             uint64
	Inodes           map[uint64]*Inode
	Dentry           map[DentryKey]uint64
	Snapshots        map[string]uint64
	Checkpoints      []Checkpoint
	NextInodeID      uint64
	CommittedRecords uint64
	MaxTxID          uint64
}

// Checkpoint records an INDEX_CHECKPOINT summary.
type Checkpoint struct {
	Root        uint64
	InodeCount  uint64
	DentryCount uint64
	Digest      [32]byte
}

// New returns an empty index.
func New() *Index {
	return &Index{
		Inodes:      make(map[uint64]*Inode),
		Dentry:      make(map[DentryKey]uint64),
		Snapshots:   make(map[string]uint64),
		NextInodeID: record.RootInode,
	}
}

// Replay rebuilds the index by replaying the committed prefix [0, end) of the
// log at dir.
func Replay(dir string, extents []segment.Extent, end uint64) (*Index, error) {
	idx := New()
	if end == 0 {
		return nil, orfserr.Corrupt("replaying an empty log")
	}
	err := log.ReadRecords(dir, extents, end, func(rec *record.Record, offset uint64, raw []byte) error {
		idx.CommittedRecords++
		return idx.Apply(rec, offset)
	})
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// Inode returns an inode by id.
func (idx *Index) Inode(id uint64) (*Inode, bool) {
	in, ok := idx.Inodes[id]
	return in, ok
}

// Lookup resolves a directory entry.
func (idx *Index) Lookup(parent uint64, name string) (*Inode, bool) {
	child, ok := idx.Dentry[DentryKey{Parent: parent, Name: name}]
	if !ok {
		return nil, false
	}
	in, ok := idx.Inodes[child]
	return in, ok
}

// Children lists a directory's members sorted by name (byte order).
func (idx *Index) Children(parent uint64) []DirEntry {
	var out []DirEntry
	for k, child := range idx.Dentry {
		if k.Parent == parent {
			out = append(out, DirEntry{Name: k.Name, Child: child})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ParentOf finds a directory's unique parent.
func (idx *Index) ParentOf(child uint64) (uint64, bool) {
	for k, c := range idx.Dentry {
		if c == child {
			return k.Parent, true
		}
	}
	return 0, false
}

func total(rec *record.Record) uint64 {
	t, _ := rec.Header.TotalLen()
	return t
}

func (idx *Index) create(in *Inode, offset uint64) {
	in.CreationRoot = offset
	in.LatestRecordOffset = offset
	in.Version = 1
	in.Live = true
	if in.Xattrs == nil {
		in.Xattrs = make(map[string][]byte)
	}
	if in.Extents == nil {
		in.Extents = NewExtentMap()
	}
	idx.Inodes[in.ID] = in
	if in.ID+1 > idx.NextInodeID {
		idx.NextInodeID = in.ID + 1
	}
}

func (idx *Index) bump(in *Inode, offset uint64) {
	in.Version++
	in.LatestRecordOffset = offset
}

// Apply replays one authoritative record into the index.
func (idx *Index) Apply(rec *record.Record, offset uint64) error {
	totalLen := total(rec)
	h := rec.Header
	switch h.RecordType {
	case record.TypeVolumeGenesis:
		root := &Inode{
			ID: record.RootInode, Kind: record.KindDir, Mode: 0o755,
			Nlink: 2, Extents: NewExtentMap(), Xattrs: make(map[string][]byte),
		}
		idx.create(root, 0)
		idx.Root = offset + totalLen
		idx.NextInodeID = record.RootInode + 1
		return nil

	case record.TypeTxBegin, record.TypeTxAbort:
		return nil

	case record.TypeTxCommit:
		idx.Root = offset + totalLen
		if h.TransactionID > idx.MaxTxID {
			idx.MaxTxID = h.TransactionID
		}
		return nil

	case record.TypeInodeCreate:
		v, err := record.DecodeInodeCreate(rec.Payload)
		if err != nil {
			return err
		}
		in := &Inode{
			ID: v.InodeID, Generation: v.Generation, Kind: v.Kind, Mode: v.Mode & 0o7777,
			UID: v.UID, GID: v.GID, Nlink: v.Nlink, Size: v.Size,
			Atime: v.Atime, Mtime: v.Mtime, Ctime: v.Ctime,
		}
		idx.create(in, offset)
		in.Extents.Resize(v.Size)
		return nil

	case record.TypeSymlinkCreate:
		v, err := record.DecodeSymlinkCreate(rec.Payload)
		if err != nil {
			return err
		}
		in := &Inode{
			ID: v.InodeID, Generation: v.Generation, Kind: record.KindSymlink, Mode: v.Mode & 0o7777,
			UID: v.UID, GID: v.GID, Nlink: 1, Size: uint64(len(v.Target)),
			Mtime: v.Mtime, Ctime: v.Mtime, Target: append([]byte(nil), v.Target...),
		}
		idx.create(in, offset)
		return nil

	case record.TypeMknodCreate:
		v, err := record.DecodeMknodCreate(rec.Payload)
		if err != nil {
			return err
		}
		in := &Inode{
			ID: v.InodeID, Generation: v.Generation, Kind: v.Kind, Mode: v.Mode & 0o7777,
			UID: v.UID, GID: v.GID, Nlink: v.Nlink, Rdev: v.Rdev,
			Atime: v.Atime, Mtime: v.Mtime, Ctime: v.Ctime,
		}
		idx.create(in, offset)
		return nil

	case record.TypeCtimeUpdate:
		v, err := record.DecodeCtimeUpdate(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[v.InodeID]
		if !ok {
			return orfserr.Corrupt("CTIME_UPDATE for unknown inode %d", v.InodeID)
		}
		in.Ctime = v.Ctime
		idx.bump(in, offset)
		return nil

	case record.TypeInodeMetadata:
		v, err := record.DecodeInodeMetadata(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[v.InodeID]
		if !ok {
			return orfserr.Corrupt("INODE_METADATA for unknown inode %d", v.InodeID)
		}
		in.Nlink = v.Nlink
		idx.bump(in, offset)
		return nil

	case record.TypePermissionChange:
		v, err := record.DecodePermissionChange(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[v.InodeID]
		if !ok {
			return orfserr.Corrupt("PERMISSION_CHANGE for unknown inode %d", v.InodeID)
		}
		if v.Mode != nil {
			in.Mode = *v.Mode & 0o7777
		}
		if v.UID != nil {
			in.UID = *v.UID
		}
		if v.GID != nil {
			in.GID = *v.GID
		}
		idx.bump(in, offset)
		return nil

	case record.TypeInodeSetattr:
		v, err := record.DecodeInodeSetattr(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[v.InodeID]
		if !ok {
			return orfserr.Corrupt("INODE_SETATTR for unknown inode %d", v.InodeID)
		}
		if v.Atime != nil {
			in.Atime = *v.Atime
		}
		if v.Mtime != nil {
			in.Mtime = *v.Mtime
			in.Ctime = *v.Mtime // ctime is derived, never carried
		}
		idx.bump(in, offset)
		return nil

	case record.TypeXattrSet:
		v, err := record.DecodeXattrSet(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[v.InodeID]
		if !ok {
			return orfserr.Corrupt("XATTR_SET for unknown inode %d", v.InodeID)
		}
		in.Xattrs[string(v.Name)] = append([]byte(nil), v.Value...)
		idx.bump(in, offset)
		return nil

	case record.TypeXattrRemove:
		v, err := record.DecodeXattrRemove(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[v.InodeID]
		if !ok {
			return orfserr.Corrupt("XATTR_REMOVE for unknown inode %d", v.InodeID)
		}
		delete(in.Xattrs, string(v.Name))
		idx.bump(in, offset)
		return nil

	case record.TypeDentryAdd:
		v, err := record.DecodeDentryAdd(rec.Payload)
		if err != nil {
			return err
		}
		idx.Dentry[DentryKey{Parent: v.Parent, Name: string(v.Name)}] = v.Child
		if in, ok := idx.Inodes[v.Child]; ok {
			idx.bump(in, offset)
		}
		return nil

	case record.TypeHardlinkAdd:
		v, err := record.DecodeHardlinkAdd(rec.Payload)
		if err != nil {
			return err
		}
		idx.Dentry[DentryKey{Parent: v.Parent, Name: string(v.Name)}] = v.Child
		if in, ok := idx.Inodes[v.Child]; ok {
			idx.bump(in, offset)
		}
		return nil

	case record.TypeDentryRemove:
		v, err := record.DecodeDentryRemove(rec.Payload)
		if err != nil {
			return err
		}
		if child, ok := idx.Dentry[DentryKey{Parent: v.Parent, Name: string(v.Name)}]; ok {
			delete(idx.Dentry, DentryKey{Parent: v.Parent, Name: string(v.Name)})
			if in, ok := idx.Inodes[child]; ok {
				idx.bump(in, offset)
			}
		}
		return nil

	case record.TypeHardlinkRemove:
		v, err := record.DecodeHardlinkRemove(rec.Payload)
		if err != nil {
			return err
		}
		if child, ok := idx.Dentry[DentryKey{Parent: v.Parent, Name: string(v.Name)}]; ok {
			delete(idx.Dentry, DentryKey{Parent: v.Parent, Name: string(v.Name)})
			if in, ok := idx.Inodes[child]; ok {
				idx.bump(in, offset)
			}
		}
		return nil

	case record.TypeDentryRename:
		v, err := record.DecodeDentryRename(rec.Payload)
		if err != nil {
			return err
		}
		oldKey := DentryKey{Parent: v.OldParent, Name: string(v.OldName)}
		newKey := DentryKey{Parent: v.NewParent, Name: string(v.NewName)}
		child, ok := idx.Dentry[oldKey]
		if !ok {
			return orfserr.Corrupt("DENTRY_RENAME of unknown entry")
		}
		if v.Flags == record.RenameExchange {
			other, ok := idx.Dentry[newKey]
			if !ok {
				return orfserr.Corrupt("DENTRY_RENAME exchange of unknown entry")
			}
			idx.Dentry[oldKey] = other
			idx.Dentry[newKey] = child
			if in, ok := idx.Inodes[other]; ok {
				idx.bump(in, offset)
			}
		} else {
			delete(idx.Dentry, oldKey)
			idx.Dentry[newKey] = child
		}
		if in, ok := idx.Inodes[child]; ok {
			idx.bump(in, offset)
		}
		return nil

	case record.TypeDataExtent:
		v, err := record.DecodeDataExtent(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[h.InodeID]
		if !ok {
			return orfserr.Corrupt("DATA_EXTENT for unknown inode %d", h.InodeID)
		}
		dataLogOffset := offset + record.HeaderLen + record.DataExtentDataOffset
		in.Extents.Write(v.Offset, dataLogOffset, uint64(len(v.Data)))
		in.Size = in.Extents.Size()
		idx.bump(in, offset)
		return nil

	case record.TypeDataZeroRange:
		v, err := record.DecodeDataZeroRange(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[h.InodeID]
		if !ok {
			return orfserr.Corrupt("DATA_ZERO_RANGE for unknown inode %d", h.InodeID)
		}
		in.Extents.Zero(v.Offset, v.Length)
		in.Size = in.Extents.Size()
		idx.bump(in, offset)
		return nil

	case record.TypeFileResize:
		v, err := record.DecodeFileResize(rec.Payload)
		if err != nil {
			return err
		}
		in, ok := idx.Inodes[h.InodeID]
		if !ok {
			return orfserr.Corrupt("FILE_RESIZE for unknown inode %d", h.InodeID)
		}
		in.Extents.Resize(v.NewSize)
		in.Size = in.Extents.Size()
		idx.bump(in, offset)
		return nil

	case record.TypeSnapshotRoot:
		v, err := record.DecodeSnapshotRoot(rec.Payload)
		if err != nil {
			return err
		}
		idx.Snapshots[string(v.Name)] = v.Root
		return nil

	case record.TypeIndexCheckpoint:
		v, err := record.DecodeIndexCheckpoint(rec.Payload)
		if err != nil {
			return err
		}
		idx.Checkpoints = append(idx.Checkpoints, Checkpoint{
			Root: v.Root, InodeCount: v.InodeCount, DentryCount: v.DentryCount, Digest: v.Digest,
		})
		return nil

	default:
		// Unknown optional extension: already validated as skippable.
		return nil
	}
}
