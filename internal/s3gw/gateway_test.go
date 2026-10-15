package s3gw

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infinityabundance/orfs-go/internal/record"
)

// These tests drive the gateway with a real httptest server over a real
// temporary OrFS volume. No FUSE, no mock: the objects really are appended to
// the log and read back through the manifested read path.

const testSegmentSize = 256 * 1024

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	vs   *VolumeSet
	cred Credential
	cfg  Config
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	vs, err := InitVolumeSet(t.TempDir(), testSegmentSize, VolumeOptions{})
	if err != nil {
		t.Fatalf("init volume: %v", err)
	}
	cred := Credential{AccessKey: "ORFSTESTACCESS", SecretKey: "orfs-test-secret-key"}
	cfg := Config{
		Default:          vs,
		Credential:       cred,
		Region:           "us-east-1",
		DefaultRetention: Retention{Indefinite: true}, // WORM by default, as the CLI ships
		PublishManifests: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := httptest.NewServer(New(cfg))
	t.Cleanup(func() {
		srv.Close()
		vs.Close()
	})
	return &harness{t: t, srv: srv, vs: vs, cred: cred, cfg: cfg}
}

// do signs and sends a request.
func (h *harness) do(method, path string, body []byte, headers map[string]string, sign bool) *http.Response {
	h.t.Helper()
	return h.doWithCred(method, path, body, headers, h.cred, h.cfg.Region, time.Now(), sign, nil)
}

func (h *harness) doWithCred(method, path string, body []byte, headers map[string]string,
	cred Credential, region string, at time.Time, sign bool, tamper func(*http.Request)) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if sign {
		bodyHash := emptySHA256
		if body != nil {
			bodyHash = hexSHA256(body)
		}
		SignRequest(req, cred, region, at, bodyHash)
	}
	if tamper != nil {
		tamper(req)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func mustStatus(t *testing.T, resp *http.Response, want int) []byte {
	t.Helper()
	body := readAll(t, resp)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d; body=%s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, body)
	}
	return body
}

func TestPutGetHeadRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	content := []byte("the quick brown fox jumps over the lazy dog\n")

	resp := h.do(http.MethodPut, "/bucket/hello.txt", content, map[string]string{"Content-Type": "text/plain"}, true)
	body := mustStatus(t, resp, http.StatusOK)
	if len(body) != 0 {
		t.Fatalf("PUT returned a body: %q", body)
	}
	etag := resp.Header.Get("ETag")
	if etag != etagOf(content) {
		t.Fatalf("ETag = %s, want %s", etag, etagOf(content))
	}

	resp = h.do(http.MethodGet, "/bucket/hello.txt", nil, nil, true)
	got := mustStatus(t, resp, http.StatusOK)
	if !bytes.Equal(got, content) {
		t.Fatalf("GET body = %q, want %q", got, content)
	}
	if resp.Header.Get("ETag") != etag {
		t.Fatalf("GET ETag = %s, want %s", resp.Header.Get("ETag"), etag)
	}
	if resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("GET Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Length") != "44" {
		t.Fatalf("GET Content-Length = %q", resp.Header.Get("Content-Length"))
	}

	resp = h.do(http.MethodHead, "/bucket/hello.txt", nil, nil, true)
	mustStatus(t, resp, http.StatusOK)
	if resp.Header.Get("Content-Length") != "44" {
		t.Fatalf("HEAD Content-Length = %q", resp.Header.Get("Content-Length"))
	}
	if resp.Header.Get("ETag") != etag {
		t.Fatalf("HEAD ETag = %s, want %s", resp.Header.Get("ETag"), etag)
	}
}

