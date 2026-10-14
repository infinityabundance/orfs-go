// Package orfserr defines the typed error vocabulary of OrFS-GO.
//
// Every failure is an *Error carrying a Kind, so callers branch on structure
// rather than on message text. This mirrors the Rust reference's OrfsError enum
// and its errno mapping (src/error.rs), and is what makes the CLI honest: an
// operation returns the documented errno or refuses, and never returns a wrong
// success.
package orfserr

import (
	"errors"
	"fmt"
	"syscall"
)

// Kind classifies an Error.
type Kind int

// Error kinds. The zero value is KindUnknown so a zero Error is inert.
const (
	KindUnknown Kind = iota

	// Framing / parsing.

	// KindTorn is a short or checksum-failing frame at the tail of the log: the
	// bytes are an incomplete (uncommitted) record, not corruption.
	KindTorn
	// KindCorrupt is a well-framed record that violates the format.
	KindCorrupt
	// KindCompatibility is a record whose format this build does not implement;
	// the bytes are never interpreted on a guess.
	KindCompatibility
	// KindUnsupportedFormatVersion is a genesis that names a log format version
	// higher than this build supports. It is detected at open.
	KindUnsupportedFormatVersion
	// KindOverflow is checked arithmetic that did not fit.
	KindOverflow

	// Store / capacity.

	// KindNoSpace is authoritative exhaustion surfaced by an explicit bound, a
	// hard quota, or an OS ENOSPC/EIO. It latches the volume read-only.
	KindNoSpace
	// KindCapacityExhausted is the pre-flight capacity-guard refusal. It is
	// returned *before* any byte is written and does not by itself latch the
	// volume (a smaller transaction that still clears the reserve may commit).
	KindCapacityExhausted
	// KindReadOnly is returned for a mutation on a degraded (latched) or
	// read-only volume.
	KindReadOnly
	// KindIo wraps an underlying I/O failure.
	KindIo

	// Namespace / POSIX.

	KindNotFound
	KindExists
	KindNotDir
	KindIsDir
	KindNotEmpty
	KindInvalid
	// KindNameTooLong is a path component longer than NAME_MAX (ENAMETOOLONG),
	// distinct from a generic invalid argument.
	KindNameTooLong
	// KindAccess is a denial by the permission-bit model (the owner/group/other
	// class check), which POSIX surfaces as EACCES.
	KindAccess
	// KindPermission is a denial for a reason other than the permission bits
	// (ownership, the sticky-directory rule, hard-linking a directory, the
	// chgrp policy), which the kernel surfaces as EPERM.
	KindPermission
	KindNotSupported

	// KindUnimplemented is part of the target design but not wired in this build.
	KindUnimplemented
)

// Error is the single typed error value returned across OrFS-GO.
type Error struct {
	Kind Kind
	Msg  string
	Err  error

	// CapacityExhausted carries the admission arithmetic.
	Needed     uint64
	Admissible uint64

	// UnsupportedFormatVersion carries the version fields.
	Found     uint16
	Supported uint16

	// Phase names the owning build phase for KindUnimplemented.
	Phase string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	prefix := e.Kind.String()
	if prefix == "" {
		if e.Msg != "" {
			return e.Msg
		}
		return "orfs error"
	}
	if e.Msg == "" {
		return prefix
	}
	return prefix + ": " + e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is an *Error of the same Kind, so errors.Is works
// against typed sentinels.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Kind == t.Kind
}

// String names the kind in the same register as the reference.
func (k Kind) String() string {
	switch k {
	case KindUnknown:
		return "orfs"
	case KindTorn:
		return "torn record"
	case KindCorrupt:
		return "corrupt"
	case KindCompatibility:
		return "compatibility"
	case KindUnsupportedFormatVersion:
		return "unsupported format version"
	case KindOverflow:
		return "overflow"
	case KindNoSpace:
		return "no space"
	case KindCapacityExhausted:
		return "capacity guard refusal"
	case KindReadOnly:
		return "read-only"
	case KindIo:
		return "io"
	case KindNotFound:
		return "not found"
	case KindExists:
		return "already exists"
	case KindNotDir:
		return "not a directory"
	case KindIsDir:
		return "is a directory"
	case KindNotEmpty:
		return "directory not empty"
	case KindInvalid:
		return "invalid argument"
	case KindNameTooLong:
		return "file name too long"
	case KindAccess:
		return "access denied"
	case KindPermission:
		return "operation not permitted"
	case KindNotSupported:
		return "operation not supported"
	case KindUnimplemented:
		return "not implemented"
	}
	return "orfs"
}

// Errno maps a Kind to the documented errno, mirroring the reference so a FUSE
// layer can surface the exact code.
func (e *Error) Errno() syscall.Errno {
	switch e.Kind {
	case KindTorn, KindCorrupt, KindCompatibility:
		return syscall.EIO
	case KindUnsupportedFormatVersion:
		return syscall.EPROTONOSUPPORT
	case KindOverflow:
		return syscall.EOVERFLOW
	case KindNoSpace, KindCapacityExhausted:
		return syscall.ENOSPC
	case KindReadOnly:
		return syscall.EROFS
	case KindIo:
		return syscall.EIO
	case KindNotFound:
		return syscall.ENOENT
	case KindExists:
		return syscall.EEXIST
	case KindNotDir:
		return syscall.ENOTDIR
	case KindIsDir:
		return syscall.EISDIR
	case KindNotEmpty:
		return syscall.ENOTEMPTY
	case KindInvalid:
		return syscall.EINVAL
	case KindNameTooLong:
		return syscall.ENAMETOOLONG
	case KindAccess:
		return syscall.EACCES
	case KindPermission:
		return syscall.EPERM
	case KindNotSupported:
		return syscall.EOPNOTSUPP
	case KindUnimplemented:
		return syscall.ENOSYS
	}
	return syscall.EIO
}

