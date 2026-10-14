package metrics

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestSetTextExposition pins the exact hand-written text exposition: HELP, TYPE,
// label escaping, integer/float formatting, and the summary's quantile/_sum/_count
// companion series are all part of the contract the operator surface depends on.
func TestSetTextExposition(t *testing.T) {
	set := NewSet()
	set.Gauge("orfs_example_gauge", "A gauge.", 42)
	set.Counter("orfs_example_total", "A counter.", 7, Label{Name: "reason", Value: `a"b`})
	set.Summary("orfs_example_seconds", "A summary.", []Quantile{{Q: 0.5, Value: 0.001}, {Q: 0.99, Value: 0.25}}, 3, 0.3)

	want := `# HELP orfs_example_gauge A gauge.
# TYPE orfs_example_gauge gauge
orfs_example_gauge 42

# HELP orfs_example_total A counter.
# TYPE orfs_example_total counter
orfs_example_total{reason="a\"b"} 7

# HELP orfs_example_seconds A summary.
# TYPE orfs_example_seconds summary
orfs_example_seconds{quantile="0.5"} 0.001
orfs_example_seconds{quantile="0.99"} 0.25
orfs_example_seconds_sum 0.3
orfs_example_seconds_count 3
`
	if got := set.Text(); got != want {
		t.Fatalf("exposition mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestSetTextIsWellFormed parses the generated lines the way a scraper does: every
// sample line must be `name[labels] value` with a parseable value.
func TestSetTextIsWellFormed(t *testing.T) {
	set := NewSet()
	set.Gauge("orfs_a", "help a", 1)
	set.Gauge("orfs_a", "help a", 2.5, Label{Name: "label", Value: "x"})
	set.Counter("orfs_b_total", "help b", 0)
	set.Summary("orfs_c_seconds", "help c", []Quantile{{Q: 0.5, Value: 0.5}, {Q: 0.99, Value: 0.99}}, 10, 3.14)

	for _, line := range strings.Split(strings.TrimRight(set.Text(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if !strings.HasPrefix(line, "# HELP ") && !strings.HasPrefix(line, "# TYPE ") {
				t.Fatalf("malformed comment line: %q", line)
			}
			continue
		}
		idx := strings.LastIndexByte(line, ' ')
		if idx <= 0 {
			t.Fatalf("malformed sample line: %q", line)
		}
		if _, err := strconv.ParseFloat(line[idx+1:], 64); err != nil {
			t.Fatalf("unparseable value in %q: %v", line, err)
		}
	}
}

// TestSetJSON asserts the JSON form is the stable orfs.metrics.v1 document.
func TestSetJSON(t *testing.T) {
	set := NewSet()
	set.Gauge("orfs_x", "help x", 3)
	set.Summary("orfs_y_seconds", "help y", []Quantile{{Q: 0.5, Value: 0.1}}, 2, 0.2)
	blob, err := set.JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var doc struct {
		Schema   string   `json:"schema"`
		Families []Family `json:"families"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Schema != "orfs.metrics.v1" {
		t.Fatalf("schema = %q", doc.Schema)
	}
	if len(doc.Families) != 2 {
		t.Fatalf("families = %d, want 2", len(doc.Families))
	}
	if doc.Families[1].Samples[len(doc.Families[1].Samples)-1].Name != "orfs_y_seconds_count" {
		t.Fatalf("summary count companion missing: %+v", doc.Families[1])
	}
}

// TestFormatValue covers the integer/special-value formatting an operator reads.
func TestFormatValue(t *testing.T) {
	cases := map[float64]string{
		0:       "0",
		1024:    "1024",
		1234567: "1234567",
		0.001:   "0.001",
		1.5:     "1.5",
	}
	for in, want := range cases {
		if got := formatValue(in); got != want {
			t.Fatalf("formatValue(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestHelpEscaping proves backslashes and newlines are escaped in HELP text.
func TestHelpEscaping(t *testing.T) {
	set := NewSet()
	set.Gauge("orfs_z", "back\\slash\nnewline", 1)
	got := set.Text()
	if !strings.Contains(got, `# HELP orfs_z back\\slash\nnewline`) {
		t.Fatalf("HELP not escaped: %q", got)
	}
}
