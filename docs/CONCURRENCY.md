# OrFS-GO Concurrency Model (Phase 13a, extended: lock-free read views; staged writes)

**Status:** implemented. Reads take no lock; the writer publishes an immutable
read view at the commit boundary and (see §9) no longer holds the write lock
across the log's durability barrier. This document is normative for how OrFS-GO
serves concurrent reads and writes and how committed state reaches them. It is
the Go port
of the Rust reference's Phase 19 model
(`.reference/OrFS/docs/CONCURRENCY.md` §1–§4, §6–§7), with one deliberate
difference: publication is a bare `atomic.Pointer` store, so obtaining the
current view is lock-free in the absolute sense, not merely a short `RwLock`
hold.

Source: `internal/view/view.go` (the immutable view and its persistent treap),
`internal/volume/volume.go` (`commit` → `readView`), `internal/namespace`
(the lock-free read entry points), `internal/index` (`Journal`).

---

## 1. The problem this removes

The original design owned the namespace as a single live index guarded by one
`sync.RWMutex`. Every read took `RLock`; every mutation took `Lock` for its whole
check-and-publish. Readers ran in parallel with each other, but a writer's write
lock excluded all of them, and the read lock was held **across the physical read**
(`ReadAuthoritative` → `readUnchecked` → `CanonicalReader.ReadAt`). At 64 readers
plus 8 writers the write lock was the serialisation point.

This was measured, not assumed. The Go-runtime GC-scaling court
(`docs/RUNTIME.md`) profiled the daemon under 64 concurrent 4 KiB random readers
plus 8 writers and found the `internal/namespace` RWMutex at the top of both the
mutex and block profiles: **~84–89% of blocked time was `sync.(*RWMutex).RLock`,
reached from the FUSE read handler**, and the windowed
`/sync/mutex/wait/total:seconds` was 88–160 goroutine-seconds per ~6 s window.

---

## 2. The read-view publication model

Writers and readers are split:

* **Writers serialise only where they conflict.** A mutation first validates and
  builds its record set against the committed read view, reserving the namespace
  keys it read (the `(parent, name)` directory entries and the inode ids whose
  link counts it emits), *releases every lock*, publishes as one authoritative
  transaction, and releases the reservation once the transaction has been applied.
  Two mutations that touch disjoint keys run fully in parallel, so their
  durability barriers batch in the log's group committer (§9). The committed index
  is mutated only by the log's commit hook, in commit-root order.
* **Readers never take a lock.** They read through an immutable, frozen
  `*view.View` loaded from an `atomic.Pointer`:

  ```go
  // internal/volume/volume.go
  readView atomic.Pointer[view.View]
  func (v *Volume) View() *view.View { return v.readView.Load() }
  ```

A read does `v := ns.vol.View()` once, then answers the whole request from `v`
with no lock at all. Because the load and the traversal take no lock, concurrent
readers never contend with each other or with the writer. A reader may hold a view
for as long as it likes: the view it names is never mutated.

A `view.View` carries everything a read needs from one frozen point:

| Field | Purpose |
|-------|---------|
| `root` | the committed log root this view reflects (`Root()`) |
| `nextID` | the next inode id the committed state would allocate |
| `inodes` | a persistent treap `id → *index.Inode` (a private copy) |
| `dentry` | a persistent treap `{parent, name} → child` |

`Read` uses the view's own `root` as the manifested-identity key, so the inode and
the root are guaranteed to be from the same commit — and the read never touches
the writer's live `Log` state (which the old `CommittedRoot()` call did).

---

## 3. When a new view is published (the commit boundary)

A new complete view is built **only** at the authoritative commit boundary. The
log's commit hook is invoked once per committing transaction, in commit-root
order, on the single committing goroutine, after the transaction's bytes are
durable and before its waiters are released:

```text
Log.Publish(data)                          // append + commit (batched)
→ hook(CommitEvent{Root, BaseOffset, Records})
   → index.Apply(rec, offset) for each record, in order   // extend the live index
   → readView.Store(prev.WithChanges(index, journal))
```

Because the hook advances the index by applying exactly the committed records at
their canonical offsets, in increasing root order, the index stays
replay-equivalent no matter which caller's `Publish` finished first, and **no
rollback path is ever needed**: the committed prefix only grows. A `Publish` that
returns has already been observed by the hook.

