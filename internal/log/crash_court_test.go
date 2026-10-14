package log_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/fsck"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
)

// The crash-consistency court. It kills a REAL writer process with SIGKILL at a
// random point mid-append, reopens the volume (which quarantines any abandoned
// tail), and proves recovery picks the newest valid committed generation, that
// the abandoned tail is MOVED (never truncated), that the committed prefix is
// byte-identical, and that `verify --deep` passes. It then publishes one more
// transaction to prove the log continues at the recovered root.
//
// Segment sizing is deliberate. The writer publishes one transaction per
// committed append, and the volume is created with a segment size equal to the
// genesis frame plus one transaction, so every committed transaction ends exactly
// at a segment boundary and the NEXT append necessarily begins in a fresh
// trailing segment (its id is the recovered committed root). A SIGKILL mid-append
// therefore always leaves the abandoned tail in a wholly-new segment, which is
// what recovery quarantines by moving. A tail that began inside a committed
// segment would instead be retained in place by design (see
// `TestTornTailRecovery`); the crash court arranges the movable case.
//
// The court is honest about volume: it kills at a random interval, so kills that
// land after a transaction's bytes are complete (e.g. during its fsync barrier)
// recover with no tail at all; those are counted as kills_without_tail.

// crashRecordsPerTx is the number of INODE_CREATE records per transaction. A
// larger value lengthens the write phase relative to the fsync barrier, so more
// kills land mid-append (the interesting case). It is fixed so the framed
// transaction length is deterministic.
const crashRecordsPerTx = 256

// crashGenesisFrame is the framed length of the genesis record written by
// log.Create: HeaderLen + FooterLen + GenesisPayloadLen.
const crashGenesisFrame = uint64(record.EnvelopeOverhead + record.GenesisPayloadLen)

// crashTxFrame is the framed length of one transaction of crashRecordsPerTx
// INODE_CREATE records: TX_BEGIN + records*INODE_CREATE + TX_COMMIT.
func crashTxFrame(records int) uint64 {
	return uint64(record.EnvelopeOverhead+record.TxBeginPayloadLen) +
		uint64(records)*uint64(record.EnvelopeOverhead+record.InodeCreatePayloadLen) +
		uint64(record.EnvelopeOverhead+record.TxCommitPayloadLen)
}

// crashSegmentSize makes every committed transaction end exactly at a segment
// boundary: genesis frame + one transaction. After such a transaction the writer
// is full, so the next append rolls into a fresh segment whose id equals the
// committed root.
func crashSegmentSize(records int) uint64 {
	return crashGenesisFrame + crashTxFrame(records)
}

