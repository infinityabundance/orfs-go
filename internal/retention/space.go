package retention

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SpaceConfig describes one physical-space measurement.
//
//   - Kind "btrfs": PhysicalBytes is the Btrfs device Used (from `btrfs
//     filesystem usage`); Total/Exclusive/SetShared come from `btrfs filesystem
//     du -s` on Subvol; LogicalBytes is the apparent size of the regular files
//     under Subvol.
//   - Kind "zfs": PhysicalBytes (UsedBytes) is the space the Dataset and all its
//     snapshots occupy including metadata (from `zfs list -o used`); Referenced/
//     LogicalUsed/LogicalReferenced are the remaining `zfs list` figures. For a
//     snapshot, `used` is the space that would be freed by destroying it (its
//     uniquely-retained bytes).
//   - Kind "dir": PhysicalBytes is the disk blocks of PhysicalDir (the
//     authoritative bytes of an OrFS volume's or.log); LogicalBytes is the
//     apparent size of the regular files under Subvol (the mounted namespace).
type SpaceConfig struct {
	Kind        string // btrfs | zfs | dir
	System      string
	Epoch       int
	State       string
	Mountpoint  string // btrfs: the mount used for `btrfs filesystem usage`
	Subvol      string // btrfs: du target and logical walk; dir: logical walk
	PhysicalDir string // dir: physical walk (default Subvol)
	Dataset     string // zfs: dataset or dataset@snapshot to account
}

// MeasureSpace measures the physical vs logical bytes of one committed state.
func MeasureSpace(cfg SpaceConfig) (Space, error) {
	s := Space{
		Schema: SpaceSchema,
		System: cfg.System,
		Epoch:  cfg.Epoch,
		State:  cfg.State,
	}
	switch cfg.Kind {
	case "btrfs":
		if cfg.Mountpoint == "" || cfg.Subvol == "" {
			return Space{}, fmt.Errorf("space btrfs: -mnt and -subvol are required")
		}
		usage, err := runCmd("btrfs", "filesystem", "usage", cfg.Mountpoint)
		if err != nil {
			return Space{}, fmt.Errorf("btrfs filesystem usage: %w", err)
		}
		used, err := ParseBtrfsUsed(usage)
		if err != nil {
			return Space{}, err
		}
		duOut, err := runCmd("btrfs", "filesystem", "du", "-s", cfg.Subvol)
		if err != nil {
			return Space{}, fmt.Errorf("btrfs filesystem du: %w", err)
		}
		total, excl, shared, err := ParseBtrfsDU(duOut)
		if err != nil {
			return Space{}, err
		}
		logical, _, err := walkBytes(cfg.Subvol)
		if err != nil {
			return Space{}, err
		}
		s.PhysicalBytes = used
		s.TotalBytes = total
		s.ExclusiveBytes = excl
		s.SetSharedBytes = shared
		s.LogicalBytes = logical
		s.Note = "PhysicalBytes is the Btrfs device Used across all subvolumes (physical space retention costs); " +
			"TotalBytes/ExclusiveBytes/SetSharedBytes are this subvolume's logical total, its uniquely-retained " +
			"bytes, and its extent-shared bytes; LogicalBytes is the apparent size of the data files."
	case "dir":
		physicalDir := cfg.PhysicalDir
		if physicalDir == "" {
			physicalDir = cfg.Subvol
		}
		logical, _, err := walkBytes(cfg.Subvol)
		if err != nil {
			return Space{}, err
		}
		_, physical, err := walkBytes(physicalDir)
		if err != nil {
			return Space{}, err
		}
		s.PhysicalBytes = physical
		s.LogicalBytes = logical
		s.Note = "PhysicalBytes is the disk blocks of the authoritative source (an OrFS volume's or.log: the " +
			"retained history); LogicalBytes is the apparent size of the mounted data files. OrFS retains every " +
			"committed transition with no extra snapshots, so there is no snapshot-sharing accounting here."
	case "zfs":
		if cfg.Dataset == "" {
			return Space{}, fmt.Errorf("space zfs: -dataset is required")
		}
		out, err := runCmd("zfs", "list", "-Hp", "-o", "used,referenced,logicalused,logicalreferenced", cfg.Dataset)
		if err != nil {
			return Space{}, fmt.Errorf("zfs list: %w", err)
		}
		used, referenced, logicalUsed, logicalReferenced, err := ParseZFSList(out)
		if err != nil {
			return Space{}, err
		}
		s.PhysicalBytes = used
		s.LogicalBytes = logicalReferenced
		s.UsedBytes = used
		s.ReferencedBytes = referenced
		s.LogicalUsedBytes = logicalUsed
		s.LogicalReferencedBytes = logicalReferenced
		s.Note = "PhysicalBytes (UsedBytes) is the space the dataset and all its snapshots occupy including CoW " +
			"metadata (from `zfs list -o used`); ReferencedBytes (referenced) is the logical bytes this " +
			"dataset/snapshot references; LogicalUsedBytes/LogicalReferencedBytes are the compression-aware " +
			"logical figures. For a snapshot, `used` is the space that would be freed by destroying it, so it is " +
			"that retained state's uniquely-retained bytes."
	default:
		return Space{}, fmt.Errorf("space: unknown kind %q (want btrfs, zfs or dir)", cfg.Kind)
	}
	return s, nil
}

