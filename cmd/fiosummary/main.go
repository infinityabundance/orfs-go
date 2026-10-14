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
)

type fioReport struct {
	Jobs []struct {
		JobName string `json:"jobname"`
		Read    fioDir `json:"read"`
		Write   fioDir `json:"write"`
	} `json:"jobs"`
}

type fioDir struct {
	IOPS float64 `json:"iops"`
	BW   float64 `json:"bw"`
	CLat struct {
		Mean       float64            `json:"mean"`
		Percentile map[string]float64 `json:"percentile"`
	} `json:"clat_ns"`
}

// Summary is the compact receipt for one job.
type Summary struct {
	Job      string  `json:"job"`
	IOPS     float64 `json:"iops"`
	BWKiBps  float64 `json:"bw_kib_per_s"`
	ClatMean float64 `json:"clat_mean_ns"`
	P50      float64 `json:"p50_ns"`
	P99      float64 `json:"p99_ns"`
	P999     float64 `json:"p99_9_ns"`
	P9999    float64 `json:"p99_99_ns"`
}

// Receipt labels each target's summaries.
type Receipt struct {
	Schema  string               `json:"schema"`
	Note    string               `json:"note"`
	Targets map[string][]Summary `json:"targets"`
}

func pick(m map[string]float64, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return 0
}

func summarize(path string) ([]Summary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep fioReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(rep.Jobs))
	for _, j := range rep.Jobs {
		d := j.Read
		if d.IOPS == 0 {
			d = j.Write
		}
		cl := d.CLat.Percentile
		out = append(out, Summary{
			Job:      j.JobName,
			IOPS:     d.IOPS,
			BWKiBps:  d.BW,
			ClatMean: d.CLat.Mean,
			P50:      pick(cl, "50.000000", "50.000000000"),
			P99:      pick(cl, "99.000000", "99.000000000"),
			P999:     pick(cl, "99.900000", "99.900000000"),
			P9999:    pick(cl, "99.990000", "99.990000000"),
		})
	}
	return out, nil
}

func main() {
	schema := flag.String("schema", "orfs.performance.io.v1", "receipt schema")
	out := flag.String("o", "", "output path (default stdout)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: fiosummary [-schema S] [-o out.json] label=path ...")
		os.Exit(2)
	}
	rec := Receipt{Schema: *schema, Note: "honest external comparison; a FUSE tax is expected and is not hidden", Targets: map[string][]Summary{}}
	for _, arg := range flag.Args() {
		label, path := "default", arg
		for i := 0; i < len(arg); i++ {
			if arg[i] == '=' {
				label, path = arg[:i], arg[i+1:]
				break
			}
		}
		sums, err := summarize(path)
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
