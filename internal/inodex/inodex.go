// Package inodex implements a sealed, dense-ordinal inode spine for the
// immutable historical role.
//
// The live writer keeps inodes in a path-copying structure (the index package),
// because it must support cheap mutation. A *historical* view is immutable, so
// it can instead be a flat, densely packed slice indexed by inode id: the id is
// the ordinal. That makes lookup O(1) and, crucially, makes the spine far
// smaller than a map of pointer-rich inode structs.
//
// A spine is immutable once sealed and is safe for concurrent reads, which is
// what the removal of the global read lock (concurrent immutable read views)
// needs.
package inodex

import (
	"sort"
	"sync/atomic"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// Entry is one packed inode slot (16 bytes). The zero `kind` (0) with a zero
// generation marks an absent slot; because kind 0 is a valid value (file), an
// explicit presence byte is carried in the high bit of the kind byte.
type Entry struct {
	kind      uint8 // low bits: inode kind; high bit: present
	_         [3]byte
	version   uint32
	latestOff uint64
}

const presentBit = 0x80

// EntryBytes is the packed size of one slot.
const EntryBytes = 16

// Spine is a sealed dense-ordinal inode store.
type Spine struct {
	slots  []Entry
	maxID  uint64 // high-water inode id; the dense ordinal bound
	sealed atomic.Bool
	size   uint64 // populated slots
}

// New returns an empty spine.
func New() *Spine { return &Spine{} }

// Put records an inode slot. It must be called before Seal; after Seal it is a
// no-op, so a sealed spine can never be mutated.
func (s *Spine) Put(id uint64, kind uint8, version uint32, latestOffset uint64) {
	if s.sealed.Load() {
		return
	}
	if id == 0 {
		return
	}
	if id > uint64(len(s.slots)) {
		// Grow geometrically so building a spine is amortised O(1) per Put.
		newLen := id
		if doubled := uint64(len(s.slots)) * 2; doubled > newLen {
			newLen = doubled
		}
		grown := make([]Entry, newLen)
		copy(grown, s.slots)
		s.slots = grown
	}
	if id > s.maxID {
		s.maxID = id
	}
	idx := id - 1
	if s.slots[idx].kind&presentBit == 0 {
		s.size++
	}
	s.slots[idx] = Entry{kind: kind | presentBit, version: version, latestOff: latestOffset}
}

// Seal freezes the spine: no further Put takes effect.
func (s *Spine) Seal() { s.sealed.Store(true) }

// Sealed reports whether the spine is frozen.
func (s *Spine) Sealed() bool { return s.sealed.Load() }

// Get returns the slot for an inode id in O(1).
func (s *Spine) Get(id uint64) (kind uint8, version uint32, latestOffset uint64, ok bool) {
	if id == 0 || id > uint64(len(s.slots)) {
		return 0, 0, 0, false
	}
	e := s.slots[id-1]
	if e.kind&presentBit == 0 {
		return 0, 0, 0, false
	}
	return e.kind &^ presentBit, e.version, e.latestOff, true
}

// Len is the number of populated slots.
func (s *Spine) Len() uint64 { return s.size }

// Bytes is the dense packed size of the spine to its high-water id, which is
// the honest "history role" footprint: one 16-byte slot per ordinal inode id.
func (s *Spine) Bytes() uint64 { return s.maxID * EntryBytes }

// IDs returns the sorted inode ids present in the spine.
func (s *Spine) IDs() []uint64 {
	out := make([]uint64, 0, s.size)
	for i := range s.slots {
		if s.slots[i].kind&presentBit != 0 {
			out = append(out, uint64(i)+1)
		}
	}
	return out
}

// FromIndex builds and seals a spine from a replayed index.
func FromIndex(idx *index.Index) *Spine {
	s := New()
	for id, in := range idx.Inodes {
		s.Put(id, in.Kind, uint32(in.Version), in.LatestRecordOffset)
	}
	s.Seal()
	return s
}

// FromIDs builds and seals a spine from a sorted id list (used by tests and by
// the historical-root directory).
func FromIDs(ids []uint64) *Spine {
	s := New()
	sorted := append([]uint64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, id := range sorted {
		s.Put(id, record.KindFile, 1, id)
	}
	s.Seal()
	return s
}
