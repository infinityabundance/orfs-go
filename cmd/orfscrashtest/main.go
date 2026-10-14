// Command orfscrashtest is the crash-test writer: it opens an existing volume and
// publishes transactions in a tight loop, taking a durability barrier on each one,
// until it is killed.
//
// It deliberately installs no signal handler and performs no cleanup. A SIGKILL
// must terminate it exactly as a power cut would, leaving whatever partial bytes
// reached the backing store mid-append — that is the whole point of this helper.
// The crash court builds this command and kills it with SIGKILL.
//
// It prints exactly one "READY" line once a configured number of warm-up
// transactions are committed (so the court knows a stable committed root exists
// and every later append begins in a fresh trailing segment), then writes nothing
// more to stdout.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/record"
)

func main() {
	backing := flag.String("backing", "", "volume backing directory (required)")
	readyAfter := flag.Int("ready-after", 1, "committed transactions before printing READY")
	records := flag.Int("records", 1, "INODE_CREATE records per transaction")
	flag.Parse()
	if *backing == "" {
		fmt.Fprintln(os.Stderr, "orfscrashtest: -backing is required")
		os.Exit(2)
	}
	if *records < 1 {
		*records = 1
	}

	l, err := log.Open(*backing, log.ReadWrite)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfscrashtest: open: %v\n", err)
		os.Exit(1)
	}
	// No defer l.Close(), no signal.Notify: the process must be killable at any
	// instant without a graceful shutdown path.
	next := l.MaxTransactionID() + 1
	for i := 1; ; i++ {
		if _, err := l.Publish(crashTx(next, *records)); err != nil {
			fmt.Fprintf(os.Stderr, "orfscrashtest: publish: %v\n", err)
			os.Exit(1)
		}
		next++
		if i == *readyAfter {
			fmt.Println("READY")
		}
	}
}

// crashTx builds one transaction of records INODE_CREATE records. The framed
// length is deterministic, which is what lets the court size segments so each
// committed transaction ends exactly at a segment boundary.
func crashTx(id uint64, records int) []*record.Record {
	recs := make([]*record.Record, 0, records)
	for j := 0; j < records; j++ {
		p := record.InodeCreate{
			InodeID:    id*1000 + uint64(j),
			Generation: 1,
			Kind:       record.KindFile,
			Mode:       0o644,
			Nlink:      1,
		}.Encode()
		recs = append(recs, &record.Record{
			Header:  record.Header{RecordType: record.TypeInodeCreate, PayloadLength: uint64(len(p))},
			Payload: p,
		})
	}
	return recs
}
