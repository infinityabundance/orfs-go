// Command osscomparesummary merges the per-arm run receipts written by
// cmd/orfscompare into the open-source head-to-head receipt
// evidence/oss-compare.json (schema orfs.oss-compare.v1).
//
// It emits one row per {system, workload, writers, readers, block_size}, embeds
// the comparability block verbatim, and refuses to merge two rows with the same
// comparable coordinate. It never ranks rows against each other: the receipt is
// data, not a leaderboard.
//
// Usage:
//
//	osscomparesummary -comparability c.json -o out.json run.json [run.json ...]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/infinityabundance/orfs-go/internal/osscompare"
)

func main() {
	comparability := flag.String("comparability", "", "comparability block JSON (verbatim)")
	out := flag.String("o", "", "output path (default stdout)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: osscomparesummary -comparability c.json -o out.json run.json ...")
		os.Exit(2)
	}

	var comp json.RawMessage
	if *comparability != "" {
		raw, err := os.ReadFile(*comparability)
		if err != nil {
			fmt.Fprintf(os.Stderr, "osscomparesummary: comparability: %v\n", err)
			os.Exit(1)
		}
		if !json.Valid(raw) {
			fmt.Fprintf(os.Stderr, "osscomparesummary: comparability %s is not valid JSON\n", *comparability)
			os.Exit(1)
		}
		comp = json.RawMessage(raw)
	}

	runs := make([]osscompare.Run, 0, flag.NArg())
	for _, path := range flag.Args() {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "osscomparesummary: %s: %v\n", path, err)
			os.Exit(1)
		}
		var run osscompare.Run
		if err := json.Unmarshal(raw, &run); err != nil {
			fmt.Fprintf(os.Stderr, "osscomparesummary: %s: %v\n", path, err)
			os.Exit(1)
		}
		runs = append(runs, run)
	}

	rec, err := osscompare.Merge(runs, comp, time.Now().Unix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "osscomparesummary: %v\n", err)
		os.Exit(1)
	}
	rec.Notes = []string{
		"Every row is one bounded arm of the shared driver (cmd/orfscompare) against a mounted filesystem on this machine.",
		"Read the comparability block before comparing any two rows; a row that could not run the same way is recorded as a caveat, not a pass or a parity claim.",
		"The receipt deliberately carries no ranking: two rows are comparable only at the same {workload, writers, readers, block_size} and under the listed configuration differences.",
	}

	blob, _ := json.MarshalIndent(rec, "", "  ")
	blob = append(blob, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "osscomparesummary: write: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Stdout.Write(blob)
}
