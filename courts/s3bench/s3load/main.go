// Command s3load is the workload driver for the WORM S3 court
// (courts/s3bench). It is deliberately one small, dependency-free Go program
// that drives *every* arm with the same latency and throughput accounting, so
// the rows are produced by one methodology:
//
//   - the S3 arms (the OrFS gateway, and any other S3 server) are driven over
//     HTTP with the header-based SigV4 subset internal/s3gw implements
//     (s3gw.SignRequest). No AWS SDK is vendored; the gateway's own signer is
//     the client, which is exactly the subset the gateway documents as
//     supported (docs/S3-GATEWAY.md §4).
//   - the FUSE baseline arm is driven as a mounted directory with the same
//     per-operation timing (write+fsync per record), so the "S3 PUT vs mounted
//     append" delta is measured by one binary.
//
// Why not `warp`? MinIO's warp is the preferred S3 driver and is staged from
// its pinned image, but it cannot drive this gateway: minio-go streams PUT
// bodies with a chunked payload signature (STREAMING-AWS4-HMAC-SHA256-PAYLOAD),
// which docs/S3-GATEWAY.md §4 records as unimplemented, so every PUT is refused.
// The court records that verbatim rather than hiding it. A Go driver using the
// gateway's own signer is the design's explicitly-welcomed third option.
//
// Every number this program prints is one it measured: it never estimates a
// rate or a percentile. Latencies are exact observed samples (each worker keeps
// a private slice; the slices are merged and sorted at the end), capped so the
// driver's own memory is bounded.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/infinityabundance/orfs-go/internal/s3gw"
)

// version is recorded in the receipt so a published number names the tool.
const version = "s3load/1"

// defaultSampleCap bounds each worker's latency slice so the driver's own
// memory stays well inside the 2 GiB container cap.
const defaultSampleCap = 400_000

func main() { os.Exit(run(os.Args[1:])) }

// point is the measured result of one matrix cell. Field names match the
// receipt's point shape.
type point struct {
	Op              string            `json:"op"`
	Concurrency     int               `json:"concurrency"`
	ObjectBytes     int64             `json:"object_bytes"`
	DurationSeconds float64           `json:"duration_seconds"`
	ElapsedSeconds  float64           `json:"elapsed_seconds"`
	Operations      uint64            `json:"operations"`
	OpsPerSecond    float64           `json:"ops_per_second"`
	BytesPerSecond  float64           `json:"bytes_per_second"`
	P50Seconds      float64           `json:"p50_seconds"`
	P95Seconds      float64           `json:"p95_seconds"`
	P99Seconds      float64           `json:"p99_seconds"`
	P999Seconds     float64           `json:"p999_seconds"`
	Errors          uint64            `json:"errors"`
	ErrorClasses    map[string]uint64 `json:"error_classes,omitempty"`
	SampleCapHit    bool              `json:"sample_cap_hit"`
	TotalBytes      int64             `json:"total_bytes"`
	BoundedBy       string            `json:"bounded_by"`
}

// driver holds the run configuration.
type driver struct {
	mode      string
	endpoint  string
	access    string
	secret    string
	region    string
	bucket    string
	prefix    string
	dir       string
	conc      int
	objBytes  int64
	duration  time.Duration
	prepBytes int64
	seed      int64
	sampleCap int
	maxBytes  int64
	maxPut    int64
	out       string

	client *http.Client
	cred   s3gw.Credential

	body []byte
	keys []string // prepared keys for get/mixed
}

