package s3gw

import (
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	for _, in := range []string{"", "0", "none", "off"} {
		r, err := ParseRetention(in)
		if err != nil || !r.None() {
			t.Fatalf("ParseRetention(%q) = %+v, %v; want none", in, r, err)
		}
	}
	for _, in := range []string{"forever", "indefinite", "-1", "FOREVER"} {
		r, err := ParseRetention(in)
		if err != nil || !r.Indefinite {
			t.Fatalf("ParseRetention(%q) = %+v, %v; want indefinite", in, r, err)
		}
	}
	r, err := ParseRetention("24h")
	if err != nil || r.Indefinite || r.Duration != 24*time.Hour {
		t.Fatalf("ParseRetention(24h) = %+v, %v", r, err)
	}
	if _, err := ParseRetention("tomorrow"); err == nil {
		t.Fatal("an unparsable retention must be refused")
	}
}

func TestRetentionRetainUntilFor(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	// An explicit header wins over the default.
	got, err := (Retention{Indefinite: true}).retainUntilFor("2027-01-01T00:00:00Z", now)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2027-01-01T00:00:00Z" {
		t.Fatalf("explicit header = %q", got)
	}
	// Indefinite default.
	if got, _ := (Retention{Indefinite: true}).retainUntilFor("", now); got != retainIndefinite {
		t.Fatalf("indefinite default = %q", got)
	}
	// Finite default is now + the duration.
	got, err = (Retention{Duration: 48 * time.Hour}).retainUntilFor("", now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if got != want {
		t.Fatalf("finite default = %q, want %q", got, want)
	}
	// No default means no retention.
	if got, _ := (Retention{}).retainUntilFor("", now); got != "" {
		t.Fatalf("no default = %q, want empty", got)
	}
}

func TestObjectMetaLocked(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		meta   objectMeta
		locked bool
		reason string
	}{
		{"unlocked", objectMeta{}, false, ""},
		{"indefinite", objectMeta{RetainUntil: retainIndefinite}, true, "retention"},
		{"future", objectMeta{RetainUntil: now.Add(time.Hour).Format(time.RFC3339Nano)}, true, "retention"},
		{"expired", objectMeta{RetainUntil: now.Add(-time.Hour).Format(time.RFC3339Nano)}, false, ""},
		{"legal hold", objectMeta{LegalHold: true}, true, "legal-hold"},
		{"unparsable fails closed", objectMeta{RetainUntil: "whenever"}, true, "retention"},
	}
	for _, c := range cases {
		locked, reason := c.meta.locked(now)
		if locked != c.locked || reason != c.reason {
			t.Fatalf("%s: locked=%v reason=%q, want %v %q", c.name, locked, reason, c.locked, c.reason)
		}
	}
}

func TestParseLegalHold(t *testing.T) {
	if hold, present, err := parseLegalHold(""); hold || present || err != nil {
		t.Fatalf("empty = %v %v %v", hold, present, err)
	}
	if hold, present, err := parseLegalHold("ON"); !hold || !present || err != nil {
		t.Fatalf("ON = %v %v %v", hold, present, err)
	}
	if hold, present, err := parseLegalHold("off"); hold || !present || err != nil {
		t.Fatalf("off = %v %v %v", hold, present, err)
	}
	if _, _, err := parseLegalHold("maybe"); err == nil {
		t.Fatal("an invalid legal hold must be refused")
	}
}

func TestValidateBucketName(t *testing.T) {
	for _, ok := range []string{"bucket", "my-bucket.1", "abc"} {
		if err := validateBucketName(ok); err != nil {
			t.Fatalf("validateBucketName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"ab", "-bucket", "bucket-", ".bucket", "Bucket", "buck..et", "buck/et", "bucket_1"} {
		if err := validateBucketName(bad); err == nil {
			t.Fatalf("validateBucketName(%q) should fail", bad)
		}
	}
}
