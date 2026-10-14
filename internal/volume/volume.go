// Package volume owns a volume's identity: the fixed, checksummed volume.meta,
// the framed genesis record, and the init/open lifecycle.
//
// The genesis record is written first and volume.meta second, so a crash during
// init leaves an orphan log (not a volume) rather than a volume without
// authoritative content. volume.meta and the genesis payload redundantly carry
// the UUID and segment size so identity can be cross-checked against
// authoritative content.
package volume

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/infinityabundance/orfs-go/internal/capacity"
	"github.com/infinityabundance/orfs-go/internal/checkpoint"
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// MetaName is the volume metadata file name.
const MetaName = "volume.meta"

// MetaLen is the fixed volume.meta length.
const MetaLen = 44

// Meta is the volume identity sidecar.
type Meta struct {
	FormatVersion     uint16
	UUID              [16]byte
	CreatedAtUnixSecs uint64
	SegmentSize       uint64
}

// Encode renders the 44-byte volume.meta.
func (m Meta) Encode() []byte {
	b := make([]byte, MetaLen)
	binary.LittleEndian.PutUint32(b[0:4], record.VolumeMetaMagic)
	binary.LittleEndian.PutUint16(b[4:6], record.FormatVersion)
	binary.LittleEndian.PutUint16(b[6:8], 0)
	copy(b[8:24], m.UUID[:])
	binary.LittleEndian.PutUint64(b[24:32], m.CreatedAtUnixSecs)
	binary.LittleEndian.PutUint64(b[32:40], m.SegmentSize)
	binary.LittleEndian.PutUint32(b[40:44], record.CRC32C(b[0:40]))
	return b
}

// DecodeMeta parses volume.meta, accepting any known version <= the current.
func DecodeMeta(b []byte) (Meta, error) {
	var m Meta
	if len(b) != MetaLen {
		return m, orfserr.Corrupt("volume.meta is %d bytes, want %d", len(b), MetaLen)
	}
	if binary.LittleEndian.Uint32(b[0:4]) != record.VolumeMetaMagic {
		return m, orfserr.Corrupt("volume.meta magic mismatch")
	}
	if binary.LittleEndian.Uint32(b[40:44]) != record.CRC32C(b[0:40]) {
		return m, orfserr.Corrupt("volume.meta checksum mismatch")
	}
	m.FormatVersion = binary.LittleEndian.Uint16(b[4:6])
	if m.FormatVersion > record.FormatVersion {
		return m, orfserr.UnsupportedFormat(m.FormatVersion, record.FormatVersion)
	}
	copy(m.UUID[:], b[8:24])
	m.CreatedAtUnixSecs = binary.LittleEndian.Uint64(b[24:32])
	m.SegmentSize = binary.LittleEndian.Uint64(b[32:40])
	if m.SegmentSize == 0 {
		return m, orfserr.Corrupt("volume.meta segment_size is zero")
	}
	return m, nil
}

// Volume is an opened volume.
type Volume struct {
	Backing string
	Meta    Meta
	Log     *log.Log

	reader *log.CanonicalReader
	index  *index.Index

	// Checkpoint policy (opt-in) and the report of how the index was
	// reconstructed at open.
	cpEveryTxns         uint64
	cpEveryBytes        uint64
	cpCorrupt           checkpoint.CorruptPolicy
	cpForceFull         bool
	compat              bool
	sinceCheckpointTxns uint64
	lastCheckpointRoot  uint64
	mountReport         *checkpoint.Report
}

// InitOptions configures volume creation.
type InitOptions struct {
	SegmentSize uint64
	Reserve     uint64
	// UUID pins the volume identity. When nil a random v4 UUID is generated;
	// pinning it makes the on-disk bytes reproducible (the cross-FS court uses
	// this together with a fixed clock).
	UUID *[16]byte
}

