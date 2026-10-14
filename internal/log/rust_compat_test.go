package log_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/fixture"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// TestScanAcceptsUnsetTxBeginRecordCount pins the cross-language rule for
// TX_BEGIN.expected_record_count: the Rust reference writes 0 from its low-level
// log API (the value its frozen fixtures carry) and treats 0 as "no expectation"
// when verifying. The Go reader must do the same, or it rejects a valid
// Rust-written volume. A *nonzero* wrong count must still be refused.
func TestScanAcceptsUnsetTxBeginRecordCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   uint32
		wantErr bool
	}{
		{"zero means unset", 0, false},
		{"wrong nonzero is refused", 99, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seg := patchGenesisBeginCount(t, fixture.Build(fixture.Version1)["or.log/0000000000000000.seg"], tc.count)
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, log.DirName), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, log.DirName, "0000000000000000.seg")
			if err := os.WriteFile(path, seg, 0o644); err != nil {
				t.Fatal(err)
			}
			scan, err := log.ScanLog(filepath.Join(dir, log.DirName))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("scan accepted expected_record_count %d; want refusal", tc.count)
				}
				return
			}
			if err != nil {
				t.Fatalf("scan rejected a valid log with expected_record_count=0: %v", err)
			}
			if scan.Tail != nil {
				t.Fatalf("unexpected tail: %+v", scan.Tail)
			}
			if scan.CommittedRecordCount != 8 {
				t.Fatalf("committed records = %d, want 8", scan.CommittedRecordCount)
			}
		})
	}
}

// patchGenesisBeginCount rewrites the TX_BEGIN.expected_record_count of the
// first transaction in a frozen segment to count, recomputing the begin payload
// frame and the following TX_COMMIT bracket checksum exactly as the format
// requires (BLAKE3 over TX_BEGIN..last data frame).
func patchGenesisBeginCount(t *testing.T, seg []byte, count uint32) []byte {
	t.Helper()
	ra := bytes.NewReader(seg)
	var frames [][]byte
	var off int64
	for off < int64(len(seg)) {
		_, raw, total, err := record.ReadFrameRawAt(ra, off, int64(len(seg)))
		if err != nil {
			t.Fatalf("read frame at %d: %v", off, err)
		}
		frames = append(frames, append([]byte(nil), raw...))
		off += total
	}
	begin := -1
	commit := -1
	var beginRec *record.Record
	var commitRec *record.Record
	for i, f := range frames {
		rec, err := record.Decode(f)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		switch rec.Header.RecordType {
		case record.TypeTxBegin:
			if begin < 0 {
				begin, beginRec = i, rec
			}
		case record.TypeTxCommit:
			if begin >= 0 && commit < 0 {
				commit, commitRec = i, rec
			}
		}
	}
	if begin < 0 || commit < 0 || commit <= begin {
		t.Fatal("fixture has no TX_BEGIN..TX_COMMIT bracket")
	}

	tb, err := record.DecodeTxBegin(beginRec.Payload)
	if err != nil {
		t.Fatalf("decode TX_BEGIN: %v", err)
	}
	tb.ExpectedRecordCount = count
	beginRec.Payload = tb.Encode()
	nf, err := beginRec.Encode()
	if err != nil {
		t.Fatalf("re-encode TX_BEGIN: %v", err)
	}
	frames[begin] = nf

	// The bracket checksum covers TX_BEGIN..last data record. Recompute it over
	// the (possibly changed) frames.
	h := blake3.New(record.BracketChecksumLen, nil)
	for i := begin; i < commit; i++ {
		h.Write(frames[i])
	}
	var sum [record.BracketChecksumLen]byte
	copy(sum[:], h.Sum(nil))

	tc, err := record.DecodeTxCommit(commitRec.Payload)
	if err != nil {
		t.Fatalf("decode TX_COMMIT: %v", err)
	}
	tc.BracketChecksum = sum
	commitRec.Payload = tc.Encode()
	cf, err := commitRec.Encode()
	if err != nil {
		t.Fatalf("re-encode TX_COMMIT: %v", err)
	}
	frames[commit] = cf

	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}
