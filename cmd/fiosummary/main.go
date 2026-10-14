// Command fiosummary reads one or more fio JSON reports and emits a compact,
// labelled receipt with the completion-latency percentiles a storage engineer
// asks for. It is used by the external-baseline and scaling courts, which must
// publish honest losses.
//
// Usage:
//
//	fiosummary [-schema S] [-o out.json] label=path [label=path ...]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/infinityabundance/orfs-go/internal/fiojson"
)

// Receipt labels each target's summaries.
type Receipt struct {
	Schema  string                       `json:"schema"`
	Note    string                       `json:"note"`
	Targets map[string][]fiojson.Summary `json:"targets"`
}

func main() {
	schema := flag.String("schema", "orfs.performance.io.v1", "receipt schema")
	out := flag.String("o", "", "output path (default stdout)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: fiosummary [-schema S] [-o out.json] label=path ...")
		os.Exit(2)
	}
	rec := Receipt{Schema: *schema, Note: "honest external comparison; a FUSE tax is expected and is not hidden", Targets: map[string][]fiojson.Summary{}}
	for _, arg := range flag.Args() {
		label, path, ok := fiojson.SplitLabel(arg)
		if !ok {
			fmt.Fprintf(os.Stderr, "fiosummary: %s: want label=path\n", arg)
			os.Exit(2)
		}
		sums, err := fiojson.Summarize(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fiosummary: %s: %v\n", label, err)
			os.Exit(1)
		}
		rec.Targets[label] = sums
	}
	blob, _ := json.MarshalIndent(rec, "", "  ")
	blob = append(blob, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "fiosummary: write: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Stdout.Write(blob)
}
