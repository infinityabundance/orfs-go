// Package view is the immutable read view of a committed namespace.
//
// Reads no longer take the writer's lock. A commit publishes a new, completely
// frozen view through an atomic pointer; a reader loads the current view once
// and traverses it with no lock at all. The view is built from persistent
// (path-copying) structures, so publishing costs O(log n) node copies per
// changed key rather than a deep copy of the whole namespace.
//
// This is the Go port of the Rust reference's Phase 19 read-view model
// (`.reference/OrFS/docs/CONCURRENCY.md` §2–§4): readers read a frozen point,
// the writer mutates the live index in place and republishes at the commit
// boundary, and a publication is a cheap pointer swap. The reference stores the
// inode/directory spines in persistent treaps (`src/persistent.rs`, `PMap`) for
// exactly this reason, and §4.1 records that its Phase 27 removed the last
// O(history) copy from the publish path.
//
// A view is immutable once published: its treap nodes are never mutated and its
// inodes are private copies, so any number of goroutines may read one
// concurrently, and a reader never contends with the writer or with another
// reader.
package view

import "github.com/infinityabundance/orfs-go/internal/index"

// splitmix64 is the reference PMap priority finaliser
// (`.reference/OrFS/src/persistent.rs`: FxHash then splitmix64). Using a
// deterministic hash of the key as the treap priority gives a treap whose shape
// depends only on the key set, not on insertion order, so a published view is
// reproducible and independent of the commit history that produced it.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func u64Priority(k uint64) uint64 { return splitmix64(k) }

// dentryPriority hashes (parent, name) with FNV-1a and finalises with splitmix64.
func dentryPriority(k index.DentryKey) uint64 {
	h := uint64(14695981039346656037) // FNV-1a offset basis
	for i := 0; i < 8; i++ {
		h ^= (k.Parent >> (8 * uint(i))) & 0xff
		h *= 1099511628211
	}
	for i := 0; i < len(k.Name); i++ {
		h ^= uint64(k.Name[i])
		h *= 1099511628211
	}
	return splitmix64(h)
}

// pnode is one immutable treap node. Its fields are written once, when the node
// is created, and never mutated; a path copy replaces the nodes on the
// root-to-key path and shares every untouched subtree with the prior version.
type pnode[K any, V any] struct {
	key         K
	val         V
	prio        uint64
	left, right *pnode[K, V]
}

// ptree is a persistent (path-copying) treap keyed by an ordered key. It is a
// value-semantics map: every mutation returns a new tree that shares structure
// with the receiver, leaving the receiver unchanged. All operations are
// expected O(log n).
type ptree[K any, V any] struct {
	root *pnode[K, V]
	less func(a, b K) bool
	prio func(k K) uint64
	size int
}

func newPTree[K any, V any](less func(a, b K) bool, prio func(k K) uint64) *ptree[K, V] {
	return &ptree[K, V]{less: less, prio: prio}
}

// get returns the value bound to key, if any.
func (t *ptree[K, V]) get(key K) (V, bool) {
	var zero V
	n := t.root
	for n != nil {
		switch {
		case t.less(key, n.key):
			n = n.left
		case t.less(n.key, key):
			n = n.right
		default:
			return n.val, true
		}
	}
	return zero, false
}

// contains reports whether key is present.
func (t *ptree[K, V]) contains(key K) bool {
	_, ok := t.get(key)
	return ok
}

// set returns a new tree with key bound to val; the receiver is unchanged.
func (t *ptree[K, V]) set(key K, val V) *ptree[K, V] {
	existed := t.contains(key)
	root := t.setNode(t.root, key, val, t.prio(key))
	size := t.size
	if !existed {
		size++
	}
	return &ptree[K, V]{root: root, less: t.less, prio: t.prio, size: size}
}

