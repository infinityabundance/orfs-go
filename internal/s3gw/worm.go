package s3gw

import (
	"encoding/json"
	"strings"
	"time"
)

// WORM model.
//
// OrFS itself never rewrites committed bytes (Law I), so the object's *content*
// is immutable the moment its transaction commits. What a WORM gateway adds is
// the *name-level* guarantee: an object under retention or legal hold cannot be
// withdrawn (unlinked) or overwritten through S3. The gateway therefore records
// a lock state per object and enforces it on PUT and DELETE.
//
// The lock state lives in a single extended attribute on the object's inode, so
// it is itself an authoritative transaction and survives recovery. A legal hold
// has no expiry; a retention has a retain-until instant, which may be
// "indefinite" (the default bucket policy) — an object so locked is never
// deletable through this gateway.

// metaXattr is the one extended attribute an object's S3 metadata lives in.
const metaXattr = "user.orfs.s3.meta"

const (
	// retainIndefinite marks a lock with no expiry.
	retainIndefinite = "indefinite"
	legalHoldOn      = "ON"
	legalHoldOff     = "OFF"
)

// objectMeta is the per-object S3 metadata, serialised into metaXattr.
type objectMeta struct {
	ETag      string `json:"etag"`
	Created   string `json:"created,omitempty"`
	Size      uint64 `json:"size"`
	LegalHold bool   `json:"legal_hold,omitempty"`
	// RetainUntil is "" (no retention), retainIndefinite, or an RFC3339Nano
	// timestamp in UTC.
	RetainUntil string `json:"retain_until,omitempty"`
	// RetainMode records the requested x-amz-object-lock-mode; it is
	// informational (this gateway always enforces compliance-style).
	RetainMode string `json:"retain_mode,omitempty"`
	// ContentType is the client-declared media type.
	ContentType string `json:"content_type,omitempty"`
}

func encodeObjectMeta(m objectMeta) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		// objectMeta is a plain struct of strings/bools/uint64: this cannot fail.
		return []byte("{}")
	}
	return b
}

func decodeObjectMeta(b []byte) (objectMeta, bool) {
	var m objectMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return objectMeta{}, false
	}
	return m, true
}

// locked reports whether the object may not be withdrawn at now, and why.
// An unparsable retain-until fails closed: a lock we cannot read is a lock.
func (m objectMeta) locked(now time.Time) (bool, string) {
	if m.LegalHold {
		return true, "legal-hold"
	}
	switch m.RetainUntil {
	case "":
		return false, ""
	case retainIndefinite:
		return true, "retention"
	}
	t, err := time.Parse(time.RFC3339Nano, m.RetainUntil)
	if err != nil {
		return true, "retention"
	}
	if t.After(now) {
		return true, "retention"
	}
	return false, ""
}

// Retention is a bucket-default WORM retention policy.
type Retention struct {
	// Indefinite means objects are retained with no expiry (the WORM default).
	Indefinite bool
	// Duration is a finite default retention; used only when Indefinite is false.
	Duration time.Duration
}

// None reports whether no default retention is configured, i.e. objects are
// locked only when the request asks or a legal hold is set.
func (r Retention) None() bool { return !r.Indefinite && r.Duration <= 0 }

// ParseRetention parses a -default-retention flag value: "" / "0" / "none"
// selects no default; "forever" / "indefinite" selects an indefinite lock;
// anything else is a Go duration (for example "24h", "8760h").
func ParseRetention(s string) (Retention, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "none", "off":
		return Retention{}, nil
	case "forever", "indefinite", "-1":
		return Retention{Indefinite: true}, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return Retention{}, err
	}
	if d <= 0 {
		return Retention{}, nil
	}
	return Retention{Duration: d}, nil
}

// retainUntilFor resolves the retain-until value for a PUT: an explicit request
// header wins, otherwise the bucket default. It returns "" when the object is
// not retained.
func (r Retention) retainUntilFor(header string, now time.Time) (string, error) {
	if header != "" {
		return parseRetainUntil(header, now)
	}
	if r.Indefinite {
		return retainIndefinite, nil
	}
	if r.Duration > 0 {
		return now.Add(r.Duration).UTC().Format(time.RFC3339Nano), nil
	}
	return "", nil
}

// parseRetainUntil parses an S3 x-amz-object-lock-retain-until-date value and
// normalises it to RFC3339Nano UTC. A date that is not in the future is
// refused, as S3 refuses it.
func parseRetainUntil(v string, now time.Time) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, v); err2 == nil {
			t, err = t2, nil
		}
	}
	if err != nil {
		return "", apiErrf(codeInvalidArgument, "invalid x-amz-object-lock-retain-until-date %q", v)
	}
	if !t.After(now) {
		return "", apiErrf(codeInvalidArgument, "the retain-until date must be in the future")
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// parseLegalHold parses an x-amz-object-lock-legal-hold value. The second
// result reports whether the header was present at all.
func parseLegalHold(v string) (hold, present bool, err error) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "":
		return false, false, nil
	case legalHoldOn:
		return true, true, nil
	case legalHoldOff:
		return false, true, nil
	}
	return false, false, apiErrf(codeInvalidArgument, "invalid x-amz-object-lock-legal-hold %q (want ON or OFF)", v)
}
