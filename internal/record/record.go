// Package record implements the OrFS record envelope and the version-1 record
// type registry, byte-exactly as the Rust reference writes them.
//
// The framing layer never casts a struct over untrusted bytes: every field is
// encoded and decoded explicitly, little-endian, field by field. Header and
// footer checks are CRC32C over the exact byte ranges named below. They are
// integrity/framing checks, not authentication (see docs/SECURITY.md).
package record

import (
	"encoding/binary"
	"hash/crc32"
	"io"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

// Wire constants. These are normative; see docs/ON_DISK.md.
const (
	// Magic is the record magic; LE bytes spell "ORFS".
	Magic uint32 = 0x5346524F
	// FooterEndMarker closes a record; LE bytes spell "END!".
	FooterEndMarker uint32 = 0x21444E45
	// VolumeMetaMagic is the volume.meta magic; LE bytes spell "ORFV".
	VolumeMetaMagic uint32 = 0x5646524F
	// WatermarkMagic is the commit.watermark magic; LE bytes spell "ORFW".
	WatermarkMagic uint32 = 0x5746524F

	// FormatVersion is the current record/log format version.
	FormatVersion uint16 = 1
	// MinFormatVersion is the oldest known (legacy) format version.
	MinFormatVersion uint16 = 0

	// HeaderLen is the fixed record header length.
	HeaderLen = 60
	// FooterLen is the fixed record footer length.
	FooterLen = 16
	// EnvelopeOverhead is HeaderLen + FooterLen.
	EnvelopeOverhead = HeaderLen + FooterLen

	// MaxPayloadLen bounds allocation from untrusted records (64 MiB).
	MaxPayloadLen uint64 = 64 * 1024 * 1024

	// OptionalTypeBit marks a record type optional (a reader may skip it).
	OptionalTypeBit uint16 = 0x8000

	// DefaultSegmentSize is the default segment-size policy (64 MiB).
	DefaultSegmentSize uint64 = 64 * 1024 * 1024
)

// castagnoli is the CRC-32C table (Castagnoli), matching Rust's crc32c crate.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C returns the CRC-32C (Castagnoli) of b.
func CRC32C(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// Header is the 60-byte record header.
type Header struct {
	FormatVersion       uint16
	RecordType          uint16
	PayloadLength       uint64
	SequenceNumber      uint64
	TransactionID       uint64
	InodeID             uint64
	PreviousInodeOffset uint64
	Flags               uint32
}

// Encode writes the header, computing header_check over bytes [0, 56).
func (h Header) Encode() []byte {
	b := make([]byte, HeaderLen)
	binary.LittleEndian.PutUint32(b[0:4], Magic)
	binary.LittleEndian.PutUint16(b[4:6], h.FormatVersion)
	binary.LittleEndian.PutUint16(b[6:8], h.RecordType)
	binary.LittleEndian.PutUint32(b[8:12], HeaderLen)
	binary.LittleEndian.PutUint64(b[12:20], h.PayloadLength)
	binary.LittleEndian.PutUint64(b[20:28], h.SequenceNumber)
	binary.LittleEndian.PutUint64(b[28:36], h.TransactionID)
	binary.LittleEndian.PutUint64(b[36:44], h.InodeID)
	binary.LittleEndian.PutUint64(b[44:52], h.PreviousInodeOffset)
	binary.LittleEndian.PutUint32(b[52:56], h.Flags)
	binary.LittleEndian.PutUint32(b[56:60], CRC32C(b[0:56]))
	return b
}

// DecodeHeader parses a 60-byte header using the documented decode order:
// checksum first (so a partial header is a torn tail, not corruption), then
// magic/length, then format compatibility, then the payload bound.
func DecodeHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderLen {
		return h, orfserr.Torn("record header needs %d bytes, have %d", HeaderLen, len(b))
	}
	if binary.LittleEndian.Uint32(b[56:60]) != CRC32C(b[0:56]) {
		return h, orfserr.Torn("record header checksum mismatch")
	}
	if binary.LittleEndian.Uint32(b[0:4]) != Magic {
		return h, orfserr.Corrupt("record magic mismatch")
	}
	if binary.LittleEndian.Uint32(b[8:12]) != HeaderLen {
		return h, orfserr.Corrupt("record header_length %d != %d", binary.LittleEndian.Uint32(b[8:12]), HeaderLen)
	}
	h.FormatVersion = binary.LittleEndian.Uint16(b[4:6])
	if h.FormatVersion != FormatVersion {
		return h, orfserr.Compatibility("record format_version %d is not implemented (%d)", h.FormatVersion, FormatVersion)
	}
	h.RecordType = binary.LittleEndian.Uint16(b[6:8])
	h.PayloadLength = binary.LittleEndian.Uint64(b[12:20])
	if h.PayloadLength > MaxPayloadLen {
		return h, orfserr.Corrupt("record payload_length %d exceeds the %d-byte bound", h.PayloadLength, MaxPayloadLen)
	}
	h.SequenceNumber = binary.LittleEndian.Uint64(b[20:28])
	h.TransactionID = binary.LittleEndian.Uint64(b[28:36])
	h.InodeID = binary.LittleEndian.Uint64(b[36:44])
	h.PreviousInodeOffset = binary.LittleEndian.Uint64(b[44:52])
	h.Flags = binary.LittleEndian.Uint32(b[52:56])
	return h, nil
}

// TotalLen is the framed length of a record with this payload length.
func (h Header) TotalLen() (uint64, error) {
	total, ok := addU64(h.PayloadLength, EnvelopeOverhead)
	if !ok {
		return 0, orfserr.Overflow("record total length overflow for payload %d", h.PayloadLength)
	}
	return total, nil
}

// Footer is the 16-byte record footer.
type Footer struct {
	TotalRecordLength uint64
	RecordCheck       uint32
	EndMarker         uint32
}

// EncodeFooter builds the footer given the header+payload bytes.
func EncodeFooter(headerAndPayload []byte) []byte {
	b := make([]byte, FooterLen)
	binary.LittleEndian.PutUint64(b[0:8], uint64(len(headerAndPayload))+FooterLen)
	binary.LittleEndian.PutUint32(b[8:12], CRC32C(headerAndPayload))
	binary.LittleEndian.PutUint32(b[12:16], FooterEndMarker)
	return b
}

// DecodeFooter validates a footer against its header+payload.
func DecodeFooter(b []byte, headerAndPayload []byte, payloadLen uint64) (Footer, error) {
	var f Footer
	if len(b) < FooterLen {
		return f, orfserr.Torn("record footer needs %d bytes, have %d", FooterLen, len(b))
	}
	wantTotal, ok := addU64(payloadLen, EnvelopeOverhead)
	if !ok {
		return f, orfserr.Overflow("record total length overflow")
	}
	f.TotalRecordLength = binary.LittleEndian.Uint64(b[0:8])
	if f.TotalRecordLength != wantTotal {
		return f, orfserr.Torn("record footer length %d != expected %d", f.TotalRecordLength, wantTotal)
	}
	f.RecordCheck = binary.LittleEndian.Uint32(b[8:12])
	if f.RecordCheck != CRC32C(headerAndPayload) {
		return f, orfserr.Torn("record footer checksum mismatch")
	}
	f.EndMarker = binary.LittleEndian.Uint32(b[12:16])
	if f.EndMarker != FooterEndMarker {
		return f, orfserr.Corrupt("record footer end marker mismatch")
	}
	return f, nil
}

// Record is a decoded record: its header plus the opaque payload bytes.
type Record struct {
	Header  Header
	Payload []byte
}

// TypeClass classifies a record type code.
type TypeClass int

const (
	// ClassKnown is a registry type this build understands.
	ClassKnown TypeClass = iota
	// ClassOptionalUnknown is an unknown type with bit 15 set: skippable.
	ClassOptionalUnknown
	// ClassMandatoryUnknown is an unknown type with bit 15 clear: refuse.
	ClassMandatoryUnknown
)

// ClassifyCode classifies a record type code.
func ClassifyCode(code uint16) TypeClass {
	if _, ok := typeName[code]; ok {
		return ClassKnown
	}
	if code&OptionalTypeBit != 0 {
		return ClassOptionalUnknown
	}
	return ClassMandatoryUnknown
}

// Encode renders the full framed record, computing all checks.
func (r *Record) Encode() ([]byte, error) {
	total, err := r.Header.TotalLen()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, total)
	h := r.Header
	h.FormatVersion = FormatVersion
	copy(buf[0:HeaderLen], h.Encode())
	copy(buf[HeaderLen:HeaderLen+len(r.Payload)], r.Payload)
	copy(buf[HeaderLen+len(r.Payload):], EncodeFooter(buf[0:HeaderLen+len(r.Payload)]))
	return buf, nil
}

