package server

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// KV is a small per-project key-value store (think DynamoDB-lite / Redis-lite over HTTP):
// tables of keys with byte values up to 1 MiB, versions for compare-and-set, TTLs and
// atomic counters. It lives in the hub's SQLite, so it survives any node going away.

const kvSchema = `CREATE TABLE IF NOT EXISTS kv(
  project TEXT NOT NULL, tbl TEXT NOT NULL, k TEXT NOT NULL, v BLOB NOT NULL, content_type TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL, expires INTEGER NOT NULL DEFAULT 0, updated INTEGER NOT NULL,
  PRIMARY KEY(project, tbl, k));
CREATE INDEX IF NOT EXISTS kv_expires ON kv(expires) WHERE expires > 0;`

const kvMaxValue = 1 << 20

var kvTableRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// KVItem describes a stored key (value omitted in listings unless ?values=1).
type KVItem struct {
	Key        string     `json:"key"`
	Version    int64      `json:"version"`
	Size       int        `json:"size"`
	Value      *string    `json:"value,omitempty"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`
	UpdateTime time.Time  `json:"update_time"`
}

// KVTable summarizes a table.
type KVTable struct {
	Name  string `json:"name"`
	Keys  int    `json:"keys"`
	Bytes int64  `json:"bytes"`
}

func (s *Server) initKV() error {
	_, err := s.st.DB.Exec(kvSchema)
	return err
}

func (s *Server) kvGC() {
	_, _ = s.st.DB.Exec(`DELETE FROM kv WHERE expires > 0 AND expires < ?`, store.Now())
}

func kvKeyOK(k string) bool { return k != "" && len(k) <= 1024 && !strings.ContainsRune(k, 0) }

func (s *Server) registerKVRoutes(mux *http.ServeMux) {
	base := "/v1/projects/{project}/kv"
	live := `(expires = 0 OR expires > ?)`

	mux.HandleFunc("GET "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		rows, err := s.st.DB.Query(`SELECT tbl, COUNT(*), COALESCE(SUM(LENGTH(v)),0) FROM kv WHERE project=? AND `+live+` GROUP BY tbl ORDER BY tbl`, p, store.Now())
		if err != nil {
			storeErr(w, err, "kv")
			return
		}
		defer rows.Close()
		out := []KVTable{}
		for rows.Next() {
			var t KVTable
			if rows.Scan(&t.Name, &t.Keys, &t.Bytes) == nil {
				out = append(out, t)
			}
		}
		writeJSON(w, http.StatusOK, api.List[KVTable]{Items: out})
	}))

	mux.HandleFunc("GET "+base+"/{table}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		tbl := r.PathValue("table")
		q := r.URL.Query()
		limit := qint(r, "limit", 100)
		if limit < 1 || limit > 1000 {
			limit = 1000
		}
		prefix := q.Get("prefix")
		after := q.Get("after")
		withValues := q.Get("values") == "1"
		rows, err := s.st.DB.Query(`SELECT k, version, LENGTH(v), v, expires, updated FROM kv
			WHERE project=? AND tbl=? AND `+live+` AND substr(k,1,?)=? AND k > ? ORDER BY k LIMIT ?`,
			p, tbl, store.Now(), len(prefix), prefix, after, limit+1)
		if err != nil {
			storeErr(w, err, "kv")
			return
		}
		defer rows.Close()
		out := []KVItem{}
		for rows.Next() {
			var it KVItem
			var v []byte
			var exp, upd int64
			if rows.Scan(&it.Key, &it.Version, &it.Size, &v, &exp, &upd) != nil {
				continue
			}
			it.UpdateTime = store.T(upd)
			if exp > 0 {
				t := store.T(exp)
				it.ExpireTime = &t
			}
			if withValues {
				sv := string(v)
				it.Value = &sv
			}
			out = append(out, it)
		}
		next := ""
		if len(out) > limit {
			out = out[:limit]
			next = out[limit-1].Key
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_after": next})
	}))

	mux.HandleFunc("GET "+base+"/{table}/{key...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var v []byte
		var ct string
		var ver, exp int64
		err := s.st.DB.QueryRow(`SELECT v, content_type, version, expires FROM kv WHERE project=? AND tbl=? AND k=? AND `+live,
			p, r.PathValue("table"), r.PathValue("key"), store.Now()).Scan(&v, &ct, &ver, &exp)
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, http.StatusNotFound, "not found", "no such key")
			return
		} else if err != nil {
			storeErr(w, err, "kv")
			return
		}
		etag := `"` + strconv.FormatInt(ver, 10) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("OGM-Version", strconv.FormatInt(ver, 10))
		if exp > 0 {
			w.Header().Set("OGM-Expires", store.T(exp).Format(time.RFC3339))
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.Itoa(len(v)))
		_, _ = w.Write(v)
	}))

	mux.HandleFunc("PUT "+base+"/{table}/{key...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		tbl, key := r.PathValue("table"), r.PathValue("key")
		if !kvTableRe.MatchString(tbl) || !kvKeyOK(key) {
			problem(w, http.StatusBadRequest, "invalid key", "table: 1-64 of [A-Za-z0-9_.-]; key: 1-1024 bytes")
			return
		}
		v, err := io.ReadAll(io.LimitReader(r.Body, kvMaxValue+1))
		if err != nil {
			problem(w, http.StatusBadRequest, "read failed", err.Error())
			return
		}
		if len(v) > kvMaxValue {
			problem(w, http.StatusRequestEntityTooLarge, "value too large", "values are limited to 1 MiB; use the file store or S3 for larger objects")
			return
		}
		var exp int64
		if t := qseconds(r, "ttl", 0); t > 0 {
			exp = store.Now() + int64(t)*1000
		}
		ver, status, msg := s.kvWrite(p, tbl, key, v, r.Header.Get("Content-Type"), exp, r.Header.Get("If-Match"), r.Header.Get("If-None-Match"))
		if status != 0 {
			problem(w, status, "precondition failed", msg)
			return
		}
		w.Header().Set("ETag", `"`+strconv.FormatInt(ver, 10)+`"`)
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "version": ver})
	}))

	mux.HandleFunc("POST "+base+"/{table}/{key...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		tbl, key := r.PathValue("table"), r.PathValue("key")
		if !kvTableRe.MatchString(tbl) || !kvKeyOK(key) {
			problem(w, http.StatusBadRequest, "invalid key", "bad table or key")
			return
		}
		by := int64(qint(r, "by", 1))
		s.mu.Lock()
		defer s.mu.Unlock()
		var cur []byte
		var exp int64
		err := s.st.DB.QueryRow(`SELECT v, expires FROM kv WHERE project=? AND tbl=? AND k=? AND `+live, p, tbl, key, store.Now()).Scan(&cur, &exp)
		n := int64(0)
		if err == nil {
			n, err = strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64)
			if err != nil {
				problem(w, http.StatusConflict, "not a counter", "the current value is not an integer")
				return
			}
		}
		n += by
		if t := qseconds(r, "ttl", 0); t > 0 {
			exp = store.Now() + int64(t)*1000
		}
		ver, _, _ := s.kvWriteLocked(p, tbl, key, []byte(strconv.FormatInt(n, 10)), "text/plain", exp, "", "")
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": n, "version": ver})
	}))

	mux.HandleFunc("DELETE "+base+"/{table}/{key...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, args := `DELETE FROM kv WHERE project=? AND tbl=? AND k=?`, []any{p, r.PathValue("table"), r.PathValue("key")}
		if m := strings.Trim(r.Header.Get("If-Match"), `"`); m != "" {
			q += ` AND version=?`
			args = append(args, m)
		}
		res, err := s.st.DB.Exec(q, args...)
		if err != nil {
			storeErr(w, err, "kv")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if r.Header.Get("If-Match") != "" {
				problem(w, http.StatusPreconditionFailed, "precondition failed", "version does not match")
				return
			}
			problem(w, http.StatusNotFound, "not found", "no such key")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("DELETE "+base+"/{table}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		res, err := s.st.DB.Exec(`DELETE FROM kv WHERE project=? AND tbl=?`, p, r.PathValue("table"))
		if err != nil {
			storeErr(w, err, "kv")
			return
		}
		n, _ := res.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	}))
}

