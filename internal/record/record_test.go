package record

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

func TestHeaderRoundTripAndChecksumCoverage(t *testing.T) {
	h := Header{
		FormatVersion:       FormatVersion,
		RecordType:          TypeDataExtent,
		PayloadLength:       1234,
		SequenceNumber:      7,
		TransactionID:       3,
		InodeID:             42,
		PreviousInodeOffset: 100,
		Flags:               0,
	}
	b := h.Encode()
	if len(b) != HeaderLen {
		t.Fatalf("header len = %d, want %d", len(b), HeaderLen)
	}
	// magic spells "ORFS" on disk.
	if !bytes.Equal(b[0:4], []byte("ORFS")) {
		t.Fatalf("magic bytes = %q, want ORFS", b[0:4])
	}
	// header_check covers exactly [0,56).
	if got := binary.LittleEndian.Uint32(b[56:60]); got != CRC32C(b[0:56]) {
		t.Fatalf("header_check mismatch")
	}
	got, err := DecodeHeader(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != h {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, h)
	}
}

func TestDecodeHeaderClassification(t *testing.T) {
	good := Header{FormatVersion: FormatVersion, RecordType: TypeTxBegin, PayloadLength: 28}.Encode()

	// Too short -> torn.
	if _, err := DecodeHeader(good[:59]); !errors.Is(err, orfserr.ErrTorn) {
		t.Fatalf("short header: want torn, got %v", err)
	}
	// Flipped header_check byte -> torn (checksum first).
	flipped := append([]byte(nil), good...)
	flipped[0] ^= 0xFF
	if _, err := DecodeHeader(flipped); !errors.Is(err, orfserr.ErrTorn) {
		t.Fatalf("bad header crc: want torn, got %v", err)
	}
	// Bad magic with a valid checksum -> corrupt.
	bad := Header{FormatVersion: FormatVersion, RecordType: TypeTxBegin, PayloadLength: 28}.Encode()
	binary.LittleEndian.PutUint32(bad[0:4], 0xDEADBEEF)
	binary.LittleEndian.PutUint32(bad[56:60], CRC32C(bad[0:56]))
	if _, err := DecodeHeader(bad); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("bad magic: want corrupt, got %v", err)
	}
	// Bad header_length with a valid checksum -> corrupt.
	bl := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(bl[8:12], 61)
	binary.LittleEndian.PutUint32(bl[56:60], CRC32C(bl[0:56]))
	if _, err := DecodeHeader(bl); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("bad header_length: want corrupt, got %v", err)
	}
	// Unknown format version with a valid checksum -> compatibility.
	cv := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(cv[4:6], 99)
	binary.LittleEndian.PutUint32(cv[56:60], CRC32C(cv[0:56]))
	if _, err := DecodeHeader(cv); !errors.Is(err, orfserr.ErrCompat) {
		t.Fatalf("bad version: want compat, got %v", err)
	}
	// Oversized payload -> corrupt, before any allocation.
	pl := append([]byte(nil), good...)
	binary.LittleEndian.PutUint64(pl[12:20], MaxPayloadLen+1)
	binary.LittleEndian.PutUint32(pl[56:60], CRC32C(pl[0:56]))
	if _, err := DecodeHeader(pl); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("oversized payload: want corrupt, got %v", err)
	}
}

