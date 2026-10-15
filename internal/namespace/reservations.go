package namespace

import (
	"sync"

	"github.com/infinityabundance/orfs-go/internal/index"
)

// reservations is the writer's commitment table. A staged mutation reserves the
// namespace keys its record set was built from — the directory-entry names it
// reads or writes and the inode ids whose mutable fields it reads or writes —
// before it releases the write lock across the durability barrier. Any other
// staged mutation that wants one of those keys blocks until the first has
// committed and applied, so a record set built under a reservation is built
// against state no in-flight transaction can change. Non-conflicting mutations
// (the common case: distinct keys) proceed fully in parallel, which is what lets
// the log's group committer batch their barriers.
//
// A name reservation is always exclusive: presence/absence of a (parent, name)
// entry is the conflict. A node reservation is shared for a read of an inode's
// mutable state (a permission or kind check, an append) and exclusive for a
// change to an inode's link count, which is carried as an absolute value in the
// record and so is order-sensitive.
type reservations struct {
	mu   sync.Mutex
	cond *sync.Cond

	names map[index.DentryKey]*resEntry
	nodes map[uint64]*resEntry

	// nextID is the inode-id high-water mark. Allocation is monotonic and never
	// reuses an id, so two staged creates can never emit the same inode id even
	// if they commit out of allocation order.
	nextID uint64
}

type resEntry struct {
	shared int
	excl   int
}

func newReservations(nextID uint64) *reservations {
	r := &reservations{
		names:  make(map[index.DentryKey]*resEntry),
		nodes:  make(map[uint64]*resEntry),
		nextID: nextID,
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// reservation is one staged transaction's read/write key set.
type reservation struct {
	names       []index.DentryKey
	sharedNodes []uint64
	exclNodes   []uint64
}

func rname(parent uint64, name string) index.DentryKey {
	return index.DentryKey{Parent: parent, Name: name}
}

func (r *reservation) addName(parent uint64, name string) {
	r.names = append(r.names, rname(parent, name))
}

func (r *reservation) addShared(id uint64) { r.sharedNodes = append(r.sharedNodes, id) }
func (r *reservation) addExcl(id uint64)   { r.exclNodes = append(r.exclNodes, id) }

// same reports whether two reservations name the same key sets in the same
// order (the staging/rebuild pair derives them from the same code path, so an
// unchanged committed state yields identical sets).
func (r reservation) same(o reservation) bool {
	if len(r.names) != len(o.names) || len(r.sharedNodes) != len(o.sharedNodes) || len(r.exclNodes) != len(o.exclNodes) {
		return false
	}
	for i := range r.names {
		if r.names[i] != o.names[i] {
			return false
		}
	}
	for i := range r.sharedNodes {
		if r.sharedNodes[i] != o.sharedNodes[i] {
			return false
		}
	}
	for i := range r.exclNodes {
		if r.exclNodes[i] != o.exclNodes[i] {
			return false
		}
	}
	return true
}

// free reports whether every key in r is available for the requested mode.
func (rs *reservations) free(r reservation) bool {
	for _, k := range r.names {
		if e := rs.names[k]; e != nil && (e.excl > 0 || e.shared > 0) {
			return false
		}
	}
	for _, id := range r.sharedNodes {
		if e := rs.nodes[id]; e != nil && e.excl > 0 {
			return false
		}
	}
	for _, id := range r.exclNodes {
		if e := rs.nodes[id]; e != nil && (e.excl > 0 || e.shared > 0) {
			return false
		}
	}
	return true
}

func (rs *reservations) add(r reservation) {
	for _, k := range r.names {
		e := rs.names[k]
		if e == nil {
			e = &resEntry{}
			rs.names[k] = e
		}
		e.excl++
	}
	for _, id := range r.sharedNodes {
		e := rs.nodes[id]
		if e == nil {
			e = &resEntry{}
			rs.nodes[id] = e
		}
		e.shared++
	}
	for _, id := range r.exclNodes {
		e := rs.nodes[id]
		if e == nil {
			e = &resEntry{}
			rs.nodes[id] = e
		}
		e.excl++
	}
}

func (rs *reservations) sub(r reservation) {
	for _, k := range r.names {
		if e := rs.names[k]; e != nil {
			e.excl--
			if e.excl <= 0 && e.shared <= 0 {
				delete(rs.names, k)
			}
		}
	}
	for _, id := range r.sharedNodes {
		if e := rs.nodes[id]; e != nil {
			e.shared--
			if e.excl <= 0 && e.shared <= 0 {
				delete(rs.nodes, id)
			}
		}
	}
	for _, id := range r.exclNodes {
		if e := rs.nodes[id]; e != nil {
			e.excl--
			if e.excl <= 0 && e.shared <= 0 {
				delete(rs.nodes, id)
			}
		}
	}
}

// acquire blocks until every key in r can be taken together, then takes them.
// A waiting transaction is woken by any release and re-tests, so acquisition is
// all-or-nothing and no reader/writer prioritisation is promised.
func (rs *reservations) acquire(r reservation) {
	rs.mu.Lock()
	for !rs.free(r) {
		rs.cond.Wait()
	}
	rs.add(r)
	rs.mu.Unlock()
}

// release returns the keys taken by acquire.
func (rs *reservations) release(r reservation) {
	rs.mu.Lock()
	rs.sub(r)
	rs.cond.Broadcast()
	rs.mu.Unlock()
}

// allocID reserves the next inode id. Ids are never reused.
func (rs *reservations) allocID() uint64 {
	rs.mu.Lock()
	id := rs.nextID
	rs.nextID++
	rs.mu.Unlock()
	return id
}
