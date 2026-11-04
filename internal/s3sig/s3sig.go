// Package s3sig implements AWS Signature Version 4 for the S3 service: signing and presigning
// requests (used by the ogm CLI) and verifying them, including aws-chunked streaming uploads
// (used by the hub's S3-compatible API).
package s3sig

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Algorithm  = "AWS4-HMAC-SHA256"
	TimeFormat = "20060102T150405Z"
	DateFormat = "20060102"

	UnsignedPayload          = "UNSIGNED-PAYLOAD"
	StreamingPayload         = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingPayloadTrailer  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

	EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// MaxPresignExpiry is the longest presigned URL lifetime S3 accepts.
	MaxPresignExpiry = 7 * 24 * time.Hour
)

// Credentials is an S3 access key pair.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// Error is an S3 error with its wire code (e.g. SignatureDoesNotMatch).
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, f string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(f, a...), Status: status}
}

// ErrNoAuth means the request carries no SigV4 credentials at all.
var ErrNoAuth = errf(http.StatusForbidden, "AccessDenied", "anonymous access is not allowed; sign requests with SigV4")

// ---------- encoding ----------

func unreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~'
}

func escape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if unreserved(c) || (keepSlash && c == '/') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Escape URI-encodes s the way SigV4 requires (everything but A-Za-z0-9-_.~).
func Escape(s string) string { return escape(s, false) }

// EscapePath is Escape but keeps '/' (S3 object keys in paths).
func EscapePath(s string) string { return escape(s, true) }

// CanonicalQuery sorts and encodes query parameters. X-Amz-Signature is dropped.
func CanonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	var all []kv
	for k, vs := range q {
		if k == "X-Amz-Signature" {
			continue
		}
		for _, v := range vs {
			all = append(all, kv{Escape(k), Escape(v)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].k != all[j].k {
			return all[i].k < all[j].k
		}
		return all[i].v < all[j].v
	})
	parts := make([]string, len(all))
	for i, p := range all {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func trimHeader(v string) string { return strings.Join(strings.Fields(v), " ") }

// CanonicalRequest builds the SigV4 canonical request. hdr returns the (already joined) value
// of a lowercase header name.
func CanonicalRequest(method, uri, query string, hdr func(string) string, signed []string, payloadHash string) string {
	var b strings.Builder
	b.WriteString(method + "\n" + uri + "\n" + query + "\n")
	for _, h := range signed {
		b.WriteString(h + ":" + trimHeader(hdr(h)) + "\n")
	}
	b.WriteString("\n" + strings.Join(signed, ";") + "\n" + payloadHash)
	return b.String()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// StringToSign builds the SigV4 string to sign.
func StringToSign(amzDate, scope, canonical string) string {
	return Algorithm + "\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
}

// SigningKey derives the SigV4 signing key.
func SigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// Scope returns "date/region/service/aws4_request".
func Scope(date, region, service string) string {
	return date + "/" + region + "/" + service + "/aws4_request"
}

// ---------- client side ----------

func clientHeader(req *http.Request) func(string) string {
	return func(name string) string {
		if name == "host" {
			if req.Host != "" {
				return req.Host
			}
			return req.URL.Host
		}
		if name == "content-length" {
			return strconv.FormatInt(req.ContentLength, 10)
		}
		return strings.Join(req.Header.Values(name), ",")
	}
}

func clientSignedHeaders(req *http.Request) []string {
	signed := []string{"host"}
	for k := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" {
			signed = append(signed, lk)
		}
	}
	sort.Strings(signed)
	return signed
}

func requestURI(req *http.Request) string {
	if req.URL.Path == "" {
		return "/"
	}
	return EscapePath(req.URL.Path)
}

// Sign adds SigV4 header authentication to req. payloadHash is the hex sha256 of the body or
// UnsignedPayload.
func Sign(req *http.Request, c Credentials, region, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate, date := now.Format(TimeFormat), now.Format(DateFormat)
	if payloadHash == "" {
		payloadHash = UnsignedPayload
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	// Keep the wire path identical to the signed path.
	req.URL.RawPath = requestURI(req)
	signed := clientSignedHeaders(req)
	scope := Scope(date, region, "s3")
	cr := CanonicalRequest(req.Method, requestURI(req), CanonicalQuery(req.URL.Query()), clientHeader(req), signed, payloadHash)
	sig := hex.EncodeToString(hmacSHA256(SigningKey(c.SecretKey, date, region, "s3"), StringToSign(amzDate, scope, cr)))
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, c.AccessKey, scope, strings.Join(signed, ";"), sig))
}

