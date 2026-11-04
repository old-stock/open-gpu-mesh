package server

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/s3sig"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

type s3Object struct {
	Key         string
	Size        int64
	ETag        string // without quotes
	ContentType string
	Meta        map[string]string // x-amz-meta-* (lowercase name without prefix)
	Headers     map[string]string // stored standard headers (Cache-Control, ...)
	Blob        string
	Modified    int64
}

var s3StoredHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

func s3Time(ms int64) string { return store.T(ms).Format("2006-01-02T15:04:05.000Z") }

func quoteETag(e string) string { return `"` + e + `"` }

func unquoteETag(e string) string { return strings.Trim(strings.TrimSpace(e), `"`) }

func (s *Server) s3BlobPath(bucket, blob string) string {
	return filepath.Join(s.s3Root(), bucket, blob[:2], blob)
}

func newBlobID() string {
	return strings.ToLower(randAlnum(32, "abcdefghijklmnopqrstuvwxyz0123456789"))
}

// s3WriteBlob streams r into a new blob file and returns its id, size and md5.
func (s *Server) s3WriteBlob(bucket string, r io.Reader, limit int64) (blob string, size int64, sum []byte, err error) {
	blob = newBlobID()
	dst := s.s3BlobPath(bucket, blob)
	if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return "", 0, nil, err
	}
	h := md5.New()
	quotaLimited := false
	var project string
	if s.st.DB.QueryRow(`SELECT project FROM s3_buckets WHERE name=?`, bucket).Scan(&project) == nil {
		if left := s.storageRemaining(project); left >= 0 && left < limit {
			limit, quotaLimited = left, true
		}
	}
	size, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && size > limit {
		if quotaLimited {
			err = &s3sig.Error{Code: "QuotaExceeded", Message: "this upload exceeds the project's storage quota", Status: 403}
		} else {
			err = &s3sig.Error{Code: "EntityTooLarge", Message: "your proposed upload exceeds the maximum allowed size", Status: 400}
		}
	}
	if err == nil {
		err = os.Rename(f.Name(), dst)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, nil, err
	}
	return blob, size, h.Sum(nil), nil
}

func (s *Server) s3RemoveBlob(bucket, blob string) {
	if blob != "" {
		_ = os.Remove(s.s3BlobPath(bucket, blob))
	}
}

// s3RequestMeta collects x-amz-meta-*, the content type and stored headers from a request.
func s3RequestMeta(r *http.Request) (ct string, meta, hdrs map[string]string) {
	meta, hdrs = map[string]string{}, map[string]string{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(v) > 0 {
			meta[strings.TrimPrefix(lk, "x-amz-meta-")] = strings.Join(v, ",")
		}
	}
	for _, h := range s3StoredHeaders {
		v := r.Header.Get(h)
		if h == "Content-Encoding" {
			var keep []string
			for _, e := range strings.Split(v, ",") {
				if e = strings.TrimSpace(e); e != "" && e != "aws-chunked" {
					keep = append(keep, e)
				}
			}
			v = strings.Join(keep, ",")
		}
		if v != "" {
			hdrs[h] = v
		}
	}
	ct = r.Header.Get("Content-Type")
	if ct == "" {
		ct = "binary/octet-stream"
	}
	return ct, meta, hdrs
}

// checkContentMD5 compares an optional Content-MD5 header with the computed digest.
func checkContentMD5(r *http.Request, sum []byte) error {
	v := r.Header.Get("Content-MD5")
	if v == "" {
		return nil
	}
	want, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(want) != md5.Size {
		return &s3sig.Error{Code: "InvalidDigest", Message: "the Content-MD5 you specified is not valid", Status: 400}
	}
	if !bytes.Equal(want, sum) {
		return &s3sig.Error{Code: "BadDigest", Message: "the Content-MD5 you specified did not match what we received", Status: 400}
	}
	return nil
}

