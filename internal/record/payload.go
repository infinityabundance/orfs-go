package record

import (
	"encoding/binary"
	"unicode/utf8"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

// Payload magics. Each is a u32 little-endian whose on-disk bytes spell the
// mnemonic to the right; the last character is a revision digit and there is no
// separate version field.
const (
	MagicTxBegin          uint32 = 0x314E4254 // "TBN1"
	MagicTxCommit         uint32 = 0x314D4354 // "TCM1"
	MagicTxAbort          uint32 = 0x31414254 // "TBA1"
	MagicInodeCreate      uint32 = 0x31524349 // "ICR1"
	MagicMknodCreate      uint32 = 0x31504E4D // "MNP1"
	MagicCtimeUpdate      uint32 = 0x31555443 // "CTU1"
	MagicSymlinkCreate    uint32 = 0x314B4C53 // "SLK1"
	MagicInodeMetadata    uint32 = 0x31544D49 // "IMT1"
	MagicPermissionChange uint32 = 0x31484350 // "PCH1"
	MagicInodeSetattr     uint32 = 0x31415349 // "ISA1"
	MagicXattrSet         uint32 = 0x31534158 // "XAS1"
	MagicXattrRemove      uint32 = 0x31524158 // "XAR1"
	MagicDentryAdd        uint32 = 0x31444144 // "DAD1"
	MagicDentryRemove     uint32 = 0x314D5244 // "DRM1"
	MagicDentryRename     uint32 = 0x314E5244 // "DRN1"
	MagicHardlinkAdd      uint32 = 0x31414C48 // "HLA1"
	MagicHardlinkRemove   uint32 = 0x31524C48 // "HLR1"
	MagicDataExtent       uint32 = 0x31584544 // "DEX1"
	MagicDataZeroRange    uint32 = 0x31525A44 // "DZR1"
	MagicFileResize       uint32 = 0x31535246 // "FRS1"
	MagicSnapshotRoot     uint32 = 0x31524E53 // "SNR1"
	MagicIndexCheckpoint  uint32 = 0x31504349 // "ICP1"
)

// Fixed payload lengths (including the leading magic and the trailing CRC).
const (
	TxBeginPayloadLen     = 28
	TxCommitPayloadLen    = 72
	TxAbortPrefixLen      = 20
	InodeCreatePayloadLen = 88
	// MknodCreatePayloadLen matches INODE_CREATE; the device number occupies the
	// Size slot, since a special file carries no content size.
	MknodCreatePayloadLen = 88
	CtimeUpdatePayloadLen = 28
	// SymlinkCreatePrefixLen is the offset of the symlink target within the
	// payload (the fixed fields including the u32 target_len end at 48).
	SymlinkCreatePrefixLen     = 48
	InodeMetadataPayloadLen    = 20
	PermissionChangePayloadLen = 32
	InodeSetattrPayloadLen     = 44
	DataZeroRangePayloadLen    = 24
	FileResizePayloadLen       = 16
	IndexCheckpointFixedLen    = 64
)

// Bounds and limits.
const (
	// BracketChecksumLen is the BLAKE3 digest length in TX_COMMIT.
	BracketChecksumLen = 32
	// TxAbortMaxMessage bounds a TX_ABORT message.
	TxAbortMaxMessage = 4096
	// NameMax bounds a path component.
	NameMax = 255
	// XattrNameMax bounds an extended-attribute name.
	XattrNameMax = 255
	// XattrValueMax bounds an extended-attribute value.
	XattrValueMax = 65536
	// SymlinkTargetMax bounds a symlink target.
	SymlinkTargetMax = 4096
	// DataExtentDataOffset is the offset of `data` within a DATA_EXTENT payload.
	DataExtentDataOffset = 20
	// RootInode is the root inode id, matching FUSE_ROOT_ID.
	RootInode uint64 = 1
)

// Abort reason codes.
const (
	AbortUserRequested  uint16 = 1
	AbortValidationFail uint16 = 2
	AbortInternalError  uint16 = 3
	AbortDurabilityFail uint16 = 4
)

// Inode kinds.
const (
	KindFile    uint8 = 0
	KindDir     uint8 = 1
	KindSymlink uint8 = 2
	KindFifo    uint8 = 3
	KindChar    uint8 = 4
	KindBlock   uint8 = 5
	KindSocket  uint8 = 6
)

// Rename flags.
const (
	RenameMove     uint8 = 0
	RenameExchange uint8 = 1
)

// InodeKindName names an inode kind.
func InodeKindName(k uint8) string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "directory"
	case KindSymlink:
		return "symlink"
	case KindFifo:
		return "fifo"
	case KindChar:
		return "char device"
	case KindBlock:
		return "block device"
	case KindSocket:
		return "socket"
	}
	return "unknown"
}