func TestNestedKeysAndEncodedPaths(t *testing.T) {
	h := newHarness(t, nil)
	content := []byte("nested")
	// A key with a space exercises percent-encoding through SigV4 and routing.
	if resp := h.do(http.MethodPut, "/bucket/logs/2026/audit%20one.log", content, nil, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT nested: status %d", resp.StatusCode)
	}
	got := mustStatus(t, h.do(http.MethodGet, "/bucket/logs/2026/audit%20one.log", nil, nil, true), http.StatusOK)
	if !bytes.Equal(got, content) {
		t.Fatalf("GET nested = %q", got)
	}
	// The key's path components really are OrFS directories.
	v := h.vs.Vol.View()
	bucketDir, ok := v.Lookup(record.RootInode, "bucket")
	if !ok || bucketDir.Kind != record.KindDir {
		t.Fatal("the bucket is not a directory in the OrFS namespace")
	}
	logsDir, ok := v.Lookup(bucketDir.ID, "logs")
	if !ok || logsDir.Kind != record.KindDir {
		t.Fatal("the intermediate directory logs/ was not created")
	}
	if _, ok := v.Lookup(logsDir.ID, "2026"); !ok {
		t.Fatal("the intermediate directory logs/2026/ was not created")
	}
}

func TestGetServedFromManifestedPath(t *testing.T) {
	h := newHarness(t, nil)
	content := bytes.Repeat([]byte("cache-admitted bytes. "), 64)
	if resp := h.do(http.MethodPut, "/bucket/m.log", content, nil, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}
	// The just-written object was manifested, so the read is cache-admitted.
	mustStatus(t, h.do(http.MethodGet, "/bucket/m.log", nil, nil, true), http.StatusOK)
	manifested, _, refused := h.vs.Cache.Stats()
	if manifested == 0 {
		t.Fatal("the read was not served from the manifested path")
	}
	if refused != 0 {
		t.Fatalf("manifested body was refused %d times", refused)
	}
}

func TestWormDeleteRetentionForbidden(t *testing.T) {
	h := newHarness(t, nil)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	resp := h.do(http.MethodPut, "/bucket/locked.bin", []byte("immutable"),
		map[string]string{
			"x-amz-object-lock-retain-until-date": future,
			"x-amz-object-lock-mode":              "COMPLIANCE",
		}, true)
	mustStatus(t, resp, http.StatusOK)
	if resp.Header.Get("x-amz-object-lock-retain-until-date") == "" {
		t.Fatal("PUT did not echo the retain-until date")
	}

	// DELETE is refused: that refusal is the WORM guarantee.
	del := h.do(http.MethodDelete, "/bucket/locked.bin", nil, nil, true)
	body := mustStatus(t, del, http.StatusForbidden)
	if !strings.Contains(string(body), codeAccessDenied) {
		t.Fatalf("DELETE body = %s, want an AccessDenied document", body)
	}
	if del.Header.Get("x-amz-worm-refusal") != "retention" {
		t.Fatalf("refusal reason = %q", del.Header.Get("x-amz-worm-refusal"))
	}
	// The object is still there.
	mustStatus(t, h.do(http.MethodGet, "/bucket/locked.bin", nil, nil, true), http.StatusOK)

	// Overwriting a locked object is refused too.
	over := h.do(http.MethodPut, "/bucket/locked.bin", []byte("new bytes"), nil, true)
	mustStatus(t, over, http.StatusForbidden)
}

func TestWormLegalHoldForbidden(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(http.MethodPut, "/bucket/held.bin", []byte("held"),
		map[string]string{"x-amz-object-lock-legal-hold": "ON"}, true)
	mustStatus(t, resp, http.StatusOK)

	del := h.do(http.MethodDelete, "/bucket/held.bin", nil, nil, true)
	mustStatus(t, del, http.StatusForbidden)
	if del.Header.Get("x-amz-worm-refusal") != "legal-hold" {
		t.Fatalf("refusal reason = %q, want legal-hold", del.Header.Get("x-amz-worm-refusal"))
	}
}