// s3SetObject records an object version and deletes the blob it replaces.
func (s *Server) s3SetObject(bucket string, o *s3Object) error {
	tx, err := s.st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	_ = tx.QueryRow(`SELECT blob FROM s3_objects WHERE bucket=? AND key=?`, bucket, o.Key).Scan(&old)
	if _, err := tx.Exec(`INSERT INTO s3_objects(bucket,key,size,etag,content_type,user_meta,headers,blob,modified) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(bucket,key) DO UPDATE SET size=excluded.size, etag=excluded.etag, content_type=excluded.content_type,
		user_meta=excluded.user_meta, headers=excluded.headers, blob=excluded.blob, modified=excluded.modified`,
		bucket, o.Key, o.Size, o.ETag, o.ContentType, store.JSON(o.Meta), store.JSON(o.Headers), o.Blob, o.Modified); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if old != o.Blob {
		s.s3RemoveBlob(bucket, old)
	}
	return nil
}

func (s *Server) s3LoadObject(bucket, key string) (*s3Object, error) {
	o := &s3Object{Key: key}
	var meta, hdrs string
	err := s.st.DB.QueryRow(`SELECT size,etag,content_type,user_meta,headers,blob,modified FROM s3_objects WHERE bucket=? AND key=?`, bucket, key).
		Scan(&o.Size, &o.ETag, &o.ContentType, &meta, &hdrs, &o.Blob, &o.Modified)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(meta), &o.Meta)
	_ = json.Unmarshal([]byte(hdrs), &o.Headers)
	return o, nil
}

// ---------- buckets ----------

type s3Owner struct {
	ID          string
	DisplayName string
}

func (s *Server) s3ListBuckets(c *s3Req) {
	type bucket struct {
		Name         string
		CreationDate string
	}
	rows, err := s.st.DB.Query(`SELECT name, created FROM s3_buckets WHERE project=? ORDER BY name`, c.project)
	if err != nil {
		c.failErr(err)
		return
	}
	var out []bucket
	for rows.Next() {
		var b bucket
		var t int64
		if rows.Scan(&b.Name, &t) == nil {
			b.CreationDate = s3Time(t)
			out = append(out, b)
		}
	}
	rows.Close()
	c.xml(http.StatusOK, struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		NS      string   `xml:"xmlns,attr"`
		Owner   s3Owner
		Buckets []bucket `xml:"Buckets>Bucket"`
	}{NS: s3XMLNS, Owner: s3Owner{c.project, c.project}, Buckets: out})
}

func (s *Server) s3CreateBucket(c *s3Req) {
	_, _ = io.Copy(io.Discard, io.LimitReader(c.auth.Body(c.r, c.secret), 1<<20)) // CreateBucketConfiguration: any region is fine
	if !validBucketName(c.bucket) {
		c.fail(http.StatusBadRequest, "InvalidBucketName", "bucket names are 3-63 chars of lowercase letters, digits, '-' and '.', starting and ending with a letter or digit")
		return
	}
	var project string
	if s.st.DB.QueryRow(`SELECT project FROM s3_buckets WHERE name=?`, c.bucket).Scan(&project) == nil {
		if project == c.project {
			c.fail(http.StatusConflict, "BucketAlreadyOwnedByYou", "your previous request to create the named bucket succeeded and you already own it")
		} else {
			c.fail(http.StatusConflict, "BucketAlreadyExists", "the requested bucket name is not available")
		}
		return
	}
	if _, err := s.st.DB.Exec(`INSERT INTO s3_buckets(name,project,created) VALUES(?,?,?)`, c.bucket, c.project, store.Now()); err != nil {
		c.fail(http.StatusConflict, "BucketAlreadyExists", "the requested bucket name is not available")
		return
	}
	if err := os.MkdirAll(filepath.Join(s.s3Root(), c.bucket), 0o755); err != nil {
		c.failErr(err)
		return
	}
	s.st.AddEvent(c.project, "", "", "", "s3.bucket.created", "S3 bucket "+c.bucket+" created")
	c.w.Header().Set("Location", "/"+c.bucket)
	c.w.WriteHeader(http.StatusOK)
}

