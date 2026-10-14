package checkpoint_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/checkpoint"
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

var cred = namespace.Credential{}

// setupVolume builds a volume with `txns` create transactions, taking a
// checkpoint every `every` transactions when every > 0.
func setupVolume(t *testing.T, txns int, every uint64) string {
	t.Helper()
	backing := t.TempDir()
	var opts []volume.Option
	if every > 0 {
		opts = append(opts, volume.WithCheckpointEvery(every))
	}
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 32 * 1024}, opts...)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	ns := namespace.New(v)
	for i := 0; i < txns; i++ {
		name := fmt.Sprintf("f%05d", i)
		f, err := ns.Create(namespace.Credential{}, record.RootInode, name, 0o644, 0, 0)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if i%3 == 0 {
			if _, err := ns.Write(namespace.Credential{}, f.ID, 0, []byte("payload")); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
		}
	}
	if err := v.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return backing
}

func openLog(t *testing.T, backing string) *log.Log {
	t.Helper()
	l, err := log.Open(backing, log.ReadOnly)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func fullReplay(t *testing.T, backing string, l *log.Log) *index.Index {
	t.Helper()
	idx, err := index.Replay(log.Dir(backing), l.Extents(), l.CommittedRoot())
	if err != nil {
		t.Fatalf("full replay: %v", err)
	}
	return idx
}

// TestCheckpointMountEqualsFullReplay is the core correctness property: a mount
// that loads a checkpoint and replays only the suffix is byte-identical to a
// from-scratch replay, and lands on the same committed root.
func TestCheckpointMountEqualsFullReplay(t *testing.T) {
	backing := setupVolume(t, 48, 12)
	l := openLog(t, backing)

	full := fullReplay(t, backing, l)
	idx, rep, err := checkpoint.Mount(l, checkpoint.Options{Corrupt: checkpoint.FallbackFull})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if !rep.UsedCheckpoint {
		t.Fatalf("expected a checkpoint to be used: %+v", rep)
	}
	if !idx.Equal(full) {
		t.Fatal("checkpoint mount is not equal to full replay")
	}
	if idx.Root != l.CommittedRoot() {
		t.Fatalf("mount root %d != committed root %d", idx.Root, l.CommittedRoot())
	}
	if rep.BytesReplayed >= l.CommittedRoot() {
		t.Fatalf("suffix replay (%d) did not skip any of the %d committed bytes", rep.BytesReplayed, l.CommittedRoot())
	}
	// The checkpoint root is a real committed boundary.
	found := false
	for _, b := range l.Boundaries() {
		if b == rep.CheckpointRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("checkpoint root %d is not a committed boundary", rep.CheckpointRoot)
	}
}

// TestCheckpointIsAcceleratorNotAuthority proves the derived structures are not
// load-bearing: ignoring every checkpoint, or deleting the derived directory,
// yields the same recoverable index.
func TestCheckpointIsAcceleratorNotAuthority(t *testing.T) {
	backing := setupVolume(t, 40, 12)
	l := openLog(t, backing)
	full := fullReplay(t, backing, l)

	forced, _, err := checkpoint.Mount(l, checkpoint.Options{ForceFullReplay: true})
	if err != nil {
		t.Fatalf("forced full replay: %v", err)
	}
	ckpt, _, err := checkpoint.Mount(l, checkpoint.Options{})
	if err != nil {
		t.Fatalf("checkpoint mount: %v", err)
	}
	if !forced.Equal(full) || !ckpt.Equal(full) {
		t.Fatal("a checkpoint changed the recoverable index")
	}

	if err := os.RemoveAll(filepath.Join(backing, checkpoint.DirName)); err != nil {
		t.Fatal(err)
	}
	after, _, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount after deleting the derived directory: %v", err)
	}
	if !after.Equal(full) {
		t.Fatal("deleting the derived directory changed the recoverable index")
	}
}

