package namespace

import (
	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// Credential is the caller's identity, including supplementary groups.
type Credential struct {
	UID    uint32
	GID    uint32
	Groups []uint32
}

// HasPermission is the single POSIX class-based rule, mirroring the reference's
// inode::has_permission_grouped. Root bypasses read/write; execute on a regular
// file needs at least one x bit; execute on a directory is always allowed. The
// group class matches the effective group or any supplementary group.
func HasPermission(cred Credential, in *index.Inode, mask uint32) bool {
	perm := in.Mode & 0o777
	if cred.UID == 0 {
		if mask&0o1 != 0 && in.Kind == record.KindFile {
			return perm&0o111 != 0
		}
		return true
	}
	class := (perm >> permClassGroups(cred, in)) & 0o7
	return (class & mask) == mask
}

func permClassGroups(cred Credential, in *index.Inode) uint {
	if cred.UID == in.UID {
		return 6
	}
	if cred.GID == in.GID {
		return 3
	}
	for _, g := range cred.Groups {
		if g == in.GID {
			return 3
		}
	}
	return 0
}

// IsOwner reports whether the caller owns the inode (for sticky-bit checks).
func IsOwner(cred Credential, in *index.Inode) bool {
	return cred.UID == 0 || cred.UID == in.UID
}