func (s *Server) s3DeleteBucket(c *s3Req) {
	var n int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM s3_objects WHERE bucket=?`, c.bucket).Scan(&n)
	if n > 0 {
		c.fail(http.StatusConflict, "BucketNotEmpty", "the bucket you tried to delete is not empty")
		return
	}
	_, _ = s.st.DB.Exec(`DELETE FROM s3_parts WHERE upload_id IN (SELECT upload_id FROM s3_uploads WHERE bucket=?)`, c.bucket)
	_, _ = s.st.DB.Exec(`DELETE FROM s3_uploads WHERE bucket=?`, c.bucket)
	if _, err := s.st.DB.Exec(`DELETE FROM s3_buckets WHERE name=?`, c.bucket); err != nil {
		c.failErr(err)
		return
	}
	_ = os.RemoveAll(filepath.Join(s.s3Root(), c.bucket))
	s.st.AddEvent(c.project, "", "", "", "s3.bucket.deleted", "S3 bucket "+c.bucket+" deleted")
	c.w.WriteHeader(http.StatusNoContent)
}

// ---------- listing ----------

type s3Contents struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

type s3Prefix struct {
	Prefix string
}

// s3List walks keys after `after` under prefix, grouping by delimiter. A marker that is itself
// a common prefix skips everything under it.
func (s *Server) s3List(bucket, prefix, delim, after string, max int) (objs []s3Contents, cps []s3Prefix, truncated bool, next string, err error) {
	if max <= 0 {
		return nil, nil, false, "", nil
	}
	skip := ""
	if delim != "" && strings.HasSuffix(after, delim) && strings.HasPrefix(after, prefix) && len(after) > len(prefix) {
		skip = after
	}
	lower := prefix
	rows, err := s.st.DB.Query(`SELECT key,size,etag,modified FROM s3_objects WHERE bucket=? AND key>=? AND key>? ORDER BY key`, bucket, lower, after)
	if err != nil {
		return nil, nil, false, "", err
	}
	defer rows.Close()
	count, lastCP := 0, ""
	for rows.Next() {
		var k, etag string
		var size, mod int64
		if err := rows.Scan(&k, &size, &etag, &mod); err != nil {
			return nil, nil, false, "", err
		}
		if !strings.HasPrefix(k, prefix) {
			break
		}
		if skip != "" && strings.HasPrefix(k, skip) {
			continue
		}
		if delim != "" {
			if i := strings.Index(k[len(prefix):], delim); i >= 0 {
				cp := k[:len(prefix)+i+len(delim)]
				if cp == lastCP {
					continue
				}
				if count == max {
					truncated = true
					break
				}
				cps = append(cps, s3Prefix{cp})
				lastCP, next = cp, cp
				count++
				continue
			}
		}
		if count == max {
			truncated = true
			break
		}
		objs = append(objs, s3Contents{Key: k, LastModified: s3Time(mod), ETag: quoteETag(etag), Size: size, StorageClass: "STANDARD"})
		next = k
		count++
	}
	return objs, cps, truncated, next, rows.Err()
}

func (s *Server) s3ListObjects(c *s3Req, v2 bool) {
	q := c.r.URL.Query()
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	max := 1000
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.fail(http.StatusBadRequest, "InvalidArgument", "max-keys must be a non-negative integer")
			return
		}
		max = min(n, 1000)
	}
	enc := func(v string) string { return v }
	encType := ""
	if q.Get("encoding-type") == "url" {
		encType = "url"
		enc = s3sig.EscapePath
	}
	after := q.Get("marker")
	token := q.Get("continuation-token")
	if v2 {
		after = q.Get("start-after")
		if token != "" {
			b, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil {
				c.fail(http.StatusBadRequest, "InvalidArgument", "the continuation token provided is incorrect")
				return
			}
			after = string(b)
		}
	}
	objs, cps, truncated, next, err := s.s3List(c.bucket, prefix, delim, after, max)
	if err != nil {
		c.failErr(err)
		return
	}
	for i := range objs {
		objs[i].Key = enc(objs[i].Key)
	}
	for i := range cps {
		cps[i].Prefix = enc(cps[i].Prefix)
	}
	if v2 {
		res := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			NS                    string   `xml:"xmlns,attr"`
			Name                  string
			Prefix                string
			Delimiter             string `xml:",omitempty"`
			MaxKeys               int
			KeyCount              int
			IsTruncated           bool
			ContinuationToken     string `xml:",omitempty"`
			NextContinuationToken string `xml:",omitempty"`
			StartAfter            string `xml:",omitempty"`
			EncodingType          string `xml:",omitempty"`
			Contents              []s3Contents
			CommonPrefixes        []s3Prefix
		}{NS: s3XMLNS, Name: c.bucket, Prefix: enc(prefix), Delimiter: enc(delim), MaxKeys: max, KeyCount: len(objs) + len(cps),
			IsTruncated: truncated, ContinuationToken: token, StartAfter: enc(q.Get("start-after")), EncodingType: encType,
			Contents: objs, CommonPrefixes: cps}
		if truncated {
			res.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(next))
		}
		c.xml(http.StatusOK, res)
		return
	}
	res := struct {
		XMLName        xml.Name `xml:"ListBucketResult"`
		NS             string   `xml:"xmlns,attr"`
		Name           string
		Prefix         string
		Marker         string
		NextMarker     string `xml:",omitempty"`
		Delimiter      string `xml:",omitempty"`
		MaxKeys        int
		IsTruncated    bool
		EncodingType   string `xml:",omitempty"`
		Contents       []s3Contents
		CommonPrefixes []s3Prefix
	}{NS: s3XMLNS, Name: c.bucket, Prefix: enc(prefix), Marker: enc(after), Delimiter: enc(delim), MaxKeys: max,
		IsTruncated: truncated, EncodingType: encType, Contents: objs, CommonPrefixes: cps}
	if truncated {
		res.NextMarker = enc(next)
	}
	c.xml(http.StatusOK, res)
}

// ---------- objects ----------

func (s *Server) s3PutObject(c *s3Req) {
	body := c.auth.Body(c.r, c.secret)
	blob, size, sum, err := s.s3WriteBlob(c.bucket, body, s3MaxPart)
	if err != nil {
		c.failErr(err)
		return
	}
	if err := s.s3CheckUpload(c, size, sum); err != nil {
		s.s3RemoveBlob(c.bucket, blob)
		c.failErr(err)
		return
	}
	ct, meta, hdrs := s3RequestMeta(c.r)
	o := &s3Object{Key: c.key, Size: size, ETag: hex.EncodeToString(sum), ContentType: ct, Meta: meta, Headers: hdrs, Blob: blob, Modified: store.Now()}
	if err := s.s3SetObject(c.bucket, o); err != nil {
		s.s3RemoveBlob(c.bucket, blob)
		c.failErr(err)
		return
	}
	c.w.Header().Set("ETag", quoteETag(o.ETag))
	c.w.WriteHeader(http.StatusOK)
}

// s3CheckUpload validates Content-MD5 and the declared decoded length of aws-chunked bodies.
func (s *Server) s3CheckUpload(c *s3Req, size int64, sum []byte) error {
	if err := checkContentMD5(c.r, sum); err != nil {
		return err
	}
	if v := c.r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" && c.auth.IsStreaming() {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n != size {
			return &s3sig.Error{Code: "IncompleteBody", Message: "you did not provide the number of bytes specified by x-amz-decoded-content-length", Status: 400}
		}
	}
	return nil
}

func (s *Server) s3GetObject(c *s3Req) {
	o, err := s.s3LoadObject(c.bucket, c.key)
	if err != nil {
		c.fail(http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
		return
	}
	f, err := os.Open(s.s3BlobPath(c.bucket, o.Blob))
	if err != nil {
		c.fail(http.StatusInternalServerError, "InternalError", "object data is missing on disk")
		return
	}
	defer f.Close()
	h := c.w.Header()
	h.Set("ETag", quoteETag(o.ETag))
	h.Set("Content-Type", o.ContentType)
	h.Set("Accept-Ranges", "bytes")
	for k, v := range o.Headers {
		h.Set(k, v)
	}
	for k, v := range o.Meta {
		h["x-amz-meta-"+k] = []string{v} // lowercase like S3; boto3 keeps the case it sees
	}
	q := c.r.URL.Query()
	for param, hdr := range map[string]string{"response-content-type": "Content-Type", "response-content-disposition": "Content-Disposition",
		"response-content-encoding": "Content-Encoding", "response-content-language": "Content-Language",
		"response-cache-control": "Cache-Control", "response-expires": "Expires"} {
		if v := q.Get(param); v != "" {
			h.Set(hdr, v)
		}
	}
	hardenUserContent(h, path.Base(c.key))
	http.ServeContent(c.w, c.r, "", time.UnixMilli(o.Modified), f)
}

func (s *Server) s3DeleteKey(bucket, key string) error {
	var blob string
	if s.st.DB.QueryRow(`SELECT blob FROM s3_objects WHERE bucket=? AND key=?`, bucket, key).Scan(&blob) != nil {
		return nil // deleting a missing key succeeds in S3
	}
	if _, err := s.st.DB.Exec(`DELETE FROM s3_objects WHERE bucket=? AND key=? AND blob=?`, bucket, key, blob); err != nil {
		return err
	}
	s.s3RemoveBlob(bucket, blob)
	return nil
}

func (s *Server) s3DeleteObject(c *s3Req) {
	if err := s.s3DeleteKey(c.bucket, c.key); err != nil {
		c.failErr(err)
		return
	}
	c.w.WriteHeader(http.StatusNoContent)
}

func (s *Server) s3DeleteObjects(c *s3Req) {
	var req struct {
		Quiet   bool
		Objects []struct{ Key string } `xml:"Object"`
	}
	b, err := io.ReadAll(io.LimitReader(c.auth.Body(c.r, c.secret), 2<<20))
	if err != nil {
		c.failErr(err)
		return
	}
	if err := xml.Unmarshal(b, &req); err != nil {
		c.fail(http.StatusBadRequest, "MalformedXML", "the XML you provided was not well-formed")
		return
	}
	if len(req.Objects) > 1000 {
		c.fail(http.StatusBadRequest, "MalformedXML", "at most 1000 keys per request")
		return
	}
	type deleted struct{ Key string }
	type delErr struct{ Key, Code, Message string }
	res := struct {
		XMLName xml.Name  `xml:"DeleteResult"`
		NS      string    `xml:"xmlns,attr"`
		Deleted []deleted `xml:"Deleted"`
		Errors  []delErr  `xml:"Error"`
	}{NS: s3XMLNS}
	for _, o := range req.Objects {
		if err := s.s3DeleteKey(c.bucket, o.Key); err != nil {
			res.Errors = append(res.Errors, delErr{o.Key, "InternalError", err.Error()})
		} else if !req.Quiet {
			res.Deleted = append(res.Deleted, deleted{o.Key})
		}
	}
	c.xml(http.StatusOK, res)
}

// s3CopySource parses x-amz-copy-source ("/bucket/key" or "bucket/key", URL-encoded) and
// checks the source bucket belongs to the caller's project.
func (s *Server) s3CopySource(c *s3Req) (*s3Object, string, bool) {
	src := c.r.Header.Get("X-Amz-Copy-Source")
	if i := strings.Index(src, "?versionId="); i >= 0 {
		src = src[:i]
	}
	if u, err := url.PathUnescape(src); err == nil {
		src = u
	}
	sb, sk, ok := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	if !ok || sb == "" || sk == "" {
		c.fail(http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source must be BUCKET/KEY")
		return nil, "", false
	}
	if !s.s3BucketAccess(c, sb) {
		return nil, "", false
	}
	o, err := s.s3LoadObject(sb, sk)
	if err != nil {
		c.fail(http.StatusNotFound, "NoSuchKey", "the specified copy source does not exist")
		return nil, "", false
	}
	if m := c.r.Header.Get("X-Amz-Copy-Source-If-Match"); m != "" && unquoteETag(m) != o.ETag {
		c.fail(http.StatusPreconditionFailed, "PreconditionFailed", "x-amz-copy-source-if-match did not match")
		return nil, "", false
	}
	if m := c.r.Header.Get("X-Amz-Copy-Source-If-None-Match"); m != "" && unquoteETag(m) == o.ETag {
		c.fail(http.StatusPreconditionFailed, "PreconditionFailed", "x-amz-copy-source-if-none-match matched")
		return nil, "", false
	}
	return o, sb, true
}

func (s *Server) s3CopyObject(c *s3Req) {
	src, sb, ok := s.s3CopySource(c)
	if !ok {
		return
	}
	f, err := os.Open(s.s3BlobPath(sb, src.Blob))
	if err != nil {
		c.fail(http.StatusInternalServerError, "InternalError", "source data is missing on disk")
		return
	}
	blob, size, _, err := s.s3WriteBlob(c.bucket, f, s3MaxObject)
	f.Close()
	if err != nil {
		c.failErr(err)
		return
	}
	o := &s3Object{Key: c.key, Size: size, ETag: src.ETag, ContentType: src.ContentType, Meta: src.Meta, Headers: src.Headers, Blob: blob, Modified: store.Now()}
	if strings.EqualFold(c.r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		o.ContentType, o.Meta, o.Headers = s3RequestMeta(c.r)
	}
	if err := s.s3SetObject(c.bucket, o); err != nil {
		s.s3RemoveBlob(c.bucket, blob)
		c.failErr(err)
		return
	}
	c.xml(http.StatusOK, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		NS           string   `xml:"xmlns,attr"`
		LastModified string
		ETag         string
	}{NS: s3XMLNS, LastModified: s3Time(o.Modified), ETag: quoteETag(o.ETag)})
}

// ---------- multipart ----------

func (s *Server) s3CreateUpload(c *s3Req) {
	id := newBlobID()
	ct, meta, hdrs := s3RequestMeta(c.r)
	if _, err := s.st.DB.Exec(`INSERT INTO s3_uploads(upload_id,bucket,key,content_type,user_meta,headers,created) VALUES(?,?,?,?,?,?,?)`,
		id, c.bucket, c.key, ct, store.JSON(meta), store.JSON(hdrs), store.Now()); err != nil {
		c.failErr(err)
		return
	}
	c.xml(http.StatusOK, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Bucket   string
		Key      string
		UploadId string
	}{NS: s3XMLNS, Bucket: c.bucket, Key: c.key, UploadId: id})
}

// s3Upload checks an upload id belongs to this bucket/key.
func (s *Server) s3Upload(c *s3Req) (string, bool) {
	id := c.r.URL.Query().Get("uploadId")
	var n int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM s3_uploads WHERE upload_id=? AND bucket=? AND key=?`, id, c.bucket, c.key).Scan(&n)
	if n == 0 {
		c.fail(http.StatusNotFound, "NoSuchUpload", "the specified multipart upload does not exist")
		return "", false
	}
	return id, true
}

