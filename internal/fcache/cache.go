// Package fcache implements a self-contained suite of volatile cache eviction
// policies and the court that measures their scan resistance.
//
// Three policies are provided behind one tiny interface, Cache:
//
//   - NewS3FIFO: a scan-resistant admission cache,
//   - NewFIFO: the insertion-ordered FIFO baseline, and
//   - NewLRU: a faithful least-recently-used baseline.
//
// # What S3-FIFO is
//
// S3-FIFO is the eviction policy of Yang et al., "FIFO Queues are All You Need
// for Cache Eviction" (SOSP 2023). It keeps two FIFO queues and a bounded,
// keys-only "ghost" history:
//
//   - a small probationary queue (about 10% of the capacity),
//   - a main queue (the remaining ~90%), and
//   - a ghost queue remembering keys recently evicted from small.
//
// Each resident key carries a frequency bit. A key that misses is inserted into
// small with its frequency bit clear. If it is referenced again while resident,
// the bit is set; when small must evict, a key whose bit is set is promoted to
// main, and a key still clear is discarded to ghost. A later insertion of a
// ghosted key is re-admitted straight to main -- a second chance. A key evicted
// from main with its bit clear is gone; only keys referenced more than once are
// protected.
//
// # Why admission matters for scan resistance
//
// An insertion-ordered FIFO is destroyed by a single one-pass scan over more
// distinct keys than the cache holds: every scan insert evicts the queue front,
// so after one pass the entire hot working set is gone. Admission fixes this
// without any recency bookkeeping. A scan key is touched exactly once, so its
// frequency bit stays clear: it cycles through small (and ghost) and is evicted
// immediately, and it never displaces a hot key that has already been promoted
// into main. The cost of a scan becomes the cold keys alone instead of the whole
// cache. This package's court measures exactly that.
//
// # The honest framing: a hit-rate win is not a latency win
//
// The reference court this package reproduces measured, over one identical
// scan-heavy key trace (capacity 32, a 20-key hot set, a 512-key scan), a
// scan-phase hit rate of 95.2% for S3-FIFO admission, 88.0% for the
// insertion-ordered FIFO, and 95.2% for LRU. Two honest readings of those
// numbers:
//
//   - Admission beats FIFO by a wide margin, but it only matches LRU on this
//     interleaved trace: the hot set is re-referenced every scan step, which
//     protects it under recency too. The win over FIFO is the scan-resistance
//     result; there is no win over LRU here.
//   - A hit rate is not a latency. Serving a hit removes a cache-miss cost, but
//     this package measures hit rates only and makes no latency claim. Every
//     operation is O(1) amortised, which is a complexity statement, not a
//     benchmark.
//
// The keys here stand in for volatile, re-derivable state (parsed structures,
// read plans); evicting one forgets a cached representation, never authoritative
// data. Every policy is deterministic: no clock, no randomness, no hashing of
// user input.
package fcache

// Stats is the cumulative hit/miss accounting of a cache since construction.
type Stats struct {
	// Hits is the number of Get calls served from a resident key.
	Hits uint64
	// Misses is the number of Get calls for a non-resident key.
	Misses uint64
}

// Total returns Hits+Misses.
func (s Stats) Total() uint64 { return s.Hits + s.Misses }

// Rate returns the hit rate Hits/(Hits+Misses), or 0 when there were no lookups.
func (s Stats) Rate() float64 {
	if s.Total() == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Total())
}

// Cache is the minimal volatile-cache surface the court replays. The policies
// are not safe for concurrent use; the court drives each from one goroutine.
type Cache interface {
	// Get reports whether key is resident. It records a hit when it is and a
	// miss when it is not.
	Get(key uint64) bool
	// Put admits key. Putting a resident key is a no-op for the FIFO baseline,
	// and refreshes recency (LRU) or frequency (S3-FIFO) for the others.
	Put(key uint64)
	// Len reports the number of resident keys.
	Len() int
	// Stats reports the cumulative hit/miss accounting.
	Stats() Stats
}

