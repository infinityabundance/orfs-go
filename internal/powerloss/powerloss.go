// Package powerloss models a write-back device cache so that crash consistency
// can be tested against contract-violating behaviour, not just a torn file.
//
// The model keeps a durable image and a volatile pending buffer. A Sync
// promotes pending bytes to durable according to a FlushMode; a PowerFail
// discards everything not promoted and applies configured durable damage. The
// model never rewrites committed bytes: a power failure makes history shorter,
// never different (Law I).
//
// This proves safety relative to the modelled device. It does NOT prove
// real-hardware durability: no profile claims power-loss durability.
package powerloss

// FlushMode describes how a completed fsync behaves.
type FlushMode int

const (
	// Barrier: a completed fsync is an honoured barrier (all pending durable).
	Barrier FlushMode = iota
	// Partial: a completed fsync promotes only a seeded subset of sectors.
	Partial
	// Reordered: an honoured barrier that applies writes out of order.
	Reordered
)

// DurableDamage describes damage applied to the already-durable image at the
// moment of power failure.
type DurableDamage int

const (
	// NoDamage: the durable image survives exactly.
	NoDamage DurableDamage = iota
	// TearTail: truncate the trailing partial (or one full) sector of the image.
	TearTail
)

// DefaultSector is the modelled sector size.
const DefaultSector = 512

// Device is a simulated write-back device.
type Device struct {
	mode   FlushMode
	damage DurableDamage
	sector int
	rng    *rng

	durable []byte
	pending []byte
}

// New builds a device with a fixed seed (deterministic).
func New(mode FlushMode, damage DurableDamage, seed uint64) *Device {
	return &Device{mode: mode, damage: damage, sector: DefaultSector, rng: newRNG(seed)}
}

// Write appends bytes to the pending buffer (write-through to the volatile
// cache; in-session reads would see them, but recovery is what we model).
func (d *Device) Write(p []byte) { d.pending = append(d.pending, p...) }

// Sync promotes pending bytes to durable per the flush mode.
func (d *Device) Sync() {
	switch d.mode {
	case Barrier:
		d.durable = append(d.durable, d.pending...)
		d.pending = nil
	case Reordered:
		// Nothing is lost; order alone is unobservable on a monotonic append,
		// so promote everything.
		d.durable = append(d.durable, d.pending...)
		d.pending = nil
	case Partial:
		kept := d.pending[:0:0]
		for off := 0; off < len(d.pending); off += d.sector {
			end := off + d.sector
			if end > len(d.pending) {
				end = len(d.pending)
			}
			if d.rng.next()&1 == 0 {
				d.durable = append(d.durable, d.pending[off:end]...)
			} else {
				kept = append(kept, d.pending[off:end]...)
			}
		}
		d.pending = kept
	}
}

// PowerFail discards pending bytes, applies durable damage, and returns the
// surviving image.
func (d *Device) PowerFail() []byte {
	d.pending = nil
	img := append([]byte(nil), d.durable...)
	if d.damage == TearTail {
		rem := len(img) % d.sector
		if rem > 0 {
			img = img[:len(img)-rem]
		} else if len(img) >= d.sector {
			img = img[:len(img)-d.sector]
		}
	}
	return img
}

// DurableLen is the number of durable bytes.
func (d *Device) DurableLen() int { return len(d.durable) }

// splitmix64: a deterministic, seedable PRNG (matching the reference's choice).
type rng struct{ state uint64 }

func newRNG(seed uint64) *rng { return &rng{state: seed ^ 0x9E3779B97F4A7C15} }

func (r *rng) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
