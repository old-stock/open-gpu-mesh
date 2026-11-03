package server

// A minimal OCI Distribution (Docker Registry v2) server built into ogm, so members can push
// custom images straight to their cluster without a registry account:
//
//	ogm push my-image:dev            -> registry.ogm/{project}/my-image:dev
//
// Images named "registry.ogm/..." in a spec are pulled by agents through a localhost proxy
// (Docker allows plain HTTP to 127.0.0.0/8), authenticated with the node token.
//
// Storage: blobs are content-addressed files under {data}/registry/blobs; manifests and tags
// live in SQLite. Repositories are namespaced by project: "{project}/{name}".

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const registrySchema = `
CREATE TABLE IF NOT EXISTS registry_manifests (repo TEXT NOT NULL, digest TEXT NOT NULL, media_type TEXT NOT NULL,
  data BLOB NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(repo, digest));
CREATE TABLE IF NOT EXISTS registry_tags (repo TEXT NOT NULL, tag TEXT NOT NULL, digest TEXT NOT NULL,
  updated INTEGER NOT NULL, PRIMARY KEY(repo, tag));
`

var (
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	repoRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
)

type registryPrincipal struct {
	user *store.UserRow
	node *store.NodeRow
}

func (s *Server) registryDir(parts ...string) string {
	return filepath.Join(append([]string{s.regDir}, parts...)...)
}

func (s *Server) blobPath(digest string) string {
	hexd := strings.TrimPrefix(digest, "sha256:")
	return s.registryDir("blobs", hexd[:2], hexd)
}

func regErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// registryAuth accepts an API key or node token via Bearer, OGM-Api-Key, or Basic (password).
func (s *Server) registryAuth(r *http.Request) *registryPrincipal {
	key := apiKeyFrom(r)
	if key == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Basic ") {
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "Basic ")); err == nil {
				if _, pw, ok := strings.Cut(string(b), ":"); ok {
					key = pw
				}
			}
		}
	}
	if key == "" {
		return nil
	}
	if strings.HasPrefix(key, "ogmn_") {
		if n, err := s.st.NodeByToken(key); err == nil {
			return &registryPrincipal{node: n}
		}
		return nil
	}
	if u, err := s.st.UserByKey(key); err == nil {
		return &registryPrincipal{user: u}
	}
	return nil
}

// canAccess: nodes may pull any repository (they run whatever is scheduled on them);
// members may pull and push repositories of projects they belong to.
func (s *Server) canAccess(p *registryPrincipal, repo string, write bool) bool {
	if p.node != nil {
		return !write
	}
	project, _, _ := strings.Cut(repo, "/")
	var owner string
	if err := s.st.DB.QueryRow(`SELECT owner_id FROM projects WHERE name=?`, project).Scan(&owner); err != nil {
		return false
	}
	if p.user.Role == "admin" || owner == p.user.ID {
		return true
	}
	var n int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM project_members WHERE project=? AND user_id=?`, project, p.user.ID).Scan(&n)
	return n > 0
}

func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := s.registryAuth(r)
	if p == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="ogm"`)
		regErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "use your ogm API key as the password")
		return
	}
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v2"), "/")
	if path == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if path == "_catalog" {
		s.registryCatalog(w, r, p)
		return
	}
	// Split "<repo>/<kind>/<rest>" where kind ∈ blobs|manifests|tags.
	var repo, kind, rest string
	for _, k := range []string{"/blobs/uploads/", "/blobs/", "/manifests/", "/tags/"} {
		if i := strings.LastIndex(path, k); i > 0 {
			repo, kind, rest = path[:i], strings.Trim(k, "/"), path[i+len(k):]
			break
		}
	}
	if strings.HasSuffix(path, "/blobs/uploads") { // POST without trailing slash
		repo, kind = strings.TrimSuffix(path, "/blobs/uploads"), "blobs/uploads"
	}
	if repo == "" || !repoRe.MatchString(repo) {
		regErr(w, http.StatusNotFound, "NAME_INVALID", "repository must be {project}/{name}")
		return
	}
	write := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete
	if !s.canAccess(p, repo, write) {
		regErr(w, http.StatusForbidden, "DENIED", "no access to "+repo)
		return
	}
	switch kind {
	case "blobs":
		s.registryBlob(w, r, rest)
	case "blobs/uploads":
		s.registryUpload(w, r, repo, rest, p)
	case "manifests":
		s.registryManifest(w, r, repo, rest, p)
	case "tags":
		s.registryTags(w, repo)
	default:
		regErr(w, http.StatusNotFound, "UNSUPPORTED", "unsupported")
	}
}

