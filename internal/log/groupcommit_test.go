package log

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/record"
)

// courtConfig is the workload for the group-commit court.
type courtConfig struct {
	writers      int
	opsPerWriter int
	maxBatch     int
	maxWait      time.Duration
	payloadBytes int
}

// armResult is one measured arm.
type armResult struct {
	GroupCommit       bool    `json:"group_commit"`
	Ops               int     `json:"ops"`
	WallNs            int64   `json:"wall_ns"`
	ThroughputOpsPerS float64 `json:"throughput_ops_per_sec"`
	P50Ns             int64   `json:"p50_ns"`
	P99Ns             int64   `json:"p99_ns"`
	P999Ns            int64   `json:"p999_ns"`
	MeanNs            int64   `json:"mean_ns"`
}

// TestGroupCommitCourt measures publish-latency p50/p99 and throughput for N
// concurrent single-transaction writers with group commit OFF (a barrier per
// publish) versus ON (one barrier per batch), and writes
// evidence/performance/groupcommit.json when ORFS_EVIDENCE=1.
//
// Honest reporting: the win comes entirely from fewer durability barriers, so it
// depends on the backing store's fsync cost. On a backing where fsync is nearly
// free (a tmpfs) there may be little or no win. This court is meant to run inside
// the court container, whose backing is overlay/ext4, where fsync has real cost.
// Whatever it observes is what it records; no win is claimed beyond the numbers.
func TestGroupCommitCourt(t *testing.T) {
	if testing.Short() {
		t.Skip("group-commit court skipped in -short mode")
	}
	cfg := courtConfig{
		writers:      envPositiveInt("ORFS_GC_WRITERS", 64),
		opsPerWriter: envPositiveInt("ORFS_GC_OPS", 16),
		maxBatch:     envPositiveInt("ORFS_GC_MAXBATCH", 64),
		maxWait:      time.Duration(envPositiveInt("ORFS_GC_MAXWAIT_US", 2000)) * time.Microsecond,
		payloadBytes: envPositiveInt("ORFS_GC_PAYLOAD", 0),
	}

	off := runCourtArm(t, "off", cfg, false)
	on := runCourtArm(t, "on", cfg, true)
	probe := barrierProbe(t, cfg)

	thr := on.ThroughputOpsPerS / off.ThroughputOpsPerS
	p50 := float64(off.P50Ns) / float64(on.P50Ns)
	t.Logf("uncontended single-publish (encode+append+fsync) mean = %s", time.Duration(probe))
	t.Logf("group commit OFF: p50=%s p99=%s thr=%.0f ops/s",
		time.Duration(off.P50Ns), time.Duration(off.P99Ns), off.ThroughputOpsPerS)
	t.Logf("group commit ON:  p50=%s p99=%s thr=%.0f ops/s",
		time.Duration(on.P50Ns), time.Duration(on.P99Ns), on.ThroughputOpsPerS)
	t.Logf("throughput ON/OFF = %.2fx, p50 OFF/ON = %.2fx (writers=%d ops/writer=%d maxBatch=%d maxWait=%s payload=%d)",
		thr, p50, cfg.writers, cfg.opsPerWriter, cfg.maxBatch, cfg.maxWait, cfg.payloadBytes)

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeGroupCommitEvidence(t, cfg, off, on, probe)
	}
}

// barrierProbe measures the uncontended cost of one publish (record encode +
// append + one fsync) with group commit off. It lets a reader see how expensive
// the durability barrier actually is on this backing, and therefore how much
// headroom a single barrier per batch has.
func barrierProbe(t *testing.T, cfg courtConfig) int64 {
	t.Helper()
	backing := t.TempDir()
	l, err := Create(backing, testGenesis())
	if err != nil {
		t.Fatalf("probe create: %v", err)
	}
	defer l.Close()
	const ops = 64
	var total int64
	for i := 0; i < ops; i++ {
		t0 := time.Now()
		if _, err := l.Publish(courtTx(uint64(9_000_000+i), cfg.payloadBytes)); err != nil {
			t.Fatalf("probe publish: %v", err)
		}
		total += time.Since(t0).Nanoseconds()
	}
	return total / ops
}

