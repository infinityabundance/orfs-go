package manifest

import (
	"sync"
	"sync/atomic"

	ekbloom "github.com/infinityabundance/ekbloom-go"
	"github.com/infinityabundance/orfs-go/internal/fcache"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// AuthorityRead reads a file's logical bytes from the authoritative log,
// bypassing any manifested path. It is the oracle a manifested body must agree
// with.
type AuthorityRead func(inode, offset, size uint64) ([]byte, bool)

type key struct {
	inode   uint64
	version uint64
	root    uint64
}

type liveReader struct {
	reader     *ekbloom.Reader
	logicalLen uint64
}

// DefaultAdmissionCapacity is the default bound (number of resident manifested
// bodies) of the S3-FIFO admission cache. Bodies are small (a decoded EKBLOOM
// projection), so a few hundred is cheap; the point of a bound is that a scan
// of cold inodes can never grow the accelerator without limit.
const DefaultAdmissionCapacity = 128

// Policy selects the admission policy for the resident manifested-body cache.
type Policy int

const (
	// PolicyS3FIFO is the scan-resistant S3-FIFO admission policy (the default):
	// a first touch enters a small probationary queue, only a re-referenced body
	// is promoted to main, and a one-pass scan of cold inodes is evicted without
	// displacing the hot working set.
	PolicyS3FIFO Policy = iota
	// PolicyLRU is a recency-only baseline (evicts the least-recently-used body).
	PolicyLRU
	// PolicyUnbounded is the legacy unbounded map: nothing is ever evicted. It is
	// retained as an honest baseline and for measurement, not as a default.
	PolicyUnbounded
)

// Option configures a Cache.
type Option func(*Cache)

// WithAdmissionCapacity bounds the resident manifested-body cache to n bodies
// under the selected bounded policy. n <= 0 selects the unbounded legacy map
// (nothing is ever evicted), whatever policy is otherwise set.
func WithAdmissionCapacity(n int) Option {
	return func(c *Cache) { c.capacity = n }
}

// WithPolicy selects the admission policy for the resident manifested-body
// cache. It is ignored when the capacity is unbounded (n <= 0).
func WithPolicy(p Policy) Option {
	return func(c *Cache) { c.policy = p }
}

// Cache is the live read-planner seam: it certifies a manifested body once for
// an exact (inode, version, root) and then serves reads from it, falling back to
// authority whenever there is no certified body.
//
// Authority establishes truth; the cache only accelerates observation of it. A
// miss or a failed certification is never an error — it is a fallback. Misses
// are negative-cached so the common (unmanifested) case costs no filesystem I/O.
//
// The set of resident bodies is an admission-controlled cache keyed by the
// manifested identity. Admission decides only whether a body is served from
// memory or re-opened and re-certified from the store; it never changes the
// bytes returned, and a miss re-certifies from authority exactly as an empty
// cache would.
type Cache struct {
	vol       *volume.Volume
	authority AuthorityRead

	// policy/capacity configure the resident-body cache; bodies is the built
	// policy. bodies is not concurrency-safe, so every access holds mu.
	policy   Policy
	capacity int

	mu     sync.Mutex
	bodies fcache.Cache[key, *liveReader]
	absent map[key]struct{}

	servedManifested atomic.Uint64
	missed           atomic.Uint64
	refused          atomic.Uint64
}

// NewCache builds a cache over a volume. By default an S3-FIFO admission cache
// of DefaultAdmissionCapacity resident bodies bounds the accelerator; pass
// WithAdmissionCapacity and/or WithPolicy to change that.
func NewCache(vol *volume.Volume, opts ...Option) *Cache {
	c := &Cache{
		vol:      vol,
		policy:   PolicyS3FIFO,
		capacity: DefaultAdmissionCapacity,
		absent:   make(map[key]struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	c.buildBodies()
	return c
}

// buildBodies instantiates the resident-body cache from the configured policy
// and capacity. A non-positive capacity means unbounded (the legacy map).
func (c *Cache) buildBodies() {
	switch {
	case c.capacity <= 0 || c.policy == PolicyUnbounded:
		c.bodies = fcache.NewMap[key, *liveReader]()
	case c.policy == PolicyLRU:
		c.bodies = fcache.NewLRU[key, *liveReader](c.capacity)
	default:
		c.bodies = fcache.NewS3FIFO[key, *liveReader](c.capacity)
	}
}

// SetAuthority installs the authoritative oracle. When set, a manifested body is
// trusted only if its logical content digest equals what authority produces now.
func (c *Cache) SetAuthority(fn AuthorityRead) { c.authority = fn }

// Publish builds, certifies, and primes a manifested body for content under an
// exact identity.
func (c *Cache) Publish(inode, version, root uint64, content []byte) (*Manifest, error) {
	m, err := Publish(c.vol, inode, version, root, content)
	if err != nil {
		return nil, err
	}
	k := key{inode, version, root}
	if lr, ok := c.open(m); ok {
		c.mu.Lock()
		c.bodies.Put(k, lr)
		delete(c.absent, k)
		c.mu.Unlock()
	}
	return m, nil
}

// ManifestedRead serves a read from a certified manifested body. The second
// result is false when the caller must fall back to authority.
func (c *Cache) ManifestedRead(inode, version, root, offset, size uint64) ([]byte, bool) {
	k := key{inode, version, root}

	c.mu.Lock()
	_, absent := c.absent[k]
	var lr *liveReader
	var ok bool
	if !absent {
		lr, ok = c.bodies.Get(k)
	}
	c.mu.Unlock()

	if !ok {
		if absent {
			c.missed.Add(1)
			return nil, false
		}
		m, _, found := loadCertified(c.vol, inode, version, root)
		if !found {
			c.mu.Lock()
			c.absent[k] = struct{}{}
			c.mu.Unlock()
			c.missed.Add(1)
			return nil, false
		}
		opened, ok2 := c.open(m)
		if !ok2 {
			c.refused.Add(1)
			return nil, false
		}
		// Self-consistency is not enough: bind the body to the bytes authority
		// holds now, when an oracle is installed.
		if c.authority != nil && m.LogicalBlake3 != "" {
			if logical, ok3 := c.authority(inode, 0, m.LogicalLen); ok3 {
				if LogicalDigest(logical) != m.LogicalBlake3 {
					c.refused.Add(1)
					return nil, false
				}
			}
		}
		lr = opened
		// Admission decides only whether this body is retained in the accelerator;
		// the bytes served are identical either way, and a later miss re-certifies.
		c.mu.Lock()
		c.bodies.Put(k, lr)
		c.mu.Unlock()
	}

	if offset >= lr.logicalLen {
		c.servedManifested.Add(1)
		return []byte{}, true
	}
	want := size
	if offset+want > lr.logicalLen {
		want = lr.logicalLen - offset
	}
	out, err := lr.reader.ReadAt(offset, want)
	if err != nil {
		c.refused.Add(1)
		return nil, false
	}
	c.servedManifested.Add(1)
	return out, true
}

// open verifies a manifest's body self-digest and opens an EKBLOOM reader.
func (c *Cache) open(m *Manifest) (*liveReader, bool) {
	_, body, ok := loadCertified(c.vol, m.Inode, m.Version, m.Root)
	if !ok {
		return nil, false
	}
	r, err := ekbloom.Open(body)
	if err != nil {
		return nil, false
	}
	if r.LogicalLen() != m.LogicalLen {
		return nil, false
	}
	return &liveReader{reader: r, logicalLen: m.LogicalLen}, true
}

// Stats reports the served counters: manifested reads, misses (no manifest for
// the identity), and refusals (a manifest existed but failed certification).
func (c *Cache) Stats() (manifested, missed, refused uint64) {
	return c.servedManifested.Load(), c.missed.Load(), c.refused.Load()
}

// BodyStats reports the resident manifested-body cache's cumulative hit/miss
// accounting and current residency. A hit is a read answered from an already-open
// certified body (no store I/O, no re-certification); a miss is a read that had
// to load and certify — or that found no manifest for the identity (a genuine
// miss: no body was resident). This is the hit rate of the admission policy, not
// a latency.
func (c *Cache) BodyStats() (hits, misses, resident uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.bodies.Stats()
	return s.Hits, s.Misses, uint64(c.bodies.Len())
}
