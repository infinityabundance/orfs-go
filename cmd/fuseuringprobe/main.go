// Command fuseuringprobe attempts the FUSE INIT handshake against the running
// kernel, once as a control and once requesting FUSE_OVER_IO_URING, and prints
// exactly what the kernel did. It exists so the F2 transport question is
// answered by observation, not by a promise.
//
// It must run as root with /dev/fuse and CAP_SYS_ADMIN (the pinned court
// container supplies both). It performs no io_uring syscalls; it records the
// negotiation, which is what decides reachability, and whether a follow-up
// request still arrives on the plain /dev/fuse path.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/infinityabundance/orfs-go/internal/transport"

	// Link the FUSE library so debug.ReadBuildInfo reports its pinned version in
	// the probe receipt; internal/transport names the library but does not link
	// it.
	_ "github.com/hanwen/go-fuse/v2/fuse"
)

// Report is the receipt this probe writes.
type Report struct {
	Schema      string                 `json:"schema"`
	Support     transport.Support      `json:"support"`
	Negotiation *transport.Negotiation `json:"negotiation"`
}

func main() {
	out := flag.String("o", "", "output path (default stdout)")
	timeout := flag.Duration("timeout", 3*time.Second, "per-read timeout")
	flag.Parse()

	rep := Report{Schema: "orfs.transport.probe.v1", Support: transport.Probe()}

	mnt, err := os.MkdirTemp("", "orfs-transport-probe-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuseuringprobe: mktemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(mnt)

	neg, err := transport.Negotiate(mnt, *timeout)
	rep.Negotiation = neg

	blob, _ := json.MarshalIndent(rep, "", "  ")
	blob = append(blob, '\n')
	if *out != "" {
		if werr := os.WriteFile(*out, blob, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "fuseuringprobe: write: %v\n", werr)
			os.Exit(1)
		}
	} else {
		os.Stdout.Write(blob)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuseuringprobe: negotiation: %v\n", err)
		os.Exit(1)
	}
}
