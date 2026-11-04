package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/agent"
	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
	"github.com/old-stock/open-gpu-mesh/internal/demoapp"
	"github.com/old-stock/open-gpu-mesh/internal/server"
)

func logger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func cmdServer(e *env, args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", envOr("OGM_LISTEN", ":7777"), "listen address")
	data := fs.String("data", envOr("OGM_DATA", "./ogm-data"), "data directory (SQLite db + files)")
	public := fs.String("public-url", os.Getenv("OGM_PUBLIC_URL"), "externally reachable URL, e.g. https://gpu.example.com")
	gwDomain := fs.String("gateway-domain", os.Getenv("OGM_GATEWAY_DOMAIN"), "wildcard domain for {group}-{project}.DOMAIN gateway routing (optional)")
	gwListen := fs.String("gateway-listen", os.Getenv("OGM_GATEWAY_LISTEN"), "serve workloads (/gw/...) on their own listener, e.g. :7778, so they get an origin separate from the API and dashboard")
	gwURL := fs.String("gateway-url", os.Getenv("OGM_GATEWAY_URL"), "public base URL of --gateway-listen (default: the public URL's host on that port)")
	tlsOn := fs.Bool("tls", os.Getenv("OGM_TLS") == "1", "automatic HTTPS via Let's Encrypt on :443 (+ :80 for challenges/redirect) for the public URL host and custom domains")
	acmeEmail := fs.String("acme-email", os.Getenv("OGM_ACME_EMAIL"), "contact email for Let's Encrypt (with --tls)")
	name := fs.String("name", envOr("OGM_CLUSTER_NAME", "open-gpu-mesh"), "cluster name shown in the dashboard")
	admin := fs.String("admin", "admin", "name of the first admin user")
	dist := fs.String("dist-dir", os.Getenv("OGM_DIST_DIR"), "directory with ogm-{os}-{arch} binaries served at /dl")
	starter := fs.Float64("starter-credits", 10, "credits granted to each new member")
	level := fs.String("log-level", "info", "debug|info|warn|error")
	parse(fs, args)
	srv, err := server.New(server.Config{
		DataDir: *data, Listen: *listen, PublicURL: *public, GatewayDomain: *gwDomain, GatewayListen: *gwListen, GatewayURL: *gwURL, ClusterName: *name, TLS: *tlsOn, ACMEEmail: *acmeEmail,
		DistDir: *dist, StarterCredits: *starter, Logger: logger(*level),
	})
	check(err)
	defer srv.Close()
	key, err := srv.Bootstrap(*admin)
	check(err)
	cfg := srv.Config()
	if key != "" {
		fmt.Printf(`
  ┌─ open-gpu-mesh initialised ───────────────────────────────────────
  │ admin API key (shown once, save it!):
  │   %s
  │
  │ log in from any machine:   ogm login %s --key %s
  │ add a GPU node:             ogm node add my-gaming-pc
  │ invite friends:             ogm invite --uses 100
  │ dashboard:                  %s
  └──────────────────────────────────────────────────────────────────────

`, key, cfg.PublicURL, key, cfg.PublicURL)
	}
	check(srv.ListenAndServe(e.ctx))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func defaultSocket() string {
	if os.Geteuid() == 0 {
		return "/run/ogm-agent.sock"
	}
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "ogm", "agent.sock")
}

