// Command orfs is the OrFS-GO CLI.
//
// The CLI is honest: a subcommand that is part of the design but not yet wired
// rejects the request with its owning phase rather than pretending to work.
// Successful commands exit 0; a failure prints "orfs: <error>" to stderr and
// exits 1; a usage error exits 2.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/infinityabundance/orfs-go/internal/doctor"
	"github.com/infinityabundance/orfs-go/internal/fsck"
	"github.com/infinityabundance/orfs-go/internal/fusefs"
	"github.com/infinityabundance/orfs-go/internal/gcruntime"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/metrics"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/observe"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/transport"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "init":
		return cmdInit(rest)
	case "inspect":
		return cmdInspect(rest)
	case "verify":
		return cmdVerify(rest)
	case "recover":
		return cmdRecover(rest)
	case "checkpoint":
		return cmdCheckpoint(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "metrics":
		return cmdMetrics(rest)
	case "mount":
		return cmdMount(rest)
	case "unmount":
		return cmdUnmount(rest)
	case "stat":
		return cmdStat(rest)
	case "manifest":
		return cmdManifest(rest)
	case "grow":
		return fail(orfserr.Unimplemented("8", "concealed manifestation growth"))
	case "query":
		return fail(orfserr.Unimplemented("52", "the global search directory"))
	case "snapshot":
		return fail(orfserr.Unimplemented("20", "named historical roots"))
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "orfs: unknown command %q\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "orfs: %v\n", err)
	return 1
}

func usage(w *os.File) {
	fmt.Fprint(w, `orfs — non-withdrawal, concealed-expansion filesystem (Go)

usage: orfs <command> [flags]

  init <backing> [--segment-size bytes] [--reserve bytes]
  inspect <backing> [--json]
  verify <backing> [--deep] [--json]
  recover <backing> [--json]
  checkpoint <backing> [--json]
  doctor [--capacity --backing <dir> [--reserve bytes]] [--json]
  metrics <backing> [--json] [--skip-verify]
  mount <backing> <mountpoint> [--direct-io] [--transport f0|f0-single|fuse-uring] [--readers N] [--compat] [--profile-dir dir]
  unmount <mountpoint>
  stat <backing> [--json]

or.log is the source of truth; everything else is derived.
`)
}

// parseFlags parses flags interspersed with positional arguments: Go's flag
// package stops at the first positional, so we peel positionals one at a time
// and keep parsing.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func cmdInit(args []string) int {
	fs := newFlagSet("init")
	segSize := fs.Uint64("segment-size", record.DefaultSegmentSize, "segment size in bytes")
	reserve := fs.Uint64("reserve", 0, "backing bytes to keep free")
	uuidHex := fs.String("uuid", "", "fixed 32-hex-digit volume uuid (default random); for reproducible bytes")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("init requires exactly one <backing> directory"))
	}
	opts := volume.InitOptions{SegmentSize: *segSize, Reserve: *reserve}
	if *uuidHex != "" {
		raw, err := hex.DecodeString(*uuidHex)
		if err != nil || len(raw) != 16 {
			return fail(orfserr.Invalid("uuid must be 32 hex digits"))
		}
		var u [16]byte
		copy(u[:], raw)
		opts.UUID = &u
	}
	v, err := volume.Init(pos[0], opts)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	fmt.Printf("initialised volume %s\n", pos[0])
	fmt.Printf("  uuid:          %x\n", v.Meta.UUID)
	fmt.Printf("  segment size:  %d\n", v.Meta.SegmentSize)
	fmt.Printf("  genesis root:  %d\n", v.Log.CommittedRoot())
	return 0
}

func cmdInspect(args []string) int {
	fs := newFlagSet("inspect")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("inspect requires exactly one <backing> directory"))
	}
	v, err := volume.Open(pos[0], log.ReadOnly)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	phys, _ := v.Log.PhysicalBytes()
	if *asJSON {
		rep, err := fsck.Verify(pos[0], false)
		if err != nil {
			return fail(err)
		}
		out, _ := rep.JSON()
		fmt.Println(string(out))
		return 0
	}
	fmt.Printf("volume:            %s\n", pos[0])
	fmt.Printf("uuid:              %x\n", v.Meta.UUID)
	fmt.Printf("log format ver:    %d\n", v.Log.Genesis().FormatVersion)
	fmt.Printf("committed root:    %d\n", v.Log.CommittedRoot())
	fmt.Printf("committed records: %d\n", v.Log.CommittedRecordCount())
	fmt.Printf("segments:          %d\n", len(v.Log.Extents()))
	fmt.Printf("physical bytes:    %d\n", phys)
	fmt.Printf("inodes:            %d\n", len(v.Index().Inodes))
	fmt.Printf("dentries:          %d\n", len(v.Index().Dentry))
	if rep := v.MountReport(); rep != nil {
		fmt.Printf("index source:      %s\n", rep.DiscoverySource)
		if rep.UsedCheckpoint {
			fmt.Printf("checkpoint root:   %d\n", rep.CheckpointRoot)
		}
		fmt.Printf("suffix replayed:   %d byte(s) / %d record(s)\n", rep.BytesReplayed, rep.RecordsReplayed)
	}
	return 0
}