// crashGenesis is the fixed volume identity used by the court.
func crashGenesis(records int) record.Genesis {
	return record.Genesis{
		UUID:        [16]byte{0xc1, 0xa5, 0x5b, 0xc0, 0x0f, 0x11, 0x4e, 0x00, 0x9f, 0x00, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		SegmentSize: crashSegmentSize(records),
	}
}

// crashTx mirrors cmd/orfscrashtest's transaction shape exactly: `records`
// INODE_CREATE records.
func crashTx(id uint64, records int) []*record.Record {
	recs := make([]*record.Record, 0, records)
	for j := 0; j < records; j++ {
		p := record.InodeCreate{
			InodeID:    id*1000 + uint64(j),
			Generation: 1,
			Kind:       record.KindFile,
			Mode:       0o644,
			Nlink:      1,
		}.Encode()
		recs = append(recs, &record.Record{
			Header:  record.Header{RecordType: record.TypeInodeCreate, PayloadLength: uint64(len(p))},
			Payload: p,
		})
	}
	return recs
}

// crashIteration is one killed-writer recovery, for the receipt's sample.
type crashIteration struct {
	Index            int    `json:"i"`
	Tail             bool   `json:"tail"`
	TailKind         string `json:"tail_kind,omitempty"`
	TailAbandoned    uint64 `json:"tail_abandoned_bytes,omitempty"`
	Quarantined      bool   `json:"quarantined"`
	QuarantinedBytes uint64 `json:"quarantined_bytes,omitempty"`
	RecoveredRoot    uint64 `json:"recovered_root"`
	PrefixBytes      uint64 `json:"prefix_bytes"`
	PrefixIdentical  bool   `json:"prefix_identical"`
	RootIsBoundary   bool   `json:"root_is_boundary"`
	VerifyPassed     bool   `json:"verify_passed"`
	Continued        bool   `json:"continued"`
}

// crashReceipt is evidence/crash.json (schema orfs.performance.crash.v1).
type crashReceipt struct {
	Schema              string           `json:"schema"`
	Method              string           `json:"method"`
	Iterations          int              `json:"iterations"`
	RequestedIterations int              `json:"requested_iterations"`
	KillsWithTail       int              `json:"kills_with_tail"`
	KillsWithoutTail    int              `json:"kills_without_tail"`
	TornTails           int              `json:"torn_tails"`
	UncommittedTails    int              `json:"uncommitted_tails"`
	QuarantinedCount    int              `json:"quarantined_count"`
	Truncations         int              `json:"truncations"`
	PrefixMismatches    int              `json:"prefix_mismatches"`
	RecoverFailures     int              `json:"recover_failures"`
	VerifyPasses        int              `json:"verify_passes"`
	ContinuedCommits    int              `json:"continued_commits"`
	SegmentSize         uint64           `json:"segment_size"`
	RecordsPerTx        int              `json:"records_per_tx"`
	TxBytes             uint64           `json:"tx_bytes"`
	WallSeconds         float64          `json:"wall_seconds"`
	IterationsSample    []crashIteration `json:"iterations_sample"`
	Note                string           `json:"note"`
}

// TestCrashConsistencyCourt is the crash-consistency evidence court.
func TestCrashConsistencyCourt(t *testing.T) {
	if testing.Short() {
		t.Skip("crash court skipped in -short mode")
	}
	iters := envInt("ORFS_CRASH_ITERS", 120)
	budget := time.Duration(envInt("ORFS_CRASH_BUDGET_S", 45)) * time.Second
	maxWait := time.Duration(envInt("ORFS_CRASH_MAXWAIT_US", 1500)) * time.Microsecond
	seed := int64(envInt("ORFS_CRASH_SEED", 1))

	bin := buildCrashWriter(t)
	base := t.TempDir()
	rng := rand.New(rand.NewSource(seed))

	rec := crashReceipt{
		Schema: "orfs.performance.crash.v1",
		Method: "SIGKILL the cmd/orfscrashtest writer mid-append, then reopen read-write " +
			"(quarantining the abandoned tail) and verify the committed prefix is unchanged",
		RequestedIterations: iters,
		SegmentSize:         crashSegmentSize(crashRecordsPerTx),
		RecordsPerTx:        crashRecordsPerTx,
		TxBytes:             crashTxFrame(crashRecordsPerTx),
	}
	start := time.Now()
	for i := 0; i < iters; i++ {
		if time.Since(start) > budget {
			t.Logf("crash court: wall-clock budget reached after %d/%d iterations", i, iters)
			break
		}
		iter := runCrashIteration(t, bin, base, i, rng, maxWait, &rec)
		rec.IterationsSample = append(rec.IterationsSample, iter)
	}
	rec.Iterations = len(rec.IterationsSample)
	rec.WallSeconds = time.Since(start).Seconds()
	rec.KillsWithTail = rec.TornTails + rec.UncommittedTails
	rec.KillsWithoutTail = rec.Iterations - rec.KillsWithTail
	rec.Note = crashNote(rec)
	rec.IterationsSample = sampleIterations(rec.IterationsSample, rec.Iterations)

	if os.Getenv("ORFS_EVIDENCE") == "1" {
		writeCrashEvidence(t, rec)
	}

	// The court's contract: no lost bytes, no rewritten committed prefix, no
	// failed recovery.
	if rec.Iterations == 0 {
		t.Fatal("crash court ran zero iterations")
	}
	if rec.Truncations != 0 || rec.PrefixMismatches != 0 || rec.RecoverFailures != 0 {
		t.Fatalf("crash court FAILED: truncations=%d prefix_mismatches=%d recover_failures=%d",
			rec.Truncations, rec.PrefixMismatches, rec.RecoverFailures)
	}
	if rec.KillsWithTail == 0 {
		t.Fatal("crash court observed no abandoned tail: the random kills never landed mid-append")
	}
	if rec.VerifyPasses != rec.Iterations {
		t.Fatalf("verify --deep passed %d/%d iterations", rec.VerifyPasses, rec.Iterations)
	}
	if rec.ContinuedCommits != rec.Iterations {
		t.Fatalf("log continued at the recovered root in %d/%d iterations", rec.ContinuedCommits, rec.Iterations)
	}
	t.Logf("crash court: %d iterations, %d with tail (%d torn / %d uncommitted), %d quarantined, 0 truncations/0 prefix mismatches/0 recover failures in %.1fs",
		rec.Iterations, rec.KillsWithTail, rec.TornTails, rec.UncommittedTails, rec.QuarantinedCount, rec.WallSeconds)
}

// runCrashIteration performs one fresh-volume kill/recover cycle and folds its
// outcome into rec.
func runCrashIteration(t *testing.T, bin, base string, i int, rng *rand.Rand, maxWait time.Duration, rec *crashReceipt) crashIteration {
	t.Helper()
	out := crashIteration{Index: i}
	backing, err := os.MkdirTemp(base, "vol")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(backing)

	genesis := crashGenesis(crashRecordsPerTx)
	l, err := log.Create(backing, genesis)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close after create: %v", err)
	}

	// Spawn the real writer and wait until it has committed its warm-up.
	stdout, err := spawnWriter(bin, backing)
	if err != nil {
		t.Fatalf("spawn writer: %v", err)
	}
	// Random small interval, then SIGKILL — no clean shutdown, no destructors.
	wait := time.Duration(0)
	if maxWait > 0 {
		wait = time.Duration(rng.Int63n(int64(maxWait) + 1))
	}
	time.Sleep(wait)
	if err := stdout.kill(); err != nil {
		t.Fatalf("kill writer: %v", err)
	}

	// Recover: read the surviving committed prefix, then reopen read-write,
	// which quarantines the tail (if any).
	dir := log.Dir(backing)
	preScan, err := log.ScanLog(dir)
	if err != nil {
		rec.RecoverFailures++
		t.Errorf("iter %d: scan after kill: %v", i, err)
		return out
	}
	out.Tail = preScan.Tail != nil
	if preScan.Tail != nil {
		out.TailKind = preScan.Tail.Kind.String()
		out.TailAbandoned = preScan.Tail.AbandonedBytes
		if preScan.Tail.Kind == log.TailTornRecord {
			rec.TornTails++
		} else {
			rec.UncommittedTails++
		}
	}
	prefixPre := canonicalPrefix(t, dir, preScan.CommittedRoot)

	rw, err := log.Open(backing, log.ReadWrite)
	if err != nil {
		rec.RecoverFailures++
		t.Errorf("iter %d: reopen: %v", i, err)
		return out
	}
	disp := rw.Recovery()
	out.Quarantined = disp.Quarantined
	out.RecoveredRoot = rw.CommittedRoot()
	out.PrefixBytes = rw.CommittedRoot()

	if rw.Recovery().Quarantined {
		rec.QuarantinedCount++
	}

	// The recovered root is a committed boundary.
	if !containsBoundary(rw.Boundaries(), rw.CommittedRoot()) {
		rec.RecoverFailures++
		t.Errorf("iter %d: recovered root %d is not a committed boundary", i, rw.CommittedRoot())
	}
	out.RootIsBoundary = containsBoundary(rw.Boundaries(), rw.CommittedRoot())

	// The committed prefix is byte-identical to the bytes that survived the kill.
	prefixPost := canonicalPrefix(t, dir, rw.CommittedRoot())
	if rw.CommittedRoot() != preScan.CommittedRoot || !bytes.Equal(prefixPre, prefixPost) {
		rec.PrefixMismatches++
		t.Errorf("iter %d: committed prefix changed by recovery (root %d -> %d, %d -> %d bytes)",
			i, preScan.CommittedRoot, rw.CommittedRoot(), len(prefixPre), len(prefixPost))
	}
	out.PrefixIdentical = bytes.Equal(prefixPre, prefixPost)

	// The abandoned tail is quarantined (moved), and every abandoned byte is
	// retained: a shortfall is a truncation.
	qbytes := quarantinedBytes(t, dir)
	out.QuarantinedBytes = qbytes
	if preScan.Tail != nil {
		if !disp.Quarantined || qbytes < preScan.Tail.AbandonedBytes {
			rec.Truncations++
			t.Errorf("iter %d: abandoned tail of %d byte(s) not fully quarantined (quarantined=%v, %d bytes retained)",
				i, preScan.Tail.AbandonedBytes, disp.Quarantined, qbytes)
		}
	}

	// verify --deep over the recovered volume.
	rep, verr := fsck.Verify(backing, true)
	if verr != nil || !rep.Passed() {
		rec.RecoverFailures++
		t.Errorf("iter %d: verify --deep failed: err=%v passed=%v", i, verr, rep != nil && rep.Passed())
	} else {
		rec.VerifyPasses++
		out.VerifyPassed = true
	}

	// The log continues at the recovered root: one more transaction commits
	// exactly one transaction-length past it.
	rootBefore := rw.CommittedRoot()
	rootAfter, perr := rw.Publish(crashTx(rw.MaxTransactionID()+1, crashRecordsPerTx))
	if perr != nil {
		rec.RecoverFailures++
		t.Errorf("iter %d: publish after recovery: %v", i, perr)
		rw.Close()
		return out
	}
	if rootAfter-rootBefore != crashTxFrame(crashRecordsPerTx) {
		rec.RecoverFailures++
		t.Errorf("iter %d: continuation advanced %d bytes at root %d, want %d", i, rootAfter-rootBefore, rootBefore, crashTxFrame(crashRecordsPerTx))
	} else {
		out.Continued = true
		rec.ContinuedCommits++
	}
	if err := rw.Close(); err != nil {
		t.Errorf("iter %d: close recovered volume: %v", i, err)
	}
	return out
}

