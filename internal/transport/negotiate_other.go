//go:build !linux

package transport

import (
	"errors"
	"time"
)

// Negotiation is the empirical result of attempting the FUSE INIT handshake.
// On non-Linux platforms there is no FUSE transport to probe.
type Negotiation struct {
	Error string `json:"error,omitempty"`
}

// Negotiate is unavailable off Linux: OrFS serves through Linux FUSE only.
func Negotiate(mnt string, timeout time.Duration) (*Negotiation, error) {
	return &Negotiation{Error: "FUSE transport negotiation is Linux-only"},
		errors.New("FUSE transport negotiation is Linux-only")
}
