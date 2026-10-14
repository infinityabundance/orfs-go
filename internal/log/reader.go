package log

import (
	"sync"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// CanonicalReader reads authoritative bytes at a canonical logical offset,
// caching open segment files. It is safe for concurrent use: immutable read
// views may be served in parallel after the global read lock is removed.
type CanonicalReader struct {
	dir     string
	extents []segment.Extent

	mu   sync.Mutex
	open map[uint64]*segment.Segment
}

// NewCanonicalReader builds a reader over the given canonical extents.
func NewCanonicalReader(dir string, extents []segment.Extent) *CanonicalReader {
	return &CanonicalReader{dir: dir, extents: extents, open: make(map[uint64]*segment.Segment)}
}

// UpdateExtents replaces the canonical extent list. It is called after the log
// grows so that a reader established at open can reach newly appended bytes.
func (r *CanonicalReader) UpdateExtents(extents []segment.Extent) {
	r.mu.Lock()
	r.extents = extents
	r.mu.Unlock()
}

func (r *CanonicalReader) segment(id uint64) (*segment.Segment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seg, ok := r.open[id]; ok {
		return seg, nil
	}
	seg, err := segment.OpenReadOnly(r.dir, id)
	if err != nil {
		return nil, err
	}
	r.open[id] = seg
	return seg, nil
}

// ReadAt reads len(p) bytes at canonical logical offset off.
func (r *CanonicalReader) ReadAt(off uint64, p []byte) (int, error) {
	r.mu.Lock()
	extents := r.extents
	r.mu.Unlock()
	for _, e := range extents {
		if off < e.ID || off >= e.End() {
			continue
		}
		seg, err := r.segment(e.ID)
		if err != nil {
			return 0, err
		}
		avail := e.End() - off
		want := uint64(len(p))
		if want > avail {
			want = avail
		}
		n, err := seg.ReadAt(p[:want], int64(off-e.ID))
		if err != nil && uint64(n) == want {
			err = nil
		}
		return n, err
	}
	return 0, orfserr.Corrupt("logical offset %d is not within any canonical extent", off)
}

// Close releases the cached segment files.
func (r *CanonicalReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	for id, seg := range r.open {
		if e := seg.Close(); e != nil && err == nil {
			err = e
		}
		delete(r.open, id)
	}
	return err
}

// ReadRecords iterates the validated canonical records in [0, end) across the
// given extents, calling fn with each record, its logical offset, and its exact
// framed bytes. It opens each segment read-only and touches nothing.
func ReadRecords(dir string, extents []segment.Extent, end uint64, fn func(rec *record.Record, offset uint64, raw []byte) error) error {
	return ReadRecordsRange(dir, extents, 0, end, fn)
}

// ReadRecordsRange iterates the validated canonical records in [start, end)
// across the given extents. It seeks straight to `start` and decodes no byte
// before it, so the decoded span is exactly `end - start` regardless of how much
// history precedes it. `start` must be a record boundary (in practice a
// committed transaction boundary); a start that does not sit on a record frame
// is refused rather than silently applying a garbled record.
func ReadRecordsRange(dir string, extents []segment.Extent, start, end uint64, fn func(rec *record.Record, offset uint64, raw []byte) error) error {
	if start > end {
		return orfserr.Corrupt("record range start %d exceeds end %d", start, end)
	}
	if start == end {
		return nil
	}
	off := start
	for _, e := range extents {
		if off >= end {
			break
		}
		if e.ID > off {
			return orfserr.Corrupt("canonical gap: extent %d starts beyond offset %d", e.ID, off)
		}
		if e.End() <= off {
			continue
		}
		seg, err := segment.OpenReadOnly(dir, e.ID)
		if err != nil {
			return err
		}
		// Bytes of this segment that are canonical and inside the requested range.
		segLimit := e.ValidLen
		if segLimit > end-e.ID {
			segLimit = end - e.ID
		}
		pos := int64(off - e.ID)
		for uint64(pos) < segLimit {
			rec, raw, total, err := record.ReadFrameRawAt(seg, pos, int64(segLimit))
			if err != nil {
				seg.Close()
				return err
			}
			if err := fn(rec, e.ID+uint64(pos), raw); err != nil {
				seg.Close()
				return err
			}
			pos += total
		}
		seg.Close()
		off = e.ID + segLimit
	}
	if off != end {
		return orfserr.Corrupt("canonical bytes stop at %d, want %d", off, end)
	}
	return nil
}
