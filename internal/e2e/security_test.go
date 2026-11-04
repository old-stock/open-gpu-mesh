package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
)

// TestSecurityHardening covers the protections added after the pre-release security review.
func TestSecurityHardening(t *testing.T) {
	cl := newCluster(t)
	ctx := cl.ctx
	for _, p := range []string{"sec-xy", "xy", "tmp"} {
		if err := cl.c.Do(ctx, "POST", "/v1/projects", map[string]string{"name": p}, nil); err != nil {
			t.Fatal(err)
		}
	}
	problemHas := func(err error, want string) bool {
		pr, ok := err.(*api.Problem)
		return ok && strings.Contains(fmt.Sprintf("%s %+v", pr.Detail, pr.Errors), want)
	}

	t.Run("custom domain cannot claim the hub", func(t *testing.T) {
		s := cpuSpec("grab", 0)
		s.Networking.CustomDomains = []string{"ogm.internal"}
		if _, err := cl.c.CreateGroup(ctx, "default", s); !problemHas(err, "reserved") {
			t.Fatalf("ogm.internal accepted: %v", err)
		}
	})

	t.Run("gateway hostnames cannot collide across projects", func(t *testing.T) {
		if _, err := cl.c.CreateGroup(ctx, "sec-xy", cpuSpec("web", 0)); err != nil {
			t.Fatal(err)
		}
		// web-sec + xy and web + sec-xy both serve web-sec-xy.<gateway domain>
		if _, err := cl.c.CreateGroup(ctx, "xy", cpuSpec("web-sec", 0)); !problemHas(err, "already used") {
			t.Fatalf("colliding gateway label accepted: %v", err)
		}
		_ = cl.c.DeleteGroup(ctx, "sec-xy", "web")
	})

	var dev map[string]any
	if err := cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "unbound-dev", "role": "developer"}, &dev); err != nil {
		t.Fatal(err)
	}
	dk := dev["key"].(string)

	t.Run("scoped keys cannot escalate", func(t *testing.T) {
		if c, _, _ := cl.raw(dk, "POST", "/v1/me/rotate-key", "", nil); c != 403 {
			t.Fatalf("rotate-key with scoped key: %d", c)
		}
		if c, _, _ := cl.raw(dk, "POST", "/v1/invites", `{"role":"admin"}`, nil); c != 403 {
			t.Fatalf("invite with scoped key: %d", c)
		}
		if c, _, _ := cl.raw(dk, "DELETE", "/v1/projects/tmp", "", nil); c != 403 {
			t.Fatalf("project delete with developer key: %d", c)
		}
		if c, _, _ := cl.raw(dk, "POST", "/v1/projects/tmp/members", `{"user":"admin"}`, nil); c != 403 {
			t.Fatalf("member add with developer key: %d", c)
		}
		if c, b, _ := cl.raw(cl.key, "DELETE", "/v1/projects/tmp", "", nil); c != 204 {
			t.Fatalf("owner delete: %d %s", c, b)
		}
	})

	t.Run("user content is served safely", func(t *testing.T) {
		html := `<script>alert(localStorage.ogm_key)</script>`
		if c, b, _ := cl.raw(cl.key, "PUT", "/v1/projects/default/files/x.html", html, map[string]string{"Content-Type": "text/html"}); c/100 != 2 {
			t.Fatalf("put: %d %s", c, b)
		}
		c, _, h := cl.raw(cl.key, "GET", "/v1/projects/default/files/x.html", "", nil)
		if c != 200 || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("html served inline: %d %v", c, h)
		}
		if c, _, _ := cl.raw(cl.key, "PUT", "/v1/projects/default/files/.snapshots/g/snap_x.tar.gz", "evil", nil); c != 403 {
			t.Fatalf("snapshot overwrite: %d", c)
		}
		if c, _, _ := cl.raw(cl.key, "DELETE", "/v1/projects/default/files/.snapshots/g/snap_x.tar.gz", "", nil); c != 403 {
			t.Fatalf("snapshot delete: %d", c)
		}
		if c, _, _ := cl.raw(cl.key, "POST", "/v1/projects/default/file-tokens/.snapshots//g/s.tar.gz", `{"method":"PUT"}`, nil); c != 403 {
			t.Fatalf("signed PUT into .snapshots: %d", c)
		}
		if c, _, _ := cl.raw(cl.key, "POST", "/v1/projects/default/file-tokens/x.html", `{"expires_in_s":99999999}`, nil); c != 422 && c != 400 {
			t.Fatalf("unbounded signed URL: %d", c)
		}
	})

	t.Run("webhook keys are per project", func(t *testing.T) {
		var a, b map[string]string
		_ = cl.c.Do(ctx, "GET", "/v1/webhook-secret?project=default", nil, &a)
		_ = cl.c.Do(ctx, "GET", "/v1/webhook-secret?project=xy", nil, &b)
		if a["secret_key"] == "" || a["secret_key"] == b["secret_key"] {
			t.Fatalf("webhook keys: %v %v", a, b)
		}
		var pk map[string]any
		_ = cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "xk", "project": "xy", "role": "viewer"}, &pk)
		if c, _, _ := cl.raw(pk["key"].(string), "GET", "/v1/webhook-secret?project=default", "", nil); c != 403 {
			t.Fatalf("cross-project webhook key: %d", c)
		}
	})

	t.Run("default quota applies to new projects", func(t *testing.T) {
		if err := cl.c.Do(ctx, "PUT", "/v1/projects/*/quota", map[string]int{"instances": 1}, nil); err != nil {
			t.Fatal(err)
		}
		if err := cl.c.Do(ctx, "POST", "/v1/projects", map[string]string{"name": "fresh"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.c.CreateGroup(ctx, "fresh", cpuSpec("big", 3)); !problemHas(err, "quota") {
			t.Fatalf("default quota not enforced: %v", err)
		}
		_ = cl.c.Do(ctx, "PUT", "/v1/projects/*/quota", map[string]int{}, nil)
	})

	t.Run("secrets only go to trusted nodes", func(t *testing.T) {
		var inv api.Invite
		if err := cl.c.Do(ctx, "POST", "/v1/invites", map[string]any{"uses": 1}, &inv); err != nil {
			t.Fatal(err)
		}
		var vol api.User
		if err := cl.c.Do(ctx, "POST", "/v1/join", map[string]any{"code": inv.Code, "name": "volunteer"}, &vol); err != nil {
			t.Fatal(err)
		}
		cl.addAgentAs(client.New(cl.url, vol.APIKey), "community", "")
		eventually(t, 10*time.Second, "community node online", func() (bool, string) {
			ns, _ := cl.c.Nodes(ctx)
			return len(ns) == 1 && ns[0].Status == "online" && !ns[0].Trusted, fmt.Sprint(ns)
		})
		// Every user is a member of "default" (and could read its secrets anyway by
		// deploying a group there), so use a project the volunteer is not in.
		if err := cl.c.Do(ctx, "POST", "/v1/projects", map[string]string{"name": "private"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := cl.c.Do(ctx, "PUT", "/v1/projects/private/secrets/db-pass", map[string]string{"value": "hunter2"}, nil); err != nil {
			t.Fatal(err)
		}
		s := cpuSpec("sek", 1)
		s.Container.Env = map[string]string{"DB_PASS": "{{secret:db-pass}}"}
		if _, err := cl.c.CreateGroup(ctx, "private", s); err != nil {
			t.Fatal(err)
		}
		pub := cpuSpec("nosecret", 1)
		if _, err := cl.c.CreateGroup(ctx, "private", pub); err != nil {
			t.Fatal(err)
		}
		cl.waitReadyIn("private", "nosecret", 1) // groups without secrets still use community nodes
		time.Sleep(2 * time.Second)
		ins, _ := cl.c.Instances(ctx, "private", "sek")
		for _, in := range ins {
			if in.NodeID != "" {
				b, _ := json.Marshal(ins)
				t.Fatalf("secret group placed on an untrusted node: %s", b)
			}
		}
		// The volunteer cannot trust their own node.
		ns, _ := cl.c.Nodes(ctx)
		if c, _, _ := cl.raw(vol.APIKey, "PATCH", "/v1/nodes/"+ns[0].ID, `{"trusted":true}`, nil); c != http.StatusForbidden {
			t.Fatalf("owner self-trust: %d", c)
		}
		if err := cl.c.Do(ctx, "PATCH", "/v1/nodes/"+ns[0].ID, map[string]bool{"trusted": true}, nil); err != nil {
			t.Fatal(err)
		}
		cl.waitReadyIn("private", "sek", 1)
		_ = cl.c.DeleteGroup(ctx, "private", "sek")
		_ = cl.c.DeleteGroup(ctx, "private", "nosecret")
	})

	t.Run("path gateway on the hub origin is sandboxed", func(t *testing.T) {
		s := cpuSpec("page", 1)
		s.Networking.Auth = false
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		cl.waitReady("page", 1)
		c, _, h := cl.raw("", "GET", "/gw/default/page/health", "", nil)
		if c != 200 || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox allow-scripts") || strings.Contains(h.Get("Content-Security-Policy"), "allow-same-origin") {
			t.Fatalf("gateway CSP: %d %v", c, h)
		}
		_ = cl.c.DeleteGroup(ctx, "default", "page")
	})
}

// waitReadyIn waits for n ready instances of a group in project p.
func (cl *cluster) waitReadyIn(p, group string, n int) {
	cl.t.Helper()
	eventually(cl.t, 30*time.Second, group+" ready", func() (bool, string) {
		ins, _ := cl.c.Instances(cl.ctx, p, group)
		c := 0
		for _, in := range ins {
			if in.Ready {
				c++
			}
		}
		b, _ := json.Marshal(ins)
		return c == n, string(b)
	})
}
