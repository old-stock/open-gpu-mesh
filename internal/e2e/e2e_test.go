package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/old-stock/open-gpu-mesh/internal/agent"
	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
	"github.com/old-stock/open-gpu-mesh/internal/demoapp"
	"github.com/old-stock/open-gpu-mesh/internal/server"
)

// The process runtime re-executes this test binary with "demo-app".
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "demo-app" {
		if err := demoapp.Run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type cluster struct {
	t      *testing.T
	srv    *server.Server
	url    string
	key    string
	c      *client.Client
	ctx    context.Context
	cancel context.CancelFunc
	agents map[string]context.CancelFunc
}

func quietLogger() *slog.Logger {
	if os.Getenv("OGM_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + ln.Addr().String()
	srv, err := server.New(server.Config{
		DataDir: t.TempDir(), PublicURL: u, ReconcileInterval: 200 * time.Millisecond, OfflineGrace: 2 * time.Second,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := srv.Bootstrap("admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, ln) }()
	cl := &cluster{t: t, srv: srv, url: u, key: key, c: client.New(u, key), ctx: ctx, cancel: cancel, agents: map[string]context.CancelFunc{}}
	t.Cleanup(func() {
		for _, c := range cl.agents {
			c()
		}
		cancel()
		time.Sleep(100 * time.Millisecond)
		srv.Close()
	})
	return cl
}

func (cl *cluster) addAgent(name, gpus string) {
	cl.t.Helper()
	tok, err := cl.c.CreateNode(cl.ctx, name)
	if err != nil {
		cl.t.Fatal(err)
	}
	exe, _ := os.Executable()
	a, err := agent.New(agent.Config{
		Server: cl.url, Token: tok.Token, Name: name, Runtime: &agent.ProcessRuntime{SelfExe: exe},
		GPUs: agent.ParseFakeGPUs(gpus), HeartbeatEvery: 500 * time.Millisecond, StopGrace: 2 * time.Second,
		Country: "us", Logger: quietLogger(),
	})
	if err != nil {
		cl.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(cl.ctx)
	cl.agents[name] = cancel
	go func() { _ = a.Run(ctx) }()
}

func (cl *cluster) killAgent(name string) {
	cl.agents[name]()
	delete(cl.agents, name)
}

func eventually(t *testing.T, timeout time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, msg := f()
		if ok {
			return
		}
		last = msg
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (last: %s)", what, last)
}

func (cl *cluster) readyCount(group string) (int, string) {
	g, err := cl.c.Group(cl.ctx, "default", group)
	if err != nil {
		return 0, err.Error()
	}
	b, _ := json.Marshal(g.CurrentState)
	return g.CurrentState.InstanceStatusCounts.Ready, string(b)
}

func demoSpec(name string, replicas int) api.ContainerGroupSpec {
	return api.ContainerGroupSpec{
		Name: name, Replicas: replicas, Priority: "medium",
		Container:      api.ContainerSpec{Image: "ogm/demo", Resources: api.Resources{CPU: 1, MemoryMB: 512, GPUCount: 1}},
		Networking:     &api.Networking{Port: 8080, Auth: true},
		ReadinessProbe: &api.Probe{HTTP: &api.HTTPProbe{Path: "/health", Port: 8080}, PeriodSeconds: 1},
	}
}

func TestEndToEnd(t *testing.T) {
	cl := newCluster(t)
	ctx := cl.ctx
	cl.addAgent("alice-4090", "RTX 4090:24564")
	cl.addAgent("bob-3090", "RTX 3090:24576")
	eventually(t, 10*time.Second, "nodes online", func() (bool, string) {
		ns, _ := cl.c.Nodes(ctx)
		on := 0
		for _, n := range ns {
			if n.Status == "online" {
				on++
			}
		}
		return on == 2, fmt.Sprintf("%d online", on)
	})

	if _, err := cl.c.CreateQueue(ctx, "default", "jobs", api.QueueSettings{RetryBackoffBaseS: 1, RetryBackoffMaxS: 2}); err != nil {
		t.Fatal(err)
	}
	spec := demoSpec("demo", 2)
	spec.QueueConnection = &api.QueueConnection{QueueName: "jobs", Port: 8080, Path: "/generate"}
	g, err := cl.c.CreateGroup(ctx, "default", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(g.GatewayURL, "/gw/default/demo/") {
		t.Fatalf("gateway url = %q", g.GatewayURL)
	}
	eventually(t, 20*time.Second, "2 ready instances", func() (bool, string) {
		n, s := cl.readyCount("demo")
		return n == 2, s
	})

	t.Run("gateway auth and load balancing", func(t *testing.T) {
		resp, err := http.Get(g.GatewayURL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("unauthenticated status = %d", resp.StatusCode)
		}
		seen := map[string]int{}
		for i := 0; i < 20; i++ {
			req, _ := http.NewRequest("GET", g.GatewayURL, nil)
			req.Header.Set("OGM-Api-Key", cl.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %v", resp.StatusCode, body)
			}
			if resp.Header.Get("X-OGM-Instance-Id") != body["instance_id"] {
				t.Fatalf("instance header mismatch")
			}
			seen[body["instance_id"].(string)]++
		}
		if len(seen) != 2 {
			t.Fatalf("expected traffic on both instances, got %v", seen)
		}
	})

	t.Run("shareable gateway token and rotation", func(t *testing.T) {
		gg, _ := cl.c.Group(ctx, "default", "demo")
		if !strings.HasPrefix(gg.GatewayToken, "ogmg_") {
			t.Fatalf("no gateway token: %q", gg.GatewayToken)
		}
		status := func(tok string) int {
			req, _ := http.NewRequest("GET", g.GatewayURL, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		if c := status(gg.GatewayToken); c != 200 {
			t.Fatalf("gateway token rejected: %d", c)
		}
		var rotated api.ContainerGroup
		if err := cl.c.Do(ctx, "POST", "/v1/projects/default/containers/demo/rotate-gateway-token", nil, &rotated); err != nil {
			t.Fatal(err)
		}
		if c := status(gg.GatewayToken); c != 401 {
			t.Fatalf("old token still accepted: %d", c)
		}
		if c := status(rotated.GatewayToken); c != 200 {
			t.Fatalf("new token rejected: %d", c)
		}
		// The token is scoped to the gateway: it must not work on the management API.
		req, _ := http.NewRequest("GET", cl.url+"/v1/me", nil)
		req.Header.Set("OGM-Api-Key", rotated.GatewayToken)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("gateway token accepted by API: %d", resp.StatusCode)
		}
	})

	t.Run("gateway streaming", func(t *testing.T) {
		req, _ := http.NewRequest("GET", g.GatewayURL+"stream?n=3", nil)
		req.Header.Set("Authorization", "Bearer "+cl.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		n := 0
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: token") {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("got %d SSE tokens", n)
		}
	})

	t.Run("gateway websocket with query token", func(t *testing.T) {
		wsURL := strings.Replace(g.GatewayURL, "http://", "ws://", 1) + "ws?ogm_token=" + url.QueryEscape(cl.key)
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		if err := c.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
			t.Fatal(err)
		}
		_, b, err := c.Read(ctx)
		if err != nil || string(b) != "echo: hi" {
			t.Fatalf("ws echo = %q, %v", b, err)
		}
	})

	t.Run("imds metadata from inside the instance", func(t *testing.T) {
		req, _ := http.NewRequest("GET", g.GatewayURL+"imds", nil)
		req.Header.Set("OGM-Api-Key", cl.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var md map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&md)
		if md["container_group"] != "demo" || md["project"] != "default" {
			t.Fatalf("imds metadata = %v", md)
		}
	})

	t.Run("job queue batch", func(t *testing.T) {
		var jps []api.JobPrototype
		for i := 0; i < 12; i++ {
			jps = append(jps, api.JobPrototype{Input: json.RawMessage(fmt.Sprintf(`{"prompt":"cat %d","sleep_ms":50}`, i))})
		}
		jobs, err := cl.c.SubmitJobs(ctx, "default", "jobs", jps)
		if err != nil || len(jobs) != 12 {
			t.Fatalf("submit: %v %d", err, len(jobs))
		}
		instances := map[string]bool{}
		for _, j := range jobs {
			got, err := cl.c.Job(ctx, "default", "jobs", j.ID, 30)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != api.JobSucceeded {
				t.Fatalf("job %s status %s err %s", got.ID, got.Status, got.Error)
			}
			var out map[string]any
			_ = json.Unmarshal(got.Output, &out)
			if out["image_sha256"] == "" || out["instance_id"] == nil {
				t.Fatalf("bad output %s", got.Output)
			}
			instances[out["instance_id"].(string)] = true
		}
		if len(instances) != 2 {
			t.Fatalf("jobs ran on %d instances", len(instances))
		}
	})

	t.Run("job retries with backoff on a different instance then fails", func(t *testing.T) {
		j, err := cl.c.SubmitJob(ctx, "default", "jobs", api.JobPrototype{Input: json.RawMessage(`{"fail":true,"sleep_ms":10}`), MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		got, err := cl.c.Job(ctx, "default", "jobs", j.ID, 30)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != api.JobFailed || got.Attempts != 3 {
			t.Fatalf("status=%s attempts=%d", got.Status, got.Attempts)
		}
		var insts []string
		for _, e := range got.Events {
			if e.Action == "started" {
				insts = append(insts, e.InstanceID)
			}
		}
		if len(insts) != 3 || insts[0] == insts[1] {
			t.Fatalf("expected retry on a different instance, got %v", insts)
		}
		if !strings.Contains(got.Error, "HTTP 500") {
			t.Fatalf("error = %q", got.Error)
		}
	})

	t.Run("4xx is not retried", func(t *testing.T) {
		j, _ := cl.c.SubmitJob(ctx, "default", "jobs", api.JobPrototype{Input: json.RawMessage(`{"fail":true,"status":422,"sleep_ms":10}`)})
		got, _ := cl.c.Job(ctx, "default", "jobs", j.ID, 30)
		if got.Status != api.JobFailed || got.Attempts != 1 {
			t.Fatalf("status=%s attempts=%d", got.Status, got.Attempts)
		}
	})

	t.Run("cancel running job aborts it", func(t *testing.T) {
		j, _ := cl.c.SubmitJob(ctx, "default", "jobs", api.JobPrototype{Input: json.RawMessage(`{"sleep_ms":20000}`)})
		eventually(t, 10*time.Second, "job running", func() (bool, string) {
			got, _ := cl.c.Job(ctx, "default", "jobs", j.ID, 0)
			return got.Status == api.JobRunning, got.Status
		})
		if err := cl.c.CancelJob(ctx, "default", "jobs", j.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := cl.c.Job(ctx, "default", "jobs", j.ID, 0)
		if got.Status != api.JobCancelled {
			t.Fatalf("status = %s", got.Status)
		}
		// The demo app logs the cancellation when the request context is aborted.
		eventually(t, 10*time.Second, "cancellation reached the app", func() (bool, string) {
			logs, _ := cl.c.Logs(ctx, "default", "demo", url.Values{"q": {"cancelled"}})
			return len(logs) > 0, fmt.Sprint(len(logs))
		})
	})

	t.Run("logs and events", func(t *testing.T) {
		logs, err := cl.c.Logs(ctx, "default", "demo", url.Values{"q": {"generated image"}})
		if err != nil || len(logs) == 0 {
			t.Fatalf("logs: %v (%d)", err, len(logs))
		}
		evs, _ := cl.c.Events(ctx, "default", "demo", 200)
		kinds := map[string]bool{}
		for _, e := range evs {
			kinds[e.Kind] = true
		}
		for _, k := range []string{"group.created", "instance.allocated", "instance.running", "instance.ready"} {
			if !kinds[k] {
				t.Fatalf("missing event %s in %v", k, kinds)
			}
		}
	})

	t.Run("node loss reschedules onto new capacity", func(t *testing.T) {
		insts, _ := cl.c.Instances(ctx, "default", "demo")
		var victim string
		for _, in := range insts {
			if in.NodeName == "bob-3090" {
				victim = in.ID
			}
		}
		if victim == "" {
			t.Fatal("no instance on bob")
		}
		cl.killAgent("bob-3090")
		eventually(t, 15*time.Second, "replacement waiting for capacity", func() (bool, string) {
			g, _ := cl.c.Group(ctx, "default", "demo")
			return g.CurrentState.InstanceStatusCounts.Allocating == 1, g.CurrentState.Description
		})
		cl.addAgent("carol-a100", "NVIDIA A100-SXM4-80GB:81920")
		eventually(t, 20*time.Second, "back to 2 ready", func() (bool, string) {
			n, s := cl.readyCount("demo")
			return n == 2, s
		})
		insts, _ = cl.c.Instances(ctx, "default", "demo")
		for _, in := range insts {
			if in.ID == victim {
				t.Fatalf("victim instance still live: %+v", in)
			}
		}
		idx := map[int]bool{}
		for _, in := range insts {
			idx[in.ReplicaIndex] = true
		}
		if !idx[0] || !idx[1] {
			t.Fatalf("replacement should reuse the freed replica index; got %v", idx)
		}
	})

	t.Run("scale down respects deletion cost", func(t *testing.T) {
		insts, _ := cl.c.Instances(ctx, "default", "demo")
		keep := insts[0].ID
		if err := cl.c.Do(ctx, "PATCH", "/v1/projects/default/containers/demo/instances/"+keep, map[string]int{"deletion_cost": 100}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.c.PatchGroup(ctx, "default", "demo", map[string]any{"replicas": 1}); err != nil {
			t.Fatal(err)
		}
		eventually(t, 15*time.Second, "one instance left", func() (bool, string) {
			insts, _ := cl.c.Instances(ctx, "default", "demo")
			return len(insts) == 1 && insts[0].ID == keep, fmt.Sprint(len(insts))
		})
	})

	t.Run("rolling update bumps version", func(t *testing.T) {
		g, err := cl.c.PatchGroup(ctx, "default", "demo", map[string]any{"container": map[string]any{"env": map[string]string{"NEW": "1"}}})
		if err != nil {
			t.Fatal(err)
		}
		if g.Version != 2 {
			t.Fatalf("version = %d", g.Version)
		}
		eventually(t, 20*time.Second, "instance on v2 and ready", func() (bool, string) {
			insts, _ := cl.c.Instances(ctx, "default", "demo")
			return len(insts) == 1 && insts[0].Version == 2 && insts[0].Ready, fmt.Sprintf("%+v", insts)
		})
		// Gateway setting changes do not bump the version.
		g, _ = cl.c.PatchGroup(ctx, "default", "demo", map[string]any{"networking": map[string]any{"load_balancer": "round_robin"}})
		if g.Version != 2 {
			t.Fatalf("networking change bumped version to %d", g.Version)
		}
	})

	t.Run("stop and start", func(t *testing.T) {
		if err := cl.c.GroupAction(ctx, "default", "demo", "stop"); err != nil {
			t.Fatal(err)
		}
		eventually(t, 15*time.Second, "no instances", func() (bool, string) {
			insts, _ := cl.c.Instances(ctx, "default", "demo")
			return len(insts) == 0, fmt.Sprint(len(insts))
		})
		req, _ := http.NewRequest("GET", g.GatewayURL, nil)
		req.Header.Set("OGM-Api-Key", cl.key)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Fatalf("stopped gateway status %d", resp.StatusCode)
		}
		if err := cl.c.GroupAction(ctx, "default", "demo", "start"); err != nil {
			t.Fatal(err)
		}
		eventually(t, 20*time.Second, "ready again", func() (bool, string) {
			n, s := cl.readyCount("demo")
			return n == 1, s
		})
	})
}

func TestQueueAutoscalerAndPriority(t *testing.T) {
	cl := newCluster(t)
	ctx := cl.ctx
	cl.addAgent("n1", "RTX 4090:24564")
	cl.addAgent("n2", "RTX 4090:24564")
	eventually(t, 10*time.Second, "nodes online", func() (bool, string) {
		ns, _ := cl.c.Nodes(ctx)
		return len(ns) == 2 && ns[0].Status == "online" && ns[1].Status == "online", ""
	})
	if _, err := cl.c.CreateQueue(ctx, "default", "work", api.QueueSettings{}); err != nil {
		t.Fatal(err)
	}
	spec := demoSpec("batchy", 0)
	spec.Priority = "batch"
	spec.Networking = nil
	spec.QueueConnection = &api.QueueConnection{QueueName: "work", Port: 8080, Path: "/generate"}
	spec.QueueAutoscaler = &api.QueueAutoscaler{MinReplicas: 0, MaxReplicas: 2, DesiredQueueLength: 2, PollingPeriod: 5, MaxUpscalePerMinute: 60, MaxDownscalePerMinute: 60}
	if _, err := cl.c.CreateGroup(ctx, "default", spec); err != nil {
		t.Fatal(err)
	}
	// Scale from zero when jobs arrive.
	var jps []api.JobPrototype
	for i := 0; i < 6; i++ {
		jps = append(jps, api.JobPrototype{Input: json.RawMessage(`{"sleep_ms":1500}`)})
	}
	jobs, err := cl.c.SubmitJobs(ctx, "default", "work", jps)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "autoscaled to 2 ready", func() (bool, string) {
		n, s := cl.readyCount("batchy")
		return n == 2, s
	})

	// A high priority group preempts a batch instance.
	hp := demoSpec("urgent", 1)
	hp.Priority = "high"
	if _, err := cl.c.CreateGroup(ctx, "default", hp); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "urgent ready via preemption", func() (bool, string) {
		n, s := cl.readyCount("urgent")
		return n == 1, s
	})
	evs, _ := cl.c.Events(ctx, "default", "batchy", 200)
	found := false
	for _, e := range evs {
		if strings.Contains(e.Message, "Interrupted (Priority)") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a priority interruption event")
	}
	// All jobs still complete despite the preemption.
	for _, j := range jobs {
		got, err := cl.c.Job(ctx, "default", "work", j.ID, 60)
		if err != nil || got.Status != api.JobSucceeded {
			t.Fatalf("job %s: %v %s %s", j.ID, err, got.Status, got.Error)
		}
	}
	// Queue drained -> scale back to zero.
	eventually(t, 30*time.Second, "scaled to zero", func() (bool, string) {
		g, _ := cl.c.Group(ctx, "default", "batchy")
		return g.DesiredReplicas == 0, fmt.Sprint(g.DesiredReplicas)
	})
}
