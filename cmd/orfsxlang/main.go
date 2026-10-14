// Command orfsxlang is a small, read-only probe used by the cross-language
// compatibility court (scripts/crosslang-court.sh). It reads a volume through
// the Go implementation *without* a FUSE mount, so the court can assert the
// replayed namespace, the record-type histogram, and frame-level byte identity
// deterministically. It never writes: every subcommand opens read-only.
//
// Usage:
//
//	orfsxlang dump <backing>       # replay and print the namespace + type histogram
//	orfsxlang types <backing>      # print the committed record-type histogram
//	orfsxlang roundtrip <backing>  # decode+re-encode every committed frame, byte-exact
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/segment"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: orfsxlang {dump|types|roundtrip} <backing>")
		return 2
	}
	cmd, backing := args[0], args[1]
	var err error
	switch cmd {
	case "dump":
		err = dump(backing)
	case "types":
		err = types(backing)
	case "roundtrip":
		err = roundtrip(backing)
	default:
		fmt.Fprintf(os.Stderr, "orfsxlang: unknown command %q\n", cmd)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfsxlang: %v\n", err)
		return 1
	}
	return 0
}

// root is a read-only caller: uid 0 bypasses the permission checks, so the probe
// observes the namespace rather than returning an access verdict.
var root = namespace.Credential{UID: 0}

func dump(backing string) error {
	scan, err := log.ScanLog(log.Dir(backing))
	if err != nil {
		return err
	}
	v, err := volume.Open(backing, log.ReadOnly)
	if err != nil {
		return err
	}
	defer v.Close()
	ns := namespace.New(v)

	fmt.Printf("committed_root=%d\n", scan.CommittedRoot)
	fmt.Printf("committed_records=%d\n", scan.CommittedRecordCount)
	fmt.Printf("max_transaction_id=%d\n", scan.MaxTransactionID)
	for _, tc := range sortedTypes(scan.ByType) {
		fmt.Printf("type=%s count=%d\n", record.TypeName(tc.code), tc.count)
	}
	return walk(ns, record.RootInode, "")
}

// walk prints a directory subtree in ascending name order. Every line is one
// inode, keyed by its absolute path, with a content digest for regular files.
func walk(ns *namespace.Namespace, id uint64, prefix string) error {
	if id == record.RootInode {
		in, err := ns.Getattr(id)
		if err != nil {
			return err
		}
		line, err := inodeLine(ns, "/", in)
		if err != nil {
			return err
		}
		fmt.Println(line)
	}
	entries, err := ns.Readdir(root, id)
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := prefix + "/" + e.Name
		in, err := ns.Getattr(e.Child)
		if err != nil {
			return err
		}
		line, err := inodeLine(ns, path, in)
		if err != nil {
			return err
		}
		fmt.Println(line)
		if in.Kind == record.KindDir {
			if err := walk(ns, in.ID, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func inodeLine(ns *namespace.Namespace, path string, in *index.Inode) (string, error) {
	s := fmt.Sprintf("%s %s mode=%04o nlink=%d size=%d", path, kindName(in.Kind), in.Mode&0o7777, in.Nlink, in.Size)
	switch in.Kind {
	case record.KindSymlink:
		s += " target=" + string(in.Target)
	case record.KindFile:
		b, err := ns.ReadAll(root, in.ID)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		s += " sha256=" + hex.EncodeToString(sum[:])
	}
	if len(in.Xattrs) > 0 {
		names := make([]string, 0, len(in.Xattrs))
		for k := range in.Xattrs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			s += fmt.Sprintf(" xattr:%s=%s", k, hex.EncodeToString(in.Xattrs[k]))
		}
	}
	return s, nil
}

func kindName(k uint8) string {
	switch k {
	case record.KindDir:
		return "dir"
	case record.KindSymlink:
		return "symlink"
	case record.KindFifo:
		return "fifo"
	case record.KindChar:
		return "chardev"
	case record.KindBlock:
		return "blockdev"
	case record.KindSocket:
		return "socket"
	default:
		return "file"
	}
}

type typeCount struct {
	code  uint16
	count uint64
}

func sortedTypes(m map[uint16]uint64) []typeCount {
	out := make([]typeCount, 0, len(m))
	for code, count := range m {
		out = append(out, typeCount{code, count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].code < out[j].code })
	return out
}

// types prints the committed record-type histogram, flagging every code that is
// outside the registry the Rust reference understands (0x0001..0x0015).
func types(backing string) error {
	scan, err := log.ScanLog(log.Dir(backing))
	if err != nil {
		return err
	}
	for _, tc := range sortedTypes(scan.ByType) {
		known := "rust_known"
		if tc.code > 0x0015 || tc.code&record.OptionalTypeBit != 0 {
			known = "not_in_rust_registry"
		}
		fmt.Printf("0x%04X %s count=%d %s\n", tc.code, record.TypeName(tc.code), tc.count, known)
	}
	return nil
}

// roundtrip proves frame-level byte identity: it reads the committed byte prefix
// (the canonical bytes) and, for every framed record, decodes it and re-encodes
// it, requiring the re-encoding to equal the on-disk frame byte-for-byte. This
// makes the Go codec, not merely its logical replay, byte-compatible with
// whatever implementation wrote the volume.
func roundtrip(backing string) error {
	scan, err := log.ScanLog(log.Dir(backing))
	if err != nil {
		return err
	}
	canonical, err := committedBytes(backing, scan)
	if err != nil {
		return err
	}
	ra := bytes.NewReader(canonical)
	var off int64
	var records int
	for off < int64(len(canonical)) {
		rec, raw, total, err := record.ReadFrameRawAt(ra, off, int64(len(canonical)))
		if err != nil {
			return fmt.Errorf("read frame at %d: %w", off, err)
		}
		re, err := rec.Encode()
		if err != nil {
			return fmt.Errorf("re-encode frame at %d: %w", off, err)
		}
		if !bytes.Equal(re, raw) {
			return fmt.Errorf("frame at offset %d is not byte-identical after decode+re-encode (type %s)", off, record.TypeName(rec.Header.RecordType))
		}
		off += total
		records++
	}
	fmt.Printf("roundtrip: records=%d committed_root=%d byte_identical=true\n", records, scan.CommittedRoot)
	return nil
}

// committedBytes returns the canonical committed prefix [0, committed_root): the
// concatenation of the valid bytes of each canonical segment extent.
func committedBytes(backing string, scan *log.ScanResult) ([]byte, error) {
	dir := log.Dir(backing)
	var out []byte
	remaining := scan.CommittedRoot
	for _, e := range scan.Extents {
		if remaining == 0 {
			break
		}
		take := e.ValidLen
		if take > remaining {
			take = remaining
		}
		seg, err := segment.OpenReadOnly(dir, e.ID)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, take)
		if _, err := seg.ReadAt(buf, 0); err != nil {
			seg.Close()
			return nil, fmt.Errorf("read segment %d: %w", e.ID, err)
		}
		seg.Close()
		out = append(out, buf...)
		remaining -= take
		if take < e.ValidLen {
			break
		}
	}
	if remaining != 0 {
		return nil, fmt.Errorf("committed root %d extends past the canonical bytes", scan.CommittedRoot)
	}
	return out, nil
}
