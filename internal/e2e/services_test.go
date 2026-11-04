package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
)

func (cl *cluster) raw(key, method, path, body string, hdr map[string]string) (int, string, http.Header) {
	cl.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, cl.url+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cl.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, compactJSON(b), resp.Header
}

// TestServices covers IAM keys + instance identity, KV, schedules, snapshots and metrics.
func TestServices(t *testing.T) {
	cl := newCluster(t)
	cl.addAgent("cpu-a", "")
	cl.addAgent("cpu-b", "")
	ctx := cl.ctx
	if err := cl.c.Do(ctx, "POST", "/v1/projects", map[string]string{"name": "other"}, nil); err != nil {
		t.Fatal(err)
	}

	t.Run("kv", func(t *testing.T) {
		k := cl.key
		if c, b, _ := cl.raw(k, "PUT", "/v1/projects/default/kv/users/alice", `{"age":30}`, map[string]string{"Content-Type": "application/json"}); c != 200 || !strings.Contains(b, `"version":1`) {
			t.Fatalf("put: %d %s", c, b)
		}
		c, b, h := cl.raw(k, "GET", "/v1/projects/default/kv/users/alice", "", nil)
		if c != 200 || b != `{"age":30}` || h.Get("Content-Type") != "application/json" || h.Get("ETag") != `"1"` {
			t.Fatalf("get: %d %s %v", c, b, h)
		}
		if c, _, _ := cl.raw(k, "PUT", "/v1/projects/default/kv/users/alice", "x", map[string]string{"If-Match": "7"}); c != 412 {
			t.Fatalf("CAS with wrong version: %d", c)
		}
		if c, _, _ := cl.raw(k, "PUT", "/v1/projects/default/kv/users/alice", `{"age":31}`, map[string]string{"If-Match": "1"}); c != 200 {
			t.Fatalf("CAS with right version: %d", c)
		}
		if c, _, _ := cl.raw(k, "PUT", "/v1/projects/default/kv/users/alice", "x", map[string]string{"If-None-Match": "*"}); c != 412 {
			t.Fatalf("create-only on existing: %d", c)
		}
		if c, _, _ := cl.raw(k, "GET", "/v1/projects/default/kv/users/alice", "", map[string]string{"If-None-Match": `"2"`}); c != 304 {
			t.Fatalf("conditional get: %d", c)
		}
		for i := 0; i < 5; i++ {
			cl.raw(k, "POST", "/v1/projects/default/kv/stats/hits?by=2", "", nil)
		}
		if _, b, _ := cl.raw(k, "GET", "/v1/projects/default/kv/stats/hits", "", nil); b != "10" {
			t.Fatalf("counter = %q", b)
		}
		cl.raw(k, "PUT", "/v1/projects/default/kv/users/bob/profile", "b", nil)
		cl.raw(k, "PUT", "/v1/projects/default/kv/tmp/x?ttl=1", "gone soon", nil)
		_, b, _ = cl.raw(k, "GET", "/v1/projects/default/kv/users?prefix=b&values=1", "", nil)
		if !strings.Contains(b, `"key":"bob/profile"`) || strings.Contains(b, "alice") || !strings.Contains(b, `"value":"b"`) {
			t.Fatalf("list prefix: %s", b)
		}
		time.Sleep(1100 * time.Millisecond)
		if c, _, _ := cl.raw(k, "GET", "/v1/projects/default/kv/tmp/x", "", nil); c != 404 {
			t.Fatalf("ttl key still there: %d", c)
		}
		_, b, _ = cl.raw(k, "GET", "/v1/projects/default/kv", "", nil)
		if !strings.Contains(b, `"name":"users"`) || !strings.Contains(b, `"name":"stats"`) {
			t.Fatalf("tables: %s", b)
		}
		if c, _, _ := cl.raw(k, "PUT", "/v1/projects/default/kv/big/x", strings.Repeat("a", 1<<20+1), nil); c != 413 {
			t.Fatalf("oversize: %d", c)
		}
		if c, _, _ := cl.raw(k, "DELETE", "/v1/projects/default/kv/users/alice", "", map[string]string{"If-Match": "1"}); c != 412 {
			t.Fatalf("delete CAS: %d", c)
		}
		if c, _, _ := cl.raw(k, "DELETE", "/v1/projects/default/kv/users/alice", "", nil); c != 204 {
			t.Fatalf("delete: %d", c)
		}
	})

	t.Run("scoped keys", func(t *testing.T) {
		var dev, view map[string]any
		if err := cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "ci", "project": "default", "role": "developer"}, &dev); err != nil {
			t.Fatal(err)
		}
		if err := cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "dash", "role": "viewer", "expires_in_hours": 1}, &view); err != nil {
			t.Fatal(err)
		}
		dk, vk := dev["key"].(string), view["key"].(string)
		if !strings.HasPrefix(dk, "ogmk_") {
			t.Fatalf("key = %v", dev)
		}
		if c, _, _ := cl.raw(dk, "PUT", "/v1/projects/default/kv/t/a", "1", nil); c != 200 {
			t.Fatalf("dev key write own project: %d", c)
		}
		if c, _, _ := cl.raw(dk, "GET", "/v1/projects/other/kv", "", nil); c != 403 {
			t.Fatalf("dev key other project: %d", c)
		}
		if c, b, _ := cl.raw(dk, "GET", "/v1/me", "", nil); c != 200 || !strings.Contains(b, `"credential":{"type":"api_key","key_id":"`) || !strings.Contains(b, `"project":"default","role":"developer"`) {
			t.Fatalf("scoped /v1/me: %d %s", c, b)
		}
		if c, b, _ := cl.raw(dk, "GET", "/v1/projects", "", nil); c != 200 || !strings.Contains(b, `"default"`) || strings.Contains(b, `"other"`) {
			t.Fatalf("scoped /v1/projects: %d %s", c, b)
		}
		if c, _, _ := cl.raw(dk, "GET", "/v1/nodes", "", nil); c != 200 {
			t.Fatalf("scoped /v1/nodes: %d", c)
		}
		if c, b, _ := cl.raw(cl.key, "GET", "/v1/me", "", nil); c != 200 || !strings.Contains(b, `"credential":{"type":"user_key","role":"admin"}`) {
			t.Fatalf("user /v1/me: %d %s", c, b)
		}
		if c, _, _ := cl.raw(dk, "GET", "/v1/users", "", nil); c != 403 {
			t.Fatalf("dev key admin-ish global route: %d", c)
		}
		if c, _, _ := cl.raw(dk, "POST", "/v1/invites", `{"uses":1}`, nil); c != 403 {
			t.Fatalf("dev key admin endpoint: %d", c)
		}
		if c, _, _ := cl.raw(vk, "GET", "/v1/projects/other/kv", "", nil); c != 200 {
			t.Fatalf("viewer read any project: %d", c)
		}
		if c, _, _ := cl.raw(vk, "PUT", "/v1/projects/default/kv/t/a", "1", nil); c != 403 {
			t.Fatalf("viewer write: %d", c)
		}
		if c, b, _ := cl.raw(vk, "GET", "/metrics", "", nil); c != 200 || !strings.Contains(b, "ogm_node_up") {
			t.Fatalf("viewer metrics: %d %.200s", c, b)
		}
		if c, _, _ := cl.raw(dk, "POST", "/v1/keys", `{"name":"x","project":"other"}`, nil); c != 403 {
			t.Fatalf("scoped key minting cross-project key: %d", c)
		}
		if c, _, _ := cl.raw(dk, "POST", "/v1/keys", `{"name":"x","role":"admin","project":"default"}`, nil); c != 403 {
			t.Fatalf("escalation: %d", c)
		}
		var list api.List[map[string]any]
		_ = cl.c.Do(ctx, "GET", "/v1/keys", nil, &list)
		if len(list.Items) != 2 || strings.Contains(fmt.Sprint(list.Items), "ogmk_") {
			t.Fatalf("list keys: %+v", list.Items)
		}
		if err := cl.c.Do(ctx, "DELETE", "/v1/keys/"+dev["id"].(string), nil, nil); err != nil {
			t.Fatal(err)
		}
		if c, _, _ := cl.raw(dk, "GET", "/v1/projects/default/kv", "", nil); c != 401 {
			t.Fatalf("revoked key: %d", c)
		}
		// Gateway accepts a project-scoped key for its project only.
		var gk map[string]any
		_ = cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "gw", "project": "other", "role": "viewer"}, &gk)
		s := cpuSpec("gwtest", 1)
		_, _ = cl.c.CreateGroup(ctx, "default", s)
		cl.waitReady("gwtest", 1)
		g, _ := cl.c.Group(ctx, "default", "gwtest")
		req, _ := http.NewRequest("GET", g.GatewayURL, nil)
		req.Header.Set("Authorization", "Bearer "+gk["key"].(string))
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("other-project key on gateway: %d", resp.StatusCode)
		}
		_ = cl.c.DeleteGroup(ctx, "default", "gwtest")
	})

	t.Run("instance identity", func(t *testing.T) {
		s := cpuSpec("ident", 1)
		_, _ = cl.c.CreateGroup(ctx, "default", s)
		cl.waitReady("ident", 1)
		for i := 1; i <= 3; i++ {
			c, b := cl.gw("GET", "ident", "/hub-incr?key=visits", "")
			if c != 200 || !strings.Contains(b, fmt.Sprintf(`"value":%d`, i)) {
				t.Fatalf("hub-incr %d: %d %s", i, c, b)
			}
		}
		_ = cl.c.DeleteGroup(ctx, "default", "ident")
	})

	t.Run("schedules", func(t *testing.T) {
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/queues", map[string]any{"name": "nightly"}, nil); err != nil {
			t.Fatal(err)
		}
		bad := map[string]any{"name": "x", "schedule": "not cron", "job": map[string]any{"queue": "nightly"}}
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/schedules", bad, nil); err == nil {
			t.Fatal("invalid cron accepted")
		}
		sp := map[string]any{"name": "every-minute", "schedule": "* * * * *", "job": map[string]any{"queue": "nightly", "input": map[string]any{"prompt": "hi"}}}
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/schedules", sp, nil); err != nil {
			t.Fatal(err)
		}
		var run map[string]string
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/schedules/every-minute/run", nil, &run); err != nil || !strings.HasPrefix(run["status"], "ok: job") {
			t.Fatalf("run: %v %v", err, run)
		}
		jobs, err := cl.c.Jobs(ctx, "default", "nightly", "", 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs: %v %d", err, len(jobs))
		}
		// Scale action, and the cron loop firing on its own at the next minute boundary.
		_, _ = cl.c.CreateGroup(ctx, "default", cpuSpec("scaled", 0))
		sc := map[string]any{"schedule": "* * * * *", "scale": map[string]any{"group": "scaled", "replicas": 1}}
		if err := cl.c.Do(ctx, "PUT", "/v1/projects/default/schedules/scale-up", sc, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, 75*time.Second, "cron fired the scale schedule", func() (bool, string) {
			g, _ := cl.c.Group(ctx, "default", "scaled")
			return g.DesiredReplicas == 1, fmt.Sprint(g.DesiredReplicas)
		})
		var got map[string]any
		_ = cl.c.Do(ctx, "GET", "/v1/projects/default/schedules/scale-up", nil, &got)
		if got["runs"].(float64) < 1 || got["next_run_time"] == nil {
			t.Fatalf("schedule state: %v", got)
		}
		_ = cl.c.Do(ctx, "DELETE", "/v1/projects/default/schedules/scale-up", nil, nil)
		_ = cl.c.Do(ctx, "DELETE", "/v1/projects/default/schedules/every-minute", nil, nil)
		_ = cl.c.DeleteGroup(ctx, "default", "scaled")
	})

	t.Run("snapshots and migration", func(t *testing.T) {
		s := cpuSpec("store", 1)
		s.Container.Volumes = []api.Volume{{Name: "data", Mount: "/data"}}
		_, _ = cl.c.CreateGroup(ctx, "default", s)
		cl.waitReady("store", 1)
		cl.gw("PUT", "store", "/kv/a", "apple")
		ins, _ := cl.c.Instances(ctx, "default", "store")
		from := ins[0].NodeID
		var sn map[string]any
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/volumes/store/snapshots", map[string]any{"name": "before"}, &sn); err != nil {
			t.Fatal(err)
		}
		id := sn["id"].(string)
		eventually(t, 20*time.Second, "snapshot ready", func() (bool, string) {
			_ = cl.c.Do(ctx, "GET", "/v1/projects/default/snapshots/"+id, nil, &sn)
			return sn["status"] == "ready", fmt.Sprint(sn)
		})
		cl.gw("PUT", "store", "/kv/a", "changed")
		// Restore needs the replica stopped.
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/snapshots/"+id+"/restore", map[string]any{}, nil); err == nil {
			t.Fatal("restore while running accepted")
		}
		_ = cl.c.Do(ctx, "POST", "/v1/projects/default/containers/store/stop", nil, nil)
		eventually(t, 20*time.Second, "stopped", func() (bool, string) {
			ins, _ := cl.c.Instances(ctx, "default", "store")
			return len(ins) == 0, ""
		})
		// Restore onto the OTHER node: a migration.
		nodes, _ := cl.c.Nodes(ctx)
		to := ""
		for _, n := range nodes {
			if n.ID != from {
				to = n.Name
			}
		}
		var rr map[string]any
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/snapshots/"+id+"/restore", map[string]any{"node": to}, &rr); err != nil {
			t.Fatal(err)
		}
		eventually(t, 20*time.Second, "restored", func() (bool, string) {
			_ = cl.c.Do(ctx, "GET", "/v1/projects/default/snapshots/"+id, nil, &sn)
			return sn["restore_status"] == "restored", fmt.Sprint(sn["restore_status"])
		})
		_ = cl.c.Do(ctx, "POST", "/v1/projects/default/containers/store/start", nil, nil)
		cl.waitReady("store", 1)
		ins, _ = cl.c.Instances(ctx, "default", "store")
		if ins[0].NodeID == from {
			t.Fatalf("replica did not move to %s", to)
		}
		if c, b := cl.gw("GET", "store", "/kv/a", ""); c != 200 || b != "apple" {
			t.Fatalf("restored data: %d %q", c, b)
		}
		var list api.List[map[string]any]
		_ = cl.c.Do(ctx, "GET", "/v1/projects/default/snapshots?group=store", nil, &list)
		if len(list.Items) != 1 || list.Items[0]["bytes"].(float64) <= 0 {
			t.Fatalf("snapshot list: %+v", list.Items)
		}
		if err := cl.c.Do(ctx, "DELETE", "/v1/projects/default/snapshots/"+id, nil, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		_, _ = cl.c.CreateGroup(ctx, "default", cpuSpec("met", 1))
		cl.waitReady("met", 1)
		for i := 0; i < 10; i++ {
			cl.gw("GET", "met", "/", "")
		}
		cl.gw("GET", "met", "/env?name=NOPE", "") // 403 from the app
		var m struct {
			Points  []map[string]any `json:"points"`
			Summary map[string]any   `json:"summary"`
		}
		if err := cl.c.Do(ctx, "GET", "/v1/projects/default/containers/met/metrics?minutes=5", nil, &m); err != nil {
			t.Fatal(err)
		}
		if m.Summary["requests"].(float64) != 11 || m.Summary["errors_4xx"].(float64) != 1 || m.Summary["p50_ms"].(float64) <= 0 {
			t.Fatalf("summary: %v", m.Summary)
		}
		c, b, _ := cl.raw(cl.key, "GET", "/metrics", "", nil)
		for _, want := range []string{`ogm_gateway_requests_total{project="default",group="met",code="200"} 10`,
			`ogm_gateway_request_duration_seconds_count{project="default",group="met"} 11`, `ogm_group_desired_replicas{project="default",group="met"} 1`} {
			if c != 200 || !strings.Contains(b, want) {
				t.Fatalf("prometheus missing %q", want)
			}
		}
		var dk map[string]any
		_ = cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "p", "project": "default"}, &dk)
		if c, _, _ := cl.raw(dk["key"].(string), "GET", "/metrics", "", nil); c != 403 {
			t.Fatalf("project key on /metrics: %d", c)
		}
	})
	t.Run("custom domains", func(t *testing.T) {
		s := cpuSpec("site", 1)
		s.Networking.Auth = false
		s.Networking.CustomDomains = []string{"App.Example.COM"}
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		dup := cpuSpec("site2", 1)
		dup.Networking.CustomDomains = []string{"app.example.com"}
		if _, err := cl.c.CreateGroup(ctx, "default", dup); err == nil || !strings.Contains(fmt.Sprintf("%+v", err.(*api.Problem).Errors), "already used") {
			t.Fatalf("duplicate domain: %v", err)
		}
		cl.waitReady("site", 1)
		time.Sleep(5100 * time.Millisecond) // domain cache
		req, _ := http.NewRequest("GET", cl.url+"/", nil)
		req.Host = "app.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(compactJSON(b), `"group":"site"`) {
			t.Fatalf("custom domain: %d %s", resp.StatusCode, b)
		}
		_ = cl.c.DeleteGroup(ctx, "default", "site")
	})
	t.Run("replicas default to 1", func(t *testing.T) {
		c, b, _ := cl.raw(cl.key, "PUT", "/v1/projects/default/containers/defrep", `{"container":{"image":"x/y:1"},"autostart_policy":false}`, nil)
		if c != 201 || !strings.Contains(b, `"replicas":1`) {
			t.Fatalf("omitted replicas: %d %s", c, b)
		}
		c, b, _ = cl.raw(cl.key, "PUT", "/v1/projects/default/containers/defrep0", `{"replicas":0,"container":{"image":"x/y:1"},"autostart_policy":false}`, nil)
		if c != 201 || !strings.Contains(b, `"replicas":0`) {
			t.Fatalf("explicit 0: %d %s", c, b)
		}
		_ = cl.c.DeleteGroup(ctx, "default", "defrep")
		_ = cl.c.DeleteGroup(ctx, "default", "defrep0")
	})

	t.Run("quotas", func(t *testing.T) {
		if err := cl.c.Do(ctx, "POST", "/v1/projects", map[string]string{"name": "quota"}, nil); err != nil {
			t.Fatal(err)
		}
		var dev map[string]any
		if err := cl.c.Do(ctx, "POST", "/v1/keys", map[string]any{"name": "q", "project": "quota", "role": "developer"}, &dev); err != nil {
			t.Fatal(err)
		}
		if c, _, _ := cl.raw(dev["key"].(string), "PUT", "/v1/projects/quota/quota", `{"cpu":100}`, nil); c != 403 {
			t.Fatalf("non-admin set quota: %d", c)
		}
		if c, b, _ := cl.raw(cl.key, "PUT", "/v1/projects/quota/quota", `{"cpu":3,"instances":3,"storage_mb":1}`, nil); c != 200 || !strings.Contains(b, `"cpu":3`) {
			t.Fatalf("set quota: %d %s", c, b)
		}
		if _, err := cl.c.CreateGroup(ctx, "quota", cpuSpec("qa", 2)); err != nil {
			t.Fatalf("%v %+v", err, err.(*api.Problem).Errors)
		}
		isQuota := func(err error) bool {
			p, ok := err.(*api.Problem)
			return ok && p.Status == 403 && p.Title == "quota exceeded"
		}
		if _, err := cl.c.CreateGroup(ctx, "quota", cpuSpec("qb", 2)); !isQuota(err) {
			t.Fatalf("over cpu quota: %v", err)
		}
		auto := cpuSpec("qc", 0)
		auto.HTTPAutoscaler = &api.HTTPAutoscaler{MinReplicas: 0, MaxReplicas: 5, TargetConcurrency: 1}
		if _, err := cl.c.CreateGroup(ctx, "quota", auto); !isQuota(err) {
			t.Fatalf("autoscaler max over quota: %v", err)
		}
		if _, err := cl.c.CreateGroup(ctx, "quota", cpuSpec("qd", 1)); err != nil {
			t.Fatalf("within quota: %v", err)
		}
		if c, _, _ := cl.raw(cl.key, "PATCH", "/v1/projects/quota/containers/qa", `{"replicas":3}`, map[string]string{"Content-Type": "application/merge-patch+json"}); c != 403 {
			t.Fatalf("scale over quota: %d", c)
		}
		var sched map[string]any
		_ = cl.c.Do(ctx, "POST", "/v1/projects/quota/schedules", map[string]any{"name": "big", "schedule": "0 0 1 1 *", "scale": map[string]any{"group": "qd", "replicas": 4}}, nil)
		_ = cl.c.Do(ctx, "POST", "/v1/projects/quota/schedules/big/run", nil, &sched)
		_ = cl.c.Do(ctx, "GET", "/v1/projects/quota/schedules/big", nil, &sched)
		if !strings.Contains(fmt.Sprint(sched["last_status"]), "quota exceeded") {
			t.Fatalf("schedule over quota: %v", sched)
		}
		if c, _, _ := cl.raw(cl.key, "PUT", "/v1/projects/quota/files/a.bin", strings.Repeat("x", 600<<10), nil); c != 201 {
			t.Fatalf("upload within storage quota: %d", c)
		}
		if c, b, _ := cl.raw(cl.key, "PUT", "/v1/projects/quota/files/b.bin", strings.Repeat("x", 600<<10), nil); c != 403 {
			t.Fatalf("upload over storage quota: %d %s", c, b)
		}
		if c, _, _ := cl.raw(cl.key, "PUT", "/v1/projects/quota/files/a.bin", strings.Repeat("y", 900<<10), nil); c != 201 {
			t.Fatalf("overwrite frees old size: %d", c)
		}
		c, b, _ := cl.raw(cl.key, "GET", "/v1/projects/quota/quota", "", nil)
		if c != 200 || !strings.Contains(b, `"usage":{"cpu":3,"memory_mb":768,"gpus":0,"instances":3,"storage_bytes":921600}`) {
			t.Fatalf("usage: %d %s", c, b)
		}
		for _, g := range []string{"qa", "qd"} {
			_ = cl.c.DeleteGroup(ctx, "quota", g)
		}
	})
	_ = client.New
	_ = json.Marshal
}

// compactJSON strips insignificant whitespace from JSON bodies (the API pretty-prints).
func compactJSON(b []byte) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) == nil {
		return buf.String()
	}
	return string(b)
}