func TestPastRetainUntilRefused(t *testing.T) {
	h := newHarness(t, nil)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	resp := h.do(http.MethodPut, "/bucket/past.bin", []byte("x"),
		map[string]string{"x-amz-object-lock-retain-until-date": past}, true)
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestDeleteUnretainedObject(t *testing.T) {
	// With no default retention an object is retention-free: the gateway says so
	// explicitly and allows the name to be withdrawn. The bytes stay in the log.
	h := newHarness(t, func(c *Config) { c.DefaultRetention = Retention{} })
	mustStatus(t, h.do(http.MethodPut, "/bucket/tmp.txt", []byte("scratch"), nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodDelete, "/bucket/tmp.txt", nil, nil, true), http.StatusNoContent)
	mustStatus(t, h.do(http.MethodGet, "/bucket/tmp.txt", nil, nil, true), http.StatusNotFound)
	// DELETE is idempotent.
	mustStatus(t, h.do(http.MethodDelete, "/bucket/tmp.txt", nil, nil, true), http.StatusNoContent)
}

func TestOverwriteUnlockedObject(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DefaultRetention = Retention{} })
	mustStatus(t, h.do(http.MethodPut, "/bucket/o.txt", []byte("first-and-longer"), nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/bucket/o.txt", []byte("second"), nil, true), http.StatusOK)
	got := mustStatus(t, h.do(http.MethodGet, "/bucket/o.txt", nil, nil, true), http.StatusOK)
	if string(got) != "second" {
		t.Fatalf("GET after overwrite = %q, want %q", got, "second")
	}
}

func TestRangeGet(t *testing.T) {
	h := newHarness(t, nil)
	content := []byte("0123456789abcdefghij")
	mustStatus(t, h.do(http.MethodPut, "/bucket/r.bin", content, nil, true), http.StatusOK)

	resp := h.do(http.MethodGet, "/bucket/r.bin", nil, map[string]string{"Range": "bytes=5-9"}, true)
	got := mustStatus(t, resp, http.StatusPartialContent)
	if string(got) != "56789" {
		t.Fatalf("range body = %q", got)
	}
	if resp.Header.Get("Content-Range") != "bytes 5-9/20" {
		t.Fatalf("Content-Range = %q", resp.Header.Get("Content-Range"))
	}

	resp = h.do(http.MethodGet, "/bucket/r.bin", nil, map[string]string{"Range": "bytes=-4"}, true)
	if got := mustStatus(t, resp, http.StatusPartialContent); string(got) != "ghij" {
		t.Fatalf("suffix range body = %q", got)
	}

	resp = h.do(http.MethodGet, "/bucket/r.bin", nil, map[string]string{"Range": "bytes=100-200"}, true)
	mustStatus(t, resp, http.StatusRequestedRangeNotSatisfiable)
}

type listResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	Name        string   `xml:"Name"`
	Prefix      string   `xml:"Prefix"`
	Delimiter   string   `xml:"Delimiter"`
	MaxKeys     int      `xml:"MaxKeys"`
	KeyCount    int      `xml:"KeyCount"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextToken   string   `xml:"NextContinuationToken"`
	Contents    []struct {
		Key  string `xml:"Key"`
		Size uint64 `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (r listResult) keys() []string {
	var out []string
	for _, c := range r.Contents {
		out = append(out, c.Key)
	}
	return out
}

func (r listResult) prefixes() []string {
	var out []string
	for _, c := range r.CommonPrefixes {
		out = append(out, c.Prefix)
	}
	return out
}

func (h *harness) list(t *testing.T, query string) listResult {
	t.Helper()
	resp := h.do(http.MethodGet, "/bucket?"+query, nil, nil, true)
	body := mustStatus(t, resp, http.StatusOK)
	var out listResult
	if err := xml.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal list: %v\n%s", err, body)
	}
	return out
}

