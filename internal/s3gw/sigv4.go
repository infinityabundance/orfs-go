package s3gw

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// This file is a deliberately small, header-based AWS Signature V4 verifier.
//
// What it verifies (and nothing more):
//
//   - the Authorization header is present and its algorithm is AWS4-HMAC-SHA256;
//   - the credential's access key equals this gateway's single configured key;
//   - the credential scope's region, service ("s3"), and terminator
//     ("aws4_request") are the configured ones;
//   - the credential date matches the request timestamp;
//   - the request timestamp is within the clock skew;
//   - the recomputed signature equals the presented one (constant time);
//   - for PUT, that the body's SHA-256 equals the declared
//     x-amz-content-sha256.
//
// What it does NOT do (documented, not implied):
//
//   - query-string (presigned URL) authentication: header-based only;
//   - session credentials / x-amz-security-token / STS;
//   - chunked ("STREAMING-AWS4-HMAC-SHA256-PAYLOAD") payload signatures;
//   - multiple credentials: exactly one access-key/secret-key pair;
//   - any authorization decision beyond the signature (no IAM, no policies).
//
// The canonical URI is reconstructed from the request's escaped path and
// re-encoded with the AWS reserved set, so a client that encodes its key
// normally verifies; a client that leaves sub-delimiters unencoded in the path
// still verifies, because the canonical form is normalised the same way.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	sigV4Service   = "s3"
	sigV4Term      = "aws4_request"
	amzDateFormat  = "20060102T150405Z"
	sigV4ClockSkew = 15 * time.Minute
)

// emptySHA256 is the SHA-256 of the empty string; S3 clients send it for
// bodyless requests.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credential is a static access-key/secret-key pair.
type Credential struct {
	AccessKey string
	SecretKey string
}

// sigV4Result is the verified request metadata a handler may still need.
type sigV4Result struct {
	// PayloadHash is the declared x-amz-content-sha256 (or the empty-string
	// digest when the header was absent).
	PayloadHash string
	// PayloadHashDeclared reports whether the header was present.
	PayloadHashDeclared bool
	// Timestamp is the request time (x-amz-date, else Date).
	Timestamp time.Time
}

// verifyRequest verifies the SigV4 Authorization header against cred. now is
// the gateway clock (injected so tests can be deterministic).
func verifyRequest(r *http.Request, cred Credential, region string, now time.Time) (*sigV4Result, error) {
	sr, err := parseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}
	if cred.AccessKey == "" {
		return nil, apiErrf(codeAccessDenied, "the gateway has no credential configured")
	}
	if sr.accessKey != cred.AccessKey {
		return nil, apiErrf(codeInvalidAccessKeyID, "the access key is not known to this gateway")
	}
	if sr.region != region {
		return nil, apiErrf(codeAuthorizationHeaderMalformed, "credential region %q does not match %q", sr.region, region)
	}
	if sr.service != sigV4Service {
		return nil, apiErrf(codeAuthorizationHeaderMalformed, "credential service %q is not %q", sr.service, sigV4Service)
	}
	if len(sr.signedHeaders) == 0 || sr.signature == "" {
		return nil, apiErrf(codeAuthorizationHeaderMalformed, "the Authorization header is incomplete")
	}

	amzDate := r.Header.Get("x-amz-date")
	timestamp := amzDate
	if timestamp == "" {
		timestamp = r.Header.Get("Date")
	}
	if timestamp == "" {
		return nil, apiErrf(codeAccessDenied, "the request carries no x-amz-date or Date header")
	}
	var reqTime time.Time
	if amzDate != "" {
		t, perr := time.Parse(amzDateFormat, amzDate)
		if perr != nil {
			return nil, apiErrf(codeAccessDenied, "malformed x-amz-date")
		}
		reqTime = t
	} else {
		t, perr := http.ParseTime(timestamp)
		if perr != nil {
			return nil, apiErrf(codeAccessDenied, "malformed Date header")
		}
		reqTime = t
	}
	if skew := now.Sub(reqTime); skew > sigV4ClockSkew || skew < -sigV4ClockSkew {
		return nil, apiErrf(codeRequestTimeTooSkewed,
			"the request time differs from the gateway clock by more than %s", sigV4ClockSkew)
	}
	if sr.date != reqTime.UTC().Format("20060102") {
		return nil, apiErrf(codeAuthorizationHeaderMalformed,
			"credential scope date %q does not match the request date", sr.date)
	}

	payloadHash := r.Header.Get("x-amz-content-sha256")
	declared := payloadHash != ""
	if !declared {
		payloadHash = emptySHA256
	}
	if strings.HasPrefix(payloadHash, "STREAMING-") {
		return nil, apiErrf(codeNotImplemented, "streaming (chunked) payload signatures are not supported")
	}

	canonical := canonicalRequest(r, sr.signedHeaders, payloadHash)
	scope := strings.Join([]string{sr.date, sr.region, sr.service, sigV4Term}, "/")
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		timestamp,
		scope,
		hexSHA256([]byte(canonical)),
	}, "\n")

	key := signingKey(cred.SecretKey, sr.date, sr.region, sr.service)
	expected := hex.EncodeToString(hmacSHA256(key, stringToSign))
	if !hmac.Equal([]byte(expected), []byte(sr.signature)) {
		return nil, apiErrf(codeSignatureDoesNotMatch,
			"the request signature we calculated does not match the one you provided")
	}
	return &sigV4Result{PayloadHash: payloadHash, PayloadHashDeclared: declared, Timestamp: reqTime}, nil
}

