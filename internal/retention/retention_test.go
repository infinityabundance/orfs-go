package retention_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/infinityabundance/orfs-go/internal/retention"
)

func measure(system, kind, state string, epoch, writers, readers, bs int, ops uint64) retention.Measure {
	return retention.Measure{
		Schema:    retention.MeasurementSchema,
		System:    system,
		Kind:      kind,
		State:     state,
		Epoch:     epoch,
		Writers:   writers,
		Readers:   readers,
		BlockSize: bs,
		Stats:     retention.Stats{Ops: ops, Bytes: ops * uint64(bs), P50Ns: 100, P999Ns: 900},
	}
}

func TestMergeSortsDeterministically(t *testing.T) {
	comp := json.RawMessage(`{"same_machine":true}`)
	rec, err := retention.Merge([]retention.Measure{
		measure("OrFS-GO", "read", "current", 1, 0, 1, 4096, 20),
		measure("Btrfs", "read", "snapshot", 1, 0, 1, 4096, 15),
		measure("Btrfs", "append", "append", 1, 1, 0, 4096, 10),
		measure("Btrfs", "append", "append", 0, 1, 0, 4096, 5),
	}, []retention.Space{
		{Schema: retention.SpaceSchema, System: "Btrfs", Epoch: 1, State: "snapshot"},
	}, []retention.Snapshot{
		{Schema: retention.SnapshotSchema, System: "Btrfs", Epoch: 0},
	}, comp, 42)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if rec.Schema != retention.ReceiptSchema {
		t.Fatalf("schema = %q, want %q", rec.Schema, retention.ReceiptSchema)
	}
	if string(rec.Comparability) != string(comp) {
		t.Fatalf("comparability not embedded verbatim: %s", rec.Comparability)
	}
	// system asc, kind asc (append before read), state asc, epoch asc.
	want := []struct {
		sys, kind, state string
		epoch            int
	}{
		{"Btrfs", "append", "append", 0},
		{"Btrfs", "append", "append", 1},
		{"Btrfs", "read", "snapshot", 1},
		{"OrFS-GO", "read", "current", 1},
	}
	if len(rec.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rec.Rows), len(want))
	}
	for i, w := range want {
		got := rec.Rows[i]
		if got.System != w.sys || got.Kind != w.kind || got.State != w.state || got.Epoch != w.epoch {
			t.Errorf("row %d = %+v, want %s/%s/%s epoch=%d", i, got, w.sys, w.kind, w.state, w.epoch)
		}
	}
}

func TestMergeRejectsDuplicateCoordinate(t *testing.T) {
	_, err := retention.Merge([]retention.Measure{
		measure("Btrfs", "append", "append", 0, 1, 0, 4096, 5),
		measure("Btrfs", "append", "append", 0, 1, 0, 4096, 6),
	}, nil, nil, nil, 0)
	if err == nil {
		t.Fatal("expected a duplicate-coordinate error")
	}
}

func TestMergeRejectsWrongSchema(t *testing.T) {
	bad := measure("Btrfs", "append", "append", 0, 1, 0, 4096, 5)
	bad.Schema = "something-else"
	if _, err := retention.Merge([]retention.Measure{bad}, nil, nil, nil, 0); err == nil {
		t.Fatal("expected a measure schema error")
	}
	good := measure("Btrfs", "append", "append", 0, 1, 0, 4096, 5)
	if _, err := retention.Merge([]retention.Measure{good},
		[]retention.Space{{Schema: "wrong"}}, nil, nil, 0); err == nil {
		t.Fatal("expected a space schema error")
	}
	if _, err := retention.Merge([]retention.Measure{good}, nil,
		[]retention.Snapshot{{Schema: "wrong"}}, nil, 0); err == nil {
		t.Fatal("expected a snapshot schema error")
	}
}

func TestMergeRejectsEmptySystem(t *testing.T) {
	bad := measure("", "append", "append", 0, 1, 0, 4096, 5)
	if _, err := retention.Merge([]retention.Measure{bad}, nil, nil, nil, 0); err == nil {
		t.Fatal("expected an empty-system error")
	}
}

