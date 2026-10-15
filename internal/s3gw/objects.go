package s3gw

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infinityabundance/orfs-go/internal/index"
	"github.com/infinityabundance/orfs-go/internal/orfserr"
	"github.com/infinityabundance/orfs-go/internal/record"
)

// Object operations: PUT, GET, HEAD, DELETE, and the object-to-OrFS mapping.
//
// An object key maps onto the OrFS namespace as a path: the components become
// directories and the final component is a regular file. A key ending in "/"
// maps onto a directory (the marker s3fs creates), so clients that materialise
// directories keep working. Consequence, documented rather than hidden: keys
// "a" and "a/b" cannot coexist here, because "a" would have to be both a file
// and a directory.

// handleObject dispatches an object request.
func (g *Gateway) handleObject(w http.ResponseWriter, r *http.Request, requestID, bucketName, key string) {
	q := r.URL.Query()
	if has(q, "uploadId") || has(q, "uploads") || has(q, "partNumber") {
		g.writeErr(w, r, requestID, apiErrf(codeNotImplemented, "multipart upload is not implemented"))
		return
	}
	b, err := g.resolveBucket(bucketName, r.Method == http.MethodPut)
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		g.putObject(w, r, requestID, b, key)
	case http.MethodGet:
		g.getObject(w, r, requestID, b, key, false)
	case http.MethodHead:
		g.getObject(w, r, requestID, b, key, true)
	case http.MethodDelete:
		g.deleteObject(w, r, requestID, b, key)
	case http.MethodPost:
		g.writeErr(w, r, requestID, apiErrf(codeNotImplemented, "object POST operations are not implemented"))
	default:
		g.writeErr(w, r, requestID, apiErrf(codeMethodNotAllowed, "%s is not allowed on an object", r.Method))
	}
}

// splitKey validates and splits an S3 key into namespace path components.
func splitKey(key string) ([]string, error) {
	if key == "" {
		return nil, apiErrf(codeInvalidArgument, "an object key must not be empty")
	}
	parts := strings.Split(key, "/")
	// A trailing "/" names a directory marker; the empty final component is the
	// marker, not a path element.
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return nil, apiErrf(codeInvalidArgument, "an object key must not be empty")
	}
	for _, p := range parts {
		switch {
		case p == "":
			return nil, apiErrf(codeInvalidArgument, "the key %q has an empty path component", key)
		case p == "." || p == "..":
			return nil, apiErrf(codeInvalidArgument, "the key %q has a reserved path component", key)
		case len(p) > record.NameMax:
			return nil, apiErrf(codeInvalidArgument, "the key %q has a component longer than %d bytes", key, record.NameMax)
		}
	}
	return parts, nil
}

// resolveKey returns the parent directory inode and the final component of key.
// Missing intermediate directories are created when create is set.
func (g *Gateway) resolveKey(b *bucket, key string, create bool) (uint64, string, error) {
	parts, err := splitKey(key)
	if err != nil {
		return 0, "", err
	}
	cur := b.root
	for _, comp := range parts[:len(parts)-1] {
		in, lerr := b.vs.NS.Lookup(g.cred, cur, comp)
		if lerr == nil {
			if in.Kind != record.KindDir {
				return 0, "", apiErrf(codeNoSuchKey, "the key %q does not exist", key)
			}
			cur = in.ID
			continue
		}
		if !errors.Is(lerr, orfserr.ErrNotFound) {
			return 0, "", lerr
		}
		if !create {
			return 0, "", apiErrf(codeNoSuchKey, "the key %q does not exist", key)
		}
		dir, merr := b.vs.NS.Mkdir(g.cred, cur, comp, 0o755, g.cfg.UID, g.cfg.GID)
		if merr != nil {
			if errors.Is(merr, orfserr.ErrExists) {
				if in, lerr2 := b.vs.NS.Lookup(g.cred, cur, comp); lerr2 == nil && in.Kind == record.KindDir {
					cur = in.ID
					continue
				}
			}
			return 0, "", merr
		}
		cur = dir.ID
	}
	return cur, parts[len(parts)-1], nil
}

