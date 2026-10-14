// Package wm implements the commit watermark: a small durability witness
// written beside the segments that certifies the committed root.
//
// It is added metadata, never part of the authoritative record stream. It is
// written only *after* the bytes it describes are durable, so it can never
// certify more history than the backing holds. A missing watermark leaves the
// volume unvalidated (not an error); a present-but-corrupt watermark is an
// error and is never ignored.
package wm

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// FileName is the watermark file name.
const FileName = "commit.watermark"

// Len is the fixed watermark length.
const Len = 36

// Watermark is the decoded watermark.
type Watermark struct {
	FormatVersion   uint16
	CommittedRoot   uint64
	SegmentID       uint64
	SegmentValidLen uint64
}

// Encode renders the 36-byte watermark.
func (w Watermark) Encode() []byte {
	b := make([]byte, Len)
	binary.LittleEndian.PutUint32(b[0:4], record.WatermarkMagic)
	binary.LittleEndian.PutUint16(b[4:6], record.FormatVersion)
	binary.LittleEndian.PutUint16(b[6:8], 0)
	binary.LittleEndian.PutUint64(b[8:16], w.CommittedRoot)
	binary.LittleEndian.PutUint64(b[16:24], w.SegmentID)
	binary.LittleEndian.PutUint64(b[24:32], w.SegmentValidLen)
	binary.LittleEndian.PutUint32(b[32:36], record.CRC32C(b[0:32]))
	return b
}

// Decode parses a watermark.
func Decode(b []byte) (Watermark, error) {
	var w Watermark
	if len(b) != Len {
		return w, orfserr.Corrupt("commit watermark is %d bytes, want %d", len(b), Len)
	}
	if binary.LittleEndian.Uint32(b[0:4]) != record.WatermarkMagic {
		return w, orfserr.Corrupt("commit watermark magic mismatch")
	}
	if binary.LittleEndian.Uint32(b[32:36]) != record.CRC32C(b[0:32]) {
		return w, orfserr.Corrupt("commit watermark checksum mismatch")
	}
	w.FormatVersion = binary.LittleEndian.Uint16(b[4:6])
	if w.FormatVersion != record.FormatVersion {
		return w, orfserr.Compatibility("commit watermark format_version %d is not implemented (%d)", w.FormatVersion, record.FormatVersion)
	}
	w.CommittedRoot = binary.LittleEndian.Uint64(b[8:16])
	w.SegmentID = binary.LittleEndian.Uint64(b[16:24])
	w.SegmentValidLen = binary.LittleEndian.Uint64(b[24:32])
	return w, nil
}

// Write writes the watermark atomically: tmp -> fsync -> rename -> dir fsync.
func Write(dir string, w Watermark) error {
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return orfserr.IO(err, "create watermark temp")
	}
	if _, err := f.Write(w.Encode()); err != nil {
		f.Close()
		return orfserr.IO(err, "write watermark temp")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return orfserr.IO(err, "fsync watermark temp")
	}
	if err := f.Close(); err != nil {
		return orfserr.IO(err, "close watermark temp")
	}
	if err := os.Rename(tmp, path); err != nil {
		return orfserr.IO(err, "rename watermark into place")
	}
	return syncDir(dir)
}

// Read reads and decodes the watermark; a missing file yields ok=false.
func Read(dir string) (Watermark, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Watermark{}, false, nil
		}
		return Watermark{}, false, orfserr.IO(err, "read commit watermark")
	}
	w, err := Decode(b)
	if err != nil {
		return Watermark{}, false, err
	}
	return w, true, nil
}

// Validate checks a watermark against a recovered committed root. It refuses an
// open when the watermark certifies a committed root beyond the surviving
// history, rather than silently recovering shorter history.
func Validate(dir string, recoveredRoot uint64) (*Watermark, error) {
	w, ok, err := Read(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if w.CommittedRoot > recoveredRoot {
		return &w, orfserr.Corrupt(
			"commit watermark certifies committed root %d but only %d byte(s) of committed history survive: a trailing committed segment is missing or truncated",
			w.CommittedRoot, recoveredRoot)
	}
	return &w, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return orfserr.IO(err, "open dir for fsync %s", dir)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return orfserr.IO(err, "fsync dir %s", dir)
	}
	return nil
}