// Init creates a fresh volume.
func Init(backing string, opts InitOptions, vopts ...Option) (*Volume, error) {
	if opts.SegmentSize == 0 {
		opts.SegmentSize = record.DefaultSegmentSize
	}
	initialBytes := uint64(record.EnvelopeOverhead + record.GenesisPayloadLen)
	if err := capacity.CheckReservation(backing, initialBytes, opts.Reserve); err != nil {
		return nil, err
	}
	uuid, err := randomUUID()
	if err != nil {
		return nil, err
	}
	if opts.UUID != nil {
		uuid = *opts.UUID
	}
	g := record.Genesis{UUID: uuid, SegmentSize: opts.SegmentSize}
	l, err := log.Create(backing, g)
	if err != nil {
		return nil, err
	}
	meta := Meta{
		FormatVersion:     record.FormatVersion,
		UUID:              uuid,
		CreatedAtUnixSecs: uint64(time.Now().Unix()),
		SegmentSize:       opts.SegmentSize,
	}
	if err := writeMetaAtomic(backing, meta); err != nil {
		l.Close()
		return nil, err
	}
	v := &Volume{Backing: backing, Meta: meta, Log: l}
	applyVolumeOptions(v, vopts)
	if err := v.buildIndex(true); err != nil {
		l.Close()
		return nil, err
	}
	return v, nil
}

// Open opens an existing volume. A read-only open modifies not a single byte.
func Open(backing string, mode log.Mode, vopts ...Option) (*Volume, error) {
	meta, ok, err := readMeta(backing)
	if err != nil {
		return nil, err
	}
	l, err := log.Open(backing, mode)
	if err != nil {
		return nil, err
	}
	g := l.Genesis()
	if ok {
		if meta.UUID != g.UUID {
			l.Close()
			return nil, orfserr.Corrupt("volume.meta uuid disagrees with the authoritative genesis uuid")
		}
		if meta.SegmentSize != g.SegmentSize {
			l.Close()
			return nil, orfserr.Corrupt("volume.meta segment_size %d disagrees with genesis %d", meta.SegmentSize, g.SegmentSize)
		}
	} else {
		// Legacy volume without volume.meta: derive identity from authority.
		meta = Meta{FormatVersion: g.FormatVersion, UUID: g.UUID, SegmentSize: g.SegmentSize}
	}
	v := &Volume{Backing: backing, Meta: meta, Log: l}
	applyVolumeOptions(v, vopts)
	if err := v.buildIndex(mode == log.ReadWrite); err != nil {
		l.Close()
		return nil, err
	}
	return v, nil
}

// buildIndex reconstructs the committed index, preferring the newest valid
// index checkpoint and replaying only the suffix after it. `publish` refreshes
// the derived committed-root directory (writable opens only).
func (v *Volume) buildIndex(publish bool) error {
	idx, rep, err := checkpoint.Mount(v.Log, checkpoint.Options{
		Corrupt:         v.cpCorrupt,
		UseDirectory:    true,
		Publish:         publish,
		ForceFullReplay: v.cpForceFull,
	})
	if err != nil {
		return err
	}
	v.index = idx
	v.mountReport = rep
	v.lastCheckpointRoot = v.Log.CommittedRoot()
	v.reader = log.NewCanonicalReader(log.Dir(v.Backing), v.Log.Extents())
	return nil
}

// Index returns the reconstructed namespace at the committed root.
func (v *Volume) Index() *index.Index { return v.index }

// Compat reports whether this volume is in the Rust-reference compatibility
// profile: writes stay inside the reference's registry and semantics.
func (v *Volume) Compat() bool { return v.compat }

// MountReport returns how the committed index was reconstructed at open
// (checkpoint vs full replay, discovery source, bytes replayed).
func (v *Volume) MountReport() *checkpoint.Report { return v.mountReport }

// Reader returns the canonical byte reader (used to read DATA_EXTENT bytes).
func (v *Volume) Reader() *log.CanonicalReader { return v.reader }

// Close releases the log and reader.
func (v *Volume) Close() error {
	if v.reader != nil {
		v.reader.Close()
	}
	return v.Log.Close()
}

// Sync flushes the log.
func (v *Volume) Sync() error {
	if v.Log == nil {
		return nil
	}
	// The log's writer is flushed at each Publish; a bare Sync is a no-op here.
	return nil
}

// Publish commits a transaction's data records, extends the live index by the
// newly committed suffix, and (when the opt-in policy is due) writes a
// checkpoint as a further committed transaction.
func (v *Volume) Publish(data []*record.Record) (uint64, error) {
	root, err := v.commit(data)
	if err != nil {
		return 0, err
	}
	v.maybeCheckpoint(root)
	return root, nil
}