// Presign returns a presigned URL for req (only the host header is signed).
func Presign(req *http.Request, c Credentials, region string, expires time.Duration, now time.Time) string {
	now = now.UTC()
	amzDate, date := now.Format(TimeFormat), now.Format(DateFormat)
	scope := Scope(date, region, "s3")
	q := req.URL.Query()
	q.Set("X-Amz-Algorithm", Algorithm)
	q.Set("X-Amz-Credential", c.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	cr := CanonicalRequest(req.Method, requestURI(req), CanonicalQuery(q), clientHeader(req), []string{"host"}, UnsignedPayload)
	sig := hex.EncodeToString(hmacSHA256(SigningKey(c.SecretKey, date, region, "s3"), StringToSign(amzDate, scope, cr)))
	u := *req.URL
	u.RawPath = requestURI(req)
	u.RawQuery = CanonicalQuery(q) + "&X-Amz-Signature=" + sig
	return u.String()
}

// ---------- server side ----------

// Auth is a parsed SigV4 authorization (header or presigned query).
type Auth struct {
	AccessKey     string
	Date          string // yyyymmdd from the credential scope
	Region        string
	Service       string
	AmzDate       string // X-Amz-Date
	Time          time.Time
	SignedHeaders []string
	Signature     string
	PayloadHash   string // x-amz-content-sha256 (UNSIGNED-PAYLOAD for presigned)
	Presigned     bool
	Expires       time.Duration
	V2            bool // legacy SigV2 query presign (boto3's default for generate_presigned_url)
}

// Scope is the credential scope string.
func (a *Auth) Scope() string { return Scope(a.Date, a.Region, a.Service) }

// Parse extracts SigV4 auth from a request. It returns ErrNoAuth for anonymous requests.
func Parse(r *http.Request) (*Auth, error) {
	q := r.URL.Query()
	a := &Auth{}
	var cred, signed string
	switch {
	case q.Get("AWSAccessKeyId") != "" && q.Get("Signature") != "" && q.Get("Expires") != "":
		exp, err := strconv.ParseInt(q.Get("Expires"), 10, 64)
		if err != nil {
			return nil, errf(400, "AuthorizationQueryParametersError", "Expires must be a unix timestamp")
		}
		a.V2, a.Presigned, a.AccessKey, a.Signature = true, true, q.Get("AWSAccessKeyId"), q.Get("Signature")
		a.Time, a.PayloadHash = time.Unix(exp, 0).UTC(), UnsignedPayload
		return a, nil
	case q.Get("X-Amz-Algorithm") != "":
		if q.Get("X-Amz-Algorithm") != Algorithm {
			return nil, errf(400, "AuthorizationQueryParametersError", "X-Amz-Algorithm must be %s", Algorithm)
		}
		a.Presigned = true
		cred, signed, a.Signature, a.AmzDate = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
		if err != nil || secs < 0 {
			return nil, errf(400, "AuthorizationQueryParametersError", "X-Amz-Expires must be a non-negative integer")
		}
		a.Expires = time.Duration(secs) * time.Second
		if a.Expires > MaxPresignExpiry {
			return nil, errf(400, "AuthorizationQueryParametersError", "X-Amz-Expires must be less than a week (604800 seconds)")
		}
		a.PayloadHash = UnsignedPayload
		if v := r.Header.Get("X-Amz-Content-Sha256"); v != "" {
			a.PayloadHash = v
		}
	case r.Header.Get("Authorization") != "":
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, Algorithm+" ") {
			if strings.HasPrefix(h, "AWS ") {
				return nil, errf(400, "InvalidRequest", "SigV2 is not supported; use AWS4-HMAC-SHA256 (signature_version='s3v4')")
			}
			return nil, errf(400, "AuthorizationHeaderMalformed", "unsupported authorization type")
		}
		for _, part := range strings.Split(strings.TrimPrefix(h, Algorithm+" "), ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch k {
			case "Credential":
				cred = v
			case "SignedHeaders":
				signed = v
			case "Signature":
				a.Signature = v
			}
		}
		a.AmzDate = r.Header.Get("X-Amz-Date")
		if a.AmzDate == "" {
			if d, err := http.ParseTime(r.Header.Get("Date")); err == nil {
				a.AmzDate = d.UTC().Format(TimeFormat)
			}
		}
		a.PayloadHash = r.Header.Get("X-Amz-Content-Sha256")
	default:
		return nil, ErrNoAuth
	}
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" {
		return nil, errf(400, "AuthorizationHeaderMalformed", "credential must be ACCESS_KEY/DATE/REGION/s3/aws4_request")
	}
	a.AccessKey, a.Date, a.Region, a.Service = parts[0], parts[1], parts[2], parts[3]
	if a.Service != "s3" {
		return nil, errf(400, "AuthorizationHeaderMalformed", "credential scope service must be s3, not %q", a.Service)
	}
	if signed == "" || a.Signature == "" {
		return nil, errf(400, "AuthorizationHeaderMalformed", "missing SignedHeaders or Signature")
	}
	a.SignedHeaders = strings.Split(signed, ";")
	t, err := time.Parse(TimeFormat, a.AmzDate)
	if err != nil {
		return nil, errf(403, "AccessDenied", "missing or malformed X-Amz-Date")
	}
	a.Time = t
	if t.Format(DateFormat) != a.Date {
		return nil, errf(403, "SignatureDoesNotMatch", "credential scope date does not match X-Amz-Date")
	}
	return a, nil
}

