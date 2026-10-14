package checkpoint

import (
	"path/filepath"
	"time"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// DiscoverySource records how an index reconstruction found (or did not find) a
// checkpoint. It is reported truthfully, never assumed.
type DiscoverySource string

const (
	// SourceDirectory means the derived committed-root directory supplied the
	// checkpoint locations (possibly extended by a suffix scan).
	SourceDirectory DiscoverySource = "committed-root-directory"
	// SourceLogScan means a header-only scan of the authoritative log supplied
	// them.
	SourceLogScan DiscoverySource = "log-discovery-scan"
	// SourceFullReplay means no checkpoint validated and the index was rebuilt
	// by replaying the whole committed prefix.
	SourceFullReplay DiscoverySource = "full-replay"
)

// CorruptPolicy is the explicit policy for a *fully framed* checkpoint whose
// embedded snapshot fails to decode or fails its self-digest. It is never
// silent: either recovery falls back to a full replay (and records that it did),
// or it refuses with a typed error.
type CorruptPolicy int

const (
	// FallbackFull replays from zero when a fully framed checkpoint is corrupt,
	// recording the rejection. This is the default.
	FallbackFull CorruptPolicy = iota
	// Refuse returns a typed corrupt error instead of falling back.
	Refuse
)

func (p CorruptPolicy) String() string {
	if p == Refuse {
		return "refuse"
	}
	return "fallback-full"
}

// Options configures a checkpoint-bounded mount.
type Options struct {
	// Corrupt selects the corrupt-checkpoint policy (default FallbackFull).
	Corrupt CorruptPolicy
	// UseDirectory consults the derived committed-root directory before scanning.
	UseDirectory bool
	// Publish refreshes the derived committed-root directory after a scan. It is
	// set only for writable opens so a read-only open writes not a single byte.
	Publish bool
	// ForceFullReplay ignores every checkpoint and replays the whole prefix. It
	// exists for measurement baselines, not for recovery.
	ForceFullReplay bool
}

// Report truthfully records how an index was reconstructed.
type Report struct {
	TargetRoot          uint64          `json:"target_root"`
	UsedCheckpoint      bool            `json:"used_checkpoint"`
	CheckpointRoot      uint64          `json:"checkpoint_root,omitempty"`
	CheckpointOffset    uint64          `json:"checkpoint_offset,omitempty"`
	DiscoverySource     DiscoverySource `json:"discovery_source"`
	CheckpointsFound    int             `json:"checkpoints_found"`
	CheckpointsRejected int             `json:"checkpoints_rejected"`
	RecordsReplayed     uint64          `json:"records_replayed"`
	BytesReplayed       uint64          `json:"bytes_replayed"`
	FullReplayFallback  bool            `json:"full_replay_fallback"`
	DirectoryTrusted    bool            `json:"directory_trusted"`
	InodeCount          int             `json:"index_inode_count"`
	DentryCount         int             `json:"index_dentry_count"`
	WallMicros          uint64          `json:"wall_micros"`
}

var checkpointTypes = map[uint16]bool{record.TypeIndexCheckpoint: true}

// Mount reconstructs the committed index of l, preferring the newest valid
// INDEX_CHECKPOINT in the committed prefix and replaying only the suffix after
// it. Discovery uses the derived committed-root directory when it is trusted
// (and scans only the part of the log the directory does not cover), else a
// header-only scan. Every candidate is read from the log and fully validated
// before it is trusted; a corrupt directory or checkpoint can only ever cost a
// fallback, never a wrong result.
func Mount(l *log.Log, opt Options) (*index.Index, *Report, error) {
	started := time.Now()
	target := l.CommittedRoot()
	backing := filepath.Dir(l.Dir())
	uuid := l.Genesis().UUID
	boundaries := boundarySet(l.Boundaries())

	rep := &Report{TargetRoot: target, DiscoverySource: SourceLogScan}

	if !opt.ForceFullReplay {
		offsets, source, scanned, err := discover(l, backing, uuid, target, opt)
		if err != nil {
			return nil, nil, err
		}
		rep.DiscoverySource = source
		rep.CheckpointsFound = len(offsets)
		for i := len(offsets) - 1; i >= 0; i-- {
			off := offsets[i]
			idx, cpRoot, ok, err := tryCandidate(l, off, target, boundaries, opt.Corrupt)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				rep.CheckpointsRejected++
				continue
			}
			records, bytes, err := idx.ReplayRange(l.Dir(), l.Extents(), cpRoot, target)
			if err != nil {
				return nil, nil, err
			}
			rep.UsedCheckpoint = true
			rep.CheckpointRoot = cpRoot
			rep.CheckpointOffset = off
			rep.RecordsReplayed = records
			rep.BytesReplayed = bytes
			rep.InodeCount = len(idx.Inodes)
			rep.DentryCount = len(idx.Dentry)
			rep.WallMicros = uint64(time.Since(started).Microseconds())
			rep.DirectoryTrusted = source == SourceDirectory
			publish(backing, uuid, target, offsets, scanned, opt)
			return idx, rep, nil
		}
		// Every candidate was rejected: record the discovered locations so a later
		// mount need not rescan, then fall through to a full replay.
		publish(backing, uuid, target, offsets, scanned, opt)
	}

	// No usable checkpoint (or a forced baseline): rebuild from genesis. The
	// reconstruction is identical to the bounded one.
	idx, err := index.Replay(l.Dir(), l.Extents(), target)
	if err != nil {
		return nil, nil, err
	}
	rep.FullReplayFallback = true
	rep.DiscoverySource = SourceFullReplay
	rep.RecordsReplayed = l.CommittedRecordCount()
	rep.BytesReplayed = target
	rep.InodeCount = len(idx.Inodes)
	rep.DentryCount = len(idx.Dentry)
	rep.WallMicros = uint64(time.Since(started).Microseconds())
	return idx, rep, nil
}