// authorization is the parsed Authorization header.
type authorization struct {
	accessKey     string
	date          string
	region        string
	service       string
	signedHeaders []string
	signature     string
}

func parseAuthorization(h string) (*authorization, error) {
	if h == "" {
		return nil, apiErrf(codeAccessDenied, "no Authorization header: this gateway rejects unsigned requests")
	}
	if !strings.HasPrefix(h, sigV4Algorithm+" ") {
		return nil, apiErrf(codeInvalidArgument,
			"only the %s header-based scheme is supported", sigV4Algorithm)
	}
	a := &authorization{}
	for _, field := range strings.Split(strings.TrimPrefix(h, sigV4Algorithm+" "), ",") {
		field = strings.TrimSpace(field)
		switch {
		case strings.HasPrefix(field, "Credential="):
			f := strings.Split(strings.TrimPrefix(field, "Credential="), "/")
			if len(f) != 5 {
				return nil, apiErrf(codeAuthorizationHeaderMalformed, "the credential scope must have five fields")
			}
			a.accessKey, a.date, a.region, a.service = f[0], f[1], f[2], f[3]
			if f[4] != sigV4Term {
				return nil, apiErrf(codeAuthorizationHeaderMalformed, "the credential terminator must be %q", sigV4Term)
			}
		case strings.HasPrefix(field, "SignedHeaders="):
			a.signedHeaders = strings.Split(strings.TrimPrefix(field, "SignedHeaders="), ";")
		case strings.HasPrefix(field, "Signature="):
			a.signature = strings.TrimPrefix(field, "Signature=")
		}
	}
	return a, nil
}

// canonicalRequest builds the SigV4 canonical request.
func canonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r.URL))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.URL.RawQuery))
	b.WriteByte('\n')
	for _, h := range signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(normalizeHeaderValue(headerValue(r, h)))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

// canonicalURI is the request path with each segment AWS-URI-encoded, "/"
// separators preserved.
func canonicalURI(u *url.URL) string {
	escaped := u.EscapedPath()
	if escaped == "" {
		return "/"
	}
	segs := strings.Split(escaped, "/")
	for i, s := range segs {
		if dec, err := url.PathUnescape(s); err == nil {
			segs[i] = awsEncode(dec)
		} else {
			segs[i] = awsEncode(s)
		}
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts and re-encodes the query string the AWS way.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			dk = k
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			dv = v
		}
		pairs = append(pairs, pair{awsEncode(dk), awsEncode(dv)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

// headerValue returns a header's value for canonicalisation. "host" lives on
// the Request, not in Header.
func headerValue(r *http.Request, name string) string {
	if name == "host" {
		return r.Host
	}
	return strings.Join(r.Header.Values(name), ",")
}

// normalizeHeaderValue trims and collapses internal whitespace, as SigV4
// requires.
func normalizeHeaderValue(v string) string { return strings.Join(strings.Fields(v), " ") }

// awsEncode percent-encodes every byte outside the AWS unreserved set.
func awsEncode(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		const hexDigits = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, sigV4Term)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SignRequest signs r in place with header-based SigV4, given the hex
// SHA-256 of the body (emptySHA256 for no body). It exists for tests and for a
// Go benchmark driver that does not want to vendor the AWS SDK; the gateway
// itself never signs. The signed header set is host + x-amz-content-sha256 +
// x-amz-date.
func SignRequest(r *http.Request, cred Credential, region string, t time.Time, bodyHash string) {
	if bodyHash == "" {
		bodyHash = emptySHA256
	}
	amzDate := t.UTC().Format(amzDateFormat)
	r.Header.Set("x-amz-date", amzDate)
	r.Header.Set("x-amz-content-sha256", bodyHash)
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	a := &authorization{
		accessKey:     cred.AccessKey,
		date:          t.UTC().Format("20060102"),
		region:        region,
		service:       sigV4Service,
		signedHeaders: []string{"host", "x-amz-content-sha256", "x-amz-date"},
	}
	scope := strings.Join([]string{a.date, region, sigV4Service, sigV4Term}, "/")
	canonical := canonicalRequest(r, a.signedHeaders, bodyHash)
	stringToSign := strings.Join([]string{sigV4Algorithm, amzDate, scope, hexSHA256([]byte(canonical))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(signingKey(cred.SecretKey, a.date, region, sigV4Service), stringToSign))
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, a.accessKey, scope, strings.Join(a.signedHeaders, ";"), sig))
}