The replay is instrumented with an `index.Journal` that records the inode ids and
dentry keys the commit touched. `WithChanges` folds that change set into a new
view as **one batch** — every changed inode and dentry key lands together — and
the new view is stored atomically. Consequently:

* A reader observes either the **whole old** committed state or the **whole new**
  one — never an intermediate, partially applied index. The mutable index is
  private to the writer; the published view is an immutable structure of private
  inode copies.
* The published view is never mutated, and the inode copies it owns are never
  shared with the live index, so a later commit cannot alter a view a reader is
  traversing.
* Publication is a single atomic store; it holds no lock and happens after all
  commit I/O.

The derived-index equality invariant is unaffected: `Apply` is still the only
index mutation, so the live index still equals a from-scratch rebuild, and a view
is just a frozen snapshot of it.

---

## 4. View persistence strategy and its cost

The reference explicitly warns against deep-cloning the whole namespace on every
commit ("that just moves cost to writes"), and stores the index in persistent
path-copying collections (`src/persistent.rs`, `PMap`). OrFS-GO does the same:

* `inodes` and `dentry` are **persistent path-copying treaps**
  (`internal/view/view.go`). A commit copies only the `O(log n)` nodes on the
  root-to-changed-key path; every untouched subtree is shared with the previous
  view. The treap priority is a deterministic hash of the key (FxHash-style FNV
  plus the reference's splitmix64 finaliser), so a view's shape depends only on
  its key set, not on the commit history that produced it.
* Each **changed** inode is copied once into the new view (its struct, xattr map,
  target, and extent slice); an unchanged inode is shared by pointer.

So the per-commit cost is `O(log n)` node copies per changed key plus one inode
copy per changed inode — **independent of the total retained namespace size**, and
never a full-namespace copy. At open, `view.FromIndex` copies the whole index once
(there is no writer then).

The Rust reference's `index_layout` competition
(`.reference/OrFS/docs/INDEX.md` §2–§4) found that a path-copying treap wins for
the live, publication-sensitive spine because replacing it regresses mutation
(its page tree's insert+remove is 2.66× the treap's at 1 M inodes). The
immutable historical role is served by a sealed dense-ordinal spine — OrFS-GO's
`internal/inodex` (phase 18). The read view needs **full** inodes (mode, uid, gid,
size, extents, target, xattrs) to answer reads, whereas the sealed spine carries
only 16 bytes per id (kind, version, latest-offset); it therefore cannot back the
read view and is not forced into this role.

---

## 5. Both the live engine and the view answer reads identically

Every read entry point in `internal/namespace` — `Getattr`, `Lookup`,
`Readdir`/`ReaddirFull`, `Read`, `ReadAuthoritative`, `ReadUnchecked`,
`ReadAll`, `Readlink`, `Listxattr`, `Getxattr`, `ParentOf` — now loads the current
view once and answers from it. The byte assembly (`readUnchecked` → the extent
plan → `CanonicalReader.ReadAt`) is the *same* code the writer used, so reads are
byte-identical by construction; only the source of the inode changed.

The writer's own reads (the check-then-publish inside a mutation) load the same
committed view, and validate against it while holding the reservation for the
keys they read, so a mutation's checks see a committed state no conflicting
in-flight transaction can change (§9).

---

## 6. Invariants and proof obligations

| Invariant | Discharged by |
|-----------|---------------|
| A reader never observes a torn/partial transaction; every read returns a whole committed state | `TestConcurrentReaderSeesWholeTransaction` (`internal/namespace/view_test.go`): 6 readers × a writer exchanging two dentry keys 400× — the two inodes have distinct modes, so a half-applied transaction would show equal modes within one `ReaddirFull` (answered from a single view). 0 tears. |
| A published view is immutable: a later commit cannot change an older view | `TestPublishedViewIsImmutable` (old view still maps `a→A`/`b→B` after an exchange while the new view maps `a→B`/`b→A`); `TestViewCopiesAreIsolated` (mutating the writer's index does not change the view). |
| Reads are race-free against a concurrent writer | `TestConcurrentReadViews` and `TestConcurrentReadsDoNotBlockOnWriter`, run under `-race` (the `container-verify` "race (concurrent immutable reads)" stage runs `go test -race ./internal/...`). |
| Concurrent read bytes equal the single-threaded path | `readUnchecked` is the single byte-assembly implementation; all equivalence/model/snapshot courts stay byte-identical. |
| The writer makes progress under reader load | `TestConcurrentReadsDoNotBlockOnWriter` runs the full read surface against a mutating writer for 300 iterations. |
| A whole change set lands atomically | `TestViewWithChangesIsAtomicBatch` (`internal/view/view_test.go`): both dentry keys moved by an exchange are visible together in the new view, and the prior view is unchanged. |
| Two writers validating the same name against a stale view cannot both commit | `TestConcurrentCreateSameName` (`internal/namespace/staging_test.go`): 48 goroutines create one name; exactly one wins and 47 get `EEXIST`, and the committed namespace holds exactly one entry. |
| An absolute link count emitted concurrently stays exact | `TestConcurrentUnlinkLinkCount`: 16 concurrent unlinks of 16 names of one inode leave `Nlink == 0` and no dentry. |
| A contended rename has exactly one winner | `TestConcurrentRenameContended`: two renames of one source name to different targets, 8 rounds — one wins, the other is `ENOENT`, the source name is gone. |
| An open, unlinked inode stays readable | `TestConcurrentUnlinkWithOpen`: reads of the inode keep returning the committed bytes across the unlink. |
| The live index is a pure function of the committed prefix under concurrency | Every test above ends with `assertReplayEquivalent` (`index.Replay` of the committed log equals `Volume.Index()`), and `TestConcurrentMixedWorkloadReplayEquivalent` does it after a mixed create/write/xattr/link/unlink/rename storm. |
| The lock is not held across the barrier (the batch grows) | `TestGroupCommitBatchesThroughNamespace`: 32 goroutines × 24 creates with group commit on; `batch_sum == 768`, `batch_avg > 1`, and the index is replay-equivalent. |

---

## 7. What this does *not* claim

* **Reads are not entirely lock-free end-to-end.** Obtaining the current view and
  traversing it take no lock, but a read still performs physical I/O through the
  `CanonicalReader`, whose segment/extent cache and the manifest cache each take a
  short-lived plain `sync.Mutex`. Those residual locks are the next measured
  bottleneck (`docs/RUNTIME.md` §3b) but are **not** held across the `pread`; the
  namespace read lock was.
* **Writers serialise only by key.** A name or link-count change reserves the
  keys it read for the whole publish, so conflicting mutations serialise; distinct
  keys run in parallel (§9). The port does not implement conflict-aware parallel
  preparation (the reference's Phase 31).
* **The view duplicates per-inode memory** with the live index: the live writer
  keeps a map-based `index.Index` (for checkpoints, snapshots and `verify`), and
  each view holds private inode copies. The duplication is a constant factor per
  live inode, not per byte of file data. The reference does not duplicate because
  its `PMap` *is* the live spine; here the map-based index is retained so the
  on-disk snapshot/checkpoint format and `verify --deep` are unchanged.
* **Open now builds the read view once.** `volume.Open`/`Init` materialize the
  whole view (`view.FromIndex`), so an open that already decodes a `Θ(index)`
  checkpoint pays one extra `Θ(index)` pass. In one same-session run of the
  checkpoint-recovery court the namespace-heavy open at `2^20` committed bytes
  moved ~6.0 → ~7.1 ms (full open ~8.2 → ~9.7 ms), while the constant-index
  (overwrite) arm was within run-to-run noise; the committed
  `evidence/recovery/checkpoints.json` was **not** refreshed. The added cost is a
  constant factor on the already-`Θ(index)` decode, not a new asymptote.
* **Deleting an inode does not remove it from the view**, exactly as the live map
  did not: an open file unlinked from every directory must stay readable (POSIX).
* **Treap shape is key-derived, so adversarial names could in principle degrade
  it.** Inode ids are dense and monotonic and dentry names are bounded; no
  degradation was observed, and the same property holds for the reference's
  `FxHasher` priorities.

---

## 8. Where this sits in the phases

* Phase 13a introduced the `RWMutex` (readers parallel, a read never sees a
  partial transaction). This extension **completes 13a's named optimization**:
  the lock is gone from reads, publication is atomic, and the commit boundary is
  the only place a new view is built.
* The sealed dense-ordinal spine (phase 18, `internal/inodex`) remains the
  immutable historical role; the read view is a separate, full-inode structure.
* The measured effect is in `docs/RUNTIME.md` §3c and
  `evidence/performance/gc-scaling.json`.
* The staged-write extension (§9) **completes what §7 called the next
  bottleneck**: the write lock is no longer held across the durability barrier,
  so the log's group committer can batch concurrent writers. Its measured effect
  is in `docs/BENCH.md` §7l and `evidence/s3bench.json`.

---

## 9. Staged mutations and namespace-key reservations

The write side no longer holds a single mutex across `Publish`. A mutation runs in
three phases:

1. **Stage.** Validate against the current committed view and build the record
   set, *reserving* the namespace keys the record set depends on.
2. **Publish.** Release every lock and publish the records as one authoritative
   transaction. Concurrent, non-conflicting `Publish` calls overlap, so the log's
   group committer batches them behind one durability barrier.
3. **Release.** The log's commit hook has already applied the transaction to the
   live index, in commit-root order, before `Publish` returned; the reservation is
   then dropped and any waiting conflicting mutation re-validates.

### 9.1 Why reservations, and what they cover

Validating against a *committed* view alone is not enough: two writers could both
see "no entry `x`" and both commit a create for it. So a mutation reserves, for the
whole publish, every key whose not-yet-committed change could invalidate its
record set:

| Key | Mode | Used by |
|-----|------|---------|
| `(parent, name)` dentry | exclusive | every mutation that reads or writes a name: `Create`, `Mkdir`, `Mknod`, `Symlink`, `Unlink`, `Rmdir`, `Rename` (both names), `Link` |
| inode id, shared | shared | a mutation that only *reads* an inode's mutable state (a permission or kind check, an append): `Write`, `Truncate`, `Setattr`, `Setxattr`, `Removexattr`, and the parent of every name operation |
| inode id, exclusive | exclusive | a mutation that emits an **absolute link count** or depends on one: `Mkdir`/`Rmdir` (parent), `Unlink`/`Link` (child/target), `Rename` (both parents) |

Inode ids are allocated from a monotonic counter under the same lock, so two
staged creates can never emit the same id even though they may commit out of
allocation order. A shared reservation permits concurrent readers of an inode and
conflicts only with an exclusive one, so the common case — many creates of
distinct names in one directory — reserves only their distinct dentry keys and
proceeds fully in parallel; that is what lets the barrier batch.

### 9.2 Why it is sound

* `staged` derives the key set, acquires every key atomically (all-or-nothing,
  with a condition-variable wait), then **re-derives** the key set against a
  freshly loaded view. If the committed state moved while it waited, the sets
  differ and it releases and retries instead of committing a record set built
  against stale state. So the record set is always built from state that no
  in-flight transaction can change.
* Because commit order equals the order in which conflicting mutations acquire
  their reservations, the absolute values they emit (link counts, resize sizes)
  compose in that order.
* The index advance still happens exactly once per committed transaction, in
  commit-root order, applying the committed records at their canonical offsets, so
  the live index remains byte-identical to a from-scratch replay
  (`TestConcurrentMixedWorkloadReplayEquivalent`, and every `assertReplayEquivalent`
  above).

### 9.3 What is deliberately *not* serialised

* **Reads.** They take no lock and never observe uncommitted state.
* **Permission/ownership changes against a concurrent name operation.** A
  `chmod`/`chown` takes only a shared reservation, so it does not order against
  another mutation's permission check of the same inode. POSIX specifies no
  outcome for that race; Linux's own VFS has an equivalent TOCTOU window.
* **Two writes to one file.** A data extent is positional, so the extents compose
  in whatever order they commit; the size `Write` returns is the committed size
  after this write applied, which may include a racing write.
* **Directory-ancestry walks** (`Rename`'s cycle check) read the committed view;
  a concurrently moved ancestor is not ordered against them.
