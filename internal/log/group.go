// Group commit (opt-in). Concurrent Publish calls are coalesced into a batch:
// the batch's frames are appended, then ONE segment.Sync() and ONE watermark
// write cover the whole batch, and only then is every waiter acknowledged.
//
// The barrier is the only thing that moves: the bytes appended for a batch are
// exactly the bytes a sequence of per-publish commits would have written, in the
// same order, with the same sequence numbers, transaction ids, and committed
// roots. Each transaction still owns its own root (the end of its TX_COMMIT) and
// its own contiguous sequence range. A batch is bounded by a maximum count and a
// maximum wait.
//
// Failure is all-or-nothing: if the barrier fails, every transaction in the
// batch is failed and the previous committed root is left intact (nothing is
// acknowledged, no watermark is advanced).
package log

import (
	"os"
	"sync"
	"time"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// Group-commit defaults: a batch of up to 128 transactions, flushed after at
// most 2 ms.
const (
	DefaultGroupMaxBatch = 128
	DefaultGroupMaxWait  = 2 * time.Millisecond
)

// Option configures a Log at Create/Open.
type Option func(*Log)

// WithGroupCommit enables group commit: Publish calls are coalesced into batches
// of at most maxBatch transactions, and a batch is flushed after at most
// maxWait. A non-positive bound selects the default (DefaultGroupMaxBatch /
// DefaultGroupMaxWait). Group commit is off unless requested here or via
// ORFS_GROUP_COMMIT=1.
func WithGroupCommit(maxBatch int, maxWait time.Duration) Option {
	return func(l *Log) { l.groupCfg = &groupConfig{maxBatch: maxBatch, maxWait: maxWait} }
}

// groupConfig is the resolved group-commit configuration.
type groupConfig struct {
	maxBatch int
	maxWait  time.Duration
}

// applyOptions resolves options and the environment, then starts the committer.
func applyOptions(l *Log, opts []Option) {
	for _, o := range opts {
		o(l)
	}
	if l.groupCfg == nil && os.Getenv("ORFS_GROUP_COMMIT") == "1" {
		l.groupCfg = &groupConfig{}
	}
	if l.groupCfg == nil || l.mode != ReadWrite {
		return
	}
	if l.groupCfg.maxBatch <= 0 {
		l.groupCfg.maxBatch = DefaultGroupMaxBatch
	}
	if l.groupCfg.maxWait <= 0 {
		l.groupCfg.maxWait = DefaultGroupMaxWait
	}
	l.startGroup()
}

// publishResult is the outcome delivered to one waiter.
type publishResult struct {
	root uint64
	err  error
}

// publishRequest is one transaction awaiting a batch barrier.
type publishRequest struct {
	data []*record.Record
	done chan publishResult
}

// groupCommitter coalesces concurrent publishes behind one barrier per batch.
type groupCommitter struct {
	log      *Log
	maxBatch int
	maxWait  time.Duration

	reqs chan *publishRequest
	stop chan struct{}
	wg   sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// startGroup installs and launches the committer. Callers hold no locks.
func (l *Log) startGroup() {
	g := &groupCommitter{
		log:      l,
		maxBatch: l.groupCfg.maxBatch,
		maxWait:  l.groupCfg.maxWait,
		reqs:     make(chan *publishRequest, l.groupCfg.maxBatch),
		stop:     make(chan struct{}),
	}
	l.group = g
	g.wg.Add(1)
	go g.run()
}

// publish submits one transaction and blocks until the batch barrier that covers
// its bytes acknowledges it.
func (g *groupCommitter) publish(data []*record.Record) (uint64, error) {
	req := &publishRequest{data: data, done: make(chan publishResult, 1)}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return 0, orfserr.ReadOnly("log is closing")
	}
	// A buffered send cannot block for long: the loop keeps draining until
	// close() flips closed and closes stop. Holding mu across the send is what
	// makes "no request can be enqueued after close" true.
	g.reqs <- req
	g.mu.Unlock()

	res := <-req.done
	return res.root, res.err
}

// run is the committer loop: receive a first request, gather a batch bounded by
// maxBatch/maxWait, commit it under one barrier, and repeat.
func (g *groupCommitter) run() {
	defer g.wg.Done()
	for {
		select {
		case <-g.stop:
			g.drain()
			return
		case first := <-g.reqs:
			g.commitBatch(g.collect(first))
		}
	}
}

// collect adds requests to the batch until it reaches maxBatch or maxWait
// elapses (measured from the first request).
func (g *groupCommitter) collect(first *publishRequest) []*publishRequest {
	batch := []*publishRequest{first}
	if g.maxBatch <= 1 {
		return batch
	}
	timer := time.NewTimer(g.maxWait)
	defer timer.Stop()
	for len(batch) < g.maxBatch {
		select {
		case r := <-g.reqs:
			batch = append(batch, r)
		case <-timer.C:
			return batch
		case <-g.stop:
			return batch
		}
	}
	return batch
}

// drain fails any request enqueued but not yet committed (only reachable when a
// request races Close, which is a misuse but must not hang or lie).
func (g *groupCommitter) drain() {
	for {
		select {
		case r := <-g.reqs:
			r.done <- publishResult{err: orfserr.ReadOnly("log is closing")}
		default:
			return
		}
	}
}

// close stops the committer and waits for an in-flight batch to finish. It must
// not be called concurrently with Publish.
func (g *groupCommitter) close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	g.mu.Unlock()
	close(g.stop)
	g.wg.Wait()
}

