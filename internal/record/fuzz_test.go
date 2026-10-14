package record

import (
	"bytes"
	"testing"
)

// validFrame returns a well-formed framed record for fuzz seeds.
func validFrame() []byte {
	payload := DataExtent{Offset: 4096, Data: []byte("hello, orfs\n")}.Encode()
	r := &Record{Header: Header{FormatVersion: FormatVersion, RecordType: TypeDataExtent, PayloadLength: uint64(len(payload)), InodeID: 5}, Payload: payload}
	b, err := r.Encode()
	if err != nil {
		panic(err)
	}
	return b
}

// FuzzDecodeFrame feeds arbitrary bytes to the frame reader. It must never
// panic, and any record it accepts must be internally consistent.
func FuzzDecodeFrame(f *testing.F) {
	f.Add(validFrame())
	f.Add([]byte("ORFS"))
	f.Add([]byte{})
	f.Add(validFrame()[:40])
	f.Fuzz(func(t *testing.T, data []byte) {
		rec, n, err := ReadFrameAt(bytes.NewReader(data), 0, int64(len(data)))
		if err != nil {
			return
		}
		if n <= 0 || n > int64(len(data)) {
			t.Fatalf("accepted frame length %d out of range for %d bytes", n, len(data))
		}
		if int64(HeaderLen)+int64(rec.Header.PayloadLength)+int64(FooterLen) != n {
			t.Fatalf("accepted frame length %d inconsistent with payload %d", n, rec.Header.PayloadLength)
		}
	})
}

// FuzzDecodePayloads feeds arbitrary bytes to every typed payload decoder. None
// may panic; each either returns a value or a typed error.
func FuzzDecodePayloads(f *testing.F) {
	for _, p := range [][]byte{
		TxBegin{}.Encode(), TxCommit{}.Encode(), TxAbort{ReasonCode: AbortUserRequested}.Encode(),
		InodeCreate{}.Encode(), InodeMetadata{}.Encode(), PermissionChange{}.Encode(),
		InodeSetattr{}.Encode(), DataZeroRange{}.Encode(), FileResize{}.Encode(),
		XattrSet{Name: []byte("user.a"), Value: []byte("v")}.Encode(),
		DentryRename{Flags: RenameMove, OldName: []byte("a"), NewName: []byte("b")}.Encode(),
	} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeTxBegin(data)
		_, _ = DecodeTxCommit(data)
		_, _ = DecodeTxAbort(data)
		_, _ = DecodeInodeCreate(data)
		_, _ = DecodeSymlinkCreate(data)
		_, _ = DecodeInodeMetadata(data)
		_, _ = DecodePermissionChange(data)
		_, _ = DecodeInodeSetattr(data)
		_, _ = DecodeXattrSet(data)
		_, _ = DecodeXattrRemove(data)
		_, _ = DecodeDentryAdd(data)
		_, _ = DecodeDentryRemove(data)
		_, _ = DecodeDentryRename(data)
		_, _ = DecodeDataExtent(data)
		_, _ = DecodeDataZeroRange(data)
		_, _ = DecodeFileResize(data)
		_, _ = DecodeSnapshotRoot(data)
		_, _ = DecodeIndexCheckpoint(data)
	})
}