// cmdCheckpoint writes an index checkpoint over the current committed index as a
// normal committed transaction, so a later open can bound its replay to the
// suffix after it.
func cmdCheckpoint(args []string) int {
	fs := newFlagSet("checkpoint")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("checkpoint requires exactly one <backing> directory"))
	}
	v, err := volume.Open(pos[0], log.ReadWrite)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	root, err := v.TakeCheckpoint()
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		fmt.Printf("{\"schema\":\"orfs.checkpoint.v1\",\"root\":%d,\"inodes\":%d,\"dentries\":%d}\n",
			root, len(v.Index().Inodes), len(v.Index().Dentry))
		return 0
	}
	fmt.Printf("checkpoint committed at root %d\n", root)
	fmt.Printf("  index inodes:   %d\n", len(v.Index().Inodes))
	fmt.Printf("  index dentries: %d\n", len(v.Index().Dentry))
	return 0
}

func cmdVerify(args []string) int {
	fs := newFlagSet("verify")
	deep := fs.Bool("deep", false, "validate brackets, replay, and cross-check")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("verify requires exactly one <backing> directory"))
	}
	rep, err := fsck.Verify(pos[0], *deep)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		out, _ := rep.JSON()
		fmt.Println(string(out))
	} else {
		fmt.Print(rep.Text())
	}
	if !rep.Passed() {
		return 1
	}
	return 0
}

func cmdRecover(args []string) int {
	fs := newFlagSet("recover")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("recover requires exactly one <backing> directory"))
	}
	// Scan first so a scan failure is reported honestly: recovery cannot invent
	// data.
	scan, err := log.ScanLog(log.Dir(pos[0]))
	if err != nil {
		return fail(err)
	}
	v, err := volume.Open(pos[0], log.ReadWrite)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	disp := v.Log.Recovery()
	if *asJSON {
		fmt.Printf("{\"recovered_root\":%d,\"committed_records\":%d,\"quarantined\":%v,\"tail_bytes\":%d}\n",
			v.Log.CommittedRoot(), v.Log.CommittedRecordCount(), disp.Quarantined, tailBytes(scan))
		return 0
	}
	fmt.Printf("recovered root:    %d\n", v.Log.CommittedRoot())
	fmt.Printf("committed records: %d\n", v.Log.CommittedRecordCount())
	if scan.Tail == nil {
		fmt.Println("abandoned tail:    none; nothing to quarantine")
	} else {
		fmt.Printf("abandoned tail:    %d byte(s) (%s) at segment %d\n",
			scan.Tail.AbandonedBytes, scan.Tail.Kind, scan.Tail.SegmentID)
		if disp.Quarantined {
			fmt.Printf("                   retained under %s/%s/ and continued in a fresh segment\n",
				log.DirName, segment.QuarantineDir)
		} else {
			fmt.Printf("                   retained in place (began inside the committed segment)\n")
		}
	}
	return 0
}

func tailBytes(scan *log.ScanResult) uint64 {
	if scan.Tail == nil {
		return 0
	}
	return scan.Tail.AbandonedBytes
}

func cmdDoctor(args []string) int {
	fs := newFlagSet("doctor")
	capacityFlag := fs.Bool("capacity", false, "emit a capacity report")
	backing := fs.String("backing", "", "backing directory (with --capacity)")
	reserve := fs.Uint64("reserve", 0, "reserve bytes override (with --capacity)")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	_ = pos
	reserveSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "reserve" {
			reserveSet = true
		}
	})
	if !*capacityFlag {
		p := doctor.CapabilityProbe()
		if *asJSON {
			out, _ := p.JSON()
			fmt.Println(string(out))
		} else {
			fmt.Print(p.Text())
		}
		return 0
	}
	if *backing == "" {
		return fail(orfserr.Invalid("doctor --capacity requires --backing <dir>"))
	}
	vol, err := volume.Open(*backing, log.ReadOnly)
	if err != nil {
		return fail(err)
	}
	defer vol.Close()
	var reserveOverride *uint64
	if reserveSet {
		reserveOverride = reserve
	}
	st, err := doctor.CapacityStatus(vol.Log, doctor.BackingVolumeDisplay(*backing), reserveOverride)
	if err != nil {
		return fail(err)
	}
	if snap, err := observe.Collect(vol, observe.Options{}); err == nil {
		st.MergeMetrics(snap)
	}
	if *asJSON {
		out, _ := st.JSON()
		fmt.Println(string(out))
	} else {
		fmt.Print(st.Text())
	}
	return 0
}