func serverHeader(r *http.Request) func(string) string {
	return func(name string) string {
		switch name {
		case "host":
			return r.Host
		case "content-length":
			if v := r.Header.Get("Content-Length"); v != "" {
				return v
			}
			return strconv.FormatInt(r.ContentLength, 10)
		case "transfer-encoding":
			if v := r.Header.Get("Transfer-Encoding"); v != "" {
				return v
			}
			return strings.Join(r.TransferEncoding, ",")
		}
		vs := r.Header.Values(name)
		for i := range vs {
			vs[i] = trimHeader(vs[i])
		}
		return strings.Join(vs, ",")
	}
}

// Verify checks the signature against secret. For header auth without x-amz-content-sha256
// the body (up to 32 MiB) is buffered and hashed.
func (a *Auth) Verify(r *http.Request, secret string, now time.Time, skew time.Duration) error {
	if a.V2 {
		return a.verifyV2(r, secret, now)
	}
	if a.Presigned {
		if a.Time.After(now.Add(skew)) {
			return errf(403, "AccessDenied", "presigned URL is not valid yet")
		}
		if now.After(a.Time.Add(a.Expires)) {
			return errf(403, "AccessDenied", "request has expired")
		}
	} else if d := now.Sub(a.Time); d > skew || d < -skew {
		return errf(403, "RequestTimeTooSkewed", "the difference between the request time and the server's time is too large")
	}
	hasHost := false
	for _, h := range a.SignedHeaders {
		hasHost = hasHost || h == "host"
	}
	if !hasHost {
		return errf(403, "AccessDenied", "the host header must be signed")
	}
	if a.PayloadHash == "" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20+1))
		if err != nil {
			return errf(400, "IncompleteBody", "%v", err)
		}
		if len(body) > 32<<20 {
			return errf(400, "MissingSecurityHeader", "x-amz-content-sha256 is required for bodies over 32 MiB")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		a.PayloadHash = sha256Hex(body)
	}
	q := CanonicalQuery(r.URL.Query())
	key := SigningKey(secret, a.Date, a.Region, a.Service)
	for _, uri := range candidateURIs(r) {
		cr := CanonicalRequest(r.Method, uri, q, serverHeader(r), a.SignedHeaders, a.PayloadHash)
		sig := hex.EncodeToString(hmacSHA256(key, StringToSign(a.AmzDate, a.Scope(), cr)))
		if hmac.Equal([]byte(sig), []byte(strings.ToLower(a.Signature))) {
			return nil
		}
	}
	return errf(403, "SignatureDoesNotMatch", "the request signature we calculated does not match the signature you provided; check your key and signing method")
}

