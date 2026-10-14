// Package orbench holds courts that measure OrFS-GO against the laws it claims:
// the growth of retained authoritative history (non-withdrawal), the manifested
// space it spends to accelerate reads, and the amplification those imply.
//
// These are courts, not features: they produce machine-readable receipts under
// evidence/ and they never fabricate a number. If a run is bounded, the bound is
// recorded.
package orbench

import (
	"os"
	"path/filepath"

	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// Arm names the measurement arm.
type Arm string

const (
	// ArmAuthoritative measures the authoritative log alone.
	ArmAuthoritative Arm = "authoritative"
	// ArmManifested measures the authoritative log plus manifested bodies.
	ArmManifested Arm = "manifested"
)

// Sample is one epoch's byte accounting.
type Sample struct {
	Epoch                      int     `json:"epoch"`
	LogicalBytes               uint64  `json:"logical_bytes"`
	AuthoritativePhysicalBytes uint64  `json:"authoritative_physical_bytes"`
	ManifestedBytes            uint64  `json:"manifested_bytes"`
	TotalPhysicalBytes         uint64  `json:"total_physical_bytes"`
	AmplificationAuth          float64 `json:"amplification_authoritative"`
	AmplificationManifested    float64 `json:"amplification_manifested"`
	Transactions               uint64  `json:"transactions"`
}

// AmplificationResult is the receipt for one arm.
type AmplificationResult struct {
	Schema  string   `json:"schema"`
	Arm     Arm      `json:"arm"`
	Epochs  int      `json:"epochs"`
	Payload int      `json:"payload_bytes"`
	Note    string   `json:"note"`
	Samples []Sample `json:"samples"`
}

// Options configures a growth run.
type Options struct {
	Epochs    int
	Payload   int
	SegmentSz uint64
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.Epochs <= 0 {
		out.Epochs = 24
	}
	if out.Payload <= 0 {
		out.Payload = 16 * 1024
	}
	if out.SegmentSz == 0 {
		out.SegmentSz = 4 * 1024 * 1024
	}
	return out
}

func payload(n int, epoch int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*7 + epoch*131) % 251)
	}
	return b
}

// Run drives a growing retained history and samples the byte accounting.
//
// Each epoch overwrites one file: the DATA_EXTENT bytes are appended and the old
// bytes are retained (Law I), so the authoritative physical footprint grows
// while the logical content stays one payload. In the manifested arm every
// version is also manifested, so the manifested arm spends strictly more.
func Run(backing string, arm Arm, opt Options) (AmplificationResult, error) {
	o := opt.withDefaults()
	res := AmplificationResult{
		Schema:  "orfs.performance.amplification.v1",
		Arm:     arm,
		Epochs:  o.Epochs,
		Payload: o.Payload,
		Note:    "bounded growth court; each epoch overwrites one file so retained history grows monotonically (Law I)",
	}

	v, err := volume.Init(backing, volume.InitOptions{SegmentSize: o.SegmentSz})
	if err != nil {
		return res, err
	}
	defer v.Close()
	ns := namespace.New(v)
	cred := namespace.Credential{}

	var cache *manifest.Cache
	if arm == ArmManifested {
		cache = manifest.NewCache(v)
		ns.SetManifestSource(cache)
	}

	file, err := ns.Create(cred, record.RootInode, "hot.bin", 0o644, 0, 0)
	if err != nil {
		return res, err
	}

	for e := 0; e < o.Epochs; e++ {
		data := payload(o.Payload, e)
		if _, err := ns.Write(cred, file.ID, 0, data); err != nil {
			return res, err
		}
		if cache != nil {
			in, _ := ns.Getattr(file.ID)
			if _, err := cache.Publish(file.ID, in.Version, v.Log.CommittedRoot(), data); err != nil {
				return res, err
			}
		}
		s, err := sample(v, "hot.bin", e, arm)
		if err != nil {
			return res, err
		}
		res.Samples = append(res.Samples, s)
	}
	return res, nil
}

func sample(v *volume.Volume, _ string, epoch int, arm Arm) (Sample, error) {
	files, err := os.ReadDir(filepath.Join(v.Backing, "or.log"))
	if err != nil {
		return Sample{}, orfserr.IO(err, "read log dir")
	}
	var logBytes uint64
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		logBytes += uint64(info.Size())
	}

	manBytes := dirBytes(filepath.Join(v.Backing, manifest.BloomDirName))

	// Logical content is the visible file size (one payload), read back from the
	// namespace so it reflects what a user sees.
	ns := namespace.New(v)
	in, _ := ns.Lookup(namespace.Credential{}, record.RootInode, "hot.bin")
	var logical uint64
	if in != nil {
		logical = in.Size
	}

	total := logBytes + manBytes
	s := Sample{
		Epoch:                      epoch + 1,
		LogicalBytes:               logical,
		AuthoritativePhysicalBytes: logBytes,
		ManifestedBytes:            manBytes,
		TotalPhysicalBytes:         total,
		Transactions:               v.Log.CommittedRecordCount(),
	}
	if logical > 0 {
		s.AmplificationAuth = float64(logBytes) / float64(logical)
		s.AmplificationManifested = float64(manBytes) / float64(logical)
	}
	_ = arm
	return s, nil
}

func dirBytes(dir string) uint64 {
	var total uint64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}
