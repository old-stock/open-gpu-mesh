package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
	"github.com/old-stock/open-gpu-mesh/internal/web"
	"github.com/old-stock/open-gpu-mesh/internal/webhook"
)

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerGroupRoutes(mux)
	s.registerQueueRoutes(mux)
	s.registerMiscRoutes(mux)
	s.registerAssessRoutes(mux)
	s.registerSecretRoutes(mux)
	s.registerVolumeRoutes(mux)
	s.registerS3Routes(mux)
	s.registerMCPRoutes(mux)
	s.registerIAMRoutes(mux)
	s.registerKVRoutes(mux)
	s.registerScheduleRoutes(mux)
	s.registerSnapshotRoutes(mux)
	s.registerQuotaRoutes(mux)
	s.registerMetricsRoutes(mux)
	s.registerDocRoutes(mux)
	mux.HandleFunc("GET /v1/agent/connect", s.handleConnect)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.Handle("GET /", web.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.hostGateway(w, r) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/gw/") {
			s.handlePathGateway(w, r)
			return
		}
		if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
			s.handleRegistry(w, r)
			return
		}
		if r.URL.Path == "/s3" || strings.HasPrefix(r.URL.Path, "/s3/") {
			s.handleS3(w, r)
			return
		}
		w.Header().Set("X-OGM-Version", api.Version)
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) createProject(name, ownerID string) error {
	_, err := s.st.DB.Exec(`INSERT INTO projects(name,owner_id,created) VALUES(?,?,?)`, name, ownerID, store.Now())
	if err == nil {
		_, _ = s.st.DB.Exec(`INSERT OR IGNORE INTO project_members(project,user_id) VALUES(?,?)`, name, ownerID)
	}
	return err
}

func (s *Server) userAPI(u *store.UserRow) api.User {
	out := api.User{ID: u.ID, Name: u.Name, Role: u.Role, Credits: round4(u.Credits), CreateTime: store.T(u.Created)}
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE owner_id=?`, u.ID).Scan(&out.Nodes)
	_ = s.st.DB.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM ledger WHERE user_id=? AND reason='contribution'`, u.ID).Scan(&out.Contributed)
	_ = s.st.DB.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM ledger WHERE user_id=? AND reason='usage'`, u.ID).Scan(&out.Consumed)
	out.Contributed, out.Consumed = round4(out.Contributed), round4(out.Consumed)
	return out
}

func (s *Server) nodeAPI(n *store.NodeRow) api.Node {
	out := api.Node{
		ID: n.ID, Name: n.Name, Status: n.Status, Paused: n.Paused || n.AdminPaused, PauseReason: n.PauseReason,
		Runtime: n.Info.Runtime, Version: n.Info.Version, OS: n.Info.OS, Arch: n.Info.Arch, CPU: n.Info.CPU,
		MemoryMB: n.Info.MemoryMB, DiskGB: n.Info.DiskGB, GPUs: n.Info.GPUs, Country: n.Info.Country, Labels: n.Info.Labels,
		AllowPriorities: n.Info.AllowPriorities, Window: n.Info.Window, LastSeen: store.TP(n.LastSeen),
		ConnectedSince: store.TP(n.ConnectedSince), CreateTime: store.T(n.Created),
	}
	if out.GPUs == nil {
		out.GPUs = []api.GPU{}
	}
	if n.TrackedSeconds > 0 {
		out.UptimeRatio = float64(n.OnlineSeconds) / float64(n.TrackedSeconds)
		if out.UptimeRatio > 1 {
			out.UptimeRatio = 1
		}
	}
	if u, err := s.st.UserByID(n.OwnerID); err == nil {
		out.Owner = u.Name
	}
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM instances WHERE node_id=? AND state!='terminated'`, n.ID).Scan(&out.Instances)
	if a, err := s.st.Assessment(n.ID); err == nil {
		out.Assessment = a
	}
	return out
}

func (s *Server) installCmd(token string) string {
	return fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s", s.cfg.PublicURL, token)
}

