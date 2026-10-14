// Package fsck implements read-only verification. It modifies not a single
// byte: verify and inspect open read-only and repair nothing.
package fsck

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/log"
	"github.com/infinityabundance/orfs-go/internal/wm"
)

// Check is one named verification check.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Report is the outcome of verification.
type Report struct {
	Volume           string  `json:"volume"`
	FormatVersion    uint16  `json:"log_format_version"`
	UUID             string  `json:"uuid"`
	CommittedRoot    uint64  `json:"committed_root"`
	CommittedRecords uint64  `json:"committed_records"`
	TransactionCount uint64  `json:"transaction_count"`
	Segments         int     `json:"segments"`
	PhysicalBytes    uint64  `json:"physical_bytes"`
	TailBytes        uint64  `json:"tail_abandoned_bytes"`
	TailKind         string  `json:"tail_kind,omitempty"`
	Checks           []Check `json:"checks"`
	Deep             bool    `json:"deep"`

	// Deep-only fields.
	Inodes   int `json:"inodes,omitempty"`
	Dentries int `json:"dentries,omitempty"`
}

// Verify scans and validates a volume read-only, optionally deeply.
func Verify(backing string, deep bool) (*Report, error) {
	dir := log.Dir(backing)
	scan, err := log.ScanLog(dir)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		Volume:           backing,
		FormatVersion:    scan.Genesis.FormatVersion,
		UUID:             hex.EncodeToString(scan.Genesis.UUID[:]),
		CommittedRoot:    scan.CommittedRoot,
		CommittedRecords: scan.CommittedRecordCount,
		Segments:         len(scan.Extents),
		PhysicalBytes:    scan.PhysicalEnd,
		Deep:             deep,
	}
	if len(scan.Boundaries) > 0 {
		rep.TransactionCount = uint64(len(scan.Boundaries) - 1)
	}
	if scan.Tail != nil {
		rep.TailBytes = scan.Tail.AbandonedBytes
		rep.TailKind = scan.Tail.Kind.String()
	}

	rep.Checks = append(rep.Checks, Check{Name: "framing", OK: true, Detail: "every committed record framed and checksummed"})
	rep.Checks = append(rep.Checks, Check{Name: "transactions", OK: true, Detail: "every committed bracket validated"})

	// Watermark cross-check.
	if _, err := wm.Validate(dir, scan.CommittedRoot); err != nil {
		rep.Checks = append(rep.Checks, Check{Name: "watermark", OK: false, Detail: err.Error()})
	} else {
		rep.Checks = append(rep.Checks, Check{Name: "watermark", OK: true})
	}

	if deep {
		idx, err := index.Replay(dir, scan.Extents, scan.CommittedRoot)
		if err != nil {
			rep.Checks = append(rep.Checks, Check{Name: "replay", OK: false, Detail: err.Error()})
		} else {
			rep.Inodes = len(idx.Inodes)
			rep.Dentries = len(idx.Dentry)
			rep.Checks = append(rep.Checks, Check{Name: "replay", OK: true,
				Detail: fmt.Sprintf("%d inode(s), %d dentr(ies)", len(idx.Inodes), len(idx.Dentry))})
		}
	}
	return rep, nil
}

// Passed reports whether every check passed.
func (r *Report) Passed() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// JSON renders the report.
func (r *Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Text renders the report.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "volume:            %s\n", r.Volume)
	fmt.Fprintf(&b, "log format ver:    %d\n", r.FormatVersion)
	fmt.Fprintf(&b, "uuid:              %s\n", r.UUID)
	fmt.Fprintf(&b, "committed root:    %d\n", r.CommittedRoot)
	fmt.Fprintf(&b, "committed records: %d\n", r.CommittedRecords)
	fmt.Fprintf(&b, "committed txns:    %d\n", r.TransactionCount)
	fmt.Fprintf(&b, "segments:          %d\n", r.Segments)
	if r.TailBytes > 0 {
		fmt.Fprintf(&b, "abandoned tail:    %d byte(s) (%s)\n", r.TailBytes, r.TailKind)
	}
	if r.Deep {
		fmt.Fprintf(&b, "inodes:            %d\n", r.Inodes)
		fmt.Fprintf(&b, "dentries:          %d\n", r.Dentries)
	}
	b.WriteString("checks:\n")
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAIL"
		}
		if c.Detail != "" {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", mark, c.Name, c.Detail)
		} else {
			fmt.Fprintf(&b, "  [%s] %s\n", mark, c.Name)
		}
	}
	return b.String()
}
