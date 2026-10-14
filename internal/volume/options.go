package volume

import (
	"os"
	"strconv"

	"github.com/infinityabundance/orfs-go/internal/checkpoint"
)

// Options configures the runtime behaviour of an opened volume. Unlike
// InitOptions these are policy, not identity: they never change the authoritative
// bytes.
type Options struct {
	// CheckpointEveryTransactions writes an index checkpoint after this many
	// committed transactions. Zero disables the transaction trigger.
	CheckpointEveryTransactions uint64
	// CheckpointEveryBytes writes an index checkpoint after this many committed
	// bytes since the last checkpoint. Zero disables the byte trigger.
	CheckpointEveryBytes uint64
	// CheckpointCorrupt selects the corrupt-checkpoint recovery policy.
	CheckpointCorrupt checkpoint.CorruptPolicy
	// ForceFullReplay ignores every checkpoint and replays the whole prefix. It
	// exists for measurement baselines, never for production recovery.
	ForceFullReplay bool
}

// Option mutates an Options.
type Option func(*Options)

// WithCheckpointEvery sets the transaction-interval checkpoint policy.
func WithCheckpointEvery(n uint64) Option {
	return func(o *Options) { o.CheckpointEveryTransactions = n }
}

// WithCheckpointEveryBytes sets the byte-interval checkpoint policy.
func WithCheckpointEveryBytes(n uint64) Option {
	return func(o *Options) { o.CheckpointEveryBytes = n }
}

// WithCheckpointCorruptPolicy sets the corrupt-checkpoint recovery policy.
func WithCheckpointCorruptPolicy(p checkpoint.CorruptPolicy) Option {
	return func(o *Options) { o.CheckpointCorrupt = p }
}

// WithForceFullReplay disables checkpoint use (a measurement baseline).
func WithForceFullReplay() Option {
	return func(o *Options) { o.ForceFullReplay = true }
}

// defaultOptions reads the opt-in policy from the environment. Checkpointing is
// off unless ORFS_CHECKPOINT_EVERY names a positive transaction interval.
func defaultOptions() Options {
	var o Options
	if v := os.Getenv("ORFS_CHECKPOINT_EVERY"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			o.CheckpointEveryTransactions = n
		}
	}
	if v := os.Getenv("ORFS_CHECKPOINT_BYTES"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			o.CheckpointEveryBytes = n
		}
	}
	return o
}