func (s *Server) registerMiscRoutes(mux *http.ServeMux) {
	// ----- identity -----
	mux.HandleFunc("GET /v1/me", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		out := s.userAPI(u)
		cred := &api.Credential{Type: "user_key", Role: u.Role}
		if sc := scopeFrom(r.Context()); sc != nil {
			cred = &api.Credential{Type: "api_key", KeyID: sc.KeyID, Project: sc.Project, Role: sc.Role, Group: sc.Group}
			if sc.Role == RoleInstance {
				cred.Type, cred.KeyID = "instance", ""
			}
		}
		out.Credential = cred
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("POST /v1/me/rotate-key", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		key, err := s.st.RotateUserKey(u.ID)
		if err != nil {
			storeErr(w, err, "user")
			return
		}
		out := s.userAPI(u)
		out.APIKey = key
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /v1/me/ledger", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		rows, err := s.st.DB.Query(`SELECT id,delta,reason,ref,time FROM ledger WHERE user_id=? ORDER BY id DESC LIMIT ?`, u.ID, qint(r, "limit", 100))
		if err != nil {
			storeErr(w, err, "ledger")
			return
		}
		defer rows.Close()
		out := []api.LedgerEntry{}
		for rows.Next() {
			var e api.LedgerEntry
			var t int64
			if rows.Scan(&e.ID, &e.Delta, &e.Reason, &e.Ref, &t) == nil {
				e.Time, e.User = store.T(t), u.Name
				out = append(out, e)
			}
		}
		writeJSON(w, http.StatusOK, api.List[api.LedgerEntry]{Items: out})
	}))

	// ----- invites & joining -----
	mux.HandleFunc("POST /v1/invites", s.admin(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		var body struct {
			Role     string `json:"role"`
			Uses     int    `json:"uses"`
			TTLHours int    `json:"ttl_hours"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Role == "" {
			body.Role = "member"
		}
		if body.Role != "member" && body.Role != "admin" {
			validationProblem(w, map[string][]string{"role": {"member or admin"}})
			return
		}
		if body.Uses <= 0 {
			body.Uses = 1
		}
		if body.TTLHours <= 0 {
			body.TTLHours = 24 * 7
		}
		code := "inv_" + store.NewSecret(12)
		exp := time.Now().Add(time.Duration(body.TTLHours) * time.Hour)
		if _, err := s.st.DB.Exec(`INSERT INTO invites(code,role,uses_left,expires,created_by,created) VALUES(?,?,?,?,?,?)`,
			code, body.Role, body.Uses, exp.UnixMilli(), u.ID, store.Now()); err != nil {
			storeErr(w, err, "invite")
			return
		}
		writeJSON(w, http.StatusCreated, api.Invite{Code: code, Role: body.Role, UsesLeft: body.Uses, Expires: &exp,
			JoinCmd: fmt.Sprintf("ogm join %s %s --name <your-name>", s.cfg.PublicURL, code)})
	}))
	mux.HandleFunc("POST /v1/join", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		if !api.ValidName(body.Name) {
			validationProblem(w, map[string][]string{"name": {"must be a lowercase DNS label, e.g. alice"}})
			return
		}
		var role string
		var uses int
		var exp int64
		err := s.st.DB.QueryRow(`SELECT role,uses_left,COALESCE(expires,0) FROM invites WHERE code=?`, body.Code).Scan(&role, &uses, &exp)
		if err != nil || uses <= 0 || (exp > 0 && exp < store.Now()) {
			problem(w, http.StatusForbidden, "invalid invite", "invite code is invalid, used up or expired")
			return
		}
		u, key, err := s.st.CreateUser(body.Name, role, s.cfg.StarterCredits)
		if err != nil {
			storeErr(w, err, "user "+body.Name)
			return
		}
		_, _ = s.st.DB.Exec(`UPDATE invites SET uses_left=uses_left-1 WHERE code=?`, body.Code)
		_ = s.createProject(body.Name, u.ID)
		_, _ = s.st.DB.Exec(`INSERT OR IGNORE INTO project_members(project,user_id) VALUES('default',?)`, u.ID)
		out := s.userAPI(u)
		out.APIKey = key
		writeJSON(w, http.StatusCreated, out)
	})

	// ----- users (admin) -----
	mux.HandleFunc("GET /v1/users", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		us, _ := s.st.Users()
		out := []api.User{}
		for _, x := range us {
			out = append(out, s.userAPI(x))
		}
		sort.SliceStable(out, func(a, b int) bool { return out[a].Contributed > out[b].Contributed })
		writeJSON(w, http.StatusOK, api.List[api.User]{Items: out})
	}))
	mux.HandleFunc("PATCH /v1/users/{name}", s.admin(func(w http.ResponseWriter, r *http.Request, admin *store.UserRow) {
		u, err := s.st.UserByName(r.PathValue("name"))
		if err != nil {
			storeErr(w, err, "user")
			return
		}
		var body struct {
			Role         *string  `json:"role"`
			GrantCredits *float64 `json:"grant_credits"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Role != nil && (*body.Role == "admin" || *body.Role == "member") {
			_, _ = s.st.DB.Exec(`UPDATE users SET role=? WHERE id=?`, *body.Role, u.ID)
		}
		if body.GrantCredits != nil {
			_ = s.st.AddCredits(u.ID, *body.GrantCredits, "grant", "by "+admin.Name)
		}
		u, _ = s.st.UserByID(u.ID)
		writeJSON(w, http.StatusOK, s.userAPI(u))
	}))

	// ----- projects -----
	mux.HandleFunc("GET /v1/projects", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		q := `SELECT p.name, p.owner_id, p.created FROM projects p`
		var args []any
		if u.Role != "admin" {
			q += ` WHERE (p.owner_id=? OR EXISTS (SELECT 1 FROM project_members m WHERE m.project=p.name AND m.user_id=?))`
			args = append(args, u.ID, u.ID)
		}
		if sc := scopeFrom(r.Context()); sc != nil && sc.Project != "" {
			if strings.Contains(q, "WHERE") {
				q += ` AND p.name=?`
			} else {
				q += ` WHERE p.name=?`
			}
			args = append(args, sc.Project)
		}
		rows, err := s.st.DB.Query(q+` ORDER BY p.created`, args...)
		if err != nil {
			storeErr(w, err, "projects")
			return
		}
		type pr struct {
			name, owner string
			created     int64
		}
		var ps []pr
		for rows.Next() {
			var x pr
			if rows.Scan(&x.name, &x.owner, &x.created) == nil {
				ps = append(ps, x)
			}
		}
		rows.Close()
		out := []api.Project{}
		for _, x := range ps {
			p := api.Project{Name: x.name, CreateTime: store.T(x.created), Members: []string{}}
			if o, err := s.st.UserByID(x.owner); err == nil {
				p.Owner = o.Name
			}
			mrows, _ := s.st.DB.Query(`SELECT u.name FROM project_members m JOIN users u ON u.id=m.user_id WHERE m.project=? ORDER BY u.name`, x.name)
			if mrows != nil {
				for mrows.Next() {
					var n string
					if mrows.Scan(&n) == nil {
						p.Members = append(p.Members, n)
					}
				}
				mrows.Close()
			}
			out = append(out, p)
		}
		writeJSON(w, http.StatusOK, api.List[api.Project]{Items: out})
	}))
	mux.HandleFunc("POST /v1/projects", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		if !api.ValidName(body.Name) {
			validationProblem(w, map[string][]string{"name": {"must be a lowercase DNS label"}})
			return
		}
		if err := s.createProject(body.Name, u.ID); err != nil {
			storeErr(w, err, "project "+body.Name)
			return
		}
		writeJSON(w, http.StatusCreated, api.Project{Name: body.Name, Owner: u.Name, Members: []string{u.Name}, CreateTime: time.Now().UTC()})
	}))
	mux.HandleFunc("POST /v1/projects/{project}/members", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		if !s.projectManager(w, r, u, p) {
			return
		}
		var body struct {
			User string `json:"user"`
		}
		if !decode(w, r, &body) {
			return
		}
		m, err := s.st.UserByName(body.User)
		if err != nil {
			storeErr(w, err, "user")
			return
		}
		_, _ = s.st.DB.Exec(`INSERT OR IGNORE INTO project_members(project,user_id) VALUES(?,?)`, p, m.ID)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /v1/projects/{project}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		if !s.projectManager(w, r, u, p) {
			return
		}
		var n int
		_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM container_groups WHERE project=?`, p).Scan(&n)
		if n > 0 {
			problem(w, http.StatusConflict, "project not empty", "delete its container groups first")
			return
		}
		_, _ = s.st.DB.Exec(`DELETE FROM projects WHERE name=?`, p)
		_, _ = s.st.DB.Exec(`DELETE FROM project_members WHERE project=?`, p)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /v1/projects/{project}/events", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		writeJSON(w, http.StatusOK, api.List[api.Event]{Items: s.events(`project=?`, qint(r, "limit", 200), p)})
	}))

	// ----- nodes -----
	mux.HandleFunc("GET /v1/nodes", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		ns, _ := s.st.Nodes(false)
		out := []api.Node{}
		for _, n := range ns {
			out = append(out, s.nodeAPI(n))
		}
		writeJSON(w, http.StatusOK, api.List[api.Node]{Items: out})
	}))
	mux.HandleFunc("POST /v1/nodes", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Name == "" {
			body.Name = u.Name + "-" + store.NewSecret(2)
		}
		n, tok, err := s.st.CreateNode(body.Name, u.ID)
		if err != nil {
			storeErr(w, err, "node")
			return
		}
		writeJSON(w, http.StatusCreated, api.NodeToken{NodeID: n.ID, Name: n.Name, Token: tok, InstallCmd: s.installCmd(tok)})
	}))
	nodeOwned := func(w http.ResponseWriter, r *http.Request, u *store.UserRow) (*store.NodeRow, bool) {
		n, err := s.st.NodeByID(r.PathValue("node"))
		if err != nil {
			storeErr(w, err, "node")
			return nil, false
		}
		if n.OwnerID != u.ID && u.Role != "admin" {
			problem(w, http.StatusForbidden, "forbidden", "only the node owner or an admin can manage this node")
			return nil, false
		}
		return n, true
	}
	mux.HandleFunc("GET /v1/nodes/{node}", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		n, err := s.st.NodeByID(r.PathValue("node"))
		if err != nil {
			storeErr(w, err, "node")
			return
		}
		writeJSON(w, http.StatusOK, s.nodeAPI(n))
	}))
	mux.HandleFunc("PATCH /v1/nodes/{node}", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		n, ok := nodeOwned(w, r, u)
		if !ok {
			return
		}
		var body struct {
			Name   *string `json:"name"`
			Paused *bool   `json:"paused"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Name != nil && *body.Name != "" {
			_, _ = s.st.DB.Exec(`UPDATE nodes SET name=? WHERE id=?`, *body.Name, n.ID)
		}
		if body.Paused != nil {
			_, _ = s.st.DB.Exec(`UPDATE nodes SET admin_paused=? WHERE id=?`, *body.Paused, n.ID)
			s.hub.send(n.ID, proto.TypeNodeConfig, proto.NodeConfig{Paused: body.Paused})
			s.Poke()
		}
		n, _ = s.st.NodeByID(n.ID)
		writeJSON(w, http.StatusOK, s.nodeAPI(n))
	}))
	mux.HandleFunc("DELETE /v1/nodes/{node}", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		n, ok := nodeOwned(w, r, u)
		if !ok {
			return
		}
		s.mu.Lock()
		insts, _ := s.st.InstancesWhere(`node_id=? AND state!='terminated'`, n.ID)
		for _, in := range insts {
			s.requestStop(in, "node removed", "", 10)
		}
		s.mu.Unlock()
		if ns := s.hub.get(n.ID); ns != nil {
			ns.sess.Close()
		}
		_, _ = s.st.DB.Exec(`DELETE FROM nodes WHERE id=?`, n.ID)
		_, _ = s.st.DB.Exec(`DELETE FROM node_assessments WHERE node_id=?`, n.ID)
		s.Poke()
		w.WriteHeader(http.StatusNoContent)
	}))

	// ----- GPU classes & availability -----
	mux.HandleFunc("GET /v1/gpu-classes", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		writeJSON(w, http.StatusOK, api.List[api.GPUClass]{Items: s.gpuClasses()})
	}))
	mux.HandleFunc("GET /v1/availability", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		writeJSON(w, http.StatusOK, api.List[api.GPUClass]{Items: s.gpuClasses()})
	}))
	mux.HandleFunc("PATCH /v1/gpu-classes/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		var body struct {
			CreditsPerHour *float64 `json:"credits_per_hour"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.CreditsPerHour != nil {
			_, _ = s.st.DB.Exec(`UPDATE gpu_classes SET credits_per_hour=? WHERE id=?`, *body.CreditsPerHour, r.PathValue("id"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// ----- webhook secret, cluster, jwks -----
	mux.HandleFunc("GET /v1/webhook-secret", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		// Each project gets its own signing key, so members of one project cannot
		// forge webhooks delivered to another project's receivers.
		p := r.URL.Query().Get("project")
		if p == "" {
			p = "default"
			if sc := scopeFrom(r.Context()); sc != nil && sc.Project != "" {
				p = sc.Project
			}
		}
		if sc := scopeFrom(r.Context()); sc != nil && sc.Project != "" && sc.Project != p {
			problem(w, http.StatusForbidden, "forbidden", "this key is bound to project "+sc.Project)
			return
		}
		if !s.projectAccess(w, u, p) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"secret_key": s.webhookSecret(p), "project": p})
	}))
	mux.HandleFunc("POST /v1/webhook-secret", s.admin(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		sec := store.NewSecret(32)
		b, _ := hex.DecodeString(sec)
		enc := base64Std(b)
		_ = s.st.SetSetting("webhook_secret", enc)
		// Every project's key changes; return the default project's for convenience.
		writeJSON(w, http.StatusOK, map[string]string{"secret_key": s.webhookSecret("default"), "project": "default"})
	}))
	mux.HandleFunc("GET /v1/cluster", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.clusterInfo())
	})
	mux.HandleFunc("GET /v1/events", s.admin(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		writeJSON(w, http.StatusOK, api.List[api.Event]{Items: s.events("", qint(r, "limit", 200))})
	}))
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{webhook.JWK(s.jwtPub, s.jwtKid)}})
	})

	// ----- files -----
	mux.HandleFunc("GET /v1/projects/{project}/files", s.projected(s.handleListFiles))
	mux.HandleFunc("PUT /v1/projects/{project}/files/{path...}", s.projected(s.handlePutFile))
	mux.HandleFunc("GET /v1/projects/{project}/files/{path...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		s.serveFile(w, r, p, r.PathValue("path"))
	}))
	mux.HandleFunc("DELETE /v1/projects/{project}/files/{path...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		fp, ok := cleanFilePath(r.PathValue("path"))
		if !ok {
			problem(w, http.StatusBadRequest, "bad path", "invalid file path")
			return
		}
		if reservedFilePath(fp) {
			problem(w, http.StatusForbidden, "forbidden", "snapshots are managed through the snapshots API")
			return
		}
		_, _ = s.st.DB.Exec(`DELETE FROM files WHERE project=? AND path=?`, p, fp)
		_ = os.Remove(s.filePath(p, fp))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /v1/projects/{project}/file-tokens/{path...}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var body struct {
			ExpiresIn int    `json:"expires_in_s"`
			Method    string `json:"method"`
		}
		_ = decode(w, r, &body)
		if body.ExpiresIn <= 0 {
			body.ExpiresIn = 3600
		}
		if c, _ := cleanFilePath(r.PathValue("path")); body.Method != "" && !strings.EqualFold(body.Method, "GET") && reservedFilePath(c) {
			problem(w, http.StatusForbidden, "forbidden", "snapshots are managed through the snapshots API")
			return
		}
		if body.ExpiresIn > 7*24*3600 {
			validationProblem(w, map[string][]string{"expires_in_s": {"must be at most 604800 (7 days)"}})
			return
		}
		if body.Method == "" {
			body.Method = "GET"
		}
		exp := time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
		fp := r.PathValue("path")
		sig := s.signFile(body.Method, p, fp, exp.Unix())
		url := fmt.Sprintf("%s/v1/signed/%s/%s?method=%s&exp=%d&sig=%s", s.cfg.PublicURL, p, fp, body.Method, exp.Unix(), sig)
		writeJSON(w, http.StatusOK, map[string]any{"url": url, "expires": exp.UTC(), "method": body.Method})
	}))
	signed := func(w http.ResponseWriter, r *http.Request) {
		p, fp := r.PathValue("project"), r.PathValue("path")
		q := r.URL.Query()
		exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64)
		method := q.Get("method")
		if method == "" {
			method = "GET"
		}
		if exp < time.Now().Unix() || !hmac.Equal([]byte(q.Get("sig")), []byte(s.signFile(method, p, fp, exp))) || !strings.EqualFold(method, r.Method) && !(method == "GET" && r.Method == "HEAD") {
			problem(w, http.StatusForbidden, "forbidden", "invalid or expired signature")
			return
		}
		if r.Method == http.MethodPut {
			s.handlePutFile(w, r, nil, p)
			return
		}
		s.serveFile(w, r, p, fp)
	}
	mux.HandleFunc("GET /v1/signed/{project}/{path...}", signed)
	mux.HandleFunc("PUT /v1/signed/{project}/{path...}", signed)

	// ----- installer & binaries -----
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /dl/{target}", s.handleDownload)
}

func (s *Server) gpuClasses() []api.GPUClass {
	cls, _ := s.st.GPUClasses()
	nodes, _ := s.st.Nodes(true)
	used := map[string]map[int]bool{}
	insts, _ := s.st.InstancesWhere(`state!='terminated' AND node_id!=''`)
	for _, in := range insts {
		if used[in.NodeID] == nil {
			used[in.NodeID] = map[int]bool{}
		}
		for _, i := range in.GPUIndexes {
			used[in.NodeID][i] = true
		}
	}
	out := []api.GPUClass{}
	for _, c := range cls {
		gc := api.GPUClass{ID: c.ID, Name: c.Name, VRAMMB: c.VRAMMB, CreditsPerHr: c.CreditsPerHour, PricePriority: map[string]float64{}}
		for p, m := range priorityMultiplier {
			gc.PricePriority[p] = round4(c.CreditsPerHour * m)
		}
		for _, n := range nodes {
			for _, g := range n.Info.GPUs {
				if g.Class != c.ID {
					continue
				}
				gc.Online++
				if used[n.ID][g.Index] {
					gc.InUse++
				} else if !n.Paused && !n.AdminPaused {
					gc.Free++
				}
			}
		}
		gc.HighDemand = gc.Online > 0 && gc.Free == 0
		out = append(out, gc)
	}
	return out
}

func (s *Server) clusterInfo() api.ClusterInfo {
	ci := api.ClusterInfo{Name: s.cfg.ClusterName, Version: api.Version, GatewayDomain: s.cfg.GatewayDomain, PublicURL: s.cfg.PublicURL}
	nodes, _ := s.st.Nodes(false)
	ci.Nodes = len(nodes)
	vram := 0
	for _, n := range nodes {
		if n.Status == "online" {
			ci.NodesOnline++
			ci.GPUs += len(n.Info.GPUs)
			for _, g := range n.Info.GPUs {
				vram += g.VRAMMB
			}
		}
	}
	ci.TotalVRAMGB = float64(vram) / 1024
	for _, c := range s.gpuClasses() {
		ci.GPUsFree += c.Free
	}
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&ci.Users)
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM container_groups`).Scan(&ci.Groups)
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM instances WHERE state='running'`).Scan(&ci.Instances)
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='pending'`).Scan(&ci.JobsPending)
	return ci
}