// TotalLen is the framed length of this record.
func (r *Record) TotalLen() (uint64, error) { return r.Header.TotalLen() }

// Decode parses a record from a buffer that must contain exactly one frame.
func Decode(buf []byte) (*Record, error) {
	h, err := DecodeHeader(buf)
	if err != nil {
		return nil, err
	}
	total, err := h.TotalLen()
	if err != nil {
		return nil, err
	}
	if uint64(len(buf)) < total {
		return nil, orfserr.Torn("record needs %d bytes, have %d", total, len(buf))
	}
	payload := make([]byte, h.PayloadLength)
	copy(payload, buf[HeaderLen:HeaderLen+int(h.PayloadLength)])
	if _, err := DecodeFooter(buf[HeaderLen+int(h.PayloadLength):total], buf[0:HeaderLen+int(h.PayloadLength)], h.PayloadLength); err != nil {
		return nil, err
	}
	return &Record{Header: h, Payload: payload}, nil
}

// ReadFrameAt reads and validates one record starting at off in ra, whose total
// size is fileSize. It returns the record and its framed length. A short read or
// a failing footer yields a torn error; the caller decides whether a torn frame
// in the final segment is an uncommitted tail.
func ReadFrameAt(ra io.ReaderAt, off, fileSize int64) (*Record, int64, error) {
	rec, _, total, err := ReadFrameRawAt(ra, off, fileSize)
	return rec, total, err
}

