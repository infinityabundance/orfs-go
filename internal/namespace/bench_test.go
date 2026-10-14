package namespace_test

import (
	"testing"

	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// benchFileSize is 1 MiB: large enough that a 4 KiB read is a non-trivial slice
// of a single contiguous extent, so the per-read cost is dominated by the read
// path itself rather than by setup.
const benchFileSize = 1 << 20

// benchSink keeps the read result alive so the compiler cannot elide the work.
var benchSink []byte

// benchReadVolume builds a filesystem-backed volume in a temp dir holding one
// 1 MiB file written as a single DATA_EXTENT, and returns the namespace, the
// read credential, and the file id. No manifest source is registered, so the
// authoritative path is the one exercised.
func benchReadVolume(b *testing.B) (namespace.Credential, *namespace.Namespace, uint64) {
	b.Helper()
	v, err := volume.Init(b.TempDir(), volume.InitOptions{SegmentSize: 4 * 1024 * 1024})
	if err != nil {
		b.Fatalf("init volume: %v", err)
	}
	b.Cleanup(func() { v.Close() })

	ns := namespace.New(v)
	cred := namespace.Credential{}
	file, err := ns.Create(cred, record.RootInode, "hot.bin", 0o644, 0, 0)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	data := make([]byte, benchFileSize)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	if _, err := ns.Write(cred, file.ID, 0, data); err != nil {
		b.Fatalf("write: %v", err)
	}
	return cred, ns, file.ID
}

// BenchmarkAuthoritativeRead4K measures the steady-state authoritative 4 KiB
// read path: one million-byte file, reads walked across the whole file so the
// working set is the file, not a single hot cache line. It reports ns/op, B/op,
// and allocs/op.
func BenchmarkAuthoritativeRead4K(b *testing.B) {
	cred, ns, id := benchReadVolume(b)
	const chunk = 4096
	steps := uint64(benchFileSize / chunk)
	b.SetBytes(chunk)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := uint64(i) % steps * chunk
		buf, err := ns.Read(cred, id, off, chunk)
		if err != nil {
			b.Fatalf("read: %v", err)
		}
		if len(buf) != chunk {
			b.Fatalf("short read: got %d bytes, want %d", len(buf), chunk)
		}
		benchSink = buf
	}
}

// BenchmarkAuthoritativeByteAt measures a single-byte authoritative read, the
// analogous "ByteAt" reference: the smallest possible read still pays the same
// fixed per-read cost (destination + planner), so its allocs/op is the floor.
func BenchmarkAuthoritativeByteAt(b *testing.B) {
	cred, ns, id := benchReadVolume(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := uint64(i) % benchFileSize
		buf, err := ns.Read(cred, id, off, 1)
		if err != nil {
			b.Fatalf("read: %v", err)
		}
		if len(buf) != 1 {
			b.Fatalf("short read: got %d bytes, want 1", len(buf))
		}
		benchSink = buf
	}
}
