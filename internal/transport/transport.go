// Package transport names, probes, and negotiates the OrFS FUSE transport.
//
// OrFS serves through Linux FUSE. This package makes the transport story
// explicit instead of implied:
//
//	f0          /dev/fuse driven by go-fuse's multi-reader serving loop (default)
//	f0-single   /dev/fuse driven by a single-threaded serving loop
//	fuse-uring  FUSE-over-io_uring (the reference's "F2"): requested, probed,
//	            and reported unavailable by this build
//
// Availability is never assumed. A transport is reported Available only when
// the kernel provides it AND the userspace FUSE library this build links can
// actually drive it. This build's library, github.com/hanwen/go-fuse/v2
// v2.12.0, exposes no FUSE-over-io_uring transport: it names the capability bit
// (fuse/types.go:325 CAP_OVER_IO_URING, printed at fuse/print.go:58) and
// nothing else. Its transport is a blocking read(2)/writev(2) loop on
// /dev/fuse (fuse/fusefd.go), so `fuse-uring` cannot be served and the daemon
// says so with the exact reason rather than silently downgrading.
package transport

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// Mode names a transport/serving mode.
type Mode string

const (
	// F0 is the /dev/fuse transport with go-fuse's multi-reader serving loop.
	F0 Mode = "f0"
	// F0Single is /dev/fuse with a single-threaded serving loop (one request
	// is processed at a time).
	F0Single Mode = "f0-single"
	// F2 is FUSE-over-io_uring: queue-affine request delivery over io_uring.
	F2 Mode = "fuse-uring"
)

// Parse validates a --transport value. An empty string selects the default.
func Parse(s string) (Mode, error) {
	switch Mode(s) {
	case "":
		return F0, nil
	case F0, F0Single, F2:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("unknown transport %q (want %s, %s, or %s)", s, F0, F0Single, F2)
	}
}

// IsIOUring reports whether the mode is the FUSE-over-io_uring transport.
func (m Mode) IsIOUring() bool { return m == F2 }

// Serving is the concrete serving configuration derived from a transport
// selection. GOMAXPROCS is 0 when it should be left unchanged.
type Serving struct {
	Mode           Mode
	SingleThreaded bool
	GOMAXPROCS     int
	Fallback       bool
	Reason         string
}

// Resolve decides which transport is actually served. f0 and f0-single are
// always honoured. fuse-uring is honoured only when Probe reports it available;
// otherwise it falls back to f0 carrying the exact reason, so the downgrade is
// never silent.
func Resolve(requested Mode) Serving {
	if !requested.IsIOUring() {
		return Serving{Mode: requested}
	}
	sup := Probe()
	if sup.Available {
		return Serving{Mode: requested}
	}
	return Serving{Mode: F0, Fallback: true, Reason: sup.Reason}
}

// Plan turns a transport selection and a reader-concurrency hint into the
// concrete serving configuration.
//
// go-fuse spawns up to clamp(runtime.GOMAXPROCS, 2, 16) reader goroutines, and
// exposes no option to set that count directly, so `--readers N` is expressed
// as GOMAXPROCS=N (go-fuse then clamps it to [2,16]). f0-single is the opposite
// end: one Go processor plus go-fuse serialising request processing.
func Plan(requested Mode, readers int) (Serving, error) {
	res := Resolve(requested)
	s := res
	switch {
	case res.Mode == F0Single:
		if readers != 0 {
			return Serving{}, fmt.Errorf("--readers cannot be combined with --transport %s", F0Single)
		}
		s.SingleThreaded = true
		s.GOMAXPROCS = 1
	case readers > 0:
		s.GOMAXPROCS = readers
	}
	return s, nil
}

// Probe paths: the kernel surface the probe reads, and the FUSE library this
// build names.
var (
	// enableUringPath exists (with value "Y" or "N") exactly when the running
	// kernel was built with CONFIG_FUSE_IO_URING.
	enableUringPath = "/sys/module/fuse/parameters/enable_uring"
	// goFuseModulePath is the FUSE library this build links.
	goFuseModulePath = "github.com/hanwen/go-fuse/v2"
)

// Support is the transport-capability probe: what the kernel provides and what
// the userspace FUSE library this build links can drive.
type Support struct {
	KernelRelease   string `json:"kernel_release"`
	KernelCompiled  bool   `json:"kernel_compiled_in"`
	KernelEnabled   bool   `json:"kernel_enabled"`
	KernelParamPath string `json:"kernel_param_path,omitempty"`
	KernelParamVal  string `json:"kernel_param_value,omitempty"`
	Library         string `json:"library"`
	LibraryVersion  string `json:"library_version"`
	LibraryExposes  bool   `json:"library_exposes_transport"`
	// Available is true only when this build can actually serve the transport:
	// the kernel provides it and the linked library exposes it. Because the
	// library exposes no io_uring transport, this is always false in this build,
	// so `--transport fuse-uring` always resolves to a loud fallback to f0.
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// Probe reports whether the FUSE-over-io_uring transport is available to this
// build on this host. It never mutates anything.
func Probe() Support {
	release := kernelRelease()
	libVersion := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == goFuseModulePath {
				libVersion = d.Version
				break
			}
		}
	}
	return probeFrom(enableUringPath, release, libVersion)
}

// probeFrom is Probe with its inputs made explicit, so the decision logic can be
// tested without the host kernel.
func probeFrom(paramPath, kernelRelease, libVersion string) Support {
	s := Support{
		KernelRelease:   kernelRelease,
		KernelParamPath: paramPath,
		Library:         goFuseModulePath,
		LibraryVersion:  libVersion,
		// This build drives FUSE through go-fuse, which has no io_uring
		// transport at all (see the package comment for the exact symbols).
		LibraryExposes: false,
	}
	if raw, err := os.ReadFile(paramPath); err == nil {
		s.KernelCompiled = true
		s.KernelParamVal = strings.TrimSpace(string(raw))
		s.KernelEnabled = s.KernelParamVal == "Y" || s.KernelParamVal == "1"
	}
	s.Available = s.KernelCompiled && s.KernelEnabled && s.LibraryExposes
	s.Reason = reasonFor(s)
	return s
}

// reasonFor states, precisely and unmovably, why io_uring is or is not usable.
func reasonFor(s Support) string {
	var b strings.Builder
	if !s.LibraryExposes {
		lib := s.Library
		if s.LibraryVersion != "" {
			lib += " " + s.LibraryVersion
		}
		fmt.Fprintf(&b, "the linked FUSE library (%s) exposes no FUSE-over-io_uring transport: it only names the capability bit (fuse/types.go:325 CAP_OVER_IO_URING, printed at fuse/print.go:58) and serves through a read(2)/writev(2) loop on /dev/fuse", lib)
	}
	if !s.KernelCompiled {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "the kernel does not provide FUSE-over-io_uring (%s is absent), so CONFIG_FUSE_IO_URING is not set", s.KernelParamPath)
	} else if !s.KernelEnabled {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "the kernel compiled FUSE-over-io_uring but disabled it at runtime (%s = %q)", s.KernelParamPath, s.KernelParamVal)
	}
	if s.Available {
		b.Reset()
		fmt.Fprintf(&b, "kernel and %s both provide FUSE-over-io_uring", s.Library)
	}
	return b.String()
}

// kernelRelease reads the running kernel release without depending on
// architecture-specific uname field types.
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