func (s *Server) kvWrite(p, tbl, key string, v []byte, ct string, exp int64, ifMatch, ifNoneMatch string) (int64, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kvWriteLocked(p, tbl, key, v, ct, exp, ifMatch, ifNoneMatch)
}

// kvWriteLocked upserts with optional preconditions: If-Match "<version>" (compare-and-set)
// or If-None-Match * (create only). Returns the new version or an HTTP status + message.
func (s *Server) kvWriteLocked(p, tbl, key string, v []byte, ct string, exp int64, ifMatch, ifNoneMatch string) (int64, int, string) {
	now := store.Now()
	var cur int64
	err := s.st.DB.QueryRow(`SELECT version FROM kv WHERE project=? AND tbl=? AND k=? AND (expires = 0 OR expires > ?)`, p, tbl, key, now).Scan(&cur)
	exists := err == nil
	if ifNoneMatch == "*" && exists {
		return 0, http.StatusPreconditionFailed, "key already exists"
	}
	if m := strings.Trim(ifMatch, `"`); m != "" {
		want, _ := strconv.ParseInt(m, 10, 64)
		if !exists || cur != want {
			return 0, http.StatusPreconditionFailed, "version does not match (current " + strconv.FormatInt(cur, 10) + ")"
		}
	}
	ver := cur + 1
	if _, err := s.st.DB.Exec(`INSERT INTO kv(project,tbl,k,v,content_type,version,expires,updated) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(project,tbl,k) DO UPDATE SET v=excluded.v, content_type=excluded.content_type, version=excluded.version,
		expires=excluded.expires, updated=excluded.updated`, p, tbl, key, v, ct, ver, exp, now); err != nil {
		return 0, http.StatusInternalServerError, err.Error()
	}
	return ver, 0, ""
}
