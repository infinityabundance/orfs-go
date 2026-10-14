// Package fixture builds the deterministic, frozen mini-volumes used by the
// format-compatibility court.
//
// A fixture is a minimal but semantically valid volume: volume.meta plus a
// single or.log segment holding a VOLUME_GENESIS followed by two committed
// transactions (create hello.txt; write "hello, orfs\n"). Only the committed
// bytes are included; the derived sidecars (index/, .or_bloom/, watermark) are
// deliberately absent because they are rebuildable, not part of the frozen
// identity.
package fixture

import (
	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/record"
)

// Version0 is the pre-versioning log format: a 24-byte genesis, version
// inferred as 0.
const Version0 uint16 = 0

// Version1 is the current log format: a 26-byte genesis carrying format_version.
const Version1 uint16 = 1

// FixedUUID is the fixture volume identity.
var FixedUUID = [16]byte{
	0x4f, 0x52, 0x46, 0x53, 0x2d, 0x47, 0x4f, 0x2d,
	0x66, 0x69, 0x78, 0x74, 0x75, 0x72, 0x65, 0x31,
}

// Files is a fixture's relative-path -> bytes map.
type Files map[string][]byte

// Build returns the frozen files for a log format version.
func Build(version uint16) Files {
	seg := buildSegment(version)
	return Files{
		"volume.meta":                 metaBytes(version),
		"or.log/0000000000000000.seg": seg,
	}
}

func metaBytes(version uint16) []byte {
	b := make([]byte, 44)
	putU32(b[0:4], record.VolumeMetaMagic)
	putU16(b[4:6], version)
	putU16(b[6:8], 0)
	copy(b[8:24], FixedUUID[:])
	putU64(b[24:32], 1_700_000_000) // fixed creation time
	putU64(b[32:40], record.DefaultSegmentSize)
	putU32(b[40:44], record.CRC32C(b[0:40]))
	return b
}

func buildSegment(version uint16) []byte {
	var genesisPayload []byte
	if version == Version0 {
		genesisPayload = make([]byte, record.LegacyGenesisPayloadLen)
		copy(genesisPayload[0:16], FixedUUID[:])
		putU64(genesisPayload[16:24], record.DefaultSegmentSize)
	} else {
		genesisPayload = record.EncodeGenesis(record.Genesis{UUID: FixedUUID, SegmentSize: record.DefaultSegmentSize})
	}

	var out []byte
	seq := uint64(0)
	appendRec := func(code uint16, inode uint64, payload []byte, txID uint64) {
		rec := &record.Record{
			Header: record.Header{
				FormatVersion:  record.FormatVersion,
				RecordType:     code,
				PayloadLength:  uint64(len(payload)),
				SequenceNumber: seq,
				TransactionID:  txID,
				InodeID:        inode,
			},
			Payload: payload,
		}
		frame, err := rec.Encode()
		if err != nil {
			panic(err)
		}
		out = append(out, frame...)
		seq++
	}

	// Genesis (standalone, transaction id 0).
	appendRec(record.TypeVolumeGenesis, 0, genesisPayload, 0)

	// Transaction 1: create hello.txt.
	appendTxn(&out, &seq, 1, []txnRecord{
		{code: record.TypeInodeCreate, inode: 0, payload: record.InodeCreate{
			InodeID: 2, Generation: 1, Kind: record.KindFile, Mode: 0o644,
			UID: 0, GID: 0, Nlink: 1, Size: 0,
		}.Encode()},
		{code: record.TypeDentryAdd, inode: 0, payload: record.EncodeDentryAdd(record.DentryAdd{
			Parent: record.RootInode, Child: 2, Name: []byte("hello.txt"),
		})},
	})

	// Transaction 2: write "hello, orfs\n".
	appendTxn(&out, &seq, 2, []txnRecord{
		{code: record.TypeDataExtent, inode: 2, payload: record.DataExtent{
			Offset: 0, Data: []byte("hello, orfs\n"),
		}.Encode()},
	})
	return out
}

type txnRecord struct {
	code    uint16
	inode   uint64
	payload []byte
}

func appendTxn(out *[]byte, seq *uint64, txID uint64, data []txnRecord) {
	// Bracket accounting: the bracket is begin + data records.
	beginTotal := uint64(record.EnvelopeOverhead + record.TxBeginPayloadLen)
	bracketBytes := beginTotal
	dataTotals := make([]uint64, len(data))
	for i, r := range data {
		t, _ := (&record.Record{Header: record.Header{PayloadLength: uint64(len(r.payload))}}).Header.TotalLen()
		dataTotals[i] = t
		bracketBytes += t
	}

	begin := &record.Record{Header: record.Header{
		FormatVersion: record.FormatVersion, RecordType: record.TypeTxBegin,
		PayloadLength: record.TxBeginPayloadLen, SequenceNumber: *seq, TransactionID: txID,
	}, Payload: record.TxBegin{
		TransactionID: txID, ExpectedByteLength: bracketBytes, ExpectedRecordCount: uint32(len(data) + 1),
	}.Encode()}
	beginFrame, _ := begin.Encode()
	*seq = *seq + 1

	hash := blake3.New(record.BracketChecksumLen, nil)
	hash.Write(beginFrame)
	frames := [][]byte{beginFrame}
	for i, r := range data {
		rec := &record.Record{Header: record.Header{
			FormatVersion: record.FormatVersion, RecordType: r.code,
			PayloadLength: uint64(len(r.payload)), SequenceNumber: *seq, TransactionID: txID, InodeID: r.inode,
		}, Payload: r.payload}
		frame, err := rec.Encode()
		if err != nil {
			panic(err)
		}
		if uint64(len(frame)) != dataTotals[i] {
			panic("fixture bracket accounting mismatch")
		}
		hash.Write(frame)
		frames = append(frames, frame)
		*seq = *seq + 1
	}

	commitSeq := *seq
	var bracket [record.BracketChecksumLen]byte
	copy(bracket[:], hash.Sum(nil))
	commit := &record.Record{Header: record.Header{
		FormatVersion: record.FormatVersion, RecordType: record.TypeTxCommit,
		PayloadLength: record.TxCommitPayloadLen, SequenceNumber: commitSeq, TransactionID: txID,
	}, Payload: record.TxCommit{
		TransactionID: txID, RecordCount: uint64(len(data) + 1),
		BracketedByteLength: bracketBytes, CommitSequenceNumber: commitSeq, BracketChecksum: bracket,
	}.Encode()}
	commitFrame, _ := commit.Encode()
	frames = append(frames, commitFrame)
	*seq = *seq + 1

	for _, f := range frames {
		*out = append(*out, f...)
	}
}

func putU16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
