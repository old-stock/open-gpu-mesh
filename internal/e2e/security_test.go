package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/old-stock/open-gpu-mesh/internal/api"
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
	})
}
