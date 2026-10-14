// Package fcache implements a self-contained suite of volatile cache eviction
// policies and the court that measures their scan resistance.
//
// Four policies are provided behind one tiny interface, Cache:
//
//   - NewS3FIFO: a scan-resistant admission cache,
//   - NewFIFO: the insertion-ordered FIFO baseline,
//   - NewLRU: a faithful least-recently-used baseline, and
//   - NewMap: the unbounded map baseline (never evicts).
//
// A Cache maps a comparable key to an opaque value, so a caller can bound real
// resident state (a parsed reader, a decoded body) rather than only count keys.
// The policies are deterministic and each entry is O(1) amortised: no clock, no
// randomness, no hashing of user input.
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
// data.
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

// Cache is the minimal volatile-cache surface. Get records a reference and
// reports residency together with the resident value; Put admits (or refreshes)
// a key with its value. The policies are not safe for concurrent use; the caller
// serialises them.
type Cache[K comparable, V any] interface {
	// Get reports whether key is resident and, if so, returns its value. It
	// records a hit when it is and a miss when it is not.
	Get(key K) (V, bool)
	// Put admits key with value. Putting a resident key is a no-op for the FIFO
	// baseline, and refreshes recency (LRU) or frequency (S3-FIFO) for the others.
	Put(key K, value V)
	// Len reports the number of resident keys.
	Len() int
	// Stats reports the cumulative hit/miss accounting.
	Stats() Stats
}

// Replay drives a key trace through a cache under the Get/Put contract: every
// key is looked up, and a miss admits it with the caller's placeholder value
// (the trace only exercises residency). It returns the cache's cumulative stats.
func Replay[K comparable, V any](c Cache[K, V], trace []K, zero V) Stats {
	for _, k := range trace {
		if _, ok := c.Get(k); !ok {
			c.Put(k, zero)
		}
	}
	return c.Stats()
}

// mapCache is an unbounded map: nothing is ever evicted, so it has a 100% hit
// rate on any trace and unbounded memory. It is the pre-admission baseline the
// admission policies are measured against (bounding residency is the point).
type mapCache[K comparable, V any] struct {
	entries map[K]V
	stats   Stats
}

// NewMap returns an unbounded map cache.
func NewMap[K comparable, V any]() Cache[K, V] {
	return &mapCache[K, V]{entries: make(map[K]V)}
}

// Get reports residency.
func (c *mapCache[K, V]) Get(key K) (V, bool) {
	if v, ok := c.entries[key]; ok {
		c.stats.Hits++
		return v, true
	}
	c.stats.Misses++
	var zero V
	return zero, false
}

// Put admits or replaces key.
func (c *mapCache[K, V]) Put(key K, value V) { c.entries[key] = value }

// Len reports the number of resident keys.
func (c *mapCache[K, V]) Len() int { return len(c.entries) }

// Stats reports the cumulative hit/miss accounting.
func (c *mapCache[K, V]) Stats() Stats { return c.stats }

// fifo is an insertion-ordered FIFO cache: a hit does not reorder the queue, so
// the oldest inserted key is evicted first. It is the pre-admission baseline.
type fifo[K comparable, V any] struct {
	capacity int
	order    []K // ring buffer of resident keys, head is the oldest
	values   map[K]V
	head     int
	size     int
	stats    Stats
}

// NewFIFO returns an insertion-ordered FIFO cache of the given capacity. A
// capacity of zero or less yields a cache that never stores anything.
func NewFIFO[K comparable, V any](capacity int) Cache[K, V] {
	c := &fifo[K, V]{capacity: capacity, values: make(map[K]V)}
	if capacity > 0 {
		c.order = make([]K, capacity)
	}
	return c
}

// Get reports residency without reordering the queue.
func (c *fifo[K, V]) Get(key K) (V, bool) {
	if v, ok := c.values[key]; ok {
		c.stats.Hits++
		return v, true
	}
	c.stats.Misses++
	var zero V
	return zero, false
}

// Put appends key, evicting the oldest resident key when full.
func (c *fifo[K, V]) Put(key K, value V) {
	if c.capacity <= 0 {
		return
	}
	if _, ok := c.values[key]; ok {
		return
	}
	if c.size == c.capacity {
		delete(c.values, c.order[c.head])
		c.order[c.head] = key
		c.head = (c.head + 1) % c.capacity
	} else {
		c.order[(c.head+c.size)%c.capacity] = key
		c.size++
	}
	c.values[key] = value
}

// Len reports the number of resident keys.
func (c *fifo[K, V]) Len() int { return len(c.values) }

// Stats reports the cumulative hit/miss accounting.
func (c *fifo[K, V]) Stats() Stats { return c.stats }

// lruNode is one node of the LRU's intrusive doubly linked list.
type lruNode[K comparable, V any] struct {
	key        K
	value      V
	prev, next *lruNode[K, V]
}

// lru is a faithful least-recently-used cache. A hit promotes the key to
// most-recently-used; a miss evicts the least-recently-used key.
type lru[K comparable, V any] struct {
	capacity int
	index    map[K]*lruNode[K, V]
	front    *lruNode[K, V] // most recently used
	back     *lruNode[K, V] // least recently used
	stats    Stats
}

// NewLRU returns a least-recently-used cache of the given capacity. A capacity
// of zero or less yields a cache that never stores anything.
func NewLRU[K comparable, V any](capacity int) Cache[K, V] {
	return &lru[K, V]{capacity: capacity, index: make(map[K]*lruNode[K, V])}
}

// Get reports residency, promoting a hit to most-recently-used.
func (c *lru[K, V]) Get(key K) (V, bool) {
	if n, ok := c.index[key]; ok {
		c.moveToFront(n)
		c.stats.Hits++
		return n.value, true
	}
	c.stats.Misses++
	var zero V
	return zero, false
}

// Put admits key at most-recently-used, evicting the least-recently-used key
// when the capacity would be exceeded.
func (c *lru[K, V]) Put(key K, value V) {
	if c.capacity <= 0 {
		return
	}
	if n, ok := c.index[key]; ok {
		n.value = value
		c.moveToFront(n)
		return
	}
	n := &lruNode[K, V]{key: key, value: value}
	c.index[key] = n
	c.pushFront(n)
	if len(c.index) > c.capacity {
		c.evictBack()
	}
}

// Len reports the number of resident keys.
func (c *lru[K, V]) Len() int { return len(c.index) }

// Stats reports the cumulative hit/miss accounting.
func (c *lru[K, V]) Stats() Stats { return c.stats }

func (c *lru[K, V]) pushFront(n *lruNode[K, V]) {
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

func (c *lru[K, V]) moveToFront(n *lruNode[K, V]) {
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

func (c *lru[K, V]) evictBack() {
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
