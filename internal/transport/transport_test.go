package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", F0, false},
		{"f0", F0, false},
		{"f0-single", F0Single, false},
		{"fuse-uring", F2, false},
		{"uring", "", true},
		{"F0", "", true},
		{"f2", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModeIsIOUring(t *testing.T) {
	if !F2.IsIOUring() {
		t.Error("F2.IsIOUring() = false, want true")
	}
	if F0.IsIOUring() || F0Single.IsIOUring() {
		t.Error("F0/F0Single must not report io_uring")
	}
}

// TestResolveIOUringFallsBack: this build exposes no io_uring transport, so
// requesting it always resolves to f0 with a non-empty, exact reason. It never
// resolves silently.
func TestResolveIOUringFallsBack(t *testing.T) {
	s := Resolve(F2)
	if s.Mode != F0 {
		t.Errorf("Resolve(F2).Mode = %q, want %q", s.Mode, F0)
	}
	if !s.Fallback {
		t.Error("Resolve(F2).Fallback = false, want true")
	}
	if s.Reason == "" {
		t.Error("Resolve(F2).Reason is empty; a downgrade must be explained")
	}
}

func TestResolveF0Modes(t *testing.T) {
	for _, m := range []Mode{F0, F0Single} {
		s := Resolve(m)
		if s.Mode != m || s.Fallback {
			t.Errorf("Resolve(%q) = %+v, want mode %q with no fallback", m, s, m)
		}
	}
}

func TestPlan(t *testing.T) {
	s, err := Plan(F0Single, 0)
	if err != nil {
		t.Fatalf("Plan(F0Single, 0): %v", err)
	}
	if s.Mode != F0Single || !s.SingleThreaded || s.GOMAXPROCS != 1 {
		t.Errorf("Plan(F0Single, 0) = %+v, want single-threaded with GOMAXPROCS 1", s)
	}

	if _, err := Plan(F0Single, 4); err == nil {
		t.Error("Plan(F0Single, 4) must reject combining --readers with f0-single")
	}

	s, err = Plan(F0, 8)
	if err != nil {
		t.Fatalf("Plan(F0, 8): %v", err)
	}
	if s.Mode != F0 || s.SingleThreaded || s.GOMAXPROCS != 8 {
		t.Errorf("Plan(F0, 8) = %+v, want GOMAXPROCS 8 and not single-threaded", s)
	}

	// A falling-back request still honours the reader hint.
	s, err = Plan(F2, 8)
	if err != nil {
		t.Fatalf("Plan(F2, 8): %v", err)
	}
	if s.Mode != F0 || !s.Fallback || s.GOMAXPROCS != 8 {
		t.Errorf("Plan(F2, 8) = %+v, want fallback to f0 with GOMAXPROCS 8", s)
	}
}

func writeParam(t *testing.T, val string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "enable_uring")
	if err := os.WriteFile(p, []byte(val+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProbeUnavailableBecauseLibrary is the load-bearing test: even on a kernel
// that compiles in and enables FUSE-over-io_uring, this build reports it
// unavailable because the linked FUSE library exposes no such transport.
func TestProbeUnavailableBecauseLibrary(t *testing.T) {
	s := probeFrom(writeParam(t, "Y"), "7.2.9-test", "v2.12.0")
	if !s.KernelCompiled || !s.KernelEnabled {
		t.Fatalf("kernel flags = compiled %v enabled %v, want both true", s.KernelCompiled, s.KernelEnabled)
	}
	if s.LibraryExposes {
		t.Fatal("go-fuse exposes no io_uring transport; LibraryExposes must be false")
	}
	if s.Available {
		t.Error("Available must be false while the library exposes no transport")
	}
	if !strings.Contains(s.Reason, "go-fuse") || !strings.Contains(s.Reason, "fuse/types.go:325") {
		t.Errorf("reason must name the library and the symbol; got %q", s.Reason)
	}
	if !strings.Contains(s.Reason, "v2.12.0") {
		t.Errorf("reason must name the library version; got %q", s.Reason)
	}
}

func TestProbeKernelAbsent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	s := probeFrom(missing, "6.1.0-test", "")
	if s.KernelCompiled {
		t.Error("KernelCompiled must be false when the parameter file is absent")
	}
	if s.Available {
		t.Error("Available must be false without kernel support")
	}
	if !strings.Contains(s.Reason, "CONFIG_FUSE_IO_URING") {
		t.Errorf("reason must name the missing config; got %q", s.Reason)
	}
}

func TestProbeKernelDisabled(t *testing.T) {
	s := probeFrom(writeParam(t, "N"), "7.2.9-test", "v2.12.0")
	if !s.KernelCompiled {
		t.Error("KernelCompiled must be true when the parameter file exists")
	}
	if s.KernelEnabled {
		t.Error("KernelEnabled must be false for value N")
	}
	if !strings.Contains(s.Reason, "disabled") {
		t.Errorf("reason must say the kernel disabled it; got %q", s.Reason)
	}
}
