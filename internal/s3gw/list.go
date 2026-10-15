package s3gw

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/view"
)

// LIST Objects (v1 and v2).
//
// The listing is computed from a single immutable read view: every object key
// under the bucket root is collected, sorted, and then filtered by prefix and
// folded by delimiter. It is O(all keys in the bucket), not an index lookup —
// an honest cost of the minimal surface, not a claim.

const (
	defaultMaxKeys = 1000
	maxMaxKeys     = 1000
)

type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         uint64 `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Marker                string         `xml:"Marker,omitempty"`
	NextMarker            string         `xml:"NextMarker,omitempty"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	KeyCount              int            `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	Contents              []listObject   `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type listEntry struct {
	key      string
	isPrefix bool
}

// listObjects serves GET on a bucket without a sub-resource.
func (g *Gateway) listObjects(w http.ResponseWriter, r *http.Request, requestID string, b *bucket) {
	g.lists.Add(1)
	q := r.URL.Query()
	v2 := q.Get("list-type") == "2"
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	encoding := q.Get("encoding-type")
	if encoding != "" && encoding != "url" {
		g.writeErr(w, r, requestID, apiErrf(codeInvalidArgument, "unsupported encoding-type %q", encoding))
		return
	}

	maxKeys := defaultMaxKeys
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			g.writeErr(w, r, requestID, apiErrf(codeInvalidArgument, "invalid max-keys %q", v))
			return
		}
		if n < maxKeys {
			maxKeys = n
		}
	}

	var token, startAfter string
	if v2 {
		token = q.Get("continuation-token")
		startAfter = q.Get("start-after")
		if token == "" {
			token = startAfter
		}
	} else {
		token = q.Get("marker")
	}

	v := b.vs.Vol.View()
	keys := collectKeys(v, b.root, "")
	sort.Strings(keys)

	var entries []listEntry
	seen := map[string]bool{}
	for _, k := range keys {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				cp := k[:len(prefix)+i+len(delimiter)]
				if !seen[cp] {
					seen[cp] = true
					entries = append(entries, listEntry{key: cp, isPrefix: true})
				}
				continue
			}
		}
		entries = append(entries, listEntry{key: k})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	var candidates []listEntry
	for _, e := range entries {
		if token != "" && e.key <= token {
			continue
		}
		candidates = append(candidates, e)
	}
	truncated := len(candidates) > maxKeys
	if truncated {
		candidates = candidates[:maxKeys]
	}

	out := listBucketResult{
		Xmlns:        s3XMLNS,
		Name:         b.name,
		Prefix:       encodeMaybe(prefix, encoding),
		Delimiter:    encodeMaybe(delimiter, encoding),
		MaxKeys:      maxKeys,
		KeyCount:     len(candidates),
		IsTruncated:  truncated,
		EncodingType: encoding,
	}
	if v2 {
		out.ContinuationToken = token
		if startAfter != "" {
			out.StartAfter = encodeMaybe(startAfter, encoding)
		}
		if truncated && len(candidates) > 0 {
			out.NextContinuationToken = candidates[len(candidates)-1].key
		}
	} else {
		out.Marker = encodeMaybe(q.Get("marker"), encoding)
		if truncated && len(candidates) > 0 {
			out.NextMarker = candidates[len(candidates)-1].key
		}
	}

	for _, e := range candidates {
		if e.isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: encodeMaybe(e.key, encoding)})
			continue
		}
		obj := listObject{
			Key:          encodeMaybe(e.key, encoding),
			StorageClass: "STANDARD",
			ETag:         `""`,
		}
		if in, ok := lookupKey(v, b.root, e.key); ok {
			obj.Size = in.Size
			obj.LastModified = timespecToTime(in.Mtime).UTC().Format("2006-01-02T15:04:05.000Z")
			if meta := g.objectMetaOf(in); meta.ETag != "" {
				obj.ETag = meta.ETag
			}
		}
		out.Contents = append(out.Contents, obj)
	}
	writeXML(w, http.StatusOK, out)
}

// collectKeys walks a directory subtree and returns every regular-file key
// below it, with dirPrefix prepended.
func collectKeys(v *view.View, dir uint64, dirPrefix string) []string {
	var out []string
	var walk func(dir uint64, prefix string)
	walk = func(dir uint64, prefix string) {
		for _, e := range v.Children(dir) {
			in, ok := v.Inode(e.Child)
			if !ok {
				continue
			}
			full := e.Name
			if prefix != "" {
				full = prefix + "/" + e.Name
			}
			if in.Kind == record.KindDir {
				walk(e.Child, full)
				continue
			}
			out = append(out, full)
		}
	}
	walk(dir, dirPrefix)
	return out
}

// lookupKey resolves a full key within a bucket root in a read view.
func lookupKey(v *view.View, root uint64, key string) (*index.Inode, bool) {
	parts, err := splitKey(key)
	if err != nil {
		return nil, false
	}
	cur := root
	for i, comp := range parts {
		in, ok := v.Lookup(cur, comp)
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return in, true
		}
		if in.Kind != record.KindDir {
			return nil, false
		}
		cur = in.ID
	}
	return nil, false
}

// encodeMaybe percent-encodes a value when the client asked for
// encoding-type=url, preserving "/" so that prefix and key structure survive.
func encodeMaybe(s, encoding string) string {
	if encoding != "url" || s == "" {
		return s
	}
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		segs[i] = awsEncode(seg)
	}
	return strings.Join(segs, "/")
}
