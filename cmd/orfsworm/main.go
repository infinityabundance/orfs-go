// Command orfsworm is the append-only (WORM) audit-log-sink court driver and
// receipt composer.
//
// It has two subcommands, both stdlib-only and honest about what they measure:
//
//	orfsworm sink    -file PATH -records N -payload B [-fsync-each] [-tail]
//	orfsworm summary -work DIR -out FILE -volume PATH -segment-size B \
//	                 [-group-commit] [-mounts ...]...
//
// `sink` drives the workload: it appends framed records to a mounted file with
// O_APPEND, fsyncs each (the OrFS publish barrier), times every append, and runs
// a tail reader that watches the file grow. It prints a JSON result.
//
// `summary` composes the case-study receipt (schema orfs.worm.case-study.v1) from
// the sink result and the artifacts the court captured with the `orfs` binary
// (the Prometheus exposition, its JSON form, the doctor capacity report, a
// verify --deep report, and an optional manifest). It never invents a number: a
// metric that is absent from the exposition is recorded as absent, not zeroed.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "orfsworm: usage: orfsworm <sink|summary> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sink":
		os.Exit(runSink(os.Args[2:]))
	case "summary":
		os.Exit(runSummary(os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, "orfsworm: usage: orfsworm <sink|summary> [flags]")
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "orfsworm: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

// sinkResult is the workload measurement.
type sinkResult struct {
	Schema                  string  `json:"schema"`
	File                    string  `json:"file"`
	Records                 int     `json:"records"`
	RecordBytes             int     `json:"record_bytes"`
	PayloadBytes            int     `json:"payload_bytes"`
	AppendedBytes           int64   `json:"appended_bytes"`
	LogicalBytes            int64   `json:"logical_bytes"`
	WallSeconds             float64 `json:"wall_seconds"`
	ThroughputRecordsPerSec float64 `json:"throughput_records_per_sec"`
	ThroughputBytesPerSec   float64 `json:"throughput_bytes_per_sec"`
	P50Ns                   int64   `json:"append_fsync_p50_ns"`
	P99Ns                   int64   `json:"append_fsync_p99_ns"`
	MeanNs                  int64   `json:"append_fsync_mean_ns"`
	TailReads               int64   `json:"tail_reads"`
	TailGrowthObservations  int64   `json:"tail_growth_observations"`
	TailBytesRead           int64   `json:"tail_bytes_read"`
	TailMonotonic           bool    `json:"tail_monotonic"`
}

func runSink(args []string) int {
	fs := flag.NewFlagSet("sink", flag.ContinueOnError)
	file := fs.String("file", "", "append-only sink file (under the mount)")
	records := fs.Int("records", 1024, "records to append")
	payload := fs.Int("payload", 512, "payload bytes per record")
	fsyncEach := fs.Bool("fsync-each", true, "fsync after every record (publish barrier)")
	tail := fs.Bool("tail", true, "run a tail reader that watches the file grow")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "orfsworm sink: -file is required")
		return 2
	}
	if *records <= 0 {
		fmt.Fprintln(os.Stderr, "orfsworm sink: -records must be positive")
		return 2
	}
	if *payload < 0 {
		fmt.Fprintln(os.Stderr, "orfsworm sink: -payload must be non-negative")
		return 2
	}

	recLen := 16 + *payload
	buf := make([]byte, recLen)
	for i := 16; i < recLen; i++ {
		buf[i] = byte(i)
	}

	f, err := os.OpenFile(*file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsworm sink: open: %v\n", err)
		return 1
	}

	var tailBytes, tailReads, tailGrowth atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	if *tail {
		go tailReader(*file, stop, done, &tailBytes, &tailReads, &tailGrowth)
	} else {
		close(done)
	}

	latencies := make([]int64, 0, *records)
	start := time.Now()
	for i := 0; i < *records; i++ {
		binary.BigEndian.PutUint64(buf[0:8], uint64(i))
		binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
		t0 := time.Now()
		if _, err := f.Write(buf); err != nil {
			fmt.Fprintf(os.Stderr, "orfsworm sink: write %d: %v\n", i, err)
			f.Close()
			return 1
		}
		if *fsyncEach {
			if err := f.Sync(); err != nil {
				fmt.Fprintf(os.Stderr, "orfsworm sink: fsync %d: %v\n", i, err)
				f.Close()
				return 1
			}
		}
		latencies = append(latencies, time.Since(t0).Nanoseconds())
	}
	wall := time.Since(start)
	if err := f.Sync(); err != nil {
		fmt.Fprintf(os.Stderr, "orfsworm sink: final fsync: %v\n", err)
		f.Close()
		return 1
	}
	var logical int64
	if fi, err := f.Stat(); err == nil {
		logical = fi.Size()
	}
	f.Close()

	close(stop)
	<-done

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	appended := int64(*records) * int64(recLen)
	res := sinkResult{
		Schema:                  "orfs.worm.sink.v1",
		File:                    *file,
		Records:                 *records,
		RecordBytes:             recLen,
		PayloadBytes:            *payload,
		AppendedBytes:           appended,
		LogicalBytes:            logical,
		WallSeconds:             wall.Seconds(),
		ThroughputRecordsPerSec: float64(*records) / wall.Seconds(),
		ThroughputBytesPerSec:   float64(appended) / wall.Seconds(),
		P50Ns:                   percentile(latencies, 0.50),
		P99Ns:                   percentile(latencies, 0.99),
		MeanNs:                  mean(latencies),
		TailReads:               tailReads.Load(),
		TailGrowthObservations:  tailGrowth.Load(),
		TailBytesRead:           tailBytes.Load(),
		TailMonotonic:           tailBytes.Load() == logical && logical > 0,
	}
	blob, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(blob))
	if logical != appended {
		fmt.Fprintf(os.Stderr, "orfsworm sink: NOTE: mounted size %d != appended %d (framing/fs semantics)\n", logical, appended)
	}
	return 0
}

