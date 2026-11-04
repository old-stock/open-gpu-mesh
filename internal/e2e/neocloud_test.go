package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// cpuSpec is a CPU-only demo group (no GPU requested).
func cpuSpec(name string, replicas int) api.ContainerGroupSpec {
	s := demoSpec(name, replicas)
	s.Container.Resources = api.Resources{CPU: 1, MemoryMB: 256}
	return s
}

func (cl *cluster) gw(method, group, path, body string) (int, string) {
	cl.t.Helper()
	g, err := cl.c.Group(cl.ctx, "default", group)
	if err != nil {
		cl.t.Fatal(err)
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, strings.TrimSuffix(g.GatewayURL, "/")+path, rd)
	req.Header.Set("OGM-Api-Key", cl.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, compactJSON(b)
}

func (cl *cluster) waitReady(group string, n int) {
	cl.t.Helper()
	eventually(cl.t, 30*time.Second, group+" ready", func() (bool, string) {
		c, msg := cl.readyCount(group)
		return c == n, msg
	})
}

// TestNeocloud covers the container-deployment building blocks on CPU-only nodes: secrets,
// links between groups, persistent volumes, HTTP autoscaling from zero and max_concurrency.
func TestNeocloud(t *testing.T) {
	cl := newCluster(t)
	cl.addAgent("cpu-a", "")
	cl.addAgent("cpu-b", "")
	ctx := cl.ctx

	t.Run("secrets", func(t *testing.T) {
		s := cpuSpec("api", 1)
		s.Container.Env = map[string]string{"DEMO_TOKEN": "Bearer {{secret:token}}"}
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, "instance waiting for secret", func() (bool, string) {
			ins, _ := cl.c.Instances(ctx, "default", "api")
			b, _ := json.Marshal(ins)
			return len(ins) == 1 && strings.Contains(ins[0].Message, `secret "token" not found`), string(b)
		})
		if err := cl.c.Do(ctx, "PUT", "/v1/projects/default/secrets/token", map[string]string{"value": "s3cr3t"}, nil); err != nil {
			t.Fatal(err)
		}
		cl.waitReady("api", 1)
		if code, body := cl.gw("GET", "api", "/env?name=DEMO_TOKEN", ""); code != 200 || !strings.Contains(body, `"Bearer s3cr3t"`) {
			t.Fatalf("env: %d %s", code, body)
		}
		var list api.List[map[string]any]
		if err := cl.c.Do(ctx, "GET", "/v1/projects/default/secrets", nil, &list); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(list)
		if len(list.Items) != 1 || strings.Contains(string(b), "s3cr3t") {
			t.Fatalf("secret list leaks or wrong: %s", b)
		}
		// The secret is not readable through the group spec either.
		g, _ := cl.c.Group(ctx, "default", "api")
		if gb, _ := json.Marshal(g); strings.Contains(string(gb), "s3cr3t") {
			t.Fatal("group API leaks secret value")
		}
	})

	t.Run("links", func(t *testing.T) {
		s := cpuSpec("web", 1)
		s.Container.Env = map[string]string{"DEMO_API": "{{link:api:8080}}", "DEMO_API_HOST": "{{link_host:api:8080}}"}
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		cl.waitReady("web", 1)
		code, body := cl.gw("GET", "web", "/fetch?env=DEMO_API&path=/", "")
		if code != 200 || !strings.Contains(body, `"group":"api"`) {
			t.Fatalf("link fetch: %d %s", code, body)
		}
		if code, body := cl.gw("GET", "web", "/env?name=DEMO_API_HOST", ""); code != 200 || !strings.Contains(body, "127.0.0.1") {
			t.Fatalf("link_host: %d %s", code, body)
		}
		// A group cannot reach ports it did not declare a link to.
		s2 := cpuSpec("nolink", 1)
		s2.Container.Env = map[string]string{"DEMO_X": "{{link:api:9999}}"}
		if _, err := cl.c.CreateGroup(ctx, "default", s2); err != nil {
			t.Fatal(err)
		}
		cl.waitReady("nolink", 1)
		start := time.Now()
		if code, _ := cl.gw("GET", "nolink", "/fetch?env=DEMO_X&path=/", ""); code == 200 {
			t.Fatal("unexposed port reachable")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("refusing an unexposed port took %s", d)
		}
		_ = cl.c.DeleteGroup(ctx, "default", "nolink")
	})

	t.Run("volumes", func(t *testing.T) {
		s := cpuSpec("db", 1)
		s.Container.Volumes = []api.Volume{{Name: "data", Mount: "/data"}}
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		cl.waitReady("db", 1)
		if code, body := cl.gw("PUT", "db", "/kv/hello", "world"); code != 204 {
			t.Fatalf("kv put: %d %s", code, body)
		}
		ins, _ := cl.c.Instances(ctx, "default", "db")
		node := ins[0].NodeID
		// Stop and start (3x, two nodes): the replacement must land on the volume's node.
		for cycle := 0; cycle < 3; cycle++ {
			for _, a := range []string{"stop", "start"} {
				if err := cl.c.Do(ctx, "POST", "/v1/projects/default/containers/db/"+a, nil, nil); err != nil {
					t.Fatal(err)
				}
				if a == "stop" {
					eventually(t, 20*time.Second, "db stopped", func() (bool, string) {
						ins, _ := cl.c.Instances(ctx, "default", "db")
						return len(ins) == 0, fmt.Sprint(len(ins))
					})
				}
			}
			cl.waitReady("db", 1)
			ins, _ = cl.c.Instances(ctx, "default", "db")
			if ins[0].NodeID != node {
				t.Fatalf("cycle %d: replica moved off its volume node: %s -> %s", cycle, node, ins[0].NodeID)
			}
		}
		if code, body := cl.gw("GET", "db", "/kv/hello", ""); code != 200 || body != "world" {
			t.Fatalf("data lost: %d %q", code, body)
		}
		var vols api.List[map[string]any]
		if err := cl.c.Do(ctx, "GET", "/v1/projects/default/volumes", nil, &vols); err != nil || len(vols.Items) != 1 {
			t.Fatalf("volumes ls: %v %+v", err, vols)
		}
		if err := cl.c.Do(ctx, "DELETE", "/v1/projects/default/volumes/db", nil, nil); err == nil || !strings.Contains(err.Error(), "in use") {
			t.Fatalf("delete in-use volume: %v", err)
		}
		if err := cl.c.DeleteGroup(ctx, "default", "db"); err != nil {
			t.Fatal(err)
		}
		eventually(t, 20*time.Second, "volume deletable", func() (bool, string) {
			err := cl.c.Do(ctx, "DELETE", "/v1/projects/default/volumes/db", nil, nil)
			return err == nil, fmt.Sprint(err)
		})
	})

	t.Run("http autoscaler from zero", func(t *testing.T) {
		s := cpuSpec("auto", 0)
		s.HTTPAutoscaler = &api.HTTPAutoscaler{MinReplicas: 0, MaxReplicas: 2, TargetConcurrency: 1, StableWindowSeconds: 6, ScaleDownDelaySeconds: 2}
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		code, body := cl.gw("GET", "auto", "/", "")
		if code != 200 || !strings.Contains(body, `"group":"auto"`) {
			t.Fatalf("cold request: %d %s", code, body)
		}
		t.Logf("cold start served in %s", time.Since(start).Round(time.Millisecond))
		eventually(t, 30*time.Second, "scale back to zero", func() (bool, string) {
			g, _ := cl.c.Group(ctx, "default", "auto")
			return g.DesiredReplicas == 0 && g.CurrentState.InstanceStatusCounts.Running == 0, fmt.Sprintf("%d replicas, %+v", g.DesiredReplicas, g.CurrentState.InstanceStatusCounts)
		})
	})

	t.Run("max concurrency", func(t *testing.T) {
		s := cpuSpec("mc", 1)
		s.Networking.MaxConcurrency = 1
		if _, err := cl.c.CreateGroup(ctx, "default", s); err != nil {
			t.Fatal(err)
		}
		cl.waitReady("mc", 1)
		start := time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if code, body := cl.gw("POST", "mc", "/generate", `{"sleep_ms":400}`); code != 200 {
					t.Errorf("request: %d %s", code, body)
				}
			}()
		}
		wg.Wait()
		if d := time.Since(start); d < 1200*time.Millisecond {
			t.Fatalf("3 requests with max_concurrency=1 finished in %s; they ran in parallel", d)
		}
	})
}
