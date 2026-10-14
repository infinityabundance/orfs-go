package log

import (
	"os"
	"path/filepath"
	"sync"

	"lukechampine.com/blake3"

	"github.com/infinityabundance/orfs-go/internal/capacity"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/wm"
)

// DirName is the log directory name within a volume.
const DirName = "or.log"

// Dir returns the log directory for a volume backing directory.
func Dir(backing string) string { return filepath.Join(backing, DirName) }

// Mode is the open mode.
type Mode int

const (
	// ReadOnly modifies not a single byte.
	ReadOnly Mode = iota
	// ReadWrite may quarantine a retained tail.
	ReadWrite
)

// RecoveryDisposition reports what recovery observed and did.
type RecoveryDisposition struct {
	Clean       bool
	Tail        *TailInfo
	Quarantined bool
}

// Log is the authoritative append-only log handle.
type Log struct {
	dir         string
	segmentSize uint64
	mode        Mode

	locator *segment.Locator

	writer      *segment.Segment
	writerID    uint64
	writerValid uint64

	committedRoot    uint64
	committedRecords uint64
	boundaries       []uint64
	maxTxID          uint64
	nextSeq          uint64

	genesis record.Genesis

	tail        *TailInfo
	quarantined bool
	recovery    RecoveryDisposition

	cap *capacity.Governor

	durableWatermark *wm.Watermark

	// Group commit (opt-in). When group is non-nil, Publish delegates to it so
	// concurrent publishes share one durability barrier. pubMu serialises the
	// per-publish path when group commit is off (concurrent, unbatched writers).
	pubMu    sync.Mutex
	group    *groupCommitter
	groupCfg *groupConfig

	// syncHook is the fsync seam. It is nil in production; a test may install a
	// fake to force a barrier failure without touching the on-disk format.
	syncHook func() error
}

// Create initialises a fresh log: it creates the directory and segment 0, writes
// the framed genesis record, fsyncs, and persists an initial watermark.
func Create(backing string, genesis record.Genesis, opts ...Option) (*Log, error) {
	dir := Dir(backing)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, orfserr.IO(err, "create log directory %s", dir)
	}
	ids, err := segment.DiscoverSegmentIDs(dir)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		return nil, orfserr.Exists("log directory %s already contains segments", dir)
	}

	seg, err := segment.Create(dir, 0)
	if err != nil {
		return nil, err
	}
	payload := record.EncodeGenesis(genesis)
	rec := &record.Record{
		Header: record.Header{
			FormatVersion: record.FormatVersion,
			RecordType:    record.TypeVolumeGenesis,
			PayloadLength: uint64(len(payload)),
		},
		Payload: payload,
	}
	frame, err := rec.Encode()
	if err != nil {
		seg.Close()
		return nil, err
	}
	if _, err := seg.WriteAt(frame, 0); err != nil {
		seg.Close()
		return nil, orfserr.IO(err, "write genesis record")
	}
	if err := seg.Sync(); err != nil {
		seg.Close()
		return nil, err
	}

	loc, err := segment.NewLocator([]segment.Extent{{ID: 0, ValidLen: uint64(len(frame))}})
	if err != nil {
		seg.Close()
		return nil, err
	}
	l := &Log{
		dir:              dir,
		segmentSize:      genesis.SegmentSize,
		mode:             ReadWrite,
		locator:          loc,
		writer:           seg,
		writerID:         0,
		writerValid:      uint64(len(frame)),
		committedRoot:    uint64(len(frame)),
		committedRecords: 1,
		boundaries:       []uint64{uint64(len(frame))},
		nextSeq:          1,
		genesis:          genesis,
		recovery:         RecoveryDisposition{Clean: true},
		cap:              capacity.New(capacity.DefaultPolicy()),
	}
	l.cap.ResolveFromBacking(dir, l.committedRoot)
	if err := l.persistWatermark(); err != nil {
		seg.Close()
		return nil, err
	}
	applyOptions(l, opts)
	return l, nil
}

