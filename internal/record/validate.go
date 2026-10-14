package record

import "github.com/infinityabundance/orfs-go/internal/orfserr"

// subject resolves the subject inode of a record using the shared rule: use the
// payload id when non-zero, else the header id; the result must be non-zero, and
// if both are non-zero and differ the record is corrupt.
func subject(headerInode, payloadInode uint64) (uint64, error) {
	var id uint64
	switch {
	case payloadInode != 0:
		id = payloadInode
	case headerInode != 0:
		id = headerInode
	default:
		return 0, orfserr.Corrupt("record has no subject inode")
	}
	if headerInode != 0 && payloadInode != 0 && headerInode != payloadInode {
		return 0, orfserr.Corrupt("payload inode id %d disagrees with record header %d", payloadInode, headerInode)
	}
	return id, nil
}

// ValidatePayload decodes a committed data record's payload and rejects
// out-of-domain values, so a malformed committed record is refused rather than
// indexed. Structural records are validated by the transaction machine.
func ValidatePayload(rec *Record) error {
	h := rec.Header
	p := rec.Payload
	switch h.RecordType {
	case TypeInodeCreate:
		v, err := DecodeInodeCreate(p)
		if err != nil {
			return err
		}
		if v.InodeID <= RootInode {
			return orfserr.Corrupt("INODE_CREATE reserved inode id %d", v.InodeID)
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeSymlinkCreate:
		v, err := DecodeSymlinkCreate(p)
		if err != nil {
			return err
		}
		if v.InodeID <= RootInode {
			return orfserr.Corrupt("SYMLINK_CREATE reserved inode id %d", v.InodeID)
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeMknodCreate:
		v, err := DecodeMknodCreate(p)
		if err != nil {
			return err
		}
		if v.InodeID <= RootInode {
			return orfserr.Corrupt("MKNOD_CREATE reserved inode id %d", v.InodeID)
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeCtimeUpdate:
		v, err := DecodeCtimeUpdate(p)
		if err != nil {
			return err
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeInodeMetadata:
		v, err := DecodeInodeMetadata(p)
		if err != nil {
			return err
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypePermissionChange:
		v, err := DecodePermissionChange(p)
		if err != nil {
			return err
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeInodeSetattr:
		v, err := DecodeInodeSetattr(p)
		if err != nil {
			return err
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeXattrSet:
		v, err := DecodeXattrSet(p)
		if err != nil {
			return err
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeXattrRemove:
		v, err := DecodeXattrRemove(p)
		if err != nil {
			return err
		}
		_, err = subject(h.InodeID, v.InodeID)
		return err
	case TypeDentryAdd:
		_, err := DecodeDentryAdd(p)
		return err
	case TypeDentryRemove:
		_, err := DecodeDentryRemove(p)
		return err
	case TypeDentryRename:
		_, err := DecodeDentryRename(p)
		return err
	case TypeHardlinkAdd:
		_, err := DecodeHardlinkAdd(p)
		return err
	case TypeHardlinkRemove:
		_, err := DecodeHardlinkRemove(p)
		return err
	case TypeDataExtent:
		if _, err := DecodeDataExtent(p); err != nil {
			return err
		}
		_, err := subject(h.InodeID, 0)
		return err
	case TypeDataZeroRange:
		if _, err := DecodeDataZeroRange(p); err != nil {
			return err
		}
		_, err := subject(h.InodeID, 0)
		return err
	case TypeFileResize:
		if _, err := DecodeFileResize(p); err != nil {
			return err
		}
		_, err := subject(h.InodeID, 0)
		return err
	case TypeSnapshotRoot:
		_, err := DecodeSnapshotRoot(p)
		return err
	case TypeIndexCheckpoint:
		_, err := DecodeIndexCheckpoint(p)
		return err
	default:
		return nil
	}
}
