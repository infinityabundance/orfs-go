// Package metrics implements a small, dependency-free Prometheus text exposition
// for the OrFS operator surface.
//
// The exposition is hand-written — `# HELP`, `# TYPE`, and `name{labels} value`
// lines — with no third-party client. Nothing here invents a number: a family is
// emitted only when it is backed by a real measurement or a real counter, and a
// runtime counter that a read-only inspection cannot observe is emitted as an
// explicit 0 with a HELP note saying so.
//
// This package owns only the exposition primitives (Set, Family, Sample, and the
// bounded Histogram). Collecting the values from a volume lives in
// internal/observe so that this package stays free of log/volume dependencies.
package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind is a Prometheus metric type.
type Kind string

const (
	// Gauge is an instantaneous value.
	Gauge Kind = "gauge"
	// Counter is a monotonically non-decreasing total.
	Counter Kind = "counter"
	// Summary is a distribution exposed as client-side quantiles plus _sum/_count.
	Summary Kind = "summary"
)

// Label is one `name="value"` pair.
type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Sample is one exposition line: a name (defaulting to its family name), optional
// labels, and a value.
type Sample struct {
	Name   string  `json:"name,omitempty"`
	Labels []Label `json:"labels,omitempty"`
	Value  float64 `json:"value"`
}

// Family is one metric family: a name, a HELP string, a type, and its samples.
type Family struct {
	Name    string   `json:"name"`
	Help    string   `json:"help"`
	Type    Kind     `json:"type"`
	Samples []Sample `json:"samples"`
}

// Set is an ordered collection of families.
type Set struct {
	families []Family
	index    map[string]int
}

// NewSet returns an empty set.
func NewSet() *Set { return &Set{index: make(map[string]int)} }

// family returns the family with the given name, creating it with the given HELd
// and type on first use.
func (s *Set) family(name, help string, kind Kind) *Family {
	if i, ok := s.index[name]; ok {
		return &s.families[i]
	}
	s.index[name] = len(s.families)
	s.families = append(s.families, Family{Name: name, Help: help, Type: kind})
	return &s.families[len(s.families)-1]
}

// Gauge appends a gauge sample.
func (s *Set) Gauge(name, help string, value float64, labels ...Label) {
	f := s.family(name, help, Gauge)
	f.Samples = append(f.Samples, Sample{Labels: labels, Value: value})
}

// Counter appends a counter sample.
func (s *Set) Counter(name, help string, value float64, labels ...Label) {
	f := s.family(name, help, Counter)
	f.Samples = append(f.Samples, Sample{Labels: labels, Value: value})
}

// Quantile is one summary quantile.
type Quantile struct {
	Q     float64
	Value float64
}

// Summary appends a Prometheus summary: one sample per quantile labeled
// quantile="…", plus the name_sum and name_count companion series.
func (s *Set) Summary(name, help string, quantiles []Quantile, count uint64, sum float64) {
	f := s.family(name, help, Summary)
	for _, q := range quantiles {
		f.Samples = append(f.Samples, Sample{
			Labels: []Label{{Name: "quantile", Value: formatQuantile(q.Q)}},
			Value:  q.Value,
		})
	}
	f.Samples = append(f.Samples, Sample{Name: name + "_sum", Value: sum})
	f.Samples = append(f.Samples, Sample{Name: name + "_count", Value: float64(count)})
}

// Families returns a copy of the set's families.
func (s *Set) Families() []Family { return append([]Family(nil), s.families...) }

// Len is the number of families.
func (s *Set) Len() int { return len(s.families) }

// Text renders the Prometheus text exposition format.
func (s *Set) Text() string {
	var b strings.Builder
	for i, f := range s.families {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "# HELP %s %s\n", f.Name, escapeHelp(f.Help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.Name, f.Type)
		for _, sm := range f.Samples {
			name := sm.Name
			if name == "" {
				name = f.Name
			}
			b.WriteString(name)
			writeLabels(&b, sm.Labels)
			b.WriteByte(' ')
			b.WriteString(formatValue(sm.Value))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// JSON renders the set as a stable document (schema orfs.metrics.v1).
func (s *Set) JSON() ([]byte, error) {
	doc := struct {
		Schema   string   `json:"schema"`
		Families []Family `json:"families"`
	}{Schema: "orfs.metrics.v1", Families: s.families}
	return json.MarshalIndent(doc, "", "  ")
}

func writeLabels(b *strings.Builder, labels []Label) {
	if len(labels) == 0 {
		return
	}
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// escapeLabelValue escapes backslash, double-quote, and line feed per the
// Prometheus text format.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// escapeHelp escapes backslash and line feed per the Prometheus text format.
func escapeHelp(v string) string {
	if !strings.ContainsAny(v, "\\\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(v)
}

func formatQuantile(q float64) string { return strconv.FormatFloat(q, 'g', -1, 64) }

// formatValue renders a float the way an operator reads it: integral values
// within 2^53 print as integers, everything else in %g, and the three special
// values use Prometheus spellings.
func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < (1<<53) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