// payloadCRCValid checks the trailing CRC32C over the preceding bytes.
func payloadCRCValid(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	return binary.LittleEndian.Uint32(b[len(b)-4:]) == CRC32C(b[:len(b)-4])
}

// appendCRC appends the trailing CRC32C over b.
func appendCRC(b []byte) []byte {
	var tail [4]byte
	binary.LittleEndian.PutUint32(tail[:], CRC32C(b))
	return append(b, tail[:]...)
}

// checkFixed validates the magic and trailing CRC of a fixed-length payload.
func checkFixed(b []byte, want uint32, wantLen int, name string) error {
	if len(b) != wantLen {
		return orfserr.Corrupt("%s payload length %d != %d", name, len(b), wantLen)
	}
	if binary.LittleEndian.Uint32(b[0:4]) != want {
		return orfserr.Corrupt("%s payload magic mismatch", name)
	}
	if !payloadCRCValid(b) {
		return orfserr.Corrupt("%s payload checksum mismatch", name)
	}
	return nil
}

// validateName enforces the committed-name rule: non-empty, ≤ NAME_MAX bytes,
// no '/', no NUL, and not "." or "..".
func validateName(name []byte) error {
	if len(name) == 0 {
		return orfserr.Corrupt("empty name")
	}
	if len(name) > NameMax {
		return orfserr.Corrupt("name length %d exceeds %d", len(name), NameMax)
	}
	for _, c := range name {
		if c == '/' || c == 0 {
			return orfserr.Corrupt("name contains a forbidden byte")
		}
	}
	if string(name) == "." || string(name) == ".." {
		return orfserr.Corrupt("name is %q", name)
	}
	return nil
}

// ValidateName is the exported committed-name validator.
func ValidateName(name []byte) error { return validateName(name) }

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }
func putU8(b []byte, v uint8)   { b[0] = v }

func getU8(b []byte) uint8   { return b[0] }
func getU16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func getU32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func getU64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// payloadReader is a bounds-checked cursor over variable payload bytes.
type payloadReader struct {
	b   []byte
	off int
	err error
}

func (r *payloadReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if r.off+n > len(r.b) {
		r.err = orfserr.Corrupt("payload truncated at offset %d", r.off)
		return false
	}
	return true
}

func (r *payloadReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := getU32(r.b[r.off:])
	r.off += 4
	return v
}

func (r *payloadReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := getU64(r.b[r.off:])
	r.off += 8
	return v
}

func (r *payloadReader) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := getU16(r.b[r.off:])
	r.off += 2
	return v
}

func (r *payloadReader) u8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}

func (r *payloadReader) skip(n int) {
	if r.need(n) {
		r.off += n
	}
}

// varBytes reads a u32 length prefix followed by that many bytes.
func (r *payloadReader) varBytes(max int, what string) []byte {
	n := int(r.u32())
	if r.err != nil {
		return nil
	}
	if max >= 0 && n > max {
		r.err = orfserr.Corrupt("%s length %d exceeds bound %d", what, n, max)
		return nil
	}
	if !r.need(n) {
		return nil
	}
	out := make([]byte, n)
	copy(out, r.b[r.off:r.off+n])
	r.off += n
	return out
}

// finish requires the cursor to have consumed the payload except for the final
// 4-byte CRC, and that the CRC is valid.
func (r *payloadReader) finish() error {
	if r.err != nil {
		return r.err
	}
	if r.off+4 != len(r.b) {
		return orfserr.Corrupt("trailing bytes: consumed %d of %d", r.off, len(r.b))
	}
	if !payloadCRCValid(r.b) {
		return orfserr.Corrupt("payload checksum mismatch")
	}
	return nil
}

// checkTrailingCRC validates a variable payload's trailing CRC when the reader
// consumed everything but the CRC.
func checkTrailingCRC(b []byte, consumed int, name string) error {
	if consumed+4 != len(b) {
		return orfserr.Corrupt("%s trailing bytes", name)
	}
	if !payloadCRCValid(b) {
		return orfserr.Corrupt("%s payload checksum mismatch", name)
	}
	return nil
}

// validUTF8 reports whether b is valid UTF-8.
func validUTF8(b []byte) bool { return utf8.Valid(b) }