// candidateURIs: the path exactly as sent, then the canonical re-encoding of the decoded path.
func candidateURIs(r *http.Request) []string {
	var out []string
	add := func(u string) {
		if u == "" {
			u = "/"
		}
		for _, o := range out {
			if o == u {
				return
			}
		}
		out = append(out, u)
	}
	if r.RequestURI != "" && !strings.Contains(r.RequestURI, "://") {
		raw, _, _ := strings.Cut(r.RequestURI, "?")
		add(raw)
	}
	add(EscapePath(r.URL.Path))
	add(r.URL.EscapedPath())
	return out
}

// Body returns a reader for the request payload: it verifies a literal x-amz-content-sha256
// at EOF and decodes (and, when signed, verifies) aws-chunked streaming bodies.
func (a *Auth) Body(r *http.Request, secret string) io.Reader {
	switch a.PayloadHash {
	case UnsignedPayload:
		return r.Body
	case StreamingPayload, StreamingPayloadTrailer:
		return &chunkReader{br: bufio.NewReaderSize(r.Body, 64<<10), signed: true,
			key: SigningKey(secret, a.Date, a.Region, a.Service), prevSig: strings.ToLower(a.Signature), amzDate: a.AmzDate, scope: a.Scope()}
	case StreamingUnsignedTrailer:
		return &chunkReader{br: bufio.NewReaderSize(r.Body, 64<<10)}
	}
	if strings.HasPrefix(a.PayloadHash, "STREAMING-") {
		return errReader{errf(400, "InvalidArgument", "unsupported x-amz-content-sha256 %q", a.PayloadHash)}
	}
	return &hashReader{r: r.Body, h: sha256.New(), want: strings.ToLower(a.PayloadHash)}
}

// IsStreaming reports whether the payload is aws-chunked encoded.
func (a *Auth) IsStreaming() bool { return strings.HasPrefix(a.PayloadHash, "STREAMING-") }

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type hashReader struct {
	r    io.Reader
	h    hash.Hash
	want string
}

func (h *hashReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(h.h.Sum(nil)) != h.want {
		return n, errf(400, "XAmzContentSHA256Mismatch", "the provided x-amz-content-sha256 does not match what was computed")
	}
	return n, err
}

// chunkReader decodes aws-chunked: "hex-size[;chunk-signature=sig]\r\n<data>\r\n" ... "0...\r\n"
// [trailers]"\r\n".
type chunkReader struct {
	br      *bufio.Reader
	signed  bool
	key     []byte
	prevSig string
	amzDate string
	scope   string

	remaining int64
	inChunk   bool
	chunkSig  string
	h         hash.Hash
	done      bool
	err       error
}

const maxChunk = 1 << 30

func (c *chunkReader) fail(code, f string, a ...any) error {
	c.err = errf(400, code, f, a...)
	return c.err
}

