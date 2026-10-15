package s3gw

import (
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// VolumeSet is one opened OrFS volume together with the two things the gateway
// needs from it: the live namespace (writes and path resolution) and the
// manifested read path (cache-admitted reads).
//
// The volume is opened read-write and appended through the library; no FUSE
// mount is involved. Every mutation is an ordinary authoritative transaction,
// so a gateway append is exactly the append the FUSE path would have produced.
type VolumeSet struct {
	Vol *volume.Volume
	NS  *namespace.Namespace
	// Cache is the manifested-body read planner, or nil when the concealed read
	// plane is disabled for this volume.
	Cache *manifest.Cache
}

// VolumeOptions configure how a VolumeSet is opened.
type VolumeOptions struct {
	// ManifestCacheCapacity bounds the resident manifested-body cache (the
	// S3-FIFO admission policy by default). Zero selects
	// manifest.DefaultAdmissionCapacity; a negative value disables the plane.
	ManifestCacheCapacity int
	// ManifestPolicy selects the admission policy for resident bodies.
	ManifestPolicy manifest.Policy
	// DisableManifest turns the concealed read plane off entirely: reads go
	// straight to the authoritative extents.
	DisableManifest bool
	// CheckpointEveryTransactions writes an index checkpoint every n committed
	// transactions, bounding restart replay. Zero disables it.
	CheckpointEveryTransactions uint64
}

// OpenVolumeSet opens an existing OrFS volume read-write and installs the
// manifested read path.
func OpenVolumeSet(backing string, o VolumeOptions) (*VolumeSet, error) {
	var vopts []volume.Option
	if o.CheckpointEveryTransactions > 0 {
		vopts = append(vopts, volume.WithCheckpointEvery(o.CheckpointEveryTransactions))
	}
	v, err := volume.Open(backing, log.ReadWrite, vopts...)
	if err != nil {
		return nil, err
	}
	return newVolumeSet(v, o), nil
}

// InitVolumeSet creates a fresh OrFS volume and installs the manifested read
// path. It exists for tests and for a first-run convenience flag.
func InitVolumeSet(backing string, segmentSize uint64, o VolumeOptions) (*VolumeSet, error) {
	var vopts []volume.Option
	if o.CheckpointEveryTransactions > 0 {
		vopts = append(vopts, volume.WithCheckpointEvery(o.CheckpointEveryTransactions))
	}
	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: segmentSize}, vopts...)
	if err != nil {
		return nil, err
	}
	return newVolumeSet(v, o), nil
}

func newVolumeSet(v *volume.Volume, o VolumeOptions) *VolumeSet {
	ns := namespace.New(v)
	vs := &VolumeSet{Vol: v, NS: ns}
	if o.DisableManifest || o.ManifestCacheCapacity < 0 {
		return vs
	}
	capacity := o.ManifestCacheCapacity
	if capacity == 0 {
		capacity = manifest.DefaultAdmissionCapacity
	}
	cache := manifest.NewCache(v,
		manifest.WithAdmissionCapacity(capacity),
		manifest.WithPolicy(o.ManifestPolicy),
	)
	// The authoritative oracle: a manifested body is trusted only if its
	// logical digest equals what the log holds now.
	cache.SetAuthority(func(inode, offset, size uint64) ([]byte, bool) {
		b, err := ns.ReadAuthoritative(namespace.Credential{}, inode, offset, size)
		if err != nil {
			return nil, false
		}
		return b, true
	})
	ns.SetManifestSource(cache)
	vs.Cache = cache
	return vs
}

// Close releases the volume.
func (vs *VolumeSet) Close() error {
	if vs == nil || vs.Vol == nil {
		return nil
	}
	return vs.Vol.Close()
}
