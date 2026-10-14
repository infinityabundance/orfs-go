// Package segment implements OrFS log segments: the append-only files that
// hold the canonical logical byte stream.
//
// A segment's name is its id, which is the first logical byte offset it covers,
// so the physical offset of a canonical logical offset L in segment S is L - S.
// Segment ids are unique and strictly increasing; the canonical log is
// contiguous, so S[i+1].id == S[i].id + valid_len(S[i]).
package segment

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

// NameDigits is the fixed number of decimal digits in a segment name.
const NameDigits = 16

// Suffix is the segment file-name suffix.
const Suffix = ".seg"

// QuarantineDir is the directory holding retained abandoned tails. Segment ids
// are never reused, so quarantined segments are not canonical.
const QuarantineDir = "quarantine"

// MaxSegmentID is the largest representable segment id.
const MaxSegmentID uint64 = 9_999_999_999_999_999

// SegmentName renders the file name for a segment id.
func SegmentName(id uint64) string {
	return fmt.Sprintf("%0*d%s", NameDigits, id, Suffix)
}

// ParseSegmentName returns the id of a segment file name, or ok=false if the
// name is not exactly 16 ASCII decimal digits followed by ".seg".
func ParseSegmentName(name string) (uint64, bool) {
	if !strings.HasSuffix(name, Suffix) {
		return 0, false
	}
	stem := name[:len(name)-len(Suffix)]
	if len(stem) != NameDigits {
		return 0, false
	}
	var id uint64
	for i := 0; i < len(stem); i++ {
		c := stem[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		id = id*10 + uint64(c-'0')
	}
	return id, true
}

// DiscoverSegmentIDs lists the segment ids in dir, ignoring every non-segment
// entry (including the quarantine directory), sorted ascending. Duplicate ids
// are reported as corruption.
func DiscoverSegmentIDs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, orfserr.IO(err, "read log directory %s", dir)
	}
	seen := make(map[uint64]bool)
	var ids []uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := ParseSegmentName(e.Name())
		if !ok {
			continue
		}
		if seen[id] {
			return nil, orfserr.Corrupt("duplicate segment id %d in %s", id, dir)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// Extent is a canonical segment: an id and the number of valid bytes it holds.
type Extent struct {
	ID       uint64
	ValidLen uint64
}

// End is the exclusive logical end of the extent.
func (e Extent) End() uint64 { return e.ID + e.ValidLen }

// Locator is the ordered, contiguous set of canonical segment extents.
type Locator struct {
	extents []Extent
}

// NewLocator validates the contiguity rule and builds a locator: non-empty,
// first id 0, and every following id equal to the previous extent's end.
func NewLocator(extents []Extent) (*Locator, error) {
	if len(extents) == 0 {
		return nil, orfserr.Corrupt("segment locator is empty")
	}
	if extents[0].ID != 0 {
		return nil, orfserr.Corrupt("first segment id %d is not 0", extents[0].ID)
	}
	for i := 1; i < len(extents); i++ {
		if extents[i].ID != extents[i-1].End() {
			return nil, orfserr.Corrupt("segment %d does not continue segment %d (gap or overlap)", extents[i].ID, extents[i-1].ID)
		}
	}
	return &Locator{extents: append([]Extent(nil), extents...)}, nil
}

// Extents returns a copy of the locator's extents.
func (l *Locator) Extents() []Extent { return append([]Extent(nil), l.extents...) }

// Len is the number of extents.
func (l *Locator) Len() int { return len(l.extents) }

// TotalLen is the total canonical bytes across all extents.
func (l *Locator) TotalLen() uint64 {
	if len(l.extents) == 0 {
		return 0
	}
	return l.extents[len(l.extents)-1].End()
}

// Last returns the final extent.
func (l *Locator) Last() Extent { return l.extents[len(l.extents)-1] }

// Append adds a new extent, enforcing the contiguity rule.
func (l *Locator) Append(e Extent) error {
	if len(l.extents) > 0 {
		last := l.extents[len(l.extents)-1]
		if e.ID != last.End() {
			return orfserr.Corrupt("new segment %d does not continue segment %d", e.ID, last.ID)
		}
	}
	l.extents = append(l.extents, e)
	return nil
}

// SetLastValid updates the final extent's valid length (used as the writer
// advances within the active segment).
func (l *Locator) SetLastValid(v uint64) {
	if len(l.extents) == 0 {
		return
	}
	l.extents[len(l.extents)-1].ValidLen = v
}

// Truncate returns extents whose id is strictly below committed, discarding
// tail extents at or beyond it. It never truncates a file; it only narrows the
// canonical view.
func Truncate(extents []Extent, committed uint64) []Extent {
	var out []Extent
	for _, e := range extents {
		if e.ID >= committed {
			break
		}
		out = append(out, e)
	}
	return out
}

// Segment is an open log segment file.
type Segment struct {
	id   uint64
	path string
	f    *os.File
}

// Create creates a new, empty segment file, fsyncing the containing directory.
func Create(dir string, id uint64) (*Segment, error) {
	path := filepath.Join(dir, SegmentName(id))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, orfserr.IO(err, "create segment %s", path)
	}
	if err := SyncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	return &Segment{id: id, path: path, f: f}, nil
}

// Open opens an existing segment file for read/write.
func Open(dir string, id uint64) (*Segment, error) {
	path := filepath.Join(dir, SegmentName(id))
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, orfserr.IO(err, "open segment %s", path)
	}
	return &Segment{id: id, path: path, f: f}, nil
}

// OpenReadOnly opens an existing segment file for reading only.
func OpenReadOnly(dir string, id uint64) (*Segment, error) {
	path := filepath.Join(dir, SegmentName(id))
	f, err := os.Open(path)
	if err != nil {
		return nil, orfserr.IO(err, "open segment %s (read-only)", path)
	}
	return &Segment{id: id, path: path, f: f}, nil
}

// ID is the segment id (also its first logical offset).
func (s *Segment) ID() uint64 { return s.id }

// Path is the on-disk path.
func (s *Segment) Path() string { return s.path }

// Len is the current physical length.
func (s *Segment) Len() (int64, error) {
	fi, err := s.f.Stat()
	if err != nil {
		return 0, orfserr.IO(err, "stat segment")
	}
	return fi.Size(), nil
}

// ReadAt reads exactly len(p) bytes at off.
func (s *Segment) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }

// WriteAt writes p at off.
func (s *Segment) WriteAt(p []byte, off int64) (int, error) { return s.f.WriteAt(p, off) }

// Truncate sets the file length.
func (s *Segment) Truncate(n int64) error {
	if err := s.f.Truncate(n); err != nil {
		return orfserr.IO(err, "truncate segment")
	}
	return nil
}

// Sync flushes the segment to stable storage.
func (s *Segment) Sync() error {
	if err := s.f.Sync(); err != nil {
		return orfserr.IO(err, "fsync segment")
	}
	return nil
}

// Close closes the segment.
func (s *Segment) Close() error { return s.f.Close() }

// ReaderAt is satisfied by Segment for use by the frame reader.
var _ io.ReaderAt = (*Segment)(nil)

// SyncDir fsyncs a directory so that a created rename is durable.
func SyncDir(dir string) error {
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