func run(args []string) int {
	d := &driver{}
	fs := flag.NewFlagSet("s3load", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&d.mode, "mode", "", "put | get | mixed | posix-append | worm | metrics")
	fs.StringVar(&d.endpoint, "endpoint", "127.0.0.1:9000", "S3 endpoint host:port (path-style, http)")
	fs.StringVar(&d.access, "access", "", "SigV4 access key")
	fs.StringVar(&d.secret, "secret", "", "SigV4 secret key")
	fs.StringVar(&d.region, "region", "us-east-1", "SigV4 region")
	fs.StringVar(&d.bucket, "bucket", "s3bench", "bucket")
	fs.StringVar(&d.prefix, "prefix", "sbench", "object key prefix")
	fs.StringVar(&d.dir, "dir", "", "mounted directory (posix-append)")
	fs.IntVar(&d.conc, "concurrency", 1, "concurrent clients")
	fs.Int64Var(&d.objBytes, "object-bytes", 4096, "object/record size in bytes")
	fs.DurationVar(&d.duration, "duration", 5*time.Second, "bounded measured window")
	fs.Int64Var(&d.prepBytes, "prepare-bytes", 64<<20, "bytes budget of objects pre-PUT for get/mixed")
	fs.Int64Var(&d.seed, "seed", 1, "deterministic seed for the object body")
	fs.IntVar(&d.sampleCap, "sample-cap", defaultSampleCap, "per-worker latency sample cap")
	fs.Int64Var(&d.maxBytes, "max-bytes-per-writer", 256<<20, "posix-append: per-writer byte cap")
	fs.Int64Var(&d.maxPut, "max-put-bytes", 512<<20, "put/mixed: stop the arm once this many object bytes have been written (0 = unbounded); bounds the retained history an append-only target must hold")
	fs.StringVar(&d.out, "out", "", "write the JSON result here (default stdout)")
	showVer := fs.Bool("version", false, "print the driver version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVer {
		fmt.Println(version)
		return 0
	}
	if d.mode == "" {
		fmt.Fprintln(os.Stderr, "s3load: -mode is required")
		return 2
	}
	if d.sampleCap <= 0 {
		d.sampleCap = defaultSampleCap
	}
	if d.conc <= 0 {
		d.conc = 1
	}

	var (
		result any
		err    error
	)
	switch d.mode {
	case "put", "get", "mixed":
		d.setupHTTP()
		result, err = d.runS3Load()
	case "posix-append":
		result, err = d.runPosixAppend()
	case "worm":
		d.setupHTTP()
		result, err = d.runWorm()
	case "metrics":
		d.setupHTTP()
		result, err = d.runMetrics()
	default:
		fmt.Fprintf(os.Stderr, "s3load: unknown mode %q\n", d.mode)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3load: %v\n", err)
		return 1
	}
	blob, _ := json.MarshalIndent(result, "", "  ")
	blob = append(blob, '\n')
	if d.out == "" {
		os.Stdout.Write(blob)
		return 0
	}
	if err := os.WriteFile(d.out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "s3load: write %s: %v\n", d.out, err)
		return 1
	}
	return 0
}

func (d *driver) setupHTTP() {
	d.cred = s3gw.Credential{AccessKey: d.access, SecretKey: d.secret}
	maxIdle := 2*d.conc + 16
	d.client = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        maxIdle,
			MaxIdleConnsPerHost: maxIdle,
			DisableCompression:  true,
			ForceAttemptHTTP2:   false,
		},
	}
	// A deterministic body per object size.
	d.body = make([]byte, d.objBytes)
	rng := newRand(d.seed)
	rng.Read(d.body)
}

// --- S3 request plumbing ----------------------------------------------------

// s3Reply is the outcome of one signed request.
type s3Reply struct {
	status int
	code   string // S3 error code, "" on success
	header http.Header
	body   []byte
}