func TestListObjectsPrefixDelimiterContinuation(t *testing.T) {
	h := newHarness(t, nil)
	keys := []string{"a/1.txt", "a/2.txt", "b/1.txt", "top.txt"}
	for _, k := range keys {
		mustStatus(t, h.do(http.MethodPut, "/bucket/"+k, []byte(k), nil, true), http.StatusOK)
	}

	// No delimiter: every key, sorted.
	all := h.list(t, "list-type=2")
	if want := []string{"a/1.txt", "a/2.txt", "b/1.txt", "top.txt"}; !equalStrings(all.keys(), want) {
		t.Fatalf("list all = %v, want %v", all.keys(), want)
	}
	if all.KeyCount != 4 {
		t.Fatalf("KeyCount = %d, want 4", all.KeyCount)
	}

	// Delimiter "/" folds the shared prefixes.
	folded := h.list(t, "list-type=2&delimiter=%2F")
	if want := []string{"top.txt"}; !equalStrings(folded.keys(), want) {
		t.Fatalf("folded keys = %v, want %v", folded.keys(), want)
	}
	if want := []string{"a/", "b/"}; !equalStrings(folded.prefixes(), want) {
		t.Fatalf("folded prefixes = %v, want %v", folded.prefixes(), want)
	}

	// Prefix selects a subtree.
	sub := h.list(t, "list-type=2&prefix=a%2F")
	if want := []string{"a/1.txt", "a/2.txt"}; !equalStrings(sub.keys(), want) {
		t.Fatalf("prefixed keys = %v, want %v", sub.keys(), want)
	}

	// Continuation: one key at a time.
	page1 := h.list(t, "list-type=2&max-keys=1")
	if !page1.IsTruncated || len(page1.keys()) != 1 || page1.NextToken == "" {
		t.Fatalf("page1 = %+v", page1)
	}
	page2 := h.list(t, "list-type=2&max-keys=1&continuation-token="+page1.NextToken)
	if page2.keys()[0] != "a/2.txt" {
		t.Fatalf("page2 first key = %v", page2.keys())
	}

	// LIST v1 with a marker.
	v1 := h.list(t, "marker=a%2F2.txt")
	if want := []string{"b/1.txt", "top.txt"}; !equalStrings(v1.keys(), want) {
		t.Fatalf("v1 list = %v, want %v", v1.keys(), want)
	}
}