// runCourtArm drives cfg.writers goroutines, each performing cfg.opsPerWriter
// publishes, and returns the measured arm.
func runCourtArm(t *testing.T, label string, cfg courtConfig, group bool) armResult {
	t.Helper()
	backing := t.TempDir()
	var opts []Option
	if group {
		opts = append(opts, WithGroupCommit(cfg.maxBatch, cfg.maxWait))
	}
	l, err := Create(backing, testGenesis(), opts...)
	if err != nil {
		t.Fatalf("%s: create: %v", label, err)
	}
	defer l.Close()

	total := cfg.writers * cfg.opsPerWriter
	latencies := make([]int64, 0, total)
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})

	wallStart := time.Now()
	for w := 0; w < cfg.writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for op := 0; op < cfg.opsPerWriter; op++ {
				recs := courtTx(uint64(1_000_000+w*cfg.opsPerWriter+op), cfg.payloadBytes)
				t0 := time.Now()
				if _, err := l.Publish(recs); err != nil {
					t.Errorf("%s: publish: %v", label, err)
					return
				}
				d := time.Since(t0).Nanoseconds()
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()
	wall := time.Since(wallStart)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return armResult{
		GroupCommit:       group,
		Ops:               len(latencies),
		WallNs:            wall.Nanoseconds(),
		ThroughputOpsPerS: float64(len(latencies)) / wall.Seconds(),
		P50Ns:             percentile(latencies, 0.50),
		P99Ns:             percentile(latencies, 0.99),
		P999Ns:            percentile(latencies, 0.999),
		MeanNs:            mean(latencies),
	}
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func mean(sorted []int64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return sum / int64(len(sorted))
}

func envPositiveInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// courtTx builds one transaction: an INODE_CREATE plus, when payloadBytes > 0, a
// DATA_EXTENT carrying that many bytes (so a barrier has real data to flush).
func courtTx(id uint64, payloadBytes int) []*record.Record {
	recs := []*record.Record{inodeCreate(id)}
	if payloadBytes > 0 {
		data := make([]byte, payloadBytes)
		for i := range data {
			data[i] = byte(i)
		}
		p := record.DataExtent{Offset: 0, Data: data}.Encode()
		recs = append(recs, &record.Record{
			Header:  record.Header{RecordType: record.TypeDataExtent, PayloadLength: uint64(len(p)), InodeID: id},
			Payload: p,
		})
	}
	return recs
}

func writeGroupCommitEvidence(t *testing.T, cfg courtConfig, off, on armResult, probe int64) {
	t.Helper()
	thr := on.ThroughputOpsPerS / off.ThroughputOpsPerS
	p50 := float64(off.P50Ns) / float64(on.P50Ns)
	note := fmt.Sprintf(
		"the win comes only from fewer fsync barriers, so it tracks the backing store's fsync cost; "+
			"a batch of up to %d transactions shares one barrier. On this backing an uncontended "+
			"single publish (encode+append+one fsync) costs %d ns, so the win is bounded by both the "+
			"barrier cost and the per-batch CPU to frame/append the batch. Measured "+
			"throughput_on/throughput_off=%.2fx, p50_off/p50_on=%.2fx. The OFF arm is the honest "+
			"baseline: without group commit, concurrent publishes serialize and each pays its own "+
			"fsync. Run inside the court container (overlay/ext4) where fsync has real cost; on tmpfs "+
			"the win is expected to be small or absent.", cfg.maxBatch, probe, thr, p50)

	doc := map[string]any{
		"schema": "orfs.performance.groupcommit.v1",
		"method": "N concurrent single-transaction publishers against one Log; OFF = one durability barrier per publish, ON = one barrier per batch (bounded by max count and max wait)",
		"config": map[string]any{
			"writers":        cfg.writers,
			"ops_per_writer": cfg.opsPerWriter,
			"total_ops":      cfg.writers * cfg.opsPerWriter,
			"max_batch":      cfg.maxBatch,
			"max_wait_us":    cfg.maxWait.Microseconds(),
			"payload_bytes":  cfg.payloadBytes,
		},
		"barrier_probe_mean_ns": probe,
		"arms":                  []armResult{off, on},
		"note":                  note,
	}

	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	dir := filepath.Join("..", "..", "evidence", "performance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	path := filepath.Join(dir, "groupcommit.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s", path)
}