// do signs and performs one request. When collect is false the response body is
// drained to io.Discard (no allocation on the hot path); when true it is
// returned (small payloads only: worm/metrics).
func (d *driver) do(method, key string, body []byte, headers map[string]string, collect bool) (s3Reply, error) {
	url := "http://" + d.endpoint + "/" + d.bucket + "/" + key
	var rdr io.Reader
	hash := ""
	if body != nil {
		rdr = bytes.NewReader(body)
		hash = hexSHA256(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return s3Reply{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	s3gw.SignRequest(req, d.cred, d.region, time.Now(), hash)
	resp, err := d.client.Do(req)
	if err != nil {
		return s3Reply{}, err
	}
	defer resp.Body.Close()
	rep := s3Reply{status: resp.StatusCode, header: resp.Header}
	if collect || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		rep.body = b
		if resp.StatusCode >= 300 {
			rep.code = errCodeOf(b)
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return rep, nil
}

// doLoad is the hot path: it returns the status and an error class, allocating
// nothing for a successful response body.
func (d *driver) doLoad(method, key string, body []byte) (int, string, error) {
	url := "http://" + d.endpoint + "/" + d.bucket + "/" + key
	var rdr io.Reader
	hash := ""
	if body != nil {
		rdr = bytes.NewReader(body)
		hash = hexSHA256(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, "", err
	}
	s3gw.SignRequest(req, d.cred, d.region, time.Now(), hash)
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, "transport", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		code := errCodeOf(b)
		if code == "" {
			code = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, fmt.Sprintf("s3:%d:%s", resp.StatusCode, code), nil
	}
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, "", nil
}

// --- the S3 load matrix -----------------------------------------------------

type workerStats struct {
	ops     uint64
	bytes   uint64
	errs    map[string]uint64
	samples []int64
	capHit  bool
}

func (d *driver) runS3Load() (point, error) {
	op := d.mode
	if op == "get" || op == "mixed" {
		n := d.prepareCount()
		for j := 0; j < n; j++ {
			key := fmt.Sprintf("%s-get-%d", d.prefix, j)
			st, class, err := d.doLoad("PUT", key, d.body)
			if err != nil || st != 200 {
				return point{}, fmt.Errorf("prepare PUT %s: status=%d class=%s err=%v", key, st, class, err)
			}
			d.keys = append(d.keys, key)
		}
	}

	results := make([]workerStats, d.conc)
	deadline := time.Now().Add(d.duration)
	var putBytes int64
	var seq = make([]uint64, d.conc) // per-worker monotonically increasing key counter
	var wg sync.WaitGroup
	for w := 0; w < d.conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ws := workerStats{errs: map[string]uint64{}}
			rng := newRand(d.seed + int64(w)*7919)
			for time.Now().Before(deadline) {
				doPut := op == "put"
				if op == "mixed" {
					doPut = seq[w]%2 == 0
				}
				// An append-only target retains every byte, so a PUT arm must be
				// bounded by bytes as well as by time, or it fills the disk.
				if doPut && d.maxPut > 0 && atomic.LoadInt64(&putBytes) >= d.maxPut {
					break
				}
				var key string
				if doPut {
					key = fmt.Sprintf("%s-w%d-%d", d.prefix, w, seq[w])
				} else {
					key = d.keys[rng.Intn(len(d.keys))]
				}
				seq[w]++
				start := time.Now()
				var st int
				var class string
				var err error
				if doPut {
					st, class, err = d.doLoad(http.MethodPut, key, d.body)
				} else {
					st, class, err = d.doLoad(http.MethodGet, key, nil)
				}
				el := time.Since(start).Nanoseconds()
				if err != nil {
					ws.errs[class]++
					continue
				}
				if st != 200 {
					ws.errs[class]++
					continue
				}
				ws.ops++
				if doPut {
					ws.bytes += uint64(len(d.body))
					atomic.AddInt64(&putBytes, int64(len(d.body)))
				} else {
					ws.bytes += uint64(len(d.body))
				}
				if len(ws.samples) < d.sampleCap {
					ws.samples = append(ws.samples, el)
				} else {
					ws.capHit = true
				}
			}
			results[w] = ws
		}(w)
	}
	startAll := time.Now()
	wg.Wait()
	elapsed := time.Since(startAll).Seconds()
	if elapsed <= 0 {
		elapsed = 1e-9
	}
	p := summarise(op, d.conc, d.objBytes, d.duration.Seconds(), elapsed, results)
	p.TotalBytes = atomic.LoadInt64(&putBytes)
	if d.maxPut > 0 && p.TotalBytes >= d.maxPut {
		p.BoundedBy = "bytes"
	} else {
		p.BoundedBy = "duration"
	}
	return p, nil
}

func (d *driver) prepareCount() int {
	if d.objBytes <= 0 {
		return 1
	}
	n := int(d.prepBytes / d.objBytes)
	if n < 1 {
		n = 1
	}
	if n > 8192 {
		n = 8192
	}
	// The prepared set must be addressable by the get workers.
	d.keys = make([]string, 0, n)
	return n
}

// --- the FUSE baseline arm --------------------------------------------------

func (d *driver) runPosixAppend() (point, error) {
	if d.dir == "" {
		return point{}, errors.New("posix-append requires -dir")
	}
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return point{}, err
	}
	files := make([]*os.File, d.conc)
	for w := 0; w < d.conc; w++ {
		p := filepath.Join(d.dir, fmt.Sprintf("sbench-append-%d.bin", w))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return point{}, fmt.Errorf("create %s: %w", p, err)
		}
		files[w] = f
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	rec := make([]byte, d.objBytes)
	newRand(d.seed).Read(rec)

	results := make([]workerStats, d.conc)
	deadline := time.Now().Add(d.duration)
	var wg sync.WaitGroup
	for w := 0; w < d.conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			f := files[w]
			ws := workerStats{errs: map[string]uint64{}}
			var written int64
			for time.Now().Before(deadline) {
				start := time.Now()
				n, err := f.Write(rec)
				if err == nil {
					err = f.Sync()
				}
				el := time.Since(start).Nanoseconds()
				if err != nil {
					ws.errs[errnoClass(err)]++
					continue
				}
				ws.ops++
				ws.bytes += uint64(n)
				if len(ws.samples) < d.sampleCap {
					ws.samples = append(ws.samples, el)
				} else {
					ws.capHit = true
				}
				written += int64(n)
				if d.maxBytes > 0 && written >= d.maxBytes {
					break
				}
			}
			results[w] = ws
		}(w)
	}
	startAll := time.Now()
	wg.Wait()
	elapsed := time.Since(startAll).Seconds()
	if elapsed <= 0 {
		elapsed = 1e-9
	}
	return summarise("append", d.conc, d.objBytes, d.duration.Seconds(), elapsed, results), nil
}