// ---------- files ----------

func (s *Server) filesRoot() string {
	if s.cfg.DataDir == "" {
		return filepath.Join(os.TempDir(), "ogm-files-"+s.jwtKid)
	}
	return filepath.Join(s.cfg.DataDir, "files")
}

// reservedFilePath reports paths the files API may not write or delete: snapshot
// archives live under .snapshots/ and are only managed by the snapshots API.
func reservedFilePath(p string) bool {
	return p == ".snapshots" || strings.HasPrefix(p, ".snapshots/")
}

// setUserContentHeaders makes user-uploaded content safe to serve from the hub's
// origin: it can never run script there (CSP sandbox, nosniff), and anything a
// browser might render as a document is downloaded instead of displayed.
func setUserContentHeaders(h http.Header, ct, name string) {
	h.Set("Content-Type", ct)
	hardenUserContent(h, name)
}

// hardenUserContent applies the user-content policy to the headers already set.
func hardenUserContent(h http.Header, name string) {
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
		h.Set("Content-Type", ct)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch {
	case strings.HasPrefix(mt, "image/") && mt != "image/svg+xml",
		strings.HasPrefix(mt, "audio/"), strings.HasPrefix(mt, "video/"),
		mt == "text/plain", mt == "text/csv", mt == "application/json", mt == "application/pdf", mt == "application/octet-stream":
	default:
		if name == "" {
			h.Set("Content-Disposition", "attachment")
		} else {
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		}
	}
}

