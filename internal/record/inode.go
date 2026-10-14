package record

import "github.com/infinityabundance/orfs-go/internal/orfserr"

// Timespec is a nanosecond-resolution timestamp as written on the wire.
type Timespec struct {
	Secs  int64
	Nanos uint32
}

// ZeroTimespec is the all-zero timestamp used by genesis and symlink creation.
var ZeroTimespec = Timespec{}

func putTimespec(b []byte, t Timespec) {
	putU64(b[0:8], uint64(t.Secs))
	putU32(b[8:12], t.Nanos)
}

func getTimespec(b []byte) Timespec {
	return Timespec{Secs: int64(getU64(b[0:8])), Nanos: getU32(b[8:12])}
}

// InodeCreate is the fixed 88-byte INODE_CREATE payload.
type InodeCreate struct {
	InodeID    uint64
	Generation uint64
	Kind       uint8
	Mode       uint32
	UID        uint32
	GID        uint32
	Nlink      uint32
	Size       uint64
	Atime      Timespec
	Mtime      Timespec
	Ctime      Timespec
}

// Encode renders the INODE_CREATE payload.
func (t InodeCreate) Encode() []byte {
	b := make([]byte, InodeCreatePayloadLen)
	putU32(b[0:4], MagicInodeCreate)
	putU64(b[4:12], t.InodeID)
	putU64(b[12:20], t.Generation)
	putU8(b[20:21], t.Kind)
	// bytes [21,24) are zero padding, not read.
	putU32(b[24:28], t.Mode)
	putU32(b[28:32], t.UID)
	putU32(b[32:36], t.GID)
	putU32(b[36:40], t.Nlink)
	putU64(b[40:48], t.Size)
	putTimespec(b[48:60], t.Atime)
	putTimespec(b[60:72], t.Mtime)
	putTimespec(b[72:84], t.Ctime)
	putU32(b[84:88], CRC32C(b[0:84]))
	return b
}

// DecodeInodeCreate parses an INODE_CREATE payload.
func DecodeInodeCreate(b []byte) (InodeCreate, error) {
	var t InodeCreate
	if err := checkFixed(b, MagicInodeCreate, InodeCreatePayloadLen, "INODE_CREATE"); err != nil {
		return t, err
	}
	t.InodeID = getU64(b[4:12])
	t.Generation = getU64(b[12:20])
	t.Kind = getU8(b[20:21])
	if t.Kind > KindSymlink {
		return t, orfserr.Corrupt("INODE_CREATE unknown kind %d", t.Kind)
	}
	t.Mode = getU32(b[24:28])
	t.UID = getU32(b[28:32])
	t.GID = getU32(b[32:36])
	t.Nlink = getU32(b[36:40])
	t.Size = getU64(b[40:48])
	t.Atime = getTimespec(b[48:60])
	t.Mtime = getTimespec(b[60:72])
	t.Ctime = getTimespec(b[72:84])
	return t, nil
}

// MknodCreate is the fixed 88-byte MKNOD_CREATE payload. It mirrors
// INODE_CREATE's field layout with the device number in the Size slot: a FIFO,
// socket, or device node carries identity and metadata but no content.
type MknodCreate struct {
	InodeID    uint64
	Generation uint64
	Kind       uint8
	Mode       uint32
	UID        uint32
	GID        uint32
	Nlink      uint32
	Rdev       uint64
	Atime      Timespec
	Mtime      Timespec
	Ctime      Timespec
}

// Encode renders the MKNOD_CREATE payload.
func (t MknodCreate) Encode() []byte {
	b := make([]byte, MknodCreatePayloadLen)
	putU32(b[0:4], MagicMknodCreate)
	putU64(b[4:12], t.InodeID)
	putU64(b[12:20], t.Generation)
	putU8(b[20:21], t.Kind)
	// bytes [21,24) are zero padding, not read.
	putU32(b[24:28], t.Mode)
	putU32(b[28:32], t.UID)
	putU32(b[32:36], t.GID)
	putU32(b[36:40], t.Nlink)
	putU64(b[40:48], t.Rdev)
	putTimespec(b[48:60], t.Atime)
	putTimespec(b[60:72], t.Mtime)
	putTimespec(b[72:84], t.Ctime)
	putU32(b[84:88], CRC32C(b[0:84]))
	return b
}

