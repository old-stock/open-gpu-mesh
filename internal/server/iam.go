package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
	"github.com/old-stock/open-gpu-mesh/internal/webhook"
)

// IAM-lite: besides each user's personal key, users can mint scoped API keys (one project,
// a role, an expiry), and workloads authenticate with their instance identity token (the
// IMDS JWT) — the equivalent of an IAM role, so no long-lived key is baked into images.

const iamSchema = `CREATE TABLE IF NOT EXISTS api_keys(
  id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, user_id TEXT NOT NULL, name TEXT NOT NULL,
  project TEXT NOT NULL DEFAULT '', role TEXT NOT NULL, expires INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0);`

// Key roles.
const (
	RoleAdmin     = "admin"     // everything the owning user can do
	RoleDeveloper = "developer" // member-level access, never admin endpoints
	RoleViewer    = "viewer"    // read-only (GET/HEAD)
	RoleInstance  = "instance"  // workload identity: data-plane APIs of its own project
)

// keyScope restricts what a request may do. nil = the user's personal key (no restriction).
type keyScope struct {
	KeyID   string
	Project string // "" = all projects the user can access
	Role    string
	Group   string // instance tokens: the calling container group
}

type scopeCtxKey struct{}

func scopeFrom(ctx context.Context) *keyScope {
	sc, _ := ctx.Value(scopeCtxKey{}).(*keyScope)
	return sc
}

func (s *Server) initIAM() error {
	_, err := s.st.DB.Exec(iamSchema)
	return err
}

// resolveCredential maps a bearer credential to a user and scope.
func (s *Server) resolveCredential(key string) (*store.UserRow, *keyScope, string) {
	if u, err := s.st.UserByKey(key); err == nil {
		return u, nil, ""
	}
	if strings.HasPrefix(key, "ogmk_") {
		var id, uid, project, role string
		var expires int64
		err := s.st.DB.QueryRow(`SELECT id,user_id,project,role,expires FROM api_keys WHERE key_hash=?`, store.Hash(key)).
			Scan(&id, &uid, &project, &role, &expires)
		if err != nil {
			return nil, nil, "invalid API key"
		}
		if expires > 0 && store.Now() > expires {
			return nil, nil, "API key expired"
		}
		u, err := s.st.UserByID(uid)
		if err != nil {
			return nil, nil, "API key owner no longer exists"
		}
		_, _ = s.st.DB.Exec(`UPDATE api_keys SET last_used=? WHERE id=?`, store.Now(), id)
		if role != RoleAdmin || u.Role != "admin" {
			cp := *u
			cp.Role = "member"
			u = &cp
		}
		return u, &keyScope{KeyID: id, Project: project, Role: role}, ""
	}
	if strings.Count(key, ".") == 2 {
		claims, err := webhook.VerifyJWT(s.jwtPub, key)
		if err != nil {
			return nil, nil, "invalid token: " + err.Error()
		}
		if claims["aud"] != jwtAudHub {
			return nil, nil, "token audience is not " + jwtAudHub + " (fetch it with $OGM_IMDS_URL/v1/token?audience=" + jwtAudHub + ")"
		}
		inst, _ := claims["sub"].(string)
		project, _ := claims["project"].(string)
		in, err := s.st.Instance(inst)
		if err != nil || in.State == api.InstTerminated || in.Project != project {
			return nil, nil, "instance token no longer valid"
		}
		var owner string
		if err := s.st.DB.QueryRow(`SELECT owner_id FROM projects WHERE name=?`, project).Scan(&owner); err != nil {
			return nil, nil, "project not found"
		}
		u, err := s.st.UserByID(owner)
		if err != nil {
			return nil, nil, "project owner not found"
		}
		cp := *u
		cp.Role = "member"
		group, _ := claims["container_group"].(string)
		return &cp, &keyScope{KeyID: inst, Project: project, Role: RoleInstance, Group: group}, ""
	}
	return nil, nil, "invalid API key"
}

// instancePaths are the project sub-paths a workload identity may use.

// scopeAllows enforces a scoped credential on a request.
func scopeAllows(sc *keyScope, r *http.Request) (bool, string) {
	if sc == nil {
		return true, ""
	}
	path := r.URL.Path
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if path == "/v1/mcp" { // tool calls are re-checked as sub-requests
		return true, ""
	}
	if sc.Role == RoleViewer && !read {
		return false, "read-only key"
	}
	if path == "/v1/me/rotate-key" {
		return false, "scoped credentials cannot rotate the user's personal key"
	}
	if !read && (strings.HasPrefix(path, "/v1/users") || strings.HasPrefix(path, "/v1/invites")) {
		return false, "scoped credentials cannot manage users or invites"
	}
	if strings.HasPrefix(path, "/v1/keys") && sc.Role != RoleAdmin && sc.Role != RoleDeveloper {
		return false, "this credential cannot manage API keys"
	}
	if sc.Project == "" {
		return true, ""
	}
	prefix := "/v1/projects/" + sc.Project + "/"
	if strings.HasPrefix(path, prefix) {
		if sc.Role != RoleInstance {
			return true, ""
		}
		sub := strings.SplitN(strings.TrimPrefix(path, prefix), "/", 2)[0]
		switch sub {
		case "kv", "files", "file-tokens", "schedules-none":
			return true, ""
		case "queues": // submit/read jobs, not manage queues
			if strings.Contains(path, "/jobs") || read {
				return true, ""
			}
		case "containers", "events":
			if read {
				return true, ""
			}
		}
		return false, "instance identities may use kv, files and queue jobs, and read containers"
	}
	if read {
		switch path {
		case "/v1/me", "/v1/gpu-classes", "/v1/availability", "/v1/cluster":
			return true, ""
		case "/v1/projects": // filtered to the key's project by the handler
			return true, ""
		case "/v1/nodes":
			return sc.Role != RoleInstance, "instance identities cannot list nodes"
		}
		if path == "/v1/projects/"+sc.Project {
			return true, ""
		}
	}
	if path == "/v1/keys" || strings.HasPrefix(path, "/v1/keys/") {
		return sc.Role != RoleInstance, "instance identities cannot manage keys"
	}
	return false, "credential is limited to project " + sc.Project
}

