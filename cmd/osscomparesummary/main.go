// Command osscomparesummary merges the per-arm run receipts written by
// cmd/orfscompare into the open-source head-to-head receipt
// evidence/oss-compare.json (schema orfs.oss-compare.v1).
//
// It emits one row per {system, workload, writers, readers, block_size}, records
// the set of systems it merged, embeds the comparability block verbatim, and
// refuses to merge two rows with the same comparable coordinate. It can also be
// told which systems must be present (-expect-systems) and fails if one produced
// no row, so a claimed third system is never silently omitted. It never ranks
// rows against each other: the receipt is data, not a leaderboard.
//
// Usage:
//
//	osscomparesummary -comparability c.json [-expect-systems A,B,C] -o out.json run.json [run.json ...]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/infinityabundance/orfs-go/internal/osscompare"
)

func main() {
	comparability := flag.String("comparability", "", "comparability block JSON (verbatim)")
	expectSystems := flag.String("expect-systems", "", "comma-separated system labels that must each contribute at least one row")
	out := flag.String("o", "", "output path (default stdout)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: osscomparesummary -comparability c.json [-expect-systems A,B,C] -o out.json run.json ...")
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

	// No silent holes: a system the court claimed to measure must have produced at
	// least one row, or the merge refuses to publish.
	if missing := checkExpectedSystems(runs, *expectSystems); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "osscomparesummary: expected system(s) produced no rows: %s\n", strings.Join(missing, ", "))
		os.Exit(1)
	}

	rec, err := osscompare.Merge(runs, comp, time.Now().Unix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "osscomparesummary: %v\n", err)
		os.Exit(1)
	}
	rec.Notes = []string{
		"Every row is one bounded arm of the shared driver (cmd/orfscompare) against a mounted filesystem on this machine.",
		"Read the comparability block before comparing any two rows; a row that could not run the same way is recorded as a caveat, not a pass or a parity claim. not_comparable lists the axes that must not be ranked.",
		"The receipt deliberately carries no ranking: two rows are comparable only at the same {system, workload, writers, readers, block_size} and under the listed configuration differences.",
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

// splitCSV splits a comma-separated list, trimming surrounding spaces and
// dropping empty entries.
func splitCSV(s string) []string {
	out := make([]string, 0)
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkExpectedSystems returns the expected system labels (from a comma-separated
// list) that produced no row. An empty list checks nothing.
func checkExpectedSystems(runs []osscompare.Run, csv string) []string {
	return osscompare.MissingSystems(runs, splitCSV(csv))
}