// DecodeMknodCreate parses a MKNOD_CREATE payload.
func DecodeMknodCreate(b []byte) (MknodCreate, error) {
	var t MknodCreate
	if err := checkFixed(b, MagicMknodCreate, MknodCreatePayloadLen, "MKNOD_CREATE"); err != nil {
		return t, err
	}
	t.InodeID = getU64(b[4:12])
	t.Generation = getU64(b[12:20])
	t.Kind = getU8(b[20:21])
	if t.Kind < KindFifo || t.Kind > KindSocket {
		return t, orfserr.Corrupt("MKNOD_CREATE kind %d out of range", t.Kind)
	}
	t.Mode = getU32(b[24:28])
	t.UID = getU32(b[28:32])
	t.GID = getU32(b[32:36])
	t.Nlink = getU32(b[36:40])
	t.Rdev = getU64(b[40:48])
	t.Atime = getTimespec(b[48:60])
	t.Mtime = getTimespec(b[60:72])
	t.Ctime = getTimespec(b[72:84])
	return t, nil
}

// CtimeUpdate is the fixed 28-byte CTIME_UPDATE payload. A metadata-only change
// (chmod, chown, link, unlink, an overwrite) updates ctime without touching
// mtime; INODE_SETATTR derives ctime from mtime for content changes, so those
// carry no separate ctime.
type CtimeUpdate struct {
	InodeID uint64
	Ctime   Timespec
}

// Encode renders the CTIME_UPDATE payload.
func (t CtimeUpdate) Encode() []byte {
	b := make([]byte, CtimeUpdatePayloadLen)
	putU32(b[0:4], MagicCtimeUpdate)
	putU64(b[4:12], t.InodeID)
	putTimespec(b[12:24], t.Ctime)
	putU32(b[24:28], CRC32C(b[0:24]))
	return b
}

// DecodeCtimeUpdate parses a CTIME_UPDATE payload.
func DecodeCtimeUpdate(b []byte) (CtimeUpdate, error) {
	var t CtimeUpdate
	if err := checkFixed(b, MagicCtimeUpdate, CtimeUpdatePayloadLen, "CTIME_UPDATE"); err != nil {
		return t, err
	}
	t.InodeID = getU64(b[4:12])
	t.Ctime = getTimespec(b[12:24])
	return t, nil
}

// SymlinkCreate is the variable SYMLINK_CREATE payload.
type SymlinkCreate struct {
	InodeID    uint64
	Generation uint64
	Mode       uint32
	UID        uint32
	GID        uint32
	Mtime      Timespec
	Target     []byte
}

// Encode renders the SYMLINK_CREATE payload.
func (t SymlinkCreate) Encode() []byte {
	b := make([]byte, SymlinkCreatePrefixLen+len(t.Target))
	putU32(b[0:4], MagicSymlinkCreate)
	putU64(b[4:12], t.InodeID)
	putU64(b[12:20], t.Generation)
	putU32(b[20:24], t.Mode)
	putU32(b[24:28], t.UID)
	putU32(b[28:32], t.GID)
	putTimespec(b[32:44], t.Mtime)
	putU32(b[44:48], uint32(len(t.Target)))
	copy(b[48:], t.Target)
	return appendCRC(b)
}

// DecodeSymlinkCreate parses a SYMLINK_CREATE payload.
func DecodeSymlinkCreate(b []byte) (SymlinkCreate, error) {
	var t SymlinkCreate
	if len(b) < SymlinkCreatePrefixLen+4 {
		return t, orfserr.Corrupt("SYMLINK_CREATE payload too short")
	}
	if getU32(b[0:4]) != MagicSymlinkCreate {
		return t, orfserr.Corrupt("SYMLINK_CREATE magic mismatch")
	}
	targetLen := int(getU32(b[44:48]))
	if targetLen < 1 || targetLen > SymlinkTargetMax {
		return t, orfserr.Corrupt("SYMLINK_CREATE target length %d out of range", targetLen)
	}
	if SymlinkCreatePrefixLen+targetLen+4 != len(b) {
		return t, orfserr.Corrupt("SYMLINK_CREATE length %d != %d", len(b), SymlinkCreatePrefixLen+targetLen+4)
	}
	if !payloadCRCValid(b) {
		return t, orfserr.Corrupt("SYMLINK_CREATE checksum mismatch")
	}
	t.InodeID = getU64(b[4:12])
	t.Generation = getU64(b[12:20])
	t.Mode = getU32(b[20:24])
	t.UID = getU32(b[24:28])
	t.GID = getU32(b[28:32])
	t.Mtime = getTimespec(b[32:44])
	t.Target = make([]byte, targetLen)
	copy(t.Target, b[48:48+targetLen])
	return t, nil
}