func (s *Server) s3UploadPart(c *s3Req) {
	id, ok := s.s3Upload(c)
	if !ok {
		return
	}
	pn, err := strconv.Atoi(c.r.URL.Query().Get("partNumber"))
	if err != nil || pn < 1 || pn > 10000 {
		c.fail(http.StatusBadRequest, "InvalidArgument", "partNumber must be an integer between 1 and 10000")
		return
	}
	var blob string
	var size int64
	var sum []byte
	copying := c.r.Header.Get("X-Amz-Copy-Source") != ""
	if copying {
		src, sb, ok := s.s3CopySource(c)
		if !ok {
			return
		}
		f, err := os.Open(s.s3BlobPath(sb, src.Blob))
		if err != nil {
			c.fail(http.StatusInternalServerError, "InternalError", "source data is missing on disk")
			return
		}
		defer f.Close()
		var rd io.Reader = f
		if rng := c.r.Header.Get("X-Amz-Copy-Source-Range"); rng != "" {
			var a, b int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err != nil || a < 0 || b < a || b >= src.Size {
				c.fail(http.StatusBadRequest, "InvalidArgument", "x-amz-copy-source-range must be bytes=first-last within the source")
				return
			}
			rd = io.NewSectionReader(f, a, b-a+1)
		}
		blob, size, sum, err = s.s3WriteBlob(c.bucket, rd, s3MaxPart)
		if err != nil {
			c.failErr(err)
			return
		}
	} else {
		blob, size, sum, err = s.s3WriteBlob(c.bucket, c.auth.Body(c.r, c.secret), s3MaxPart)
		if err != nil {
			c.failErr(err)
			return
		}
		if err := s.s3CheckUpload(c, size, sum); err != nil {
			s.s3RemoveBlob(c.bucket, blob)
			c.failErr(err)
			return
		}
	}
	etag := hex.EncodeToString(sum)
	now := store.Now()
	var old string
	_ = s.st.DB.QueryRow(`SELECT blob FROM s3_parts WHERE upload_id=? AND part_number=?`, id, pn).Scan(&old)
	if _, err := s.st.DB.Exec(`INSERT INTO s3_parts(upload_id,part_number,size,etag,blob,modified) VALUES(?,?,?,?,?,?)
		ON CONFLICT(upload_id,part_number) DO UPDATE SET size=excluded.size, etag=excluded.etag, blob=excluded.blob, modified=excluded.modified`,
		id, pn, size, etag, blob, now); err != nil {
		s.s3RemoveBlob(c.bucket, blob)
		c.failErr(err)
		return
	}
	if old != "" && old != blob {
		s.s3RemoveBlob(c.bucket, old)
	}
	if copying {
		c.xml(http.StatusOK, struct {
			XMLName      xml.Name `xml:"CopyPartResult"`
			NS           string   `xml:"xmlns,attr"`
			LastModified string
			ETag         string
		}{NS: s3XMLNS, LastModified: s3Time(now), ETag: quoteETag(etag)})
		return
	}
	c.w.Header().Set("ETag", quoteETag(etag))
	c.w.WriteHeader(http.StatusOK)
}