// SnapshotConfig describes one read-only snapshot creation.
//
//   - Kind "btrfs" (the default): Live is a source subvolume and Dest a
//     destination path; the command is `btrfs subvolume snapshot -r Live Dest`.
//   - Kind "zfs": Live is a dataset and Dest a snapshot short name; the command
//     is `zfs snapshot Live@Dest`. A ZFS snapshot is always read-only.
type SnapshotConfig struct {
	Kind   string // btrfs | zfs (default btrfs)
	Live   string // btrfs: source subvolume; zfs: dataset
	Dest   string // btrfs: destination path; zfs: snapshot short name
	System string
	Epoch  int
}

// CreateSnapshot makes a read-only snapshot of Live at Dest and returns the
// measured creation cost. Dest must not exist.
func CreateSnapshot(cfg SnapshotConfig) (Snapshot, error) {
	if cfg.Live == "" || cfg.Dest == "" {
		return Snapshot{}, fmt.Errorf("snapshot: -live and -dest are required")
	}
	name, args, recordedDest, note, err := snapshotCommand(cfg.Kind, cfg.Live, cfg.Dest)
	if err != nil {
		return Snapshot{}, err
	}
	t0 := time.Now()
	if _, err := runCmd(name, args...); err != nil {
		return Snapshot{}, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	elapsed := time.Since(t0)
	return Snapshot{
		Schema:        SnapshotSchema,
		System:        cfg.System,
		Epoch:         cfg.Epoch,
		Dest:          recordedDest,
		CreateNs:      elapsed.Nanoseconds(),
		CreateSeconds: elapsed.Seconds(),
		Note:          note,
	}, nil
}

// snapshotCommand builds the create-snapshot command for a comparator kind and
// returns the program, its arguments, the destination name to record, and the
// receipt note. An empty kind is Btrfs (the Btrfs court's behaviour is
// unchanged).
func snapshotCommand(kind, live, dest string) (name string, args []string, recordedDest, note string, err error) {
	switch kind {
	case "", "btrfs":
		return "btrfs", []string{"subvolume", "snapshot", "-r", live, dest}, dest,
			"Read-only Btrfs snapshot created with `btrfs subvolume snapshot -r`; the wall-clock cost of creating one retained historical state.", nil
	case "zfs":
		return "zfs", []string{"snapshot", live + "@" + dest}, live + "@" + dest,
			"Read-only OpenZFS snapshot created with `zfs snapshot` (ZFS snapshots are always read-only); the wall-clock cost of creating one retained historical state.", nil
	default:
		return "", nil, "", "", fmt.Errorf("snapshot: unknown kind %q (want btrfs or zfs)", kind)
	}
}

// runCmd runs a command and returns its combined stdout (trimmed of a trailing
// newline). Stderr is folded into the error so a failure is actionable.
func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// walkBytes returns the summed apparent size of the regular files under root
// (logical) and the summed allocated disk bytes (st_blocks * 512) of every
// regular file and directory (physical).
func walkBytes(root string) (logical int64, physical int64, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Mode().IsRegular() {
			logical += info.Size()
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			physical += st.Blocks * 512
		}
		return nil
	})
	return logical, physical, err
}

