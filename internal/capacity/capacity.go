// Package capacity implements the OrFS capacity guard.
//
// Two spaces are kept strictly separate: the authoritative space (or.log/*.seg
// plus commit.watermark) and the optional manifestation space (.or_bloom/). An
// authoritative admission is a whole framed transaction, checked *before the
// first byte is written*; the optional plane is throttled/skipped and never
// fails an authoritative commit.
//
// The guard only ever refuses growth. It never truncates, reclaims, reuses, or
// rewrites committed history (Law I). Footprint grows monotonically, so the
// backing will eventually fill; the guard's job is to make exhaustion an
// orderly, typed, read-only transition rather than a mid-commit ENOSPC.
package capacity

import (
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

// Policy is the only set of tunables the governor consults.
type Policy struct {
	// ReserveBytes is the floor of free backing bytes to keep.
	ReserveBytes uint64
	// SoftQuotaBytes throttles the optional plane above this total.
	SoftQuotaBytes *uint64
	// HardQuotaBytes refuses authoritative growth above this total.
	HardQuotaBytes *uint64
	// AuthoritativeCapacityBytes is an explicit authoritative bound. When nil,
	// the effective capacity is derived from the backing at open.
	AuthoritativeCapacityBytes *uint64
}

// DefaultPolicy is unbounded: unconfigured volumes behave exactly as before,
// with the OS ENOSPC as the ultimate backstop.
func DefaultPolicy() Policy { return Policy{} }

// Verdict is the operator-facing capacity classification.
type Verdict int

const (
	// VerdictOK is comfortable.
	VerdictOK Verdict = iota
	// VerdictWarn is thin: plan to grow or relocate.
	VerdictWarn
	// VerdictExhausted means commits are being refused now.
	VerdictExhausted
)

func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictWarn:
		return "warn"
	case VerdictExhausted:
		return "exhausted"
	}
	return "warn"
}

// ClassifyVerdict computes the verdict from free bytes, the reserve, and the
// sampled bytes per transaction.
func ClassifyVerdict(free, reserve uint64, bytesPerTx *uint64) Verdict {
	var headroom uint64
	if free > reserve {
		headroom = free - reserve
	}
	if headroom == 0 {
		return VerdictExhausted
	}
	if headroom < reserve {
		return VerdictWarn
	}
	if bytesPerTx != nil && headroom < *bytesPerTx {
		return VerdictWarn
	}
	return VerdictOK
}

// Governor is the capacity governor.
type Governor struct {
	policy   Policy
	capacity uint64
	hasCap   bool

	degraded bool
	reason   string

	optionalUsed      uint64
	optionalThrottled bool

	// breaches counts admission refusals by reason. It is the operator-facing
	// signal that the guard is refusing growth (Law I: it refuses, never
	// reclaims), and it is what `orfs metrics` exposes as orfs_breach_total.
	breachMu sync.Mutex
	breaches map[string]uint64
}

// Breach reason labels.
const (
	// BreachBackingReserve is a refusal of the live backing-reserve pre-flight.
	BreachBackingReserve = "backing_reserve"
	// BreachAuthoritative is a refusal of the explicit authoritative bound.
	BreachAuthoritative = "authoritative"
	// BreachHardQuota is a refusal of the explicit hard quota.
	BreachHardQuota = "hard_quota"
)

// New builds a governor with no device probe.
func New(p Policy) *Governor {
	g := &Governor{policy: p, breaches: make(map[string]uint64)}
	if p.AuthoritativeCapacityBytes != nil {
		g.capacity = *p.AuthoritativeCapacityBytes
		g.hasCap = true
	}
	return g
}

// noteBreach records one admission refusal under reason.
func (g *Governor) noteBreach(reason string) {
	g.breachMu.Lock()
	if g.breaches == nil {
		g.breaches = make(map[string]uint64)
	}
	g.breaches[reason]++
	g.breachMu.Unlock()
}

// BreachCounts returns a copy of the refusal counters by reason.
func (g *Governor) BreachCounts() map[string]uint64 {
	g.breachMu.Lock()
	defer g.breachMu.Unlock()
	out := make(map[string]uint64, len(g.breaches))
	for k, v := range g.breaches {
		out[k] = v
	}
	return out
}

// Policy returns the policy in force.
func (g *Governor) Policy() Policy { return g.policy }

// SetPolicy replaces the policy and re-resolves the derived capacity.
func (g *Governor) SetPolicy(p Policy, dir string, used uint64) {
	g.policy = p
	g.hasCap = false
	if p.AuthoritativeCapacityBytes != nil {
		g.capacity = *p.AuthoritativeCapacityBytes
		g.hasCap = true
		return
	}
	g.ResolveFromBacking(dir, used)
}

// ResolveFromBacking derives the effective capacity from live free space:
// capacity = free + used.
func (g *Governor) ResolveFromBacking(dir string, used uint64) {
	if g.policy.AuthoritativeCapacityBytes != nil {
		g.capacity = *g.policy.AuthoritativeCapacityBytes
		g.hasCap = true
		return
	}
	if free, ok := BackingAvailableBytes(dir); ok {
		g.capacity = free + used
		g.hasCap = true
	}
}