// putObject implements single-shot PUT.
func (g *Gateway) putObject(w http.ResponseWriter, r *http.Request, requestID string, b *bucket, key string) {
	g.puts.Add(1)
	if r.ContentLength < 0 {
		g.writeErr(w, r, requestID, apiErrf(codeMissingContentLength, "a PUT must carry a Content-Length"))
		return
	}
	if r.ContentLength > g.cfg.MaxObjectBytes {
		g.writeErr(w, r, requestID, apiErrf(codeEntityTooLarge,
			"the object exceeds the %d-byte single-PUT limit (multipart is not implemented)", g.cfg.MaxObjectBytes))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, g.cfg.MaxObjectBytes+1))
	if err != nil {
		g.writeErr(w, r, requestID, apiErrf(codeInvalidRequest, "reading the request body: %v", err))
		return
	}
	if int64(len(body)) > g.cfg.MaxObjectBytes {
		g.writeErr(w, r, requestID, apiErrf(codeEntityTooLarge,
			"the object exceeds the %d-byte single-PUT limit", g.cfg.MaxObjectBytes))
		return
	}
	if declared := r.Header.Get("x-amz-content-sha256"); declared != "" && !strings.HasPrefix(declared, "STREAMING-") {
		if !strings.EqualFold(declared, hexSHA256(body)) {
			g.writeErr(w, r, requestID, apiErrf(codeContentSHA256Mismatch,
				"the request body's SHA-256 does not match x-amz-content-sha256"))
			return
		}
	}
	if cmd5 := r.Header.Get("Content-MD5"); cmd5 != "" {
		sum := md5.Sum(body)
		if base64.StdEncoding.EncodeToString(sum[:]) != cmd5 {
			g.writeErr(w, r, requestID, apiErrf(codeBadDigest,
				"the Content-MD5 you specified does not match what we received"))
			return
		}
	}

	now := g.now()
	retainUntil, err := g.cfg.DefaultRetention.retainUntilFor(r.Header.Get("x-amz-object-lock-retain-until-date"), now)
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	hold, holdPresent, err := parseLegalHold(r.Header.Get("x-amz-object-lock-legal-hold"))
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	if !holdPresent {
		hold = g.cfg.DefaultLegalHold
	}
	etag := etagOf(body)

	parent, name, err := g.resolveKey(b, key, true)
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}

	// A key ending in "/" is a directory marker.
	if strings.HasSuffix(key, "/") {
		if len(body) != 0 {
			g.writeErr(w, r, requestID, apiErrf(codeInvalidArgument,
				"a key ending in %q is a directory marker and must have an empty body", "/"))
			return
		}
		if existing, lerr := b.vs.NS.Lookup(g.cred, parent, name); lerr == nil {
			if existing.Kind != record.KindDir {
				g.writeErr(w, r, requestID, apiErrf(codeInvalidRequest,
					"the key %q names an object, not a directory", key))
				return
			}
		} else if _, merr := b.vs.NS.Mkdir(g.cred, parent, name, 0o755, g.cfg.UID, g.cfg.GID); merr != nil && !errors.Is(merr, orfserr.ErrExists) {
			g.writeErr(w, r, requestID, merr)
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		return
	}

	existing, lerr := b.vs.NS.Lookup(g.cred, parent, name)
	hasExisting := lerr == nil
	if hasExisting {
		if existing.Kind == record.KindDir {
			g.writeErr(w, r, requestID, apiErrf(codeInvalidRequest,
				"the key %q names a directory", key))
			return
		}
		if locked, reason := g.objectMetaOf(existing).locked(now); locked {
			g.refuseLocked(w, r, requestID, key, reason)
			return
		}
	}

	meta := objectMeta{
		ETag:        etag,
		Size:        uint64(len(body)),
		ContentType: r.Header.Get("Content-Type"),
		RetainUntil: retainUntil,
		RetainMode:  r.Header.Get("x-amz-object-lock-mode"),
		LegalHold:   hold,
		Created:     now.UTC().Format(time.RFC3339Nano),
	}
	metaBytes := encodeObjectMeta(meta)

	var inode *index.Inode
	if hasExisting {
		// Overwriting an unlocked object: the whole extent is replaced, and the
		// replacement bytes, the truncate, and the refreshed WORM metadata commit
		// in one authoritative transaction (one durability barrier, not three).
		inode, err = b.vs.NS.ReplaceFileData(g.cred, existing.ID, body, metaXattr, metaBytes)
	} else {
		// A new object: the inode, its directory entry, its bytes, and the WORM
		// metadata commit in one authoritative transaction.
		inode, err = b.vs.NS.CreateFileData(g.cred, parent, name, 0o644, g.cfg.UID, g.cfg.GID, body, metaXattr, metaBytes)
	}
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}

	g.publishManifest(b, inode.ID, body)

	w.Header().Set("ETag", etag)
	w.Header().Set("x-amz-object-lock-legal-hold", legalHoldHeader(hold))
	if retainUntil != "" {
		w.Header().Set("x-amz-object-lock-retain-until-date", retainUntil)
	}
	w.WriteHeader(http.StatusOK)
}

