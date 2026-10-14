// Command orfsoptprobe constructs a tiny, deterministic OrFS volume whose last
// committed transaction contains an extra record of a caller-chosen type code.
// It exists to answer one empirical question for the cross-language court: how
// does the Rust reference behave when a transaction carries a record whose type
// is *outside* its registry — specifically, when bit 15 (0x8000, the "optional"
// bit) is set versus clear?
//
// The base volume is the frozen `current_v1` fixture shape (VOLUME_GENESIS,
// then create hello.txt; write "hello, orfs\n"). The probe appends a third
// transaction:
//
//	TX_BEGIN
//	PERMISSION_CHANGE  (chmod hello.txt 0640 -- a type the Rust registry knows)
//	<code>             (a CTIME_UPDATE-payload record re-typed to <code>)
//	TX_COMMIT
//
// The bracket counts, byte length, and BLAKE3 checksum are computed exactly the
// way this repo computes them for every publish, so the only variable is the
// record's type code.
//
// Usage:
//
//	orfsoptprobe <outdir> <code-hex>
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/fixture"
	"github.com/infinityabundance/orfs-go/internal/record"
)

const helloInode uint64 = 2

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: orfsoptprobe <outdir> <code-hex>")
		os.Exit(2)
	}
	out := os.Args[1]
	raw := strings.TrimPrefix(strings.TrimPrefix(os.Args[2], "0x"), "0X")
	code64, err := strconv.ParseUint(raw, 16, 16)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsoptprobe: bad type code %q: %v\n", os.Args[2], err)
		os.Exit(2)
	}
	if err := build(out, uint16(code64)); err != nil {
		fmt.Fprintf(os.Stderr, "orfsoptprobe: %v\n", err)
		os.Exit(1)
	}
}

func build(out string, code uint16) error {
	base := fixture.Build(fixture.Version1)
	seg := base["or.log/0000000000000000.seg"]

	nextSeq, maxTx, err := decodeBoundary(seg)
	if err != nil {
		return err
	}

	// The appended transaction mirrors a Go chmod: a known PERMISSION_CHANGE
	// followed by the ctime-only record re-typed to the caller's code.
	pc := record.PermissionChange{InodeID: helloInode, Mode: u32ptr(0o640)}.Encode()
	ctime := record.CtimeUpdate{InodeID: helloInode, Ctime: record.Timespec{Secs: 1_700_000_100}}.Encode()

	appended, err := appendTxn(nextSeq, maxTx+1, []txnRec{
		{code: record.TypePermissionChange, inode: 0, payload: pc},
		{code: code, inode: helloInode, payload: ctime},
	})
	if err != nil {
		return err
	}
	full := append(append([]byte(nil), seg...), appended...)

	if err := os.MkdirAll(filepath.Join(out, "or.log"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "volume.meta"), base["volume.meta"], 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "or.log", "0000000000000000.seg"), full, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: type=0x%04X optional=%v committed_bytes=%d blake3=%s\n",
		out, code, code&record.OptionalTypeBit != 0, len(full), blake3Hex(full))
	return nil
}

func u32ptr(v uint32) *uint32 { return &v }

type txnRec struct {
	code    uint16
	inode   uint64
	payload []byte
}

// decodeBoundary decodes every complete frame in seg and returns the next
// sequence number (the frame count) and the greatest transaction id.
func decodeBoundary(seg []byte) (nextSeq, maxTx uint64, err error) {
	ra := bytes.NewReader(seg)
	var off int64
	for int(off) < len(seg) {
		rec, total, err := record.ReadFrameAt(ra, off, int64(len(seg)))
		if err != nil {
			return 0, 0, fmt.Errorf("decode frame at %d: %w", off, err)
		}
		if rec.Header.TransactionID > maxTx {
			maxTx = rec.Header.TransactionID
		}
		off += total
		nextSeq++
	}
	return nextSeq, maxTx, nil
}

// appendTxn frames TX_BEGIN, the data records, and a TX_COMMIT whose bracket
// covers exactly TX_BEGIN..last data record. It is the same accounting the log
// uses for every real publish.
func appendTxn(seq, txID uint64, data []txnRec) ([]byte, error) {
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
		PayloadLength: record.TxBeginPayloadLen, SequenceNumber: seq, TransactionID: txID,
	}, Payload: record.TxBegin{
		TransactionID: txID, ExpectedByteLength: bracketBytes, ExpectedRecordCount: uint32(len(data) + 1),
	}.Encode()}
	beginFrame, err := begin.Encode()
	if err != nil {
		return nil, err
	}
	seq++

	hash := blake3.New(record.BracketChecksumLen, nil)
	hash.Write(beginFrame)
	out := append([]byte(nil), beginFrame...)
	for i, r := range data {
		rec := &record.Record{Header: record.Header{
			FormatVersion: record.FormatVersion, RecordType: r.code,
			PayloadLength: uint64(len(r.payload)), SequenceNumber: seq, TransactionID: txID, InodeID: r.inode,
		}, Payload: r.payload}
		frame, err := rec.Encode()
		if err != nil {
			return nil, err
		}
		if uint64(len(frame)) != dataTotals[i] {
			return nil, fmt.Errorf("bracket accounting mismatch for type 0x%04X", r.code)
		}
		hash.Write(frame)
		out = append(out, frame...)
		seq++
	}

	var bracket [record.BracketChecksumLen]byte
	copy(bracket[:], hash.Sum(nil))
	commit := &record.Record{Header: record.Header{
		FormatVersion: record.FormatVersion, RecordType: record.TypeTxCommit,
		PayloadLength: record.TxCommitPayloadLen, SequenceNumber: seq, TransactionID: txID,
	}, Payload: record.TxCommit{
		TransactionID: txID, RecordCount: uint64(len(data) + 1),
		BracketedByteLength: bracketBytes, CommitSequenceNumber: seq, BracketChecksum: bracket,
	}.Encode()}
	commitFrame, err := commit.Encode()
	if err != nil {
		return nil, err
	}
	return append(out, commitFrame...), nil
}

func blake3Hex(b []byte) string {
	// Kept dependency-light: the court only needs a stable identifier.
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}
