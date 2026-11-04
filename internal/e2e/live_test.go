package e2e

// TestLive exercises a REAL cluster (real Docker, real machines, mixed architectures).
// It is skipped unless OGM_LIVE_SERVER and OGM_LIVE_KEY are set. Agents must be started with
// `--label host=<name>` and, for Docker agents, `--label runtime=docker`.
//
//	OGM_LIVE_SERVER=http://hub:7790 OGM_LIVE_KEY=ogm_... \
//	OGM_LIVE_HOSTS=a,b,c            # host labels of Docker agents (all must be online)
//	OGM_LIVE_PROCESS_HOST=d         # optional: host label of a process-runtime agent
//	OGM_LIVE_GPU_HOST=c             # optional: Docker host with a usable NVIDIA GPU
//	OGM_LIVE_IMAGES=registry.ogm/default   # where demo:2 and postgres:17-alpine were pushed
//	HTTP_PROXY=socks5://127.0.0.1:1055     # if the hub is only reachable through a proxy
//	go test ./internal/e2e -run TestLive -v -timeout 60m

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
)

type live struct {
	t        *testing.T
	url, key string
	c        *client.Client
	ctx      context.Context
	images   string
	report   *strings.Builder
}

// run runs a subtest with l bound to it, so resources deployed inside are
// cleaned up when the subtest ends (nodes have little capacity to spare).
func (l *live) run(name string, fn func(t *testing.T)) {
	parent := l.t
	parent.Run(name, func(t *testing.T) {
		l.t = t
		defer func() { l.t = parent }()
		fn(t)
	})
}

func (l *live) logf(f string, a ...any) {
	l.t.Helper()
	msg := fmt.Sprintf(f, a...)
	l.t.Log(msg)
	fmt.Fprintf(l.report, "- %s\n", msg)
}

func (l *live) do(method, path string, body, out any) error {
	return l.c.Do(l.ctx, method, path, body, out)
}

func (l *live) must(err error) {
	l.t.Helper()
	if err != nil {
		l.t.Fatal(err)
	}
}

func (l *live) http(method, url, key, body string, hdr map[string]string) (int, string, time.Duration) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(l.ctx, method, url, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error(), time.Since(start)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, compactJSON(b), time.Since(start)
}

func (l *live) gw(group, method, path, body string) (int, string, time.Duration) {
	return l.http(method, l.url+"/gw/default/"+group+path, l.key, body, nil)
}

func (l *live) deploy(s api.ContainerGroupSpec) {
	l.t.Helper()
	_ = l.c.DeleteGroup(l.ctx, "default", s.Name)
	l.waitGone(s.Name)
	_, err := l.c.Deploy(l.ctx, "default", s)
	l.must(err)
	l.t.Cleanup(func() { _ = l.c.DeleteGroup(context.Background(), "default", s.Name) })
}

func (l *live) waitGone(group string) {
	l.t.Helper()
	eventually(l.t, 2*time.Minute, group+" gone", func() (bool, string) {
		ins, err := l.c.Instances(l.ctx, "default", group)
		return err != nil || len(ins) == 0, fmt.Sprint(len(ins))
	})
}

func (l *live) waitReady(group string, n int, timeout time.Duration) []api.Instance {
	l.t.Helper()
	var ins []api.Instance
	eventually(l.t, timeout, fmt.Sprintf("%s: %d ready", group, n), func() (bool, string) {
		ins, _ = l.c.Instances(l.ctx, "default", group)
		ready := 0
		var msgs []string
		for _, in := range ins {
			if in.Ready {
				ready++
			}
			msgs = append(msgs, in.State+":"+in.NodeName+":"+in.Message)
		}
		return ready >= n, strings.Join(msgs, " | ")
	})
	return ins
}

func (l *live) demo(name string, sel map[string]string) api.ContainerGroupSpec {
	img := l.images + "/demo:2"
	if sel["host"] != "" && sel["host"] == os.Getenv("OGM_LIVE_PROCESS_HOST") {
		img = "ogm/demo"
	}
	return api.ContainerGroupSpec{
		Name: name, Replicas: 1, NodeSelector: sel,
		Container:      api.ContainerSpec{Image: img, Resources: api.Resources{CPU: 1, MemoryMB: 128}},
		Networking:     &api.Networking{Port: 8080, Auth: true},
		ReadinessProbe: &api.Probe{HTTP: &api.HTTPProbe{Path: "/health", Port: 8080}, PeriodSeconds: 1},
	}
}