func (c *chunkReader) line() (string, error) {
	s, err := c.br.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	if len(s) > 4096 {
		return "", errors.New("aws-chunked line too long")
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func (c *chunkReader) verifyChunk() error {
	if !c.signed {
		return nil
	}
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.amzDate + "\n" + c.scope + "\n" + c.prevSig + "\n" + EmptySHA256 + "\n" + hex.EncodeToString(c.h.Sum(nil))
	sig := hex.EncodeToString(hmacSHA256(c.key, sts))
	if !hmac.Equal([]byte(sig), []byte(c.chunkSig)) {
		return c.fail("SignatureDoesNotMatch", "aws-chunked chunk signature does not match")
	}
	c.prevSig = sig
	return nil
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.done {
		return 0, io.EOF
	}
	for c.remaining == 0 {
		if c.inChunk {
			if l, err := c.line(); err != nil || l != "" {
				return 0, c.fail("IncompleteBody", "malformed aws-chunked body (missing CRLF after chunk)")
			}
			if err := c.verifyChunk(); err != nil {
				return 0, err
			}
			c.inChunk = false
		}
		l, err := c.line()
		if err != nil {
			return 0, c.fail("IncompleteBody", "malformed aws-chunked body: %v", err)
		}
		sizeStr, ext, _ := strings.Cut(l, ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
		if err != nil || size < 0 || size > maxChunk {
			return 0, c.fail("IncompleteBody", "malformed aws-chunked size %q", sizeStr)
		}
		c.chunkSig = ""
		if k, v, ok := strings.Cut(ext, "="); ok && strings.TrimSpace(k) == "chunk-signature" {
			c.chunkSig = strings.ToLower(strings.TrimSpace(v))
		}
		if c.signed && c.chunkSig == "" {
			return 0, c.fail("SignatureDoesNotMatch", "aws-chunked chunk is missing its signature")
		}
		c.h = sha256.New()
		if size == 0 {
			if err := c.verifyChunk(); err != nil {
				return 0, err
			}
			// Trailing headers (x-amz-checksum-*, x-amz-trailer-signature) until a blank line.
			for {
				t, err := c.line()
				if err != nil || t == "" {
					break
				}
			}
			c.done = true
			return 0, io.EOF
		}
		c.remaining, c.inChunk = size, true
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.br.Read(p)
	c.h.Write(p[:n])
	c.remaining -= int64(n)
	if err == io.EOF {
		if c.remaining > 0 {
			return n, c.fail("IncompleteBody", "aws-chunked body ended mid-chunk")
		}
		err = nil
	}
	return n, err
}

// SigV2 subresources that are part of the canonical resource.
var v2Subresources = []string{"acl", "delete", "lifecycle", "location", "logging", "notification", "partNumber",
	"policy", "requestPayment", "response-cache-control", "response-content-disposition", "response-content-encoding",
	"response-content-language", "response-content-type", "response-expires", "tagging", "torrent", "uploadId",
	"uploads", "versionId", "versioning", "versions", "website"}

// verifyV2 checks a legacy SigV2 presigned URL (?AWSAccessKeyId&Expires&Signature). The
// canonical resource is tried with and without the endpoint's path prefix (e.g. /s3).
func (a *Auth) verifyV2(r *http.Request, secret string, now time.Time) error {
	if now.After(a.Time) {
		return errf(403, "AccessDenied", "request has expired")
	}
	q := r.URL.Query()
	var amz []string
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") {
			amz = append(amz, lk+":"+strings.Join(r.Header.Values(k), ","))
		}
	}
	sort.Strings(amz)
	var subs []string
	for _, k := range v2Subresources { // already sorted
		if vs, ok := q[k]; ok {
			if len(vs) > 0 && vs[0] != "" {
				subs = append(subs, k+"="+vs[0])
			} else {
				subs = append(subs, k)
			}
		}
	}
	sub := ""
	if len(subs) > 0 {
		sub = "?" + strings.Join(subs, "&")
	}
	head := r.Method + "\n" + r.Header.Get("Content-MD5") + "\n" + r.Header.Get("Content-Type") + "\n" + q.Get("Expires") + "\n"
	for _, h := range amz {
		head += h + "\n"
	}
	want, err := base64.StdEncoding.DecodeString(a.Signature)
	if err != nil {
		return errf(403, "SignatureDoesNotMatch", "malformed signature")
	}
	for _, uri := range candidateURIs(r) {
		resources := []string{uri}
		if i := strings.Index(uri[1:], "/"); i >= 0 {
			resources = append(resources, uri[1+i:]) // without the endpoint prefix
		}
		for _, res := range resources {
			m := hmac.New(sha1.New, []byte(secret))
			m.Write([]byte(head + res + sub))
			if hmac.Equal(m.Sum(nil), want) {
				return nil
			}
		}
	}
	return errf(403, "SignatureDoesNotMatch", "the request signature we calculated does not match the signature you provided; check your key and signing method")
}
