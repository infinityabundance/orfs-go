package fixture_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/fixture"
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

const fixturesRoot = "../../tests/fixtures"

// TestGenerateFixtures (re)writes the frozen fixtures. It is opt-in so ordinary
// runs never mutate them.
func TestGenerateFixtures(t *testing.T) {
	if os.Getenv("ORFS_GENERATE_FIXTURES") == "" {
		t.Skip("set ORFS_GENERATE_FIXTURES=1 to (re)write the frozen fixtures")
	}
	for _, tc := range []struct {
		dir     string
		version uint16
	}{
		{"legacy_v0", fixture.Version0},
		{"current_v1", fixture.Version1},
	} {
		base := filepath.Join(fixturesRoot, tc.dir)
		if err := os.RemoveAll(base); err != nil {
			t.Fatal(err)
		}
		for rel, data := range fixture.Build(tc.version) {
			path := filepath.Join(base, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("wrote %s", base)
	}
}

func TestFrozenFixturesOpenAndReplay(t *testing.T) {
	cases := []struct {
		dir           string
		version       uint16
		wantUUID      [16]byte
		wantRootDelta uint64 // 0 for legacy, 2 for versioned genesis
	}{
		{"legacy_v0", fixture.Version0, fixture.FixedUUID, 0},
		{"current_v1", fixture.Version1, fixture.FixedUUID, 2},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			base := filepath.Join(fixturesRoot, tc.dir)
			logDir := filepath.Join(base, "or.log")

			scan, err := log.ScanLog(logDir)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if scan.Genesis.FormatVersion != tc.version {
				t.Fatalf("format version = %d, want %d", scan.Genesis.FormatVersion, tc.version)
			}
			if scan.Genesis.UUID != tc.wantUUID {
				t.Fatalf("uuid mismatch")
			}
			if scan.Tail != nil {
				t.Fatalf("unexpected tail: %+v", scan.Tail)
			}
			if scan.CommittedRecordCount != 8 { // genesis + 2 txns of (begin+..+commit)
				t.Fatalf("committed records = %d, want 8", scan.CommittedRecordCount)
			}

			// Replay and check the logical content.
			idx, err := index.Replay(logDir, scan.Extents, scan.CommittedRoot)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			in, ok := idx.Lookup(record.RootInode, "hello.txt")
			if !ok {
				t.Fatal("hello.txt missing after replay")
			}
			if in.Size != 12 {
				t.Fatalf("hello.txt size = %d, want 12", in.Size)
			}

			// Byte-identity: the frozen bytes must equal a fresh build.
			fresh := fixture.Build(tc.version)
			for rel, want := range fresh {
				got, err := os.ReadFile(filepath.Join(base, rel))
				if err != nil {
					t.Fatalf("read %s: %v", rel, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s is not byte-identical to a fresh build", rel)
				}
			}
		})
	}
}

func TestVersionedGenesisIsExactlyTwoBytesLarger(t *testing.T) {
	v0 := fixture.Build(fixture.Version0)["or.log/0000000000000000.seg"]
	v1 := fixture.Build(fixture.Version1)["or.log/0000000000000000.seg"]
	if len(v1)-len(v0) != 2 {
		t.Fatalf("current_v1 segment is %d bytes larger than legacy_v0, want 2", len(v1)-len(v0))
	}
	// Everything after the genesis record must be byte-identical.
	if !bytes.Equal(v0[record.EnvelopeOverhead+record.LegacyGenesisPayloadLen:],
		v1[record.EnvelopeOverhead+record.GenesisPayloadLen:]) {
		t.Fatal("transaction bytes differ between versions")
	}
}

func TestFutureVersionRefused(t *testing.T) {
	// Take the current fixture and bump the genesis format version past this
	// build's; the reader must refuse it, never guess.
	seg := append([]byte(nil), fixture.Build(fixture.Version1)["or.log/0000000000000000.seg"]...)
	// Genesis payload begins at offset HeaderLen; format_version is at +24.
	pv := binary.LittleEndian.Uint16(seg[record.HeaderLen+24 : record.HeaderLen+26])
	binary.LittleEndian.PutUint16(seg[record.HeaderLen+24:record.HeaderLen+26], pv+5)
	// Recompute the header check (payload length unchanged).
	hdr := seg[:record.HeaderLen]
	binary.LittleEndian.PutUint32(hdr[56:60], record.CRC32C(hdr[0:56]))
	// Recompute the record footer check (covers header || payload).
	payloadLen := int(binary.LittleEndian.Uint64(hdr[12:20]))
	ftrOff := record.HeaderLen + payloadLen
	binary.LittleEndian.PutUint32(seg[ftrOff+8:ftrOff+12], record.CRC32C(seg[:ftrOff]))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "or.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "or.log", "0000000000000000.seg"), seg, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := log.ScanLog(filepath.Join(dir, "or.log"))
	if !errors.Is(err, orfserr.ErrUnsupported) {
		t.Fatalf("want unsupported-version refusal, got %v", err)
	}
}