// Open scans, validates, then opens a log. A writable open quarantines a
// retained tail (moving, never truncating) and continues in a fresh segment.
func Open(backing string, mode Mode, opts ...Option) (*Log, error) {
	dir := Dir(backing)
	scan, err := ScanLog(dir)
	if err != nil {
		return nil, err
	}
	committed := scan.CommittedRoot

	loc, err := segment.NewLocator(scan.Extents)
	if err != nil {
		return nil, err
	}
	dw, err := wm.Validate(dir, committed)
	if err != nil {
		return nil, err
	}

	l := &Log{
		dir:              dir,
		segmentSize:      scan.Genesis.SegmentSize,
		mode:             mode,
		locator:          loc,
		committedRoot:    committed,
		committedRecords: scan.CommittedRecordCount,
		boundaries:       scan.Boundaries,
		maxTxID:          scan.MaxTransactionID,
		nextSeq:          scan.CommittedRecordCount,
		genesis:          scan.Genesis,
		tail:             scan.Tail,
		durableWatermark: dw,
		cap:              capacity.New(capacity.DefaultPolicy()),
	}

	if mode == ReadWrite {
		if scan.PhysicalEnd > committed {
			if err := quarantineAfter(dir, scan.Extents, committed); err != nil {
				return nil, err
			}
			l.quarantined = true
			loc, err := segment.NewLocator(truncateExtents(scan.Extents, committed))
			if err != nil {
				return nil, err
			}
			seg, err := segment.Create(dir, committed)
			if err != nil {
				return nil, err
			}
			if err := loc.Append(segment.Extent{ID: committed, ValidLen: 0}); err != nil {
				seg.Close()
				return nil, err
			}
			l.locator = loc
			l.writer = seg
			l.writerID = committed
			l.writerValid = 0
		} else {
			last := loc.Last()
			seg, err := segment.Open(dir, last.ID)
			if err != nil {
				return nil, err
			}
			n, err := seg.Len()
			if err != nil {
				seg.Close()
				return nil, err
			}
			if uint64(n) != last.ValidLen {
				seg.Close()
				return nil, orfserr.Corrupt("segment %d length %d disagrees with canonical valid length %d", last.ID, n, last.ValidLen)
			}
			l.writer = seg
			l.writerID = last.ID
			l.writerValid = last.ValidLen
		}
	}

	if committed > l.locator.TotalLen() {
		return nil, orfserr.Corrupt("committed root %d extends beyond the canonical bytes %d (a committed segment is missing or truncated)", committed, l.locator.TotalLen())
	}

	if scan.Tail != nil {
		l.recovery = RecoveryDisposition{Tail: scan.Tail, Quarantined: l.quarantined}
	} else {
		l.recovery = RecoveryDisposition{Clean: true}
	}
	l.cap.ResolveFromBacking(dir, l.appendPos())
	applyOptions(l, opts)
	return l, nil
}

// Close stops the group committer (if any) and closes the writer. It must not
// be called concurrently with Publish.
func (l *Log) Close() error {
	if l.group != nil {
		l.group.close()
	}
	if l.writer != nil {
		return l.writer.Close()
	}
	return nil
}

// Recovery returns the disposition recorded at open.
func (l *Log) Recovery() RecoveryDisposition { return l.recovery }

// Dir is the log directory.
func (l *Log) Dir() string { return l.dir }

// Genesis returns the authoritative-log identity.
func (l *Log) Genesis() record.Genesis { return l.genesis }

// CommittedRoot is the committed end offset R.
func (l *Log) CommittedRoot() uint64 { return l.committedRoot }

// CommittedRecordCount is the number of committed records.
func (l *Log) CommittedRecordCount() uint64 { return l.committedRecords }

// Boundaries returns the committed boundaries (0 is implicit; genesis end and
// each TX_COMMIT end follow).
func (l *Log) Boundaries() []uint64 { return append([]uint64(nil), l.boundaries...) }

// MaxTransactionID is the greatest transaction id ever observed.
func (l *Log) MaxTransactionID() uint64 { return l.maxTxID }

// SegmentSize is the segment-size policy.
func (l *Log) SegmentSize() uint64 { return l.segmentSize }

// Extents returns the canonical segment extents.
func (l *Log) Extents() []segment.Extent { return l.locator.Extents() }

// Capacity returns the governor.
func (l *Log) Capacity() *capacity.Governor { return l.cap }

// Degraded reports the read-only latch.
func (l *Log) Degraded() (bool, string) { return l.cap.Degraded() }

// appendPos is the current end of the canonical byte stream (>= committedRoot
// once an aborted transaction has been written).
func (l *Log) appendPos() uint64 { return l.writerID + l.writerValid }

// PhysicalBytes is the retained authoritative bytes: sum of segment sizes plus
// the watermark.
func (l *Log) PhysicalBytes() (uint64, error) {
	var total uint64
	for _, e := range l.locator.Extents() {
		path := filepath.Join(l.dir, segment.SegmentName(e.ID))
		fi, err := os.Stat(path)
		if err != nil {
			return 0, orfserr.IO(err, "stat segment")
		}
		total += uint64(fi.Size())
	}
	if fi, err := os.Stat(filepath.Join(l.dir, wm.FileName)); err == nil {
		total += uint64(fi.Size())
	}
	return total, nil
}

