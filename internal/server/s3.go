package server

// S3-compatible object storage at /s3 (path-style). Credentials are per project; SigV4 is
// verified with internal/s3sig. Object bytes live in {DataDir}/s3/{bucket}/xx/{blob}, where
// blob is a random id, so object keys never touch the filesystem. See docs/s3.md.

import (
	"crypto/rand"
	"encoding/xml"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/s3sig"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const s3Schema = `
CREATE TABLE IF NOT EXISTS s3_credentials (access_key TEXT PRIMARY KEY, secret_sealed TEXT NOT NULL,
  user_id TEXT NOT NULL, project TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS s3_credentials_project ON s3_credentials(project);
CREATE TABLE IF NOT EXISTS s3_buckets (name TEXT PRIMARY KEY, project TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS s3_objects (bucket TEXT NOT NULL, key TEXT NOT NULL, size INTEGER NOT NULL,
  etag TEXT NOT NULL, content_type TEXT NOT NULL, user_meta TEXT NOT NULL DEFAULT '{}',
  headers TEXT NOT NULL DEFAULT '{}', blob TEXT NOT NULL, modified INTEGER NOT NULL, PRIMARY KEY(bucket, key));
CREATE TABLE IF NOT EXISTS s3_uploads (upload_id TEXT PRIMARY KEY, bucket TEXT NOT NULL, key TEXT NOT NULL,
  content_type TEXT NOT NULL, user_meta TEXT NOT NULL DEFAULT '{}', headers TEXT NOT NULL DEFAULT '{}', created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS s3_parts (upload_id TEXT NOT NULL, part_number INTEGER NOT NULL, size INTEGER NOT NULL,
  etag TEXT NOT NULL, blob TEXT NOT NULL, modified INTEGER NOT NULL, PRIMARY KEY(upload_id, part_number));`

const (
	s3Region      = "us-east-1"
	s3ClockSkew   = 15 * time.Minute
	s3MaxObject   = 5 << 40 // 5 TiB, S3's object limit
	s3MaxPart     = 5 << 30 // 5 GiB
	s3MinPart     = 5 << 20 // 5 MiB (all but the last part)
	s3MaxKeyBytes = 1024
	s3XMLNS       = "http://s3.amazonaws.com/doc/2006-03-01/"
)

func (s *Server) initS3() error {
	_, err := s.st.DB.Exec(s3Schema)
	return err
}

// s3Root is a sibling of the project files root, so S3 data never mixes with /files.
func (s *Server) s3Root() string {
	if s.cfg.DataDir == "" {
		return filepath.Join(os.TempDir(), "ogm-s3-"+s.jwtKid)
	}
	return filepath.Join(s.cfg.DataDir, "s3")
}

// ---------- credentials (normal API) ----------

// S3Credential is an S3 access key bound to a project.
type S3Credential struct {
	AccessKeyID     string     `json:"access_key_id"`
	SecretAccessKey string     `json:"secret_access_key,omitempty"`
	Endpoint        string     `json:"endpoint"`
	Region          string     `json:"region"`
	Project         string     `json:"project"`
	User            string     `json:"user,omitempty"`
	CreateTime      time.Time  `json:"create_time"`
	LastUsed        *time.Time `json:"last_used,omitempty"`
}

func randAlnum(n int, alphabet string) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, _ := rand.Int(rand.Reader, max)
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

const (
	upperAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	mixedAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

func (s *Server) registerS3Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{project}/s3-credentials", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		ak := "OGMS3" + randAlnum(15, upperAlnum)
		secret := randAlnum(40, mixedAlnum)
		sealed, err := s.sealSecret([]byte(secret))
		if err != nil {
			storeErr(w, err, "s3 credential")
			return
		}
		now := store.Now()
		if _, err := s.st.DB.Exec(`INSERT INTO s3_credentials(access_key,secret_sealed,user_id,project,created) VALUES(?,?,?,?,?)`,
			ak, sealed, u.ID, p, now); err != nil {
			storeErr(w, err, "s3 credential")
			return
		}
		s.st.AddEvent(p, "", "", "", "s3.credential.created", "S3 access key "+ak+" created by "+u.Name)
		writeJSON(w, http.StatusCreated, S3Credential{AccessKeyID: ak, SecretAccessKey: secret, Endpoint: s.cfg.PublicURL + "/s3",
			Region: s3Region, Project: p, User: u.Name, CreateTime: store.T(now)})
	}))
	mux.HandleFunc("GET /v1/projects/{project}/s3-credentials", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		rows, err := s.st.DB.Query(`SELECT c.access_key, COALESCE(u.name,''), c.created, c.last_used FROM s3_credentials c
			LEFT JOIN users u ON u.id=c.user_id WHERE c.project=? ORDER BY c.created`, p)
		if err != nil {
			storeErr(w, err, "s3 credentials")
			return
		}
		defer rows.Close()
		out := []S3Credential{}
		for rows.Next() {
			var c S3Credential
			var created, used int64
			if rows.Scan(&c.AccessKeyID, &c.User, &created, &used) == nil {
				c.Endpoint, c.Region, c.Project, c.CreateTime = s.cfg.PublicURL+"/s3", s3Region, p, store.T(created)
				if used > 0 {
					t := store.T(used)
					c.LastUsed = &t
				}
				out = append(out, c)
			}
		}
		writeJSON(w, http.StatusOK, api.List[S3Credential]{Items: out})
	}))
	mux.HandleFunc("DELETE /v1/projects/{project}/s3-credentials/{access_key}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		ak := r.PathValue("access_key")
		res, err := s.st.DB.Exec(`DELETE FROM s3_credentials WHERE project=? AND access_key=?`, p, ak)
		if err != nil {
			storeErr(w, err, "s3 credential")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			problem(w, http.StatusNotFound, "not found", "no such S3 access key in project "+p)
			return
		}
		s.st.AddEvent(p, "", "", "", "s3.credential.deleted", "S3 access key "+ak+" deleted by "+u.Name)
		w.WriteHeader(http.StatusNoContent)
	}))
}

// ---------- S3 protocol ----------

type s3Req struct {
	w       http.ResponseWriter
	r       *http.Request
	auth    *s3sig.Auth
	secret  string
	project string
	userID  string
	bucket  string
	key     string
}

type s3ErrorXML struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string `xml:",omitempty"`
	RequestID string `xml:"RequestId"`
}

func s3Fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Del("ETag")
	w.Header().Del("Content-Length")
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(s3ErrorXML{Code: code, Message: msg, Resource: r.URL.Path, RequestID: w.Header().Get("X-Amz-Request-Id")})
}

func (c *s3Req) fail(status int, code, msg string) { s3Fail(c.w, c.r, status, code, msg) }

// failErr maps s3sig errors (and body read errors) to S3 responses.
func (c *s3Req) failErr(err error) {
	var se *s3sig.Error
	if errors.As(err, &se) {
		c.fail(se.Status, se.Code, se.Message)
		return
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, http.ErrBodyReadAfterClose) {
		c.fail(http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	c.fail(http.StatusInternalServerError, "InternalError", err.Error())
}

func (c *s3Req) xml(status int, v any) {
	c.w.Header().Set("Content-Type", "application/xml")
	c.w.WriteHeader(status)
	_, _ = c.w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(c.w).Encode(v)
}

func (s *Server) handleS3(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Amz-Request-Id", strings.ToUpper(store.NewID("")))
	w.Header().Set("X-OGM-Version", api.Version)
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/s3"), "/")
	bucket, key, _ := strings.Cut(rest, "/")
	c := &s3Req{w: w, r: r, bucket: bucket, key: key}
	if !s.s3Authenticate(c) {
		return
	}
	s.s3Dispatch(c)
}

func (s *Server) s3Authenticate(c *s3Req) bool {
	a, err := s3sig.Parse(c.r)
	if err != nil {
		c.failErr(err)
		return false
	}
	var sealed string
	if err := s.st.DB.QueryRow(`SELECT secret_sealed, user_id, project FROM s3_credentials WHERE access_key=?`, a.AccessKey).
		Scan(&sealed, &c.userID, &c.project); err != nil {
		c.fail(http.StatusForbidden, "InvalidAccessKeyId", "the AWS access key ID you provided does not exist in our records")
		return false
	}
	secret, err := s.openSecret(sealed)
	if err != nil {
		c.fail(http.StatusInternalServerError, "InternalError", "credential can't be decrypted (secrets.key changed?)")
		return false
	}
	now := time.Now()
	if err := a.Verify(c.r, string(secret), now, s3ClockSkew); err != nil {
		c.failErr(err)
		return false
	}
	// The credential is only as good as its owner's current membership.
	u, err := s.st.UserByID(c.userID)
	if err != nil {
		c.fail(http.StatusForbidden, "InvalidAccessKeyId", "the credential's user no longer exists")
		return false
	}
	if u.Role != "admin" {
		var n int
		_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM projects p WHERE p.name=? AND (p.owner_id=? OR EXISTS
			(SELECT 1 FROM project_members m WHERE m.project=p.name AND m.user_id=?))`, c.project, u.ID, u.ID).Scan(&n)
		if n == 0 {
			c.fail(http.StatusForbidden, "AccessDenied", "the credential's user is no longer a member of project "+c.project)
			return false
		}
	}
	ms := store.Now()
	_, _ = s.st.DB.Exec(`UPDATE s3_credentials SET last_used=? WHERE access_key=? AND last_used<?`, ms, a.AccessKey, ms-60_000)
	c.auth, c.secret = a, string(secret)
	return true
}

func has(c *s3Req, sub string) bool {
	_, ok := c.r.URL.Query()[sub]
	return ok
}

// Bucket subresources we don't implement: answering NotImplemented (rather than treating the
// request as a listing) keeps clients from misreading the response.
var s3Unsupported = []string{"policy", "cors", "lifecycle", "tagging", "encryption", "website", "logging",
	"notification", "replication", "requestPayment", "accelerate", "analytics", "inventory", "metrics",
	"object-lock", "ownershipControls", "publicAccessBlock", "intelligent-tiering", "versions", "retention",
	"legal-hold", "torrent", "restore", "select", "attributes"}

func (s *Server) s3Dispatch(c *s3Req) {
	r := c.r
	if c.bucket == "" {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.s3ListBuckets(c)
			return
		}
		c.fail(http.StatusMethodNotAllowed, "MethodNotAllowed", "the specified method is not allowed against this resource")
		return
	}
	for _, sub := range s3Unsupported {
		if has(c, sub) {
			if r.Method == http.MethodGet && c.key == "" {
				switch sub {
				case "policy":
					c.fail(http.StatusNotFound, "NoSuchBucketPolicy", "the bucket policy does not exist")
					return
				case "cors":
					c.fail(http.StatusNotFound, "NoSuchCORSConfiguration", "the CORS configuration does not exist")
					return
				case "lifecycle":
					c.fail(http.StatusNotFound, "NoSuchLifecycleConfiguration", "the lifecycle configuration does not exist")
					return
				case "tagging":
					c.fail(http.StatusNotFound, "NoSuchTagSet", "the TagSet does not exist")
					return
				}
			}
			c.fail(http.StatusNotImplemented, "NotImplemented", "?"+sub+" is not supported by ogm's S3 API")
			return
		}
	}
	if c.key == "" {
		if r.Method == http.MethodPut && !has(c, "acl") {
			s.s3CreateBucket(c)
			return
		}
		if !s.s3BucketAccess(c, c.bucket) {
			return
		}
		switch r.Method {
		case http.MethodHead:
			c.w.Header().Set("X-Amz-Bucket-Region", s3Region)
			c.w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			s.s3DeleteBucket(c)
		case http.MethodPost:
			if has(c, "delete") {
				s.s3DeleteObjects(c)
				return
			}
			c.fail(http.StatusNotImplemented, "NotImplemented", "unsupported bucket POST")
		case http.MethodGet:
			switch {
			case has(c, "location"):
				c.xml(http.StatusOK, struct {
					XMLName xml.Name `xml:"LocationConstraint"`
					NS      string   `xml:"xmlns,attr"`
					Value   string   `xml:",chardata"`
				}{NS: s3XMLNS}) // empty = us-east-1
			case has(c, "versioning"):
				c.xml(http.StatusOK, struct {
					XMLName xml.Name `xml:"VersioningConfiguration"`
					NS      string   `xml:"xmlns,attr"`
				}{NS: s3XMLNS})
			case has(c, "acl"):
				s.s3ACL(c)
			case has(c, "uploads"):
				s.s3ListUploads(c)
			case r.URL.Query().Get("list-type") == "2":
				s.s3ListObjects(c, true)
			default:
				s.s3ListObjects(c, false)
			}
		case http.MethodPut:
			if has(c, "acl") {
				c.w.WriteHeader(http.StatusOK) // ACLs are project-wide; accept and ignore
				return
			}
			c.fail(http.StatusNotImplemented, "NotImplemented", "unsupported bucket PUT")
		default:
			c.fail(http.StatusMethodNotAllowed, "MethodNotAllowed", "the specified method is not allowed against this resource")
		}
		return
	}
	if !s.s3BucketAccess(c, c.bucket) {
		return
	}
	if len(c.key) > s3MaxKeyBytes {
		c.fail(http.StatusBadRequest, "KeyTooLongError", "your key is too long")
		return
	}
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		switch {
		case q.Get("uploadId") != "":
			s.s3UploadPart(c)
		case has(c, "acl"):
			c.w.WriteHeader(http.StatusOK)
		case r.Header.Get("X-Amz-Copy-Source") != "":
			s.s3CopyObject(c)
		default:
			s.s3PutObject(c)
		}
	case http.MethodGet, http.MethodHead:
		switch {
		case q.Get("uploadId") != "" && r.Method == http.MethodGet:
			s.s3ListParts(c)
		case has(c, "acl") && r.Method == http.MethodGet:
			s.s3ACL(c)
		default:
			s.s3GetObject(c)
		}
	case http.MethodDelete:
		if q.Get("uploadId") != "" {
			s.s3AbortUpload(c)
			return
		}
		s.s3DeleteObject(c)
	case http.MethodPost:
		switch {
		case has(c, "uploads"):
			s.s3CreateUpload(c)
		case q.Get("uploadId") != "":
			s.s3CompleteUpload(c)
		default:
			c.fail(http.StatusNotImplemented, "NotImplemented", "unsupported object POST")
		}
	default:
		c.fail(http.StatusMethodNotAllowed, "MethodNotAllowed", "the specified method is not allowed against this resource")
	}
}

