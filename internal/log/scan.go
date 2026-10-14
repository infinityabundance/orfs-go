// Package log implements the authoritative append-only log: scanning,
// validation, transactions, committed roots, recovery, and appends.
//
// A root R is a *position* (committed end offset) in the canonical logical byte
// stream, never a content hash. R advances only when a complete, valid
// VOLUME_GENESIS or TX_COMMIT ends. Nothing is ever rewritten or truncated;
// a tail beyond R is retained (and, on a writable open, moved to quarantine/).
package log

import (
	"errors"
	"hash"

	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// TailKind classifies the abandoned tail beyond the committed root.
type TailKind int

const (
	// TailTornRecord is an incomplete frame (short read or failing footer).
	TailTornRecord TailKind = iota
	// TailUncommitted is one or more complete frames after the last commit.
	TailUncommitted
)

func (k TailKind) String() string {
	if k == TailTornRecord {
		return "torn-record"
	}
	return "uncommitted"
}

// TailInfo describes the abandoned bytes beyond the committed root.
type TailInfo struct {
	Kind           TailKind
	SegmentID      uint64
	BoundaryOffset uint64
	PhysicalEnd    uint64
	AbandonedBytes uint64
}

// ScanResult is the outcome of scanning and validating a log directory.
type ScanResult struct {
	Extents              []segment.Extent
	CommittedRoot        uint64
	CommittedRecordCount uint64
	Boundaries           []uint64
	MaxTransactionID     uint64
	Genesis              record.Genesis
	HasGenesis           bool
	Tail                 *TailInfo
	PhysicalEnd          uint64
	ByType               map[uint16]uint64
}

// machineFault distinguishes a hard fault (corruption even in the final
// segment) from a soft one (an uncommitted tail when it is the last segment).
type machineFault struct {
	err  error
	hard bool
}

func (f *machineFault) Error() string { return f.err.Error() }
func (f *machineFault) Unwrap() error { return f.err }

func soft(format string, a ...any) *machineFault {
	return &machineFault{err: orfserr.Corrupt(format, a...), hard: false}
}
func hard(format string, a ...any) *machineFault {
	return &machineFault{err: orfserr.Corrupt(format, a...), hard: true}
}
func hardErr(err error) *machineFault { return &machineFault{err: err, hard: true} }

type bracket struct {
	txID       uint64
	records    uint64 // TX_BEGIN + data records
	byteLen    uint64
	hash       hash.Hash
	beginCount uint32
}

// machine is the transaction state machine that establishes the committed root.
type machine struct {
	started          bool
	nextSeq          uint64
	count            uint64
	committedEnd     uint64
	committedRecords uint64
	boundaries       []uint64
	maxTxID          uint64
	genesis          record.Genesis
	byType           map[uint16]uint64
	open             *bracket
}

func newMachine() *machine {
	return &machine{byType: make(map[uint16]uint64)}
}

// accept processes one complete, framed record and advances the machine.
func (m *machine) accept(rec *record.Record, raw []byte, endOffset uint64) *machineFault {
	h := rec.Header
	if h.SequenceNumber != m.nextSeq {
		return soft("record sequence number %d != expected %d", h.SequenceNumber, m.nextSeq)
	}
	total := uint64(len(raw))
	code := h.RecordType

	if !m.started {
		if code != record.TypeVolumeGenesis {
			return soft("first record is not VOLUME_GENESIS")
		}
		g, err := record.ParseGenesis(rec.Payload)
		if err != nil {
			return hardErr(err)
		}
		m.started = true
		m.genesis = g
		m.committedEnd = endOffset
		m.count++
		m.committedRecords = m.count
		m.boundaries = append(m.boundaries, endOffset)
		m.nextSeq = m.count
		m.byType[code]++
		return nil
	}

	switch record.ClassifyCode(code) {
	case record.ClassMandatoryUnknown:
		return hardErr(orfserr.Compatibility("unknown mandatory record type 0x%04X", code))
	case record.ClassOptionalUnknown:
		// Skippable extension. It may still lie inside a transaction.
		if m.open != nil {
			m.open.records++
			m.open.byteLen += total
			m.open.hash.Write(raw)
		}
		m.count++
		m.nextSeq = m.count
		m.byType[code]++
		return nil
	}

	switch code {
	case record.TypeVolumeGenesis:
		return soft("duplicate VOLUME_GENESIS")
	case record.TypeTxBegin:
		if m.open != nil {
			return soft("nested TX_BEGIN")
		}
		tb, err := record.DecodeTxBegin(rec.Payload)
		if err != nil {
			return hardErr(err)
		}
		if tb.TransactionID == 0 {
			return soft("TX_BEGIN with zero transaction id")
		}
		if tb.TransactionID != h.TransactionID {
			return hard("TX_BEGIN payload transaction id %d disagrees with header %d", tb.TransactionID, h.TransactionID)
		}
		bh := blake3.New(record.BracketChecksumLen, nil)
		bh.Write(raw)
		m.open = &bracket{txID: tb.TransactionID, records: 1, byteLen: total, hash: bh, beginCount: tb.ExpectedRecordCount}
		if tb.TransactionID > m.maxTxID {
			m.maxTxID = tb.TransactionID
		}
		m.count++
		m.nextSeq = m.count
		m.byType[code]++
		return nil
	case record.TypeTxCommit:
		if m.open == nil {
			return soft("TX_COMMIT without an open transaction")
		}
		tc, err := record.DecodeTxCommit(rec.Payload)
		if err != nil {
			return hardErr(err)
		}
		if tc.TransactionID != m.open.txID {
			return soft("TX_COMMIT transaction id %d != open %d", tc.TransactionID, m.open.txID)
		}
		if tc.RecordCount != m.open.records {
			return hard("TX_COMMIT record_count %d != bracketed %d", tc.RecordCount, m.open.records)
		}
		if tc.BracketedByteLength != m.open.byteLen {
			return hard("TX_COMMIT bracketed_byte_length %d != %d", tc.BracketedByteLength, m.open.byteLen)
		}
		var sum [record.BracketChecksumLen]byte
		copy(sum[:], m.open.hash.Sum(nil))
		if sum != tc.BracketChecksum {
			return hard("TX_COMMIT bracket checksum mismatch")
		}
		// TX_BEGIN.expected_record_count is advisory: the Rust reference writes 0
		// here from its low-level log API (the value the frozen fixtures carry)
		// and treats 0 as "no expectation" when verifying (reference
		// `log.rs`/`transaction.rs::verify_bracket`: `expected_record_count != 0
		// && …`). The authoritative cross-check is TX_COMMIT.record_count against
		// the actual bracket, done above. Enforcing a bare 0 here would reject a
		// valid Rust-written volume, so match the reference exactly: 0 means
		// unset.
		if m.open.beginCount != 0 && uint64(m.open.beginCount) != m.open.records {
			return hard("TX_BEGIN expected_record_count %d != bracketed %d", m.open.beginCount, m.open.records)
		}
		m.open = nil
		m.count++
		m.committedEnd = endOffset
		m.committedRecords = m.count
		m.boundaries = append(m.boundaries, endOffset)
		m.nextSeq = m.count
		m.byType[code]++
		return nil
	case record.TypeTxAbort:
		if m.open == nil {
			return soft("TX_ABORT without an open transaction")
		}
		ta, err := record.DecodeTxAbort(rec.Payload)
		if err != nil {
			return hardErr(err)
		}
		if ta.TransactionID != m.open.txID {
			return soft("TX_ABORT transaction id %d != open %d", ta.TransactionID, m.open.txID)
		}
		m.open = nil
		m.count++
		m.nextSeq = m.count
		m.byType[code]++
		return nil
	default:
		if m.open == nil {
			return soft("record type %s outside a transaction", record.TypeName(code))
		}
		if err := record.ValidatePayload(rec); err != nil {
			return hardErr(err)
		}
		m.open.records++
		m.open.byteLen += total
		m.open.hash.Write(raw)
		m.count++
		m.nextSeq = m.count
		m.byType[code]++
		return nil
	}
}

// ScanLog scans and validates a log directory without modifying a single byte.
func ScanLog(dir string) (*ScanResult, error) {
	ids, err := segment.DiscoverSegmentIDs(dir)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, orfserr.Corrupt("log directory %s contains no segments", dir)
	}
	m := newMachine()
	var extents []segment.Extent
	var totalValid uint64
	var physicalEnd uint64
	var tail *TailInfo

	for i, id := range ids {
		isLast := i == len(ids)-1
		seg, err := segment.OpenReadOnly(dir, id)
		if err != nil {
			return nil, err
		}
		plen, err := seg.Len()
		if err != nil {
			seg.Close()
			return nil, err
		}
		segPhysicalEnd := id + uint64(plen)
		physicalEnd = segPhysicalEnd
		var off int64
		stopped := false
		for off < plen {
			rec, raw, total, err := record.ReadFrameRawAt(seg, off, plen)
			if err != nil {
				if errors.Is(err, orfserr.ErrTorn) {
					if isLast {
						tail = &TailInfo{
							Kind:           TailTornRecord,
							SegmentID:      id,
							BoundaryOffset: id + uint64(off),
							PhysicalEnd:    segPhysicalEnd,
							AbandonedBytes: uint64(plen) - uint64(off),
						}
						stopped = true
						break
					}
					seg.Close()
					return nil, orfserr.Corrupt("torn record in non-final segment %d at physical offset %d", id, off)
				}
				seg.Close()
				return nil, err
			}
			end := id + uint64(off) + uint64(total)
			if fault := m.accept(rec, raw, end); fault != nil {
				if fault.hard {
					seg.Close()
					return nil, fault.err
				}
				if isLast && m.started {
					tail = &TailInfo{
						Kind:           TailUncommitted,
						SegmentID:      id,
						BoundaryOffset: id + uint64(off),
						PhysicalEnd:    segPhysicalEnd,
						AbandonedBytes: uint64(plen) - uint64(off),
					}
					stopped = true
					break
				}
				seg.Close()
				return nil, fault.err
			}
			off += total
		}
		extents = append(extents, segment.Extent{ID: id, ValidLen: uint64(off)})
		totalValid += uint64(off)
		seg.Close()
		if stopped {
			break
		}
	}

	if !m.started {
		return nil, orfserr.Corrupt("log directory %s has no VOLUME_GENESIS record", dir)
	}

	if physicalEnd > m.committedEnd && tail == nil {
		segmentID := uint64(0)
		for i := len(extents) - 1; i >= 0; i-- {
			if extents[i].ID <= m.committedEnd {
				segmentID = extents[i].ID
				break
			}
		}
		tail = &TailInfo{
			Kind:           TailUncommitted,
			SegmentID:      segmentID,
			BoundaryOffset: m.committedEnd,
			PhysicalEnd:    physicalEnd,
			AbandonedBytes: physicalEnd - m.committedEnd,
		}
	}

	return &ScanResult{
		Extents:              extents,
		CommittedRoot:        m.committedEnd,
		CommittedRecordCount: m.committedRecords,
		Boundaries:           append([]uint64(nil), m.boundaries...),
		MaxTransactionID:     m.maxTxID,
		Genesis:              m.genesis,
		HasGenesis:           m.started,
		Tail:                 tail,
		PhysicalEnd:          physicalEnd,
		ByType:               m.byType,
	}, nil
}
