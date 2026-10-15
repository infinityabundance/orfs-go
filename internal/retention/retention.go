// Package retention holds the shared, testable vocabulary of the retention
// courts: the Btrfs retained-snapshots court (courts/retention-btrfs) and the
// OpenZFS retained-snapshot court (courts/retention-zfs). It defines the
// measurement a single bounded arm produces (cmd/retentionbench), the OpenZFS
// space/snapshot accounting, and the merged receipt the courts publish
// (cmd/retentionsummary).
//
// The court answers a retention question, not an IOPS question: can a system
// retain every committed historical transition while achieving comparable tail
// latency, throughput and physical-space economics? OrFS retains history in its
// authoritative log (non-withdrawal, Law I); Btrfs retains it with read-only
// snapshots. Both are driven by one defined, bounded epoch workload.
//
// The receipt is *data, not a leaderboard*: it never ranks incomparable
// workloads. Merge preserves every arm as its own row keyed by
// {system, kind, state, epoch, writers, readers, block_size} and sorts them
// deterministically so a diff of two receipts is meaningful.
package retention

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"syscall"
)

// Schemas published by this court.
const (
	MeasurementSchema = "orfs.retention.measure.v1"
	SpaceSchema       = "orfs.retention.space.v1"
	SnapshotSchema    = "orfs.retention.snapshot.v1"
	ReceiptSchema     = "orfs.retention.btrfs.v1"
	// ZFSReceiptSchema is the receipt schema of the OpenZFS retention court
	// (courts/retention-zfs), which reuses this merged vocabulary.
	ZFSReceiptSchema = "orfs.retention.zfs.v1"
)

// Measured arm kinds.
const (
	KindAppend = "append"
	KindRead   = "read"
)

// State labels. "current" is the live/current committed state; "snapshot:"
// prefixes a retained historical state (the snapshot taken after an epoch).
const (
	StateAppend   = "append"
	StateCurrent  = "current"
	StateSnapshot = "snapshot"
)

// Stats is one arm's measured distribution. Latencies are exact observed
// samples (the per-worker slices are merged and sorted, so a p99.9 is a real
// sample, not a bucket edge). Errors are classified by errno class name.
//
// The shape mirrors internal/osscompare.Stats so an operator reads the same
// latency vocabulary across the mounted courts.
type Stats struct {
	Ops            uint64            `json:"ops"`
	Bytes          uint64            `json:"bytes"`
	OpsPerSecond   float64           `json:"ops_per_second"`
	BytesPerSecond float64           `json:"bytes_per_second"`
	MeanNs         int64             `json:"mean_ns"`
	P50Ns          int64             `json:"p50_ns"`
	P95Ns          int64             `json:"p95_ns"`
	P99Ns          int64             `json:"p99_ns"`
	P999Ns         int64             `json:"p99_9_ns"`
	MaxNs          int64             `json:"max_ns"`
	Sampled        uint64            `json:"sampled"`
	Truncated      bool              `json:"sample_buffer_truncated"`
	Errors         uint64            `json:"errors"`
	ErrorRate      float64           `json:"error_rate"`
	ErrorClasses   map[string]uint64 `json:"error_classes"`
}

