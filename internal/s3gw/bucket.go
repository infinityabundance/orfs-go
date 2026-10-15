package s3gw

import (
	"encoding/xml"
	"math"
	"net/http"
	"time"

	"github.com/infinityabundance/orfs-go/internal/record"
)

// Service-level and bucket-level operations.

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketInfo struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   owner    `xml:"Owner"`
	Buckets struct {
		Bucket []bucketInfo `xml:"Bucket"`
	} `xml:"Buckets"`
}

// handleService serves GET / (ListBuckets). Everything else on / is refused.
func (g *Gateway) handleService(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodGet {
		g.writeErr(w, r, requestID, apiErrf(codeMethodNotAllowed, "%s is not allowed on the service root", r.Method))
		return
	}
	doc := listAllMyBucketsResult{Xmlns: s3XMLNS}
	doc.Owner = owner{ID: "orfs", DisplayName: "orfs"}
	for _, name := range g.bucketNames() {
		doc.Buckets.Bucket = append(doc.Buckets.Bucket, bucketInfo{
			Name:         name,
			CreationDate: g.bucketCreationDate(name),
		})
	}
	writeXML(w, http.StatusOK, doc)
}

func (g *Gateway) bucketCreationDate(name string) string {
	if vs, ok := g.cfg.Buckets[name]; ok && vs != nil {
		return time.Unix(int64(vs.Vol.Meta.CreatedAtUnixSecs), 0).UTC().Format(time.RFC3339)
	}
	if g.cfg.Default != nil {
		if in, ok := g.cfg.Default.Vol.View().Lookup(record.RootInode, name); ok {
			return timespecToTime(in.Mtime).Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// handleBucket dispatches a request whose path names only a bucket.
func (g *Gateway) handleBucket(w http.ResponseWriter, r *http.Request, requestID, name string) {
	switch r.Method {
	case http.MethodPut:
		g.createBucket(w, r, requestID, name)
	case http.MethodHead:
		g.headBucket(w, r, requestID, name)
	case http.MethodDelete:
		g.deleteBucket(w, r, requestID, name)
	case http.MethodGet:
		g.getBucket(w, r, requestID, name)
	case http.MethodPost:
		g.writeErr(w, r, requestID, apiErrf(codeNotImplemented, "bucket POST operations (multi-object delete) are not implemented"))
	default:
		g.writeErr(w, r, requestID, apiErrf(codeMethodNotAllowed, "%s is not allowed on a bucket", r.Method))
	}
}

// createBucket creates the bucket, or succeeds idempotently when it already
// exists. S3 answers BucketAlreadyOwnedByYou in that case; this gateway treats
// it as success so an unconditional bucket-create in a client is harmless.
func (g *Gateway) createBucket(w http.ResponseWriter, r *http.Request, requestID, name string) {
	if _, err := g.resolveBucket(name, true); err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) headBucket(w http.ResponseWriter, r *http.Request, requestID, name string) {
	if _, err := g.resolveBucket(name, false); err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// deleteBucket refuses. A WORM store does not withdraw a bucket, and no client
// needs it; the refusal is explicit rather than a silent no-op.
func (g *Gateway) deleteBucket(w http.ResponseWriter, r *http.Request, requestID, name string) {
	if _, err := g.resolveBucket(name, false); err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	g.writeErr(w, r, requestID, apiErrf(codeAccessDenied,
		"bucket deletion is refused: this gateway is WORM and never withdraws a bucket or its committed history"))
}

// getBucket dispatches the GET-on-bucket sub-resources and the default list.
func (g *Gateway) getBucket(w http.ResponseWriter, r *http.Request, requestID, name string) {
	b, err := g.resolveBucket(name, false)
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	q := r.URL.Query()
	switch {
	case has(q, "location"):
		g.getBucketLocation(w, name)
	case has(q, "object-lock"):
		g.getObjectLockConfiguration(w, name)
	case has(q, "versioning"):
		writeXML(w, http.StatusOK, versioningConfiguration{Xmlns: s3XMLNS})
	case has(q, "uploads"), has(q, "uploadId"):
		g.writeErr(w, r, requestID, apiErrf(codeNotImplemented, "multipart upload is not implemented"))
	default:
		g.listObjects(w, r, requestID, b)
	}
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

func (g *Gateway) getBucketLocation(w http.ResponseWriter, name string) {
	// S3 reports an empty LocationConstraint for us-east-1.
	region := g.cfg.Region
	if region == "us-east-1" {
		region = ""
	}
	writeXML(w, http.StatusOK, locationConstraint{Xmlns: s3XMLNS, Value: region})
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
	Status  string   `xml:"Status,omitempty"`
}

type objectLockConfiguration struct {
	XMLName xml.Name `xml:"ObjectLockConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
	// ObjectLockEnabled is "Enabled".
	ObjectLockEnabled string          `xml:"ObjectLockEnabled"`
	Rule              *objectLockRule `xml:"Rule,omitempty"`
}

type objectLockRule struct {
	DefaultRetention defaultRetention `xml:"DefaultRetention"`
}

type defaultRetention struct {
	Mode  string `xml:"Mode"`
	Days  int    `xml:"Days,omitempty"`
	Years int    `xml:"Years,omitempty"`
}

// getObjectLockConfiguration reports the bucket's default WORM retention. When
// the default is indefinite there is no finite Days value to report, so the
// Rule element is omitted and only the Enabled flag is returned — the gateway
// does not invent a number.
func (g *Gateway) getObjectLockConfiguration(w http.ResponseWriter, name string) {
	doc := objectLockConfiguration{Xmlns: s3XMLNS, ObjectLockEnabled: "Enabled"}
	if d := g.cfg.DefaultRetention.Duration; d > 0 && !g.cfg.DefaultRetention.Indefinite {
		doc.Rule = &objectLockRule{DefaultRetention: defaultRetention{
			Mode: "COMPLIANCE",
			Days: int(math.Ceil(d.Hours() / 24)),
		}}
	}
	writeXML(w, http.StatusOK, doc)
}

func has(q map[string][]string, key string) bool { _, ok := q[key]; return ok }
