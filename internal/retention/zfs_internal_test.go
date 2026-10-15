package retention

import (
	"strings"
	"testing"
)

// snapshotCommand is the one piece of the ZFS snapshot path that can be checked
// without an OpenZFS kernel module: it builds the exact argv for each kind. The
// empty kind must remain the Btrfs court's command.
func TestSnapshotCommand(t *testing.T) {
	cases := []struct {
		kind       string
		live, dest string
		wantName   string
		wantArgs   []string
		wantDest   string
	}{
		{"", "/mnt/live", "/mnt/snapshots/snap-0", "btrfs",
			[]string{"subvolume", "snapshot", "-r", "/mnt/live", "/mnt/snapshots/snap-0"},
			"/mnt/snapshots/snap-0"},
		{"btrfs", "/mnt/live", "/mnt/snapshots/snap-1", "btrfs",
			[]string{"subvolume", "snapshot", "-r", "/mnt/live", "/mnt/snapshots/snap-1"},
			"/mnt/snapshots/snap-1"},
		{"zfs", "tank/live", "snap-2", "zfs",
			[]string{"snapshot", "tank/live@snap-2"},
			"tank/live@snap-2"},
	}
	for _, c := range cases {
		name, args, dest, note, err := snapshotCommand(c.kind, c.live, c.dest)
		if err != nil {
			t.Fatalf("kind %q: %v", c.kind, err)
		}
		if name != c.wantName {
			t.Errorf("kind %q: name=%q want %q", c.kind, name, c.wantName)
		}
		if strings.Join(args, " ") != strings.Join(c.wantArgs, " ") {
			t.Errorf("kind %q: args=%v want %v", c.kind, args, c.wantArgs)
		}
		if dest != c.wantDest {
			t.Errorf("kind %q: dest=%q want %q", c.kind, dest, c.wantDest)
		}
		if note == "" {
			t.Errorf("kind %q: empty note", c.kind)
		}
	}
	if _, _, _, _, err := snapshotCommand("ext4", "/a", "/b"); err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
}

// A zfs Space measurement without a dataset is refused before any command runs.
func TestMeasureSpaceZFSRequiresDataset(t *testing.T) {
	if _, err := MeasureSpace(SpaceConfig{Kind: "zfs", System: "OpenZFS"}); err == nil {
		t.Fatal("expected an error when -dataset is missing")
	}
}
