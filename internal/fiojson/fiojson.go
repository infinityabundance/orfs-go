// Package fiojson reads fio JSON reports and turns them into the compact,
// labelled completion-latency summary a storage engineer asks for: IOPS,
// bandwidth, mean completion latency, and the p50 / p95 / p99 / p99.9 / p99.99
// percentiles. It is shared by cmd/fiosummary (the external baseline and
// scaling courts) and cmd/transportsummary (the transport court).
package fiojson

import (
	"encoding/json"
	"fmt"
	"os"
)

type report struct {
	Jobs []job `json:"jobs"`
}

type job struct {
	JobName string `json:"jobname"`
	Read    dir    `json:"read"`
	Write   dir    `json:"write"`
}

type dir struct {
	IOPS float64 `json:"iops"`
	BW   float64 `json:"bw"`
	CLat struct {
		Mean       float64            `json:"mean"`
		Percentile map[string]float64 `json:"percentile"`
	} `json:"clat_ns"`
}

// Summary is the compact receipt for one fio job.
type Summary struct {
	Job      string  `json:"job"`
	IOPS     float64 `json:"iops"`
	BWKiBps  float64 `json:"bw_kib_per_s"`
	ClatMean float64 `json:"clat_mean_ns"`
	P50      float64 `json:"p50_ns"`
	P95      float64 `json:"p95_ns"`
	P99      float64 `json:"p99_ns"`
	P999     float64 `json:"p99_9_ns"`
	P9999    float64 `json:"p99_99_ns"`
}

func pick(m map[string]float64, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return 0
}

// Summarize reads one fio JSON report and returns a Summary per job. A job with
// no read result is summarised from its write result (and vice versa).
func Summarize(path string) ([]Summary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep report
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
			P95:      pick(cl, "95.000000", "95.000000000"),
			P99:      pick(cl, "99.000000", "99.000000000"),
			P999:     pick(cl, "99.900000", "99.900000000"),
			P9999:    pick(cl, "99.990000", "99.990000000"),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no jobs", path)
	}
	return out, nil
}

// SplitLabel splits a "label=path" argument at the first '='.
func SplitLabel(arg string) (label, path string, ok bool) {
	for i := 0; i < len(arg); i++ {
		if arg[i] == '=' {
			return arg[:i], arg[i+1:], true
		}
	}
	return "", "", false
}
