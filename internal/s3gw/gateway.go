// Package s3gw implements a deliberately minimal, dependency-free WORM S3
// gateway over an OrFS volume.
//
// The gateway exists to serve the workload OrFS is designed for — an
// append-only audit/log sink — to ordinary S3 clients (aws-cli, s3fs, a Loki
// S3 client) without going through FUSE. An object PUT becomes an ordinary
// authoritative OrFS append; an object GET is served through the manifested
// read path (internal/manifest) so it is cache-admitted rather than a raw log
// scan; a DELETE of an object under retention or legal hold is refused with
// 403, because that refusal *is* the WORM guarantee.
//
// Scope is deliberately narrow: single-shot PUT only (no multipart), no
// versioning, no lifecycle, no SSE, no ACLs beyond a fixed WORM policy, and no
// presigned URLs. It is written against net/http and the standard library, in
// the same hand-rolled taste as internal/metrics.
//
// Two guarantees compose here, and it is worth being exact about which is
// which:
//
//   - OrFS's Law I (non-withdrawal) already means committed bytes are never
//     overwritten, truncated, reused, or garbage-collected. Nothing this
//     gateway can do changes that.
//   - This gateway adds the name-level WORM policy: a lock state recorded as an
//     authoritative transaction on the object's inode, enforced on PUT
//     (no overwrite) and DELETE (no withdrawal). See worm.go.
//
// An unlocked object's DELETE unlinks the name. The bytes remain in the log —
// that is Law I, not a durability claim about this gateway.
package s3gw

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/infinityabundance/orfs-go/internal/namespace"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// defaultMaxObjectBytes is the largest single PUT body the gateway accepts. It
// is the largest payload a single DATA_EXTENT record admits (there is no
// multipart), and it bounds the gateway's per-request memory to one object.
const defaultMaxObjectBytes int64 = 64 << 20

// s3XMLNS is the S3 response namespace every XML document carries.
const s3XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// Config configures a Gateway.
type Config struct {
	// Default is the volume serving buckets that have no explicit mapping. A
	// bucket on the default volume is a directory at that volume's root,
	// created on first use.
	Default *VolumeSet
	// Buckets maps an S3 bucket name to a dedicated OrFS volume; a mapped
	// bucket's objects live at that volume's root inode.
	Buckets map[string]*VolumeSet
	// Credential is the single access-key/secret-key pair SigV4 is verified
	// against. An empty AccessKey makes every request fail closed.
	Credential Credential
	// Region is the SigV4 region and the reported bucket location.
	Region string
	// UID and GID own every object and directory the gateway creates.
	UID, GID uint32
	// DefaultRetention is the bucket-default WORM retention.
	DefaultRetention Retention
	// DefaultLegalHold applies when a request sends no legal-hold header.
	DefaultLegalHold bool
	// PublishManifests makes a PUT publish a manifested body for the object it
	// just wrote, so the object is immediately cache-admitted on read. It costs
	// one EKBLOOM encode and two files under .or_bloom/ per PUT.
	PublishManifests bool
	// MetricsRequireAuth requires a SigV4 signature on /metrics as well.
	MetricsRequireAuth bool
	// MaxObjectBytes bounds a single PUT body; zero selects 64 MiB.
	MaxObjectBytes int64
	// Now is the gateway clock; nil selects time.Now.
	Now func() time.Time
}

// Gateway is an http.Handler serving the WORM S3 subset over OrFS.
type Gateway struct {
	cfg  Config
	now  func() time.Time
	cred namespace.Credential

	puts, gets, heads, deletes, lists  atomic.Uint64
	refusedRetention, refusedLegalHold atomic.Uint64
}

// New builds a Gateway. Only the fields of cfg actually used are required; a
// nil Default with no Buckets makes every bucket request a NoSuchBucket.
func New(cfg Config) *Gateway {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.MaxObjectBytes <= 0 {
		cfg.MaxObjectBytes = defaultMaxObjectBytes
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Gateway{
		cfg:  cfg,
		now:  now,
		cred: namespace.Credential{UID: cfg.UID, GID: cfg.GID},
	}
}

// ServeHTTP routes one S3 request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", requestID)
	w.Header().Set("Server", "orfs-s3")

	// /metrics is reserved: the endpoint is exposed unauthenticated for a
	// Prometheus scrape unless MetricsRequireAuth is set.
	if r.URL.Path == "/metrics" || r.URL.Path == "/metrics/" {
		if g.cfg.MetricsRequireAuth {
			if _, err := verifyRequest(r, g.cfg.Credential, g.cfg.Region, g.now()); err != nil {
				g.writeErr(w, r, requestID, err)
				return
			}
		}
		g.handleMetrics(w, r)
		return
	}

	if _, err := verifyRequest(r, g.cfg.Credential, g.cfg.Region, g.now()); err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}

	bucketName, key := requestPath(r)
	if bucketName == "" {
		g.handleService(w, r, requestID)
		return
	}
	if err := validateBucketName(bucketName); err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	if key == "" {
		g.handleBucket(w, r, requestID, bucketName)
		return
	}
	g.handleObject(w, r, requestID, bucketName, key)
}

