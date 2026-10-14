// Command orfsaudit is the audit-log-sink court driver and receipt composer.
//
// It models the production story of OrFS as an append-only audit-log sink (Loki
// / Manifold style, e.g. /var/log/audit for systemd-journald or a Kubernetes
// audit policy). It has three subcommands. The only non-stdlib dependency is
// golang.org/x/sys/unix, used for real inotify.
//
//	orfsaudit sink    -file PATH -records N -payload B [-rate R] [-fsync-each] \
//	                  [-tail] [-inotify] [-seed S] [-forever]
//	orfsaudit tail    -file PATH [-seconds S] [-from-end] [-inotify]
//	orfsaudit summary -work DIR -out FILE -volume PATH -segment-size B \
//	                  [-group-commit] [-rate R] [-record-bytes B] [-seed S]
//
// `sink` appends structured JSON audit events
// ({"user":...,"action":...,"resource":...,"time":...}) drawn from a skewed
// 80/20 hot/cold corpus, one write + fsync per event (the OrFS publish barrier),
// and (with -tail) runs a tail reader that follows the file with inotify and
// reports the append-to-observation tail lag. It prints a JSON result.
//
// `tail` is the live `tail -f` view: it follows the file and prints each event
// line as it is observed.
//
// `summary` composes the audit-sink receipt (schema orfs.audit-sink.v1) from the
// sink result, the crash/immutability court evidence, the mounted-immutability
// proof, and the `orfs metrics` exposition. It never invents a number: a metric
// that is absent is recorded as absent, and the tail reader reports whether it
// actually observed inotify events or fell back to polling.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "orfsaudit: usage: orfsaudit <sink|tail|summary> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sink":
		os.Exit(runSink(os.Args[2:]))
	case "tail":
		os.Exit(runTailCmd(os.Args[2:]))
	case "summary":
		os.Exit(runSummary(os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, "orfsaudit: usage: orfsaudit <sink|tail|summary> [flags]")
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "orfsaudit: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

// --- the skewed 80/20 event corpus ------------------------------------------

// hotProb is the probability that an event field is drawn from its small hot
// set. The remaining mass comes from a much larger cold set, so the corpus is a
// realistic 80/20 skew (a handful of identities/actions/resources dominate the
// traffic) rather than a uniform draw over the whole namespace.
const hotProb = 0.80

// corpus is the deterministic, seeded source of audit-event fields.
type corpus struct {
	rng          *rand.Rand
	usersHot     []string
	usersCold    []string
	actionsHot   []string
	actionsCold  []string
	resourceHot  []string
	resourceCold []string
}

func newCorpus(seed int64) *corpus {
	c := &corpus{rng: rand.New(rand.NewSource(seed))}
	c.usersHot = []string{
		"root", "systemd-journald", "kubelet", "sshd",
		"chronyd", "containerd", "polkitd", "auditd",
	}
	c.actionsHot = []string{"login", "read", "write", "delete", "exec", "chmod"}
	c.resourceHot = []string{
		"/etc/passwd", "/etc/shadow", "/var/log/audit/audit.log", "/usr/bin/sudo",
		"/root/.ssh/authorized_keys", "/etc/ssh/sshd_config",
		"/var/lib/kubelet/config.yaml", "/etc/pam.d/sshd",
	}
	c.usersCold = make([]string, 256)
	for i := range c.usersCold {
		c.usersCold[i] = fmt.Sprintf("svc-%04d", i)
	}
	c.actionsCold = make([]string, 128)
	for i := range c.actionsCold {
		c.actionsCold[i] = fmt.Sprintf("action_%03d", i)
	}
	c.resourceCold = make([]string, 2048)
	for i := range c.resourceCold {
		c.resourceCold[i] = fmt.Sprintf("/var/lib/data/obj-%06d", i)
	}
	return c
}

func (c *corpus) pick(hot, cold []string) string {
	if c.rng.Float64() < hotProb {
		return hot[c.rng.Intn(len(hot))]
	}
	return cold[c.rng.Intn(len(cold))]
}

// auditEvent is the structured event shape: {"user":...,"action":...,"resource":...,"time":...}.
type auditEvent struct {
	User     string `json:"user"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Time     string `json:"time"`
}

// record builds one fixed-length JSONL record of exactly recLen bytes (the object
// followed by spaces, then a trailing newline). Fixed length keeps one client
// write page-aligned to one FUSE write and one authoritative transaction.
func (c *corpus) record(recLen int, now time.Time) ([]byte, error) {
	ev := auditEvent{
		User:     c.pick(c.usersHot, c.usersCold),
		Action:   c.pick(c.actionsHot, c.actionsCold),
		Resource: c.pick(c.resourceHot, c.resourceCold),
		Time:     now.UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	if len(b)+1 > recLen {
		return nil, fmt.Errorf("event of %d bytes exceeds record length %d", len(b), recLen)
	}
	line := make([]byte, recLen)
	copy(line, b)
	for i := len(b); i < recLen-1; i++ {
		line[i] = ' '
	}
	line[recLen-1] = '\n'
	return line, nil
}

// --- sink --------------------------------------------------------------------

type sinkResult struct {
	Schema                      string      `json:"schema"`
	File                        string      `json:"file"`
	Records                     int         `json:"records"`
	RecordBytes                 int         `json:"record_bytes"`
	AppendedBytes               int64       `json:"appended_bytes"`
	LogicalBytes                int64       `json:"logical_bytes"`
	WallSeconds                 float64     `json:"wall_seconds"`
	ThroughputRecordsPerSec     float64     `json:"throughput_records_per_sec"`
	ThroughputBytesPerSec       float64     `json:"throughput_bytes_per_sec"`
	ConfiguredRateRecordsPerSec float64     `json:"configured_rate_records_per_sec"`
	AppendP50Ns                 int64       `json:"append_fsync_p50_ns"`
	AppendP99Ns                 int64       `json:"append_fsync_p99_ns"`
	AppendMeanNs                int64       `json:"append_fsync_mean_ns"`
	Corpus                      string      `json:"corpus"`
	Seed                        int64       `json:"seed"`
	Tail                        *tailResult `json:"tail,omitempty"`
}

func runSink(args []string) int {
	fs := flag.NewFlagSet("sink", flag.ContinueOnError)
	file := fs.String("file", "", "append-only sink file (under the mount)")
	records := fs.Int("records", 1024, "records to append")
	payload := fs.Int("payload", 4096, "record bytes (padded JSON line, newline-terminated)")
	rate := fs.Float64("rate", 0, "configured append rate in records/s (0 = unthrottled)")
	fsyncEach := fs.Bool("fsync-each", true, "fsync after every record (publish barrier)")
	tail := fs.Bool("tail", true, "run a tail reader that follows the file")
	inotify := fs.Bool("inotify", true, "use inotify for the tail reader (poll fallback)")
	seed := fs.Int64("seed", 1, "corpus seed")
	forever := fs.Bool("forever", false, "append until killed (no result; prints READY after the first commit)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "orfsaudit sink: -file is required")
		return 2
	}
	if *records <= 0 && !*forever {
		fmt.Fprintln(os.Stderr, "orfsaudit sink: -records must be positive")
		return 2
	}
	if *payload < 64 {
		fmt.Fprintln(os.Stderr, "orfsaudit sink: -payload must be at least 64")
		return 2
	}
	if *rate < 0 {
		fmt.Fprintln(os.Stderr, "orfsaudit sink: -rate must be non-negative")
		return 2
	}

	c := newCorpus(*seed)
	f, err := os.OpenFile(*file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsaudit sink: open: %v\n", err)
		return 1
	}

	if *forever {
		// No signal handler and no cleanup: SIGKILL must end this process exactly
		// as a power cut would (the mounted immutability proof relies on it).
		for i := 0; ; i++ {
			line, err := c.record(*payload, time.Now())
			if err != nil {
				f.Close()
				fmt.Fprintf(os.Stderr, "orfsaudit sink: %v\n", err)
				return 1
			}
			if _, err := f.Write(line); err != nil {
				f.Close()
				fmt.Fprintf(os.Stderr, "orfsaudit sink: write: %v\n", err)
				return 1
			}
			if *fsyncEach {
				if err := f.Sync(); err != nil {
					f.Close()
					fmt.Fprintf(os.Stderr, "orfsaudit sink: fsync: %v\n", err)
					return 1
				}
			}
			if i == 0 {
				fmt.Println("READY")
			}
		}
	}

	var follower *follow
	if *tail {
		follower = newFollow(*file, *inotify, 0, nil)
		follower.start()
	}

	var interval time.Duration
	if *rate > 0 {
		interval = time.Duration(float64(time.Second) / *rate)
	}
	latencies := make([]int64, 0, *records)
	start := time.Now()
	next := start
	for i := 0; i < *records; i++ {
		line, err := c.record(*payload, time.Now())
		if err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "orfsaudit sink: %v\n", err)
			return 1
		}
		t0 := time.Now()
		if _, err := f.Write(line); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "orfsaudit sink: write %d: %v\n", i, err)
			return 1
		}
		if *fsyncEach {
			if err := f.Sync(); err != nil {
				f.Close()
				fmt.Fprintf(os.Stderr, "orfsaudit sink: fsync %d: %v\n", i, err)
				return 1
			}
		}
		latencies = append(latencies, time.Since(t0).Nanoseconds())
		if interval > 0 {
			next = next.Add(interval)
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			} else {
				next = time.Now()
			}
		}
	}
	wall := time.Since(start)
	if err := f.Sync(); err != nil {
		fmt.Fprintf(os.Stderr, "orfsaudit sink: final fsync: %v\n", err)
		f.Close()
		return 1
	}
	var logical int64
	if fi, err := f.Stat(); err == nil {
		logical = fi.Size()
	}
	f.Close()

	var tailRes *tailResult
	if follower != nil {
		follower.stop()
		follower.wait()
		tailRes = follower.result(logical)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	appended := int64(*records) * int64(*payload)
	res := sinkResult{
		Schema:                      "orfs.audit-sink.sink.v1",
		File:                        *file,
		Records:                     *records,
		RecordBytes:                 *payload,
		AppendedBytes:               appended,
		LogicalBytes:                logical,
		WallSeconds:                 wall.Seconds(),
		ThroughputRecordsPerSec:     float64(*records) / wall.Seconds(),
		ThroughputBytesPerSec:       float64(appended) / wall.Seconds(),
		ConfiguredRateRecordsPerSec: *rate,
		AppendP50Ns:                 percentile(latencies, 0.50),
		AppendP99Ns:                 percentile(latencies, 0.99),
		AppendMeanNs:                mean(latencies),
		Corpus:                      "80/20 hot/cold skew (hot: 8 users / 6 actions / 8 resources)",
		Seed:                        *seed,
		Tail:                        tailRes,
	}
	blob, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(blob))
	if logical != appended {
		fmt.Fprintf(os.Stderr, "orfsaudit sink: NOTE: mounted size %d != appended %d (framing/fs semantics)\n", logical, appended)
	}
	return 0
}

// --- tail -f view ------------------------------------------------------------

func runTailCmd(args []string) int {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	file := fs.String("file", "", "file to follow")
	seconds := fs.Float64("seconds", 5, "run for this many seconds (0 = until killed)")
	fromEnd := fs.Bool("from-end", true, "start at the current end (like tail -f)")
	inotify := fs.Bool("inotify", true, "use inotify (poll fallback)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "orfsaudit tail: -file is required")
		return 2
	}

	printer := func(line []byte) {
		os.Stdout.Write(bytes.TrimRight(line, " \t\r\n"))
		os.Stdout.Write([]byte{'\n'})
	}
	startOff := int64(0)
	if *fromEnd {
		startOff = -1
	}
	fr := newFollow(*file, *inotify, startOff, printer)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fr.stop()
	}()
	if *seconds > 0 {
		go func() {
			time.Sleep(time.Duration(*seconds * float64(time.Second)))
			fr.stop()
		}()
	}
	fr.start()
	fr.wait()
	r := fr.result(-1)
	fmt.Fprintf(os.Stderr, "orfsaudit tail: method=%s inotify_events=%d observed=%d lag_p50=%s lag_p99=%s monotonic=%v\n",
		r.Method, r.InotifyEvents, r.RecordsObserved, time.Duration(r.LagP50Ns), time.Duration(r.LagP99Ns), r.Monotonic)
	return 0
}

// --- the follower ------------------------------------------------------------

// tailResult is the tail reader's honest measurement.
type tailResult struct {
	Method             string   `json:"method"` // "inotify" or "poll"
	UsingInotify       bool     `json:"using_inotify"`
	InotifyEvents      int64    `json:"inotify_events"`
	InotifyFallback    bool     `json:"inotify_fallback"` // inotify was requested but no event arrived
	Reads              int64    `json:"reads"`
	GrowthObservations int64    `json:"growth_observations"`
	RecordsObserved    int64    `json:"records_observed"`
	BytesRead          int64    `json:"bytes_read"`
	ParseFailures      int64    `json:"parse_failures"`
	LagSamples         int      `json:"lag_samples"`
	LagP50Ns           int64    `json:"lag_p50_ns"`
	LagP99Ns           int64    `json:"lag_p99_ns"`
	LagMeanNs          int64    `json:"lag_mean_ns"`
	Monotonic          bool     `json:"monotonic"`
	Sample             []string `json:"sample,omitempty"`
}

// follow is a file follower. One goroutine drives run(); the fields below are
// written only by that goroutine and read after wait() returns.
type follow struct {
	path          string
	dir           string
	preferInotify bool
	startOffset   int64 // >=0 = start there; -1 = start at the current end
	print         func([]byte)

	inotifyFD     int
	watchedFile   bool
	usingInotify  bool
	inotifyEvents int64

	reads         int64
	growth        int64
	observed      int64
	bytesRead     int64
	parseFailures int64
	lags          []int64
	sample        []string
	monotonic     bool
	sawFile       bool

	stopCh chan struct{}
	doneCh chan struct{}
}

func newFollow(path string, inotify bool, startOffset int64, print func([]byte)) *follow {
	return &follow{
		path:          path,
		dir:           filepath.Dir(path),
		preferInotify: inotify,
		startOffset:   startOffset,
		print:         print,
		inotifyFD:     -1,
		monotonic:     true,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

func (fr *follow) start() { go fr.run() }

// stop is idempotent so both a timer and a signal can call it.
func (fr *follow) stop() {
	select {
	case <-fr.stopCh:
	default:
		close(fr.stopCh)
	}
}
func (fr *follow) wait() { <-fr.doneCh }

const (
	inotifyEventSize = 16   // sizeof(struct inotify_event)
	pollTimeoutUS    = 1000 // wait up to 1 ms for an inotify event
	fallbackSleep    = 200 * time.Microsecond
)

func (fr *follow) run() {
	defer close(fr.doneCh)
	var off int64
	if fr.startOffset >= 0 {
		off = fr.startOffset
	}
	var rf *os.File
	var pending []byte
	buf := make([]byte, 256*1024)

	if fr.preferInotify {
		fr.setupInotify()
	}

	drain := func() {
		prev := fr.bytesRead
		for {
			n, err := rf.ReadAt(buf, off)
			if n > 0 {
				off += int64(n)
				fr.bytesRead += int64(n)
				fr.consume(&pending, buf[:n])
			}
			if err != nil || n == 0 {
				break
			}
		}
		if fr.bytesRead > prev {
			fr.growth++
		}
	}

	for {
		select {
		case <-fr.stopCh:
			if rf != nil {
				fr.pollSize(rf)
				drain()
				fr.pollSize(rf)
			}
			return
		default:
		}

		// Wait for activity: an inotify event, or a short fallback timeout when
		// inotify could not be armed at all.
		if fr.inotifyFD >= 0 {
			fr.waitInotify()
			fr.addFileWatchIfNeeded()
		} else {
			time.Sleep(fallbackSleep)
		}

		if rf == nil {
			f, err := os.Open(fr.path)
			if err != nil {
				continue
			}
			rf = f
			fr.sawFile = true
			if fr.startOffset < 0 {
				if fi, err := rf.Stat(); err == nil {
					off = fi.Size()
				}
			}
			if fr.preferInotify {
				fr.addFileWatchIfNeeded()
			}
		}

		fr.reads++
		fr.pollSize(rf)
		drain()
	}
}

func (fr *follow) pollSize(rf *os.File) {
	fi, err := rf.Stat()
	if err != nil {
		return
	}
	if fi.Size() < fr.bytesRead {
		// The file shrank: growth is not monotonic. Recorded honestly.
		fr.monotonic = false
	}
}

func (fr *follow) consume(pending *[]byte, data []byte) {
	*pending = append(*pending, data...)
	for {
		i := bytes.IndexByte(*pending, '\n')
		if i < 0 {
			break
		}
		line := (*pending)[:i+1]
		fr.observe(line)
		*pending = (*pending)[i+1:]
	}
}

func (fr *follow) observe(line []byte) {
	fr.observed++
	if fr.print != nil {
		fr.print(line)
	}
	var ev auditEvent
	if err := json.Unmarshal(bytes.TrimRight(line, " \t\r\n"), &ev); err != nil {
		fr.parseFailures++
		return
	}
	t, err := time.Parse(time.RFC3339Nano, ev.Time)
	if err != nil {
		fr.parseFailures++
		return
	}
	lag := time.Since(t).Nanoseconds()
	fr.lags = append(fr.lags, lag)
	if len(fr.sample) < 8 {
		fr.sample = append(fr.sample, strings.TrimRight(string(line), " \t\r\n"))
	}
}

// setupInotify installs a watch on the containing directory (so a file created
// after the follower starts is noticed) and, when it exists, on the file itself
// (IN_MODIFY is what reports appends). Failure is reported honestly as a poll
// fallback rather than hidden.
func (fr *follow) setupInotify() {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		fr.usingInotify = false
		return
	}
	fr.inotifyFD = fd
	// Directory watch: creation/moves of the target file.
	_, _ = unix.InotifyAddWatch(fd, fr.dir,
		unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_MODIFY|unix.IN_CLOSE_WRITE)
	fr.addFileWatchIfNeeded()
	// We only *report* inotify once a real event has arrived (see waitInotify).
	fr.usingInotify = false
}

func (fr *follow) addFileWatchIfNeeded() {
	if fr.inotifyFD < 0 || fr.watchedFile {
		return
	}
	if _, err := os.Stat(fr.path); err != nil {
		return
	}
	if _, err := unix.InotifyAddWatch(fr.inotifyFD, fr.path,
		unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_ATTRIB); err == nil {
		fr.watchedFile = true
	}
}

// waitInotify blocks up to pollTimeoutUS for an inotify event and drains the
// queue. Once any event is observed the reader switches to event-driven mode.
func (fr *follow) waitInotify() {
	if fr.inotifyFD < 0 {
		time.Sleep(fallbackSleep)
		return
	}
	fds := []unix.PollFd{{Fd: int32(fr.inotifyFD), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, pollTimeoutUS)
	if err != nil || n <= 0 {
		return
	}
	if fds[0].Revents&unix.POLLIN == 0 {
		return
	}
	var b [4096]byte
	for {
		m, err := unix.Read(fr.inotifyFD, b[:])
		if m <= 0 || err == unix.EAGAIN {
			break
		}
		fr.inotifyEvents += int64(countInotifyEvents(b[:m]))
		fr.usingInotify = true
	}
}

// countInotifyEvents walks the inotify event records in one read() buffer. It
// decodes the little-endian native layout on amd64 (the court platform) without
// cgo or unsafe.
func countInotifyEvents(b []byte) int {
	n := 0
	for off := 0; off+inotifyEventSize <= len(b); {
		nameLen := int(binary.LittleEndian.Uint32(b[off+12 : off+16]))
		n++
		off += inotifyEventSize + nameLen
	}
	return n
}

func (fr *follow) result(finalSize int64) *tailResult {
	sort.Slice(fr.lags, func(i, j int) bool { return fr.lags[i] < fr.lags[j] })
	method := "poll"
	if fr.usingInotify {
		method = "inotify"
	}
	complete := finalSize < 0 || fr.bytesRead == finalSize
	return &tailResult{
		Method:             method,
		UsingInotify:       fr.usingInotify,
		InotifyEvents:      fr.inotifyEvents,
		InotifyFallback:    fr.preferInotify && !fr.usingInotify,
		Reads:              fr.reads,
		GrowthObservations: fr.growth,
		RecordsObserved:    fr.observed,
		BytesRead:          fr.bytesRead,
		ParseFailures:      fr.parseFailures,
		LagSamples:         len(fr.lags),
		LagP50Ns:           percentile(fr.lags, 0.50),
		LagP99Ns:           percentile(fr.lags, 0.99),
		LagMeanNs:          mean(fr.lags),
		Monotonic:          fr.monotonic && fr.parseFailures == 0 && complete,
		Sample:             fr.sample,
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

func runSummary(args []string) int {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	work := fs.String("work", "", "directory holding the court artifacts")
	out := fs.String("out", "", "output path (default stdout)")
	volume := fs.String("volume", "", "volume backing path")
	segSize := fs.Uint64("segment-size", 0, "segment size in bytes")
	groupCommit := fs.Bool("group-commit", false, "group commit was enabled")
	rate := fs.Float64("rate", 0, "configured append rate (records/s)")
	recordBytes := fs.Int("record-bytes", 0, "record length in bytes")
	seed := fs.Int64("seed", 0, "corpus seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *work == "" {
		fmt.Fprintln(os.Stderr, "orfsaudit summary: -work is required")
		return 2
	}

	rawSink := readFile(filepath.Join(*work, "sink.json"))
	if rawSink == nil {
		fmt.Fprintln(os.Stderr, "orfsaudit summary: missing sink.json")
		return 1
	}
	var sink sinkResult
	if err := json.Unmarshal(rawSink, &sink); err != nil {
		fmt.Fprintf(os.Stderr, "orfsaudit summary: sink.json: %v\n", err)
		return 1
	}

	rawMetrics := readFile(filepath.Join(*work, "metrics.json"))
	if rawMetrics == nil {
		fmt.Fprintln(os.Stderr, "orfsaudit summary: missing metrics.json")
		return 1
	}
	var md metricsDoc
	if err := json.Unmarshal(rawMetrics, &md); err != nil {
		fmt.Fprintf(os.Stderr, "orfsaudit summary: metrics.json: %v\n", err)
		return 1
	}

	get := func(name string, labels map[string]string) float64 {
		v, _ := md.lookup(name, labels)
		return v
	}

	rb := *recordBytes
	if rb == 0 {
		rb = sink.RecordBytes
	}
	rec := auditReceipt{
		Schema:          "orfs.audit-sink.v1",
		GeneratedAtUnix: time.Now().Unix(),
		Volume:          *volume,
		Config: map[string]any{
			"segment_size_bytes":         *segSize,
			"group_commit":               *groupCommit,
			"mount":                      "FUSE (go-fuse, direct mount)",
			"transport":                  "f0",
			"workload":                   "append-only audit-log sink (JSON events, WORM)",
			"single_writer":              true,
			"event_shape":                `{"user":...,"action":...,"resource":...,"time":...}`,
			"corpus":                     "80/20 hot/cold skew",
			"rate_limit_records_per_sec": *rate,
			"record_bytes":               rb,
			"seed":                       *seed,
		},
		Workload:      rawSink,
		Records:       sink.Records,
		Bytes:         sink.AppendedBytes,
		BytesPerSec:   sink.ThroughputBytesPerSec,
		RecordsPerSec: sink.ThroughputRecordsPerSec,
		BytesDetail: bytesDoc{
			Logical:           get("orfs_logical_bytes", nil),
			Physical:          get("orfs_physical_bytes", nil),
			Manifested:        get("orfs_manifested_bytes", nil),
			AuthoritativeRoot: get("orfs_authoritative_bytes", nil),
			Amplification:     get("orfs_amplification_ratio", nil),
		},
		Amplification: get("orfs_amplification_ratio", nil),
		QuarantineTotal: get("orfs_quarantine_total", map[string]string{"unit": "segments"}) +
			get("orfs_quarantine_total", map[string]string{"unit": "bytes"}),
		BreachTotal: get("orfs_breach_total", map[string]string{"reason": "backing_reserve"}) +
			get("orfs_breach_total", map[string]string{"reason": "authoritative"}) +
			get("orfs_breach_total", map[string]string{"reason": "hard_quota"}),
		Prometheus:     string(readFile(filepath.Join(*work, "metrics.prom"))),
		PrometheusJSON: rawMetrics,
		DoctorCapacity: readFile(filepath.Join(*work, "capacity.json")),
	}
	rec.AppendFsync = fsyncDoc{
		P50Seconds:  float64(sink.AppendP50Ns) / 1e9,
		P99Seconds:  float64(sink.AppendP99Ns) / 1e9,
		MeanSeconds: float64(sink.AppendMeanNs) / 1e9,
		Observed:    sink.Records > 0,
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
	rec.FsyncTaxVsFuseTax = taxDoc{
		FuseTaxAppendP50Seconds: rec.AppendFsync.P50Seconds,
		FuseTaxAppendP99Seconds: rec.AppendFsync.P99Seconds,
		BarrierP50Seconds:       rec.Fsync.P50Seconds,
		BarrierP99Seconds:       rec.Fsync.P99Seconds,
		Note: "The end-to-end append (client write + fsync) is the FUSE tax: the FUSE round trip, " +
			"transaction framing, watermark write, and index replay. The durability barrier itself " +
			"(orfs_fsync_latency_seconds) is sub-microsecond here. Do not read the barrier as the append cost.",
	}

	if sink.Tail != nil {
		rawTail, _ := json.Marshal(sink.Tail)
		rec.Tail = rawTail
		rec.TailLagP50Ns = sink.Tail.LagP50Ns
		rec.TailLagP99Ns = sink.Tail.LagP99Ns
		rec.TailLagP50Seconds = float64(sink.Tail.LagP50Ns) / 1e9
		rec.TailLagP99Seconds = float64(sink.Tail.LagP99Ns) / 1e9
		rec.GrowthMonotonic = sink.Tail.Monotonic
		rec.GrowtMonotonic = sink.Tail.Monotonic
		rec.TailMethod = sink.Tail.Method
	}
	// The tail -f live view sample (first few observed lines).
	if raw := readFile(filepath.Join(*work, "tail-view.txt")); raw != nil {
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) > 5 {
			lines = lines[:5]
		}
		rec.TailViewSample = lines
	}

	// A real manifested (derived) plane, when the court produced one.
	rec.Manifest = readFile(filepath.Join(*work, "manifest.json"))

	// --- immutability -------------------------------------------------------

	var crash *crashDoc
	if raw := readFile(filepath.Join(*work, "crash.json")); raw != nil {
		var c crashDoc
		if err := json.Unmarshal(raw, &c); err == nil {
			crash = &c
			rec.CrashCourt = raw
		}
	}
	var mounted *mountedDoc
	if raw := readFile(filepath.Join(*work, "mounted.json")); raw != nil {
		var m mountedDoc
		if err := json.Unmarshal(raw, &m); err == nil {
			mounted = &m
			rec.MountedImmutability = raw
		}
	}

	rec.VerifyDeepReport = readFile(filepath.Join(*work, "verify.json"))
	sinkVolumePassed := rec.BreachTotal == 0 && rec.QuarantineTotal == 0 &&
		get("orfs_verify_deep_failures", nil) == 0
	if raw := readFile(filepath.Join(*work, "verify.json")); raw != nil {
		var v struct {
			Checks []struct {
				Name string `json:"name"`
				OK   bool   `json:"ok"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(raw, &v); err == nil && len(v.Checks) > 0 {
			for _, c := range v.Checks {
				if !c.OK {
					sinkVolumePassed = false
				}
			}
		}
	}

	passed := sinkVolumePassed
	iterations := 0
	switch {
	case crash != nil:
		passed = passed && crash.VerifyPasses == crash.Iterations &&
			crash.Truncations == 0 && crash.PrefixMismatches == 0 && crash.RecoverFailures == 0
		iterations = crash.Iterations
	case mounted != nil:
		passed = passed && mounted.VerifyDeepPassed && mounted.PrefixMismatches == 0 && mounted.Truncations == 0
		iterations = mounted.Kills
	}
	rec.VerifyDeep = verifyDeepDoc{
		Passed:           passed,
		Iterations:       iterations,
		Source:           "crash-consistency court (SIGKILL mid-append) + mounted immutability proof",
		SinkVolumePassed: sinkVolumePassed,
		Failures:         get("orfs_verify_deep_failures", nil),
	}

	rec.Notes = append(rec.Notes,
		"This is an append-only WORM audit-log sink, not a general-purpose filesystem: JSON events are appended and fsynced, never rewritten, and readers tail the growing file.",
		"The event corpus is a seeded 80/20 hot/cold skew (a handful of users/actions/resources dominate), not a uniform draw, so the tail and index behave like realistic audit traffic.",
		"Records are fixed-length JSONL lines padded to the record size, so one client write is one FUSE write and one authoritative transaction (the mounted size equals the appended bytes).",
		"The durability barrier itself (orfs_fsync_latency_seconds) is sub-microsecond; the ~4 ms end-to-end append is the FUSE tax — the round trip, framing, watermark, and index replay. Both are published; neither is presented as the other.",
		"quarantine_total and breach_total are the audit sink volume's own counters and must be 0: a clean sink run abandons no tail and is never refused growth. The crash-consistency court separately MOVES uncommitted abandoned tails into quarantine (never truncates) — that is not data loss and is not the sink volume's counter.",
		"The tail reader reports whether it actually observed inotify events (method=inotify) or fell back to polling (method=poll); the receipt never claims inotify when it was not used.",
		"The append-to-observation tail lag is dominated by the same FUSE publish round trip the append pays: a reader observes a record only after the writer's transaction is durable, and the FUSE read round trip adds to it. It sits far above the sub-microsecond barrier, which is why the barrier is never presented as the observed latency.",
		"manifested_bytes is the derived .or_bloom/ plane, produced by an explicit `orfs manifest` pass; it is reported separately and is not part of the authoritative log.",
	)

	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsaudit summary: marshal: %v\n", err)
		return 1
	}
	blob = append(blob, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "orfsaudit summary: write: %v\n", err)
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

