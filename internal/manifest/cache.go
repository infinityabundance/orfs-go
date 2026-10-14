package manifest

import (
	"sync"
	"sync/atomic"

	ekbloom "github.com/infinityabundance/ekbloom-go"
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

// Cache is the live read-planner seam: it certifies a manifested body once for
// an exact (inode, version, root) and then serves reads from it, falling back to
// authority whenever there is no certified body.
//
// Authority establishes truth; the cache only accelerates observation of it. A
// miss or a failed certification is never an error — it is a fallback. Misses
// are negative-cached so the common (unmanifested) case costs no filesystem I/O.
type Cache struct {
	vol       *volume.Volume
	authority AuthorityRead

	mu     sync.Mutex
	live   map[key]*liveReader
	absent map[key]struct{}

	servedManifested atomic.Uint64
	missed           atomic.Uint64
	refused          atomic.Uint64
}

// NewCache builds a cache over a volume.
func NewCache(vol *volume.Volume) *Cache {
	return &Cache{vol: vol, live: make(map[key]*liveReader), absent: make(map[key]struct{})}
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
	if lr, ok := c.open(m); ok {
		c.mu.Lock()
		c.live[key{inode, version, root}] = lr
		delete(c.absent, key{inode, version, root})
		c.mu.Unlock()
	}
	return m, nil
}

// ManifestedRead serves a read from a certified manifested body. The second
// result is false when the caller must fall back to authority.
func (c *Cache) ManifestedRead(inode, version, root, offset, size uint64) ([]byte, bool) {
	k := key{inode, version, root}

	c.mu.Lock()
	lr, ok := c.live[k]
	absent := false
	if !ok {
		_, absent = c.absent[k]
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
		c.mu.Lock()
		c.live[k] = lr
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