// publishManifest publishes a manifested body for the object just written, so a
// subsequent read is cache-admitted. It is best-effort: the manifested plane is
// derived, so a failure is not an error (Law II).
func (g *Gateway) publishManifest(b *bucket, inodeID uint64, content []byte) {
	if !g.cfg.PublishManifests || b.vs.Cache == nil {
		return
	}
	in, err := b.vs.NS.Getattr(inodeID)
	if err != nil {
		return
	}
	_, _ = b.vs.Cache.Publish(inodeID, in.Version, b.vs.Vol.Log.CommittedRoot(), content)
}

// getObject serves GET and HEAD through the manifested read path.
func (g *Gateway) getObject(w http.ResponseWriter, r *http.Request, requestID string, b *bucket, key string, head bool) {
	if head {
		g.heads.Add(1)
	} else {
		g.gets.Add(1)
	}
	parent, name, err := g.resolveKey(b, key, false)
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	in, lerr := b.vs.NS.Lookup(g.cred, parent, name)
	if lerr != nil {
		g.writeErr(w, r, requestID, apiErrf(codeNoSuchKey, "the key %q does not exist", key))
		return
	}

	if in.Kind == record.KindDir {
		// Directory marker: S3 stores a zero-byte object with a trailing slash.
		w.Header().Set("ETag", etagOf(nil))
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Last-Modified", timespecToTime(in.Mtime).Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		return
	}

	// The read goes through namespace.Read, which consults the manifested-body
	// cache first and falls back to the authoritative extent read. A cache miss
	// is a fallback, never a different answer.
	content, err := b.vs.NS.Read(g.cred, in.ID, 0, in.Size)
	if err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}

	meta := g.objectMetaOf(in)
	etag := meta.ETag
	if etag == "" {
		etag = etagOf(content)
	}

	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", timespecToTime(in.Mtime).Format(http.TimeFormat))
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	w.Header().Set("x-amz-object-lock-legal-hold", legalHoldHeader(meta.LegalHold))
	if meta.RetainUntil != "" {
		w.Header().Set("x-amz-object-lock-retain-until-date", meta.RetainUntil)
	}
	if meta.RetainMode != "" {
		w.Header().Set("x-amz-object-lock-mode", meta.RetainMode)
	}

	start, end, ok, satisfiable := parseRange(r.Header.Get("Range"), uint64(len(content)))
	if ok && !satisfiable {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatUint(uint64(len(content)), 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if ok {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatUint(start, 10)+"-"+strconv.FormatUint(end, 10)+"/"+strconv.FormatUint(uint64(len(content)), 10))
		w.Header().Set("Content-Length", strconv.FormatUint(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if !head {
			w.Write(content[start : end+1])
		}
		return
	}

	w.Header().Set("Content-Length", strconv.FormatUint(uint64(len(content)), 10))
	w.WriteHeader(http.StatusOK)
	if !head {
		w.Write(content)
	}
}

// deleteObject implements DELETE, which is the WORM refusal surface.
func (g *Gateway) deleteObject(w http.ResponseWriter, r *http.Request, requestID string, b *bucket, key string) {
	g.deletes.Add(1)
	parent, name, err := g.resolveKey(b, key, false)
	if err != nil {
		// S3 DELETE is idempotent: a key that does not exist is a 204.
		var ae *apiError
		if errors.As(err, &ae) && ae.code == codeNoSuchKey {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		g.writeErr(w, r, requestID, err)
		return
	}
	in, lerr := b.vs.NS.Lookup(g.cred, parent, name)
	if errors.Is(lerr, orfserr.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if lerr != nil {
		g.writeErr(w, r, requestID, lerr)
		return
	}
	if in.Kind == record.KindDir {
		// A directory is not an object; it is not withdrawn either.
		g.writeErr(w, r, requestID, apiErrf(codeAccessDenied,
			"the key %q names a directory; this gateway is WORM and does not withdraw directories", key))
		return
	}
	if locked, reason := g.objectMetaOf(in).locked(g.now()); locked {
		g.refuseLocked(w, r, requestID, key, reason)
		return
	}
	// Unlocked: withdraw the name. The bytes stay in the log (Law I); only the
	// namespace entry is removed.
	if err := b.vs.NS.Unlink(g.cred, parent, name); err != nil {
		g.writeErr(w, r, requestID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refuseLocked answers a WORM refusal. It is not an error to hide: it is the
// guarantee.
func (g *Gateway) refuseLocked(w http.ResponseWriter, r *http.Request, requestID, key, reason string) {
	if reason == "legal-hold" {
		g.refusedLegalHold.Add(1)
	} else {
		g.refusedRetention.Add(1)
	}
	w.Header().Set("x-amz-worm-refusal", reason)
	g.writeErr(w, r, requestID, apiErrf(codeAccessDenied,
		"the object %q is WORM-locked (%s) and cannot be withdrawn or overwritten", key, reason))
}

// objectMetaOf reads an object's S3 metadata from its inode.
func (g *Gateway) objectMetaOf(in *index.Inode) objectMeta {
	if in == nil || in.Xattrs == nil {
		return objectMeta{}
	}
	raw, ok := in.Xattrs[metaXattr]
	if !ok {
		return objectMeta{}
	}
	m, ok := decodeObjectMeta(raw)
	if !ok {
		return objectMeta{}
	}
	return m
}

// etagOf is the object digest: a quoted hex MD5, the S3 single-part ETag.
func etagOf(content []byte) string {
	sum := md5.Sum(content)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func legalHoldHeader(on bool) string {
	if on {
		return legalHoldOn
	}
	return legalHoldOff
}

// parseRange parses a single byte range. ok=false means "no usable Range
// header"; satisfiable=false means the range lies beyond the object.
func parseRange(header string, size uint64) (start, end uint64, ok, satisfiable bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false, false
	}
	spec := strings.TrimPrefix(header, prefix)
	if strings.Contains(spec, ",") {
		// Multi-range responses are not implemented; serve the whole object.
		return 0, 0, false, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, false
	}
	if a == "" {
		// Suffix range: the last n bytes.
		n, err := strconv.ParseUint(strings.TrimSpace(b), 10, 64)
		if err != nil || n == 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		if size == 0 {
			return 0, 0, true, false
		}
		return size - n, size - 1, true, true
	}
	s, err := strconv.ParseUint(strings.TrimSpace(a), 10, 64)
	if err != nil {
		return 0, 0, false, false
	}
	if s >= size {
		return 0, 0, true, false
	}
	e := size - 1
	if b != "" {
		parsed, err := strconv.ParseUint(strings.TrimSpace(b), 10, 64)
		if err != nil {
			return 0, 0, false, false
		}
		if parsed < s {
			return 0, 0, false, false
		}
		if parsed < e {
			e = parsed
		}
	}
	return s, e, true, true
}