func TestLive(t *testing.T) {
	srv, key := os.Getenv("OGM_LIVE_SERVER"), os.Getenv("OGM_LIVE_KEY")
	if srv == "" || key == "" {
		t.Skip("set OGM_LIVE_SERVER and OGM_LIVE_KEY to run against a real cluster")
	}
	hosts := strings.Split(os.Getenv("OGM_LIVE_HOSTS"), ",")
	procHost := os.Getenv("OGM_LIVE_PROCESS_HOST")
	gpuHost := os.Getenv("OGM_LIVE_GPU_HOST")
	images := os.Getenv("OGM_LIVE_IMAGES")
	if images == "" {
		images = "registry.ogm/default"
	}
	report := &strings.Builder{}
	l := &live{t: t, url: strings.TrimRight(srv, "/"), key: key, c: client.New(srv, key), ctx: context.Background(), images: images, report: report}
	l.c.HTTP.Timeout = 5 * time.Minute
	defer func() {
		if p := os.Getenv("OGM_LIVE_REPORT"); p != "" {
			_ = os.WriteFile(p, []byte(report.String()), 0o644)
		}
	}()
	dockerSel := map[string]string{"runtime": "docker"}

	// ---------- fleet ----------
	nodeOf := map[string]api.Node{} // host label -> node
	l.run("fleet", func(t *testing.T) {
		nodes, err := l.c.Nodes(l.ctx)
		l.must(err)
		arches := map[string]int{}
		for _, n := range nodes {
			if h := n.Labels["host"]; h != "" && n.Status == "online" {
				nodeOf[h] = n
				arches[n.Arch]++
			}
		}
		want := append([]string{}, hosts...)
		if procHost != "" {
			want = append(want, procHost)
		}
		for _, h := range want {
			if _, ok := nodeOf[h]; !ok {
				t.Fatalf("host %s not online (have %v)", h, keys(nodeOf))
			}
		}
		l.logf("fleet: %d nodes online, architectures %v", len(nodeOf), arches)
		for _, h := range want {
			var a api.Assessment
			if err := l.do("GET", "/v1/nodes/"+nodeOf[h].ID+"/assessment", nil, &a); err == nil {
				l.logf("assessment %s: tier=%s cpu=%dc/%dt mem=%dGB nat=%s gpus=%d", h, a.Tier, a.CPU.Cores, a.CPU.Threads, a.Memory.TotalMB/1024, a.Network.NAT, len(a.GPUs))
			} else {
				l.logf("assessment %s: none (%v)", h, err)
			}
		}
	})

	l.run("registry multi-arch", func(t *testing.T) {
		var cat struct{ Repositories []string }
		code, body, _ := l.http("GET", l.url+"/v2/_catalog", key, "", nil)
		_ = json.Unmarshal([]byte(body), &cat)
		if code != 200 || len(cat.Repositories) == 0 {
			t.Fatalf("catalog: %d %s", code, body)
		}
		for _, ref := range []string{"demo/manifests/2", "postgres/manifests/17-alpine"} {
			code, body, _ = l.http("GET", l.url+"/v2/default/"+ref, key, "", map[string]string{
				"Accept": "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"})
			if code != 200 || !strings.Contains(body, "arm64") || !strings.Contains(body, "amd64") {
				t.Fatalf("%s is not multi-arch (push it from both architectures, or `ogm push --platform`): %d %.300s", ref, code, body)
			}
		}
		l.logf("registry: %v; demo:2 and postgres:17-alpine have amd64+arm64", cat.Repositories)
	})

	// ---------- compute on every machine ----------
	l.run("container on every host", func(t *testing.T) {
		all := append([]string{}, hosts...)
		if procHost != "" {
			all = append(all, procHost)
		}
		var wg sync.WaitGroup
		for _, h := range all {
			l.deploy(l.demo("lv-cpu-"+h, map[string]string{"host": h}))
		}
		for _, h := range all {
			h := h
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				ins := l.waitReady("lv-cpu-"+h, 1, 5*time.Minute)
				ready := time.Since(start)
				code, body, rtt := l.gw("lv-cpu-"+h, "GET", "/", "")
				if code != 200 || !strings.Contains(body, ins[0].ID) {
					t.Errorf("%s gateway: %d %s", h, code, body)
					return
				}
				l.logf("host %s (%s/%s): ready in %s incl. image pull, gateway round trip %s", h, nodeOf[h].OS, nodeOf[h].Arch, ready.Round(time.Second), rtt.Round(time.Millisecond))
			}()
		}
		wg.Wait()
	})

	l.run("lifecycle: logs, events, rolling update, restart, recreate, reallocate", func(t *testing.T) {
		g := "lv-cpu-" + hosts[0]
		s := l.demo(g, map[string]string{"host": hosts[0]})
		l.deploy(s)
		l.waitReady(g, 1, 3*time.Minute)
		s.Container.Env = map[string]string{"DEMO_V": "2"}
		_, err := l.c.Deploy(l.ctx, "default", s)
		l.must(err)
		eventually(t, 3*time.Minute, "rolled to v2", func() (bool, string) {
			ins, _ := l.c.Instances(l.ctx, "default", g)
			return len(ins) == 1 && ins[0].Version == 2 && ins[0].Ready, fmt.Sprint(ins)
		})
		if c, b, _ := l.gw(g, "GET", "/env?name=DEMO_V", ""); c != 200 || !strings.Contains(b, `"2"`) {
			t.Fatalf("v2 env: %d %s", c, b)
		}
		ins, _ := l.c.Instances(l.ctx, "default", g)
		for _, action := range []string{"restart", "recreate"} {
			l.must(l.do("POST", "/v1/projects/default/containers/"+g+"/instances/"+ins[0].ID+"/"+action, nil, nil))
			time.Sleep(2 * time.Second)
			l.waitReady(g, 1, 2*time.Minute)
		}
		var logs struct {
			Items []map[string]any `json:"items"`
		}
		l.must(l.do("GET", "/v1/projects/default/containers/"+g+"/logs?since=30m&limit=50", nil, &logs))
		var evs struct {
			Items []map[string]any `json:"items"`
		}
		l.must(l.do("GET", "/v1/projects/default/containers/"+g+"/events?limit=50", nil, &evs))
		if len(evs.Items) < 5 {
			t.Fatalf("few events: %d", len(evs.Items))
		}
		// Reallocate moves the instance off its node (any Docker host allowed).
		s2 := l.demo("lv-realloc", dockerSel)
		l.deploy(s2)
		in := l.waitReady("lv-realloc", 1, 3*time.Minute)[0]
		l.must(l.do("POST", "/v1/projects/default/containers/lv-realloc/instances/"+in.ID+"/reallocate", nil, nil))
		eventually(t, 3*time.Minute, "reallocated to another node", func() (bool, string) {
			ins, _ := l.c.Instances(l.ctx, "default", "lv-realloc")
			for _, x := range ins {
				if x.Ready && x.NodeID != in.NodeID {
					return true, ""
				}
			}
			return false, fmt.Sprint(ins)
		})
		l.logf("lifecycle: rolling update to v2, restart, recreate and reallocate (moved off %s) all OK; %d log lines, %d events", in.NodeName, len(logs.Items), len(evs.Items))
	})

	// ---------- networking ----------
	l.run("links across nodes and architectures", func(t *testing.T) {
		api1 := l.demo("lv-api", map[string]string{"host": hosts[len(hosts)-1]})
		l.deploy(api1)
		webHost := hosts[0]
		if procHost != "" {
			webHost = procHost
		}
		web := l.demo("lv-web", map[string]string{"host": webHost})
		web.Container.Env = map[string]string{"DEMO_API": "{{link:lv-api:8080}}"}
		l.deploy(web)
		l.waitReady("lv-api", 1, 5*time.Minute)
		l.waitReady("lv-web", 1, 5*time.Minute)
		var total time.Duration
		for i := 0; i < 5; i++ {
			code, body, d := l.gw("lv-web", "GET", "/fetch?env=DEMO_API&path=/", "")
			if code != 200 || !strings.Contains(body, `"group":"lv-api"`) {
				t.Fatalf("link: %d %s", code, body)
			}
			total += d
		}
		l.logf("link: %s (%s) -> hub -> %s (%s) works, avg %s per request incl. gateway", webHost, nodeOf[webHost].Arch, hosts[len(hosts)-1], nodeOf[hosts[len(hosts)-1]].Arch, (total / 5).Round(time.Millisecond))
	})

	// ---------- state: secrets, volumes, snapshots, migration (real Postgres) ----------
	l.run("postgres: secret, volume, link, snapshot, migrate", func(t *testing.T) {
		l.must(l.do("PUT", "/v1/projects/default/secrets/lv-pg-pass", map[string]string{"value": fmt.Sprintf("pw-%d", time.Now().UnixNano())}, nil))
		amd := map[string]string{"runtime": "docker", "arch": "amd64"}
		pg := api.ContainerGroupSpec{Name: "lv-pg", Replicas: 1, NodeSelector: amd,
			Container: api.ContainerSpec{Image: images + "/postgres:17-alpine", Ports: []int{5432},
				Env: map[string]string{"POSTGRES_USER": "app", "POSTGRES_DB": "app", "POSTGRES_PASSWORD": "{{secret:lv-pg-pass}}",
					"PGDATA": "/var/lib/postgresql/data/pgdata"},
				Volumes:   []api.Volume{{Name: "data", Mount: "/var/lib/postgresql/data"}},
				Resources: api.Resources{CPU: 1, MemoryMB: 512}},
			ReadinessProbe: &api.Probe{TCP: &api.TCPProbe{Port: 5432}, PeriodSeconds: 2}}
		// A volume left by an earlier run would hold a database initialised with
		// an older password: start from empty, and clean up groups before volumes.
		for _, g := range []string{"lv-psql", "lv-pg"} {
			_ = l.c.DeleteGroup(l.ctx, "default", g)
			l.waitGone(g)
		}
		_ = l.do("DELETE", "/v1/projects/default/volumes/lv-pg", nil, nil)
		t.Cleanup(func() {
			for _, g := range []string{"lv-psql", "lv-pg"} {
				_ = l.c.DeleteGroup(context.Background(), "default", g)
				l.waitGone(g)
			}
			_ = l.do("DELETE", "/v1/projects/default/volumes/lv-pg", nil, nil)
		})
		l.deploy(pg)
		clientHost := hosts[len(hosts)-1] // ideally an arm64 node
		psql := api.ContainerGroupSpec{Name: "lv-psql", Replicas: 1, NodeSelector: map[string]string{"host": clientHost},
			Container: api.ContainerSpec{Image: images + "/postgres:17-alpine",
				Command: []string{"sh", "-c", `until pg_isready -q; do sleep 1; done; psql -q -c 'CREATE TABLE IF NOT EXISTS boots(at timestamptz default now())' -c 'INSERT INTO boots DEFAULT VALUES'; echo "BOOTS=$(psql -tA -c 'SELECT count(*) FROM boots')"; sleep 3600`},
				Env: map[string]string{"PGHOST": "{{link_host:lv-pg:5432}}", "PGPORT": "{{link_port:lv-pg:5432}}", "PGUSER": "app",
					"PGDATABASE": "app", "PGPASSWORD": "{{secret:lv-pg-pass}}"},
				Resources: api.Resources{CPU: 1, MemoryMB: 128}}}
		l.deploy(psql)
		pgIns := l.waitReady("lv-pg", 1, 5*time.Minute)[0]
		l.waitReady("lv-psql", 1, 3*time.Minute)
		boots := func(want int) {
			t.Helper()
			eventually(t, 3*time.Minute, fmt.Sprintf("BOOTS=%d", want), func() (bool, string) {
				var logs struct {
					Items []struct {
						Line string `json:"line"`
					} `json:"items"`
				}
				_ = l.do("GET", "/v1/projects/default/containers/lv-psql/logs?since=60m&q=BOOTS&limit=20", nil, &logs)
				var last string
				for _, x := range logs.Items {
					last = x.Line
				}
				return strings.Contains(last, fmt.Sprintf("BOOTS=%d", want)), last
			})
		}
		boots(1)
		l.logf("postgres on %s (amd64), psql client on %s via link, secret password: BOOTS=1", pgIns.NodeName, clientHost)

		var sn map[string]any
		l.must(l.do("POST", "/v1/projects/default/volumes/lv-pg/snapshots", map[string]any{"name": "live"}, &sn))
		id := sn["id"].(string)
		start := time.Now()
		eventually(t, 5*time.Minute, "snapshot ready", func() (bool, string) {
			_ = l.do("GET", "/v1/projects/default/snapshots/"+id, nil, &sn)
			return sn["status"] != "pending", fmt.Sprint(sn["status"], sn["error"])
		})
		if sn["status"] != "ready" {
			t.Fatalf("snapshot: %v", sn)
		}
		l.logf("snapshot %s: %.1f MB in %s", id, sn["bytes"].(float64)/1e6, time.Since(start).Round(time.Second))

		l.must(l.do("POST", "/v1/projects/default/containers/lv-pg/stop", nil, nil))
		l.waitGone("lv-pg")
		target := ""
		for _, h := range hosts {
			if n := nodeOf[h]; n.Arch == "amd64" && n.ID != pgIns.NodeID {
				target = n.Name
				break
			}
		}
		if target == "" {
			t.Skip("no second amd64 Docker node to migrate to")
		}
		l.must(l.do("POST", "/v1/projects/default/snapshots/"+id+"/restore", map[string]any{"node": target}, nil))
		eventually(t, 5*time.Minute, "restored", func() (bool, string) {
			_ = l.do("GET", "/v1/projects/default/snapshots/"+id, nil, &sn)
			return sn["restore_status"] != "restoring", fmt.Sprint(sn["restore_status"])
		})
		if sn["restore_status"] != "restored" {
			t.Fatalf("restore: %v", sn["restore_status"])
		}
		l.must(l.do("POST", "/v1/projects/default/containers/lv-pg/start", nil, nil))
		moved := l.waitReady("lv-pg", 1, 5*time.Minute)[0]
		if moved.NodeName != target {
			t.Fatalf("postgres on %s, want %s", moved.NodeName, target)
		}
		ps, _ := l.c.Instances(l.ctx, "default", "lv-psql")
		l.must(l.do("POST", "/v1/projects/default/containers/lv-psql/instances/"+ps[0].ID+"/restart", nil, nil))
		boots(2)
		l.logf("migrated postgres %s -> %s via snapshot; data intact (BOOTS=2)", pgIns.NodeName, target)
		_ = l.do("DELETE", "/v1/projects/default/snapshots/"+id, nil, nil)
		l.must(l.c.DeleteGroup(l.ctx, "default", "lv-psql"))
		l.must(l.c.DeleteGroup(l.ctx, "default", "lv-pg"))
		l.waitGone("lv-pg")
	})

	// ---------- scaling ----------
	l.run("http autoscaler: zero -> N -> zero", func(t *testing.T) {
		s := l.demo("lv-auto", dockerSel)
		s.Replicas = 0
		s.HTTPAutoscaler = &api.HTTPAutoscaler{MinReplicas: 0, MaxReplicas: 3, TargetConcurrency: 2, StableWindowSeconds: 6, ScaleDownDelaySeconds: 15}
		s.Networking.MaxConcurrency = 2
		l.deploy(s)
		code, _, cold := l.gw("lv-auto", "GET", "/", "")
		if code != 200 {
			t.Fatalf("cold: %d", code)
		}
		ctx, cancel := context.WithTimeout(l.ctx, 25*time.Second)
		var wg sync.WaitGroup
		var mu sync.Mutex
		okN, failN := 0, 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					c, _, _ := l.gw("lv-auto", "POST", "/generate", `{"sleep_ms":1500}`)
					mu.Lock()
					if c == 200 {
						okN++
					} else if ctx.Err() == nil {
						failN++
					}
					mu.Unlock()
				}
			}()
		}
		peak := 0
		nodes := map[string]bool{}
		for ctx.Err() == nil {
			ins, _ := l.c.Instances(l.ctx, "default", "lv-auto")
			r := 0
			for _, in := range ins {
				if in.Ready {
					r++
					nodes[in.NodeName] = true
				}
			}
			peak = max(peak, r)
			time.Sleep(time.Second)
		}
		cancel()
		wg.Wait()
		if peak < 2 {
			t.Fatalf("never scaled past %d replicas", peak)
		}
		eventually(t, 2*time.Minute, "back to zero", func() (bool, string) {
			g, _ := l.c.Group(l.ctx, "default", "lv-auto")
			return g.DesiredReplicas == 0 && g.CurrentState.InstanceStatusCounts.Running == 0, fmt.Sprint(g.DesiredReplicas)
		})
		l.logf("http autoscaler: cold start %s, peak %d ready replicas on %v under 8 concurrent clients, %d ok / %d failed requests, back to 0", cold.Round(time.Millisecond), peak, keys(nodes), okN, failN)
	})

	l.run("queue + queue autoscaler across architectures", func(t *testing.T) {
		_ = l.do("DELETE", "/v1/projects/default/queues/lv-q?force=1", nil, nil)
		_, err := l.c.CreateQueue(l.ctx, "default", "lv-q", api.QueueSettings{})
		l.must(err)
		t.Cleanup(func() { _ = l.do("DELETE", "/v1/projects/default/queues/lv-q?force=1", nil, nil) })
		s := l.demo("lv-worker", dockerSel)
		s.Networking = nil
		s.Replicas = 0
		s.QueueConnection = &api.QueueConnection{QueueName: "lv-q", Port: 8080, Path: "/generate", Concurrency: 1}
		s.QueueAutoscaler = &api.QueueAutoscaler{MinReplicas: 0, MaxReplicas: len(hosts), DesiredQueueLength: 2, PollingPeriod: 5, MaxUpscalePerMinute: 20, MaxDownscalePerMinute: 20}
		l.deploy(s)
		var jps []api.JobPrototype
		for i := 0; i < 24; i++ {
			jps = append(jps, api.JobPrototype{Input: json.RawMessage(fmt.Sprintf(`{"prompt":"job %d","sleep_ms":1500}`, i))})
		}
		jobs, err := l.c.SubmitJobs(l.ctx, "default", "lv-q", jps)
		l.must(err)
		start := time.Now()
		eventually(t, 6*time.Minute, "all jobs succeeded", func() (bool, string) {
			done := 0
			for _, j := range jobs {
				jj, err := l.c.Job(l.ctx, "default", "lv-q", j.ID, 0)
				if err == nil && jj.Status == api.JobSucceeded {
					done++
				}
			}
			return done == len(jobs), fmt.Sprint(done)
		})
		took := time.Since(start)
		var all []api.Instance
		_ = l.do("GET", "/v1/projects/default/containers/lv-worker/instances?all=1", nil, &struct {
			Items *[]api.Instance `json:"items"`
		}{&all})
		instNode := map[string]string{}
		for _, in := range all {
			instNode[in.ID] = in.NodeName
		}
		perNode := map[string]int{}
		for _, j := range jobs {
			jj, _ := l.c.Job(l.ctx, "default", "lv-q", j.ID, 0)
			perNode[instNode[jj.InstanceID]]++
		}
		l.logf("queue: 24 jobs done in %s; jobs per node %v", took.Round(time.Second), perNode)
	})

	l.run("max_concurrency", func(t *testing.T) {
		s := l.demo("lv-mc", map[string]string{"host": hosts[0]})
		s.Networking.MaxConcurrency = 1
		l.deploy(s)
		l.waitReady("lv-mc", 1, 3*time.Minute)
		start := time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); l.gw("lv-mc", "POST", "/generate", `{"sleep_ms":1000}`) }()
		}
		wg.Wait()
		if d := time.Since(start); d < 2900*time.Millisecond {
			t.Fatalf("ran in parallel: %s", d)
		}
		l.logf("max_concurrency=1: 3 x 1 s requests serialized (%s)", time.Since(start).Round(100*time.Millisecond))
	})

	// ---------- data services ----------
	l.run("kv + instance identity from containers", func(t *testing.T) {
		ihosts := []string{hosts[len(hosts)-1]}
		if procHost != "" {
			ihosts = append(ihosts, procHost)
		}
		key := fmt.Sprintf("visits-%d", time.Now().Unix())
		n := 0
		for _, h := range ihosts {
			l.deploy(l.demo("lv-ident-"+h, map[string]string{"host": h}))
			l.waitReady("lv-ident-"+h, 1, 3*time.Minute)
			for i := 0; i < 3; i++ {
				n++
				code, body, _ := l.gw("lv-ident-"+h, "GET", "/hub-incr?key="+key, "")
				if code != 200 || !strings.Contains(body, fmt.Sprintf(`"value":%d`, n)) {
					t.Fatalf("%s hub-incr: %d %s", h, code, body)
				}
			}
		}
		code, body, _ := l.http("GET", l.url+"/v1/projects/default/kv/demo/"+key, l.key, "", nil)
		if code != 200 || body != fmt.Sprint(n) {
			t.Fatalf("counter: %d %s", code, body)
		}
		_ = l.do("DELETE", "/v1/projects/default/kv/demo", nil, nil)
		l.logf("kv: containers on %v incremented a shared counter to %d using IMDS identity tokens (no API keys)", ihosts, n)
	})

	l.run("schedules", func(t *testing.T) {
		s := l.demo("lv-sched", dockerSel)
		s.Replicas = 0
		l.deploy(s)
		sp := map[string]any{"schedule": "* * * * *", "scale": map[string]any{"group": "lv-sched", "replicas": 1}}
		l.must(l.do("PUT", "/v1/projects/default/schedules/lv-up", sp, nil))
		t.Cleanup(func() { _ = l.do("DELETE", "/v1/projects/default/schedules/lv-up", nil, nil) })
		l.waitReady("lv-sched", 1, 4*time.Minute)
		hs := map[string]any{"schedule": "0 0 1 1 *", "http": map[string]any{"group": "lv-sched", "method": "POST", "path": "/generate", "body": `{"sleep_ms":10}`}}
		l.must(l.do("PUT", "/v1/projects/default/schedules/lv-http", hs, nil))
		t.Cleanup(func() { _ = l.do("DELETE", "/v1/projects/default/schedules/lv-http", nil, nil) })
		var run map[string]string
		l.must(l.do("POST", "/v1/projects/default/schedules/lv-http/run", nil, &run))
		if run["status"] != "ok: HTTP 200" {
			t.Fatalf("http schedule: %v", run)
		}
		l.logf("schedules: cron scale-up fired on its own; http schedule -> %s", run["status"])
	})

	l.run("files + signed URLs", func(t *testing.T) {
		payload := bytes.Repeat([]byte("ogm"), 1<<20) // 3 MB
		resp, err := l.c.Raw(l.ctx, "PUT", "/v1/projects/default/files/lv/blob.bin", bytes.NewReader(payload), nil)
		l.must(err)
		resp.Body.Close()
		var tok map[string]any
		l.must(l.do("POST", "/v1/projects/default/file-tokens/lv/blob.bin", map[string]any{"expires_in_s": 60}, &tok))
		code, body, d := l.http("GET", tok["url"].(string), "", "", nil)
		if code != 200 || len(body) != len(payload) {
			t.Fatalf("signed get: %d %d", code, len(body))
		}
		_ = l.do("DELETE", "/v1/projects/default/files/lv/blob.bin", nil, nil)
		l.logf("files: 3 MB via presigned URL in %s", d.Round(time.Millisecond))
	})

	l.run("s3 api", func(t *testing.T) {
		cr := newS3Cred(t, l.c, "default")
		defer l.do("DELETE", "/v1/projects/default/s3-credentials/"+cr.AccessKeyID, nil, nil) //nolint:errcheck
		sc := s3For(l.url+"/s3", cr.AccessKeyID, cr.SecretAccessKey, "us-east-1")
		bucket := fmt.Sprintf("lv-%d", time.Now().Unix())
		_, err := sc.CreateBucket(l.ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		l.must(err)
		body := bytes.Repeat([]byte("x"), 8<<20)
		start := time.Now()
		_, err = sc.PutObject(l.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("a/b.bin"), Body: bytes.NewReader(body)})
		l.must(err)
		up := time.Since(start)
		out, err := sc.GetObject(l.ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("a/b.bin")})
		l.must(err)
		got, _ := io.ReadAll(out.Body)
		out.Body.Close()
		if !bytes.Equal(got, body) {
			t.Fatal("s3 round trip mismatch")
		}
		_, _ = sc.DeleteObject(l.ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("a/b.bin")})
		_, err = sc.DeleteBucket(l.ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		l.must(err)
		l.logf("s3: aws-sdk-go-v2 put/get 8 MB (put %s), bucket create/delete OK", up.Round(time.Millisecond))
	})

	l.run("scoped keys", func(t *testing.T) {
		var k map[string]any
		l.must(l.do("POST", "/v1/keys", map[string]any{"name": "lv-viewer", "project": "default", "role": "viewer", "expires_in_hours": 1}, &k))
		defer l.do("DELETE", "/v1/keys/"+k["id"].(string), nil, nil) //nolint:errcheck
		vk := k["key"].(string)
		if c, _, _ := l.http("GET", l.url+"/v1/projects/default/containers", vk, "", nil); c != 200 {
			t.Fatalf("viewer read: %d", c)
		}
		if c, _, _ := l.http("POST", l.url+"/v1/projects/default/containers/lv-mc/stop", vk, "", nil); c != 403 {
			t.Fatalf("viewer write: %d", c)
		}
		l.logf("iam: viewer key reads, cannot write")
	})

	l.run("metrics + prometheus", func(t *testing.T) {
		var m struct {
			Summary map[string]any `json:"summary"`
		}
		l.deploy(l.demo("lv-met", dockerSel))
		l.waitReady("lv-met", 1, 3*time.Minute)
		for i := 0; i < 20; i++ {
			l.gw("lv-met", "GET", "/", "")
		}
		l.gw("lv-met", "GET", "/no-such-path", "")
		eventually(t, 2*time.Minute, "metrics recorded", func() (bool, string) {
			l.must(l.do("GET", "/v1/projects/default/containers/lv-met/metrics?minutes=30", nil, &m))
			n, _ := m.Summary["requests"].(float64)
			return n >= 21, fmt.Sprint(m.Summary)
		})
		code, body, _ := l.http("GET", l.url+"/metrics", key, "", nil)
		if code != 200 || !strings.Contains(body, "ogm_gateway_requests_total") {
			t.Fatalf("prometheus: %d", code)
		}
		l.logf("metrics: lv-met %v requests, p50 %v ms, p95 %v ms, p99 %v ms; /metrics has %d lines", m.Summary["requests"], m.Summary["p50_ms"], m.Summary["p95_ms"], m.Summary["p99_ms"], strings.Count(body, "\n"))
	})

	l.run("custom domain", func(t *testing.T) {
		dom := fmt.Sprintf("lv-%d.example.test", time.Now().Unix())
		s := l.demo("lv-dom", dockerSel)
		s.Networking.Auth = false
		s.Networking.CustomDomains = []string{dom}
		l.deploy(s)
		l.waitReady("lv-dom", 1, 3*time.Minute)
		eventually(t, 30*time.Second, "routed by Host", func() (bool, string) {
			code, body, _ := l.http("GET", l.url+"/", "", "", map[string]string{"Host": dom})
			return code == 200 && strings.Contains(body, "lv-dom"), fmt.Sprint(code, " ", body)
		})
		l.logf("custom domain: Host %s routed to lv-dom", dom)
	})

	l.run("quotas", func(t *testing.T) {
		_ = l.do("POST", "/v1/projects", map[string]string{"name": "lv-quota"}, nil)
		t.Cleanup(func() { _ = l.do("DELETE", "/v1/projects/lv-quota", nil, nil) })
		l.must(l.do("PUT", "/v1/projects/lv-quota/quota", map[string]int{"cpu": 2, "instances": 2, "storage_mb": 1}, nil))
		s := l.demo("lv-qa", dockerSel)
		s.Replicas = 2
		_, err := l.c.Deploy(l.ctx, "lv-quota", s)
		l.must(err)
		t.Cleanup(func() { _ = l.c.DeleteGroup(context.Background(), "lv-quota", "lv-qa") })
		s.Name = "lv-qb"
		s.Replicas = 1
		if _, err := l.c.Deploy(l.ctx, "lv-quota", s); err == nil || !strings.Contains(err.Error(), "quota") {
			t.Fatalf("over quota deploy: %v", err)
		}
		code, _, _ := l.http("PUT", l.url+"/v1/projects/lv-quota/files/big.bin", key, strings.Repeat("x", 2<<20), nil)
		if code != 403 {
			t.Fatalf("over storage quota: %d", code)
		}
		var st map[string]any
		l.must(l.do("GET", "/v1/projects/lv-quota/quota", nil, &st))
		l.logf("quotas: cpu=2 project ran 2 replicas, third rejected; 2 MB upload over 1 MB quota rejected; usage %v", st["usage"])
	})

	l.run("mcp", func(t *testing.T) {
		code, body, _ := l.http("POST", l.url+"/v1/mcp", key, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_nodes","arguments":{}}}`, map[string]string{"Content-Type": "application/json"})
		if code != 200 || !strings.Contains(body, hosts[0]) && !strings.Contains(body, "node_") {
			t.Fatalf("mcp: %d %.300s", code, body)
		}
		code, body, _ = l.http("POST", l.url+"/v1/mcp", key, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"Content-Type": "application/json"})
		l.logf("mcp: tools/list -> %d tools; tools/call list_nodes OK", strings.Count(body, `"inputSchema"`))
	})

	l.run("docs", func(t *testing.T) {
		for _, p := range []string{"/openapi.json", "/docs", "/llms.txt"} {
			code, body, _ := l.http("GET", l.url+p, "", "", nil)
			if code != 200 || len(body) < 100 {
				t.Errorf("%s: %d", p, code)
			}
		}
		l.logf("docs: /openapi.json, /docs, /llms.txt served")
	})

	if gpuHost != "" {
		l.run("gpu", func(t *testing.T) {
			s := l.demo("lv-gpu", map[string]string{"host": gpuHost})
			s.Container.Resources.GPUCount = 1
			s.Priority = "low"
			l.deploy(s)
			l.waitReady("lv-gpu", 1, 3*time.Minute)
			code, body, _ := l.gw("lv-gpu", "GET", "/gpu", "")
			if code != 200 || !strings.Contains(body, "GPU 0") {
				t.Fatalf("gpu: %d %s", code, body)
			}
			l.logf("gpu: container on %s sees %s", gpuHost, strings.TrimSpace(body))
		})
	}

	l.run("owner pause moves work", func(t *testing.T) {
		s := l.demo("lv-pause", dockerSel)
		l.deploy(s)
		in := l.waitReady("lv-pause", 1, 3*time.Minute)[0]
		l.must(l.do("PATCH", "/v1/nodes/"+in.NodeID, map[string]any{"paused": true}, nil))
		defer l.do("PATCH", "/v1/nodes/"+in.NodeID, map[string]any{"paused": false}, nil) //nolint:errcheck
		start := time.Now()
		eventually(t, 4*time.Minute, "moved off paused node", func() (bool, string) {
			ins, _ := l.c.Instances(l.ctx, "default", "lv-pause")
			for _, x := range ins {
				if x.Ready && x.NodeID != in.NodeID {
					return true, ""
				}
			}
			return false, fmt.Sprint(ins)
		})
		l.logf("pause: %s paused, workload ready elsewhere after %s", in.NodeName, time.Since(start).Round(time.Second))
	})
}

func keys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
