// Package manifest implements Concealed Expansion (Law II) on top of the
// EKBLOOM codec.
//
// A manifested body is an EKBLOOM projection of a file's immutable bytes,
// written under .or_bloom/ with a small manifest that binds the body to an exact
// identity: (inode, inode version, committed root). A manifested read is served
// only when the identity matches the request exactly; otherwise the read falls
// back to the authoritative extent read, which always yields the same bytes.
//
// Authority establishes truth; a concealed structure only accelerates
// observation of it. The manifested plane is derived: deleting .or_bloom/ never
// changes the recoverable set (Law I).
package manifest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"lukechampine.com/blake3"

	ekbloom "github.com/infinityabundance/ekbloom-go"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/volume"
)

// BloomDirName is the concealed acceleration directory.
const BloomDirName = ".or_bloom"

// EKBloomVersion records the codec identity used to build a body.
const EKBloomVersion = "ekbloom-go"

// Manifest binds a manifested body to an exact identity.
type Manifest struct {
	Schema         string `json:"schema"`
	Inode          uint64 `json:"inode"`
	Version        uint64 `json:"inode_version"`
	Root           uint64 `json:"root"`
	LogicalLen     uint64 `json:"logical_len"`
	PhysicalBytes  uint64 `json:"physical_bytes"`
	BodyBlake3     string `json:"body_blake3"`
	LogicalBlake3  string `json:"logical_blake3"`
	EKBloomVersion string `json:"ekbloom_version"`
}

// LogicalDigest is the BLAKE3 of a file's logical content, used to bind a
// manifested body to the bytes authority holds *now*.
func LogicalDigest(content []byte) string {
	sum := blake3.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func bloomDir(vol *volume.Volume) string { return filepath.Join(vol.Backing, BloomDirName) }

func bodyPath(vol *volume.Volume, inode, version, root uint64) string {
	return filepath.Join(bloomDir(vol), "ekbloom", fmt.Sprintf("%d-v%d-r%d.ekb", inode, version, root))
}

func manifestPath(vol *volume.Volume, inode, version, root uint64) string {
	return filepath.Join(bloomDir(vol), "manifests", fmt.Sprintf("%d-v%d-r%d.json", inode, version, root))
}

// Publish builds a manifested body for content under an exact identity and
// writes it atomically. The body is a root-profile EKBLOOM projection.
func Publish(vol *volume.Volume, inode, version, root uint64, content []byte) (*Manifest, error) {
	body, err := ekbloom.WriteBody(1, content, ekbloom.Profile{Kind: ekbloom.ProfileRoot})
	if err != nil {
		return nil, orfserr.IO(err, "ekbloom write body")
	}
	sum := blake3.Sum256(body)
	m := &Manifest{
		Schema:         "orfs.manifest.v1",
		Inode:          inode,
		Version:        version,
		Root:           root,
		LogicalLen:     uint64(len(content)),
		PhysicalBytes:  uint64(len(body)),
		BodyBlake3:     hex.EncodeToString(sum[:]),
		LogicalBlake3:  LogicalDigest(content),
		EKBloomVersion: EKBloomVersion,
	}
	if err := writeAtomic(bodyPath(vol, inode, version, root), body); err != nil {
		return nil, err
	}
	meta, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, orfserr.IO(err, "encode manifest")
	}
	if err := writeAtomic(manifestPath(vol, inode, version, root), meta); err != nil {
		return nil, err
	}
	return m, nil
}

// TryRead serves a read from a manifested body when the identity matches the
// request exactly and the body's self-digest verifies. It returns ok=false when
// there is no certified body, so the caller falls back to authority.
func TryRead(vol *volume.Volume, inode, version, root, offset, size uint64) ([]byte, bool) {
	m, body, ok := loadCertified(vol, inode, version, root)
	if !ok {
		return nil, false
	}
	if offset >= m.LogicalLen {
		return []byte{}, true
	}
	r, err := ekbloom.Open(body)
	if err != nil {
		return nil, false
	}
	want := size
	if offset+want > m.LogicalLen {
		want = m.LogicalLen - offset
	}
	out, err := r.ReadAt(offset, want)
	if err != nil {
		return nil, false
	}
	return out, true
}

// loadCertified loads a manifest for the exact identity and verifies the body's
// self-digest.
func loadCertified(vol *volume.Volume, inode, version, root uint64) (*Manifest, []byte, bool) {
	raw, err := os.ReadFile(manifestPath(vol, inode, version, root))
	if err != nil {
		return nil, nil, false
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, false
	}
	if m.Inode != inode || m.Version != version || m.Root != root || m.EKBloomVersion != EKBloomVersion {
		return nil, nil, false
	}
	body, err := os.ReadFile(bodyPath(vol, inode, version, root))
	if err != nil {
		return nil, nil, false
	}
	sum := blake3.Sum256(body)
	if hex.EncodeToString(sum[:]) != m.BodyBlake3 {
		return nil, nil, false
	}
	return &m, body, true
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return orfserr.IO(err, "create bloom directory")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return orfserr.IO(err, "write %s", tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		return orfserr.IO(err, "rename %s", path)
	}
	return nil
}
