// Package doctor implements the capability probe and the capacity report.
//
// The capacity report is the operator's growth monitor: it never reclaims space
// (Law I), so it reports the *rate* of growth and the headroom above the reserve,
// and classifies a verdict of ok / warn / exhausted.
package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/infinityabundance/orfs-go/internal/capacity"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/transport"
)

// SampleWindow is the number of trailing committed boundaries averaged for the
// bytes-per-transaction estimate.
const SampleWindow = 64

// Status is the `doctor --capacity` report.
type Status struct {
	Schema             string       `json:"schema"`
	Volume             string       `json:"volume"`
	PhysicalBytes      uint64       `json:"physical_bytes"`
	SegmentCount       int          `json:"segment_count"`
	AuthoritativeBytes uint64       `json:"authoritative_bytes"`
	FreeBytes          *uint64      `json:"free_bytes"`
	MountPoint         string       `json:"mount_point,omitempty"`
	Device             string       `json:"device,omitempty"`
	Filesystem         string       `json:"filesystem,omitempty"`
	ReserveBytes       uint64       `json:"reserve_bytes"`
	HeadroomBytes      *uint64      `json:"headroom_bytes"`
	TransactionCount   uint64       `json:"transaction_count"`
	BytesPerTxn        *uint64      `json:"bytes_per_transaction"`
	EstimatedRemaining *uint64      `json:"estimated_transactions_remaining"`
	Verdict            string       `json:"verdict"`
	Degraded           bool         `json:"degraded"`
	DegradedReason     string       `json:"degraded_reason,omitempty"`
	Policy             PolicyReport `json:"policy"`
}

// PolicyReport mirrors the capacity policy for the report.
type PolicyReport struct {
	ReserveBytes               uint64  `json:"reserve_bytes"`
	AuthoritativeCapacityBytes *uint64 `json:"authoritative_capacity_bytes"`
	SoftQuotaBytes             *uint64 `json:"soft_quota_bytes"`
	HardQuotaBytes             *uint64 `json:"hard_quota_bytes"`
}

// CapacityStatus computes the report for an opened log.
func CapacityStatus(l *log.Log, volumeDisplay string, reserveOverride *uint64) (Status, error) {
	policy := l.Capacity().Policy()
	reserve := policy.ReserveBytes
	if reserveOverride != nil {
		reserve = *reserveOverride
	}

	physical, err := l.PhysicalBytes()
	if err != nil {
		return Status{}, err
	}

	var freePtr *uint64
	if free, ok := capacity.BackingAvailableBytes(l.Dir()); ok {
		freePtr = &free
	}
	var headroom *uint64
	if freePtr != nil {
		h := *freePtr - minu(*freePtr, reserve)
		headroom = &h
	}

	bpt := sampledBytesPerTransaction(l.Boundaries())
	var remaining *uint64
	if headroom != nil && bpt != nil && *bpt > 0 {
		r := *headroom / *bpt
		remaining = &r
	}

	verdict := capacity.VerdictWarn
	if freePtr != nil {
		verdict = capacity.ClassifyVerdict(*freePtr, reserve, bpt)
	}

	transactions := uint64(0)
	if b := len(l.Boundaries()); b > 0 {
		transactions = uint64(b - 1) // excluding genesis
	}

	degraded, reason := l.Degraded()
	mp, dev, fsType := mountIdentity(l.Dir())

	return Status{
		Schema:             "orfs.capacity.report.v1",
		Volume:             volumeDisplay,
		PhysicalBytes:      physical,
		SegmentCount:       len(l.Extents()),
		AuthoritativeBytes: l.CommittedRoot(),
		FreeBytes:          freePtr,
		MountPoint:         mp,
		Device:             dev,
		Filesystem:         fsType,
		ReserveBytes:       reserve,
		HeadroomBytes:      headroom,
		TransactionCount:   transactions,
		BytesPerTxn:        bpt,
		EstimatedRemaining: remaining,
		Verdict:            verdict.String(),
		Degraded:           degraded,
		DegradedReason:     reason,
		Policy: PolicyReport{
			ReserveBytes:               policy.ReserveBytes,
			AuthoritativeCapacityBytes: policy.AuthoritativeCapacityBytes,
			SoftQuotaBytes:             policy.SoftQuotaBytes,
			HardQuotaBytes:             policy.HardQuotaBytes,
		},
	}, nil
}

// JSON renders the report in the stable envelope.
func (s Status) JSON() ([]byte, error) { return json.MarshalIndent(s, "", "  ") }

