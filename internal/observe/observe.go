// Package observe collects the OrFS runtime metrics from a volume into a
// Prometheus exposition set.
//
// It is the single place that walks the backing directory, so the CLI, the
// capacity report, and the courts all report the same numbers. Every value here
// is either an on-disk fact (physical bytes, manifested bytes, logical bytes,
// quarantine) or an explicitly-labelled runtime counter taken from a live Log or
// its persisted snapshot. Nothing is invented: when a runtime value cannot be
// observed, the family is emitted as an explicit 0 whose HELP string says so.
package observe

import (
	"os"
	"path/filepath"

	"github.com/infinityabundance/orfs-go/internal/capacity"
	"github.com/infinityabundance/orfs-go/internal/fsck"
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/metrics"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// Options controls a collection.
type Options struct {
	// Runtime supplies the process-scoped counters. When nil, Collect reads the
	// persisted snapshot sidecar if present, else reports runtime families as an
	// explicit zero.
	Runtime *metrics.Runtime
	// Verify supplies a deep-verify report. When nil, verify families are an
	// explicit zero.
	Verify *fsck.Report
	// UUID identifies the volume; optional.
	UUID string
}

// Snapshot is the numeric collection result: the values behind the exposition.
type Snapshot struct {
	UUID               string
	PhysicalBytes      uint64
	ManifestedBytes    uint64
	LogicalBytes       uint64
	CommittedRoot      uint64
	Amplification      *float64
	Breach             map[string]uint64
	BreachTotal        uint64
	Degraded           bool
	DegradedReason     string
	FsyncP50           float64
	FsyncP99           float64
	FsyncCount         uint64
	FsyncSum           float64
	BatchLast          uint64
	BatchAvg           float64
	BatchCount         uint64
	QuarantineSegments uint64
	QuarantineBytes    uint64
	VerifyDeepFailures uint64
	VerifyDeepRan      bool
	RuntimeSource      string // "live", "snapshot", or "absent"
	SnapshotCapturedAt uint64
}

// Collect gathers the metrics for an opened volume. The volume may be opened
// read-only; nothing here mutates a byte.
func Collect(v *volume.Volume, opt Options) (Snapshot, error) {
	s := Snapshot{UUID: opt.UUID, Breach: map[string]uint64{}}
	if s.UUID == "" {
		s.UUID = uuidHex(v.Meta.UUID)
	}

	phys, err := v.Log.PhysicalBytes()
	if err != nil {
		return s, err
	}
	s.PhysicalBytes = phys
	s.CommittedRoot = v.Log.CommittedRoot()
	s.ManifestedBytes = dirBytes(filepath.Join(v.Backing, manifest.BloomDirName))
	s.LogicalBytes = logicalBytes(v.Index())
	if s.LogicalBytes > 0 {
		r := float64(s.CommittedRoot) / float64(s.LogicalBytes)
		s.Amplification = &r
	}
	s.QuarantineSegments, s.QuarantineBytes = quarantineTotals(v.Backing)

	if degraded, reason := v.Log.Degraded(); degraded {
		s.Degraded = true
		s.DegradedReason = reason
	}

	// Runtime counters: prefer the live process, then the persisted snapshot.
	s.RuntimeSource = "absent"
	rt := opt.Runtime
	if rt == nil {
		if snap, ok := metrics.LoadSnapshot(v.Backing); ok {
			rt = &snap.Runtime
			s.RuntimeSource = "snapshot"
			s.SnapshotCapturedAt = snap.CapturedAt
		}
	} else {
		s.RuntimeSource = "live"
	}
	if rt != nil {
		if rt.Breach != nil {
			s.Breach = rt.Breach
		}
		s.BreachTotal = rt.BreachTotal()
		s.FsyncP50 = rt.FsyncQuantile(0.5)
		s.FsyncP99 = rt.FsyncQuantile(0.99)
		s.FsyncCount = rt.FsyncCount()
		s.FsyncSum = rt.FsyncSum()
		s.BatchLast = rt.BatchLast
		s.BatchAvg = rt.BatchAvg()
		s.BatchCount = rt.BatchCount
	}

	if opt.Verify != nil {
		s.VerifyDeepRan = true
		for _, c := range opt.Verify.Checks {
			if !c.OK {
				s.VerifyDeepFailures++
			}
		}
	}
	return s, nil
}

// logicalBytes is the sum of the sizes of the live regular files: what a user
// sees, and the denominator of the amplification ratio.
func logicalBytes(idx *index.Index) uint64 {
	var total uint64
	if idx == nil {
		return 0
	}
	for _, in := range idx.Inodes {
		if in.Kind == record.KindFile && in.Live {
			total += in.Size
		}
	}
	return total
}

// quarantineTotals counts the retained abandoned-tail segments and their bytes.
// They only ever grow (recovery moves, never truncates), so they are a counter.
func quarantineTotals(backing string) (segments, bytes uint64) {
	dir := filepath.Join(log.Dir(backing), segment.QuarantineDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := segment.ParseSegmentName(e.Name()); !ok {
			continue
		}
		segments++
		if info, err := e.Info(); err == nil {
			bytes += uint64(info.Size())
		}
	}
	return segments, bytes
}

func dirBytes(dir string) uint64 {
	var total uint64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

func uuidHex(u [16]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 32)
	for _, b := range u {
		out = append(out, hexdigits[b>>4], hexdigits[b&0xf])
	}
	return string(out)
}

// runtimeHelp appends the runtime source to a HELP string so a zero is never
// mistaken for a measurement.
func runtimeHelp(base, source string) string {
	switch source {
	case "live":
		return base + " Source: this process's live counters."
	case "snapshot":
		return base + " Source: persisted snapshot from the last mounted run."
	default:
		return base + " Source: none present (read-only inspection); reported as 0."
	}
}

// Set renders the Snapshot as a Prometheus exposition set.
func (s Snapshot) Set() *metrics.Set {
	set := metrics.NewSet()

	set.Gauge("orfs_physical_bytes",
		"Authoritative retained bytes on the backing: committed or.log/*.seg plus commit.watermark (excludes the derived .or_bloom/).",
		float64(s.PhysicalBytes))
	set.Gauge("orfs_authoritative_bytes",
		"The committed root R: the end offset of the authoritative logical byte stream.",
		float64(s.CommittedRoot))
	set.Gauge("orfs_logical_bytes",
		"Bytes of live regular-file content in the namespace (the amplification denominator).",
		float64(s.LogicalBytes))
	set.Gauge("orfs_manifested_bytes",
		"Bytes under the derived .or_bloom/ manifestation directory.",
		float64(s.ManifestedBytes))

	amp := 0.0
	if s.Amplification != nil {
		amp = *s.Amplification
	}
	set.Gauge("orfs_amplification_ratio",
		"Authoritative committed root divided by logical regular-file bytes; >= 1 is the framing/retention tax of append-only history. Reported as 0 when the volume holds no regular-file bytes.",
		amp)

	set.Gauge("orfs_degraded",
		"1 when the volume has latched read-only (a capacity degradation), else 0.",
		boolFloat(s.Degraded))

	breachHelp := "Capacity-guard admission refusals by reason; a refusal denies growth before the first byte (Law I). The read-only latch is orfs_degraded."
	reasons := []string{capacity.BreachBackingReserve, capacity.BreachAuthoritative, capacity.BreachHardQuota}
	seen := map[string]bool{}
	for _, r := range reasons {
		seen[r] = true
		set.Counter("orfs_breach_total", breachHelp, float64(s.Breach[r]), metrics.Label{Name: "reason", Value: r})
	}
	for r, n := range s.Breach {
		if seen[r] {
			continue
		}
		set.Counter("orfs_breach_total", breachHelp, float64(n), metrics.Label{Name: "reason", Value: r})
	}

	qHelp := "Retained abandoned-tail segments/bytes moved into or.log/quarantine/ (recovery moves, never truncates)."
	set.Counter("orfs_quarantine_total", qHelp, float64(s.QuarantineSegments), metrics.Label{Name: "unit", Value: "segments"})
	set.Counter("orfs_quarantine_total", qHelp, float64(s.QuarantineBytes), metrics.Label{Name: "unit", Value: "bytes"})

	verifyHelp := "Failed checks from the most recent verify --deep pass."
	if !s.VerifyDeepRan {
		verifyHelp += " No verify pass was run for this inspection; reported as 0."
	}
	set.Gauge("orfs_verify_deep_failures", verifyHelp, float64(s.VerifyDeepFailures))

	set.Summary("orfs_fsync_latency_seconds",
		runtimeHelp("Durability-barrier (fsync) latency; quantiles are a bounded fixed-bucket estimate.", s.RuntimeSource),
		[]metrics.Quantile{{Q: 0.5, Value: s.FsyncP50}, {Q: 0.99, Value: s.FsyncP99}},
		s.FsyncCount, s.FsyncSum)

	batchHelp := runtimeHelp("Group-commit batch size in transactions per barrier.", s.RuntimeSource)
	set.Gauge("orfs_group_commit_batch_size", batchHelp, float64(s.BatchLast), metrics.Label{Name: "stat", Value: "last"})
	set.Gauge("orfs_group_commit_batch_size", batchHelp, s.BatchAvg, metrics.Label{Name: "stat", Value: "avg"})

	if s.RuntimeSource == "snapshot" {
		set.Gauge("orfs_metrics_snapshot_timestamp_seconds",
			"Unix time the persisted runtime snapshot was captured.",
			float64(s.SnapshotCapturedAt))
	}
	return set
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