type s3Part struct {
	Number int
	Size   int64
	ETag   string
	Blob   string
	Mod    int64
}

func (s *Server) s3Parts(id string) ([]s3Part, error) {
	rows, err := s.st.DB.Query(`SELECT part_number,size,etag,blob,modified FROM s3_parts WHERE upload_id=? ORDER BY part_number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []s3Part
	for rows.Next() {
		var p s3Part
		if err := rows.Scan(&p.Number, &p.Size, &p.ETag, &p.Blob, &p.Mod); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) s3CompleteUpload(c *s3Req) {
	id, ok := s.s3Upload(c)
	if !ok {
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	b, err := io.ReadAll(io.LimitReader(c.auth.Body(c.r, c.secret), 4<<20))
	if err != nil {
		c.failErr(err)
		return
	}
	if err := xml.Unmarshal(b, &req); err != nil || len(req.Parts) == 0 {
		c.fail(http.StatusBadRequest, "MalformedXML", "the XML you provided was not well-formed or did not list any parts")
		return
	}
	have, err := s.s3Parts(id)
	if err != nil {
		c.failErr(err)
		return
	}
	byNum := map[int]s3Part{}
	for _, p := range have {
		byNum[p.Number] = p
	}
	var use []s3Part
	prev := 0
	for i, rp := range req.Parts {
		if rp.PartNumber <= prev {
			c.fail(http.StatusBadRequest, "InvalidPartOrder", "the list of parts was not in ascending order")
			return
		}
		prev = rp.PartNumber
		p, ok := byNum[rp.PartNumber]
		if !ok || unquoteETag(rp.ETag) != p.ETag {
			c.fail(http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d was not found or its ETag does not match", rp.PartNumber))
			return
		}
		if i < len(req.Parts)-1 && p.Size < s3MinPart {
			c.fail(http.StatusBadRequest, "EntityTooSmall", fmt.Sprintf("part %d is smaller than the 5 MiB minimum", rp.PartNumber))
			return
		}
		use = append(use, p)
	}
	// Concatenate parts into the final blob; the multipart ETag is md5(md5s)-N.
	readers := make([]io.Reader, 0, len(use))
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	etagSum := md5.New()
	for _, p := range use {
		f, err := os.Open(s.s3BlobPath(c.bucket, p.Blob))
		if err != nil {
			c.fail(http.StatusInternalServerError, "InternalError", "part data is missing on disk")
			return
		}
		files = append(files, f)
		readers = append(readers, f)
		raw, _ := hex.DecodeString(p.ETag)
		etagSum.Write(raw)
	}
	blob, size, _, err := s.s3WriteBlob(c.bucket, io.MultiReader(readers...), s3MaxObject)
	if err != nil {
		c.failErr(err)
		return
	}
	var ct, meta, hdrs string
	_ = s.st.DB.QueryRow(`SELECT content_type,user_meta,headers FROM s3_uploads WHERE upload_id=?`, id).Scan(&ct, &meta, &hdrs)
	o := &s3Object{Key: c.key, Size: size, ETag: fmt.Sprintf("%s-%d", hex.EncodeToString(etagSum.Sum(nil)), len(use)),
		ContentType: ct, Blob: blob, Modified: store.Now()}
	_ = json.Unmarshal([]byte(meta), &o.Meta)
	_ = json.Unmarshal([]byte(hdrs), &o.Headers)
	if err := s.s3SetObject(c.bucket, o); err != nil {
		s.s3RemoveBlob(c.bucket, blob)
		c.failErr(err)
		return
	}
	s.s3DropUpload(c.bucket, id, have)
	c.xml(http.StatusOK, struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}{NS: s3XMLNS, Location: s.cfg.PublicURL + "/s3/" + c.bucket + "/" + s3sig.EscapePath(c.key), Bucket: c.bucket, Key: c.key, ETag: quoteETag(o.ETag)})
}

func (s *Server) s3DropUpload(bucket, id string, parts []s3Part) {
	_, _ = s.st.DB.Exec(`DELETE FROM s3_parts WHERE upload_id=?`, id)
	_, _ = s.st.DB.Exec(`DELETE FROM s3_uploads WHERE upload_id=?`, id)
	for _, p := range parts {
		s.s3RemoveBlob(bucket, p.Blob)
	}
}

func (s *Server) s3AbortUpload(c *s3Req) {
	id, ok := s.s3Upload(c)
	if !ok {
		return
	}
	parts, _ := s.s3Parts(id)
	s.s3DropUpload(c.bucket, id, parts)
	c.w.WriteHeader(http.StatusNoContent)
}

func (s *Server) s3ListParts(c *s3Req) {
	id, ok := s.s3Upload(c)
	if !ok {
		return
	}
	q := c.r.URL.Query()
	max := 1000
	if n, err := strconv.Atoi(q.Get("max-parts")); err == nil && n >= 0 {
		max = min(n, 1000)
	}
	marker, _ := strconv.Atoi(q.Get("part-number-marker"))
	parts, err := s.s3Parts(id)
	if err != nil {
		c.failErr(err)
		return
	}
	type part struct {
		PartNumber   int
		LastModified string
		ETag         string
		Size         int64
	}
	res := struct {
		XMLName              xml.Name `xml:"ListPartsResult"`
		NS                   string   `xml:"xmlns,attr"`
		Bucket               string
		Key                  string
		UploadId             string
		PartNumberMarker     int
		NextPartNumberMarker int
		MaxParts             int
		IsTruncated          bool
		StorageClass         string
		Parts                []part `xml:"Part"`
	}{NS: s3XMLNS, Bucket: c.bucket, Key: c.key, UploadId: id, PartNumberMarker: marker, MaxParts: max, StorageClass: "STANDARD"}
	for _, p := range parts {
		if p.Number <= marker {
			continue
		}
		if len(res.Parts) == max {
			res.IsTruncated = true
			break
		}
		res.Parts = append(res.Parts, part{p.Number, s3Time(p.Mod), quoteETag(p.ETag), p.Size})
		res.NextPartNumberMarker = p.Number
	}
	c.xml(http.StatusOK, res)
}

func (s *Server) s3ListUploads(c *s3Req) {
	prefix := c.r.URL.Query().Get("prefix")
	rows, err := s.st.DB.Query(`SELECT key, upload_id, created FROM s3_uploads WHERE bucket=? ORDER BY key, created LIMIT 1000`, c.bucket)
	if err != nil {
		c.failErr(err)
		return
	}
	type upload struct {
		Key          string
		UploadId     string
		Initiated    string
		StorageClass string
	}
	var ups []upload
	for rows.Next() {
		var u upload
		var t int64
		if rows.Scan(&u.Key, &u.UploadId, &t) == nil && strings.HasPrefix(u.Key, prefix) {
			u.Initiated, u.StorageClass = s3Time(t), "STANDARD"
			ups = append(ups, u)
		}
	}
	rows.Close()
	sort.SliceStable(ups, func(i, j int) bool { return ups[i].Key < ups[j].Key })
	c.xml(http.StatusOK, struct {
		XMLName     xml.Name `xml:"ListMultipartUploadsResult"`
		NS          string   `xml:"xmlns,attr"`
		Bucket      string
		Prefix      string
		MaxUploads  int
		IsTruncated bool
		Uploads     []upload `xml:"Upload"`
	}{NS: s3XMLNS, Bucket: c.bucket, Prefix: prefix, MaxUploads: 1000, Uploads: ups})
}
