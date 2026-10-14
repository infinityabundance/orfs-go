package view

import (
	"math/rand"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/record"
)

func u64Tree() *ptree[uint64, int] {
	return newPTree[uint64, int](func(a, b uint64) bool { return a < b }, u64Priority)
}

// TestPTreeSetGetDeleteAndPersistence exercises the persistent treap's map
// semantics and, crucially, that a snapshot taken before a batch of mutations is
// unchanged by them (structural sharing, not in-place mutation).
func TestPTreeSetGetDeleteAndPersistence(t *testing.T) {
	tr := u64Tree()
	rng := rand.New(rand.NewSource(1))
	seen := map[uint64]int{}
	var keys []uint64
	for len(keys) < 2000 {
		k := uint64(rng.Intn(4000))
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = int(k) * 3
		keys = append(keys, k)
	}

	cur := tr
	for _, k := range keys {
		cur = cur.set(k, seen[k])
	}
	if cur.size != len(seen) {
		t.Fatalf("size = %d, want %d", cur.size, len(seen))
	}
	for k, val := range seen {
		if got, ok := cur.get(k); !ok || got != val {
			t.Fatalf("get(%d) = %d,%v want %d,true", k, got, ok, val)
		}
	}
	// In-order walk yields ascending keys.
	var prev uint64
	first := true
	n := 0
	cur.walk(func(k uint64, _ int) bool {
		if !first && k <= prev {
			t.Fatalf("walk out of order: %d after %d", k, prev)
		}
		prev, first = k, false
		n++
		return true
	})
	if n != len(seen) {
		t.Fatalf("walk visited %d, want %d", n, len(seen))
	}

	// The snapshot is immutable: further mutations do not touch it.
	snap := cur
	deleted := map[uint64]bool{}
	mut := cur
	for i, k := range keys {
		if i%2 == 0 {
			mut = mut.del(k)
			deleted[k] = true
		} else {
			mut = mut.set(k, -1)
		}
	}
	for k, val := range seen {
		if got, ok := snap.get(k); !ok || got != val {
			t.Fatalf("snapshot key %d changed: %d,%v", k, got, ok)
		}
	}
	for k := range seen {
		if _, ok := mut.get(k); deleted[k] && ok {
			t.Fatalf("deleted key %d still present in new tree", k)
		}
	}
	// Deleting a key the tree does not contain returns the same tree unchanged.
	if mut.del(1<<62) != mut {
		t.Fatal("deleting a missing key copied the tree")
	}
}

func testIndex() *index.Index {
	idx := index.New()
	idx.Root = 1234
	idx.NextInodeID = 10
	idx.Inodes[1] = &index.Inode{ID: 1, Kind: record.KindDir, Mode: 0o755, Nlink: 2, Extents: index.NewExtentMap(), Xattrs: map[string][]byte{}}
	idx.Inodes[2] = &index.Inode{ID: 2, Kind: record.KindFile, Mode: 0o644, Nlink: 1, Size: 4, Extents: index.NewExtentMap(), Xattrs: map[string][]byte{"user.k": {1, 2}}}
	idx.Inodes[3] = &index.Inode{ID: 3, Kind: record.KindFile, Mode: 0o600, Nlink: 1, Extents: index.NewExtentMap(), Xattrs: map[string][]byte{}}
	idx.Dentry[index.DentryKey{Parent: 1, Name: "z"}] = 3
	idx.Dentry[index.DentryKey{Parent: 1, Name: "a"}] = 2
	return idx
}

func TestViewFromIndex(t *testing.T) {
	idx := testIndex()
	v := FromIndex(idx)
	if v.Root() != 1234 || v.NextInodeID() != 10 {
		t.Fatalf("scalars: root=%d next=%d", v.Root(), v.NextInodeID())
	}
	if v.InodeCount() != 3 || v.DentryCount() != 2 {
		t.Fatalf("counts: inodes=%d dentry=%d", v.InodeCount(), v.DentryCount())
	}
	if in, ok := v.Lookup(1, "a"); !ok || in.ID != 2 {
		t.Fatalf("lookup a -> %v,%v", in, ok)
	}
	children := v.Children(1)
	if len(children) != 2 || children[0].Name != "a" || children[1].Name != "z" {
		t.Fatalf("children not sorted by name: %+v", children)
	}
	if p, ok := v.ParentOf(2); !ok || p != 1 {
		t.Fatalf("parent of 2 = %d,%v", p, ok)
	}
}

// TestViewWithChangesIsAtomicBatch proves a commit's whole change set lands in
// one publication: both dentry keys moved by an exchange are visible together in
// the new view, and the prior view is unchanged.
func TestViewWithChangesIsAtomicBatch(t *testing.T) {
	idx := testIndex()
	v0 := FromIndex(idx)

	// Simulate an exchange of a<->z and an inode mode bump, as one commit does.
	idx.Dentry[index.DentryKey{Parent: 1, Name: "a"}] = 3
	idx.Dentry[index.DentryKey{Parent: 1, Name: "z"}] = 2
	idx.Inodes[2] = &index.Inode{ID: 2, Kind: record.KindFile, Mode: 0o600, Nlink: 1, Extents: index.NewExtentMap(), Xattrs: map[string][]byte{}}

	v1 := v0.WithChanges(idx,
		map[uint64]struct{}{2: {}},
		map[index.DentryKey]struct{}{
			{Parent: 1, Name: "a"}: {},
			{Parent: 1, Name: "z"}: {},
		},
	)

	// New view: both moved, together.
	if in, _ := v1.Lookup(1, "a"); in == nil || in.ID != 3 {
		t.Fatalf("new view a -> %v", in)
	}
	if in, _ := v1.Lookup(1, "z"); in == nil || in.ID != 2 {
		t.Fatalf("new view z -> %v", in)
	}
	if in, _ := v1.Inode(2); in.Mode != 0o600 {
		t.Fatalf("new view inode 2 mode = %o", in.Mode)
	}
	// Old view: unchanged on both keys and the inode.
	if in, _ := v0.Lookup(1, "a"); in == nil || in.ID != 2 {
		t.Fatalf("old view a -> %v", in)
	}
	if in, _ := v0.Lookup(1, "z"); in == nil || in.ID != 3 {
		t.Fatalf("old view z -> %v", in)
	}
	if in, _ := v0.Inode(2); in.Mode != 0o644 {
		t.Fatalf("old view inode 2 mode = %o", in.Mode)
	}
}

// TestViewCopiesAreIsolated proves a published view owns its inodes: mutating the
// writer's index after publication must not change the view.
func TestViewCopiesAreIsolated(t *testing.T) {
	idx := testIndex()
	v := FromIndex(idx)

	idx.Inodes[2].Mode = 0o000
	idx.Inodes[2].Size = 999
	idx.Inodes[2].Xattrs["user.k"][0] = 0xff
	idx.Dentry[index.DentryKey{Parent: 1, Name: "a"}] = 99

	if in, _ := v.Inode(2); in.Mode != 0o644 || in.Size != 4 || in.Xattrs["user.k"][0] != 1 {
		t.Fatalf("view inode mutated by writer: %+v", in)
	}
	if in, _ := v.Lookup(1, "a"); in == nil || in.ID != 2 {
		t.Fatalf("view dentry mutated by writer: %v", in)
	}
}