// Measure is one bounded arm (schema orfs.retention.measure.v1): an append epoch
// or a read of the current state or of a retained snapshot.
type Measure struct {
	Schema      string `json:"schema"`
	System      string `json:"system"` // "Btrfs" | "OrFS-GO"
	Kind        string `json:"kind"`   // append | read
	State       string `json:"state"`  // append | current | snapshot
	Epoch       int    `json:"epoch"`
	Writers     int    `json:"writers"`
	Readers     int    `json:"readers"`
	BlockSize   int    `json:"block_size"`
	RecordBytes int    `json:"record_bytes,omitempty"`
	// Records is the defined epoch size (records appended per writer); an
	// append arm is bounded by it, so every committed state is well defined.
	Records        int     `json:"records,omitempty"`
	RequestSeconds float64 `json:"request_seconds"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	Seed           int64   `json:"seed"`
	Durability     string  `json:"durability"`
	// TimeCapped is true when an append epoch ended at its time safety bound
	// before its defined record count (a completed epoch is never capped).
	TimeCapped bool   `json:"time_capped,omitempty"`
	Stats      Stats  `json:"stats"`
	Note       string `json:"note"`
}

// Space is the physical-space accounting for one committed state (schema
// orfs.retention.space.v1). PhysicalBytes is the retained physical cost (the
// Btrfs device Used, or the authoritative bytes of an OrFS volume); LogicalBytes
// is the apparent size of the data that state presents.
//
// Total/Exclusive/SetShared are the Btrfs per-subvolume accounting: the exclusive
// bytes are what a retained snapshot costs *uniquely*, and SetShared is what it
// shares with other subvolumes. They are zero for the OrFS arm, which has no
// snapshot sharing.
type Space struct {
	Schema         string `json:"schema"`
	System         string `json:"system"`
	Epoch          int    `json:"epoch"`
	State          string `json:"state"`
	PhysicalBytes  int64  `json:"physical_bytes"`
	LogicalBytes   int64  `json:"logical_bytes"`
	TotalBytes     int64  `json:"total_bytes,omitempty"`
	ExclusiveBytes int64  `json:"exclusive_bytes,omitempty"`
	SetSharedBytes int64  `json:"set_shared_bytes,omitempty"`
	// ZFS accounting (the ZFS court): the four `zfs list` figures, recorded raw.
	// For a snapshot, `used` is the space freed by destroying it.
	UsedBytes              int64  `json:"used_bytes,omitempty"`
	ReferencedBytes        int64  `json:"referenced_bytes,omitempty"`
	LogicalUsedBytes       int64  `json:"logical_used_bytes,omitempty"`
	LogicalReferencedBytes int64  `json:"logical_referenced_bytes,omitempty"`
	Note                   string `json:"note"`
}

// Snapshot records the cost of creating one read-only retained snapshot (schema
// orfs.retention.snapshot.v1).
type Snapshot struct {
	Schema        string  `json:"schema"`
	System        string  `json:"system"`
	Epoch         int     `json:"epoch"`
	Dest          string  `json:"dest"`
	CreateNs      int64   `json:"create_ns"`
	CreateSeconds float64 `json:"create_seconds"`
	Note          string  `json:"note"`
}

// Row is one line of the merged receipt: an arm reduced to its comparable facts.
type Row struct {
	System         string            `json:"system"`
	Kind           string            `json:"kind"`
	State          string            `json:"state"`
	Epoch          int               `json:"epoch"`
	Writers        int               `json:"writers"`
	Readers        int               `json:"readers"`
	BlockSize      int               `json:"block_size"`
	Ops            uint64            `json:"ops"`
	Bytes          uint64            `json:"bytes"`
	OpsPerSecond   float64           `json:"ops_per_second"`
	BytesPerSecond float64           `json:"bytes_per_second"`
	MeanNs         int64             `json:"mean_ns"`
	P50Ns          int64             `json:"p50_ns"`
	P95Ns          int64             `json:"p95_ns"`
	P99Ns          int64             `json:"p99_ns"`
	P999Ns         int64             `json:"p99_9_ns"`
	MaxNs          int64             `json:"max_ns"`
	Sampled        uint64            `json:"sampled"`
	Truncated      bool              `json:"sample_buffer_truncated"`
	ErrorRate      float64           `json:"error_rate"`
	Errors         map[string]uint64 `json:"errors"`
	ElapsedSeconds float64           `json:"elapsed_seconds"`
	TimeCapped     bool              `json:"time_capped,omitempty"`
}

// Receipt is the merged document (schema orfs.retention.btrfs.v1): one row per
// measured arm, the per-state space accounting, the snapshot-creation costs, and
// a comparability block that names every way the two systems differ.
type Receipt struct {
	Schema          string          `json:"schema"`
	GeneratedAtUnix int64           `json:"generated_at_unix"`
	Note            string          `json:"note"`
	Method          string          `json:"method"`
	Comparability   json.RawMessage `json:"comparability"`
	Rows            []Row           `json:"rows"`
	Space           []Space         `json:"space"`
	Snapshots       []Snapshot      `json:"snapshots"`
	Notes           []string        `json:"notes"`
}

// Row reduces an arm to its comparable facts. The arm's own system label is used
// (never a caller-supplied one), so a row can never be misattributed.
func (m Measure) Row() Row {
	return Row{
		System:         m.System,
		Kind:           m.Kind,
		State:          m.State,
		Epoch:          m.Epoch,
		Writers:        m.Writers,
		Readers:        m.Readers,
		BlockSize:      m.BlockSize,
		Ops:            m.Stats.Ops,
		Bytes:          m.Stats.Bytes,
		OpsPerSecond:   m.Stats.OpsPerSecond,
		BytesPerSecond: m.Stats.BytesPerSecond,
		MeanNs:         m.Stats.MeanNs,
		P50Ns:          m.Stats.P50Ns,
		P95Ns:          m.Stats.P95Ns,
		P99Ns:          m.Stats.P99Ns,
		P999Ns:         m.Stats.P999Ns,
		MaxNs:          m.Stats.MaxNs,
		Sampled:        m.Stats.Sampled,
		Truncated:      m.Stats.Truncated,
		ErrorRate:      m.Stats.ErrorRate,
		Errors:         m.Stats.ErrorClasses,
		ElapsedSeconds: m.ElapsedSeconds,
		TimeCapped:     m.TimeCapped,
	}
}

// key is the row identity the receipt promises:
// {system, kind, state, epoch, writers, readers, block_size}.
func (r Row) key() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		r.System, r.Kind, r.State, r.Epoch, r.Writers, r.Readers, r.BlockSize)
}

// Merge turns validated arms plus a comparability blob into the Btrfs court's
// receipt. It rejects duplicate keys (a receipt must not carry two rows for the
// same comparable coordinate) and any arm whose schema is not the court's.
func Merge(measures []Measure, spaces []Space, snaps []Snapshot, comparability json.RawMessage, nowUnix int64) (Receipt, error) {
	return MergeFor(measures, spaces, snaps, comparability, ReceiptSchema, "Btrfs", nowUnix)
}

// MergeFor is Merge with an explicit receipt schema and comparator name, so the
// same merge serves the Btrfs retention court and the OpenZFS retention court
// (which names "OpenZFS" and stamps ZFSReceiptSchema). Merge keeps the Btrfs
// court's output byte-identical.
func MergeFor(measures []Measure, spaces []Space, snaps []Snapshot, comparability json.RawMessage, schema, comparator string, nowUnix int64) (Receipt, error) {
	seen := make(map[string]bool, len(measures))
	rows := make([]Row, 0, len(measures))
	for i, m := range measures {
		if m.Schema != MeasurementSchema {
			return Receipt{}, fmt.Errorf("measure %d: schema %q, want %q", i, m.Schema, MeasurementSchema)
		}
		if m.System == "" {
			return Receipt{}, fmt.Errorf("measure %d: empty system label", i)
		}
		row := m.Row()
		if k := row.key(); seen[k] {
			return Receipt{}, fmt.Errorf("duplicate row for %s/%s state=%s epoch=%d writers=%d readers=%d block_size=%d",
				row.System, row.Kind, row.State, row.Epoch, row.Writers, row.Readers, row.BlockSize)
		} else {
			seen[k] = true
		}
		rows = append(rows, row)
	}
	for i, s := range spaces {
		if s.Schema != SpaceSchema {
			return Receipt{}, fmt.Errorf("space %d: schema %q, want %q", i, s.Schema, SpaceSchema)
		}
	}
	for i, s := range snaps {
		if s.Schema != SnapshotSchema {
			return Receipt{}, fmt.Errorf("snapshot %d: schema %q, want %q", i, s.Schema, SnapshotSchema)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.System != b.System {
			return a.System < b.System
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.State != b.State {
			return a.State < b.State
		}
		if a.Epoch != b.Epoch {
			return a.Epoch < b.Epoch
		}
		if a.Writers != b.Writers {
			return a.Writers < b.Writers
		}
		if a.Readers != b.Readers {
			return a.Readers < b.Readers
		}
		return a.BlockSize < b.BlockSize
	})
	sort.SliceStable(spaces, func(i, j int) bool {
		a, b := spaces[i], spaces[j]
		if a.System != b.System {
			return a.System < b.System
		}
		if a.Epoch != b.Epoch {
			return a.Epoch < b.Epoch
		}
		return a.State < b.State
	})
	sort.SliceStable(snaps, func(i, j int) bool {
		a, b := snaps[i], snaps[j]
		if a.System != b.System {
			return a.System < b.System
		}
		return a.Epoch < b.Epoch
	})
	return Receipt{
		Schema:          schema,
		GeneratedAtUnix: nowUnix,
		Note: fmt.Sprintf("A shared, deterministic driver (cmd/retentionbench) drove two systems on the same machine, same "+
			"device, same container caps: %s with a read-only snapshot after every epoch, and OrFS-GO, which "+
			"retains every committed transition in its authoritative log with no extra snapshots. This receipt is "+
			"data, not a leaderboard: rows are keyed by {system, kind, state, epoch, writers, readers, block_size} "+
			"and are never ranked across incomparable coordinates. Read the comparability block before drawing any "+
			"conclusion.", comparator),
		Comparability: comparability,
		Rows:          rows,
		Space:         spaces,
		Snapshots:     snaps,
	}, nil
}

// WorkerResult is one worker's private tally, written without a lock (each worker
// owns a distinct slice index) and aggregated single-threaded at the end.
type WorkerResult struct {
	Ops     uint64
	Bytes   uint64
	Samples []int64
	Errs    map[string]uint64
}

// Summarise merges worker results plus an elapsed window into the exact-sample
// Stats. It is the same reduction cmd/orfscompare performs, so the two mounted
// courts report latency in one vocabulary.
func Summarise(results []WorkerResult, elapsed float64) Stats {
	var ops, bytes, errs uint64
	var samples []int64
	classes := map[string]uint64{}
	for _, r := range results {
		ops += r.Ops
		bytes += r.Bytes
		for k, v := range r.Errs {
			errs += v
			classes[k] += v
		}
		samples = append(samples, r.Samples...)
	}
	s := Stats{
		Ops:          ops,
		Bytes:        bytes,
		Errors:       errs,
		Sampled:      uint64(len(samples)),
		Truncated:    ops > uint64(len(samples)),
		ErrorClasses: classes,
	}
	if elapsed > 0 {
		s.OpsPerSecond = float64(ops) / elapsed
		s.BytesPerSecond = float64(bytes) / elapsed
	}
	if ops > 0 {
		s.ErrorRate = float64(errs) / float64(ops)
	}
	if len(samples) == 0 {
		return s
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var sum int64
	for _, v := range samples {
		sum += v
	}
	s.MeanNs = sum / int64(len(samples))
	s.P50Ns = Quantile(samples, 0.50)
	s.P95Ns = Quantile(samples, 0.95)
	s.P99Ns = Quantile(samples, 0.99)
	s.P999Ns = Quantile(samples, 0.999)
	s.MaxNs = samples[len(samples)-1]
	return s
}

// Quantile returns the exact q-quantile (nearest-rank) of a sorted slice.
func Quantile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// ErrnoName classifies an error by its errno, so the receipt's error classes
// match the kernel vocabulary a storage engineer expects.
func ErrnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOENT:
			return "ENOENT"
		case syscall.EEXIST:
			return "EEXIST"
		case syscall.ENOSPC:
			return "ENOSPC"
		case syscall.EROFS:
			return "EROFS"
		case syscall.ENOTCONN:
			return "ENOTCONN"
		case syscall.EIO:
			return "EIO"
		case syscall.EINTR:
			return "EINTR"
		case syscall.EAGAIN:
			return "EAGAIN"
		case syscall.EPERM:
			return "EPERM"
		case syscall.EINVAL:
			return "EINVAL"
		case syscall.EBADF:
			return "EBADF"
		case syscall.ENFILE:
			return "ENFILE"
		case syscall.EMFILE:
			return "EMFILE"
		}
		return errno.Error()
	}
	return "OTHER:" + err.Error()
}