// Degraded reports the latch state and its reason.
func (g *Governor) Degraded() (bool, string) { return g.degraded, g.reason }

// Degrade latches the volume read-only.
func (g *Governor) Degrade(reason string) {
	if !g.degraded {
		g.degraded = true
		g.reason = reason
	}
}

// Capacity returns the resolved effective capacity, if known.
func (g *Governor) Capacity() (uint64, bool) { return g.capacity, g.hasCap }

// CheckBackingReserve is the live pre-flight backing guard. It measures current
// free space and refuses a transaction that would breach the reserve. It is
// non-latching while positive headroom remains; it latches only at zero
// headroom. It runs before the first byte is written.
func (g *Governor) CheckBackingReserve(dir string, needed uint64) error {
	if g.degraded {
		return orfserr.ReadOnly("volume degraded: %s", g.reason)
	}
	free, ok := BackingAvailableBytes(dir)
	if !ok {
		// Unmeasurable: fall back to the OS ENOSPC backstop.
		return nil
	}
	reserve := g.policy.ReserveBytes
	var headroom uint64
	if free > reserve {
		headroom = free - reserve
	}
	if needed <= headroom {
		return nil
	}
	if headroom == 0 {
		g.Degrade("backing store at or below the reserve: free " + itoa(free) + " <= reserve " + itoa(reserve))
	}
	g.noteBreach(BreachBackingReserve)
	return orfserr.Exhausted(needed, headroom)
}

// CheckAuthoritative applies the explicit policy bounds. Only the explicit
// authoritative bound and the hard quota latch; a device-derived capacity is a
// snapshot used for reporting and optional throttling only.
func (g *Governor) CheckAuthoritative(used, needed uint64) error {
	if g.degraded {
		return orfserr.ReadOnly("volume degraded: %s", g.reason)
	}
	projected := satAdd(satAdd(used, g.optionalUsed), needed)
	reserve := g.policy.ReserveBytes
	if g.policy.AuthoritativeCapacityBytes != nil {
		limit := satSub(g.capacity, reserve)
		if projected > limit {
			msg := "authoritative capacity exhausted: " + itoa(projected) + " byte(s) needed but only " + itoa(limit) + " admissible"
			g.noteBreach(BreachAuthoritative)
			g.Degrade(msg)
			return orfserr.NoSpaceErr("%s", msg)
		}
	}
	if g.policy.HardQuotaBytes != nil {
		if projected > *g.policy.HardQuotaBytes {
			msg := "authoritative hard quota exceeded: " + itoa(projected) + " > " + itoa(*g.policy.HardQuotaBytes)
			g.noteBreach(BreachHardQuota)
			g.Degrade(msg)
			return orfserr.NoSpaceErr("%s", msg)
		}
	}
	return nil
}

// OptionalAllowed decides whether the optional plane may grow, throttling it
// first: (1) optional throttled, (2) authoritative continues while the reserve
// survives, (3) authoritative refused only when the reserve would be breached.
func (g *Governor) OptionalAllowed(authoritativeUsed, optionalUsed uint64) bool {
	g.optionalUsed = optionalUsed
	if g.degraded {
		g.optionalThrottled = true
		return false
	}
	total := satAdd(authoritativeUsed, optionalUsed)
	allowed := true
	if g.policy.SoftQuotaBytes != nil && total >= *g.policy.SoftQuotaBytes {
		allowed = false
	}
	if g.hasCap && total >= satSub(g.capacity, g.policy.ReserveBytes) {
		allowed = false
	}
	g.optionalThrottled = !allowed
	return allowed
}

// OptionalThrottled reports whether optional growth is currently throttled.
func (g *Governor) OptionalThrottled() bool { return g.optionalThrottled }

// BackingAvailableBytes returns the unprivileged free space on the filesystem
// holding dir: f_bavail * frsize, with frsize = f_frsize when non-zero else
// f_bsize. Returns ok=false when the measurement fails.
func BackingAvailableBytes(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	frsize := st.Frsize
	if frsize == 0 {
		frsize = st.Bsize
	}
	if frsize < 0 {
		return 0, false
	}
	avail := uint64(st.Bavail)
	return avail * uint64(frsize), true
}

// BackingAvailableNearest walks dir's ancestors until one is measurable, for
// init before the directory exists.
func BackingAvailableNearest(dir string) (uint64, bool) {
	cur := dir
	for {
		if free, ok := BackingAvailableBytes(cur); ok {
			return free, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return 0, false
		}
		cur = parent
	}
}

// CheckReservation is the init-time pre-flight: the framed genesis plus the
// reserve must fit in the nearest measurable ancestor.
func CheckReservation(dir string, initialBytes, reserve uint64) error {
	available, ok := BackingAvailableNearest(dir)
	if !ok {
		return nil
	}
	required := satAdd(initialBytes, reserve)
	if required > available {
		return orfserr.Exhausted(required, available)
	}
	return nil
}

func satAdd(a, b uint64) uint64 {
	c := a + b
	if c < a {
		return ^uint64(0)
	}
	return c
}

func satSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