// InodeMetadata is the fixed 20-byte INODE_METADATA payload.
type InodeMetadata struct {
	InodeID uint64
	Nlink   uint32
}

// Encode renders the INODE_METADATA payload.
func (t InodeMetadata) Encode() []byte {
	b := make([]byte, InodeMetadataPayloadLen)
	putU32(b[0:4], MagicInodeMetadata)
	putU64(b[4:12], t.InodeID)
	putU32(b[12:16], t.Nlink)
	putU32(b[16:20], CRC32C(b[0:16]))
	return b
}

// DecodeInodeMetadata parses an INODE_METADATA payload.
func DecodeInodeMetadata(b []byte) (InodeMetadata, error) {
	var t InodeMetadata
	if err := checkFixed(b, MagicInodeMetadata, InodeMetadataPayloadLen, "INODE_METADATA"); err != nil {
		return t, err
	}
	t.InodeID = getU64(b[4:12])
	t.Nlink = getU32(b[12:16])
	return t, nil
}

// PermissionChange is the fixed 32-byte PERMISSION_CHANGE payload. Field
// presence is carried by three separate u8 flags, not a bitmask.
type PermissionChange struct {
	InodeID uint64
	Mode    *uint32
	UID     *uint32
	GID     *uint32
}

// Encode renders the PERMISSION_CHANGE payload.
func (t PermissionChange) Encode() []byte {
	b := make([]byte, PermissionChangePayloadLen)
	putU32(b[0:4], MagicPermissionChange)
	putU64(b[4:12], t.InodeID)
	if t.Mode != nil {
		putU8(b[12:13], 1)
		putU32(b[16:20], *t.Mode)
	}
	if t.UID != nil {
		putU8(b[13:14], 1)
		putU32(b[20:24], *t.UID)
	}
	if t.GID != nil {
		putU8(b[14:15], 1)
		putU32(b[24:28], *t.GID)
	}
	putU32(b[28:32], CRC32C(b[0:28]))
	return b
}

// DecodePermissionChange parses a PERMISSION_CHANGE payload.
func DecodePermissionChange(b []byte) (PermissionChange, error) {
	var t PermissionChange
	if err := checkFixed(b, MagicPermissionChange, PermissionChangePayloadLen, "PERMISSION_CHANGE"); err != nil {
		return t, err
	}
	t.InodeID = getU64(b[4:12])
	if getU8(b[12:13]) != 0 {
		v := getU32(b[16:20])
		t.Mode = &v
	}
	if getU8(b[13:14]) != 0 {
		v := getU32(b[20:24])
		t.UID = &v
	}
	if getU8(b[14:15]) != 0 {
		v := getU32(b[24:28])
		t.GID = &v
	}
	return t, nil
}

// InodeSetattr is the fixed 44-byte INODE_SETATTR payload. On replay, ctime is
// derived (always set to mtime), never carried.
type InodeSetattr struct {
	InodeID uint64
	Atime   *Timespec
	Mtime   *Timespec
}

// Encode renders the INODE_SETATTR payload.
func (t InodeSetattr) Encode() []byte {
	b := make([]byte, InodeSetattrPayloadLen)
	putU32(b[0:4], MagicInodeSetattr)
	putU64(b[4:12], t.InodeID)
	if t.Atime != nil {
		putU8(b[12:13], 1)
		putTimespec(b[16:28], *t.Atime)
	}
	if t.Mtime != nil {
		putU8(b[13:14], 1)
		putTimespec(b[28:40], *t.Mtime)
	}
	putU32(b[40:44], CRC32C(b[0:40]))
	return b
}

