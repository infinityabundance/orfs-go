package record

import "fmt"

// Record type codes (format_version = 1). Bit 15 marks optional extensions.
const (
	TypeVolumeGenesis    uint16 = 0x0001
	TypeTxBegin          uint16 = 0x0002
	TypeTxCommit         uint16 = 0x0003
	TypeTxAbort          uint16 = 0x0004
	TypeInodeCreate      uint16 = 0x0005
	TypeInodeMetadata    uint16 = 0x0006
	TypeInodeSetattr     uint16 = 0x0007
	TypeDentryAdd        uint16 = 0x0008
	TypeDentryRemove     uint16 = 0x0009
	TypeDentryRename     uint16 = 0x000A
	TypeDataExtent       uint16 = 0x000B
	TypeDataZeroRange    uint16 = 0x000C
	TypeFileResize       uint16 = 0x000D
	TypeSymlinkCreate    uint16 = 0x000E
	TypeHardlinkAdd      uint16 = 0x000F
	TypeHardlinkRemove   uint16 = 0x0010
	TypeXattrSet         uint16 = 0x0011
	TypeXattrRemove      uint16 = 0x0012
	TypeSnapshotRoot     uint16 = 0x0013
	TypeIndexCheckpoint  uint16 = 0x0014
	TypePermissionChange uint16 = 0x0015
)

var typeName = map[uint16]string{
	TypeVolumeGenesis:    "VOLUME_GENESIS",
	TypeTxBegin:          "TX_BEGIN",
	TypeTxCommit:         "TX_COMMIT",
	TypeTxAbort:          "TX_ABORT",
	TypeInodeCreate:      "INODE_CREATE",
	TypeInodeMetadata:    "INODE_METADATA",
	TypeInodeSetattr:     "INODE_SETATTR",
	TypeDentryAdd:        "DENTRY_ADD",
	TypeDentryRemove:     "DENTRY_REMOVE",
	TypeDentryRename:     "DENTRY_RENAME",
	TypeDataExtent:       "DATA_EXTENT",
	TypeDataZeroRange:    "DATA_ZERO_RANGE",
	TypeFileResize:       "FILE_RESIZE",
	TypeSymlinkCreate:    "SYMLINK_CREATE",
	TypeHardlinkAdd:      "HARDLINK_ADD",
	TypeHardlinkRemove:   "HARDLINK_REMOVE",
	TypeXattrSet:         "XATTR_SET",
	TypeXattrRemove:      "XATTR_REMOVE",
	TypeSnapshotRoot:     "SNAPSHOT_ROOT",
	TypeIndexCheckpoint:  "INDEX_CHECKPOINT",
	TypePermissionChange: "PERMISSION_CHANGE",
}

// TypeName returns the registry name of a code, mirroring record_type_name:
// an unknown optional code is OPTIONAL_0xNNNN, an unknown mandatory code is
// UNKNOWN_0xNNNN.
func TypeName(code uint16) string {
	if n, ok := typeName[code]; ok {
		return n
	}
	if code&OptionalTypeBit != 0 {
		return fmt.Sprintf("OPTIONAL_0x%04X", code)
	}
	return fmt.Sprintf("UNKNOWN_0x%04X", code)
}

// IsStructural reports whether a type is a transaction/identity frame that may
// never appear as an ordinary data record inside a transaction.
func IsStructural(code uint16) bool {
	switch code {
	case TypeVolumeGenesis, TypeTxBegin, TypeTxCommit, TypeTxAbort:
		return true
	}
	return false
}

// PublishesRoot reports whether a record type advances the committed root when
// its record ends. Only VOLUME_GENESIS and TX_COMMIT do.
func PublishesRoot(code uint16) bool {
	return code == TypeVolumeGenesis || code == TypeTxCommit
}

// IsKnown reports whether the code is in the format-1 registry.
func IsKnown(code uint16) bool {
	_, ok := typeName[code]
	return ok
}