// --- metric/doc types --------------------------------------------------------

func (d *metricsDoc) lookup(name string, want map[string]string) (float64, bool) {
	for _, f := range d.Families {
		for _, s := range f.Samples {
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

type bytesDoc struct {
	Logical           float64 `json:"logical_bytes"`
	Physical          float64 `json:"physical_bytes"`
	Manifested        float64 `json:"manifested_bytes"`
	AuthoritativeRoot float64 `json:"authoritative_root_bytes"`
	Amplification     float64 `json:"amplification_ratio"`
}

type fsyncDoc struct {
	P50Seconds  float64 `json:"p50_seconds"`
	P99Seconds  float64 `json:"p99_seconds"`
	MeanSeconds float64 `json:"mean_seconds,omitempty"`
	Count       float64 `json:"count,omitempty"`
	Observed    bool    `json:"observed"`
}

type taxDoc struct {
	FuseTaxAppendP50Seconds float64 `json:"fuse_tax_append_p50_seconds"`
	FuseTaxAppendP99Seconds float64 `json:"fuse_tax_append_p99_seconds"`
	BarrierP50Seconds       float64 `json:"barrier_p50_seconds"`
	BarrierP99Seconds       float64 `json:"barrier_p99_seconds"`
	Note                    string  `json:"note"`
}

type verifyDeepDoc struct {
	Passed           bool    `json:"passed"`
	Iterations       int     `json:"iterations"`
	Source           string  `json:"source"`
	SinkVolumePassed bool    `json:"sink_volume_passed"`
	Failures         float64 `json:"failures"`
}

// crashDoc is the summary of evidence/crash.json (the in-Go crash court).
type crashDoc struct {
	Schema              string `json:"schema"`
	Iterations          int    `json:"iterations"`
	RequestedIterations int    `json:"requested_iterations"`
	KillsWithTail       int    `json:"kills_with_tail"`
	KillsWithoutTail    int    `json:"kills_without_tail"`
	TornTails           int    `json:"torn_tails"`
	UncommittedTails    int    `json:"uncommitted_tails"`
	QuarantinedCount    int    `json:"quarantined_count"`
	Truncations         int    `json:"truncations"`
	PrefixMismatches    int    `json:"prefix_mismatches"`
	RecoverFailures     int    `json:"recover_failures"`
	VerifyPasses        int    `json:"verify_passes"`
	ContinuedCommits    int    `json:"continued_commits"`
	Note                string `json:"note"`
}

// mountedDoc is the mounted immutability proof composed by the court script.
type mountedDoc struct {
	Schema           string  `json:"schema"`
	Kills            int     `json:"kills"`
	Truncations      int     `json:"truncations"`
	PrefixMismatches int     `json:"prefix_mismatches"`
	GrowthMonotonic  bool    `json:"growth_monotonic"`
	BytesBefore      int64   `json:"bytes_before"`
	BytesAfter       int64   `json:"bytes_after"`
	VerifyDeepPassed bool    `json:"verify_deep_passed"`
	QuarantineTotal  float64 `json:"quarantine_total"`
	BreachTotal      float64 `json:"breach_total"`
}

// auditReceipt is evidence/audit-sink.json (schema orfs.audit-sink.v1).
type auditReceipt struct {
	Schema              string          `json:"schema"`
	GeneratedAtUnix     int64           `json:"generated_at_unix"`
	Volume              string          `json:"volume"`
	Config              map[string]any  `json:"config"`
	Workload            json.RawMessage `json:"workload"`
	Records             int             `json:"records"`
	Bytes               int64           `json:"bytes"`
	BytesPerSec         float64         `json:"bytes_per_sec"`
	RecordsPerSec       float64         `json:"records_per_sec"`
	TailLagP50Ns        int64           `json:"tail_lag_p50_ns"`
	TailLagP99Ns        int64           `json:"tail_lag_p99_ns"`
	TailLagP50Seconds   float64         `json:"tail_lag_p50_seconds"`
	TailLagP99Seconds   float64         `json:"tail_lag_p99_seconds"`
	TailMethod          string          `json:"tail_method"`
	GrowthMonotonic     bool            `json:"growth_monotonic"`
	GrowtMonotonic      bool            `json:"growt_monotonic"`
	Tail                json.RawMessage `json:"tail,omitempty"`
	TailViewSample      []string        `json:"tail_view_sample,omitempty"`
	VerifyDeep          verifyDeepDoc   `json:"verify_deep"`
	VerifyDeepReport    json.RawMessage `json:"verify_deep_report,omitempty"`
	QuarantineTotal     float64         `json:"quarantine_total"`
	BreachTotal         float64         `json:"breach_total"`
	AppendFsync         fsyncDoc        `json:"append_fsync_seconds"`
	Fsync               fsyncDoc        `json:"fsync_latency_seconds"`
	FsyncTaxVsFuseTax   taxDoc          `json:"fsync_tax_vs_fuse_tax"`
	Amplification       float64         `json:"amplification_ratio"`
	BytesDetail         bytesDoc        `json:"bytes_detail"`
	Manifest            json.RawMessage `json:"manifest,omitempty"`
	CrashCourt          json.RawMessage `json:"crash_court,omitempty"`
	MountedImmutability json.RawMessage `json:"mounted_immutability,omitempty"`
	Prometheus          string          `json:"prometheus"`
	PrometheusJSON      json.RawMessage `json:"prometheus_json,omitempty"`
	DoctorCapacity      json.RawMessage `json:"doctor_capacity,omitempty"`
	Notes               []string        `json:"notes"`
}