// DecodeInodeSetattr parses an INODE_SETATTR payload.
func DecodeInodeSetattr(b []byte) (InodeSetattr, error) {
	var t InodeSetattr
	if err := checkFixed(b, MagicInodeSetattr, InodeSetattrPayloadLen, "INODE_SETATTR"); err != nil {
		return t, err
	}
	t.InodeID = getU64(b[4:12])
	if getU8(b[12:13]) != 0 {
		v := getTimespec(b[16:28])
		t.Atime = &v
	}
	if getU8(b[13:14]) != 0 {
		v := getTimespec(b[28:40])
		t.Mtime = &v
	}
	return t, nil
}

// XattrSet is the variable XATTR_SET payload.
type XattrSet struct {
	InodeID uint64
	Name    []byte
	Value   []byte
}

// Encode renders the XATTR_SET payload.
func (t XattrSet) Encode() []byte {
	b := make([]byte, 4+8+4+len(t.Name)+4+len(t.Value))
	putU32(b[0:4], MagicXattrSet)
	putU64(b[4:12], t.InodeID)
	putU32(b[12:16], uint32(len(t.Name)))
	copy(b[16:], t.Name)
	off := 16 + len(t.Name)
	putU32(b[off:off+4], uint32(len(t.Value)))
	copy(b[off+4:], t.Value)
	return appendCRC(b)
}

func validateXattrName(name []byte) error {
	if len(name) == 0 || len(name) > XattrNameMax {
		return orfserr.Corrupt("xattr name length %d out of range", len(name))
	}
	for _, c := range name {
		if c == 0 {
			return orfserr.Corrupt("xattr name contains NUL")
		}
	}
	return nil
}

// DecodeXattrSet parses an XATTR_SET payload.
func DecodeXattrSet(b []byte) (XattrSet, error) {
	var t XattrSet
	r := &payloadReader{b: b}
	if r.u32() != MagicXattrSet {
		return t, orfserr.Corrupt("XATTR_SET magic mismatch")
	}
	t.InodeID = r.u64()
	nameLen := int(r.u32())
	if r.err == nil && (nameLen == 0 || nameLen > XattrNameMax) {
		return t, orfserr.Corrupt("xattr name length %d out of range", nameLen)
	}
	if r.need(nameLen) {
		t.Name = append([]byte(nil), r.b[r.off:r.off+nameLen]...)
		r.off += nameLen
	}
	if err := validateXattrName(t.Name); err != nil {
		return t, err
	}
	valueLen := int(r.u32())
	if r.err == nil && valueLen > XattrValueMax {
		return t, orfserr.Corrupt("xattr value length %d exceeds %d", valueLen, XattrValueMax)
	}
	if r.need(valueLen) {
		t.Value = append([]byte(nil), r.b[r.off:r.off+valueLen]...)
		r.off += valueLen
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	return t, nil
}

// XattrRemove is the variable XATTR_REMOVE payload.
type XattrRemove struct {
	InodeID uint64
	Name    []byte
}

// Encode renders the XATTR_REMOVE payload.
func (t XattrRemove) Encode() []byte {
	b := make([]byte, 4+8+4+len(t.Name))
	putU32(b[0:4], MagicXattrRemove)
	putU64(b[4:12], t.InodeID)
	putU32(b[12:16], uint32(len(t.Name)))
	copy(b[16:], t.Name)
	return appendCRC(b)
}

// DecodeXattrRemove parses an XATTR_REMOVE payload.
func DecodeXattrRemove(b []byte) (XattrRemove, error) {
	var t XattrRemove
	r := &payloadReader{b: b}
	if r.u32() != MagicXattrRemove {
		return t, orfserr.Corrupt("XATTR_REMOVE magic mismatch")
	}
	t.InodeID = r.u64()
	nameLen := int(r.u32())
	if r.err == nil && (nameLen == 0 || nameLen > XattrNameMax) {
		return t, orfserr.Corrupt("xattr name length %d out of range", nameLen)
	}
	if r.need(nameLen) {
		t.Name = append([]byte(nil), r.b[r.off:r.off+nameLen]...)
		r.off += nameLen
	}
	if err := validateXattrName(t.Name); err != nil {
		return t, err
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	return t, nil
}
