package osscompare

import (
	"encoding/json"
	"testing"
)

func run(system, workload string, writers, readers, bs int, ops uint64) Run {
	return Run{
		Schema:    RunSchema,
		System:    system,
		Workload:  workload,
		Writers:   writers,
		Readers:   readers,
		BlockSize: bs,
		Stats:     Stats{Ops: ops, Bytes: ops * uint64(bs), OpsPerSecond: float64(ops), P50Ns: 100, P999Ns: 900},
	}
}

func TestMergeSortsDeterministically(t *testing.T) {
	comp := json.RawMessage(`{"same_machine":true}`)
	rec, err := Merge([]Run{
		run("OrFS-Rust", "read", 0, 64, 65536, 20),
		run("OrFS-GO", "append", 64, 0, 4096, 10),
		run("OrFS-GO", "append", 1, 0, 4096, 5),
		run("OrFS-GO", "read", 0, 64, 4096, 15),
	}, comp, 42)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if rec.Schema != ReceiptSchema {
		t.Fatalf("schema = %q, want %q", rec.Schema, ReceiptSchema)
	}
	if string(rec.Comparability) != string(comp) {
		t.Fatalf("comparability not embedded verbatim: %s", rec.Comparability)
	}
	// System asc, workload asc (append before read), block_size asc, writers asc.
	want := []struct {
		sys, wl string
		writers int
		bs      int
	}{
		{"OrFS-GO", "append", 1, 4096},
		{"OrFS-GO", "append", 64, 4096},
		{"OrFS-GO", "read", 0, 4096},
		{"OrFS-Rust", "read", 0, 65536},
	}
	if len(rec.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rec.Rows), len(want))
	}
	for i, w := range want {
		got := rec.Rows[i]
		if got.System != w.sys || got.Workload != w.wl || got.Writers != w.writers || got.BlockSize != w.bs {
			t.Errorf("row %d = %+v, want %s/%s writers=%d bs=%d", i, got, w.sys, w.wl, w.writers, w.bs)
		}
	}
}

func TestMergeRejectsDuplicateCoordinate(t *testing.T) {
	_, err := Merge([]Run{
		run("OrFS-GO", "append", 1, 0, 4096, 5),
		run("OrFS-GO", "append", 1, 0, 4096, 6),
	}, nil, 0)
	if err == nil {
		t.Fatal("expected a duplicate-coordinate error")
	}
}

func TestMergeRejectsWrongSchema(t *testing.T) {
	bad := run("OrFS-GO", "append", 1, 0, 4096, 5)
	bad.Schema = "something-else"
	if _, err := Merge([]Run{bad}, nil, 0); err == nil {
		t.Fatal("expected a schema error")
	}
}

func TestMergeRejectsEmptySystem(t *testing.T) {
	bad := run("", "append", 1, 0, 4096, 5)
	if _, err := Merge([]Run{bad}, nil, 0); err == nil {
		t.Fatal("expected an empty-system error")
	}
}

func TestRowCarriesErrorRate(t *testing.T) {
	r := run("OrFS-GO", "append", 1, 0, 4096, 100)
	r.Stats.Errors = 4
	r.Stats.ErrorRate = 0.04
	r.Stats.ErrorClasses = map[string]uint64{"ENOSPC": 4}
	row := r.Row()
	if row.ErrorRate != 0.04 {
		t.Errorf("error_rate = %v, want 0.04", row.ErrorRate)
	}
	if row.Errors["ENOSPC"] != 4 {
		t.Errorf("ENOSPC class = %d, want 4", row.Errors["ENOSPC"])
	}
	if row.Bytes != 100*4096 {
		t.Errorf("bytes = %d, want %d", row.Bytes, 100*4096)
	}
}
