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
	// Compat restricts *new* writes to the Rust reference's format-1 registry and
	// semantics, so a volume this process wrote stays readable by the reference.
	// It suppresses CTIME_UPDATE (ctime is then derived from mtime, exactly as
	// the reference does) and refuses special-file creation (the reference cannot
	// express MKNOD_CREATE, and skipping it would leave a dangling dentry). It is
	// opt-in because it gives up POSIX ctime-only advances; see docs/ON_DISK.md
	// §3.3 and docs/MIGRATION.md §4a. Reading is unaffected — the Go reader always
	// accepts every record type it knows, in either profile.
	Compat bool
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

// WithCompat selects the Rust-reference compatibility profile: new writes stay
// inside the reference's registry and semantics (no CTIME_UPDATE, no
// MKNOD_CREATE).
func WithCompat() Option {
	return func(o *Options) { o.Compat = true }
}

// WithCompatProfile sets the compatibility profile explicitly. It lets the
// `mount --compat` flag override the ORFS_COMPAT environment default in either
// direction.
func WithCompatProfile(enabled bool) Option {
	return func(o *Options) { o.Compat = enabled }
}

// EnvironCompat reports whether the ORFS_COMPAT environment variable requests
// the compatibility profile. It is the CLI flag's default.
func EnvironCompat() bool { return envFlag("ORFS_COMPAT") }

// defaultOptions reads the opt-in policy from the environment. Checkpointing is
// off unless ORFS_CHECKPOINT_EVERY names a positive transaction interval; the
// reference compatibility profile is off unless ORFS_COMPAT is truthy.
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
	if envFlag("ORFS_COMPAT") {
		o.Compat = true
	}
	return o
}

// envFlag reports whether an environment variable is set to a truthy value.
// A variable that is set but empty, "0", "false", or "no" is not truthy.
func envFlag(name string) bool {
	switch os.Getenv(name) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	}
	return false
}