func (t *ptree[K, V]) setNode(n *pnode[K, V], key K, val V, prio uint64) *pnode[K, V] {
	if n == nil {
		return &pnode[K, V]{key: key, val: val, prio: prio}
	}
	switch {
	case t.less(key, n.key):
		left := t.setNode(n.left, key, val, prio)
		nn := &pnode[K, V]{key: n.key, val: n.val, prio: n.prio, left: left, right: n.right}
		if left.prio > nn.prio {
			return rotateRight(nn)
		}
		return nn
	case t.less(n.key, key):
		right := t.setNode(n.right, key, val, prio)
		nn := &pnode[K, V]{key: n.key, val: n.val, prio: n.prio, left: n.left, right: right}
		if right.prio > nn.prio {
			return rotateLeft(nn)
		}
		return nn
	default:
		return &pnode[K, V]{key: n.key, val: val, prio: n.prio, left: n.left, right: n.right}
	}
}

// del returns a new tree without key; a missing key returns the receiver
// unchanged (structure shared, no copy).
func (t *ptree[K, V]) del(key K) *ptree[K, V] {
	if !t.contains(key) {
		return t
	}
	root := t.delNode(t.root, key)
	return &ptree[K, V]{root: root, less: t.less, prio: t.prio, size: t.size - 1}
}

func (t *ptree[K, V]) delNode(n *pnode[K, V], key K) *pnode[K, V] {
	if n == nil {
		return nil
	}
	switch {
	case t.less(key, n.key):
		return &pnode[K, V]{key: n.key, val: n.val, prio: n.prio, left: t.delNode(n.left, key), right: n.right}
	case t.less(n.key, key):
		return &pnode[K, V]{key: n.key, val: n.val, prio: n.prio, left: n.left, right: t.delNode(n.right, key)}
	default:
		return merge(n.left, n.right)
	}
}

// merge joins two treaps whose key ranges are disjoint and ordered (every key in
// a sorts before every key in b).
func merge[K any, V any](a, b *pnode[K, V]) *pnode[K, V] {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if a.prio > b.prio {
		return &pnode[K, V]{key: a.key, val: a.val, prio: a.prio, left: a.left, right: merge(a.right, b)}
	}
	return &pnode[K, V]{key: b.key, val: b.val, prio: b.prio, left: merge(a, b.left), right: b.right}
}

func rotateRight[K any, V any](n *pnode[K, V]) *pnode[K, V] {
	l := n.left
	return &pnode[K, V]{
		key: l.key, val: l.val, prio: l.prio,
		left:  l.left,
		right: &pnode[K, V]{key: n.key, val: n.val, prio: n.prio, left: l.right, right: n.right},
	}
}

func rotateLeft[K any, V any](n *pnode[K, V]) *pnode[K, V] {
	r := n.right
	return &pnode[K, V]{
		key: r.key, val: r.val, prio: r.prio,
		left:  &pnode[K, V]{key: n.key, val: n.val, prio: n.prio, left: n.left, right: r.left},
		right: r.right,
	}
}

// walk visits every entry in ascending key order. fn returns false to stop.
func (t *ptree[K, V]) walk(fn func(k K, v V) bool) {
	var stack []*pnode[K, V]
	n := t.root
	for n != nil || len(stack) > 0 {
		for n != nil {
			stack = append(stack, n)
			n = n.left
		}
		n = stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !fn(n.key, n.val) {
			return
		}
		n = n.right
	}
}

// View is a frozen, immutable snapshot of the committed namespace. Every read
// entry point in internal/namespace is answered from one of these, loaded once
// per request, so a single request is answered from one consistent committed
// state.
type View struct {
	root   uint64
	nextID uint64
	inodes *ptree[uint64, *index.Inode]
	dentry *ptree[index.DentryKey, uint64]
}

func emptyView() *View {
	return &View{
		inodes: newPTree[uint64, *index.Inode](
			func(a, b uint64) bool { return a < b }, u64Priority),
		dentry: newPTree[index.DentryKey, uint64](
			func(a, b index.DentryKey) bool {
				if a.Parent != b.Parent {
					return a.Parent < b.Parent
				}
				return a.Name < b.Name
			}, dentryPriority),
	}
}