// Replay drives a key trace through a cache under the Get/Put contract: every
// key is looked up, and a miss admits it. It returns the cache's cumulative
// stats.
func Replay(c Cache, trace []uint64) Stats {
	for _, k := range trace {
		if !c.Get(k) {
			c.Put(k)
		}
	}
	return c.Stats()
}

// fifo is an insertion-ordered FIFO cache: a hit does not reorder the queue, so
// the oldest inserted key is evicted first. It is the pre-admission baseline.
type fifo struct {
	capacity int
	buf      []uint64 // ring buffer of resident keys, head is the oldest
	resident map[uint64]struct{}
	head     int
	size     int
	stats    Stats
}

// NewFIFO returns an insertion-ordered FIFO cache of the given capacity. A
// capacity of zero or less yields a cache that never stores anything.
func NewFIFO(capacity int) Cache {
	c := &fifo{capacity: capacity, resident: make(map[uint64]struct{})}
	if capacity > 0 {
		c.buf = make([]uint64, capacity)
	}
	return c
}

// Get reports residency without reordering the queue.
func (c *fifo) Get(key uint64) bool {
	if _, ok := c.resident[key]; ok {
		c.stats.Hits++
		return true
	}
	c.stats.Misses++
	return false
}

// Put appends key, evicting the oldest resident key when full.
func (c *fifo) Put(key uint64) {
	if c.capacity <= 0 {
		return
	}
	if _, ok := c.resident[key]; ok {
		return
	}
	if c.size == c.capacity {
		delete(c.resident, c.buf[c.head])
		c.buf[c.head] = key
		c.head = (c.head + 1) % c.capacity
	} else {
		c.buf[(c.head+c.size)%c.capacity] = key
		c.size++
	}
	c.resident[key] = struct{}{}
}

// Len reports the number of resident keys.
func (c *fifo) Len() int { return c.size }

// Stats reports the cumulative hit/miss accounting.
func (c *fifo) Stats() Stats { return c.stats }

// lruNode is one node of the LRU's intrusive doubly linked list.
type lruNode struct {
	key        uint64
	prev, next *lruNode
}

// lru is a faithful least-recently-used cache. A hit promotes the key to
// most-recently-used; a miss evicts the least-recently-used key.
type lru struct {
	capacity int
	index    map[uint64]*lruNode
	front    *lruNode // most recently used
	back     *lruNode // least recently used
	stats    Stats
}

// NewLRU returns a least-recently-used cache of the given capacity. A capacity
// of zero or less yields a cache that never stores anything.
func NewLRU(capacity int) Cache {
	return &lru{capacity: capacity, index: make(map[uint64]*lruNode)}
}

// Get reports residency, promoting a hit to most-recently-used.
func (c *lru) Get(key uint64) bool {
	if n, ok := c.index[key]; ok {
		c.moveToFront(n)
		c.stats.Hits++
		return true
	}
	c.stats.Misses++
	return false
}

// Put admits key at most-recently-used, evicting the least-recently-used key
// when the capacity would be exceeded.
func (c *lru) Put(key uint64) {
	if c.capacity <= 0 {
		return
	}
	if n, ok := c.index[key]; ok {
		c.moveToFront(n)
		return
	}
	n := &lruNode{key: key}
	c.index[key] = n
	c.pushFront(n)
	if len(c.index) > c.capacity {
		c.evictBack()
	}
}

// Len reports the number of resident keys.
func (c *lru) Len() int { return len(c.index) }

// Stats reports the cumulative hit/miss accounting.
func (c *lru) Stats() Stats { return c.stats }

func (c *lru) pushFront(n *lruNode) {
	n.prev = nil
	n.next = c.front
	if c.front != nil {
		c.front.prev = n
	}
	c.front = n
	if c.back == nil {
		c.back = n
	}
}

func (c *lru) moveToFront(n *lruNode) {
	if c.front == n {
		return
	}
	if n.prev != nil {
		n.prev.next = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	}
	if c.back == n {
		c.back = n.prev
	}
	c.pushFront(n)
}

func (c *lru) evictBack() {
	n := c.back
	if n == nil {
		return
	}
	if n.prev != nil {
		n.prev.next = nil
	}
	c.back = n.prev
	if c.front == n {
		c.front = nil
	}
	delete(c.index, n.key)
}
