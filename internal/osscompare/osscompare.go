// Package osscompare holds the shared, testable vocabulary of the open-source
// storage head-to-head court (courts/oss-compare): the per-arm run measurement a
// mounted filesystem produces (cmd/orfscompare) and the merged receipt the court
// publishes (cmd/osscomparesummary).
//
// The receipt is *data, not a leaderboard*: it never ranks incomparable
// workloads. Merge preserves every arm as its own row keyed by
// {system, workload, writers, readers, block_size} and sorts them
// deterministically so a diff of two receipts is meaningful.
package osscompare

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Schemas published by this court.
const (
	RunSchema     = "orfs.oss-compare.run.v1"
	ReceiptSchema = "orfs.oss-compare.v1"
)

// Stats is one arm's measured distribution. Latencies are exact observed
// samples (the per-worker slices are merged and sorted, so a p99.9 is a real
// sample, not a bucket edge). Errors are classified by errno class name.
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

// Run is one bounded driver measurement against one mounted filesystem
// (schema orfs.oss-compare.run.v1).
type Run struct {
	Schema         string  `json:"schema"`
	System         string  `json:"system"`
	Workload       string  `json:"workload"` // "append" | "read"
	Dir            string  `json:"dir"`
	Writers        int     `json:"writers"`
	Readers        int     `json:"readers"`
	BlockSize      int     `json:"block_size"`
	RecordBytes    int     `json:"record_bytes,omitempty"`
	RequestSeconds float64 `json:"request_seconds"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	Seed           int64   `json:"seed"`
	Durability     string  `json:"durability"`
	// WriterCapped is true when an append writer stopped at the per-writer byte
	// cap before the deadline (the arm ended early; throughput is a lower bound).
	WriterCapped bool `json:"writer_capped,omitempty"`
	// FirstErrorSeconds is the time from workload start to the first failed
	// syscall (0 when none failed): it shows how early a daemon that died took
	// the mount with it.
	FirstErrorSeconds float64 `json:"first_error_seconds"`
	Stats             Stats   `json:"stats"`
	Note              string  `json:"note"`
}

// Row is one line of the merged receipt: a run reduced to the comparable facts.
type Row struct {
	System         string            `json:"system"`
	Workload       string            `json:"workload"`
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
	// ElapsedSeconds is the measured window and WriterCapped reports whether an
	// append arm ended at the byte cap early (then ops/s and bytes/s are lower
	// bounds, not full-duration rates).
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	WriterCapped   bool    `json:"writer_capped,omitempty"`
}

// Receipt is the merged document (schema orfs.oss-compare.v1): one row per
// {system, workload, writers, readers, block_size} plus the comparability block
// that names every way the two configurations differ.
type Receipt struct {
	Schema          string          `json:"schema"`
	GeneratedAtUnix int64           `json:"generated_at_unix"`
	Note            string          `json:"note"`
	Comparability   json.RawMessage `json:"comparability"`
	Rows            []Row           `json:"rows"`
	Notes           []string        `json:"notes"`
}

// Row reduces a run to its comparable facts. The run's own system label is used
// (never a caller-supplied one), so a row can never be misattributed.
func (r Run) Row() Row {
	return Row{
		System:         r.System,
		Workload:       r.Workload,
		Writers:        r.Writers,
		Readers:        r.Readers,
		BlockSize:      r.BlockSize,
		Ops:            r.Stats.Ops,
		Bytes:          r.Stats.Bytes,
		OpsPerSecond:   r.Stats.OpsPerSecond,
		BytesPerSecond: r.Stats.BytesPerSecond,
		MeanNs:         r.Stats.MeanNs,
		P50Ns:          r.Stats.P50Ns,
		P95Ns:          r.Stats.P95Ns,
		P99Ns:          r.Stats.P99Ns,
		P999Ns:         r.Stats.P999Ns,
		MaxNs:          r.Stats.MaxNs,
		Sampled:        r.Stats.Sampled,
		Truncated:      r.Stats.Truncated,
		ErrorRate:      r.Stats.ErrorRate,
		Errors:         r.Stats.ErrorClasses,
		ElapsedSeconds: r.ElapsedSeconds,
		WriterCapped:   r.WriterCapped,
	}
}

// key is the row identity the receipt promises: {system, workload, writers,
// readers, block_size}.
func (r Row) key() string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", r.System, r.Workload, r.Writers, r.Readers, r.BlockSize)
}

// Merge turns validated runs plus a comparability blob into the receipt. It
// rejects duplicate keys (a receipt must not carry two rows for the same
// comparable coordinate) and any run whose schema is not the court's.
func Merge(runs []Run, comparability json.RawMessage, nowUnix int64) (Receipt, error) {
	seen := make(map[string]bool, len(runs))
	rows := make([]Row, 0, len(runs))
	for i, r := range runs {
		if r.Schema != RunSchema {
			return Receipt{}, fmt.Errorf("run %d: schema %q, want %q", i, r.Schema, RunSchema)
		}
		if r.System == "" {
			return Receipt{}, fmt.Errorf("run %d: empty system label", i)
		}
		row := r.Row()
		if k := row.key(); seen[k] {
			return Receipt{}, fmt.Errorf("duplicate row for %s/%s writers=%d readers=%d block_size=%d",
				row.System, row.Workload, row.Writers, row.Readers, row.BlockSize)
		} else {
			seen[k] = true
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.System != b.System {
			return a.System < b.System
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.BlockSize != b.BlockSize {
			return a.BlockSize < b.BlockSize
		}
		if a.Writers != b.Writers {
			return a.Writers < b.Writers
		}
		return a.Readers < b.Readers
	})
	return Receipt{
		Schema:          ReceiptSchema,
		GeneratedAtUnix: nowUnix,
		Note: "A shared, deterministic driver (cmd/orfscompare) drove two mounted OrFS implementations on " +
			"the same machine, same device, same container caps, with equal durability. This receipt is data, " +
			"not a leaderboard: rows are keyed by {system, workload, writers, readers, block_size} and are never " +
			"ranked across incomparable coordinates. Read the comparability block before drawing any conclusion.",
		Comparability: comparability,
		Rows:          rows,
	}, nil
}
