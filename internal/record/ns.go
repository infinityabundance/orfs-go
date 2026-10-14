package record

import "github.com/infinityabundance/orfs-go/internal/orfserr"

// DentryAdd is the DENTRY_ADD / HARDLINK_ADD payload (identical layout,
// different magic).
type DentryAdd struct {
	Parent uint64
	Child  uint64
	Name   []byte
}

func encodeDentryAdd(magic uint32, t DentryAdd) []byte {
	b := make([]byte, 4+8+8+4+len(t.Name))
	putU32(b[0:4], magic)
	putU64(b[4:12], t.Parent)
	putU64(b[12:20], t.Child)
	putU32(b[20:24], uint32(len(t.Name)))
	copy(b[24:], t.Name)
	return appendCRC(b)
}

func decodeDentryAdd(b []byte, magic uint32, what string) (DentryAdd, error) {
	var t DentryAdd
	r := &payloadReader{b: b}
	if r.u32() != magic {
		return t, orfserr.Corrupt("%s magic mismatch", what)
	}
	t.Parent = r.u64()
	t.Child = r.u64()
	nameLen := int(r.u32())
	if r.err == nil && nameLen > NameMax {
		return t, orfserr.Corrupt("%s name length %d exceeds %d", what, nameLen, NameMax)
	}
	if r.need(nameLen) {
		t.Name = append([]byte(nil), r.b[r.off:r.off+nameLen]...)
		r.off += nameLen
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	if err := validateName(t.Name); err != nil {
		return t, err
	}
	return t, nil
}

// EncodeDentryAdd renders a DENTRY_ADD payload.
func EncodeDentryAdd(t DentryAdd) []byte { return encodeDentryAdd(MagicDentryAdd, t) }

// DecodeDentryAdd parses a DENTRY_ADD payload.
func DecodeDentryAdd(b []byte) (DentryAdd, error) {
	return decodeDentryAdd(b, MagicDentryAdd, "DENTRY_ADD")
}

// EncodeHardlinkAdd renders a HARDLINK_ADD payload.
func EncodeHardlinkAdd(t DentryAdd) []byte { return encodeDentryAdd(MagicHardlinkAdd, t) }

// DecodeHardlinkAdd parses a HARDLINK_ADD payload.
func DecodeHardlinkAdd(b []byte) (DentryAdd, error) {
	return decodeDentryAdd(b, MagicHardlinkAdd, "HARDLINK_ADD")
}

// DentryRemove is the DENTRY_REMOVE / HARDLINK_REMOVE payload.
type DentryRemove struct {
	Parent uint64
	Name   []byte
}

func encodeDentryRemove(magic uint32, t DentryRemove) []byte {
	b := make([]byte, 4+8+4+len(t.Name))
	putU32(b[0:4], magic)
	putU64(b[4:12], t.Parent)
	putU32(b[12:16], uint32(len(t.Name)))
	copy(b[16:], t.Name)
	return appendCRC(b)
}

func decodeDentryRemove(b []byte, magic uint32, what string) (DentryRemove, error) {
	var t DentryRemove
	r := &payloadReader{b: b}
	if r.u32() != magic {
		return t, orfserr.Corrupt("%s magic mismatch", what)
	}
	t.Parent = r.u64()
	nameLen := int(r.u32())
	if r.err == nil && nameLen > NameMax {
		return t, orfserr.Corrupt("%s name length %d exceeds %d", what, nameLen, NameMax)
	}
	if r.need(nameLen) {
		t.Name = append([]byte(nil), r.b[r.off:r.off+nameLen]...)
		r.off += nameLen
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	// Removal targets are still validated: a committed record naming "/" or NUL
	// is corrupt and is never indexed.
	if err := validateName(t.Name); err != nil {
		return t, err
	}
	return t, nil
}

// EncodeDentryRemove renders a DENTRY_REMOVE payload.
func EncodeDentryRemove(t DentryRemove) []byte { return encodeDentryRemove(MagicDentryRemove, t) }

// DecodeDentryRemove parses a DENTRY_REMOVE payload.
func DecodeDentryRemove(b []byte) (DentryRemove, error) {
	return decodeDentryRemove(b, MagicDentryRemove, "DENTRY_REMOVE")
}

// EncodeHardlinkRemove renders a HARDLINK_REMOVE payload.
func EncodeHardlinkRemove(t DentryRemove) []byte { return encodeDentryRemove(MagicHardlinkRemove, t) }

// DecodeHardlinkRemove parses a HARDLINK_REMOVE payload.
func DecodeHardlinkRemove(b []byte) (DentryRemove, error) {
	return decodeDentryRemove(b, MagicHardlinkRemove, "HARDLINK_REMOVE")
}

// DentryRename is the variable DENTRY_RENAME payload. This is the atomic
// primitive; overwriting is expressed by a preceding removal in the same
// transaction, and identity (inode id) is preserved.
type DentryRename struct {
	Flags     uint8
	OldParent uint64
	NewParent uint64
	OldName   []byte
	NewName   []byte
}

// Encode renders the DENTRY_RENAME payload.
func (t DentryRename) Encode() []byte {
	b := make([]byte, 4+1+3+8+8+4+len(t.OldName)+4+len(t.NewName))
	putU32(b[0:4], MagicDentryRename)
	putU8(b[4:5], t.Flags)
	// bytes [5,8) are zero padding.
	putU64(b[8:16], t.OldParent)
	putU64(b[16:24], t.NewParent)
	putU32(b[24:28], uint32(len(t.OldName)))
	copy(b[28:], t.OldName)
	off := 28 + len(t.OldName)
	putU32(b[off:off+4], uint32(len(t.NewName)))
	copy(b[off+4:], t.NewName)
	return appendCRC(b)
}

// DecodeDentryRename parses a DENTRY_RENAME payload.
func DecodeDentryRename(b []byte) (DentryRename, error) {
	var t DentryRename
	r := &payloadReader{b: b}
	if r.u32() != MagicDentryRename {
		return t, orfserr.Corrupt("DENTRY_RENAME magic mismatch")
	}
	t.Flags = r.u8()
	if t.Flags != RenameMove && t.Flags != RenameExchange {
		return t, orfserr.Corrupt("DENTRY_RENAME unknown flags %d", t.Flags)
	}
	r.skip(3)
	t.OldParent = r.u64()
	t.NewParent = r.u64()
	oldLen := int(r.u32())
	if r.err == nil && oldLen > NameMax {
		return t, orfserr.Corrupt("DENTRY_RENAME old name length %d exceeds %d", oldLen, NameMax)
	}
	if r.need(oldLen) {
		t.OldName = append([]byte(nil), r.b[r.off:r.off+oldLen]...)
		r.off += oldLen
	}
	newLen := int(r.u32())
	if r.err == nil && newLen > NameMax {
		return t, orfserr.Corrupt("DENTRY_RENAME new name length %d exceeds %d", newLen, NameMax)
	}
	if r.need(newLen) {
		t.NewName = append([]byte(nil), r.b[r.off:r.off+newLen]...)
		r.off += newLen
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	if err := validateName(t.OldName); err != nil {
		return t, err
	}
	if err := validateName(t.NewName); err != nil {
		return t, err
	}
	return t, nil
}