// writeErr renders any error as an S3 error document.
func (g *Gateway) writeErr(w http.ResponseWriter, r *http.Request, requestID string, err error) {
	code, message := s3ErrorOf(err)
	writeS3Error(w, r, requestID, code, message)
}

// requestPath splits the escaped request path into bucket and key, decoding
// each part. A key containing an encoded "/" is indistinguishable from a path
// separator after decoding; the mapping treats it as one.
func requestPath(r *http.Request) (bucket, key string) {
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	var be, ke string
	if i := strings.IndexByte(escaped, '/'); i < 0 {
		be = escaped
	} else {
		be, ke = escaped[:i], escaped[i+1:]
	}
	if d, err := url.PathUnescape(be); err == nil {
		be = d
	}
	if d, err := url.PathUnescape(ke); err == nil {
		ke = d
	}
	return be, ke
}

// validateBucketName enforces the S3 bucket-name subset this gateway can map to
// an OrFS directory name.
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return apiErrf(codeInvalidBucketName, "bucket name %q must be 3 to 63 characters", name)
	}
	if name[0] == '-' || name[len(name)-1] == '-' || name[0] == '.' || name[len(name)-1] == '.' {
		return apiErrf(codeInvalidBucketName, "bucket name %q must begin and end with a letter or digit", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.'
		if !ok {
			return apiErrf(codeInvalidBucketName, "bucket name %q may contain only lowercase letters, digits, hyphens and dots", name)
		}
	}
	if strings.Contains(name, "..") {
		return apiErrf(codeInvalidBucketName, "bucket name %q must not contain two adjacent dots", name)
	}
	return nil
}

// bucket is a resolved S3 bucket: a volume and the directory inode that is its
// root.
type bucket struct {
	name string
	vs   *VolumeSet
	root uint64
}

// resolveBucket finds a bucket, optionally creating it on the default volume.
func (g *Gateway) resolveBucket(name string, create bool) (*bucket, error) {
	if vs, ok := g.cfg.Buckets[name]; ok && vs != nil {
		return &bucket{name: name, vs: vs, root: record.RootInode}, nil
	}
	vs := g.cfg.Default
	if vs == nil {
		return nil, apiErrf(codeNoSuchBucket, "the bucket %q does not exist", name)
	}
	in, err := vs.NS.Lookup(g.cred, record.RootInode, name)
	if err == nil {
		if in.Kind != record.KindDir {
			return nil, apiErrf(codeNoSuchBucket, "the bucket %q does not exist", name)
		}
		return &bucket{name: name, vs: vs, root: in.ID}, nil
	}
	if !errors.Is(err, orfserr.ErrNotFound) {
		return nil, err
	}
	if !create {
		return nil, apiErrf(codeNoSuchBucket, "the bucket %q does not exist", name)
	}
	// Create is safe to race: the namespace refuses an existing name before
	// writing anything, so the loser of the race simply falls back to a lookup.
	created, err := vs.NS.Mkdir(g.cred, record.RootInode, name, 0o755, g.cfg.UID, g.cfg.GID)
	if err != nil {
		if errors.Is(err, orfserr.ErrExists) {
			if in, lerr := vs.NS.Lookup(g.cred, record.RootInode, name); lerr == nil && in.Kind == record.KindDir {
				return &bucket{name: name, vs: vs, root: in.ID}, nil
			}
		}
		return nil, err
	}
	return &bucket{name: name, vs: vs, root: created.ID}, nil
}

// bucketNames lists the addressable buckets: the mapped ones plus the
// directories at the default volume's root.
func (g *Gateway) bucketNames() []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for n := range g.cfg.Buckets {
		add(n)
	}
	if g.cfg.Default != nil {
		for _, e := range g.cfg.Default.Vol.View().Children(record.RootInode) {
			if in, ok := g.cfg.Default.Vol.View().Inode(e.Child); ok && in.Kind == record.KindDir {
				add(e.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "orfs-s3"
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// timespecToTime renders an OrFS timestamp.
func timespecToTime(ts record.Timespec) time.Time {
	return time.Unix(ts.Secs, int64(ts.Nanos)).UTC()
}