// FromIndex builds a complete view by copying every inode and dentry out of a
// mutable index. It is O(n) and is used once, at open, when there is no writer.
func FromIndex(idx *index.Index) *View {
	v := emptyView()
	for id, in := range idx.Inodes {
		v.inodes = v.inodes.set(id, cloneInode(in))
	}
	for k, child := range idx.Dentry {
		v.dentry = v.dentry.set(k, child)
	}
	v.root = idx.Root
	v.nextID = idx.NextInodeID
	return v
}

// WithChanges publishes a new view reflecting a commit. The receiver is
// unchanged; every key not named in the change set is shared with it, so the
// cost is O(log n) node copies per changed key plus a copy of each changed
// inode. inodes is the set of inode ids the committed transaction touched and
// dentry the set of directory-entry keys it added, removed, or moved; both are
// applied as one batch, so a reader that loads the result sees all of the
// transaction or none of it — never a partially applied state.
func (v *View) WithChanges(src *index.Index, inodes map[uint64]struct{}, dentry map[index.DentryKey]struct{}) *View {
	n := &View{root: src.Root, nextID: src.NextInodeID, inodes: v.inodes, dentry: v.dentry}
	for id := range inodes {
		if in, ok := src.Inodes[id]; ok {
			n.inodes = n.inodes.set(id, cloneInode(in))
		} else {
			n.inodes = n.inodes.del(id)
		}
	}
	for k := range dentry {
		if child, ok := src.Dentry[k]; ok {
			n.dentry = n.dentry.set(k, child)
		} else {
			n.dentry = n.dentry.del(k)
		}
	}
	return n
}

// Root is the committed log root this view reflects.
func (v *View) Root() uint64 { return v.root }

// NextInodeID is the next inode id the committed state would allocate.
func (v *View) NextInodeID() uint64 { return v.nextID }

// InodeCount is the number of inodes in the view.
func (v *View) InodeCount() int { return v.inodes.size }

// DentryCount is the number of directory entries in the view.
func (v *View) DentryCount() int { return v.dentry.size }

// Inode returns an inode by id.
func (v *View) Inode(id uint64) (*index.Inode, bool) { return v.inodes.get(id) }

// Lookup resolves a directory entry to its child inode.
func (v *View) Lookup(parent uint64, name string) (*index.Inode, bool) {
	child, ok := v.dentry.get(index.DentryKey{Parent: parent, Name: name})
	if !ok {
		return nil, false
	}
	return v.inodes.get(child)
}

// Children lists a directory's members sorted by name (byte order).
func (v *View) Children(parent uint64) []index.DirEntry {
	var out []index.DirEntry
	v.dentry.walk(func(k index.DentryKey, child uint64) bool {
		if k.Parent == parent {
			out = append(out, index.DirEntry{Name: k.Name, Child: child})
		}
		return true
	})
	return out
}

// ParentOf finds an inode's parent directory. A directory has exactly one; a
// hard-linked file has several, and the lexicographically first entry wins (the
// live writer's map-scan returned an unspecified one).
func (v *View) ParentOf(child uint64) (uint64, bool) {
	var parent uint64
	found := false
	v.dentry.walk(func(k index.DentryKey, c uint64) bool {
		if c == child {
			parent = k.Parent
			found = true
			return false
		}
		return true
	})
	return parent, found
}

// NameCount counts the directory entries naming an inode (its link count as the
// committed namespace records it). It is read-only and lock-free.
func (v *View) NameCount(child uint64) int {
	n := 0
	v.dentry.walk(func(_ index.DentryKey, c uint64) bool {
		if c == child {
			n++
		}
		return true
	})
	return n
}

// cloneInode returns a private copy of an inode whose maps and slices the view
// owns, so a later mutation of the writer's inode cannot alter a published view.
func cloneInode(in *index.Inode) *index.Inode {
	if in == nil {
		return nil
	}
	c := *in
	if in.Xattrs != nil {
		c.Xattrs = make(map[string][]byte, len(in.Xattrs))
		for k, val := range in.Xattrs {
			c.Xattrs[k] = append([]byte(nil), val...)
		}
	}
	if in.Extents != nil {
		c.Extents = in.Extents.Clone()
	}
	if in.Target != nil {
		c.Target = append([]byte(nil), in.Target...)
	}
	return &c
}
