package log

import (
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// RecordLocation is a discovered record's canonical offset and type.
type RecordLocation struct {
	Offset uint64
	Type   uint16
}

// ScanRecordOffsets performs a header-only discovery scan over the canonical
// records in [start, end), returning the offsets of every record whose type is
// in `want`, ascending. It reads 60 bytes per record and skips the payload and
// footer, so an intervening DATA_EXTENT body is never materialised. The frames
// it skips are still checked for framing (header check, total length) but are
// not fully validated here: a candidate is read and validated in full before it
// is trusted.
//
// `start` must sit on a record boundary (a committed root is one). A gap, a
// torn header, or a range that does not tile exactly to `end` is corruption.
func ScanRecordOffsets(dir string, extents []segment.Extent, start, end uint64, want map[uint16]bool) ([]RecordLocation, error) {
	if start > end {
		return nil, orfserr.Corrupt("record scan start %d exceeds end %d", start, end)
	}
	var out []RecordLocation
	off := start
	for _, e := range extents {
		if off >= end {
			break
		}
		if e.ID > off {
			return nil, orfserr.Corrupt("canonical gap: extent %d starts beyond offset %d", e.ID, off)
		}
		if e.End() <= off {
			continue
		}
		seg, err := segment.OpenReadOnly(dir, e.ID)
		if err != nil {
			return nil, err
		}
		segLimit := e.ValidLen
		if segLimit > end-e.ID {
			segLimit = end - e.ID
		}
		pos := int64(off - e.ID)
		var hdr [record.HeaderLen]byte
		for uint64(pos) < segLimit {
			n, err := seg.ReadAt(hdr[:], pos)
			if err != nil && n < record.HeaderLen {
				seg.Close()
				return nil, orfserr.Torn("short record header at logical offset %d", e.ID+uint64(pos))
			}
			h, err := record.DecodeHeader(hdr[:])
			if err != nil {
				seg.Close()
				return nil, err
			}
			total, err := h.TotalLen()
			if err != nil {
				seg.Close()
				return nil, err
			}
			if uint64(pos)+total > segLimit {
				seg.Close()
				return nil, orfserr.Torn("record at logical offset %d overruns the range", e.ID+uint64(pos))
			}
			if want[h.RecordType] {
				out = append(out, RecordLocation{Offset: e.ID + uint64(pos), Type: h.RecordType})
			}
			pos += int64(total)
		}
		seg.Close()
		off = e.ID + segLimit
	}
	if off != end {
		return nil, orfserr.Corrupt("canonical bytes stop at %d, want %d", off, end)
	}
	return out, nil
}

// ReadRecordAt reads and fully validates the single record whose frame begins at
// canonical logical offset. The record must lie wholly within one canonical
// extent.
func ReadRecordAt(dir string, extents []segment.Extent, offset uint64) (*record.Record, error) {
	for _, e := range extents {
		if offset < e.ID || offset >= e.End() {
			continue
		}
		seg, err := segment.OpenReadOnly(dir, e.ID)
		if err != nil {
			return nil, err
		}
		pos := int64(offset - e.ID)
		limit := int64(e.ValidLen - (offset - e.ID))
		rec, _, err := record.ReadFrameAt(seg, pos, pos+limit)
		seg.Close()
		if err != nil {
			return nil, err
		}
		return rec, nil
	}
	return nil, orfserr.Corrupt("logical offset %d is not within any canonical extent", offset)
}