// cmdMetrics renders the Prometheus exposition for a volume. It runs a
// verify --deep pass by default (the source of orfs_verify_deep_failures);
// --skip-verify omits that pass. Runtime counters that a read-only inspection
// cannot observe come from the snapshot the last mounted daemon persisted, or
// else are reported as an explicit 0.
func cmdMetrics(args []string) int {
	fs := newFlagSet("metrics")
	asJSON := fs.Bool("json", false, "emit JSON (schema orfs.metrics.v1)")
	skipVerify := fs.Bool("skip-verify", false, "do not run a verify --deep pass")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("metrics requires exactly one <backing> directory"))
	}
	v, err := volume.Open(pos[0], log.ReadOnly)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	var rep *fsck.Report
	if !*skipVerify {
		r, err := fsck.Verify(pos[0], true)
		if err != nil {
			return fail(err)
		}
		rep = r
	}
	snap, err := observe.Collect(v, observe.Options{Verify: rep})
	if err != nil {
		return fail(err)
	}
	set := snap.Set()
	if *asJSON {
		out, err := set.JSON()
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(out))
		return 0
	}
	fmt.Print(set.Text())
	return 0
}

func cmdManifest(args []string) int {
	fs := newFlagSet("manifest")
	inode := fs.Uint64("inode", 0, "inode id to manifest (required)")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || *inode == 0 {
		return fail(orfserr.Invalid("manifest requires <backing> --inode <id>"))
	}
	v, err := volume.Open(pos[0], log.ReadWrite)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	ns := namespace.New(v)
	content, err := ns.ReadAll(namespace.Credential{}, *inode)
	if err != nil {
		return fail(err)
	}
	in, err := ns.Getattr(*inode)
	if err != nil {
		return fail(err)
	}
	m, err := manifest.Publish(v, *inode, in.Version, v.Log.CommittedRoot(), content)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		out, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	fmt.Printf("manifested inode %d v%d at root %d\n", m.Inode, m.Version, m.Root)
	fmt.Printf("  logical bytes:  %d\n", m.LogicalLen)
	fmt.Printf("  physical bytes: %d\n", m.PhysicalBytes)
	if m.LogicalLen > 0 {
		fmt.Printf("  amplification:  %.2fx\n", float64(m.PhysicalBytes)/float64(m.LogicalLen))
	}
	return 0
}