func TestDirectoryMarker(t *testing.T) {
	h := newHarness(t, nil)
	mustStatus(t, h.do(http.MethodPut, "/bucket/dir/", []byte{}, nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/bucket/dir/obj.txt", []byte("x"), nil, true), http.StatusOK)
	// GET on the marker itself is an empty 200.
	resp := h.do(http.MethodGet, "/bucket/dir/", nil, nil, true)
	if got := mustStatus(t, resp, http.StatusOK); len(got) != 0 {
		t.Fatalf("directory marker GET = %q", got)
	}
	folded := h.list(t, "list-type=2&delimiter=%2F")
	if want := []string{"dir/"}; !equalStrings(folded.prefixes(), want) {
		t.Fatalf("delimiter prefixes = %v, want %v", folded.prefixes(), want)
	}
	// A non-empty body on a marker is refused.
	resp = h.do(http.MethodPut, "/bucket/other/", []byte("not empty"), nil, true)
	mustStatus(t, resp, http.StatusBadRequest)

	// A key cannot be a file and a directory at once.
	mustStatus(t, h.do(http.MethodPut, "/bucket/clash", []byte("a file"), nil, true), http.StatusOK)
	resp = h.do(http.MethodPut, "/bucket/clash/", []byte{}, nil, true)
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestSigV4AcceptAndReject(t *testing.T) {
	h := newHarness(t, nil)

	// Unsigned is refused.
	resp := h.do(http.MethodGet, "/bucket", nil, nil, false)
	body := mustStatus(t, resp, http.StatusForbidden)
	if !strings.Contains(string(body), codeAccessDenied) {
		t.Fatalf("unsigned body = %s", body)
	}

	// A wrong secret does not verify.
	wrong := Credential{AccessKey: h.cred.AccessKey, SecretKey: "not-the-secret"}
	resp = h.doWithCred(http.MethodGet, "/bucket", nil, nil, wrong, h.cfg.Region, time.Now(), true, nil)
	body = mustStatus(t, resp, http.StatusForbidden)
	if !strings.Contains(string(body), codeSignatureDoesNotMatch) {
		t.Fatalf("wrong-secret body = %s", body)
	}

	// A signature computed for another path does not verify for this one.
	resp = h.doWithCred(http.MethodGet, "/bucket", nil, nil, h.cred, h.cfg.Region, time.Now(), true,
		func(r *http.Request) { r.URL.Path = "/other" })
	mustStatus(t, resp, http.StatusForbidden)

	// A stale timestamp is refused.
	resp = h.doWithCred(http.MethodGet, "/bucket", nil, nil, h.cred, h.cfg.Region, time.Now().Add(-2*time.Hour), true, nil)
	body = mustStatus(t, resp, http.StatusForbidden)
	if !strings.Contains(string(body), codeRequestTimeTooSkewed) {
		t.Fatalf("skew body = %s", body)
	}

	// A signature for another region is refused (S3 answers 400
	// AuthorizationHeaderMalformed for a scope that does not match).
	resp = h.doWithCred(http.MethodGet, "/bucket", nil, nil, h.cred, "eu-west-1", time.Now(), true, nil)
	body = mustStatus(t, resp, http.StatusBadRequest)
	if !strings.Contains(string(body), codeAuthorizationHeaderMalformed) {
		t.Fatalf("wrong-region body = %s", body)
	}

	// A body that does not match the declared payload hash is refused.
	good := []byte("payload")
	other := []byte("tampered")
	req, err := http.NewRequest(http.MethodPut, h.srv.URL+"/bucket/t.bin", bytes.NewReader(other))
	if err != nil {
		t.Fatal(err)
	}
	SignRequest(req, h.cred, h.cfg.Region, time.Now(), hexSHA256(good))
	resp2, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body = mustStatus(t, resp2, http.StatusBadRequest)
	if !strings.Contains(string(body), codeContentSHA256Mismatch) {
		t.Fatalf("payload-mismatch body = %s", body)
	}
}

func TestBucketOps(t *testing.T) {
	h := newHarness(t, nil)

	mustStatus(t, h.do(http.MethodPut, "/newbucket", nil, nil, true), http.StatusOK)
	// Create is idempotent.
	mustStatus(t, h.do(http.MethodPut, "/newbucket", nil, nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodHead, "/newbucket", nil, nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodHead, "/missing", nil, nil, true), http.StatusNotFound)

	loc := mustStatus(t, h.do(http.MethodGet, "/newbucket?location", nil, nil, true), http.StatusOK)
	if !strings.Contains(string(loc), "LocationConstraint") {
		t.Fatalf("location = %s", loc)
	}

	lock := mustStatus(t, h.do(http.MethodGet, "/newbucket?object-lock", nil, nil, true), http.StatusOK)
	if !strings.Contains(string(lock), "Enabled") {
		t.Fatalf("object-lock = %s", lock)
	}

	buckets := mustStatus(t, h.do(http.MethodGet, "/", nil, nil, true), http.StatusOK)
	if !strings.Contains(string(buckets), "<Name>newbucket</Name>") {
		t.Fatalf("ListBuckets = %s", buckets)
	}

	// Deleting a bucket is refused (WORM).
	del := h.do(http.MethodDelete, "/newbucket", nil, nil, true)
	mustStatus(t, del, http.StatusForbidden)
}

func TestUnsupportedOperationsAreRefused(t *testing.T) {
	h := newHarness(t, nil)
	mustStatus(t, h.do(http.MethodPut, "/bucket", nil, nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodPost, "/bucket?delete", []byte("<Delete/>"), nil, true), http.StatusNotImplemented)
	mustStatus(t, h.do(http.MethodPost, "/bucket/k?uploads", nil, nil, true), http.StatusNotImplemented)
	mustStatus(t, h.do(http.MethodPut, "/bucket/k?partNumber=1&uploadId=x", []byte("y"), nil, true), http.StatusNotImplemented)
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	mustStatus(t, h.do(http.MethodPut, "/bucket/m.bin", []byte("metrics"), nil, true), http.StatusOK)
	mustStatus(t, h.do(http.MethodGet, "/bucket/m.bin", nil, nil, true), http.StatusOK)

	// /metrics is unauthenticated by default, for a Prometheus scrape.
	resp, err := h.srv.Client().Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	text := string(mustStatus(t, resp, http.StatusOK))
	for _, want := range []string{
		"orfs_physical_bytes",
		"orfs_authoritative_bytes",
		"orfs_logical_bytes",
		"orfs_amplification_ratio",
		"orfs_breach_total",
		"orfs_quarantine_total",
		"orfs_fsync_latency_seconds",
		"orfs_group_commit_batch_size",
		"orfs_s3_requests_total",
		"orfs_s3_worm_refusals_total",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics output is missing %s\n%s", want, text)
		}
	}
	if !strings.Contains(text, `op="PutObject"`) {
		t.Fatalf("metrics output is missing the PutObject counter\n%s", text)
	}
}

func TestMetricsRequireAuth(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MetricsRequireAuth = true })
	resp, err := h.srv.Client().Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusForbidden)
	mustStatus(t, h.do(http.MethodGet, "/metrics", nil, nil, true), http.StatusOK)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