// stagedTx is one transaction in a batch whose frames are already appended.
type stagedTx struct {
	req     *publishRequest
	root    uint64
	txID    uint64
	nextSeq uint64
}

// commitBatch performs the three phases of a group commit:
//
//  1. plan every transaction (frames + whole-transaction capacity pre-flight)
//     before writing a single byte, so a refusal writes nothing;
//  2. append every frame, then take exactly one barrier;
//  3. advance the committed prefix for every transaction in order and write one
//     watermark, then acknowledge every waiter.
//
// A barrier failure fails every waiter and leaves the previous committed root
// intact. No waiter is acknowledged before the barrier that covers its bytes.
func (g *groupCommitter) commitBatch(batch []*publishRequest) {
	l := g.log

	// Phase 1: plan.
	pos := l.appendPos()
	seq := l.nextSeq
	txID := l.maxTxID
	planned := make([]*preparedTx, len(batch))
	for i, req := range batch {
		p, err := l.prepareTx(req.data, txID+1, seq, pos)
		if err != nil {
			failBatch(batch, err)
			return
		}
		planned[i] = p
		pos += p.grandTotal
		txID++
		seq = p.commitSeq + 1
	}

	// Phase 2: append, then one barrier.
	staged := make([]stagedTx, 0, len(batch))
	for i, p := range planned {
		for _, f := range p.frames {
			if err := l.writeFrame(f); err != nil {
				failBatch(batch, err)
				return
			}
		}
		staged = append(staged, stagedTx{
			req:     batch[i],
			root:    l.appendPos(),
			txID:    p.txID,
			nextSeq: p.commitSeq + 1,
		})
	}
	if err := l.syncWriter(); err != nil {
		failBatch(batch, err)
		return
	}

	// Phase 3: publish the prefix, then acknowledge.
	for _, s := range staged {
		l.committedRoot = s.root
		l.boundaries = append(l.boundaries, s.root)
	}
	if n := len(staged); n > 0 {
		l.nextSeq = staged[n-1].nextSeq
		l.committedRecords = staged[n-1].nextSeq
		l.maxTxID = staged[n-1].txID
	}
	if err := l.persistWatermark(); err != nil {
		failBatch(batch, err)
		return
	}
	for _, s := range staged {
		s.req.done <- publishResult{root: s.root}
	}
}

func failBatch(batch []*publishRequest, err error) {
	for _, r := range batch {
		r.done <- publishResult{err: err}
	}
}
