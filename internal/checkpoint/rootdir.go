// Package checkpoint implements checkpoint-bounded recovery: discovering and
// validating an INDEX_CHECKPOINT record in the committed prefix, replaying only
// the suffix after it, and maintaining the derived committed-root directory that
// lets discovery avoid a whole-log scan.
//
// A checkpoint is an accelerator, never authority. Discovery is derived and
// rebuildable; a missing, corrupt, foreign, or over-claiming directory is
// distrusted and the authoritative log is scanned. A checkpoint that is torn,
// truncated, non-boundary, or self-digest-failing is discarded (or, under the
// Refuse policy, reported) and the next-newest valid checkpoint is tried; when
// none validates the index is rebuilt by a full replay. The reconstruction is
// always byte-identical to a from-scratch replay over the same records.
package checkpoint

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// DirName is the derived directory that lives beside or.log/.
const DirName = "index"

// DirectoryFileName is the committed-root directory file within DirName.
const DirectoryFileName = "committed-root.bin"

const (
	directoryMagic   uint32 = 0x3144524F // "ORD1"
	directoryVersion uint32 = 1
	directoryMinLen         = 4 + 4 + 16 + 8 + 4 + 4 // magic..count + crc
)

// Directory is the derived committed-root directory: the committed root a scan
// covered and the ascending offsets of the INDEX_CHECKPOINT records it found in
// [0, ScannedRoot).
type Directory struct {
	ScannedRoot uint64
	Offsets     []uint64
}

// DirectoryPath is the on-disk path of the derived directory file.
func DirectoryPath(backing string) string {
	return filepath.Join(backing, DirName, DirectoryFileName)
}

// ReadDirectory reads and validates the derived committed-root directory. A
// missing, corrupt, wrong-version, or foreign-uuid directory is distrusted and
// reported with ok=false — it is derived, so distrust is a fallback, never an
// error. Only an unexpected I/O failure returns a non-nil error.
func ReadDirectory(backing string, uuid [16]byte) (*Directory, bool, error) {
	b, err := os.ReadFile(DirectoryPath(backing))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, orfserr.IO(err, "read committed-root directory")
	}
	if len(b) < directoryMinLen {
		return nil, false, nil
	}
	if binary.LittleEndian.Uint32(b[0:4]) != directoryMagic {
		return nil, false, nil
	}
	if binary.LittleEndian.Uint32(b[4:8]) != directoryVersion {
		return nil, false, nil
	}
	if binary.LittleEndian.Uint32(b[len(b)-4:]) != record.CRC32C(b[:len(b)-4]) {
		return nil, false, nil
	}
	var got [16]byte
	copy(got[:], b[8:24])
	if got != uuid {
		return nil, false, nil // foreign volume identity
	}
	d := &Directory{ScannedRoot: binary.LittleEndian.Uint64(b[24:32])}
	count := binary.LittleEndian.Uint32(b[32:36])
	if directoryMinLen+int(count)*8 != len(b) {
		return nil, false, nil
	}
	var prev uint64
	for i := uint32(0); i < count; i++ {
		off := binary.LittleEndian.Uint64(b[36+int(i)*8:])
		if i > 0 && off <= prev {
			return nil, false, nil // not strictly ascending: a hole or reorder
		}
		if off >= d.ScannedRoot {
			return nil, false, nil // an offset beyond the covered prefix
		}
		prev = off
		d.Offsets = append(d.Offsets, off)
	}
	return d, true, nil
}

// WriteDirectory atomically writes the derived committed-root directory (tmp ->
// fsync -> rename -> dir fsync). A failure leaves the previous directory intact
// and changes nothing authoritative.
func WriteDirectory(backing string, uuid [16]byte, d Directory) error {
	dir := filepath.Join(backing, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return orfserr.IO(err, "create derived directory %s", dir)
	}
	b := make([]byte, 0, directoryMinLen+len(d.Offsets)*8)
	b = binary.LittleEndian.AppendUint32(b, directoryMagic)
	b = binary.LittleEndian.AppendUint32(b, directoryVersion)
	b = append(b, uuid[:]...)
	b = binary.LittleEndian.AppendUint64(b, d.ScannedRoot)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(d.Offsets)))
	for _, off := range d.Offsets {
		b = binary.LittleEndian.AppendUint64(b, off)
	}
	b = binary.LittleEndian.AppendUint32(b, record.CRC32C(b))

	path := filepath.Join(dir, DirectoryFileName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return orfserr.IO(err, "create committed-root directory temp")
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return orfserr.IO(err, "write committed-root directory temp")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return orfserr.IO(err, "fsync committed-root directory temp")
	}
	if err := f.Close(); err != nil {
		return orfserr.IO(err, "close committed-root directory temp")
	}
	if err := os.Rename(tmp, path); err != nil {
		return orfserr.IO(err, "rename committed-root directory into place")
	}
	return segment.SyncDir(dir)
}
