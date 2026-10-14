// Command retentionbench is the workload driver for the Btrfs retained-snapshots
// retention court (courts/retention-btrfs). It measures one bounded arm at a time
// against a mounted directory — the current state or a retained snapshot — and
// writes one measurement receipt (schema orfs.retention.measure.v1). It reuses
// the append/read shape and the errno-classified, exact-sample latency style of
// cmd/orfscompare.
//
// Subcommands:
//
//	prepare  -dir <mnt> [-prefix P] [-writers N]
//	append   -dir <mnt> -system S -prefix P -writers N -records R [-record-bytes B] [-seconds S] [-epoch E] [-out F]
//	read     -dir <mnt> -system S -state current|snapshot -read-file F -readers R -bs B -seconds S [-epoch E] [-out F]
//	space    -kind btrfs|dir -system S -epoch E -state current|snapshot [-mnt M] [-subvol P] [-physical-dir D] [-out F]
//	snapshot -live <subvol> -dest <path> -system S -epoch E [-out F]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/infinityabundance/orfs-go/internal/retention"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "prepare":
		os.Exit(runPrepare(os.Args[2:]))
	case "append":
		os.Exit(runAppend(os.Args[2:]))
	case "read":
		os.Exit(runRead(os.Args[2:]))
	case "space":
		os.Exit(runSpace(os.Args[2:]))
	case "snapshot":
		os.Exit(runSnapshot(os.Args[2:]))
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "retentionbench: unknown command %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `retentionbench — Btrfs retained-snapshots retention court driver

usage:
  retentionbench prepare  -dir <mnt> [-prefix P] [-writers N]
  retentionbench append   -dir <mnt> -system S -prefix P -writers N -records R [-record-bytes B] [-seconds S] [-epoch E] [-out F]
  retentionbench read     -dir <mnt> -system S -state current|snapshot -read-file F -readers R -bs B -seconds S [-epoch E] [-out F]
  retentionbench space    -kind btrfs|dir -system S -epoch E -state current|snapshot [-mnt M] [-subvol P] [-physical-dir D] [-out F]
  retentionbench snapshot -live <subvol> -dest <path> -system S -epoch E [-out F]
`)
}

func runPrepare(args []string) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	dir := fs.String("dir", "", "mounted directory (required)")
	prefix := fs.String("prefix", "append", "writer file prefix")
	writers := fs.Int("writers", 1, "writer files to create")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := retention.Prepare(*dir, *prefix, *writers); err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench prepare: %v\n", err)
		return 1
	}
	return 0
}

func runAppend(args []string) int {
	fs := flag.NewFlagSet("append", flag.ContinueOnError)
	cfg := retention.AppendConfig{}
	fs.StringVar(&cfg.Dir, "dir", "", "mounted directory (required)")
	fs.StringVar(&cfg.System, "system", "unknown", "system label for the receipt row")
	fs.StringVar(&cfg.Prefix, "prefix", "append", "writer file prefix")
	fs.IntVar(&cfg.Writers, "writers", 1, "concurrent writers")
	fs.IntVar(&cfg.Records, "records", 256, "records appended per writer (the defined epoch size)")
	fs.IntVar(&cfg.RecordBytes, "record-bytes", 4096, "record size")
	fs.Float64Var(&cfg.MaxSeconds, "seconds", 0, "safety time bound in seconds (0 = none)")
	fs.Int64Var(&cfg.MaxBytesPerWriter, "max-bytes-per-writer", 0, "bound each writer's appended bytes (0 = none)")
	fs.IntVar(&cfg.MaxSamples, "max-samples-per-worker", 0, "per-worker latency sample cap")
	fs.Int64Var(&cfg.Seed, "seed", 1, "deterministic seed")
	fs.IntVar(&cfg.Epoch, "epoch", 0, "epoch index")
	out := fs.String("out", "", "receipt path (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cfg.Dir == "" {
		fmt.Fprintln(os.Stderr, "retentionbench append: -dir is required")
		return 2
	}
	m, err := retention.RunAppend(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench append: %v\n", err)
		return 1
	}
	if m.Stats.Ops == 0 {
		fmt.Fprintln(os.Stderr, "retentionbench append: no operations ran")
		return 1
	}
	return emit(*out, m)
}

func runRead(args []string) int {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	cfg := retention.ReadConfig{}
	fs.StringVar(&cfg.Dir, "dir", "", "mounted directory or snapshot (required)")
	fs.StringVar(&cfg.System, "system", "unknown", "system label for the receipt row")
	fs.StringVar(&cfg.ReadFile, "read-file", "append-0.bin", "read arm file")
	fs.IntVar(&cfg.Readers, "readers", 1, "concurrent readers")
	fs.IntVar(&cfg.BlockSize, "bs", 4096, "block size")
	fs.Float64Var(&cfg.Seconds, "seconds", 1, "bounded run duration")
	fs.IntVar(&cfg.MaxSamples, "max-samples-per-worker", 0, "per-worker latency sample cap")
	fs.Int64Var(&cfg.Seed, "seed", 1, "deterministic seed")
	fs.IntVar(&cfg.Epoch, "epoch", 0, "epoch index")
	fs.StringVar(&cfg.State, "state", "current", "current | snapshot")
	out := fs.String("out", "", "receipt path (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cfg.Dir == "" {
		fmt.Fprintln(os.Stderr, "retentionbench read: -dir is required")
		return 2
	}
	m, err := retention.RunRead(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench read: %v\n", err)
		return 1
	}
	if m.Stats.Ops == 0 {
		fmt.Fprintln(os.Stderr, "retentionbench read: no operations ran")
		return 1
	}
	return emit(*out, m)
}

func runSpace(args []string) int {
	fs := flag.NewFlagSet("space", flag.ContinueOnError)
	cfg := retention.SpaceConfig{}
	fs.StringVar(&cfg.Kind, "kind", "btrfs", "btrfs | dir")
	fs.StringVar(&cfg.System, "system", "unknown", "system label")
	fs.IntVar(&cfg.Epoch, "epoch", 0, "epoch index")
	fs.StringVar(&cfg.State, "state", "current", "current | snapshot")
	fs.StringVar(&cfg.Mountpoint, "mnt", "", "btrfs mount used for `btrfs filesystem usage`")
	fs.StringVar(&cfg.Subvol, "subvol", "", "subvolume to du / logical walk")
	fs.StringVar(&cfg.PhysicalDir, "physical-dir", "", "physical walk root (dir kind; default subvol)")
	out := fs.String("out", "", "receipt path (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := retention.MeasureSpace(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench space: %v\n", err)
		return 1
	}
	return emit(*out, s)
}

func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	cfg := retention.SnapshotConfig{}
	fs.StringVar(&cfg.Live, "live", "", "source subvolume (required)")
	fs.StringVar(&cfg.Dest, "dest", "", "destination snapshot path (required)")
	fs.StringVar(&cfg.System, "system", "Btrfs", "system label")
	fs.IntVar(&cfg.Epoch, "epoch", 0, "epoch index")
	out := fs.String("out", "", "receipt path (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := retention.CreateSnapshot(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench snapshot: %v\n", err)
		return 1
	}
	return emit(*out, s)
}

// emit marshals v to -out (or stdout).
func emit(out string, v any) int {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench: marshal: %v\n", err)
		return 1
	}
	blob = append(blob, '\n')
	if out == "" {
		os.Stdout.Write(blob)
		return 0
	}
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "retentionbench: write receipt: %v\n", err)
		return 1
	}
	return 0
}
