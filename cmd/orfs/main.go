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
	"syscall"

	"github.com/infinityabundance/orfs-go/internal/doctor"
	"github.com/infinityabundance/orfs-go/internal/fsck"
	"github.com/infinityabundance/orfs-go/internal/fusefs"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
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
	case "doctor":
		return cmdDoctor(rest)
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
  doctor [--capacity --backing <dir> [--reserve bytes]] [--json]
  mount <backing> <mountpoint> [flags]
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
	l, err := log.Open(*backing, log.ReadOnly)
	if err != nil {
		return fail(err)
	}
	defer l.Close()
	var reserveOverride *uint64
	if reserveSet {
		reserveOverride = reserve
	}
	st, err := doctor.CapacityStatus(l, doctor.BackingVolumeDisplay(*backing), reserveOverride)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		out, _ := st.JSON()
		fmt.Println(string(out))
	} else {
		fmt.Print(st.Text())
	}
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
	defaultPerms := fs.Bool("default-permissions", false, "delegate permission checks to the kernel")
	directIO := fs.Bool("direct-io", false, "bypass the page cache (FOPEN_DIRECT_IO)")
	attrTTL := fs.Float64("attr-ttl-ms", 1000, "attribute cache TTL in ms")
	entryTTL := fs.Float64("entry-ttl-ms", 1000, "entry cache TTL in ms")
	fsName := fs.String("fsname", "", "filesystem name")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		return fail(orfserr.Invalid("mount requires <backing> <mountpoint>"))
	}
	mode := log.ReadWrite
	if *readOnly {
		mode = log.ReadOnly
	}
	v, err := volume.Open(pos[0], mode)
	if err != nil {
		return fail(err)
	}
	attrTTLSec := *attrTTL / 1000.0
	entryTTLSec := *entryTTL / 1000.0
	srv := fusefs.New(v, fusefs.MountOptions{
		AllowOther:         *allowOther,
		ReadOnly:           *readOnly,
		DefaultPermissions: *defaultPerms,
		DirectIO:           *directIO,
		AttrTTLSeconds:     &attrTTLSec,
		EntryTTLSeconds:    &entryTTLSec,
		FsName:             *fsName,
	})
	server, err := srv.Mount(pos[1])
	if err != nil {
		v.Close()
		return fail(orfserr.IO(err, "mount %s", pos[1]))
	}
	fmt.Printf("mounted %s at %s\n", pos[0], pos[1])

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	server.Unmount()
	v.Close()
	return 0
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