// preparedTx is a fully framed transaction ready to append: TX_BEGIN, the data
// records, and a TX_COMMIT whose bracket checksum covers the exact bytes. The
// frame bytes are identical to a per-publish commit; only the moment of the
// durability barrier differs.
type preparedTx struct {
	frames       [][]byte
	txID         uint64
	commitSeq    uint64
	bracketBytes uint64
	grandTotal   uint64
}

// Publish writes one committed transaction: TX_BEGIN, the data records, and a
// TX_COMMIT whose bracket is verified against the exact bytes written. The whole
// framed transaction is admitted against the capacity guard before the first
// byte is written.
//
// With group commit enabled (WithGroupCommit / ORFS_GROUP_COMMIT=1), concurrent
// Publish calls are coalesced behind one durability barrier.
func (l *Log) Publish(data []*record.Record) (uint64, error) {
	if l.mode == ReadOnly {
		return 0, orfserr.ReadOnly("log opened read-only")
	}
	if degraded, reason := l.cap.Degraded(); degraded {
		return 0, orfserr.ReadOnly("volume degraded: %s", reason)
	}
	if l.group != nil {
		return l.group.publish(data)
	}
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	return l.publishOne(data)
}

// publishOne commits a single transaction with its own barrier. Callers must
// hold pubMu (or be the single group committer).
func (l *Log) publishOne(data []*record.Record) (uint64, error) {
	p, err := l.prepareTx(data, l.maxTxID+1, l.nextSeq, l.appendPos())
	if err != nil {
		return 0, err
	}
	for _, f := range p.frames {
		if err := l.writeFrame(f); err != nil {
			return 0, err
		}
	}
	if err := l.syncWriter(); err != nil {
		return 0, err
	}
	l.applyTx(p, l.appendPos())
	if err := l.persistWatermark(); err != nil {
		return 0, err
	}
	return l.committedRoot, nil
}

// prepareTx builds the frames of one transaction for an explicit transaction id,
// sequence base, and append position. It performs the whole-transaction capacity
// pre-flight against pos before any byte is written, and never mutates log
// state. The projected append position advances by grandTotal.
func (l *Log) prepareTx(data []*record.Record, txID, seq, pos uint64) (*preparedTx, error) {
	beginTotal := uint64(record.EnvelopeOverhead + record.TxBeginPayloadLen)
	bracketBytes := beginTotal
	dataTotals := make([]uint64, len(data))
	for i, r := range data {
		t, err := r.Header.TotalLen()
		if err != nil {
			return nil, err
		}
		dataTotals[i] = t
		bracketBytes += t
	}
	commitTotal := uint64(record.EnvelopeOverhead + record.TxCommitPayloadLen)
	grandTotal := bracketBytes + commitTotal

	// Capacity pre-flight: entire framed transaction, before any byte.
	if err := l.cap.CheckBackingReserve(l.dir, grandTotal); err != nil {
		return nil, err
	}
	if err := l.cap.CheckAuthoritative(pos, grandTotal); err != nil {
		return nil, err
	}

	beginPayload := record.TxBegin{
		TransactionID:       txID,
		ExpectedByteLength:  bracketBytes,
		ExpectedRecordCount: uint32(len(data) + 1),
	}.Encode()
	begin := &record.Record{
		Header: record.Header{
			FormatVersion:  record.FormatVersion,
			RecordType:     record.TypeTxBegin,
			PayloadLength:  uint64(len(beginPayload)),
			SequenceNumber: seq,
			TransactionID:  txID,
		},
		Payload: beginPayload,
	}
	beginFrame, err := begin.Encode()
	if err != nil {
		return nil, err
	}
	hash := blake3.New(record.BracketChecksumLen, nil)
	hash.Write(beginFrame)

	frames := make([][]byte, 0, len(data)+2)
	frames = append(frames, beginFrame)
	for i, r := range data {
		r.Header.FormatVersion = record.FormatVersion
		r.Header.SequenceNumber = seq + 1 + uint64(i)
		r.Header.TransactionID = txID
		f, err := r.Encode()
		if err != nil {
			return nil, err
		}
		if uint64(len(f)) != dataTotals[i] {
			return nil, orfserr.Invalid("data record %d framed length %d != projected %d", i, len(f), dataTotals[i])
		}
		hash.Write(f)
		frames = append(frames, f)
	}
	commitSeq := seq + uint64(len(data)) + 1
	var bracket [record.BracketChecksumLen]byte
	copy(bracket[:], hash.Sum(nil))
	commitPayload := record.TxCommit{
		TransactionID:        txID,
		RecordCount:          uint64(len(data) + 1),
		BracketedByteLength:  bracketBytes,
		CommitSequenceNumber: commitSeq,
		BracketChecksum:      bracket,
	}.Encode()
	commit := &record.Record{
		Header: record.Header{
			FormatVersion:  record.FormatVersion,
			RecordType:     record.TypeTxCommit,
			PayloadLength:  uint64(len(commitPayload)),
			SequenceNumber: commitSeq,
			TransactionID:  txID,
		},
		Payload: commitPayload,
	}
	commitFrame, err := commit.Encode()
	if err != nil {
		return nil, err
	}
	frames = append(frames, commitFrame)

	return &preparedTx{
		frames:       frames,
		txID:         txID,
		commitSeq:    commitSeq,
		bracketBytes: bracketBytes,
		grandTotal:   grandTotal,
	}, nil
}