// s3BucketAccess checks the bucket exists and belongs to the credential's project.
func (s *Server) s3BucketAccess(c *s3Req, bucket string) bool {
	var project string
	if err := s.st.DB.QueryRow(`SELECT project FROM s3_buckets WHERE name=?`, bucket).Scan(&project); err != nil {
		c.fail(http.StatusNotFound, "NoSuchBucket", "the specified bucket does not exist")
		return false
	}
	if project != c.project {
		c.fail(http.StatusForbidden, "AccessDenied", "bucket "+bucket+" belongs to another project")
		return false
	}
	return true
}

func (s *Server) s3ACL(c *s3Req) {
	type grantee struct {
		XSI  string `xml:"xmlns:xsi,attr"`
		Type string `xml:"xsi:type,attr"`
		ID   string
	}
	type grant struct {
		Grantee    grantee
		Permission string
	}
	c.xml(http.StatusOK, struct {
		XMLName xml.Name `xml:"AccessControlPolicy"`
		NS      string   `xml:"xmlns,attr"`
		Owner   struct{ ID, DisplayName string }
		Grants  []grant `xml:"AccessControlList>Grant"`
	}{NS: s3XMLNS, Owner: struct{ ID, DisplayName string }{c.project, c.project},
		Grants: []grant{{Grantee: grantee{XSI: "http://www.w3.org/2001/XMLSchema-instance", Type: "CanonicalUser", ID: c.project}, Permission: "FULL_CONTROL"}}})
}

// validBucketName applies S3's naming rules.
func validBucketName(n string) bool {
	if len(n) < 3 || len(n) > 63 || strings.Contains(n, "..") || strings.Contains(n, ".-") || strings.Contains(n, "-.") {
		return false
	}
	for i := 0; i < len(n); i++ {
		ch := n[i]
		ok := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '.'
		if !ok {
			return false
		}
	}
	isAlnum := func(ch byte) bool { return ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' }
	if !isAlnum(n[0]) || !isAlnum(n[len(n)-1]) {
		return false
	}
	if net.ParseIP(n) != nil || strings.HasPrefix(n, "xn--") || strings.HasSuffix(n, "-s3alias") {
		return false
	}
	return true
}