func cmdMount(args []string) int {
	fs := newFlagSet("mount")
	readOnly := fs.Bool("read-only", false, "mount read-only")
	allowOther := fs.Bool("allow-other", false, "allow other users")
	directIO := fs.Bool("direct-io", false, "bypass the page cache (FOPEN_DIRECT_IO)")
	transportName := fs.String("transport", string(transport.F0), "f0 | f0-single | fuse-uring")
	readers := fs.Int("readers", 0, "F0 reader concurrency (0 = default; go-fuse clamps to [2,16] via GOMAXPROCS)")
	attrTTL := fs.Float64("attr-ttl-ms", 1000, "attribute cache TTL in ms")
	entryTTL := fs.Float64("entry-ttl-ms", 1000, "entry cache TTL in ms")
	fsName := fs.String("fsname", "", "filesystem name")
	profileDir := fs.String("profile-dir", "", "dump runtime-metrics.json + pprof profiles here on shutdown and on SIGUSR1 (Go-runtime court)")
	profileLocks := fs.Bool("profile-locks", false, "also enable mutex/block profiling and write mutex.prof/block.prof (perturbs latency; for the lock-profile phase)")
	compat := fs.Bool("compat", volume.EnvironCompat(), "Rust-reference compatibility profile: new writes use only the reference's registry and semantics (no CTIME_UPDATE, no special files)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		return fail(orfserr.Invalid("mount requires <backing> <mountpoint>"))
	}
	tmode, err := transport.Parse(*transportName)
	if err != nil {
		return fail(orfserr.Invalid("%v", err))
	}
	// fuse-uring (F2) is negotiated, never assumed: Plan falls back to f0 with
	// the exact reason when the transport is not available. A loud fallback,
	// never a silent downgrade.
	serving, err := transport.Plan(tmode, *readers)
	if err != nil {
		return fail(orfserr.Invalid("%v", err))
	}
	if serving.Fallback {
		fmt.Fprintf(os.Stderr, "orfs: transport %s unavailable: %s\n", tmode, serving.Reason)
		fmt.Fprintf(os.Stderr, "orfs: falling back to %s\n", serving.Mode)
	}
	if serving.GOMAXPROCS > 0 {
		runtime.GOMAXPROCS(serving.GOMAXPROCS)
	}
	mode := log.ReadWrite
	if *readOnly {
		mode = log.ReadOnly
	}
	var vopts []volume.Option
	vopts = append(vopts, volume.WithCompatProfile(*compat))
	v, err := volume.Open(pos[0], mode, vopts...)
	if err != nil {
		return fail(err)
	}
	attrTTLSec := *attrTTL / 1000.0
	entryTTLSec := *entryTTL / 1000.0
	srv := fusefs.New(v, fusefs.MountOptions{
		AllowOther:      *allowOther,
		ReadOnly:        *readOnly,
		DirectIO:        *directIO,
		SingleThreaded:  serving.SingleThreaded,
		AttrTTLSeconds:  &attrTTLSec,
		EntryTTLSeconds: &entryTTLSec,
		FsName:          *fsName,
	})
	server, err := srv.Mount(pos[1])
	if err != nil {
		v.Close()
		return fail(orfserr.IO(err, "mount %s", pos[1]))
	}
	fmt.Printf("mounted %s at %s (transport %s)\n", pos[0], pos[1], serving.Mode)

	// Optional Go-runtime instrumentation for the runtime court. It is armed
	// before any workload so the window brackets the whole serving interval. The
	// start snapshot is taken here; the end snapshot and the pprof profiles are
	// written on shutdown and on SIGUSR1, so a workload driver can request a dump
	// without stopping the daemon.
	var profileDump func()
	if *profileDir != "" {
		if *profileLocks {
			gcruntime.EnableLockProfiles()
		}
		start := gcruntime.Capture()
		profileDump = func() {
			end := gcruntime.Capture()
			err := gcruntime.WriteProfileDir(*profileDir, start, end, gcruntime.Options{
				Locks: *profileLocks,
				Note:  "start = the moment the mount began serving; end = this dump. Delta windows the cumulative runtime metrics over that interval.",
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "orfs: profile dump failed: %v\n", err)
			}
		}
		usr1 := make(chan os.Signal, 1)
		signal.Notify(usr1, syscall.SIGUSR1)
		go func() {
			for range usr1 {
				profileDump()
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	server.Unmount()
	if profileDump != nil {
		profileDump()
	}
	// Persist the process-scoped metrics snapshot (derived, non-authoritative) so
	// an offline `orfs metrics` can report this run's barrier latency and batch
	// counters. Deleting the sidecar changes nothing recoverable.
	writeMetricsSnapshot(v, pos[0])
	v.Close()
	return 0
}

// writeMetricsSnapshot persists the live runtime counters beside volume.meta. A
// failure is reported but never changes the authoritative log.
func writeMetricsSnapshot(v *volume.Volume, backing string) {
	snap := metrics.Snapshot{
		Schema:     metrics.SnapshotSchema,
		UUID:       fmt.Sprintf("%x", v.Meta.UUID),
		CapturedAt: uint64(time.Now().Unix()),
		Runtime:    v.Log.Runtime(),
	}
	if err := metrics.SaveSnapshot(backing, snap); err != nil {
		fmt.Fprintf(os.Stderr, "orfs: could not persist metrics snapshot: %v\n", err)
	}
}

func cmdUnmount(args []string) int {
	fs := newFlagSet("unmount")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("unmount requires <mountpoint>"))
	}
	mnt := pos[0]
	// Prefer the userspace helper (works for unprivileged mounts); fall back to
	// the umount2 syscall (needs CAP_SYS_ADMIN).
	for _, helper := range []string{"fusermount3", "fusermount"} {
		path, err := exec.LookPath(helper)
		if err != nil {
			continue
		}
		if err := exec.Command(path, "-u", mnt).Run(); err == nil {
			return 0
		}
	}
	if err := syscall.Unmount(mnt, syscall.MNT_DETACH); err != nil {
		return fail(orfserr.IO(err, "unmount %s", mnt))
	}
	return 0
}

func cmdStat(args []string) int {
	fs := newFlagSet("stat")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(orfserr.Invalid("stat requires <backing>"))
	}
	v, err := volume.Open(pos[0], log.ReadOnly)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	phys, _ := v.Log.PhysicalBytes()
	if *asJSON {
		fmt.Printf("{\"schema\":\"orfs.stat.v1\",\"log_bytes\":%d,\"logical_bytes\":%d}\n", phys, v.Log.CommittedRoot())
		return 0
	}
	fmt.Printf("authoritative log bytes: %d\n", phys)
	fmt.Printf("committed (logical):     %d\n", v.Log.CommittedRoot())
	return 0
}