func cmdAgent(e *env, args []string) {
	if len(args) > 0 && (args[0] == "pause" || args[0] == "resume" || args[0] == "status") {
		agentControl(e, args[0], args[1:])
		return
	}
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	srvURL := fs.String("server", os.Getenv("OGM_SERVER"), "server URL")
	token := fs.String("token", os.Getenv("OGM_NODE_TOKEN"), "node token from `ogm node add`")
	name := fs.String("name", "", "node name (default: hostname)")
	rt := fs.String("runtime", envOr("OGM_RUNTIME", "docker"), "docker|process (process = no isolation, dev only)")
	fake := fs.String("fake-gpus", "", `simulate GPUs, e.g. "RTX 4090:24564,RTX 3090:24576"`)
	maxGPUs := fs.Int("max-gpus", 0, "share at most N GPUs (0 = all)")
	noGPU := fs.Bool("no-gpu", false, "share no GPUs at all (CPU, memory and disk only); the agent never queries the GPUs")
	maxCPU := fs.Int("max-cpu", 0, "share at most N vCPUs")
	maxMem := fs.Int("max-memory-mb", 0, "share at most N MB RAM")
	maxDisk := fs.Int("max-disk-gb", 0, "share at most N GB disk")
	country := fs.String("country", os.Getenv("OGM_COUNTRY"), "ISO country code of this machine, e.g. us")
	window := fs.String("window", "", `availability window(s) in local time, e.g. "23:00-08:00,12:00-13:00"`)
	yield := fs.Float64("yield-gpu-util", 0, "pause when an unassigned GPU is busier than this %% (owner gaming)")
	pauseOn := fs.String("pause-on-process", "", "comma-separated process names that pause sharing while running (e.g. steam,valorant)")
	prios := fs.String("allow-priorities", "", "only accept these priorities (comma-separated)")
	sock := fs.String("control-socket", defaultSocket(), "unix socket for `ogm agent pause|resume|status`")
	regPort := fs.Int("registry-port", 5077, "localhost port of the cluster-registry pull proxy (-1 disables)")
	gvisor := fs.Bool("gvisor", false, "run containers with the runsc (gVisor) OCI runtime for extra isolation")
	level := fs.String("log-level", "info", "debug|info|warn|error")
	var labels multiFlag
	fs.Var(&labels, "label", "node label key=value (repeatable)")
	parse(fs, args)
	if *srvURL == "" || *token == "" {
		fatalf("usage: ogm agent --server URL --token TOKEN (create a token with `ogm node add NAME`)")
	}
	var runtime agent.Runtime
	switch *rt {
	case "docker":
		d := &agent.DockerRuntime{}
		if *gvisor {
			d.OCIRuntime = "runsc"
		}
		ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
		if err := d.Ping(ctx); err != nil {
			fatalf("docker is not reachable (%v). Install/start Docker, or use --runtime process for a dev setup", err)
		}
		cancel()
		runtime = d
	case "process":
		exe, _ := os.Executable()
		runtime = &agent.ProcessRuntime{SelfExe: exe}
	default:
		fatalf("unknown runtime %q", *rt)
	}
	var gpus agent.GPUSource = agent.NvidiaSMI{}
	if *fake != "" {
		gpus = agent.ParseFakeGPUs(*fake)
	}
	if *noGPU {
		gpus = agent.ParseFakeGPUs("")
	}
	lm := map[string]string{}
	for _, l := range labels {
		if k, v, ok := strings.Cut(l, "="); ok {
			lm[k] = v
		}
	}
	_ = os.MkdirAll(filepath.Dir(*sock), 0o755)
	cfg := agent.Config{
		Server: *srvURL, Token: *token, Name: *name, Runtime: runtime, GPUs: gpus, MaxGPUs: *maxGPUs, MaxCPU: *maxCPU,
		MaxMemoryMB: *maxMem, MaxDiskGB: *maxDisk, Country: strings.ToLower(*country), Labels: lm, Window: *window,
		YieldGPUUtil: *yield, ControlSocket: *sock, Logger: logger(*level), RegistryPort: *regPort,
	}
	if *pauseOn != "" {
		cfg.PauseOnProcess = strings.Split(*pauseOn, ",")
	}
	if *prios != "" {
		cfg.AllowPriorities = strings.Split(*prios, ",")
	}
	a, err := agent.New(cfg)
	check(err)
	// SIGHUP = in-place upgrade: re-exec the (possibly replaced) binary, keeping workloads
	// running; the new process re-attaches to them. SIGTERM/Ctrl-C = drain and stop workloads.
	exe := selfPath()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		select {
		case <-hup:
			fmt.Fprintln(os.Stderr, "SIGHUP: re-executing agent, workloads keep running")
			a.Detach()
			cancel()
		case <-ctx.Done():
		}
	}()
	err = a.Run(ctx)
	if a.Detached() {
		time.Sleep(500 * time.Millisecond) // let instance goroutines detach
		check(reexec(exe))
	}
	check(err)
}

// selfPath resolves our binary path at startup (os.Executable points at a deleted inode once
// the file has been replaced, which is exactly the upgrade case).
func selfPath() string {
	p := os.Args[0]
	if !strings.Contains(p, string(os.PathSeparator)) {
		if lp, err := exec.LookPath(p); err == nil {
			p = lp
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func agentControl(e *env, action string, args []string) {
	fs := flag.NewFlagSet("agent "+action, flag.ExitOnError)
	sock := fs.String("control-socket", defaultSocket(), "agent control socket")
	parse(fs, args)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", *sock)
	}}, Timeout: 10 * time.Second}
	method := "POST"
	if action == "status" {
		method = "GET"
	}
	req, _ := http.NewRequest(method, "http://agent/"+action, nil)
	resp, err := hc.Do(req)
	if err != nil {
		fatalf("cannot reach agent at %s: %v", *sock, err)
	}
	defer resp.Body.Close()
	var st agent.Status
	check(json.NewDecoder(resp.Body).Decode(&st))
	if e.output == "json" {
		e.json(st)
		return
	}
	state := "sharing"
	if st.Paused {
		state = "paused (" + st.PauseReason + ")"
	}
	fmt.Fprintf(e.out, "node %s  connected=%v  runtime=%s  %s\n", orDash(st.NodeID), st.Connected, st.Runtime, state)
	for _, g := range st.GPUs {
		fmt.Fprintf(e.out, "  gpu%d %s %dMB util=%.0f%% temp=%.0fC\n", g.Index, g.Name, g.VRAMMB, g.UtilPct, g.TempC)
	}
	for _, in := range st.Instances {
		fmt.Fprintf(e.out, "  instance %s %s ready=%v\n", in.ID, in.State, in.Ready)
	}
}

