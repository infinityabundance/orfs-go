package orbench

import (
	"fmt"
	"runtime"
	"time"

	"github.com/infinityabundance/orfs-go/internal/checkpoint"
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// RecoveryRow is one measurement at one retained-history size and workload.
type RecoveryRow struct {
	K                int    `json:"k"`
	Workload         string `json:"workload"`
	TargetBytes      uint64 `json:"target_bytes"`
	Transactions     uint64 `json:"transactions"`
	CommittedRecords uint64 `json:"committed_records"`
	CommittedRoot    uint64 `json:"committed_root"`

	// Full replay arm.
	FullMicros          int64  `json:"full_replay_micros"`
	FullBytesReplayed   uint64 `json:"full_replay_bytes"`
	FullRecordsReplayed uint64 `json:"full_replay_records"`
	FullHeapInuse       uint64 `json:"full_heap_inuse_bytes"`

	// Checkpoint + suffix replay arm (log-scan discovery).
	CheckpointMicros          int64  `json:"checkpoint_micros"`
	CheckpointBytesReplayed   uint64 `json:"checkpoint_bytes_replayed"`
	CheckpointRecordsReplayed uint64 `json:"checkpoint_records_replayed"`
	CheckpointRoot            uint64 `json:"checkpoint_root"`
	UsedCheckpoint            bool   `json:"used_checkpoint"`
	CheckpointHeapInuse       uint64 `json:"checkpoint_heap_inuse_bytes"`

	// Directory-assisted arm.
	DirectoryMicros int64  `json:"directory_micros"`
	DiscoverySource string `json:"directory_discovery_source"`

	// End-to-end mount (volume.Open), which still scans the authoritative log.
	OpenFullMicros       int64 `json:"open_full_micros"`
	OpenCheckpointMicros int64 `json:"open_checkpoint_micros"`

	// Suffix fraction replayed by the checkpoint arm (checkpoint_bytes/committed).
	SuffixFraction float64 `json:"suffix_fraction"`

	// Whether the bounded reconstruction equals the full replay at this size.
	Correct bool `json:"correct_equals_full_replay"`

	// GoSysBytes is process-wide (labeled honestly: Go runtime, not RSS).
	GoSysBytes uint64 `json:"go_sys_bytes"`
}

// RecoveryReceipt is evidence/recovery/checkpoints.json.
type RecoveryReceipt struct {
	Schema    string        `json:"schema"`
	Method    string        `json:"method"`
	Note      string        `json:"note"`
	Reps      int           `json:"reps"`
	KMin      int           `json:"k_min"`
	KMax      int           `json:"k_max"`
	Workloads []string      `json:"workloads"`
	Rows      []RecoveryRow `json:"rows"`
}

// The two workloads isolate the two costs. `namespace` creates a new file per
// transaction, so the index grows with the log (the checkpoint must decode an
// index that grows). `overwrite` rewrites one file, so the log grows while the
// index stays constant (the checkpoint decode is flat and only the log scan and
// replay scale) — the shape the milestone targets.
const (
	WorkloadNamespace = "namespace"
	WorkloadOverwrite = "overwrite"
)

// RunRecoveryCourt measures index-reconstruction work and Go heap across
// exponentially increasing retained histories (2^k committed bytes for k in
// [kmin, kmax]), for each workload, comparing full replay, checkpoint + suffix
// replay, and directory-assisted discovery. It asserts equality with full
// replay at every size; a mismatch is recorded as correct=false and the caller
// fails.
//
// Each timed arm is measured `reps` times and the minimum is reported, so a
// stray GC or scheduler hiccup does not distort the curve; the best-of-reps
// methodology is recorded in the receipt.
func RunRecoveryCourt(backingRoot string, kmin, kmax, reps int) (RecoveryReceipt, error) {
	if reps < 1 {
		reps = 3
	}
	rec := RecoveryReceipt{
		Schema: "orfs.recovery.checkpoints.v1",
		Method: "for each workload and size 2^k committed bytes: build a volume, take an index checkpoint, " +
			"then time (a) full replay, (b) checkpoint + suffix replay (log-scan discovery), " +
			"(c) directory-assisted discovery, and report bytes/records replayed and Go heap (runtime.MemStats).",
		Note: "Go heap (HeapInuse) and Go runtime Sys are reported, not process RSS. " +
			"Each timed arm is the minimum of `reps` runs. " +
			"volume.Open still scans the authoritative log to establish the committed root in every arm; " +
			"the checkpoint bounds only the index reconstruction. " +
			"Absolute times are single-host and sensitive to cache/GC state.",
		Reps:      reps,
		KMin:      kmin,
		KMax:      kmax,
		Workloads: []string{WorkloadNamespace, WorkloadOverwrite},
	}
	if kmin < 6 {
		kmin = 6
	}
	if kmax < kmin {
		return rec, fmt.Errorf("kmax %d < kmin %d", kmax, kmin)
	}
	// Bound the total work so a stray large k cannot run away in the capped container.
	const maxBytes = 1 << 26 // 64 MiB per size
	const maxIterations = 4_000_000

	for _, wl := range rec.Workloads {
		for k := kmin; k <= kmax; k++ {
			target := uint64(1) << uint(k)
			if target > maxBytes {
				target = maxBytes
			}
			backing := fmt.Sprintf("%s/%s-k%d", backingRoot, wl, k)
			row, err := measureSize(backing, wl, k, target, maxIterations, reps)
			if err != nil {
				return rec, fmt.Errorf("%s k=%d: %w", wl, k, err)
			}
			rec.Rows = append(rec.Rows, row)
		}
	}
	return rec, nil
}

func measureSize(backing, workload string, k int, target uint64, maxIterations, reps int) (RecoveryRow, error) {
	row := RecoveryRow{K: k, Workload: workload, TargetBytes: target}

	txns, err := grow(backing, target, maxIterations, workload)
	if err != nil {
		return row, err
	}
	row.Transactions = txns

	// Prime the derived committed-root directory with a writable open.
	v, err := volume.Open(backing, log.ReadWrite)
	if err != nil {
		return row, fmt.Errorf("prime writable open: %w", err)
	}
	v.Close()

	l, err := log.Open(backing, log.ReadOnly)
	if err != nil {
		return row, fmt.Errorf("open log: %w", err)
	}
	defer l.Close()
	dir := log.Dir(backing)
	row.CommittedRoot = l.CommittedRoot()
	row.CommittedRecords = l.CommittedRecordCount()

	// (a) full replay.
	var full *index.Index
	row.FullMicros, err = timeMin(reps, func() error {
		idx, e := index.Replay(dir, l.Extents(), l.CommittedRoot())
		if e == nil {
			full = idx
		}
		return e
	})
	if err != nil {
		return row, fmt.Errorf("full replay: %w", err)
	}
	row.FullBytesReplayed = l.CommittedRoot()
	row.FullRecordsReplayed = l.CommittedRecordCount()
	row.FullHeapInuse = heapInuse()

	// (b) checkpoint + suffix replay via a header-only discovery scan.
	var ckIdx *index.Index
	var ckRep *checkpoint.Report
	row.CheckpointMicros, err = timeMin(reps, func() error {
		idx, rep, e := checkpoint.Mount(l, checkpoint.Options{Corrupt: checkpoint.FallbackFull})
		if e == nil {
			ckIdx, ckRep = idx, rep
		}
		return e
	})
	if err != nil {
		return row, fmt.Errorf("checkpoint mount: %w", err)
	}
	row.UsedCheckpoint = ckRep.UsedCheckpoint
	row.CheckpointRoot = ckRep.CheckpointRoot
	row.CheckpointBytesReplayed = ckRep.BytesReplayed
	row.CheckpointRecordsReplayed = ckRep.RecordsReplayed
	row.CheckpointHeapInuse = heapInuse()
	if row.CommittedRoot > 0 {
		row.SuffixFraction = float64(row.CheckpointBytesReplayed) / float64(row.CommittedRoot)
	}

	// (c) directory-assisted discovery.
	var dirIdx *index.Index
	var dirRep *checkpoint.Report
	row.DirectoryMicros, err = timeMin(reps, func() error {
		idx, rep, e := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
		if e == nil {
			dirIdx, dirRep = idx, rep
		}
		return e
	})
	if err != nil {
		return row, fmt.Errorf("directory mount: %w", err)
	}
	row.DiscoverySource = string(dirRep.DiscoverySource)

	// Correctness at this size: both bounded reconstructions must equal full replay.
	row.Correct = ckIdx.Equal(full) && dirIdx.Equal(full)

	// End-to-end mount: full-replay baseline versus checkpoint-aware, both best of
	// `reps` (the authoritative scan is shared by both arms).
	row.OpenFullMicros, err = timeMin(reps, func() error {
		vf, e := volume.Open(backing, log.ReadOnly, volume.WithForceFullReplay())
		if e != nil {
			return e
		}
		return vf.Close()
	})
	if err != nil {
		return row, fmt.Errorf("open full: %w", err)
	}
	row.OpenCheckpointMicros, err = timeMin(reps, func() error {
		vc, e := volume.Open(backing, log.ReadOnly)
		if e != nil {
			return e
		}
		if !vc.Index().Equal(full) {
			row.Correct = false
		}
		return vc.Close()
	})
	if err != nil {
		return row, fmt.Errorf("open checkpoint: %w", err)
	}

	row.GoSysBytes = goSys()
	return row, nil
}

// timeMin runs fn `reps` times and returns the minimum elapsed microseconds.
func timeMin(reps int, fn func() error) (int64, error) {
	best := int64(-1)
	for i := 0; i < reps; i++ {
		start := time.Now()
		if err := fn(); err != nil {
			return 0, err
		}
		if d := time.Since(start).Microseconds(); best < 0 || d < best {
			best = d
		}
	}
	return best, nil
}

// grow builds a volume reaching ~target committed bytes for the given workload,
// taking a mid-history checkpoint and a final one, and returns the transaction
// count. It is hard-bounded by maxIterations so it can never run away.
func grow(backing string, target uint64, maxIterations int, workload string) (uint64, error) {
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 1 << 20})
	if err != nil {
		return 0, err
	}
	ns := namespace.New(v)
	cred := namespace.Credential{}
	var txns uint64
	midTaken := false
	maybeMid := func() error {
		if !midTaken && target > 4096 && v.Log.CommittedRoot() >= target/2 {
			if _, err := v.TakeCheckpoint(); err != nil {
				return err
			}
			midTaken = true
		}
		return nil
	}

	if workload == WorkloadOverwrite {
		f, err := ns.Create(cred, record.RootInode, "hot.bin", 0o644, 0, 0)
		if err != nil {
			v.Close()
			return txns, err
		}
		payload := make([]byte, 4096)
		for i := 0; i < maxIterations; i++ {
			if v.Log.CommittedRoot() >= target {
				break
			}
			if err := maybeMid(); err != nil {
				v.Close()
				return txns, err
			}
			if _, err := ns.Write(cred, f.ID, 0, payload); err != nil {
				v.Close()
				return txns, err
			}
			txns++
		}
	} else {
		payload := []byte("orfs-recovery-payload")
		for i := 0; i < maxIterations; i++ {
			if v.Log.CommittedRoot() >= target {
				break
			}
			if err := maybeMid(); err != nil {
				v.Close()
				return txns, err
			}
			name := fmt.Sprintf("f%07d", i)
			f, err := ns.Create(cred, record.RootInode, name, 0o644, 0, 0)
			if err != nil {
				v.Close()
				return txns, err
			}
			if _, err := ns.Write(cred, f.ID, 0, payload); err != nil {
				v.Close()
				return txns, err
			}
			txns += 2
		}
	}

	if _, err := v.TakeCheckpoint(); err != nil {
		v.Close()
		return txns, err
	}
	if err := v.Close(); err != nil {
		return txns, err
	}
	return txns, nil
}

func heapInuse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

func goSys() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}