// Constructors.

func Corrupt(format string, a ...any) *Error {
	return &Error{Kind: KindCorrupt, Msg: fmt.Sprintf(format, a...)}
}

func Torn(format string, a ...any) *Error {
	return &Error{Kind: KindTorn, Msg: fmt.Sprintf(format, a...)}
}

func Compatibility(format string, a ...any) *Error {
	return &Error{Kind: KindCompatibility, Msg: fmt.Sprintf(format, a...)}
}

func Overflow(format string, a ...any) *Error {
	return &Error{Kind: KindOverflow, Msg: fmt.Sprintf(format, a...)}
}

func Invalid(format string, a ...any) *Error {
	return &Error{Kind: KindInvalid, Msg: fmt.Sprintf(format, a...)}
}

func NotFound(format string, a ...any) *Error {
	return &Error{Kind: KindNotFound, Msg: fmt.Sprintf(format, a...)}
}

func Exists(format string, a ...any) *Error {
	return &Error{Kind: KindExists, Msg: fmt.Sprintf(format, a...)}
}

func NotDir(format string, a ...any) *Error {
	return &Error{Kind: KindNotDir, Msg: fmt.Sprintf(format, a...)}
}

func IsDir(format string, a ...any) *Error {
	return &Error{Kind: KindIsDir, Msg: fmt.Sprintf(format, a...)}
}

func NotEmpty(format string, a ...any) *Error {
	return &Error{Kind: KindNotEmpty, Msg: fmt.Sprintf(format, a...)}
}

// NameTooLong builds the POSIX ENAMETOOLONG refusal for an over-long component.
func NameTooLong(format string, a ...any) *Error {
	return &Error{Kind: KindNameTooLong, Msg: fmt.Sprintf(format, a...)}
}

// Access builds the EACCES refusal of the permission-bit model.
func Access(format string, a ...any) *Error {
	return &Error{Kind: KindAccess, Msg: fmt.Sprintf(format, a...)}
}

// Permission builds the EPERM refusal for a denial that is not a
// permission-bit class check (ownership, sticky rule, chgrp policy).
func Permission(format string, a ...any) *Error {
	return &Error{Kind: KindPermission, Msg: fmt.Sprintf(format, a...)}
}

func NotSupported(format string, a ...any) *Error {
	return &Error{Kind: KindNotSupported, Msg: fmt.Sprintf(format, a...)}
}

func ReadOnly(format string, a ...any) *Error {
	return &Error{Kind: KindReadOnly, Msg: fmt.Sprintf(format, a...)}
}

func IO(err error, format string, a ...any) *Error {
	msg := fmt.Sprintf(format, a...)
	if err != nil {
		msg = msg + ": " + err.Error()
	}
	return &Error{Kind: KindIo, Msg: msg, Err: err}
}

// Exhausted builds the typed capacity-guard refusal. It is ENOSPC-typed but
// does not by itself latch the volume.
func Exhausted(needed, admissible uint64) *Error {
	return &Error{
		Kind:       KindCapacityExhausted,
		Msg:        fmt.Sprintf("%d byte(s) needed, %d admissible before the reserve", needed, admissible),
		Needed:     needed,
		Admissible: admissible,
	}
}

// NoSpaceErr builds the latching authoritative exhaustion error.
func NoSpaceErr(format string, a ...any) *Error {
	return &Error{Kind: KindNoSpace, Msg: fmt.Sprintf(format, a...)}
}

// UnsupportedFormat builds the "unknown higher format version" refusal.
func UnsupportedFormat(found, supported uint16) *Error {
	return &Error{
		Kind:      KindUnsupportedFormatVersion,
		Msg:       fmt.Sprintf("log format version %d is newer than this build supports (%d)", found, supported),
		Found:     found,
		Supported: supported,
	}
}

// Unimplemented builds the honest "not wired in this build phase" error.
func Unimplemented(phase, what string) *Error {
	return &Error{
		Kind:  KindUnimplemented,
		Msg:   fmt.Sprintf("%s (owning phase: %s)", what, phase),
		Phase: phase,
	}
}

// ErrnoOf extracts the errno for any error, defaulting to EIO.
func ErrnoOf(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Errno()
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno
	}
	return syscall.EIO
}

// Sentinel values for errors.Is.
var (
	ErrTorn          = &Error{Kind: KindTorn}
	ErrCorrupt       = &Error{Kind: KindCorrupt}
	ErrCompat        = &Error{Kind: KindCompatibility}
	ErrUnsupported   = &Error{Kind: KindUnsupportedFormatVersion}
	ErrOverflow      = &Error{Kind: KindOverflow}
	ErrNoSpace       = &Error{Kind: KindNoSpace}
	ErrExhausted     = &Error{Kind: KindCapacityExhausted}
	ErrReadOnly      = &Error{Kind: KindReadOnly}
	ErrIO            = &Error{Kind: KindIo}
	ErrNotFound      = &Error{Kind: KindNotFound}
	ErrExists        = &Error{Kind: KindExists}
	ErrNotDir        = &Error{Kind: KindNotDir}
	ErrIsDir         = &Error{Kind: KindIsDir}
	ErrNotEmpty      = &Error{Kind: KindNotEmpty}
	ErrInvalid       = &Error{Kind: KindInvalid}
	ErrNameTooLong   = &Error{Kind: KindNameTooLong}
	ErrAccess        = &Error{Kind: KindAccess}
	ErrPermission    = &Error{Kind: KindPermission}
	ErrNotSupported  = &Error{Kind: KindNotSupported}
	ErrUnimplemented = &Error{Kind: KindUnimplemented}
)
