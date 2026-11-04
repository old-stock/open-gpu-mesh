package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
	"github.com/old-stock/open-gpu-mesh/internal/s3sig"
)

type s3Cred struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Endpoint        string `json:"endpoint"`
	Project         string `json:"project"`
}

func newS3Cred(t *testing.T, c *client.Client, project string) s3Cred {
	t.Helper()
	var cr s3Cred
	if err := c.Do(context.Background(), "POST", "/v1/projects/"+project+"/s3-credentials", nil, &cr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cr.AccessKeyID, "OGMS3") || len(cr.AccessKeyID) != 20 || len(cr.SecretAccessKey) != 40 {
		t.Fatalf("bad credential %+v", cr)
	}
	return cr
}

func s3For(endpoint, ak, sk, region string) *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint:     aws.String(endpoint),
		Region:           region,
		UsePathStyle:     true,
		Credentials:      credentials.NewStaticCredentialsProvider(ak, sk, ""),
		RetryMaxAttempts: 1,
	})
}

func s3Code(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return fmt.Sprint(err)
}

func s3Status(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func TestS3Compat(t *testing.T) {
	cl := newCluster(t)
	ctx := cl.ctx
	cred := newS3Cred(t, cl.c, "default")
	if cred.Endpoint != cl.url+"/s3" {
		t.Fatalf("endpoint %q", cred.Endpoint)
	}
	// Any region in the credential scope works.
	s3c := s3For(cred.Endpoint, cred.AccessKeyID, cred.SecretAccessKey, "eu-central-1")
	bkt := aws.String("data-bkt")

	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bkt}); err != nil {
		t.Fatal(err)
	}
	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("Bad_Name")}); s3Code(err) != "InvalidBucketName" {
		t.Fatalf("bad bucket name: %v", err)
	}
	if _, err := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bkt}); err != nil {
		t.Fatal(err)
	}
	lb, err := s3c.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(lb.Buckets) != 1 || *lb.Buckets[0].Name != "data-bkt" {
		t.Fatalf("list buckets %v %+v", err, lb)
	}
	loc, err := s3c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: bkt})
	if err != nil || loc.LocationConstraint != "" {
		t.Fatalf("location %v %+v", err, loc)
	}

	put := func(key, body string) string {
		t.Helper()
		out, err := s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: bkt, Key: aws.String(key), Body: strings.NewReader(body),
			ContentType: aws.String("text/plain"), Metadata: map[string]string{"owner": "alice"}})
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		return *out.ETag
	}
	keys := []string{"a.txt", "dir/b.txt", "dir/d e+f~(1).txt", "dir/sub/c.txt", "dir/sub/deep/x.txt", "z.txt"}
	for _, k := range keys {
		put(k, "hello from "+k)
	}
	etag := put("a.txt", "0123456789abcdef")
	if etag != `"`+md5hex("0123456789abcdef")+`"` {
		t.Fatalf("etag %s", etag)
	}

	t.Run("get head range conditional", func(t *testing.T) {
		g, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("a.txt")})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(g.Body)
		g.Body.Close()
		if string(b) != "0123456789abcdef" || *g.ContentType != "text/plain" || g.Metadata["owner"] != "alice" || *g.ETag != etag {
			t.Fatalf("get: %q %v %v %v", b, *g.ContentType, g.Metadata, *g.ETag)
		}
		h, err := s3c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bkt, Key: aws.String("a.txt")})
		if err != nil || *h.ContentLength != 16 || *h.ETag != etag {
			t.Fatalf("head %v %+v", err, h)
		}
		if _, err := s3c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bkt, Key: aws.String("missing")}); s3Status(err) != 404 {
			t.Fatalf("head missing: %v", err)
		}
		if _, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("missing")}); s3Code(err) != "NoSuchKey" {
			t.Fatalf("get missing: %v", err)
		}
		r, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("a.txt"), Range: aws.String("bytes=2-5")})
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(r.Body)
		r.Body.Close()
		if string(b) != "2345" || *r.ContentRange != "bytes 2-5/16" {
			t.Fatalf("range %q %v", b, *r.ContentRange)
		}
		_, err = s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("a.txt"), IfNoneMatch: aws.String(etag)})
		if s3Status(err) != 304 {
			t.Fatalf("if-none-match: %v", err)
		}
	})

	t.Run("list v2 prefix delimiter pagination", func(t *testing.T) {
		l, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bkt, Prefix: aws.String("dir/"), Delimiter: aws.String("/")})
		if err != nil {
			t.Fatal(err)
		}
		var got, cps []string
		for _, o := range l.Contents {
			got = append(got, *o.Key)
		}
		for _, p := range l.CommonPrefixes {
			cps = append(cps, *p.Prefix)
		}
		if fmt.Sprint(got) != "[dir/b.txt dir/d e+f~(1).txt]" || fmt.Sprint(cps) != "[dir/sub/]" || *l.KeyCount != 3 {
			t.Fatalf("list: %v %v", got, cps)
		}
		// Paginate one key at a time, with delimiter at the root.
		var all []string
		p := s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{Bucket: bkt, Delimiter: aws.String("/"), MaxKeys: aws.Int32(1)})
		for p.HasMorePages() {
			pg, err := p.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range pg.Contents {
				all = append(all, *o.Key)
			}
			for _, c := range pg.CommonPrefixes {
				all = append(all, *c.Prefix)
			}
		}
		if fmt.Sprint(all) != "[a.txt dir/ z.txt]" {
			t.Fatalf("paginated: %v", all)
		}
		all = nil
		p = s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{Bucket: bkt, MaxKeys: aws.Int32(2), StartAfter: aws.String("a.txt")})
		for p.HasMorePages() {
			pg, err := p.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range pg.Contents {
				all = append(all, *o.Key)
			}
		}
		if len(all) != len(keys)-1 || all[0] != "dir/b.txt" {
			t.Fatalf("start-after: %v", all)
		}
		v1, err := s3c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bkt, Prefix: aws.String("dir/sub/")})
		if err != nil || len(v1.Contents) != 2 {
			t.Fatalf("v1 %v %+v", err, v1)
		}
	})

	t.Run("copy and delete", func(t *testing.T) {
		if _, err := s3c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bkt, Key: aws.String("copy/b.txt"), CopySource: aws.String("data-bkt/dir/b.txt")}); err != nil {
			t.Fatal(err)
		}
		g, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("copy/b.txt")})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(g.Body)
		g.Body.Close()
		if string(b) != "hello from dir/b.txt" || g.Metadata["owner"] != "alice" {
			t.Fatalf("copy: %q %v", b, g.Metadata)
		}
		d, err := s3c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bkt, Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a.txt")}, {Key: aws.String("z.txt")}, {Key: aws.String("never-existed")}}}})
		if err != nil || len(d.Deleted) != 3 {
			t.Fatalf("delete objects %v %+v", err, d)
		}
		if _, err := s3c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bkt, Key: aws.String("copy/b.txt")}); err != nil {
			t.Fatal(err)
		}
		l, _ := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bkt})
		for _, o := range l.Contents {
			if k := *o.Key; k == "a.txt" || k == "z.txt" || k == "copy/b.txt" {
				t.Fatalf("%s still listed", k)
			}
		}
		if _, err := s3c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bkt}); s3Code(err) != "BucketNotEmpty" {
			t.Fatalf("delete non-empty bucket: %v", err)
		}
	})

	t.Run("multipart 12MiB", func(t *testing.T) {
		data := make([]byte, 12<<20)
		_, _ = rand.Read(data)
		key := aws.String("big/blob.bin")
		cu, err := s3c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bkt, Key: key, ContentType: aws.String("application/x-test")})
		if err != nil {
			t.Fatal(err)
		}
		bounds := [][2]int{{0, 5 << 20}, {5 << 20, 10 << 20}, {10 << 20, 12 << 20}}
		var parts []types.CompletedPart
		for i, b := range bounds {
			up, err := s3c.UploadPart(ctx, &s3.UploadPartInput{Bucket: bkt, Key: key, UploadId: cu.UploadId, PartNumber: aws.Int32(int32(i + 1)),
				Body: bytes.NewReader(data[b[0]:b[1]])})
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))})
		}
		lp, err := s3c.ListParts(ctx, &s3.ListPartsInput{Bucket: bkt, Key: key, UploadId: cu.UploadId})
		if err != nil || len(lp.Parts) != 3 {
			t.Fatalf("list parts %v %+v", err, lp)
		}
		cm, err := s3c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: bkt, Key: key, UploadId: cu.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(*cm.ETag, `-3"`) {
			t.Fatalf("multipart etag %s", *cm.ETag)
		}
		g, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(g.Body)
		g.Body.Close()
		if !bytes.Equal(got, data) || *g.ContentType != "application/x-test" || *g.ETag != *cm.ETag {
			t.Fatalf("multipart body mismatch (%d bytes, %s)", len(got), *g.ContentType)
		}
		// Too-small non-final parts are refused; aborting cleans up.
		cu2, _ := s3c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bkt, Key: aws.String("small")})
		var p2 []types.CompletedPart
		for i := 1; i <= 2; i++ {
			up, err := s3c.UploadPart(ctx, &s3.UploadPartInput{Bucket: bkt, Key: aws.String("small"), UploadId: cu2.UploadId, PartNumber: aws.Int32(int32(i)), Body: strings.NewReader("tiny")})
			if err != nil {
				t.Fatal(err)
			}
			p2 = append(p2, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i))})
		}
		_, err = s3c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: bkt, Key: aws.String("small"), UploadId: cu2.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: p2}})
		if s3Code(err) != "EntityTooSmall" {
			t.Fatalf("small parts: %v", err)
		}
		if _, err := s3c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: bkt, Key: aws.String("small"), UploadId: cu2.UploadId}); err != nil {
			t.Fatal(err)
		}
		if _, err := s3c.ListParts(ctx, &s3.ListPartsInput{Bucket: bkt, Key: aws.String("small"), UploadId: cu2.UploadId}); s3Code(err) != "NoSuchUpload" {
			t.Fatalf("aborted upload still listed: %v", err)
		}
	})

	t.Run("presigned GET", func(t *testing.T) {
		ps, err := s3.NewPresignClient(s3c).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("dir/d e+f~(1).txt")},
			s3.WithPresignExpires(10*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(ps.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "hello from dir/d e+f~(1).txt" {
			t.Fatalf("presigned: %d %s", resp.StatusCode, b)
		}
		// Tampering with the URL breaks the signature.
		resp, _ = http.Get(strings.Replace(ps.URL, "X-Amz-Expires=600", "X-Amz-Expires=6000", 1))
		b, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || !strings.Contains(string(b), "<Code>SignatureDoesNotMatch</Code>") {
			t.Fatalf("tampered presign: %d %s", resp.StatusCode, b)
		}
		// Unsigned requests are refused with an S3 XML error.
		resp, _ = http.Get(cl.url + "/s3/data-bkt/dir/b.txt")
		b, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || !strings.Contains(string(b), "<Code>AccessDenied</Code>") {
			t.Fatalf("anonymous: %d %s", resp.StatusCode, b)
		}
	})

	t.Run("aws-chunked uploads", func(t *testing.T) {
		sc := s3sig.Credentials{AccessKey: cred.AccessKeyID, SecretKey: cred.SecretAccessKey}
		payload := bytes.Repeat([]byte("chunky!"), 30000) // 210 KB -> several chunks
		for _, mode := range []string{s3sig.StreamingPayload, s3sig.StreamingUnsignedTrailer} {
			key := "chunked/" + strings.ToLower(mode)
			req := chunkedPut(t, cl.url+"/s3/data-bkt/"+key, sc, mode, payload)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s: %d %s", mode, resp.StatusCode, b)
			}
			g, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String(key)})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(g.Body)
			g.Body.Close()
			if !bytes.Equal(got, payload) || g.ContentEncoding != nil {
				t.Fatalf("%s: stored %d bytes, encoding %v", mode, len(got), g.ContentEncoding)
			}
		}
	})

	t.Run("traversal keys stay in the store", func(t *testing.T) {
		for _, k := range []string{"../../escape.txt", "..%2F..%2Fx", "a/./b/../c", "/leading"} {
			put(k, "k="+k)
			g, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String(k)})
			if err != nil {
				t.Fatalf("%s: %v", k, err)
			}
			b, _ := io.ReadAll(g.Body)
			g.Body.Close()
			if string(b) != "k="+k {
				t.Fatalf("%s: got %q", k, b)
			}
			if _, err := s3c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bkt, Key: aws.String(k)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(filepath.Join(cl.srv.Config().DataDir, "..", "escape.txt")); err == nil {
			t.Fatal("key escaped the data dir")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		bad := s3For(cred.Endpoint, cred.AccessKeyID, strings.Repeat("x", 40), "us-east-1")
		if _, err := bad.ListBuckets(ctx, &s3.ListBucketsInput{}); s3Code(err) != "SignatureDoesNotMatch" {
			t.Fatalf("wrong secret: %v", err)
		}
		unknown := s3For(cred.Endpoint, "OGMS3NOPENOPENOPENOPE", cred.SecretAccessKey, "us-east-1")
		if _, err := unknown.ListBuckets(ctx, &s3.ListBucketsInput{}); s3Code(err) != "InvalidAccessKeyId" {
			t.Fatalf("unknown key: %v", err)
		}
	})

	t.Run("project isolation", func(t *testing.T) {
		if err := cl.c.Do(ctx, "POST", "/v1/projects", map[string]any{"name": "private"}, nil); err != nil {
			t.Fatal(err)
		}
		pc := newS3Cred(t, cl.c, "private")
		priv := s3For(pc.Endpoint, pc.AccessKeyID, pc.SecretAccessKey, "us-east-1")
		if _, err := priv.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("private-bkt")}); err != nil {
			t.Fatal(err)
		}
		// Credentials are scoped to their project even for an admin user.
		if _, err := priv.GetObject(ctx, &s3.GetObjectInput{Bucket: bkt, Key: aws.String("dir/b.txt")}); s3Code(err) != "AccessDenied" {
			t.Fatalf("cross-project get: %v", err)
		}
		if _, err := s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("private-bkt"), Key: aws.String("x"), Body: strings.NewReader("x")}); s3Code(err) != "AccessDenied" {
			t.Fatalf("cross-project put: %v", err)
		}
		if _, err := priv.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bkt}); s3Code(err) != "BucketAlreadyExists" {
			t.Fatalf("steal bucket name: %v", err)
		}
		lb, _ := priv.ListBuckets(ctx, &s3.ListBucketsInput{})
		if len(lb.Buckets) != 1 || *lb.Buckets[0].Name != "private-bkt" {
			t.Fatalf("private list buckets %+v", lb.Buckets)
		}
		// A non-member can't mint credentials for the project.
		var inv api.Invite
		if err := cl.c.Do(ctx, "POST", "/v1/invites", map[string]any{"uses": 1}, &inv); err != nil {
			t.Fatal(err)
		}
		var mallory api.User
		if err := cl.c.Do(ctx, "POST", "/v1/join", map[string]any{"code": inv.Code, "name": "mallory"}, &mallory); err != nil {
			t.Fatal(err)
		}
		mc := client.New(cl.url, mallory.APIKey)
		if err := mc.Do(ctx, "POST", "/v1/projects/private/s3-credentials", nil, nil); err == nil || !strings.Contains(err.Error(), "member") {
			t.Fatalf("non-member credential: %v", err)
		}
		// Deleted credentials stop working; listing never shows secrets.
		var list api.List[map[string]any]
		if err := cl.c.Do(ctx, "GET", "/v1/projects/private/s3-credentials", nil, &list); err != nil || len(list.Items) != 1 {
			t.Fatalf("list creds %v %+v", err, list)
		}
		if _, ok := list.Items[0]["secret_access_key"]; ok || list.Items[0]["last_used"] == nil {
			t.Fatalf("listed credential %+v", list.Items[0])
		}
		if err := cl.c.Do(ctx, "DELETE", "/v1/projects/private/s3-credentials/"+pc.AccessKeyID, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := priv.ListBuckets(ctx, &s3.ListBucketsInput{}); s3Code(err) != "InvalidAccessKeyId" {
			t.Fatalf("deleted cred: %v", err)
		}
	})

	t.Run("delete bucket", func(t *testing.T) {
		var objs []types.ObjectIdentifier
		l, _ := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bkt})
		for _, o := range l.Contents {
			objs = append(objs, types.ObjectIdentifier{Key: o.Key})
		}
		sort.Slice(objs, func(i, j int) bool { return *objs[i].Key < *objs[j].Key })
		if _, err := s3c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bkt, Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s3c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bkt}); err != nil {
			t.Fatal(err)
		}
		if _, err := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bkt}); s3Status(err) != 404 {
			t.Fatalf("deleted bucket head: %v", err)
		}
	})
}