// crashWriter is a running helper process whose stdout is watched for READY.
type crashWriter struct {
	cmd    *exec.Cmd
	stdout *bufio.Scanner
	stderr *bytes.Buffer
}

// spawnWriter builds nothing (the binary is prebuilt) but starts the helper and
// blocks until it prints READY, so the court knows a committed transaction exists
// and every later append begins in a fresh trailing segment.
func spawnWriter(bin, backing string) (*crashWriter, error) {
	cmd := exec.Command(bin, "-backing", backing, "-records", strconv.Itoa(crashRecordsPerTx), "-ready-after", "1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	w := &crashWriter{cmd: cmd, stdout: bufio.NewScanner(pipe), stderr: &bytes.Buffer{}}
	cmd.Stderr = w.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ready := make(chan error, 1)
	go func() {
		for w.stdout.Scan() {
			if w.stdout.Text() == "READY" {
				ready <- nil
				return
			}
		}
		ready <- fmt.Errorf("writer exited before READY: %s", w.stderr.String())
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("writer did not become ready within 20s")
	}
	return w, nil
}

// kill sends SIGKILL and reaps the process.
func (w *crashWriter) kill() error {
	if w.cmd.Process == nil {
		return nil
	}
	err := w.cmd.Process.Kill()
	_ = w.cmd.Wait()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// buildCrashWriter compiles cmd/orfscrashtest once per court run.
func buildCrashWriter(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("crash court needs the go toolchain to build the writer: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "orfscrashtest")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/infinityabundance/orfs-go/cmd/orfscrashtest")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build crash writer: %v\n%s", err, out)
	}
	return bin
}

// canonicalPrefix reads the canonical byte stream [0, root) from the surviving
// segment files (segments are named by their start offset).
func canonicalPrefix(t *testing.T, dir string, root uint64) []byte {
	t.Helper()
	ids, err := segment.DiscoverSegmentIDs(dir)
	if err != nil {
		t.Fatalf("discover segments: %v", err)
	}
	out := make([]byte, 0, root)
	for _, id := range ids {
		if id >= root {
			break
		}
		b, err := os.ReadFile(filepath.Join(dir, segment.SegmentName(id)))
		if err != nil {
			t.Fatalf("read segment %d: %v", id, err)
		}
		n := root - id
		if uint64(len(b)) < n {
			n = uint64(len(b))
		}
		out = append(out, b[:n]...)
	}
	return out
}

// quarantinedBytes sums the bytes retained under quarantine/.
func quarantinedBytes(t *testing.T, dir string) uint64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, segment.QuarantineDir))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read quarantine dir: %v", err)
	}
	var total uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat quarantined file: %v", err)
		}
		total += uint64(info.Size())
	}
	return total
}

