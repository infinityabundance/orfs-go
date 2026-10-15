package main

import (
	"testing"

	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/s3gw"
)

func TestBucketMapFlag(t *testing.T) {
	m := bucketMap{}
	if err := m.Set("logs=/var/lib/orfs/logs"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if m["logs"] != "/var/lib/orfs/logs" {
		t.Fatalf("map = %v", m)
	}
	if err := m.Set("nope"); err == nil {
		t.Fatal("a value without PATH must be refused")
	}
	if err := m.Set("=path"); err == nil {
		t.Fatal("an empty name must be refused")
	}
}

func TestRunRejectsMissingConfiguration(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("run() with no flags = %d, want 2", code)
	}
	// A volume without credentials is refused: this gateway is not anonymous.
	if code := run([]string{"-volume", t.TempDir()}); code != 2 {
		t.Fatalf("run() without credentials = %d, want 2", code)
	}
}

func TestOpenVolumeInit(t *testing.T) {
	backing := t.TempDir()
	vopts := s3gw.VolumeOptions{ManifestCacheCapacity: 8}

	// -init creates the volume.
	vs, err := openVolume(backing, true, 256*1024, vopts)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if vs.Vol == nil || vs.Cache == nil {
		t.Fatal("the volume set is incomplete")
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Opening the same backing again does not re-init it.
	vs2, err := openVolume(backing, false, 256*1024, vopts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer vs2.Close()
	if vs2.Vol.Meta.SegmentSize != 256*1024 {
		t.Fatalf("segment size = %d", vs2.Vol.Meta.SegmentSize)
	}
}

func TestParseManifestPolicy(t *testing.T) {
	cases := []struct {
		in   string
		want manifest.Policy
	}{
		{"", manifest.PolicyS3FIFO},
		{"s3fifo", manifest.PolicyS3FIFO},
		{"lru", manifest.PolicyLRU},
	}
	for _, c := range cases {
		got, err := parseManifestPolicy(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("policy(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := parseManifestPolicy("nonsense"); err == nil {
		t.Fatal("an unknown policy must be refused")
	}
}
