package record

import (
	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

// GenesisPayloadLen is the length of the current (versioned) genesis payload.
const GenesisPayloadLen = 26

// LegacyGenesisPayloadLen is the length of the pre-versioning genesis payload.
const LegacyGenesisPayloadLen = 24

// Genesis is the authoritative-log identity carried by the first committed
// record. It carries the log format version explicitly, so a reader decides
// compatibility from the log itself rather than from a rebuildable sidecar.
type Genesis struct {
	UUID          [16]byte
	SegmentSize   uint64
	FormatVersion uint16
	// Legacy is true when the payload was exactly 24 bytes and the version was
	// inferred as 0 (the field was absent).
	Legacy bool
}

// EncodeGenesis renders the current (versioned, 26-byte) genesis payload.
func EncodeGenesis(g Genesis) []byte {
	b := make([]byte, GenesisPayloadLen)
	copy(b[0:16], g.UUID[:])
	putU64(b[16:24], g.SegmentSize)
	putU16(b[24:26], FormatVersion)
	return b
}

// ParseGenesis decodes a genesis payload using the documented algorithm:
// len < 24 is corrupt; len == 24 is legacy (version 0); len == 25 is malformed;
// len >= 26 carries the little-endian version at offset 24. A version newer than
// this build supports is refused (never guessed) at open.
func ParseGenesis(payload []byte) (Genesis, error) {
	var g Genesis
	switch {
	case len(payload) < LegacyGenesisPayloadLen:
		return g, orfserr.Corrupt("genesis payload too short: %d bytes", len(payload))
	case len(payload) == LegacyGenesisPayloadLen:
		g.FormatVersion = MinFormatVersion
		g.Legacy = true
	case len(payload) == LegacyGenesisPayloadLen+1:
		return g, orfserr.Corrupt("genesis payload length 25 is malformed")
	default:
		g.FormatVersion = getU16(payload[24:26])
	}
	copy(g.UUID[:], payload[0:16])
	g.SegmentSize = getU64(payload[16:24])
	if g.SegmentSize == 0 {
		return g, orfserr.Corrupt("genesis segment_size is zero")
	}
	if g.FormatVersion > FormatVersion {
		return g, orfserr.UnsupportedFormat(g.FormatVersion, FormatVersion)
	}
	return g, nil
}

// TxBegin is the fixed 28-byte TX_BEGIN payload.
type TxBegin struct {
	TransactionID       uint64
	ExpectedByteLength  uint64
	ExpectedRecordCount uint32
}

// Encode renders the TX_BEGIN payload.
func (t TxBegin) Encode() []byte {
	b := make([]byte, TxBeginPayloadLen)
	putU32(b[0:4], MagicTxBegin)
	putU64(b[4:12], t.TransactionID)
	putU64(b[12:20], t.ExpectedByteLength)
	putU32(b[20:24], t.ExpectedRecordCount)
	putU32(b[24:28], CRC32C(b[0:24]))
	return b
}

// DecodeTxBegin parses a TX_BEGIN payload.
func DecodeTxBegin(b []byte) (TxBegin, error) {
	var t TxBegin
	if err := checkFixed(b, MagicTxBegin, TxBeginPayloadLen, "TX_BEGIN"); err != nil {
		return t, err
	}
	t.TransactionID = getU64(b[4:12])
	t.ExpectedByteLength = getU64(b[12:20])
	t.ExpectedRecordCount = getU32(b[20:24])
	return t, nil
}

// TxCommit is the fixed 72-byte TX_COMMIT payload.
type TxCommit struct {
	TransactionID        uint64
	RecordCount          uint64
	BracketedByteLength  uint64
	CommitSequenceNumber uint64
	BracketChecksum      [BracketChecksumLen]byte
}

// Encode renders the TX_COMMIT payload.
func (t TxCommit) Encode() []byte {
	b := make([]byte, TxCommitPayloadLen)
	putU32(b[0:4], MagicTxCommit)
	putU64(b[4:12], t.TransactionID)
	putU64(b[12:20], t.RecordCount)
	putU64(b[20:28], t.BracketedByteLength)
	putU64(b[28:36], t.CommitSequenceNumber)
	copy(b[36:68], t.BracketChecksum[:])
	putU32(b[68:72], CRC32C(b[0:68]))
	return b
}

// DecodeTxCommit parses a TX_COMMIT payload.
func DecodeTxCommit(b []byte) (TxCommit, error) {
	var t TxCommit
	if err := checkFixed(b, MagicTxCommit, TxCommitPayloadLen, "TX_COMMIT"); err != nil {
		return t, err
	}
	t.TransactionID = getU64(b[4:12])
	t.RecordCount = getU64(b[12:20])
	t.BracketedByteLength = getU64(b[20:28])
	t.CommitSequenceNumber = getU64(b[28:36])
	copy(t.BracketChecksum[:], b[36:68])
	return t, nil
}

// TxAbort is the variable TX_ABORT payload.
type TxAbort struct {
	TransactionID uint64
	ReasonCode    uint16
	Message       []byte
}

// Encode renders the TX_ABORT payload.
func (t TxAbort) Encode() []byte {
	b := make([]byte, TxAbortPrefixLen+len(t.Message))
	putU32(b[0:4], MagicTxAbort)
	putU64(b[4:12], t.TransactionID)
	putU16(b[12:14], t.ReasonCode)
	putU16(b[14:16], 0)
	putU32(b[16:20], uint32(len(t.Message)))
	copy(b[20:], t.Message)
	return appendCRC(b)
}

// DecodeTxAbort parses a TX_ABORT payload.
func DecodeTxAbort(b []byte) (TxAbort, error) {
	var t TxAbort
	if len(b) < TxAbortPrefixLen+4 {
		return t, orfserr.Corrupt("TX_ABORT payload too short")
	}
	if getU32(b[0:4]) != MagicTxAbort {
		return t, orfserr.Corrupt("TX_ABORT magic mismatch")
	}
	t.TransactionID = getU64(b[4:12])
	t.ReasonCode = getU16(b[12:14])
	if t.ReasonCode < AbortUserRequested || t.ReasonCode > AbortDurabilityFail {
		return t, orfserr.Corrupt("TX_ABORT unknown reason code %d", t.ReasonCode)
	}
	msgLen := int(getU32(b[16:20]))
	if msgLen > TxAbortMaxMessage {
		return t, orfserr.Corrupt("TX_ABORT message length %d exceeds %d", msgLen, TxAbortMaxMessage)
	}
	if TxAbortPrefixLen+msgLen+4 != len(b) {
		return t, orfserr.Corrupt("TX_ABORT length %d != %d", len(b), TxAbortPrefixLen+msgLen+4)
	}
	if !payloadCRCValid(b) {
		return t, orfserr.Corrupt("TX_ABORT checksum mismatch")
	}
	msg := make([]byte, msgLen)
	copy(msg, b[20:20+msgLen])
	if !validUTF8(msg) {
		return t, orfserr.Corrupt("TX_ABORT message is not valid UTF-8")
	}
	t.Message = msg
	return t, nil
}