// --- the WORM correctness arm ----------------------------------------------

type wormReport struct {
	RetainPut         int    `json:"retain_put"`
	RetainDelete      int    `json:"retain_delete"`
	RetainRefusal     string `json:"retain_refusal_header"`
	RetainDeleteCode  string `json:"retain_delete_code"`
	GetStatus         int    `json:"get_status"`
	GetBytesMatch     bool   `json:"get_bytes_match"`
	ReassertPut       int    `json:"reassert_put"`
	ReassertCode      string `json:"reassert_code"`
	LegalHoldPut      int    `json:"legal_hold_put"`
	LegalHoldDelete   int    `json:"legal_hold_delete"`
	LegalHoldRefusal  string `json:"legal_hold_refusal_header"`
	LegalHoldCode     string `json:"legal_hold_delete_code"`
	AllExpected       bool   `json:"all_expected"`
	RetainUntilHeader string `json:"retain_until_sent"`
}

func (d *driver) runWorm() (wormReport, error) {
	var r wormReport
	payload := make([]byte, d.objBytes)
	newRand(d.seed).Read(payload)
	retainUntil := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	r.RetainUntilHeader = retainUntil

	keyRetain := d.prefix + "-retain"
	keyHold := d.prefix + "-hold"

	// 1. PUT with a finite retention.
	rep, err := d.do(http.MethodPut, keyRetain, payload, map[string]string{
		"x-amz-object-lock-retain-until-date": retainUntil,
		"x-amz-object-lock-mode":              "COMPLIANCE",
	}, false)
	if err != nil {
		return r, err
	}
	r.RetainPut = rep.status

	// 2. DELETE the retained object -> expect 403.
	rep, err = d.do(http.MethodDelete, keyRetain, nil, nil, false)
	if err != nil {
		return r, err
	}
	r.RetainDelete = rep.status
	r.RetainRefusal = rep.header.Get("x-amz-worm-refusal")
	r.RetainDeleteCode = rep.code

	// 3. GET it -> expect 200 and the exact bytes.
	rep, err = d.do(http.MethodGet, keyRetain, nil, nil, true)
	if err != nil {
		return r, err
	}
	r.GetStatus = rep.status
	r.GetBytesMatch = rep.status == 200 && bytes.Equal(rep.body, payload)

	// 4. Overwrite it -> expect 403.
	rep, err = d.do(http.MethodPut, keyRetain, payload, map[string]string{
		"x-amz-object-lock-retain-until-date": retainUntil,
	}, false)
	if err != nil {
		return r, err
	}
	r.ReassertPut = rep.status
	r.ReassertCode = rep.code

	// 5. Legal hold.
	rep, err = d.do(http.MethodPut, keyHold, payload, map[string]string{
		"x-amz-object-lock-legal-hold": "ON",
	}, false)
	if err != nil {
		return r, err
	}
	r.LegalHoldPut = rep.status
	rep, err = d.do(http.MethodDelete, keyHold, nil, nil, false)
	if err != nil {
		return r, err
	}
	r.LegalHoldDelete = rep.status
	r.LegalHoldRefusal = rep.header.Get("x-amz-worm-refusal")
	r.LegalHoldCode = rep.code

	r.AllExpected = r.RetainPut == 200 && r.RetainDelete == 403 &&
		r.RetainRefusal == "retention" && r.GetStatus == 200 && r.GetBytesMatch &&
		r.ReassertPut == 403 && r.LegalHoldPut == 200 && r.LegalHoldDelete == 403 &&
		r.LegalHoldRefusal == "legal-hold"
	return r, nil
}