// ReadFrameRawAt is ReadFrameAt but additionally returns the exact framed bytes,
// which the transaction machine hashes for the commit bracket checksum. Hashing
// the on-disk bytes (rather than a re-encoding) is what makes the bracket check
// independent of any encoder subtlety.
func ReadFrameRawAt(ra io.ReaderAt, off, fileSize int64) (*Record, []byte, int64, error) {
	if off >= fileSize {
		return nil, nil, 0, orfserr.Torn("no bytes at offset %d", off)
	}
	hdrBuf := make([]byte, HeaderLen)
	n, err := ra.ReadAt(hdrBuf, off)
	if err != nil && err != io.EOF {
		return nil, nil, 0, orfserr.IO(err, "read record header")
	}
	if n < HeaderLen {
		return nil, nil, 0, orfserr.Torn("short record header at offset %d", off)
	}
	h, err := DecodeHeader(hdrBuf)
	if err != nil {
		return nil, nil, 0, err
	}
	total, err := h.TotalLen()
	if err != nil {
		return nil, nil, 0, err
	}
	if total > uint64(fileSize-off) {
		return nil, nil, 0, orfserr.Torn("record at offset %d claims %d bytes but only %d remain", off, total, fileSize-off)
	}
	frame := make([]byte, total)
	if _, err := ra.ReadAt(frame, off); err != nil && err != io.EOF {
		return nil, nil, 0, orfserr.IO(err, "read record frame")
	}
	payload := make([]byte, h.PayloadLength)
	copy(payload, frame[HeaderLen:HeaderLen+int(h.PayloadLength)])
	if _, err := DecodeFooter(frame[HeaderLen+int(h.PayloadLength):], frame[0:HeaderLen+int(h.PayloadLength)], h.PayloadLength); err != nil {
		return nil, nil, 0, err
	}
	return &Record{Header: h, Payload: payload}, frame, int64(total), nil
}

// WithIdentity returns a copy of the header with the append-time identity
// stamped. The builder leaves sequence_number and transaction_id at zero; the
// log/sequencer stamps them at append time.
func (r *Record) WithIdentity(seq, txID, inodeID, prevInodeOffset uint64) *Record {
	r.Header.SequenceNumber = seq
	r.Header.TransactionID = txID
	r.Header.InodeID = inodeID
	r.Header.PreviousInodeOffset = prevInodeOffset
	return r
}

func addU64(a, b uint64) (uint64, bool) {
	c := a + b
	return c, c >= a
}