// Text renders a human-readable report.
func (s Status) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "volume:              %s\n", s.Volume)
	fmt.Fprintf(&b, "verdict:             %s\n", s.Verdict)
	fmt.Fprintf(&b, "reserve:             %d byte(s)\n", s.ReserveBytes)
	if s.FreeBytes != nil {
		fmt.Fprintf(&b, "backing free:        %d byte(s)\n", *s.FreeBytes)
	} else {
		fmt.Fprintf(&b, "backing free:        unmeasurable\n")
	}
	if s.HeadroomBytes != nil {
		fmt.Fprintf(&b, "headroom above rsv:  %d byte(s)\n", *s.HeadroomBytes)
	}
	fmt.Fprintf(&b, "retained physical:   %d byte(s)\n", s.PhysicalBytes)
	fmt.Fprintf(&b, "authoritative root:  %d byte(s)\n", s.AuthoritativeBytes)
	fmt.Fprintf(&b, "log segments:        %d\n", s.SegmentCount)
	fmt.Fprintf(&b, "committed txns:      %d\n", s.TransactionCount)
	if s.BytesPerTxn != nil {
		fmt.Fprintf(&b, "bytes/transaction:   %d\n", *s.BytesPerTxn)
	}
	if s.EstimatedRemaining != nil {
		fmt.Fprintf(&b, "est. txns remaining: %d\n", *s.EstimatedRemaining)
	}
	if s.Filesystem != "" {
		fmt.Fprintf(&b, "backing fs:          %s (%s on %s)\n", s.Filesystem, s.Device, s.MountPoint)
	}
	if s.Degraded {
		fmt.Fprintf(&b, "degraded:            true (%s)\n", s.DegradedReason)
	}
	return b.String()
}

func sampledBytesPerTransaction(boundaries []uint64) *uint64 {
	if len(boundaries) < 2 {
		return nil
	}
	start := 0
	if len(boundaries) > SampleWindow+1 {
		start = len(boundaries) - (SampleWindow + 1)
	}
	var total, n uint64
	for i := start + 1; i < len(boundaries); i++ {
		d := boundaries[i] - boundaries[i-1]
		if d > 0 {
			total += d
			n++
		}
	}
	if n == 0 {
		return nil
	}
	avg := total / n
	return &avg
}

// mountIdentity resolves the backing directory's mount identity from
// /proc/self/mountinfo, matching by st_dev.
func mountIdentity(dir string) (mountpoint, device, fstype string) {
	var sysStat syscall.Stat_t
	if err := syscall.Stat(dir, &sysStat); err != nil {
		return "", "", ""
	}
	want := uint64(sysStat.Dev)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		sep := -1
		for i, fld := range fields {
			if fld == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) {
			continue
		}
		// fields[2] is major:minor.
		var major, minor uint64
		if _, err := fmt.Sscanf(fields[2], "%d:%d", &major, &minor); err != nil {
			continue
		}
		if unix.Mkdev(uint32(major), uint32(minor)) != want {
			continue
		}
		return unescape(fields[4]), fields[sep+2], fields[sep+1]
	}
	return "", "", ""
}

func unescape(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// Probe is the `doctor` capability report.
type Probe struct {
	FuseDevice     bool              `json:"fuse_device"`
	FuseMountable  bool              `json:"fuse_mountable"`
	BackingDir     string            `json:"backing_dir,omitempty"`
	FuseVersionEnv string            `json:"note,omitempty"`
	Transport      transport.Support `json:"transport"`
}

// CapabilityProbe checks for FUSE availability without mutating anything.
func CapabilityProbe() Probe {
	p := Probe{Transport: transport.Probe()}
	if _, err := os.Stat("/dev/fuse"); err == nil {
		p.FuseDevice = true
	}
	p.FuseMountable = p.FuseDevice
	if !p.FuseDevice {
		p.FuseVersionEnv = "no /dev/fuse: the kernel does not (or this container may not) expose FUSE"
	}
	return p
}

// JSON renders the probe.
func (p Probe) JSON() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }

// Text renders the probe.
func (p Probe) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fuse device present: %v\n", p.FuseDevice)
	fmt.Fprintf(&b, "fuse mountable:      %v\n", p.FuseMountable)
	if p.FuseVersionEnv != "" {
		fmt.Fprintf(&b, "note:                %s\n", p.FuseVersionEnv)
	}
	t := p.Transport
	fmt.Fprintf(&b, "io_uring transport:  %v\n", t.Available)
	if t.KernelRelease != "" {
		fmt.Fprintf(&b, "kernel release:      %s\n", t.KernelRelease)
	}
	fmt.Fprintf(&b, "kernel io_uring:     compiled=%v enabled=%v", t.KernelCompiled, t.KernelEnabled)
	if t.KernelParamVal != "" {
		fmt.Fprintf(&b, " (%s)", t.KernelParamVal)
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "library transport:   %s %s exposes=%v\n", t.Library, t.LibraryVersion, t.LibraryExposes)
	fmt.Fprintf(&b, "reason:              %s\n", t.Reason)
	return b.String()
}

func minu(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// BackingVolumeDisplay names the volume for the report.
func BackingVolumeDisplay(backing string) string {
	abs, err := filepath.Abs(backing)
	if err != nil {
		return backing
	}
	return abs
}

// LogDirName is the log directory name, re-exported for callers.
const LogDirName = log.DirName

// SegmentSuffix re-exported for callers.
const SegmentSuffix = segment.Suffix