func md5hex(s string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(s)))
}

// chunkedPut builds an aws-chunked PUT the way boto3/aws-cli do (signed chunks) or the way
// newer SDKs do with trailing checksums (unsigned chunks + trailer).
func chunkedPut(t *testing.T, u string, c s3sig.Credentials, mode string, payload []byte) *http.Request {
	t.Helper()
	const chunk = 64 << 10
	var chunks [][]byte
	for i := 0; i < len(payload); i += chunk {
		chunks = append(chunks, payload[i:min(i+chunk, len(payload))])
	}
	chunks = append(chunks, nil)
	signed := mode == s3sig.StreamingPayload
	sigLen := 0
	if signed {
		sigLen = len(";chunk-signature=") + 64
	}
	var total int
	for _, ch := range chunks {
		total += len(fmt.Sprintf("%x", len(ch))) + sigLen + 2 + len(ch) + 2
	}
	trailer := ""
	if !signed {
		trailer = "x-amz-checksum-crc32:AAAAAA==\r\n"
		total += len(trailer)
	}
	req, _ := http.NewRequest("PUT", u, nil)
	req.ContentLength = int64(total)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
	if !signed {
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	}
	now := time.Now().UTC()
	s3sig.Sign(req, c, "us-east-1", mode, now)
	auth := req.Header.Get("Authorization")
	prev := auth[strings.LastIndex(auth, "=")+1:]
	key := s3sig.SigningKey(c.SecretKey, now.Format(s3sig.DateFormat), "us-east-1", "s3")
	scope := s3sig.Scope(now.Format(s3sig.DateFormat), "us-east-1", "s3")
	var body bytes.Buffer
	for i, ch := range chunks {
		fmt.Fprintf(&body, "%x", len(ch))
		if signed {
			h := sha256.Sum256(ch)
			sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + now.Format(s3sig.TimeFormat) + "\n" + scope + "\n" + prev + "\n" + s3sig.EmptySHA256 + "\n" + hex.EncodeToString(h[:])
			m := hmac.New(sha256.New, key)
			m.Write([]byte(sts))
			prev = hex.EncodeToString(m.Sum(nil))
			body.WriteString(";chunk-signature=" + prev)
		}
		body.WriteString("\r\n")
		body.Write(ch)
		if i == len(chunks)-1 {
			body.WriteString(trailer)
		}
		body.WriteString("\r\n")
	}
	if body.Len() != total {
		t.Fatalf("chunked length %d != %d", body.Len(), total)
	}
	req.Body = io.NopCloser(&body)
	return req
}

func TestS3Quota(t *testing.T) {
	cl := newCluster(t)
	ctx := cl.ctx
	if err := cl.c.Do(ctx, "PUT", "/v1/projects/default/quota", map[string]int{"storage_mb": 1}, nil); err != nil {
		t.Fatal(err)
	}
	cred := newS3Cred(t, cl.c, "default")
	s3c := s3For(cred.Endpoint, cred.AccessKeyID, cred.SecretAccessKey, "us-east-1")
	bkt := aws.String("q-bkt")
	if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bkt}); err != nil {
		t.Fatal(err)
	}
	put := func(key string, n int) error {
		_, err := s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: bkt, Key: aws.String(key), Body: bytes.NewReader(bytes.Repeat([]byte("x"), n))})
		return err
	}
	if err := put("a", 700<<10); err != nil {
		t.Fatalf("within quota: %v", err)
	}
	if err := put("b", 700<<10); s3Code(err) != "QuotaExceeded" || s3Status(err) != 403 {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := s3c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bkt, Key: aws.String("a")}); err != nil {
		t.Fatal(err)
	}
	if err := put("b", 700<<10); err != nil {
		t.Fatalf("after freeing space: %v", err)
	}
}
