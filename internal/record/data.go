package record

import "github.com/infinityabundance/orfs-go/internal/orfserr"

// DataExtent is the variable DATA_EXTENT payload. Its subject inode is the
// record header's inode_id; the payload carries only the logical offset and the
// immutable bytes.
type DataExtent struct {
	Offset uint64
	Data   []byte
}

// Encode renders the DATA_EXTENT payload.
func (t DataExtent) Encode() []byte {
	b := make([]byte, DataExtentDataOffset+len(t.Data))
	putU32(b[0:4], MagicDataExtent)
	putU64(b[4:12], t.Offset)
	putU64(b[12:20], uint64(len(t.Data)))
	copy(b[20:], t.Data)
	return appendCRC(b)
}

// DecodeDataExtent parses a DATA_EXTENT payload.
func DecodeDataExtent(b []byte) (DataExtent, error) {
	var t DataExtent
	r := &payloadReader{b: b}
	if r.u32() != MagicDataExtent {
		return t, orfserr.Corrupt("DATA_EXTENT magic mismatch")
	}
	t.Offset = r.u64()
	dataLen := r.u64()
	if r.err == nil && dataLen > MaxPayloadLen {
		return t, orfserr.Corrupt("DATA_EXTENT data length %d exceeds bound", dataLen)
	}
	if r.need(int(dataLen)) {
		t.Data = append([]byte(nil), r.b[r.off:r.off+int(dataLen)]...)
		r.off += int(dataLen)
	}
	if err := r.finish(); err != nil {
		return t, err
	}
	return t, nil
}

// DataZeroRange is the fixed 24-byte DATA_ZERO_RANGE payload.
type DataZeroRange struct {
	Offset uint64
	Length uint64
}

// Encode renders the DATA_ZERO_RANGE payload.
func (t DataZeroRange) Encode() []byte {
	b := make([]byte, DataZeroRangePayloadLen)
	putU32(b[0:4], MagicDataZeroRange)
	putU64(b[4:12], t.Offset)
	putU64(b[12:20], t.Length)
	putU32(b[20:24], CRC32C(b[0:20]))
	return b
}

// DecodeDataZeroRange parses a DATA_ZERO_RANGE payload.
func DecodeDataZeroRange(b []byte) (DataZeroRange, error) {
	var t DataZeroRange
	if err := checkFixed(b, MagicDataZeroRange, DataZeroRangePayloadLen, "DATA_ZERO_RANGE"); err != nil {
		return t, err
	}
	t.Offset = getU64(b[4:12])
	t.Length = getU64(b[12:20])
	return t, nil
}

// FileResize is the fixed 16-byte FILE_RESIZE payload.
type FileResize struct {
	NewSize uint64
}

// Encode renders the FILE_RESIZE payload.
func (t FileResize) Encode() []byte {
	b := make([]byte, FileResizePayloadLen)
	putU32(b[0:4], MagicFileResize)
	putU64(b[4:12], t.NewSize)
	putU32(b[12:16], CRC32C(b[0:12]))
	return b
}

// DecodeFileResize parses a FILE_RESIZE payload.
func DecodeFileResize(b []byte) (FileResize, error) {
	var t FileResize
	if err := checkFixed(b, MagicFileResize, FileResizePayloadLen, "FILE_RESIZE"); err != nil {
		return t, err
	}
	t.NewSize = getU64(b[4:12])
	return t, nil
}