func cleanFilePath(p string) (string, bool) {
	c := path.Clean("/" + p)
	if c == "/" || strings.Contains(p, "..") {
		return "", false
	}
	return strings.TrimPrefix(c, "/"), true
}

func (s *Server) filePath(project, p string) string {
	c, _ := cleanFilePath(p)
	return filepath.Join(s.filesRoot(), project, filepath.FromSlash(c))
}

func (s *Server) signFile(method, project, p string, exp int64) string {
	key, _ := s.st.GetSetting("files_key")
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "%s\n%s\n%s\n%d", strings.ToUpper(method), project, p, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// handlePutFile stores an upload. u is nil for hub-signed URLs, which is how agents
// upload snapshot archives; user requests may not write under .snapshots/.
func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request, u *store.UserRow, project string) {
	fp, ok := cleanFilePath(r.PathValue("path"))
	if !ok {
		problem(w, http.StatusBadRequest, "bad path", "invalid file path")
		return
	}
	if u != nil && reservedFilePath(fp) {
		problem(w, http.StatusForbidden, "forbidden", "snapshots are managed through the snapshots API")
		return
	}
	dst := s.filePath(project, fp)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		storeErr(w, err, "file")
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		storeErr(w, err, "file")
		return
	}
	h := sha256.New()
	limit := int64(50 << 30)
	if left := s.storageRemaining(project); left >= 0 {
		var old int64 // overwriting a file frees its old size
		_ = s.st.DB.QueryRow(`SELECT size FROM files WHERE project=? AND path=?`, project, fp).Scan(&old)
		if left+old < limit {
			limit = left + old
		}
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r.Body, limit+1))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		problem(w, http.StatusBadRequest, "upload failed", err.Error())
		return
	}
	if n > limit {
		os.Remove(tmp.Name())
		if limit < 50<<30 {
			problem(w, http.StatusForbidden, "quota exceeded", fmt.Sprintf("the project's storage quota allows %d more bytes", limit))
		} else {
			problem(w, http.StatusRequestEntityTooLarge, "too large", "files are limited to 50 GiB")
		}
		return
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		storeErr(w, err, "file")
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(fp))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	sum := hex.EncodeToString(h.Sum(nil))
	_, _ = s.st.DB.Exec(`INSERT INTO files(project,path,size,sha256,content_type,created) VALUES(?,?,?,?,?,?)
		ON CONFLICT(project,path) DO UPDATE SET size=excluded.size, sha256=excluded.sha256, content_type=excluded.content_type, created=excluded.created`,
		project, fp, n, sum, ct, store.Now())
	writeJSON(w, http.StatusCreated, api.FileInfo{Path: fp, Size: n, SHA256: sum, ContentType: ct, CreateTime: time.Now().UTC()})
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, project, p string) {
	fp, ok := cleanFilePath(p)
	if !ok {
		problem(w, http.StatusBadRequest, "bad path", "invalid file path")
		return
	}
	var ct string
	if err := s.st.DB.QueryRow(`SELECT content_type FROM files WHERE project=? AND path=?`, project, fp).Scan(&ct); err != nil {
		problem(w, http.StatusNotFound, "not found", "file not found")
		return
	}
	f, err := os.Open(s.filePath(project, fp))
	if err != nil {
		problem(w, http.StatusNotFound, "not found", "file missing on disk")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	setUserContentHeaders(w.Header(), ct, path.Base(fp))
	http.ServeContent(w, r, path.Base(fp), st.ModTime(), f)
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request, u *store.UserRow, project string) {
	prefix := r.URL.Query().Get("prefix")
	rows, err := s.st.DB.Query(`SELECT path,size,sha256,content_type,created FROM files WHERE project=? AND path LIKE ? ORDER BY path LIMIT ?`,
		project, prefix+"%", qint(r, "limit", 1000))
	if err != nil {
		storeErr(w, err, "files")
		return
	}
	defer rows.Close()
	out := []api.FileInfo{}
	for rows.Next() {
		var f api.FileInfo
		var t int64
		if rows.Scan(&f.Path, &f.Size, &f.SHA256, &f.ContentType, &t) == nil {
			f.CreateTime = store.T(t)
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, api.List[api.FileInfo]{Items: out})
}

// ---------- installer ----------

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target") // e.g. linux-amd64
	if strings.ContainsAny(target, "/\\.") {
		http.NotFound(w, r)
		return
	}
	if s.cfg.DistDir != "" {
		p := filepath.Join(s.cfg.DistDir, "ogm-"+target)
		if _, err := os.Stat(p); err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, p)
			return
		}
	}
	if target == runtime.GOOS+"-"+runtime.GOARCH {
		if exe, err := os.Executable(); err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, exe)
			return
		}
	}
	problem(w, http.StatusNotFound, "binary not available",
		"no ogm build for "+target+" on this server; build with `make dist` and start the server with --dist-dir")
}

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	fmt.Fprintf(w, installScript, s.cfg.PublicURL)
}

