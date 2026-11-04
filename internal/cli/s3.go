package cli

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/s3sig"
)

// s3Saved is the cached credential written by `ogm s3 credentials create --save`.
type s3Saved struct {
	Endpoint        string `json:"endpoint"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Project         string `json:"project,omitempty"`
}

type s3CredInfo struct {
	AccessKeyID     string     `json:"access_key_id"`
	SecretAccessKey string     `json:"secret_access_key,omitempty"`
	Endpoint        string     `json:"endpoint"`
	Region          string     `json:"region"`
	Project         string     `json:"project"`
	User            string     `json:"user,omitempty"`
	CreateTime      time.Time  `json:"create_time"`
	LastUsed        *time.Time `json:"last_used,omitempty"`
}

func s3ConfigPath() string { return filepath.Join(filepath.Dir(configPath()), "s3.json") }

const s3Usage = `s3 credentials create [--save] | credentials ls | credentials rm ACCESS_KEY
       ogm s3 mb BUCKET | rb BUCKET
       ogm s3 ls [BUCKET[/PREFIX]] [--recursive]
       ogm s3 cp LOCAL s3://BUCKET/KEY | cp s3://BUCKET/KEY LOCAL|-
       ogm s3 rm s3://BUCKET/KEY
       ogm s3 presign s3://BUCKET/KEY [--expires 1h]`

// cmdS3 manages S3 credentials and talks to the hub's S3-compatible API (see docs/s3.md).
func cmdS3(e *env, args []string) {
	need(args, 1, s3Usage)
	switch args[0] {
	case "credentials", "creds", "credential":
		cmdS3Creds(e, args[1:])
		return
	}
	s3c := e.s3()
	switch args[0] {
	case "mb":
		need(args, 2, "s3 mb BUCKET")
		b := strings.TrimSuffix(strings.TrimPrefix(args[1], "s3://"), "/")
		check(s3c.do(e.ctx, "PUT", b, "", nil, nil, -1, nil))
		fmt.Fprintf(e.out, "make_bucket: %s\n", b)
	case "rb":
		need(args, 2, "s3 rb BUCKET")
		b := strings.TrimSuffix(strings.TrimPrefix(args[1], "s3://"), "/")
		check(s3c.do(e.ctx, "DELETE", b, "", nil, nil, -1, nil))
		fmt.Fprintf(e.out, "remove_bucket: %s\n", b)
	case "ls", "list":
		fs := flag.NewFlagSet("s3 ls", flag.ExitOnError)
		rec := fs.Bool("recursive", false, "list all keys under the prefix (no folder grouping)")
		fs.BoolVar(rec, "r", false, "shorthand for --recursive")
		pos := parse(fs, args[1:])
		if len(pos) == 0 {
			s3ListBuckets(e, s3c)
			return
		}
		b, prefix := splitS3(pos[0])
		s3ListKeys(e, s3c, b, prefix, *rec)
	case "cp":
		need(args, 3, "s3 cp SRC DST   (one side is s3://BUCKET/KEY)")
		src, dst := args[1], args[2]
		switch {
		case strings.HasPrefix(src, "s3://") && !strings.HasPrefix(dst, "s3://"):
			b, k := splitS3(src)
			if k == "" {
				fatalf("source must be s3://BUCKET/KEY")
			}
			s3Download(e, s3c, b, k, dst)
		case !strings.HasPrefix(src, "s3://") && strings.HasPrefix(dst, "s3://"):
			b, k := splitS3(dst)
			if k == "" || strings.HasSuffix(k, "/") {
				k += filepath.Base(src)
			}
			s3Upload(e, s3c, src, b, k)
		default:
			fatalf("cp copies between a local path and s3://BUCKET/KEY")
		}
	case "rm":
		need(args, 2, "s3 rm s3://BUCKET/KEY")
		b, k := splitS3(args[1])
		if k == "" {
			fatalf("rm needs s3://BUCKET/KEY (use `ogm s3 rb` for buckets)")
		}
		check(s3c.do(e.ctx, "DELETE", b, k, nil, nil, -1, nil))
		fmt.Fprintf(e.out, "delete: s3://%s/%s\n", b, k)
	case "presign":
		fs := flag.NewFlagSet("s3 presign", flag.ExitOnError)
		exp := fs.Duration("expires", time.Hour, "URL lifetime (max 168h)")
		pos := parse(fs, args[1:])
		need(pos, 1, "s3 presign s3://BUCKET/KEY [--expires 1h]")
		b, k := splitS3(pos[0])
		req, _ := http.NewRequest("GET", s3c.url(b, k, nil), nil)
		fmt.Fprintln(e.out, s3sig.Presign(req, s3c.cred, "us-east-1", *exp, time.Now()))
	default:
		fatalf("usage: ogm %s", s3Usage)
	}
}

func cmdS3Creds(e *env, args []string) {
	c := e.client()
	base := "/v1/projects/" + url.PathEscape(e.project) + "/s3-credentials"
	if len(args) == 0 {
		args = []string{"ls"}
	}
	switch args[0] {
	case "create", "new":
		fs := flag.NewFlagSet("s3 credentials create", flag.ExitOnError)
		save := fs.Bool("save", false, "cache the credential in "+s3ConfigPath()+" for `ogm s3` commands")
		parse(fs, args[1:])
		var cr s3CredInfo
		check(c.Do(e.ctx, "POST", base, nil, &cr))
		if *save {
			b, _ := json.MarshalIndent(s3Saved{Endpoint: cr.Endpoint, AccessKeyID: cr.AccessKeyID, SecretAccessKey: cr.SecretAccessKey, Project: cr.Project}, "", "  ")
			check(os.MkdirAll(filepath.Dir(s3ConfigPath()), 0o700))
			check(os.WriteFile(s3ConfigPath(), b, 0o600))
		}
		if e.output == "json" {
			e.json(cr)
			return
		}
		fmt.Fprintf(e.out, "endpoint:          %s\naccess_key_id:     %s\nsecret_access_key: %s\nregion:            %s (any region works)\n",
			cr.Endpoint, cr.AccessKeyID, cr.SecretAccessKey, cr.Region)
		if *save {
			fmt.Fprintf(e.out, "\nsaved to %s\n", s3ConfigPath())
		}
		fmt.Fprintln(e.out, "\nThe secret is shown only once. Use path-style addressing, e.g. boto3:")
		fmt.Fprintf(e.out, "  boto3.client('s3', endpoint_url='%s', aws_access_key_id='%s', aws_secret_access_key='...')\n", cr.Endpoint, cr.AccessKeyID)
	case "ls", "list":
		var out api.List[s3CredInfo]
		check(c.Do(e.ctx, "GET", base, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, cr := range out.Items {
			used := "never"
			if cr.LastUsed != nil {
				used = ago(cr.LastUsed) + " ago"
			}
			rows = append(rows, []string{cr.AccessKeyID, orDash(cr.User), cr.CreateTime.Local().Format(time.DateTime), used})
		}
		e.table("ACCESS KEY\tUSER\tCREATED\tLAST USED", rows)
	case "rm", "delete", "revoke":
		need(args, 2, "s3 credentials rm ACCESS_KEY")
		check(c.Do(e.ctx, "DELETE", base+"/"+url.PathEscape(args[1]), nil, nil))
		fmt.Fprintf(e.out, "S3 access key %s deleted\n", args[1])
	default:
		fatalf("usage: ogm s3 credentials create [--save] | ls | rm ACCESS_KEY")
	}
}

func splitS3(s string) (bucket, key string) {
	s = strings.TrimPrefix(s, "s3://")
	bucket, key, _ = strings.Cut(s, "/")
	return bucket, key
}

// ---------- minimal SigV4 client ----------

type s3Client struct {
	endpoint string
	cred     s3sig.Credentials
	http     *http.Client
}

func (e *env) s3() *s3Client {
	var saved s3Saved
	if b, err := os.ReadFile(s3ConfigPath()); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	c := &s3Client{endpoint: saved.Endpoint, cred: s3sig.Credentials{AccessKey: saved.AccessKeyID, SecretKey: saved.SecretAccessKey}, http: &http.Client{}}
	if ak, sk := os.Getenv("OGM_S3_ACCESS_KEY"), os.Getenv("OGM_S3_SECRET_KEY"); ak != "" && sk != "" {
		c.cred = s3sig.Credentials{AccessKey: ak, SecretKey: sk}
	}
	if v := os.Getenv("OGM_S3_ENDPOINT"); v != "" {
		c.endpoint = v
	}
	if c.endpoint == "" && e.cfg.Server != "" {
		c.endpoint = strings.TrimRight(e.cfg.Server, "/") + "/s3"
	}
	if c.cred.AccessKey == "" || c.endpoint == "" {
		fatalf("no S3 credential: run `ogm s3 credentials create --save` or set OGM_S3_ACCESS_KEY / OGM_S3_SECRET_KEY")
	}
	c.endpoint = strings.TrimRight(c.endpoint, "/")
	return c
}

func (c *s3Client) url(bucket, key string, q url.Values) string {
	u := c.endpoint + "/"
	if bucket != "" {
		u += bucket
		if key != "" {
			u += "/" + s3sig.EscapePath(key)
		}
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

type s3Error struct {
	Code    string
	Message string
}

// do sends a signed request. body may be nil (size -1); out receives the response: an
// io.Writer gets the raw body, anything else is XML-decoded.
func (c *s3Client) do(ctx context.Context, method, bucket, key string, q url.Values, body io.Reader, size int64, out any, hdr ...string) error {
	_, err := c.doResp(ctx, method, bucket, key, q, body, size, out, hdr...)
	return err
}

func (c *s3Client) doResp(ctx context.Context, method, bucket, key string, q url.Values, body io.Reader, size int64, out any, hdr ...string) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(bucket, key, q), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	payload := s3sig.EmptySHA256
	if body != nil {
		payload = s3sig.UnsignedPayload
	}
	s3sig.Sign(req, c.cred, "us-east-1", payload, time.Now())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var se s3Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if xml.Unmarshal(b, &se) == nil && se.Code != "" {
			return nil, fmt.Errorf("%s: %s", se.Code, se.Message)
		}
		return nil, fmt.Errorf("%s %s: %s", method, req.URL.Path, resp.Status)
	}
	switch o := out.(type) {
	case nil:
		_, _ = io.Copy(io.Discard, resp.Body)
	case io.Writer:
		_, err = io.Copy(o, resp.Body)
	default:
		err = xml.NewDecoder(resp.Body).Decode(out)
	}
	return resp.Header, err
}

func s3Size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func s3ListBuckets(e *env, c *s3Client) {
	var res struct {
		Buckets []struct {
			Name         string
			CreationDate string
		} `xml:"Buckets>Bucket"`
	}
	check(c.do(e.ctx, "GET", "", "", nil, nil, -1, &res))
	if e.output == "json" {
		e.json(res.Buckets)
		return
	}
	var rows [][]string
	for _, b := range res.Buckets {
		rows = append(rows, []string{b.Name, b.CreationDate})
	}
	e.table("BUCKET\tCREATED", rows)
}

func s3ListKeys(e *env, c *s3Client, bucket, prefix string, recursive bool) {
	type entry struct {
		Key          string `json:"key"`
		Size         int64  `json:"size"`
		LastModified string `json:"last_modified"`
		ETag         string `json:"etag"`
	}
	var entries []entry
	var dirs []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if !recursive {
			q.Set("delimiter", "/")
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		var res struct {
			IsTruncated           bool
			NextContinuationToken string
			Contents              []entry
			CommonPrefixes        []struct{ Prefix string }
		}
		check(c.do(e.ctx, "GET", bucket, "", q, nil, -1, &res))
		entries = append(entries, res.Contents...)
		for _, p := range res.CommonPrefixes {
			dirs = append(dirs, p.Prefix)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
	}
	if e.output == "json" {
		e.json(map[string]any{"prefixes": dirs, "objects": entries})
		return
	}
	var rows [][]string
	for _, d := range dirs {
		rows = append(rows, []string{"", "PRE", d})
	}
	for _, o := range entries {
		t, _ := time.Parse(time.RFC3339, o.LastModified)
		rows = append(rows, []string{t.Local().Format(time.DateTime), s3Size(o.Size), o.Key})
	}
	e.table("MODIFIED\tSIZE\tKEY", rows)
}

const s3PartSize = 64 << 20

func s3Upload(e *env, c *s3Client, local, bucket, key string) {
	f, err := os.Open(local)
	check(err)
	defer f.Close()
	st, err := f.Stat()
	check(err)
	ct := mime.TypeByExtension(path.Ext(local))
	if ct == "" {
		ct = "application/octet-stream"
	}
	if st.Size() <= 2*s3PartSize {
		h := md5.New()
		_, err := io.Copy(h, f)
		check(err)
		_, err = f.Seek(0, io.SeekStart)
		check(err)
		check(c.do(e.ctx, "PUT", bucket, key, nil, f, st.Size(), nil,
			"Content-Type", ct, "Content-MD5", base64.StdEncoding.EncodeToString(h.Sum(nil))))
		fmt.Fprintf(e.out, "upload: %s to s3://%s/%s (%s)\n", local, bucket, key, s3Size(st.Size()))
		return
	}
	var init struct{ UploadId string }
	check(c.do(e.ctx, "POST", bucket, key, url.Values{"uploads": {""}}, nil, -1, &init, "Content-Type", ct))
	type part struct {
		PartNumber int
		ETag       string
	}
	var parts []part
	abort := func(err error) {
		_ = c.do(context.Background(), "DELETE", bucket, key, url.Values{"uploadId": {init.UploadId}}, nil, -1, nil)
		check(err)
	}
	for n, off := 1, int64(0); off < st.Size(); n, off = n+1, off+s3PartSize {
		sz := min(s3PartSize, st.Size()-off)
		h, err := c.doResp(e.ctx, "PUT", bucket, key, url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {init.UploadId}},
			io.NewSectionReader(f, off, sz), sz, nil)
		if err != nil {
			abort(err)
		}
		parts = append(parts, part{n, h.Get("ETag")})
		fmt.Fprintf(os.Stderr, "\rupload: %s / %s", s3Size(off+sz), s3Size(st.Size()))
	}
	fmt.Fprintln(os.Stderr)
	b, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{Parts: parts})
	if err := c.do(e.ctx, "POST", bucket, key, url.Values{"uploadId": {init.UploadId}}, strings.NewReader(string(b)), int64(len(b)), &struct{}{}); err != nil {
		abort(err)
	}
	fmt.Fprintf(e.out, "upload: %s to s3://%s/%s (%s, %d parts)\n", local, bucket, key, s3Size(st.Size()), len(parts))
}

func s3Download(e *env, c *s3Client, bucket, key, local string) {
	if local == "-" {
		check(c.do(e.ctx, "GET", bucket, key, nil, nil, -1, os.Stdout))
		return
	}
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		local = filepath.Join(local, path.Base(key))
	}
	tmp := local + ".part"
	f, err := os.Create(tmp)
	check(err)
	if err := c.do(e.ctx, "GET", bucket, key, nil, nil, -1, f); err != nil {
		f.Close()
		os.Remove(tmp)
		check(err)
	}
	check(f.Close())
	check(os.Rename(tmp, local))
	st, _ := os.Stat(local)
	fmt.Fprintf(e.out, "download: s3://%s/%s to %s (%s)\n", bucket, key, local, s3Size(st.Size()))
}