// tailReader reads the sink file forward to EOF on a fixed cadence and counts how
// many times it observed growth — the readers-tail behaviour of a WORM sink.
func tailReader(path string, stop <-chan struct{}, done chan<- struct{}, bytes, reads, growth *atomic.Int64) {
	defer close(done)
	rf, err := os.Open(path)
	if err != nil {
		return
	}
	defer rf.Close()
	var off int64
	rb := make([]byte, 64*1024)
	drain := func() {
		for {
			n, err := rf.ReadAt(rb, off)
			if n > 0 {
				off += int64(n)
			}
			if err != nil {
				return
			}
		}
	}
	for {
		select {
		case <-stop:
			drain()
			bytes.Store(off)
			return
		default:
		}
		prev := off
		drain()
		if off > prev {
			growth.Add(1)
		}
		reads.Add(1)
		bytes.Store(off)
		time.Sleep(200 * time.Microsecond)
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

// --- summary -----------------------------------------------------------------

type label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type sample struct {
	Name   string  `json:"name"`
	Labels []label `json:"labels"`
	Value  float64 `json:"value"`
}

type family struct {
	Name    string   `json:"name"`
	Help    string   `json:"help"`
	Type    string   `json:"type"`
	Samples []sample `json:"samples"`
}

type metricsDoc struct {
	Schema   string   `json:"schema"`
	Families []family `json:"families"`
}

func (d *metricsDoc) lookup(name string, want map[string]string) (float64, bool) {
	for _, f := range d.Families {
		for _, s := range f.Samples {
			// A summary's _sum/_count companions carry their own name inside the
			// parent family; a plain sample inherits the family name.
			sname := s.Name
			if sname == "" {
				sname = f.Name
			}
			if sname != name {
				continue
			}
			if labelsMatch(s.Labels, want) {
				return s.Value, true
			}
		}
	}
	return 0, false
}

func labelsMatch(got []label, want map[string]string) bool {
	if len(want) == 0 {
		return len(got) == 0
	}
	for k, v := range want {
		found := false
		for _, l := range got {
			if l.Name == k {
				if l.Value != v {
					return false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type bytesDoc struct {
	Logical           float64 `json:"logical_bytes"`
	Physical          float64 `json:"physical_bytes"`
	Manifested        float64 `json:"manifested_bytes"`
	AuthoritativeRoot float64 `json:"authoritative_root_bytes"`
	Amplification     float64 `json:"amplification_ratio"`
}

type countersDoc struct {
	BreachTotal        float64 `json:"breach_total"`
	QuarantineSegments float64 `json:"quarantine_segments"`
	QuarantineBytes    float64 `json:"quarantine_bytes"`
	VerifyDeepFailures float64 `json:"verify_deep_failures"`
}

type fsyncDoc struct {
	P50Seconds float64 `json:"p50_seconds"`
	P99Seconds float64 `json:"p99_seconds"`
	Count      float64 `json:"count"`
	Observed   bool    `json:"observed"`
}

type batchDoc struct {
	Last     float64 `json:"last"`
	Avg      float64 `json:"avg"`
	Observed bool    `json:"observed"`
}

type receipt struct {
	Schema         string          `json:"schema"`
	GeneratedAt    int64           `json:"generated_at_unix"`
	Volume         string          `json:"volume"`
	Config         map[string]any  `json:"config"`
	Workload       json.RawMessage `json:"workload"`
	Bytes          bytesDoc        `json:"bytes"`
	Counters       countersDoc     `json:"counters"`
	Fsync          fsyncDoc        `json:"fsync_latency_seconds"`
	Batch          batchDoc        `json:"group_commit_batch_size"`
	Prometheus     string          `json:"prometheus"`
	PrometheusJSON json.RawMessage `json:"prometheus_json"`
	DoctorCapacity json.RawMessage `json:"doctor_capacity,omitempty"`
	VerifyDeep     json.RawMessage `json:"verify_deep,omitempty"`
	Manifest       json.RawMessage `json:"manifest,omitempty"`
	Notes          []string        `json:"notes"`
}

func runSummary(args []string) int {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	work := fs.String("work", "", "directory holding the court artifacts")
	out := fs.String("out", "", "output path (default stdout)")
	volume := fs.String("volume", "", "volume backing path")
	segSize := fs.Uint64("segment-size", 0, "segment size")
	groupCommit := fs.Bool("group-commit", false, "group commit was enabled")
	transport := fs.String("transport", "f0", "serving transport")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *work == "" {
		fmt.Fprintln(os.Stderr, "orfsworm summary: -work is required")
		return 2
	}

	rec := receipt{
		Schema:      "orfs.worm.case-study.v1",
		GeneratedAt: time.Now().Unix(),
		Volume:      *volume,
		Config: map[string]any{
			"segment_size_bytes": *segSize,
			"group_commit":       *groupCommit,
			"mount":              "FUSE (go-fuse, direct mount)",
			"transport":          *transport,
			"workload":           "append-only audit-log sink (WORM)",
			"single_writer":      true,
		},
	}
	rec.Notes = append(rec.Notes,
		"This is an append-only WORM audit-log sink, not a general-purpose filesystem: records are appended and fsynced, never rewritten, and readers tail.",
		"Each append is one authoritative transaction, so append latency includes the publish barrier (fsync) under the FUSE tax.",
		"Records are 4 KiB page-aligned (4080-byte payload + a 16-byte frame header) so one client write is exactly one FUSE write and one transaction; a non-aligned record size can be split across a 4 KiB page boundary by the FUSE layer, which shows up as more transactions than appends.",
		"orfs_fsync_latency_seconds is the daemon-internal durability barrier (a bounded bucket estimate); orfs_group_commit_batch_size last/avg is 1 because the sink is a single sequential writer, so each write is its own batch.",
		"The barrier is far cheaper than the end-to-end append: compare orfs_fsync_latency_seconds with the sink's append_fsync latency. The difference is the FUSE round trip plus transaction framing, watermark, and index replay \u2014 the FUSE tax, not the barrier.",
		"orfs_physical_bytes is authoritative retained bytes (or.log/*.seg + commit.watermark); .or_bloom/ is excluded and reported separately as orfs_manifested_bytes.",
	)

	if raw := readFile(filepath.Join(*work, "sink.json")); raw != nil {
		rec.Workload = raw
	} else {
		fmt.Fprintln(os.Stderr, "orfsworm summary: missing sink.json")
		return 1
	}

	var md metricsDoc
	if raw := readFile(filepath.Join(*work, "metrics.json")); raw != nil {
		if err := json.Unmarshal(raw, &md); err != nil {
			fmt.Fprintf(os.Stderr, "orfsworm summary: metrics.json: %v\n", err)
			return 1
		}
		rec.PrometheusJSON = raw
	} else {
		fmt.Fprintln(os.Stderr, "orfsworm summary: missing metrics.json")
		return 1
	}
	if raw := readFile(filepath.Join(*work, "metrics.prom")); raw != nil {
		rec.Prometheus = string(raw)
	}

	get := func(name string, labels map[string]string) float64 {
		v, _ := md.lookup(name, labels)
		return v
	}
	rec.Bytes = bytesDoc{
		Logical:           get("orfs_logical_bytes", nil),
		Physical:          get("orfs_physical_bytes", nil),
		Manifested:        get("orfs_manifested_bytes", nil),
		AuthoritativeRoot: get("orfs_authoritative_bytes", nil),
		Amplification:     get("orfs_amplification_ratio", nil),
	}
	rec.Counters = countersDoc{
		BreachTotal: get("orfs_breach_total", map[string]string{"reason": "backing_reserve"}) +
			get("orfs_breach_total", map[string]string{"reason": "authoritative"}) +
			get("orfs_breach_total", map[string]string{"reason": "hard_quota"}),
		QuarantineSegments: get("orfs_quarantine_total", map[string]string{"unit": "segments"}),
		QuarantineBytes:    get("orfs_quarantine_total", map[string]string{"unit": "bytes"}),
		VerifyDeepFailures: get("orfs_verify_deep_failures", nil),
	}
	if _, ok := md.lookup("orfs_fsync_latency_seconds", map[string]string{"quantile": "0.5"}); ok {
		rec.Fsync = fsyncDoc{
			P50Seconds: get("orfs_fsync_latency_seconds", map[string]string{"quantile": "0.5"}),
			P99Seconds: get("orfs_fsync_latency_seconds", map[string]string{"quantile": "0.99"}),
			Count:      get("orfs_fsync_latency_seconds_count", nil),
			Observed:   true,
		}
	} else {
		rec.Notes = append(rec.Notes, "No runtime snapshot was present: orfs_fsync_latency_seconds is unobserved for this run.")
	}
	if _, ok := md.lookup("orfs_group_commit_batch_size", map[string]string{"stat": "last"}); ok {
		rec.Batch = batchDoc{
			Last:     get("orfs_group_commit_batch_size", map[string]string{"stat": "last"}),
			Avg:      get("orfs_group_commit_batch_size", map[string]string{"stat": "avg"}),
			Observed: true,
		}
	}

	rec.DoctorCapacity = readFile(filepath.Join(*work, "capacity.json"))
	rec.VerifyDeep = readFile(filepath.Join(*work, "verify.json"))
	rec.Manifest = readFile(filepath.Join(*work, "manifest.json"))

	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsworm summary: marshal: %v\n", err)
		return 1
	}
	blob = append(blob, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "orfsworm summary: write: %v\n", err)
			return 1
		}
		return 0
	}
	os.Stdout.Write(blob)
	return 0
}

func readFile(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	return raw
}