const installScript = `#!/bin/sh
# open-gpu-mesh agent installer.
# Usage: curl -fsSL <server>/install.sh | sh -s -- --token <node-token> [--profile gaming,cad] [--no-assess] [agent flags]
# The installer audits the machine (hardware, speed test, NAT) and applies suggested limits;
# agent flags you pass explicitly (e.g. --max-cpu 4) override the suggestions.
set -eu
SERVER="%s"
TOKEN=""; PROFILE=""; ASSESS=1; ARGS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; ARGS="$ARGS --token $2"; shift 2;;
    --token=*) TOKEN="${1#--token=}"; ARGS="$ARGS $1"; shift;;
    --profile) PROFILE="$2"; shift 2;;
    --profile=*) PROFILE="${1#--profile=}"; shift;;
    --no-assess) ASSESS=0; shift;;
    *) ARGS="$ARGS $1"; shift;;
  esac
done
[ -n "$TOKEN" ] || { echo "usage: install.sh --token <node-token> [--profile ...] [agent flags]"; exit 1; }
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) echo "unsupported arch $ARCH"; exit 1;; esac
BIN=/usr/local/bin/ogm
SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"
echo "==> downloading ogm for $OS-$ARCH from $SERVER"
curl -fsSL "$SERVER/dl/$OS-$ARCH" -o /tmp/ogm.$$
chmod +x /tmp/ogm.$$
$SUDO mv /tmp/ogm.$$ "$BIN"
if ! command -v docker >/dev/null 2>&1; then
  echo "!!  docker not found. Install Docker (and the NVIDIA Container Toolkit for GPUs) first:"
  echo "    https://docs.docker.com/engine/install/  and  https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/"
fi
SUGGESTED=""
if [ "$ASSESS" = 1 ]; then
  echo "==> auditing this machine (about 30 s: hardware, CPU/disk benchmark, speed test, NAT type)"
  set -- --server "$SERVER" --token "$TOKEN" --quick --flags-only
  [ -n "$PROFILE" ] && set -- "$@" --profile "$PROFILE"
  if [ -z "$PROFILE" ] && (exec </dev/tty) 2>/dev/null; then
    SUGGESTED=$("$BIN" assess "$@" </dev/tty) || SUGGESTED=""
  else
    SUGGESTED=$("$BIN" assess "$@" </dev/null) || SUGGESTED=""
  fi
  [ -n "$SUGGESTED" ] && echo "==> applying suggested limits: $SUGGESTED"
fi
# Suggested flags come first so explicitly passed flags win (last flag wins).
AGENT_ARGS="$SUGGESTED$ARGS"
if command -v systemctl >/dev/null 2>&1 && [ -d /etc/systemd/system ]; then
  echo "==> installing systemd service ogm-agent"
  $SUDO tee /etc/systemd/system/ogm-agent.service >/dev/null <<UNIT
[Unit]
Description=open-gpu-mesh agent
After=network-online.target docker.service
Wants=network-online.target

[Service]
ExecStart=$BIN agent --server $SERVER $AGENT_ARGS
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable --now ogm-agent
  echo "==> done. Logs: journalctl -u ogm-agent -f   Re-audit any time: ogm assess --token <node-token>"
else
  echo "==> no systemd; run manually:"
  echo "    $BIN agent --server $SERVER $AGENT_ARGS"
fi
`

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
