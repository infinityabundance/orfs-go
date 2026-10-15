package main

import (
	"reflect"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/osscompare"
)

func row(system string) osscompare.Run {
	return osscompare.Run{Schema: osscompare.RunSchema, System: system, Workload: "append", Writers: 1, BlockSize: 4096}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"OrFS-GO", []string{"OrFS-GO"}},
		{"OrFS-GO, OrFS-Rust , SeaweedFS", []string{"OrFS-GO", "OrFS-Rust", "SeaweedFS"}},
		{"A,,B,", []string{"A", "B"}},
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCheckExpectedSystems(t *testing.T) {
	runs := []osscompare.Run{row("OrFS-GO"), row("OrFS-Rust"), row("SeaweedFS")}

	if got := checkExpectedSystems(runs, "OrFS-GO,OrFS-Rust,SeaweedFS"); len(got) != 0 {
		t.Fatalf("all present should be satisfied, got missing %v", got)
	}
	if got := checkExpectedSystems(runs, ""); len(got) != 0 {
		t.Fatalf("empty expectation checks nothing, got missing %v", got)
	}
	// The third system silently absent must be named, not dropped.
	two := []osscompare.Run{row("OrFS-GO"), row("OrFS-Rust")}
	got := checkExpectedSystems(two, "OrFS-GO,OrFS-Rust,SeaweedFS")
	if len(got) != 1 || got[0] != "SeaweedFS" {
		t.Fatalf("missing = %v, want [SeaweedFS]", got)
	}
}
