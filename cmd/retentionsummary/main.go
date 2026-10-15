// Command retentionsummary merges the per-arm measurements written by
// cmd/retentionbench into the Btrfs retained-snapshots retention receipt
// evidence/retention-btrfs.json (schema orfs.retention.btrfs.v1).
//
// It classifies each input file by its schema (a measurement, a space
// accounting, or a snapshot-creation record), embeds the comparability block
// verbatim, emits one row per measured arm keyed by
// {system, kind, state, epoch, writers, readers, block_size}, and refuses to
// merge two rows with the same comparable coordinate. It never ranks rows
// against each other: the receipt is data, not a leaderboard.
//
// Usage:
//
//	retentionsummary -comparability c.json -o out.json file.json [file.json ...]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/infinityabundance/orfs-go/internal/retention"
)

func main() {
	comparability := flag.String("comparability", "", "comparability block JSON (verbatim)")
	schema := flag.String("schema", retention.ReceiptSchema, "receipt schema to stamp (default the Btrfs court's)")
	comparator := flag.String("comparator", "Btrfs", "snapshot-retaining comparator name (Btrfs | OpenZFS)")
	out := flag.String("o", "", "output path (default stdout)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: retentionsummary -comparability c.json -o out.json file.json ...")
		os.Exit(2)
	}

	var comp json.RawMessage
	if *comparability != "" {
		raw, err := os.ReadFile(*comparability)
		if err != nil {
			fmt.Fprintf(os.Stderr, "retentionsummary: comparability: %v\n", err)
			os.Exit(1)
		}
		if !json.Valid(raw) {
			fmt.Fprintf(os.Stderr, "retentionsummary: comparability %s is not valid JSON\n", *comparability)
			os.Exit(1)
		}
		comp = json.RawMessage(raw)
	}

	var measures []retention.Measure
	var spaces []retention.Space
	var snaps []retention.Snapshot
	for _, path := range flag.Args() {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "retentionsummary: %s: %v\n", path, err)
			os.Exit(1)
		}
		var head struct {
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			fmt.Fprintf(os.Stderr, "retentionsummary: %s: %v\n", path, err)
			os.Exit(1)
		}
		switch head.Schema {
		case retention.MeasurementSchema:
			var m retention.Measure
			if err := json.Unmarshal(raw, &m); err != nil {
				fmt.Fprintf(os.Stderr, "retentionsummary: %s: %v\n", path, err)
				os.Exit(1)
			}
			measures = append(measures, m)
		case retention.SpaceSchema:
			var s retention.Space
			if err := json.Unmarshal(raw, &s); err != nil {
				fmt.Fprintf(os.Stderr, "retentionsummary: %s: %v\n", path, err)
				os.Exit(1)
			}
			spaces = append(spaces, s)
		case retention.SnapshotSchema:
			var s retention.Snapshot
			if err := json.Unmarshal(raw, &s); err != nil {
				fmt.Fprintf(os.Stderr, "retentionsummary: %s: %v\n", path, err)
				os.Exit(1)
			}
			snaps = append(snaps, s)
		default:
			fmt.Fprintf(os.Stderr, "retentionsummary: %s: unknown schema %q\n", path, head.Schema)
			os.Exit(1)
		}
	}

	rec, err := retention.MergeFor(measures, spaces, snaps, comp, *schema, *comparator, time.Now().Unix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "retentionsummary: %v\n", err)
		os.Exit(1)
	}
	switch *comparator {
	case "OpenZFS":
		rec.Method = "One defined, bounded epoch workload (append 4 KiB records with write(2)+fsync(2) per " +
			"record) drives both systems. The OpenZFS arm takes a read-only snapshot after every epoch, so every " +
			"committed state is retained; the OrFS-GO arm takes no snapshots, because it retains every committed " +
			"transition in its authoritative log. Reads are block-aligned uniform random preads of the current state " +
			"and of each retained snapshot. Space is accounted from " +
			"`zfs list -o used,referenced,logicalused,logicalreferenced` and from the authoritative bytes of the OrFS " +
			"volume. Latencies are exact observed samples."
		rec.Notes = []string{
			"Every row is one bounded arm of cmd/retentionbench against a directory on this machine.",
			"Read the comparability block before comparing any two rows; a row that could not be measured the same way is recorded as a caveat, not a pass or a parity claim.",
			"The receipt deliberately carries no ranking: two rows are comparable only at the same {kind, state, epoch, writers, readers, block_size} and under the listed configuration differences.",
			"OpenZFS retains historical states as read-only snapshots; OrFS-GO retains them in its authoritative log and does not serve named historical roots in this build, so the historical-state read axis is OpenZFS-only (recorded as not comparable).",
		}
	default:
		rec.Method = "One defined, bounded epoch workload (append 4 KiB records with write(2)+fsync(2) per " +
			"record) drives both systems. The Btrfs arm takes a read-only snapshot after every epoch, so every " +
			"committed state is retained; the OrFS-GO arm takes no snapshots, because it retains every committed " +
			"transition in its authoritative log. Reads are block-aligned uniform random preads of the current state " +
			"and of each retained snapshot. Space is accounted from `btrfs filesystem usage`/`btrfs filesystem du` and " +
			"from the authoritative bytes of the OrFS volume. Latencies are exact observed samples."
		rec.Notes = []string{
			"Every row is one bounded arm of cmd/retentionbench against a directory on this machine.",
			"Read the comparability block before comparing any two rows; a row that could not be measured the same way is recorded as a caveat, not a pass or a parity claim.",
			"The receipt deliberately carries no ranking: two rows are comparable only at the same {kind, state, epoch, writers, readers, block_size} and under the listed configuration differences.",
			"Btrfs retains historical states as read-only snapshots; OrFS-GO retains them in its authoritative log and does not serve named historical roots in this build, so the historical-state read axis is Btrfs-only (recorded as not comparable).",
		}
	}

	blob, _ := json.MarshalIndent(rec, "", "  ")
	blob = append(blob, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "retentionsummary: write: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Stdout.Write(blob)
}