func TestRowCarriesErrorRate(t *testing.T) {
	m := measure("Btrfs", "append", "append", 0, 1, 0, 4096, 100)
	m.Stats.Errors = 4
	m.Stats.ErrorRate = 0.04
	m.Stats.ErrorClasses = map[string]uint64{"ENOSPC": 4}
	row := m.Row()
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

func TestSummariseExactSamples(t *testing.T) {
	results := []retention.WorkerResult{
		{Ops: 4, Bytes: 4 * 4096, Samples: []int64{10, 20, 30, 40}, Errs: map[string]uint64{}},
		{Ops: 3, Bytes: 3 * 4096, Samples: []int64{50, 60, 70}, Errs: map[string]uint64{"ENOSPC": 1}},
	}
	s := retention.Summarise(results, 1.0)
	if s.Ops != 7 || s.Bytes != 7*4096 {
		t.Fatalf("ops=%d bytes=%d", s.Ops, s.Bytes)
	}
	if s.Sampled != 7 || s.Truncated {
		t.Fatalf("sampled=%d truncated=%v", s.Sampled, s.Truncated)
	}
	if s.P50Ns != 40 {
		t.Errorf("p50=%d want 40", s.P50Ns)
	}
	if s.MaxNs != 70 {
		t.Errorf("max=%d want 70", s.MaxNs)
	}
	if s.Errors != 1 || s.ErrorClasses["ENOSPC"] != 1 || s.ErrorRate != 1.0/7.0 {
		t.Errorf("errors=%d classes=%v rate=%v", s.Errors, s.ErrorClasses, s.ErrorRate)
	}
	if s.OpsPerSecond != 7 {
		t.Errorf("ops/s=%v want 7", s.OpsPerSecond)
	}
}

func TestSummariseTruncation(t *testing.T) {
	results := []retention.WorkerResult{
		{Ops: 10, Samples: []int64{1, 2, 3}, Errs: map[string]uint64{}},
	}
	s := retention.Summarise(results, 0)
	if !s.Truncated {
		t.Fatal("expected Truncated=true when ops > sampled")
	}
	if s.Sampled != 3 {
		t.Fatalf("sampled=%d want 3", s.Sampled)
	}
}

func TestQuantileNearestRank(t *testing.T) {
	sorted := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := retention.Quantile(sorted, 0.50); got != 50 {
		t.Errorf("p50=%d want 50", got)
	}
	if got := retention.Quantile(sorted, 0.99); got != 100 {
		t.Errorf("p99=%d want 100", got)
	}
	if got := retention.Quantile(nil, 0.5); got != 0 {
		t.Errorf("empty quantile=%d want 0", got)
	}
}

func TestErrnoName(t *testing.T) {
	if got := retention.ErrnoName(syscall.ENOSPC); got != "ENOSPC" {
		t.Errorf("ENOSPC -> %q", got)
	}
	if got := retention.ErrnoName(syscall.ENOENT); got != "ENOENT" {
		t.Errorf("ENOENT -> %q", got)
	}
	// A non-errno error is not a syscall error and is classed OTHER.
	if got := retention.ErrnoName(os.ErrNotExist); got[:6] != "OTHER:" {
		t.Errorf("os.ErrNotExist -> %q, want an OTHER: class", got)
	}
	if got := retention.ErrnoName(syscall.Errno(9999)); got == "" {
		t.Error("unknown errno produced an empty class")
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0.00B", 0},
		{"1048576", 1048576},
		{"1.00KiB", 1024},
		{"1.00MiB", 1 << 20},
		{"2.00GiB", 2 << 30},
		{"1.50MiB", (1 << 20) * 3 / 2},
	}
	for _, c := range cases {
		got, err := retention.ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	if _, err := retention.ParseSize("nonsense"); err == nil {
		t.Error("expected an error for a unitless non-number")
	}
	if _, err := retention.ParseSize("5.00ZiB"); err == nil {
		t.Error("expected an error for an unknown unit")
	}
}

func TestParseBtrfsUsed(t *testing.T) {
	out := "Overall:\n" +
		"    Device size:                   2.00GiB\n" +
		"    Used:                         39459149\n" +
		"    Free (estimated):              1.94GiB\n"
	got, err := retention.ParseBtrfsUsed(out)
	if err != nil {
		t.Fatalf("ParseBtrfsUsed: %v", err)
	}
	if got != 39459149 {
		t.Fatalf("used=%d want 39459149", got)
	}
	if _, err := retention.ParseBtrfsUsed("no used line here"); err == nil {
		t.Fatal("expected an error when no Used: line is present")
	}
}

func TestParseBtrfsDU(t *testing.T) {
	out := "     Total   Exclusive  Set shared  Filename\n" +
		"  1.00MiB         0.00B      1.00MiB  /mnt/snapshots/snap-0\n"
	total, excl, shared, err := retention.ParseBtrfsDU(out)
	if err != nil {
		t.Fatalf("ParseBtrfsDU: %v", err)
	}
	if total != 1<<20 || excl != 0 || shared != 1<<20 {
		t.Fatalf("total=%d excl=%d shared=%d", total, excl, shared)
	}
	if _, _, _, err := retention.ParseBtrfsDU("     Total   Exclusive  Set shared  Filename\n"); err == nil {
		t.Fatal("expected an error when only the header is present")
	}
}

func TestRunAppendDefinedRecords(t *testing.T) {
	dir := t.TempDir()
	if err := retention.Prepare(dir, "append", 2); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	m, err := retention.RunAppend(retention.AppendConfig{
		Dir: dir, System: "Btrfs", Prefix: "append", Writers: 2,
		Records: 3, RecordBytes: 512, MaxSeconds: 5, Seed: 1, Epoch: 0,
	})
	if err != nil {
		t.Fatalf("RunAppend: %v", err)
	}
	if m.Kind != retention.KindAppend || m.State != retention.StateAppend {
		t.Fatalf("kind/state = %s/%s", m.Kind, m.State)
	}
	if m.Stats.Ops != 6 {
		t.Fatalf("ops=%d want 6", m.Stats.Ops)
	}
	if m.Stats.Sampled != 6 {
		t.Fatalf("sampled=%d want 6", m.Stats.Sampled)
	}
	if m.TimeCapped {
		t.Fatal("a completed epoch must not be time-capped")
	}
	info, err := os.Stat(filepath.Join(dir, "append-0.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 3*512 {
		t.Fatalf("append-0.bin size = %d, want %d", info.Size(), 3*512)
	}
}

func TestRunAppendDeterministicContent(t *testing.T) {
	read := func(dir string) []byte {
		if err := retention.Prepare(dir, "append", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := retention.RunAppend(retention.AppendConfig{
			Dir: dir, System: "Btrfs", Prefix: "append", Writers: 1,
			Records: 4, RecordBytes: 256, MaxSeconds: 5, Seed: 7, Epoch: 0,
		}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "append-0.bin"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a := read(t.TempDir())
	b := read(t.TempDir())
	if len(a) != 4*256 || len(a) != len(b) {
		t.Fatalf("lengths %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d differs: %d != %d", i, a[i], b[i])
		}
	}
}

func TestRunAppendTimeCap(t *testing.T) {
	dir := t.TempDir()
	if err := retention.Prepare(dir, "append", 1); err != nil {
		t.Fatal(err)
	}
	// An enormous record count with a tiny time bound must trip the safety cap.
	m, err := retention.RunAppend(retention.AppendConfig{
		Dir: dir, System: "Btrfs", Prefix: "append", Writers: 1,
		Records: 1 << 30, RecordBytes: 4096, MaxSeconds: 0.02, Seed: 1, Epoch: 0,
	})
	if err != nil {
		t.Fatalf("RunAppend: %v", err)
	}
	if !m.TimeCapped {
		t.Fatal("expected TimeCapped=true when the time bound trips first")
	}
	if m.Stats.Ops == 0 {
		t.Fatal("expected some operations before the cap")
	}
}

func TestRunReadCountsOps(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(dir, "append-0.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := retention.RunRead(retention.ReadConfig{
		Dir: dir, System: "Btrfs", ReadFile: "append-0.bin",
		Readers: 2, BlockSize: 4096, Seconds: 0.2, Seed: 1, Epoch: 0, State: retention.StateSnapshot,
	})
	if err != nil {
		t.Fatalf("RunRead: %v", err)
	}
	if m.Kind != retention.KindRead || m.State != retention.StateSnapshot {
		t.Fatalf("kind/state = %s/%s", m.Kind, m.State)
	}
	if m.Stats.Ops == 0 || m.Stats.Errors != 0 {
		t.Fatalf("ops=%d errors=%d", m.Stats.Ops, m.Stats.Errors)
	}
	if m.Stats.Bytes != m.Stats.Ops*4096 {
		t.Fatalf("bytes=%d ops=%d", m.Stats.Bytes, m.Stats.Ops)
	}
}

func TestRunReadRejectsShortFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "append-0.bin"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.RunRead(retention.ReadConfig{
		Dir: dir, System: "Btrfs", ReadFile: "append-0.bin",
		Readers: 1, BlockSize: 4096, Seconds: 0.1,
	}); err == nil {
		t.Fatal("expected an error when the file is smaller than the block size")
	}
}
