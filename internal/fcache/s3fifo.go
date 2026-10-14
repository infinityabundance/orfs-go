package fcache

// The S3-FIFO constants. The probationary segment is one tenth of the capacity
// (at least one slot); the frequency counter saturates at three references and a
// single re-reference (a set frequency bit) is enough to promote from small to
// main; the ghost queue holds as many keys as the cache holds entries.
const (
	s3SmallRatio = 10
	s3MaxFreq    = 3
	s3PromoteMin = 1
)

// deque is a minimal growable double-ended queue backed by a slice with a moving
// head. PopFront bounds memory by compacting the backing array when its dead
// prefix grows large, so every operation is O(1) amortised.
type deque[T any] struct {
	buf  []T
	head int
}

// pushBack appends v.
func (d *deque[T]) pushBack(v T) { d.buf = append(d.buf, v) }

// popFront removes and returns the oldest element.
func (d *deque[T]) popFront() (T, bool) {
	var zero T
	if d.head >= len(d.buf) {
		return zero, false
	}
	v := d.buf[d.head]
	d.buf[d.head] = zero
	d.head++
	switch {
	case d.head == len(d.buf):
		d.buf = d.buf[:0]
		d.head = 0
	case d.head >= 256 && d.head*2 >= len(d.buf):
		n := copy(d.buf, d.buf[d.head:])
		d.buf = d.buf[:n]
		d.head = 0
	}
	return v, true
}

// len reports the number of queued elements.
func (d *deque[T]) len() int { return len(d.buf) - d.head }

// ghostEntry is one ghost-queue record. The tag distinguishes a live record from
// a stale duplicate left behind when the key was re-admitted.
type ghostEntry[K comparable] struct {
	key K
	tag uint64
}

// s3fifo is a scan-resistant admission cache implementing the S3-FIFO idea (see
// the package comment). It is correct and deterministic: no clock, no
// randomness, and no iteration over map order on any decision path.
type s3fifo[K comparable, V any] struct {
	capacity   int
	smallQuota int
	ghostQuota int

	// entries maps a resident key to its saturating frequency counter; values
	// holds the resident payload.
	entries map[K]uint8
	values  map[K]V
	small   deque[K]
	main    deque[K]

	// ghost is a bounded, keys-only history of keys evicted from small.
	ghost      deque[ghostEntry[K]]
	ghostIndex map[K]uint64
	ghostNext  uint64

	stats Stats

	// Admission counters, exposed by Counters for the court's receipt.
	promotions uint64
	evictions  uint64
	ghostHits  uint64
}

// NewS3FIFO returns an S3-FIFO admission cache of the given capacity. A capacity
// of zero or less yields a cache that never stores anything.
func NewS3FIFO[K comparable, V any](capacity int) Cache[K, V] {
	c := &s3fifo[K, V]{
		capacity:   capacity,
		entries:    make(map[K]uint8),
		values:     make(map[K]V),
		ghostIndex: make(map[K]uint64),
	}
	if capacity > 0 {
		c.smallQuota = capacity / s3SmallRatio
		if c.smallQuota < 1 {
			c.smallQuota = 1
		}
		if c.smallQuota > capacity {
			c.smallQuota = capacity
		}
		c.ghostQuota = capacity
	}
	return c
}

// Get reports residency, bumping the frequency counter of a resident key. A miss
// for a key the ghost queue still remembers is recorded as a ghost hit.
func (c *s3fifo[K, V]) Get(key K) (V, bool) {
	if f, ok := c.entries[key]; ok {
		if f < s3MaxFreq {
			c.entries[key] = f + 1
		}
		c.stats.Hits++
		return c.values[key], true
	}
	if _, ok := c.ghostIndex[key]; ok {
		c.ghostHits++
	}
	c.stats.Misses++
	var zero V
	return zero, false
}

// Put admits key. A key the ghost queue remembers is re-admitted straight to
// main (a second chance); any other new key enters the small probationary queue.
// Re-putting a resident key refreshes its frequency in place.
func (c *s3fifo[K, V]) Put(key K, value V) {
	if c.capacity <= 0 {
		return
	}
	if f, ok := c.entries[key]; ok {
		if f < s3MaxFreq {
			c.entries[key] = f + 1
		}
		c.values[key] = value
		return
	}
	c.values[key] = value
	if _, ok := c.ghostIndex[key]; ok {
		delete(c.ghostIndex, key)
		c.entries[key] = 0
		c.main.pushBack(key)
		c.promotions++
	} else {
		c.entries[key] = 0
		c.small.pushBack(key)
	}
	c.evict()
}

// evict restores the invariant len(entries) <= capacity. While the probationary
// queue exceeds its quota it is drained first: a key whose frequency bit is set
// is promoted to main, and a key still clear is discarded to ghost. Only when
// small is within quota is main drained, giving a referenced key one second
// chance before it is evicted.
func (c *s3fifo[K, V]) evict() {
	for len(c.entries) > c.capacity {
		if c.small.len() > c.smallQuota {
			key, ok := c.small.popFront()
			if !ok {
				break
			}
			if c.entries[key] >= s3PromoteMin {
				c.entries[key] = 0
				c.main.pushBack(key)
				c.promotions++
			} else {
				delete(c.entries, key)
				delete(c.values, key)
				c.noteGhost(key)
				c.evictions++
			}
			continue
		}
		key, ok := c.main.popFront()
		if !ok {
			break
		}
		if c.entries[key] > 0 {
			c.entries[key]--
			c.main.pushBack(key)
		} else {
			delete(c.entries, key)
			delete(c.values, key)
			c.evictions++
		}
	}
}

// noteGhost records key in the bounded ghost queue, dropping any stale duplicate
// it passes. The queue never exceeds ghostQuota entries.
func (c *s3fifo[K, V]) noteGhost(key K) {
	if c.ghostQuota <= 0 {
		return
	}
	tag := c.ghostNext
	c.ghostNext++
	c.ghostIndex[key] = tag
	c.ghost.pushBack(ghostEntry[K]{key: key, tag: tag})
	for c.ghost.len() > c.ghostQuota {
		e, ok := c.ghost.popFront()
		if !ok {
			break
		}
		if t, ok := c.ghostIndex[e.key]; ok && t == e.tag {
			delete(c.ghostIndex, e.key)
		}
	}
}

// Len reports the number of resident keys.
func (c *s3fifo[K, V]) Len() int { return len(c.entries) }

// Stats reports the cumulative hit/miss accounting.
func (c *s3fifo[K, V]) Stats() Stats { return c.stats }

// AdmissionCounters reports the S3-FIFO admission-policy counters beyond the
// plain hit/miss accounting.
type AdmissionCounters struct {
	// Hits and Misses mirror Stats.
	Hits, Misses uint64
	// Promotions counts entries moved from small to main, including ghost
	// re-admissions.
	Promotions uint64
	// Evictions counts entries discarded altogether (small-to-ghost demotions
	// plus evictions from main).
	Evictions uint64
	// GhostHits counts misses for a key the ghost queue still remembered.
	GhostHits uint64
}

// Counters exposes the admission counters of an S3-FIFO cache. It returns false
// for any other policy.
func Counters[K comparable, V any](c Cache[K, V]) (AdmissionCounters, bool) {
	s, ok := c.(*s3fifo[K, V])
	if !ok {
		return AdmissionCounters{}, false
	}
	return AdmissionCounters{
		Hits:       s.stats.Hits,
		Misses:     s.stats.Misses,
		Promotions: s.promotions,
		Evictions:  s.evictions,
		GhostHits:  s.ghostHits,
	}, true
}
