package fcache_test

import (
	"testing"

	"github.com/infinityabundance/orfs-go/internal/fcache"
)

// BenchmarkCacheGetPut replays the reference scan-heavy trace through each
// policy. A fresh cache is built per iteration so no policy benefits from the
// previous iteration's residency; the reported figure is a full-trace replay.
func BenchmarkCacheGetPut(b *testing.B) {
	w := fcache.ReferenceWorkload()
	trace := w.Trace()

	ctors := []struct {
		name string
		ctor func(int) fcache.Cache
	}{
		{"s3fifo", fcache.NewS3FIFO},
		{"fifo", fcache.NewFIFO},
		{"lru", fcache.NewLRU},
	}
	for _, tc := range ctors {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c := tc.ctor(w.Capacity)
				for _, k := range trace {
					if !c.Get(k) {
						c.Put(k)
					}
				}
			}
		})
	}
}
