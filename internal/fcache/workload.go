package fcache

// The key namespaces. Hot keys and cold (scan) keys are drawn from disjoint
// ranges so a trace never conflates the two.
const (
	hotKeyBase  uint64 = 0
	coldKeyBase uint64 = 1 << 32
)

func hotKey(i int) uint64  { return hotKeyBase + uint64(i) }
func coldKey(i int) uint64 { return coldKeyBase + uint64(i) }

// Workload parameterises the scan-heavy key trace the court replays. Its zero
// value is the reference configuration (see ReferenceWorkload): a 32-entry cache,
// a 20-key hot set, and a 512-key one-pass scan.
//
// The trace is the classic cache-pollution shape. A small hot working set (the
// "20" of an 80/20 hot/cold skew -- the bulk of the reads) is interleaved with a
// full scan over a much larger cold key space (the "80"). Each scan step reads
// one fresh cold key and then HotPerStep hot keys. The cold traffic is what a
// plain FIFO cannot survive: every cold insert evicts its front.
type Workload struct {
	// Capacity is the cache capacity handed to every policy.
	Capacity int
	// Hot is the number of distinct keys in the hot working set.
	Hot int
	// Scan is the number of distinct cold keys in one scan pass.
	Scan int
	// Warmup is the number of hot-set rounds replayed before measurement, so the
	// hot set settles into main before the scan begins.
	Warmup int
	// HotPerStep is the number of hot reads interleaved after each cold scan
	// read. It defaults to Hot, i.e. one pass over the whole hot set per step.
	HotPerStep int
	// Passes is how many times the scan runs (the scan is periodic). It defaults
	// to one.
	Passes int
}

// Default capacity, hot-set size, scan length and warm-up of the reference
// configuration.
const (
	defaultCapacity   = 32
	defaultHot        = 20
	defaultScan       = 512
	defaultWarmup     = 4
	defaultHotPerStep = 20
	defaultPasses     = 1
)

// ReferenceWorkload returns the workload the reference court measured: capacity
// 32, a 20-key hot set, and one 512-key scan interleaved with the whole hot set.
func ReferenceWorkload() Workload { return Workload{}.withDefaults() }

// withDefaults replaces zero (or unset) fields with the reference defaults.
func (w Workload) withDefaults() Workload {
	if w.Capacity <= 0 {
		w.Capacity = defaultCapacity
	}
	if w.Hot <= 0 {
		w.Hot = defaultHot
	}
	if w.Scan <= 0 {
		w.Scan = defaultScan
	}
	if w.Warmup <= 0 {
		w.Warmup = defaultWarmup
	}
	if w.HotPerStep <= 0 {
		w.HotPerStep = w.Hot
	}
	if w.Passes <= 0 {
		w.Passes = defaultPasses
	}
	return w
}

// Resolve returns the workload with zero fields replaced by their defaults.
func (w Workload) Resolve() Workload { return w.withDefaults() }

// HotSetSize returns the resolved number of distinct hot keys.
func (w Workload) HotSetSize() int { return w.withDefaults().Hot }

// warmupTrace returns the priming trace: Warmup rounds over the hot set.
func (w Workload) warmupTrace(o Workload) []uint64 {
	trace := make([]uint64, 0, o.Warmup*o.Hot)
	for r := 0; r < o.Warmup; r++ {
		for i := 0; i < o.Hot; i++ {
			trace = append(trace, hotKey(i))
		}
	}
	return trace
}

// scanTrace returns the measured trace: for each of o.Passes scan passes, every
// cold key once followed by o.HotPerStep hot reads.
func (w Workload) scanTrace(o Workload) []uint64 {
	trace := make([]uint64, 0, o.Passes*o.Scan*(1+o.HotPerStep))
	for p := 0; p < o.Passes; p++ {
		for i := 0; i < o.Scan; i++ {
			trace = append(trace, coldKey(i))
			for j := 0; j < o.HotPerStep; j++ {
				trace = append(trace, hotKey(j%o.Hot))
			}
		}
	}
	return trace
}

// WarmupTrace returns the priming trace replayed before measurement.
func (w Workload) WarmupTrace() []uint64 { return w.warmupTrace(w.withDefaults()) }

// ScanTrace returns the measured, scan-heavy trace.
func (w Workload) ScanTrace() []uint64 { return w.scanTrace(w.withDefaults()) }

// Trace returns the full trace: the warm-up rounds followed by the scan.
func (w Workload) Trace() []uint64 {
	o := w.withDefaults()
	trace := w.warmupTrace(o)
	return append(trace, w.scanTrace(o)...)
}