// APIKey describes a scoped key (the secret is shown once, at creation).
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Project    string     `json:"project,omitempty"`
	Role       string     `json:"role"`
	User       string     `json:"user"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`
	CreateTime time.Time  `json:"create_time"`
	LastUsed   *time.Time `json:"last_used_time,omitempty"`
	Key        string     `json:"key,omitempty"`
}

func (s *Server) registerIAMRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/keys", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		var body struct {
			Name           string `json:"name"`
			Project        string `json:"project"`
			Role           string `json:"role"`
			ExpiresInHours int    `json:"expires_in_hours"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Role == "" {
			body.Role = RoleDeveloper
		}
		if body.Name == "" || len(body.Name) > 100 {
			problem(w, http.StatusBadRequest, "invalid key", "name is required (max 100 characters)")
			return
		}
		switch body.Role {
		case RoleDeveloper, RoleViewer:
		case RoleAdmin:
			if u.Role != "admin" {
				problem(w, http.StatusForbidden, "forbidden", "only admins can create admin keys")
				return
			}
		default:
			problem(w, http.StatusBadRequest, "invalid role", "role must be admin, developer or viewer")
			return
		}
		// A scoped key can only mint keys at most as powerful as itself.
		if sc := scopeFrom(r.Context()); sc != nil {
			if sc.Project != "" && body.Project != sc.Project {
				problem(w, http.StatusForbidden, "forbidden", "a project-scoped key can only create keys for project "+sc.Project)
				return
			}
			if body.Role == RoleAdmin && sc.Role != RoleAdmin {
				problem(w, http.StatusForbidden, "forbidden", "cannot escalate to admin")
				return
			}
		}
		if body.Project != "" && !s.projectAccess(w, u, body.Project) {
			return
		}
		if body.ExpiresInHours < 0 || body.ExpiresInHours > 24*366*5 {
			problem(w, http.StatusBadRequest, "invalid expiry", "expires_in_hours must be 0 (never) to 43920")
			return
		}
		now := store.Now()
		var exp int64
		if body.ExpiresInHours > 0 {
			exp = now + int64(body.ExpiresInHours)*3600*1000
		}
		k := APIKey{ID: store.NewID("key"), Name: body.Name, Project: body.Project, Role: body.Role, User: u.Name,
			CreateTime: store.T(now), Key: "ogmk_" + store.NewSecret(20)}
		if exp > 0 {
			t := store.T(exp)
			k.ExpireTime = &t
		}
		if _, err := s.st.DB.Exec(`INSERT INTO api_keys(id,key_hash,user_id,name,project,role,expires,created) VALUES(?,?,?,?,?,?,?,?)`,
			k.ID, store.Hash(k.Key), u.ID, k.Name, k.Project, k.Role, exp, now); err != nil {
			storeErr(w, err, "key")
			return
		}
		s.st.AddEvent(k.Project, "", "", "", "key.created", "API key "+k.Name+" ("+k.Role+") created by "+u.Name)
		writeJSON(w, http.StatusCreated, k)
	}))
	mux.HandleFunc("GET /v1/keys", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		q := `SELECT k.id,k.name,k.project,k.role,k.expires,k.created,k.last_used,us.name FROM api_keys k JOIN users us ON us.id=k.user_id`
		var args []any
		if u.Role != "admin" || r.URL.Query().Get("all") != "1" {
			q += ` WHERE k.user_id=?`
			args = append(args, u.ID)
		}
		rows, err := s.st.DB.Query(q+` ORDER BY k.created`, args...)
		if err != nil {
			storeErr(w, err, "keys")
			return
		}
		defer rows.Close()
		out := []APIKey{}
		for rows.Next() {
			var k APIKey
			var exp, created, last int64
			if rows.Scan(&k.ID, &k.Name, &k.Project, &k.Role, &exp, &created, &last, &k.User) != nil {
				continue
			}
			k.CreateTime = store.T(created)
			if exp > 0 {
				t := store.T(exp)
				k.ExpireTime = &t
			}
			if last > 0 {
				t := store.T(last)
				k.LastUsed = &t
			}
			out = append(out, k)
		}
		writeJSON(w, http.StatusOK, api.List[APIKey]{Items: out})
	}))
	mux.HandleFunc("DELETE /v1/keys/{id}", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		q, args := `DELETE FROM api_keys WHERE id=? AND user_id=?`, []any{r.PathValue("id"), u.ID}
		if u.Role == "admin" {
			q, args = `DELETE FROM api_keys WHERE id=?`, []any{r.PathValue("id")}
		}
		res, err := s.st.DB.Exec(q, args...)
		if err != nil {
			storeErr(w, err, "key")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			problem(w, http.StatusNotFound, "not found", "no such key")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// Instance-identity JWT audiences.
const (
	jwtAudWorkload = "ogm-workload" // default: proves identity to third parties
	jwtAudHub      = "ogm-hub"      // accepted as a hub API credential
)
