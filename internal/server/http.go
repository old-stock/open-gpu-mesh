package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

type ctxKey int

const userKey ctxKey = 1

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Problem{Type: "https://ogm.dev/errors/" + slugTitle(title), Title: title, Status: status, Detail: detail})
}

func validationProblem(w http.ResponseWriter, errs map[string][]string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(api.Problem{Type: "https://ogm.dev/errors/validation", Title: "validation failed", Status: 400, Errors: errs})
}

func slugTitle(t string) string { return strings.ReplaceAll(strings.ToLower(t), " ", "_") }

func storeErr(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not found", what+" not found")
		return
	}
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
		problem(w, http.StatusConflict, "conflict", what+" already exists")
		return
	}
	problem(w, http.StatusInternalServerError, "internal error", err.Error())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		problem(w, http.StatusBadRequest, "bad request", err.Error())
		return false
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	if err := json.Unmarshal(body, v); err != nil {
		problem(w, http.StatusBadRequest, "invalid json", err.Error())
		return false
	}
	return true
}

func apiKeyFrom(r *http.Request) string {
	if k := r.Header.Get("OGM-Api-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}

// authed wraps a handler requiring a valid API key.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *store.UserRow)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := apiKeyFrom(r)
		if key == "" {
			problem(w, http.StatusUnauthorized, "unauthorized", "missing API key (OGM-Api-Key header or Bearer token)")
			return
		}
		u, sc, why := s.resolveCredential(key)
		if u == nil {
			problem(w, http.StatusUnauthorized, "unauthorized", why)
			return
		}
		if ok, why := scopeAllows(sc, r); !ok {
			problem(w, http.StatusForbidden, "forbidden", why)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		if sc != nil {
			ctx = context.WithValue(ctx, scopeCtxKey{}, sc)
		}
		h(w, r.WithContext(ctx), u)
	}
}

func (s *Server) admin(h func(http.ResponseWriter, *http.Request, *store.UserRow)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		if u.Role != "admin" {
			problem(w, http.StatusForbidden, "forbidden", "admin only")
			return
		}
		h(w, r, u)
	})
}

// projectAccess checks membership; admins can access everything.
func (s *Server) projectAccess(w http.ResponseWriter, u *store.UserRow, project string) bool {
	var owner string
	err := s.st.DB.QueryRow(`SELECT owner_id FROM projects WHERE name=?`, project).Scan(&owner)
	if err != nil {
		problem(w, http.StatusNotFound, "not found", "project "+project+" not found")
		return false
	}
	if u.Role == "admin" || owner == u.ID {
		return true
	}
	var n int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM project_members WHERE project=? AND user_id=?`, project, u.ID).Scan(&n)
	if n == 0 {
		problem(w, http.StatusForbidden, "forbidden", "not a member of project "+project)
		return false
	}
	return true
}

// projectManager allows project administration (members, deletion) only to the
// project's owner or a cluster admin, and never through a non-admin scoped key.
func (s *Server) projectManager(w http.ResponseWriter, r *http.Request, u *store.UserRow, project string) bool {
	if sc := scopeFrom(r.Context()); sc != nil && sc.Role != RoleAdmin {
		problem(w, http.StatusForbidden, "forbidden", "only the project owner or a cluster admin can do this")
		return false
	}
	var owner string
	_ = s.st.DB.QueryRow(`SELECT owner_id FROM projects WHERE name=?`, project).Scan(&owner)
	if u.Role == "admin" || owner == u.ID {
		return true
	}
	problem(w, http.StatusForbidden, "forbidden", "only the project owner or a cluster admin can do this")
	return false
}

// projected wraps handlers under /v1/projects/{project}/...
func (s *Server) projected(h func(http.ResponseWriter, *http.Request, *store.UserRow, string)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		p := r.PathValue("project")
		if !s.projectAccess(w, u, p) {
			return
		}
		h(w, r, u, p)
	})
}

// qseconds reads a duration query parameter given as whole seconds ("30") or a
// Go duration ("30s", "5m"), returning seconds.
func qseconds(r *http.Request, k string, def int) int {
	v := r.URL.Query().Get(k)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	if d, err := time.ParseDuration(v); err == nil {
		return int(d.Seconds())
	}
	return def
}

func qint(r *http.Request, k string, def int) int {
	if v := r.URL.Query().Get(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