// ParseBtrfsUsed extracts the overall "Used:" value from `btrfs filesystem usage`
// output. It accepts the human-readable form ("1.00MiB") and the raw form
// ("1048576"), so it does not depend on a version-specific flag.
func ParseBtrfsUsed(out string) (int64, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Used:") {
			continue
		}
		return ParseSize(strings.TrimSpace(strings.TrimPrefix(line, "Used:")))
	}
	return 0, fmt.Errorf("no overall Used: line in btrfs filesystem usage output")
}

// ParseBtrfsDU extracts the Total, Exclusive and Set-shared columns from the last
// data line of `btrfs filesystem du -s <path>` output.
func ParseBtrfsDU(out string) (total, exclusive, shared int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return 0, 0, 0, fmt.Errorf("empty btrfs filesystem du output")
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	fields := strings.Fields(last)
	if len(fields) < 3 || strings.EqualFold(fields[0], "Total") {
		return 0, 0, 0, fmt.Errorf("unexpected btrfs filesystem du line %q", last)
	}
	if total, err = ParseSize(fields[0]); err != nil {
		return 0, 0, 0, err
	}
	if exclusive, err = ParseSize(fields[1]); err != nil {
		return 0, 0, 0, err
	}
	if shared, err = ParseSize(fields[2]); err != nil {
		return 0, 0, 0, err
	}
	return total, exclusive, shared, nil
}

// ParseZFSList parses the single output line of
// `zfs list -Hp -o used,referenced,logicalused,logicalreferenced <name>`: four
// raw byte counts separated by tabs (or whitespace). A `-` field (a figure ZFS
// does not report for that dataset type) is read as 0.
func ParseZFSList(out string) (used, referenced, logicalUsed, logicalReferenced int64, err error) {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return 0, 0, 0, 0, fmt.Errorf("empty zfs list output")
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 4 {
		fields = strings.Fields(line)
	}
	if len(fields) < 4 {
		return 0, 0, 0, 0, fmt.Errorf("unexpected zfs list line %q (want 4 numeric fields)", line)
	}
	vals := make([]int64, 4)
	for i := 0; i < 4; i++ {
		f := strings.TrimSpace(fields[i])
		if f == "-" || f == "" {
			continue
		}
		v, perr := strconv.ParseInt(f, 10, 64)
		if perr != nil {
			return 0, 0, 0, 0, fmt.Errorf("parse zfs list field %q: %w", f, perr)
		}
		vals[i] = v
	}
	return vals[0], vals[1], vals[2], vals[3], nil
}

// ParseSize parses a byte count in either raw digits or a human-readable form
// with a binary (KiB/MiB/…) or decimal (KB/MB/…) unit.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("parse size: empty string")
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	numPart := s[:i]
	unit := strings.TrimSpace(s[i:])
	if numPart == "" {
		return 0, fmt.Errorf("parse size %q: no number", s)
	}
	f, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	var mult float64
	switch unit {
	case "", "B":
		mult = 1
	case "KiB":
		mult = 1 << 10
	case "MiB":
		mult = 1 << 20
	case "GiB":
		mult = 1 << 30
	case "TiB":
		mult = 1 << 40
	case "PiB":
		mult = 1 << 50
	case "EiB":
		mult = 1 << 60
	case "KB":
		mult = 1e3
	case "MB":
		mult = 1e6
	case "GB":
		mult = 1e9
	case "TB":
		mult = 1e12
	case "PB":
		mult = 1e15
	default:
		return 0, fmt.Errorf("parse size %q: unknown unit %q", s, unit)
	}
	return int64(f*mult + 0.5), nil
}