// applyTx advances the committed prefix for one transaction whose bytes are
// durable through root.
func (l *Log) applyTx(p *preparedTx, root uint64) {
	l.nextSeq = p.commitSeq + 1
	l.committedRoot = root
	l.committedRecords = l.nextSeq
	l.boundaries = append(l.boundaries, root)
	l.maxTxID = p.txID
}

// syncWriter takes the durability barrier. The syncHook seam lets a test force
// a barrier failure.
func (l *Log) syncWriter() error {
	if l.syncHook != nil {
		return l.syncHook()
	}
	return l.writer.Sync()
}

// writeFrame appends one framed record, rolling over to a fresh segment when it
// would not fit (a record never straddles two segments).
func (l *Log) writeFrame(frame []byte) error {
	n := uint64(len(frame))
	if l.writerValid+n > l.segmentSize && l.writerValid > 0 {
		if err := l.writer.Sync(); err != nil {
			return err
		}
		if err := l.writer.Close(); err != nil {
			return err
		}
		newID := l.appendPos()
		seg, err := segment.Create(l.dir, newID)
		if err != nil {
			return err
		}
		if err := l.locator.Append(segment.Extent{ID: newID, ValidLen: 0}); err != nil {
			seg.Close()
			return err
		}
		l.writer = seg
		l.writerID = newID
		l.writerValid = 0
	}
	if _, err := l.writer.WriteAt(frame, int64(l.writerValid)); err != nil {
		return orfserr.IO(err, "append record")
	}
	l.writerValid += n
	l.locator.SetLastValid(l.writerValid)
	return nil
}

// persistWatermark writes the watermark after the bytes it describes are durable.
func (l *Log) persistWatermark() error {
	w := wm.Watermark{
		FormatVersion:   record.FormatVersion,
		CommittedRoot:   l.committedRoot,
		SegmentID:       l.writerID,
		SegmentValidLen: l.writerValid,
	}
	if err := wm.Write(l.dir, w); err != nil {
		return err
	}
	l.durableWatermark = &w
	return nil
}

// truncateExtents clamps the validated extents to the committed prefix: extents
// wholly before committed are kept, and the extent straddling it is clamped so
// its end is exactly committed.
func truncateExtents(extents []segment.Extent, committed uint64) []segment.Extent {
	var out []segment.Extent
	for _, e := range extents {
		if e.ID >= committed {
			break
		}
		if e.End() <= committed {
			out = append(out, e)
		} else {
			out = append(out, segment.Extent{ID: e.ID, ValidLen: committed - e.ID})
			break
		}
	}
	return out
}

// quarantineAfter moves every canonical segment at or beyond the committed root
// into quarantine/ (retaining every byte).
func quarantineAfter(dir string, extents []segment.Extent, committed uint64) error {
	var moved bool
	for _, e := range extents {
		if e.ID < committed {
			continue
		}
		if err := quarantineSegment(dir, e.ID); err != nil {
			return err
		}
		moved = true
	}
	if !moved {
		return nil
	}
	qdir := filepath.Join(dir, segment.QuarantineDir)
	if err := syncDir(qdir); err != nil {
		return err
	}
	return syncDir(dir)
}

// quarantineSegment moves one segment file into dir/quarantine/, never
// truncating, disambiguating a name collision with a numeric suffix.
func quarantineSegment(dir string, id uint64) error {
	name := segment.SegmentName(id)
	src := filepath.Join(dir, name)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return orfserr.IO(err, "stat segment for quarantine")
	}
	qdir := filepath.Join(dir, segment.QuarantineDir)
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return orfserr.IO(err, "create quarantine directory")
	}
	dst := filepath.Join(qdir, name)
	for n := 1; ; n++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(qdir, name+"."+itoa(uint64(n)))
	}
	if err := os.Rename(src, dst); err != nil {
		return orfserr.IO(err, "quarantine segment %s", name)
	}
	return nil
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

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