// --- metrics ----------------------------------------------------------------

// runMetrics fetches /metrics and returns the orfs_* families as name->value.
// It never signs (the endpoint is unauthenticated by default); it is also the
// court's readiness probe.
func (d *driver) runMetrics() (map[string]float64, error) {
	url := "http://" + d.endpoint + "/metrics"
	resp, err := d.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("metrics status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, "orfs_") {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		out[name] = v
	}
	return out, nil
}

// --- shared summarisation ---------------------------------------------------

func summarise(op string, conc int, objBytes int64, duration, elapsed float64, results []workerStats) point {
	p := point{
		Op:              op,
		Concurrency:     conc,
		ObjectBytes:     objBytes,
		DurationSeconds: duration,
		ElapsedSeconds:  elapsed,
		ErrorClasses:    map[string]uint64{},
	}
	var samples []int64
	for _, r := range results {
		p.Operations += r.ops
		p.BytesPerSecond += float64(r.bytes)
		for k, v := range r.errs {
			p.Errors += v
			p.ErrorClasses[k] += v
		}
		samples = append(samples, r.samples...)
		p.SampleCapHit = p.SampleCapHit || r.capHit
	}
	if len(p.ErrorClasses) == 0 {
		p.ErrorClasses = nil
	}
	p.OpsPerSecond = float64(p.Operations) / elapsed
	p.BytesPerSecond = p.BytesPerSecond / elapsed
	if len(samples) > 0 {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		p.P50Seconds = nsToSeconds(quantile(samples, 0.50))
		p.P95Seconds = nsToSeconds(quantile(samples, 0.95))
		p.P99Seconds = nsToSeconds(quantile(samples, 0.99))
		p.P999Seconds = nsToSeconds(quantile(samples, 0.999))
	}
	return p
}

func quantile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func nsToSeconds(ns int64) float64 { return float64(ns) / 1e9 }

// --- small helpers ----------------------------------------------------------

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// errDocument is the S3 <Error> body.
type errDocument struct {
	Code string `xml:"Code"`
}

func errCodeOf(body []byte) string {
	var e errDocument
	if err := xml.Unmarshal(body, &e); err == nil && e.Code != "" {
		return e.Code
	}
	return ""
}

func errnoClass(err error) string {
	var perr *os.PathError
	if errors.As(err, &perr) {
		err = perr.Err
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOSPC:
			return "ENOSPC"
		case syscall.EDQUOT:
			return "EDQUOT"
		case syscall.EIO:
			return "EIO"
		case syscall.EEXIST:
			return "EEXIST"
		}
		return errno.Error()
	}
	return "other"
}

// newRand returns a deterministic PRNG seeded with seed.
func newRand(seed int64) *randReader { return &randReader{seed: uint64(seed)*6364136223846793005 + 1} }

type randReader struct{ seed uint64 }

func (r *randReader) Read(p []byte) (int, error) {
	for i := range p {
		r.seed ^= r.seed << 13
		r.seed ^= r.seed >> 7
		r.seed ^= r.seed << 17
		p[i] = byte(r.seed)
	}
	return len(p), nil
}

// Intn returns a deterministic value in [0, n) from the same xorshift64 stream,
// so the driver's key selection is reproducible from the seed alone.
func (r *randReader) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	r.seed ^= r.seed << 13
	r.seed ^= r.seed >> 7
	r.seed ^= r.seed << 17
	return int(r.seed % uint64(n))
}
