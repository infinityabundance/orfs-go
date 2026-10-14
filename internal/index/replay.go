package index

import (
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// Replay rebuilds the index by replaying the committed prefix [0, end) of the
// log at dir.
func Replay(dir string, extents []segment.Extent, end uint64) (*Index, error) {
	idx := New()
	if end == 0 {
		return nil, orfserr.Corrupt("replaying an empty log")
	}
	if _, _, err := idx.ReplayRange(dir, extents, 0, end); err != nil {
		return nil, err
	}
	return idx, nil
}

// ReplayRange extends idx — which must already reflect the committed state at
// the boundary `start` (a decoded checkpoint snapshot, or the genesis state) —
// by replaying only the committed records in [start, end). It seeks straight to
// `start`, so the work is proportional to `end - start`, never to the history
// before it. It returns the records and bytes replayed and verifies that the
// result lands exactly on `end`.
func (idx *Index) ReplayRange(dir string, extents []segment.Extent, start, end uint64) (records, bytes uint64, err error) {
	if start > end {
		return 0, 0, orfserr.Corrupt("replay range start %d exceeds end %d", start, end)
	}
	if start == end {
		return 0, 0, nil
	}
	err = log.ReadRecordsRange(dir, extents, start, end, func(rec *record.Record, offset uint64, raw []byte) error {
		idx.CommittedRecords++
		records++
		bytes += uint64(len(raw))
		return idx.Apply(rec, offset)
	})
	if err != nil {
		return records, bytes, err
	}
	if idx.Root != end {
		return records, bytes, orfserr.Corrupt("suffix replay ended at root %d (expected %d)", idx.Root, end)
	}
	return records, bytes, nil
}