func containsBoundary(bounds []uint64, root uint64) bool {
	for _, b := range bounds {
		if b == root {
			return true
		}
	}
	return false
}

// sampleIterations bounds the per-iteration detail in the receipt: all
// iterations when few, otherwise an even stride.
func sampleIterations(all []crashIteration, n int) []crashIteration {
	const maxDetail = 200
	if len(all) <= maxDetail {
		return all
	}
	stride := len(all) / maxDetail
	if stride < 1 {
		stride = 1
	}
	out := make([]crashIteration, 0, maxDetail)
	for i := 0; i < len(all); i += stride {
		out = append(out, all[i])
	}
	return out
}

func crashNote(rec crashReceipt) string {
	return fmt.Sprintf(
		"real SIGKILL of the cmd/orfscrashtest writer at a random point mid-append, %d iterations. "+
			"The court waits for the writer's first committed transaction (a READY line) and then sleeps a random "+
			"interval before SIGKILLing, so the crash lands inside an in-progress append. "+
			"The volume is created with a segment size (genesis frame + one %d-record transaction = %d bytes) "+
			"chosen so every committed transaction ends exactly at a segment boundary, so each append begins in a "+
			"fresh trailing segment whose id is the recovered committed root; a kill mid-append therefore leaves the "+
			"abandoned tail in a movable (quarantinable) segment. %d kills left a tail (%d torn, %d uncommitted), "+
			"%d were quarantined (MOVED, never truncated), and %d recovered with no tail (the kill landed after a "+
			"transaction's bytes were complete, e.g. during its fsync barrier). This models a process kill, not "+
			"power loss: bytes in the page cache survive. Truncations, prefix mismatches and recovery failures are "+
			"all zero; every recovered volume passed verify --deep and accepted one further committed transaction "+
			"at the recovered root.",
		rec.Iterations, rec.RecordsPerTx, rec.SegmentSize, rec.KillsWithTail, rec.TornTails, rec.UncommittedTails,
		rec.QuarantinedCount, rec.KillsWithoutTail)
}

func writeCrashEvidence(t *testing.T, rec crashReceipt) {
	t.Helper()
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	dir := filepath.Join("..", "..", "evidence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir evidence: %v", err)
	}
	path := filepath.Join(dir, "crash.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("wrote %s", path)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