// TestDirectoryQuickOpen proves the derived committed-root directory supplies
// discovery (avoiding the whole-log scan) and that distrusting it is safe.
func TestDirectoryQuickOpen(t *testing.T) {
	backing := setupVolume(t, 40, 12)

	// A writable open publishes the derived directory.
	v, err := volume.Open(backing, log.ReadWrite)
	if err != nil {
		t.Fatalf("writable open: %v", err)
	}
	v.Close()

	l := openLog(t, backing)
	full := fullReplay(t, backing, l)
	idx, rep, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if rep.DiscoverySource != checkpoint.SourceDirectory {
		t.Fatalf("want directory discovery, got %q", rep.DiscoverySource)
	}
	if !rep.DirectoryTrusted {
		t.Fatal("directory should be trusted")
	}
	if !idx.Equal(full) {
		t.Fatal("directory-assisted mount is not equal to full replay")
	}

	// Deleting the directory falls back to the authoritative scan.
	if err := os.RemoveAll(filepath.Join(backing, checkpoint.DirName)); err != nil {
		t.Fatal(err)
	}
	_, rep2, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount after deletion: %v", err)
	}
	if rep2.DiscoverySource != checkpoint.SourceLogScan {
		t.Fatalf("want a log scan after deletion, got %q", rep2.DiscoverySource)
	}
}

// TestDirectoryDistrusted covers the distrust conditions: an over-claiming
// directory (a scan bound beyond the committed root) and a foreign volume uuid.
func TestDirectoryDistrusted(t *testing.T) {
	backing := setupVolume(t, 20, 10)
	l := openLog(t, backing)

	if err := checkpoint.WriteDirectory(backing, l.Genesis().UUID, checkpoint.Directory{
		ScannedRoot: l.CommittedRoot() + 1<<20,
	}); err != nil {
		t.Fatal(err)
	}
	_, rep, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if rep.DirectoryTrusted || rep.DiscoverySource != checkpoint.SourceLogScan {
		t.Fatalf("an over-claiming directory must be distrusted, got %+v", rep)
	}

	// A foreign uuid is distrusted.
	var foreign [16]byte
	for i := range foreign {
		foreign[i] = 0x5a
	}
	if err := checkpoint.WriteDirectory(backing, foreign, checkpoint.Directory{ScannedRoot: l.CommittedRoot()}); err != nil {
		t.Fatal(err)
	}
	_, rep2, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if rep2.DirectoryTrusted || rep2.DiscoverySource != checkpoint.SourceLogScan {
		t.Fatalf("a foreign directory must be distrusted, got %+v", rep2)
	}

	// A covered bound that is not a record boundary (a corrupt directory) must be
	// distrusted, not surfaced as an error.
	if err := checkpoint.WriteDirectory(backing, l.Genesis().UUID, checkpoint.Directory{ScannedRoot: 1}); err != nil {
		t.Fatal(err)
	}
	idx, rep3, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount with a corrupt covered bound must not error: %v", err)
	}
	if rep3.DirectoryTrusted || rep3.DiscoverySource != checkpoint.SourceLogScan {
		t.Fatalf("a corrupt covered bound must be distrusted, got %+v", rep3)
	}
	if !idx.Equal(fullReplay(t, backing, l)) {
		t.Fatal("fallback after a corrupt directory is not equal to full replay")
	}
}