// commit writes one transaction and replays only the newly committed suffix
// [prevRoot, root) into the live index, so the work per commit is proportional
// to the transaction, never to the retained history.
func (v *Volume) commit(data []*record.Record) (uint64, error) {
	prevRoot := v.Log.CommittedRoot()
	root, err := v.Log.Publish(data)
	if err != nil {
		return 0, err
	}
	extents := v.Log.Extents()
	v.reader.UpdateExtents(extents)
	if _, _, err := v.index.ReplayRange(log.Dir(v.Backing), extents, prevRoot, root); err != nil {
		return 0, err
	}
	return root, nil
}

// TakeCheckpoint writes an index checkpoint over the current committed index as
// its own committed transaction. The payload root names the committed boundary
// the snapshot reflects (the root current at write time); the record is then a
// normal committed record inside the prefix. It never rewrites a committed byte.
func (v *Volume) TakeCheckpoint() (uint64, error) {
	if v.Log == nil {
		return 0, orfserr.ReadOnly("no log")
	}
	doc, digest, err := index.EncodeSnapshot(v.index)
	if err != nil {
		return 0, err
	}
	payload := record.IndexCheckpoint{
		Root:        v.index.Root,
		InodeCount:  uint64(len(v.index.Inodes)),
		DentryCount: uint64(len(v.index.Dentry)),
		Digest:      digest,
		IndexBytes:  doc,
	}.Encode()
	if uint64(len(payload)) > record.MaxPayloadLen {
		return 0, orfserr.NotSupported("index checkpoint payload %d exceeds the %d-byte record limit", len(payload), record.MaxPayloadLen)
	}
	rec := &record.Record{
		Header:  record.Header{RecordType: record.TypeIndexCheckpoint, PayloadLength: uint64(len(payload)), InodeID: record.RootInode},
		Payload: payload,
	}
	root, err := v.commit([]*record.Record{rec})
	if err != nil {
		return 0, err
	}
	v.lastCheckpointRoot = root
	v.sinceCheckpointTxns = 0
	return root, nil
}

// maybeCheckpoint writes a checkpoint when the opt-in policy is due. It is
// best-effort: a failed checkpoint never fails the commit it followed, and
// changes nothing authoritative.
func (v *Volume) maybeCheckpoint(root uint64) {
	if v.cpEveryTxns == 0 && v.cpEveryBytes == 0 {
		return
	}
	v.sinceCheckpointTxns++
	due := false
	if v.cpEveryTxns > 0 && v.sinceCheckpointTxns >= v.cpEveryTxns {
		due = true
	}
	if v.cpEveryBytes > 0 && root-v.lastCheckpointRoot >= v.cpEveryBytes {
		due = true
	}
	if !due {
		return
	}
	_, _ = v.TakeCheckpoint()
}

// applyVolumeOptions merges the environment default with any explicit options.
func applyVolumeOptions(v *Volume, opts []Option) {
	o := defaultOptions()
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	v.cpEveryTxns = o.CheckpointEveryTransactions
	v.cpEveryBytes = o.CheckpointEveryBytes
	v.cpCorrupt = o.CheckpointCorrupt
	v.cpForceFull = o.ForceFullReplay
	v.compat = o.Compat
}

func randomUUID() ([16]byte, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return u, orfserr.IO(err, "generate volume uuid")
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return u, nil
}

func writeMetaAtomic(backing string, m Meta) error {
	path := filepath.Join(backing, MetaName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return orfserr.IO(err, "create volume.meta temp")
	}
	if _, err := f.Write(m.Encode()); err != nil {
		f.Close()
		return orfserr.IO(err, "write volume.meta temp")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return orfserr.IO(err, "fsync volume.meta temp")
	}
	if err := f.Close(); err != nil {
		return orfserr.IO(err, "close volume.meta temp")
	}
	if err := os.Rename(tmp, path); err != nil {
		return orfserr.IO(err, "rename volume.meta into place")
	}
	return segment.SyncDir(backing)
}

func readMeta(backing string) (Meta, bool, error) {
	b, err := os.ReadFile(filepath.Join(backing, MetaName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Meta{}, false, nil
		}
		return Meta{}, false, orfserr.IO(err, "read volume.meta")
	}
	m, err := DecodeMeta(b)
	if err != nil {
		return Meta{}, false, err
	}
	return m, true, nil
}