func TestRecordRoundTripAndFooter(t *testing.T) {
	payload := DataExtent{Offset: 4096, Data: []byte("hello, orfs\n")}.Encode()
	r := &Record{Header: Header{FormatVersion: FormatVersion, RecordType: TypeDataExtent, PayloadLength: uint64(len(payload)), InodeID: 5}, Payload: payload}
	enc, err := r.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(enc) != EnvelopeOverhead+len(payload) {
		t.Fatalf("framed len = %d", len(enc))
	}
	if !bytes.Equal(enc[len(enc)-4:], []byte("END!")) {
		t.Fatalf("end marker bytes = %q", enc[len(enc)-4:])
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	de, err := DecodeDataExtent(got.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if de.Offset != 4096 || string(de.Data) != "hello, orfs\n" {
		t.Fatalf("payload mismatch: %+v", de)
	}

	// Corrupt the end marker: with a valid record check it is corruption.
	badEnd := append([]byte(nil), enc...)
	binary.LittleEndian.PutUint32(badEnd[len(badEnd)-4:], 0)
	if _, err := Decode(badEnd); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("bad end marker: want corrupt, got %v", err)
	}
	// Corrupt a payload byte: the record check now disagrees -> torn.
	badCRC := append([]byte(nil), enc...)
	badCRC[HeaderLen] ^= 0xFF
	if _, err := Decode(badCRC); !errors.Is(err, orfserr.ErrTorn) {
		t.Fatalf("bad record check: want torn, got %v", err)
	}
	// Truncated frame -> torn.
	if _, err := Decode(enc[:len(enc)-1]); !errors.Is(err, orfserr.ErrTorn) {
		t.Fatalf("truncated: want torn, got %v", err)
	}
}

func TestFixedPayloadSizes(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"TX_BEGIN", len(TxBegin{}.Encode()), TxBeginPayloadLen},
		{"TX_COMMIT", len(TxCommit{}.Encode()), TxCommitPayloadLen},
		{"INODE_CREATE", len(InodeCreate{}.Encode()), InodeCreatePayloadLen},
		{"INODE_METADATA", len(InodeMetadata{}.Encode()), InodeMetadataPayloadLen},
		{"PERMISSION_CHANGE", len(PermissionChange{}.Encode()), PermissionChangePayloadLen},
		{"INODE_SETATTR", len(InodeSetattr{}.Encode()), InodeSetattrPayloadLen},
		{"DATA_ZERO_RANGE", len(DataZeroRange{}.Encode()), DataZeroRangePayloadLen},
		{"FILE_RESIZE", len(FileResize{}.Encode()), FileResizePayloadLen},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: len = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestPayloadMagicsOnDisk(t *testing.T) {
	// A few anchor magics checked as literal on-disk byte sequences.
	if !bytes.Equal(TxBegin{}.Encode()[0:4], []byte("TBN1")) {
		t.Error("TX_BEGIN magic")
	}
	if !bytes.Equal(TxCommit{}.Encode()[0:4], []byte("TCM1")) {
		t.Error("TX_COMMIT magic")
	}
	if !bytes.Equal(InodeCreate{}.Encode()[0:4], []byte("ICR1")) {
		t.Error("INODE_CREATE magic")
	}
	if !bytes.Equal((DentryRename{}).Encode()[0:4], []byte("DRN1")) {
		t.Error("DENTRY_RENAME magic")
	}
	if !bytes.Equal((SnapshotRoot{}).Encode()[0:4], []byte("SNR1")) {
		t.Error("SNAPSHOT_ROOT magic")
	}
}

func TestGenesisParse(t *testing.T) {
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	g := Genesis{UUID: uuid, SegmentSize: DefaultSegmentSize}
	enc := EncodeGenesis(g)
	if len(enc) != GenesisPayloadLen {
		t.Fatalf("genesis len = %d, want %d", len(enc), GenesisPayloadLen)
	}
	got, err := ParseGenesis(enc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.UUID != uuid || got.SegmentSize != DefaultSegmentSize || got.FormatVersion != FormatVersion || got.Legacy {
		t.Fatalf("genesis mismatch: %+v", got)
	}

	// Legacy: 24-byte payload, version inferred 0.
	legacy := append([]byte(nil), enc[:24]...)
	lg, err := ParseGenesis(legacy)
	if err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	if lg.FormatVersion != 0 || !lg.Legacy {
		t.Fatalf("legacy inference wrong: %+v", lg)
	}

	// 25 bytes is malformed.
	if _, err := ParseGenesis(enc[:25]); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("25-byte genesis: want corrupt, got %v", err)
	}
	// Too short.
	if _, err := ParseGenesis(enc[:20]); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("short genesis: want corrupt, got %v", err)
	}
	// A higher version is refused, never guessed.
	future := append([]byte(nil), enc...)
	binary.LittleEndian.PutUint16(future[24:26], FormatVersion+5)
	if _, err := ParseGenesis(future); !errors.Is(err, orfserr.ErrUnsupported) {
		t.Fatalf("future genesis: want unsupported, got %v", err)
	}
}

func TestNameValidation(t *testing.T) {
	bad := [][]byte{{}, []byte("."), []byte(".."), []byte("a/b"), {0}, bytes.Repeat([]byte("x"), 256)}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("name %q should be rejected", n)
		}
	}
	for _, n := range [][]byte{[]byte("a"), []byte("hello.txt"), bytes.Repeat([]byte("x"), 255)} {
		if err := ValidateName(n); err != nil {
			t.Errorf("name %q should be accepted: %v", n, err)
		}
	}
}

func TestVariablePayloadRoundTrips(t *testing.T) {
	xa := XattrSet{InodeID: 3, Name: []byte("user.orfs"), Value: []byte("v1")}
	got, err := DecodeXattrSet(xa.Encode())
	if err != nil || got.InodeID != 3 || string(got.Name) != "user.orfs" || string(got.Value) != "v1" {
		t.Fatalf("xattr set round trip: %+v err=%v", got, err)
	}
	sl := SymlinkCreate{InodeID: 9, Target: []byte("../target")}
	sgot, err := DecodeSymlinkCreate(sl.Encode())
	if err != nil || sgot.InodeID != 9 || string(sgot.Target) != "../target" {
		t.Fatalf("symlink round trip: %+v err=%v", sgot, err)
	}
	// Empty symlink target is refused on decode.
	empty := append([]byte(nil), sl.Encode()...)
	binary.LittleEndian.PutUint32(empty[44:48], 0)
	binary.LittleEndian.PutUint32(empty[len(empty)-4:], CRC32C(empty[:len(empty)-4]))
	if _, err := DecodeSymlinkCreate(empty); err == nil {
		t.Fatal("empty symlink target should be refused")
	}
}

func TestTypeClassification(t *testing.T) {
	if ClassifyCode(TypeTxCommit) != ClassKnown {
		t.Error("TX_COMMIT should be known")
	}
	if ClassifyCode(0x9000) != ClassOptionalUnknown {
		t.Error("0x9000 should be optional-unknown")
	}
	if ClassifyCode(0x0100) != ClassMandatoryUnknown {
		t.Error("0x0100 should be mandatory-unknown")
	}
	if TypeName(0x9000) != "OPTIONAL_0x9000" {
		t.Errorf("optional name = %q", TypeName(0x9000))
	}
	if TypeName(0x0100) != "UNKNOWN_0x0100" {
		t.Errorf("unknown name = %q", TypeName(0x0100))
	}
}