// TestBogusDirectoryOffsetSkipped proves a directory entry that does not name a
// readable checkpoint is skipped, and the next-newest valid checkpoint is used.
func TestBogusDirectoryOffsetSkipped(t *testing.T) {
	backing := setupVolume(t, 48, 12)
	l := openLog(t, backing)
	full := fullReplay(t, backing, l)

	locs, err := log.ScanRecordOffsets(l.Dir(), l.Extents(), 0, l.CommittedRoot(),
		map[uint16]bool{record.TypeIndexCheckpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) < 2 {
		t.Fatalf("want at least 2 checkpoints, got %d", len(locs))
	}
	real := locs[0].Offset
	bogus := locs[len(locs)-1].Offset + 1
	if bogus >= l.CommittedRoot() {
		t.Skip("no room after the newest checkpoint for a bogus offset")
	}
	if err := checkpoint.WriteDirectory(backing, l.Genesis().UUID, checkpoint.Directory{
		ScannedRoot: l.CommittedRoot(),
		Offsets:     []uint64{real, bogus},
	}); err != nil {
		t.Fatal(err)
	}
	idx, rep, err := checkpoint.Mount(l, checkpoint.Options{UseDirectory: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if !rep.UsedCheckpoint {
		t.Fatalf("expected the next-newest valid checkpoint to be used: %+v", rep)
	}
	if rep.CheckpointsRejected == 0 {
		t.Fatal("the bogus offset should have been rejected")
	}
	if !idx.Equal(full) {
		t.Fatal("reconstruction after skipping a bogus offset is not equal to full replay")
	}
}

// TestCorruptCheckpointPolicy exercises the two explicit policies for a
// fully-framed checkpoint whose snapshot fails its self-digest.
func TestCorruptCheckpointPolicy(t *testing.T) {
	backing := setupVolume(t, 24, 12)

	// Craft a fully framed, well-formed checkpoint record whose recorded digest
	// does not match its embedded snapshot.
	v, err := volume.Open(backing, log.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := index.EncodeSnapshot(v.Index())
	if err != nil {
		t.Fatal(err)
	}
	bad := record.IndexCheckpoint{
		Root:        v.Index().Root,
		InodeCount:  uint64(len(v.Index().Inodes)),
		DentryCount: uint64(len(v.Index().Dentry)),
		Digest:      [32]byte{0xde, 0xad, 0xbe, 0xef},
		IndexBytes:  doc,
	}.Encode()
	rec := &record.Record{
		Header:  record.Header{RecordType: record.TypeIndexCheckpoint, PayloadLength: uint64(len(bad)), InodeID: record.RootInode},
		Payload: bad,
	}
	if _, err := v.Publish([]*record.Record{rec}); err != nil {
		t.Fatalf("publish crafted checkpoint: %v", err)
	}
	v.Close()

	l := openLog(t, backing)
	full := fullReplay(t, backing, l)

	idx, rep, err := checkpoint.Mount(l, checkpoint.Options{Corrupt: checkpoint.FallbackFull})
	if err != nil {
		t.Fatalf("FallbackFull must not error: %v", err)
	}
	if !idx.Equal(full) {
		t.Fatal("fallback reconstruction is not equal to full replay")
	}
	if !rep.UsedCheckpoint || rep.CheckpointsRejected == 0 {
		t.Fatalf("expected the corrupt checkpoint to be rejected and an earlier one used: %+v", rep)
	}

	if _, _, err := checkpoint.Mount(l, checkpoint.Options{Corrupt: checkpoint.Refuse}); !errors.Is(err, orfserr.ErrCorrupt) {
		t.Fatalf("Refuse must return a typed corrupt error, got %v", err)
	}
}

// TestNonBoundaryCheckpointRejected proves a checkpoint whose root is not a
// committed boundary is rejected, forcing a full replay.
func TestNonBoundaryCheckpointRejected(t *testing.T) {
	backing := setupVolume(t, 12, 0)
	v, err := volume.Open(backing, log.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	doc, digest, err := index.EncodeSnapshot(v.Index())
	if err != nil {
		t.Fatal(err)
	}
	payload := record.IndexCheckpoint{
		Root:        1, // not a committed boundary
		InodeCount:  uint64(len(v.Index().Inodes)),
		DentryCount: uint64(len(v.Index().Dentry)),
		Digest:      digest,
		IndexBytes:  doc,
	}.Encode()
	rec := &record.Record{
		Header:  record.Header{RecordType: record.TypeIndexCheckpoint, PayloadLength: uint64(len(payload)), InodeID: record.RootInode},
		Payload: payload,
	}
	if _, err := v.Publish([]*record.Record{rec}); err != nil {
		t.Fatal(err)
	}
	v.Close()

	l := openLog(t, backing)
	idx, rep, err := checkpoint.Mount(l, checkpoint.Options{})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if !rep.FullReplayFallback {
		t.Fatalf("a non-boundary checkpoint must force a full replay: %+v", rep)
	}
	if !idx.Equal(fullReplay(t, backing, l)) {
		t.Fatal("full-replay fallback is not equal to full replay")
	}
}

// TestCheckpointEnvPolicy proves the opt-in environment policy writes
// checkpoints that bound replay.
func TestCheckpointEnvPolicy(t *testing.T) {
	t.Setenv("ORFS_CHECKPOINT_EVERY", "4")
	backing := t.TempDir()
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: 32 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	ns := namespace.New(v)
	for i := 0; i < 20; i++ {
		if _, err := ns.Create(cred, record.RootInode, fmt.Sprintf("g%05d", i), 0o644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	v.Close()

	l := openLog(t, backing)
	idx, rep, err := checkpoint.Mount(l, checkpoint.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.UsedCheckpoint {
		t.Fatalf("ORFS_CHECKPOINT_EVERY did not produce a usable checkpoint: %+v", rep)
	}
	if !idx.Equal(fullReplay(t, backing, l)) {
		t.Fatal("env-policy checkpoint mount is not equal to full replay")
	}
}
