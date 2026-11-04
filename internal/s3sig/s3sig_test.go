package s3sig

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Test vectors from the AWS S3 SigV4 documentation (sig-v4-header-based-auth,
// sigv4-query-string-auth, sigv4-streaming).
const (
	docAK     = "AKIAIOSFODNN7EXAMPLE"
	docSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var docTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func TestDocGetObject(t *testing.T) {
	r := httptest.NewRequest("GET", "/test.txt", nil)
	r.Host = "examplebucket.s3.amazonaws.com"
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("X-Amz-Content-Sha256", EmptySHA256)
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	a, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessKey != docAK || a.Region != "us-east-1" {
		t.Fatalf("parsed %+v", a)
	}
	if err := a.Verify(r, docSecret, docTime, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, "wrong", docTime, 15*time.Minute); !isCode(err, "SignatureDoesNotMatch") {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := a.Verify(r, docSecret, docTime.Add(time.Hour), 15*time.Minute); !isCode(err, "RequestTimeTooSkewed") {
		t.Fatalf("skew: %v", err)
	}
}

func TestDocPresigned(t *testing.T) {
	r := httptest.NewRequest("GET", "/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", nil)
	r.Host = "examplebucket.s3.amazonaws.com"
	a, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, docSecret, docTime.Add(time.Hour), 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, docSecret, docTime.Add(25*time.Hour), 15*time.Minute); !isCode(err, "AccessDenied") {
		t.Fatalf("expired: %v", err)
	}
}

func TestDocStreaming(t *testing.T) {
	var body bytes.Buffer
	a64, a1 := strings.Repeat("a", 65536), strings.Repeat("a", 1024)
	fmt.Fprintf(&body, "10000;chunk-signature=ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648\r\n%s\r\n", a64)
	fmt.Fprintf(&body, "400;chunk-signature=0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497\r\n%s\r\n", a1)
	fmt.Fprintf(&body, "0;chunk-signature=b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9\r\n\r\n")
	raw := body.Bytes()
	mk := func(b []byte) *http.Request {
		r := httptest.NewRequest("PUT", "/examplebucket/chunkObject.txt", bytes.NewReader(b))
		r.Host = "s3.amazonaws.com"
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("Content-Length", "66824")
		r.Header.Set("X-Amz-Content-Sha256", StreamingPayload)
		r.Header.Set("X-Amz-Date", "20130524T000000Z")
		r.Header.Set("X-Amz-Decoded-Content-Length", "66560")
		r.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")
		return r
	}
	r := mk(raw)
	a, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, docSecret, docTime, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(a.Body(r, docSecret))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != a64+a1 {
		t.Fatalf("decoded %d bytes", len(got))
	}
	// Tampered chunk data fails chunk verification.
	bad := bytes.Replace(raw, []byte("aaaa\r\n400"), []byte("aaab\r\n400"), 1)
	r = mk(bad)
	a, _ = Parse(r)
	if _, err := io.ReadAll(a.Body(r, docSecret)); !isCode(err, "SignatureDoesNotMatch") {
		t.Fatalf("tampered chunk: %v", err)
	}
}

func TestUnsignedTrailer(t *testing.T) {
	body := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	a := &Auth{PayloadHash: StreamingUnsignedTrailer}
	r := httptest.NewRequest("PUT", "/b/k", strings.NewReader(body))
	got, err := io.ReadAll(a.Body(r, ""))
	if err != nil || string(got) != "hello world" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestSignRoundTrip(t *testing.T) {
	c := Credentials{AccessKey: "OGMS3ABCDEFGHIJKLMNO", SecretKey: "s3cr3t"}
	now := time.Now()
	body := []byte("payload")
	req, _ := http.NewRequest("PUT", "http://hub.local:7777/s3/bkt/dir/a b+c~.txt?x-id=PutObject", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Color", "  blue   sky ")
	Sign(req, c, "eu-west-9", sha256Hex(body), now)

	// Replay as the server sees it.
	r := httptest.NewRequest("PUT", req.URL.RequestURI(), bytes.NewReader(body))
	r.Host = req.URL.Host
	r.Header = req.Header.Clone()
	a, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, c.SecretKey, now, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(a.Body(r, c.SecretKey)); err != nil || string(got) != "payload" {
		t.Fatalf("body %q %v", got, err)
	}
	// A body that doesn't match the signed hash is rejected at EOF.
	r2 := httptest.NewRequest("PUT", req.URL.RequestURI(), strings.NewReader("other"))
	r2.Host, r2.Header = r.Host, r.Header
	if _, err := io.ReadAll(a.Body(r2, c.SecretKey)); !isCode(err, "XAmzContentSHA256Mismatch") {
		t.Fatalf("mismatch: %v", err)
	}

	// Presigned.
	g, _ := http.NewRequest("GET", "http://hub.local:7777/s3/bkt/k%20y", nil)
	u := Presign(g, c, "us-east-1", time.Hour, now)
	r = httptest.NewRequest("GET", strings.TrimPrefix(u, "http://hub.local:7777"), nil)
	r.Host = "hub.local:7777"
	a, err = Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, c.SecretKey, now.Add(time.Minute), 15*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestNoAuth(t *testing.T) {
	if _, err := Parse(httptest.NewRequest("GET", "/x", nil)); err != ErrNoAuth {
		t.Fatal(err)
	}
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func TestV2Presigned(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	m := hmac.New(sha1.New, []byte("sekrit"))
	fmt.Fprintf(m, "GET\n\n\n%d\n/bkt/a%%20b.txt", exp)
	sig := url.QueryEscape(base64.StdEncoding.EncodeToString(m.Sum(nil)))
	r := httptest.NewRequest("GET", fmt.Sprintf("/s3/bkt/a%%20b.txt?AWSAccessKeyId=AK&Expires=%d&Signature=%s", exp, sig), nil)
	a, err := Parse(r)
	if err != nil || !a.V2 || a.AccessKey != "AK" {
		t.Fatalf("parse %v %+v", err, a)
	}
	if err := a.Verify(r, "sekrit", time.Now(), 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(r, "other", time.Now(), 15*time.Minute); !isCode(err, "SignatureDoesNotMatch") {
		t.Fatal(err)
	}
	if err := a.Verify(r, "sekrit", time.Now().Add(2*time.Hour), 15*time.Minute); !isCode(err, "AccessDenied") {
		t.Fatal(err)
	}
}