func cmdDemoApp(args []string) {
	if err := demoapp.Run(args); err != nil {
		fatalf("%v", err)
	}
}

var devGPUs = []string{"RTX 4090:24564", "RTX 3090:24576", "RTX 3060:12288", "RTX 4080:16376", "A100-SXM4-80GB:81920", "RTX 2080 Ti:11264"}
var devCountries = []string{"us", "ca", "de", "gb", "us", "jp"}

func cmdDev(e *env, args []string) {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	nodes := fs.Int("nodes", 3, "number of simulated GPU nodes")
	listen := fs.String("listen", "127.0.0.1:7777", "listen address")
	data := fs.String("data", "", "data directory (default: a fresh temp dir)")
	example := fs.Bool("example", true, "deploy an example 'hello' container group and queue")
	login := fs.Bool("login", true, "save CLI credentials for this dev cluster")
	parse(fs, args)
	if *data == "" {
		d, err := os.MkdirTemp("", "ogm-dev-")
		check(err)
		*data = d
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	public := "http://" + *listen
	srv, err := server.New(server.Config{DataDir: *data, Listen: *listen, PublicURL: public, ClusterName: "ogm dev cluster", Logger: quiet, ReconcileInterval: 500 * time.Millisecond})
	check(err)
	defer srv.Close()
	key, err := srv.Bootstrap("admin")
	check(err)
	ln, err := net.Listen("tcp", *listen)
	check(err)
	go func() { _ = srv.Serve(e.ctx, ln) }()
	c := client.New(public, key)
	if key == "" {
		fatalf("data dir %s is already initialised; use a fresh --data dir", *data)
	}
	exe, _ := os.Executable()
	for i := 0; i < *nodes; i++ {
		gpu := devGPUs[i%len(devGPUs)]
		name := fmt.Sprintf("friend%d-%s", i+1, api.Slugify(strings.Split(gpu, ":")[0]))
		tok, err := c.CreateNode(e.ctx, name)
		check(err)
		a, err := agent.New(agent.Config{
			Server: public, Token: tok.Token, Name: name, Runtime: &agent.ProcessRuntime{SelfExe: exe},
			GPUs: agent.ParseFakeGPUs(gpu), Country: devCountries[i%len(devCountries)], Logger: quiet, StopGrace: 3 * time.Second,
		})
		check(err)
		go func() { _ = a.Run(e.ctx) }()
	}
	if *login {
		check(saveConfig(Config{Server: public, APIKey: key, Project: "default"}))
	}
	if *example {
		_, err := c.CreateQueue(e.ctx, "default", "hello-jobs", api.QueueSettings{})
		check(err)
		_, err = c.Deploy(e.ctx, "default", api.ContainerGroupSpec{
			Name: "hello", Replicas: 2, Priority: "medium",
			Container:       api.ContainerSpec{Image: "ogm/demo", Resources: api.Resources{CPU: 1, MemoryMB: 1024, GPUCount: 1}},
			Networking:      &api.Networking{Port: 8080, Auth: false},
			ReadinessProbe:  &api.Probe{HTTP: &api.HTTPProbe{Path: "/health", Port: 8080}},
			QueueConnection: &api.QueueConnection{QueueName: "hello-jobs", Port: 8080, Path: "/generate"},
			Readme:          "Example group running the built-in demo app.",
		})
		check(err)
	}
	fmt.Printf(`
  open-gpu-mesh dev cluster is up with %d simulated GPU nodes.

  dashboard:   %s   (API key: %s)
  data dir:    %s
`, *nodes, public, key, *data)
	if *login {
		fmt.Printf("  CLI:         logged in (config %s)\n", configPath())
	} else {
		fmt.Printf("  CLI:         export OGM_SERVER=%s OGM_API_KEY=%s\n", public, key)
	}
	if *example {
		fmt.Printf(`
  try:
    ogm ls
    ogm get hello
    curl %s/gw/default/hello/
    ogm job submit hello-jobs --input '{"prompt":"a cat in space"}' --wait
    ogm logs hello -f
`, public)
	}
	fmt.Println("\n  Ctrl-C to stop.")
	<-e.ctx.Done()
	fmt.Println("\nshutting down…")
	time.Sleep(500 * time.Millisecond)
}