// discover returns the ascending checkpoint offsets in [0, target), how they
// were found, and whether an authoritative scan was performed.
func discover(l *log.Log, backing string, uuid [16]byte, target uint64, opt Options) ([]uint64, DiscoverySource, bool, error) {
	if opt.UseDirectory {
		d, ok, err := ReadDirectory(backing, uuid)
		if err != nil {
			return nil, "", false, err
		}
		if ok && d.ScannedRoot <= target {
			if d.ScannedRoot == target {
				return append([]uint64(nil), d.Offsets...), SourceDirectory, false, nil
			}
			// The log grew past what the directory covered: scan only the suffix.
			if more, err := log.ScanRecordOffsets(l.Dir(), l.Extents(), d.ScannedRoot, target, checkpointTypes); err == nil {
				offsets := append([]uint64(nil), d.Offsets...)
				for _, m := range more {
					offsets = append(offsets, m.Offset)
				}
				return offsets, SourceDirectory, true, nil
			}
			// The covered bound does not sit on a record boundary (a corrupt
			// directory): distrust it and scan authoritatively.
		}
		// Missing, foreign, corrupt, or over-claiming (ScannedRoot > target):
		// distrust and scan authoritatively.
	}
	locs, err := log.ScanRecordOffsets(l.Dir(), l.Extents(), 0, target, checkpointTypes)
	if err != nil {
		return nil, "", false, err
	}
	offsets := make([]uint64, 0, len(locs))
	for _, r := range locs {
		offsets = append(offsets, r.Offset)
	}
	return offsets, SourceLogScan, true, nil
}

// tryCandidate reads, decodes, and validates one checkpoint, then replays the
// suffix [checkpoint_root, target) into a fresh index. ok=false means the
// candidate was rejected (torn frame or structurally invalid) or, when the
// policy is FallbackFull, corrupt. Under Refuse a corrupt candidate returns a
// typed error.
func tryCandidate(l *log.Log, offset, target uint64, boundaries map[uint64]bool, policy CorruptPolicy) (*index.Index, uint64, bool, error) {
	rec, err := log.ReadRecordAt(l.Dir(), l.Extents(), offset)
	if err != nil {
		// Torn/partial frame: never accepted, silently skip (contract: incomplete
		// is never accepted).
		return nil, 0, false, nil
	}
	if rec.Header.RecordType != record.TypeIndexCheckpoint {
		return nil, 0, false, nil
	}
	corrupt := func(err error) (*index.Index, uint64, bool, error) {
		if policy == Refuse {
			return nil, 0, false, orfserr.Corrupt("corrupt index checkpoint at offset %d: %v", offset, err)
		}
		return nil, 0, false, nil
	}
	cp, err := record.DecodeIndexCheckpoint(rec.Payload)
	if err != nil {
		return corrupt(err)
	}
	idx, digest, err := index.DecodeSnapshot(cp.IndexBytes)
	if err != nil {
		return corrupt(err)
	}
	if digest != cp.Digest {
		return corrupt(orfserr.Corrupt("snapshot digest disagrees with the payload digest"))
	}
	// Structural validity: the root must be a committed boundary within the
	// prefix, and the snapshot must agree with the payload. A structurally
	// invalid checkpoint is rejected, not treated as corruption.
	if cp.Root > target || !boundaries[cp.Root] {
		return nil, 0, false, nil
	}
	if idx.Root != cp.Root {
		return nil, 0, false, nil
	}
	if uint64(len(idx.Inodes)) != cp.InodeCount || uint64(len(idx.Dentry)) != cp.DentryCount {
		return nil, 0, false, nil
	}
	return idx, cp.Root, true, nil
}

func publish(backing string, uuid [16]byte, target uint64, offsets []uint64, scanned bool, opt Options) {
	if !opt.Publish || !scanned || len(offsets) == 0 {
		return
	}
	_ = WriteDirectory(backing, uuid, Directory{ScannedRoot: target, Offsets: offsets})
}

func boundarySet(boundaries []uint64) map[uint64]bool {
	set := make(map[uint64]bool, len(boundaries))
	for _, b := range boundaries {
		set[b] = true
	}
	return set
}
