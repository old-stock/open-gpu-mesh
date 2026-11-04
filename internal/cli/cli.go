// Package cli implements the `ogm` command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/client"
)

// Config is the CLI's saved login.
type Config struct {
	Server  string `json:"server"`
	APIKey  string `json:"api_key"`
	Project string `json:"project"`
}

func configPath() string {
	if p := os.Getenv("OGM_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "ogm", "config.json")
}

func loadConfig() Config {
	var c Config
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if v := os.Getenv("OGM_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("OGM_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("OGM_PROJECT"); v != "" {
		c.Project = v
	}
	if c.Project == "" {
		c.Project = "default"
	}
	return c
}

func saveConfig(c Config) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// env carries global state for commands.
type env struct {
	cfg     Config
	project string
	output  string // "table" or "json"
	out     io.Writer
	ctx     context.Context
}

func (e *env) client() *client.Client {
	if e.cfg.Server == "" || e.cfg.APIKey == "" {
		fatalf("not logged in: run `ogm login <server> --key <api-key>` or `ogm join <server> <invite-code> --name <you>`")
	}
	return client.New(e.cfg.Server, e.cfg.APIKey)
}

func (e *env) json(v any) {
	enc := json.NewEncoder(e.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (e *env) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(e.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "ogm: "+f+"\n", a...)
	os.Exit(1)
}

func check(err error) {
	if err == nil {
		return
	}
	var p *api.Problem
	if errors.As(err, &p) {
		msg := p.Title
		if p.Detail != "" {
			msg += ": " + p.Detail
		}
		keys := make([]string, 0, len(p.Errors))
		for k := range p.Errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			msg += "\n  " + k + ": " + strings.Join(p.Errors[k], "; ")
		}
		fatalf("%s", msg)
	}
	fatalf("%v", err)
}

const usage = `ogm — open-gpu-mesh: pool your friends' GPUs and run containers on them.

Run a cluster:
  ogm server [--listen :7777] [--data ./ogm-data] [--public-url URL]   start the control plane
  ogm agent --server URL --token TOKEN [flags]                         contribute this machine
  ogm agent pause|resume|status                                        control the local agent
  ogm assess [--profile gaming,cad] [--token T]                         audit this machine: hardware, speed test, NAT, suggested limits
  ogm dev [--nodes 3]                                                  everything on your laptop (simulated GPUs)

Account:
  ogm join SERVER INVITE_CODE --name NAME     join a cluster with an invite
  ogm login SERVER --key API_KEY              save credentials
  ogm status                                  cluster + account summary
  ogm invite [--uses N] [--admin]             create an invite (admin)
  ogm users                                   leaderboard: contributed vs consumed credits
  ogm credits                                 your balance and ledger

Workloads:
  ogm deploy -f app.yaml                      create or update container groups/queues (declarative)
  ogm ls                                      list container groups
  ogm get GROUP                               group details and instances
  ogm logs GROUP [-f] [--instance ID] [--since 10m] [-q TEXT]
  ogm events GROUP
  ogm scale GROUP N
  ogm start|stop|rm GROUP
  ogm restart|recreate|reallocate GROUP INSTANCE
  ogm url GROUP [--rotate]                    print the gateway URL and its shareable token
  ogm curl GROUP [PATH] [curl args...]        call the gateway with your key

Secrets & volumes:
  ogm secret set NAME [VALUE | --from-file F] use in env as {{secret:NAME}}; stored encrypted
  ogm secret ls | rm NAME
  ogm volumes [ls] | rm GROUP [--replica N]   persistent volumes (container.volumes), pinned to a node

Data & automation:
  ogm kv tables | ls TABLE | get T K | put T K V [--ttl 10m] [--if-version N] | incr T K | rm T K | drop T
  ogm schedule [ls] | create NAME --cron "*/5 * * * *" (--queue Q | --http GROUP | --scale GROUP --replicas N) | run | rm
  ogm snapshot create GROUP [--replica N] | ls | restore ID [--node NODE] | rm ID   volume backups / migration
  ogm template [ls] | show NAME | deploy NAME [--name G]   ready-made services (postgres, redis, s3, ...)
  ogm metrics GROUP [--minutes 60]            requests, errors, latency, replicas, CPU/memory per minute
  ogm quota [show] | set --cpu N --memory-mb N --gpus N --instances N --storage-mb N   project limits (set: admin)

Access:
  ogm keys [ls] | create NAME [--project P] [--role developer|viewer|admin] [--expires 720h] | rm ID

Queues & jobs:
  ogm queue create NAME [--max-attempts N] [--timeout S] | ls | rm NAME [--force]
  ogm job submit QUEUE [--input JSON | --file F | --batch F.jsonl] [--wait]
  ogm job get QUEUE ID [--wait] | ls QUEUE [--status S] | cancel QUEUE ID

Nodes & GPUs:
  ogm nodes                                   list contributor machines
  ogm node add NAME                           create a join token + install command
  ogm node assessment NODE                    show the onboarding audit (hardware, speed, tier, advice)
  ogm node pause|resume|rm NODE_ID
  ogm gpus                                    GPU classes, availability and credit prices

Images (built-in registry, no account needed):
  ogm push [--multiarch] LOCAL_IMAGE[:TAG] [NAME[:TAG]]  push a local image -> registry.ogm/PROJECT/NAME:TAG
  ogm images                                  list images in the cluster registry
  ogm registry-proxy [--port 5078]            expose the registry on localhost for plain docker push/pull

Files:
  ogm files ls [PREFIX] | put LOCAL [REMOTE] | get REMOTE [LOCAL] | rm REMOTE | sign REMOTE [--put]
  ogm s3 credentials create [--save] | ls | rm KEY   S3-compatible storage (boto3/aws-cli/rclone; docs/s3.md)
  ogm s3 mb|rb BUCKET | ls [BUCKET[/PREFIX]] | cp SRC DST | rm s3://B/K | presign s3://B/K

AI assistants:
  ogm mcp                                     run an MCP server on stdio (for Claude Desktop/Code, Cursor...)

Global flags: --project NAME, -o json
`

// Main runs the CLI.
func Main(args []string) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return
	}
	e := &env{cfg: loadConfig(), out: os.Stdout, output: "table"}
	e.project = e.cfg.Project
	// Pull out global flags anywhere in args.
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" && i+1 < len(args):
			e.output = args[i+1]
			i++
		case a == "-o=json" || a == "--json":
			e.output = "json"
		case (a == "--project" || a == "-p") && i+1 < len(args) && args[0] != "deploy":
			e.project = args[i+1]
			i++
		case strings.HasPrefix(a, "--project="):
			e.project = strings.TrimPrefix(a, "--project=")
		default:
			rest = append(rest, a)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	e.ctx = ctx
	cmd, cargs := rest[0], rest[1:]
	switch cmd {
	case "version", "--version":
		fmt.Println("ogm", api.Version)
	case "server":
		cmdServer(e, cargs)
	case "agent":
		cmdAgent(e, cargs)
	case "dev":
		cmdDev(e, cargs)
	case "demo-app":
		cmdDemoApp(cargs)
	case "join":
		cmdJoin(e, cargs)
	case "login":
		cmdLogin(e, cargs)
	case "status":
		cmdStatus(e)
	case "invite":
		cmdInvite(e, cargs)
	case "users":
		cmdUsers(e)
	case "credits":
		cmdCredits(e)
	case "deploy":
		cmdDeploy(e, cargs)
	case "ls", "groups", "list":
		cmdList(e)
	case "get", "describe":
		cmdGet(e, cargs)
	case "ps", "instances":
		cmdPs(e, cargs)
	case "logs":
		cmdLogs(e, cargs)
	case "events":
		cmdEvents(e, cargs)
	case "scale":
		cmdScale(e, cargs)
	case "start", "stop":
		need(cargs, 1, cmd+" GROUP")
		check(e.client().GroupAction(ctx, e.project, cargs[0], cmd))
		fmt.Fprintf(e.out, "%s: %s requested\n", cargs[0], cmd)
	case "rm", "delete":
		need(cargs, 1, "rm GROUP")
		check(e.client().DeleteGroup(ctx, e.project, cargs[0]))
		fmt.Fprintf(e.out, "%s deleted\n", cargs[0])
	case "restart", "recreate", "reallocate":
		need(cargs, 2, cmd+" GROUP INSTANCE")
		check(e.client().InstanceAction(ctx, e.project, cargs[0], cargs[1], cmd))
		fmt.Fprintf(e.out, "%s: %s requested\n", cargs[1], cmd)
	case "url":
		cmdURL(e, cargs)
	case "curl":
		cmdCurl(e, cargs)
	case "queue", "queues":
		cmdQueue(e, cargs)
	case "job", "jobs":
		cmdJob(e, cargs)
	case "nodes":
		cmdNodes(e)
	case "node":
		cmdNode(e, cargs)
	case "gpus", "gpu-classes":
		cmdGPUs(e)
	case "secret", "secrets":
		cmdSecret(e, cargs)
	case "volume", "volumes":
		cmdVolumes(e, cargs)
	case "key", "keys":
		cmdKeys(e, cargs)
	case "kv":
		cmdKV(e, cargs)
	case "schedule", "schedules", "cron":
		cmdSchedule(e, cargs)
	case "snapshot", "snapshots":
		cmdSnapshot(e, cargs)
	case "metrics":
		cmdMetrics(e, cargs)
	case "quota", "quotas":
		cmdQuota(e, cargs)
	case "template", "templates":
		cmdTemplate(e, cargs)
	case "assess", "audit":
		cmdAssess(e, cargs)
	case "push":
		cmdPush(e, cargs)
	case "images":
		cmdImages(e, cargs)
	case "registry-proxy":
		cmdRegistryProxy(e, cargs)
	case "files", "file":
		cmdFiles(e, cargs)
	case "s3":
		cmdS3(e, cargs)
	case "mcp":
		cmdMCP(e, cargs)
	default:
		fatalf("unknown command %q (see `ogm help`)", cmd)
	}
}

func need(args []string, n int, use string) {
	if len(args) < n {
		fatalf("usage: ogm %s", use)
	}
}

// parse parses flags allowing them to appear after positional args.
func parse(fs *flag.FlagSet, args []string) []string {
	fs.SetOutput(os.Stderr)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return pos
}

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
