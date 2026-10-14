// Command transportsummary merges the FUSE-over-io_uring negotiation probe with
// per-arm fio 4K-randread measurements into the transport receipt
// (schema orfs.performance.transport.v1).
//
// Usage:
//
//	transportsummary [-schema S] -probe probe.json -o out.json \
//	    -arm mode/numjobs=fio.json ... [-counter mode/numjobs=counters.txt ...]
//
// A counter file holds "key=value" lines for the kernel FUSE connection:
// max_background, congestion_threshold, waiting_max, waiting_mean, samples.
// SQ/CQ counters are deliberately absent: the io_uring transport is not in use.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/infinityabundance/orfs-go/internal/fiojson"
)

// Arm is one transport/concurrency measurement.
type Arm struct {
	Mode    string `json:"mode"`
	NumJobs int    `json:"numjobs"`
	fiojson.Summary
}

// Counters are the kernel FUSE backpressure signals observed during an arm.
type Counters struct {
	Mode                string   `json:"mode"`
	NumJobs             int      `json:"numjobs"`
	MaxBackground       *int     `json:"max_background,omitempty"`
	CongestionThreshold *int     `json:"congestion_threshold,omitempty"`
	WaitingMax          *int     `json:"waiting_max,omitempty"`
	WaitingMean         *float64 `json:"waiting_mean,omitempty"`
	Samples             *int     `json:"samples,omitempty"`
}

// Receipt is the transport court's receipt.
type Receipt struct {
	Schema   string          `json:"schema"`
	Note     string          `json:"note"`
	Probe    json.RawMessage `json:"probe,omitempty"`
	Arms     []Arm           `json:"arms"`
	Counters []Counters      `json:"counters,omitempty"`
	Notes    []string        `json:"notes"`
}

type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func splitModeJobs(label string) (string, int, error) {
	i := strings.LastIndex(label, "/")
	if i < 0 {
		return "", 0, fmt.Errorf("label %q: want mode/numjobs", label)
	}
	mode := label[:i]
	nj, err := strconv.Atoi(label[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("label %q: numjobs: %v", label, err)
	}
	return mode, nj, nil
}

func main() {
	schema := flag.String("schema", "orfs.performance.transport.v1", "receipt schema")
	probePath := flag.String("probe", "", "fuseuringprobe JSON receipt")
	out := flag.String("o", "", "output path (default stdout)")
	var armArgs, counterArgs repeatable
	flag.Var(&armArgs, "arm", "mode/numjobs=fio.json (repeatable)")
	flag.Var(&counterArgs, "counter", "mode/numjobs=counters.txt (repeatable)")
	flag.Parse()

	rec := Receipt{
		Schema: *schema,
		Note:   "honest F0-vs-alternative transport comparison; a transport that is not available is named, never faked",
		Notes: []string{
			"SQ/CQ counters were not obtained: the FUSE-over-io_uring transport is not in use in this build (see probe).",
			"kernel backpressure signals come from /sys/fs/fuse/connections/<id>/{max_background,congestion_threshold,waiting}.",
		},
	}

	if *probePath != "" {
		raw, err := os.ReadFile(*probePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "transportsummary: probe: %v\n", err)
			os.Exit(1)
		}
		rec.Probe = json.RawMessage(raw)
	}

	for _, arg := range armArgs {
		label, path, ok := fiojson.SplitLabel(arg)
		if !ok {
			fmt.Fprintf(os.Stderr, "transportsummary: -arm %q: want mode/numjobs=path\n", arg)
			os.Exit(2)
		}
		mode, nj, err := splitModeJobs(label)
		if err != nil {
			fmt.Fprintf(os.Stderr, "transportsummary: -arm %q: %v\n", arg, err)
			os.Exit(2)
		}
		sums, err := fiojson.Summarize(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "transportsummary: -arm %s: %v\n", label, err)
			os.Exit(1)
		}
		for _, s := range sums {
			rec.Arms = append(rec.Arms, Arm{Mode: mode, NumJobs: nj, Summary: s})
		}
	}
	if len(rec.Arms) == 0 {
		fmt.Fprintln(os.Stderr, "transportsummary: no arms")
		os.Exit(2)
	}

	for _, arg := range counterArgs {
		label, path, ok := fiojson.SplitLabel(arg)
		if !ok {
			fmt.Fprintf(os.Stderr, "transportsummary: -counter %q: want mode/numjobs=path\n", arg)
			os.Exit(2)
		}
		mode, nj, err := splitModeJobs(label)
		if err != nil {
			fmt.Fprintf(os.Stderr, "transportsummary: -counter %q: %v\n", arg, err)
			os.Exit(2)
		}
		c, err := parseCounters(path)
		if err != nil {
			// A missing/empty counter file is not fatal: absence is reported.
			fmt.Fprintf(os.Stderr, "transportsummary: NOTE: -counter %s: %v\n", label, err)
			rec.Notes = append(rec.Notes, fmt.Sprintf("counters for %s: %v", label, err))
			continue
		}
		c.Mode, c.NumJobs = mode, nj
		rec.Counters = append(rec.Counters, c)
	}

	blob, _ := json.MarshalIndent(rec, "", "  ")
	blob = append(blob, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "transportsummary: write: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Stdout.Write(blob)
}

func parseCounters(path string) (Counters, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Counters{}, err
	}
	var c Counters
	set := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := fiojson.SplitLabel(line)
		if !ok {
			continue
		}
		switch k {
		case "max_background":
			if n, err := strconv.Atoi(v); err == nil {
				c.MaxBackground = &n
				set++
			}
		case "congestion_threshold":
			if n, err := strconv.Atoi(v); err == nil {
				c.CongestionThreshold = &n
				set++
			}
		case "waiting_max":
			if n, err := strconv.Atoi(v); err == nil {
				c.WaitingMax = &n
				set++
			}
		case "waiting_mean":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				c.WaitingMean = &f
				set++
			}
		case "samples":
			if n, err := strconv.Atoi(v); err == nil {
				c.Samples = &n
			}
		}
	}
	if set == 0 {
		return Counters{}, fmt.Errorf("no known counters in %s", path)
	}
	return c, nil
}