func (s *Server) registryBlob(w http.ResponseWriter, r *http.Request, digest string) {
	if !digestRe.MatchString(digest) {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "bad digest")
		return
	}
	f, err := os.Open(s.blobPath(digest))
	if err != nil {
		regErr(w, http.StatusNotFound, "BLOB_UNKNOWN", digest)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeContent(w, r, "", st.ModTime(), f) // supports Range for resumed pulls
}

func (s *Server) registryUpload(w http.ResponseWriter, r *http.Request, repo, id string, p *registryPrincipal) {
	if p.node != nil {
		regErr(w, http.StatusForbidden, "DENIED", "nodes cannot push")
		return
	}
	upDir := s.registryDir("uploads")
	_ = os.MkdirAll(upDir, 0o755)
	switch {
	case r.Method == http.MethodPost && id == "":
		// Cross-repository mount: blobs are global, so it's a no-op if we have it.
		if m := r.URL.Query().Get("mount"); digestRe.MatchString(m) {
			if _, err := os.Stat(s.blobPath(m)); err == nil {
				w.Header().Set("Location", "/v2/"+repo+"/blobs/"+m)
				w.Header().Set("Docker-Content-Digest", m)
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
		id = store.NewSecret(16)
		f, err := os.Create(filepath.Join(upDir, id))
		if err != nil {
			regErr(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
		f.Close()
		// Monolithic upload: POST ?digest= with the body.
		if d := r.URL.Query().Get("digest"); d != "" {
			s.appendUpload(w, r, upDir, id)
			s.finishUpload(w, repo, upDir, id, d)
			return
		}
		w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
		w.Header().Set("Docker-Upload-UUID", id)
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)
	case !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id):
		regErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
	case r.Method == http.MethodPatch:
		n, ok := s.appendUpload(w, r, upDir, id)
		if !ok {
			return
		}
		w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
		w.Header().Set("Docker-Upload-UUID", id)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max(n-1, 0)))
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut:
		if _, ok := s.appendUpload(w, r, upDir, id); !ok {
			return
		}
		s.finishUpload(w, repo, upDir, id, r.URL.Query().Get("digest"))
	case r.Method == http.MethodGet:
		st, err := os.Stat(filepath.Join(upDir, id))
		if err != nil {
			regErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
			return
		}
		w.Header().Set("Docker-Upload-UUID", id)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max(st.Size()-1, 0)))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		_ = os.Remove(filepath.Join(upDir, id))
		w.WriteHeader(http.StatusNoContent)
	default:
		regErr(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// appendUpload appends the request body to an upload and returns its new size.
func (s *Server) appendUpload(w http.ResponseWriter, r *http.Request, dir, id string) (int64, bool) {
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		regErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
		return 0, false
	}
	defer f.Close()
	if _, err := io.Copy(f, r.Body); err != nil {
		regErr(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", err.Error())
		return 0, false
	}
	st, _ := f.Stat()
	return st.Size(), true
}

func (s *Server) finishUpload(w http.ResponseWriter, repo, dir, id, digest string) {
	tmp := filepath.Join(dir, id)
	defer os.Remove(tmp)
	if !digestRe.MatchString(digest) {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "digest query parameter required")
		return
	}
	f, err := os.Open(tmp)
	if err != nil {
		regErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
		return
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil || "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "content does not match digest")
		return
	}
	dst := s.blobPath(digest)
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.Rename(tmp, dst); err != nil {
		regErr(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	w.Header().Set("Location", "/v2/"+repo+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) registryManifest(w http.ResponseWriter, r *http.Request, repo, ref string, p *registryPrincipal) {
	if !digestRe.MatchString(ref) && !tagRe.MatchString(ref) {
		regErr(w, http.StatusBadRequest, "MANIFEST_INVALID", "bad reference")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		digest := ref
		if !digestRe.MatchString(ref) {
			if err := s.st.DB.QueryRow(`SELECT digest FROM registry_tags WHERE repo=? AND tag=?`, repo, ref).Scan(&digest); err != nil {
				regErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", repo+":"+ref)
				return
			}
		}
		var mt string
		var data []byte
		if err := s.st.DB.QueryRow(`SELECT media_type, data FROM registry_manifests WHERE repo=? AND digest=?`, repo, digest).Scan(&mt, &data); err != nil {
			regErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", repo+"@"+digest)
			return
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodPut:
		if p.node != nil {
			regErr(w, http.StatusForbidden, "DENIED", "nodes cannot push")
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			regErr(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		var m struct {
			MediaType string `json:"mediaType"`
			Config    *struct {
				Digest string `json:"digest"`
			} `json:"config"`
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
			Manifests []struct {
				Digest string `json:"digest"`
			} `json:"manifests"`
		}
		if json.Unmarshal(data, &m) != nil {
			regErr(w, http.StatusBadRequest, "MANIFEST_INVALID", "not JSON")
			return
		}
		// Every referenced blob/child manifest must already be present.
		var missing []string
		if m.Config != nil && !s.haveBlob(m.Config.Digest) {
			missing = append(missing, m.Config.Digest)
		}
		for _, l := range m.Layers {
			if !s.haveBlob(l.Digest) {
				missing = append(missing, l.Digest)
			}
		}
		for _, c := range m.Manifests {
			var x int
			_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM registry_manifests WHERE repo=? AND digest=?`, repo, c.Digest).Scan(&x)
			if x == 0 {
				missing = append(missing, c.Digest)
			}
		}
		if len(missing) > 0 {
			regErr(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "missing: "+strings.Join(missing, ", "))
			return
		}
		mt := r.Header.Get("Content-Type")
		if mt == "" {
			mt = m.MediaType
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if digestRe.MatchString(ref) && ref != digest {
			regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "manifest digest mismatch")
			return
		}
		now := store.Now()
		if _, err := s.st.DB.Exec(`INSERT OR REPLACE INTO registry_manifests(repo,digest,media_type,data,created) VALUES(?,?,?,?,?)`, repo, digest, mt, data, now); err != nil {
			regErr(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
		if !digestRe.MatchString(ref) {
			tagDigest := digest
			if r.Header.Get(mergeHeader) != "" {
				if merged, err := s.mergePlatforms(repo, ref, digest); err == nil {
					tagDigest = merged
				}
			}
			_, _ = s.st.DB.Exec(`INSERT OR REPLACE INTO registry_tags(repo,tag,digest,updated) VALUES(?,?,?,?)`, repo, ref, tagDigest, now)
			platMu.Lock()
			delete(platCache, repo+":"+ref)
			platMu.Unlock()
			project, _, _ := strings.Cut(repo, "/")
			s.st.AddEvent(project, "", "", "", "registry.push", fmt.Sprintf("Image pushed: registry.ogm/%s:%s by %s", repo, ref, p.user.Name))
		}
		w.Header().Set("Location", "/v2/"+repo+"/manifests/"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if p.node != nil {
			regErr(w, http.StatusForbidden, "DENIED", "nodes cannot delete")
			return
		}
		var res sql.Result
		if digestRe.MatchString(ref) {
			res, _ = s.st.DB.Exec(`DELETE FROM registry_manifests WHERE repo=? AND digest=?`, repo, ref)
			_, _ = s.st.DB.Exec(`DELETE FROM registry_tags WHERE repo=? AND digest=?`, repo, ref)
		} else {
			res, _ = s.st.DB.Exec(`DELETE FROM registry_tags WHERE repo=? AND tag=?`, repo, ref)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			regErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", ref)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		regErr(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (s *Server) haveBlob(d string) bool {
	if !digestRe.MatchString(d) {
		return false
	}
	_, err := os.Stat(s.blobPath(d))
	return err == nil
}

func (s *Server) registryTags(w http.ResponseWriter, repo string) {
	rows, err := s.st.DB.Query(`SELECT tag FROM registry_tags WHERE repo=? ORDER BY tag`, repo)
	if err != nil {
		regErr(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	tags := []string{}
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tags = append(tags, t)
		}
	}
	rows.Close()
	writeJSON(w, http.StatusOK, map[string]any{"name": repo, "tags": tags})
}

func (s *Server) registryCatalog(w http.ResponseWriter, r *http.Request, p *registryPrincipal) {
	rows, err := s.st.DB.Query(`SELECT DISTINCT repo FROM registry_tags ORDER BY repo`)
	if err != nil {
		regErr(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	var all []string
	for rows.Next() {
		var repo string
		if rows.Scan(&repo) == nil {
			all = append(all, repo)
		}
	}
	rows.Close()
	repos := []string{}
	for _, repo := range all {
		if s.canAccess(p, repo, false) {
			repos = append(repos, repo)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

// ---- platforms: multi-arch merging and arch-aware scheduling ----

const (
	mtOCIIndex    = "application/vnd.oci.image.index.v1+json"
	mtDockerList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	mergeHeader   = "OGM-Merge-Platforms"
	platformCache = 5 * time.Second
)

type imagePlatform struct {
	OS, Arch, Variant string
	Digest, MediaType string
	Size              int
}

func (p imagePlatform) key() string { return p.OS + "/" + p.Arch + "/" + p.Variant }

// platformsOf returns the runnable platforms of a manifest (index entries, or a single
// manifest's config os/architecture). Attestation entries ("unknown/unknown") are skipped.
func (s *Server) platformsOf(repo, digest string) ([]imagePlatform, error) {
	var mt string
	var data []byte
	if err := s.st.DB.QueryRow(`SELECT media_type, data FROM registry_manifests WHERE repo=? AND digest=?`, repo, digest).Scan(&mt, &data); err != nil {
		return nil, err
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Config    *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int    `json:"size"`
			Platform  *struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if mt == mtOCIIndex || mt == mtDockerList || len(m.Manifests) > 0 {
		var out []imagePlatform
		for _, e := range m.Manifests {
			if e.Platform == nil || e.Platform.OS == "unknown" || e.Platform.Architecture == "unknown" {
				continue
			}
			out = append(out, imagePlatform{e.Platform.OS, e.Platform.Architecture, e.Platform.Variant, e.Digest, e.MediaType, e.Size})
		}
		return out, nil
	}
	if m.Config == nil || !s.haveBlob(m.Config.Digest) {
		return nil, errors.New("manifest has no config")
	}
	b, err := os.ReadFile(s.blobPath(m.Config.Digest))
	if err != nil {
		return nil, err
	}
	var c struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	if err := json.Unmarshal(b, &c); err != nil || c.OS == "" {
		return nil, errors.New("config has no platform")
	}
	return []imagePlatform{{c.OS, c.Architecture, c.Variant, digest, mt, len(data)}}, nil
}

// mergePlatforms makes tag point at an index holding the new manifest's platforms plus any
// other platforms the tag already had. Used by `ogm push --multiarch` so x86 and ARM machines
// can each push their own build of the same image.
func (s *Server) mergePlatforms(repo, tag, newDigest string) (string, error) {
	var oldDigest string
	if err := s.st.DB.QueryRow(`SELECT digest FROM registry_tags WHERE repo=? AND tag=?`, repo, tag).Scan(&oldDigest); err != nil || oldDigest == newDigest {
		return newDigest, nil
	}
	newP, err := s.platformsOf(repo, newDigest)
	if err != nil || len(newP) == 0 {
		return newDigest, nil
	}
	oldP, _ := s.platformsOf(repo, oldDigest)
	seen := map[string]bool{}
	for _, p := range newP {
		seen[p.key()] = true
	}
	combined := append([]imagePlatform(nil), newP...)
	for _, p := range oldP {
		if !seen[p.key()] {
			combined = append(combined, p)
			seen[p.key()] = true
		}
	}
	if len(combined) == len(newP) {
		return newDigest, nil // nothing to keep from the old tag
	}
	type plat struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	}
	type entry struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
		Platform  plat   `json:"platform"`
	}
	idx := struct {
		SchemaVersion int     `json:"schemaVersion"`
		MediaType     string  `json:"mediaType"`
		Manifests     []entry `json:"manifests"`
	}{SchemaVersion: 2, MediaType: mtOCIIndex}
	for _, p := range combined {
		idx.Manifests = append(idx.Manifests, entry{p.MediaType, p.Digest, p.Size, plat{p.OS, p.Arch, p.Variant}})
	}
	data, _ := json.Marshal(idx)
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if _, err := s.st.DB.Exec(`INSERT OR REPLACE INTO registry_manifests(repo,digest,media_type,data,created) VALUES(?,?,?,?,?)`,
		repo, digest, mtOCIIndex, data, store.Now()); err != nil {
		return "", err
	}
	return digest, nil
}

type platformEntry struct {
	archs   map[string]bool // "linux/amd64"
	expires time.Time
}

var (
	platMu    sync.Mutex
	platCache = map[string]platformEntry{}
)

// imageArchs returns the os/arch set an image in the cluster registry supports, or nil if the
// image is external or unknown (no filtering).
func (s *Server) imageArchs(image string) map[string]bool {
	ref, ok := strings.CutPrefix(image, "registry.ogm/")
	if !ok {
		return nil
	}
	platMu.Lock()
	if e, ok := platCache[ref]; ok && time.Now().Before(e.expires) {
		platMu.Unlock()
		return e.archs
	}
	platMu.Unlock()
	repo, tag := ref, "latest"
	if i := strings.LastIndex(ref, "@"); i > 0 {
		repo, tag = ref[:i], ref[i+1:]
	} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	digest := tag
	if !digestRe.MatchString(tag) {
		if err := s.st.DB.QueryRow(`SELECT digest FROM registry_tags WHERE repo=? AND tag=?`, repo, tag).Scan(&digest); err != nil {
			digest = ""
		}
	}
	var archs map[string]bool
	if digest != "" {
		if ps, err := s.platformsOf(repo, digest); err == nil && len(ps) > 0 {
			archs = map[string]bool{}
			for _, p := range ps {
				archs[p.OS+"/"+p.Arch] = true
			}
		}
	}
	platMu.Lock()
	platCache[ref] = platformEntry{archs, time.Now().Add(platformCache)}
	platMu.Unlock()
	return archs
}
