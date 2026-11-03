package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// deployDoc is one YAML document in a deploy file.
type deployDoc struct {
	Kind string `yaml:"kind"`
	// queue fields
	Name        string            `yaml:"name"`
	Project     string            `yaml:"project"`
	Description string            `yaml:"description"`
	Settings    api.QueueSettings `yaml:"settings"`
}

func cmdDeploy(e *env, args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	file := fs.String("f", "", "YAML or JSON file ('-' for stdin)")
	project := fs.String("project", e.project, "project")
	dry := fs.Bool("dry-run", false, "print the parsed specs without deploying")
	parse(fs, args)
	if *file == "" {
		fatalf("usage: ogm deploy -f app.yaml")
	}
	var raw []byte
	var err error
	if *file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*file)
	}
	check(err)
	raw = []byte(os.ExpandEnv(string(raw)))
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	c := e.client()
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			check(fmt.Errorf("parse %s: %w", *file, err))
		}
		var meta deployDoc
		check(node.Decode(&meta))
		proj := *project
		if meta.Project != "" {
			proj = meta.Project
		}
		switch meta.Kind {
		case "queue":
			if *dry {
				e.json(meta)
				continue
			}
			if _, err := c.Queue(e.ctx, proj, meta.Name); err == nil {
				check(c.Do(e.ctx, "PATCH", "/v1/projects/"+proj+"/queues/"+meta.Name, map[string]any{"description": meta.Description, "settings": meta.Settings}, nil))
				fmt.Fprintf(e.out, "queue %s/%s updated\n", proj, meta.Name)
			} else {
				check(c.Do(e.ctx, "POST", "/v1/projects/"+proj+"/queues", map[string]any{"name": meta.Name, "description": meta.Description, "settings": meta.Settings}, nil))
				fmt.Fprintf(e.out, "queue %s/%s created\n", proj, meta.Name)
			}
		case "", "group", "container_group":
			var spec api.ContainerGroupSpec
			check(node.Decode(&spec))
			if *dry {
				e.json(spec)
				continue
			}
			g, err := c.Deploy(e.ctx, proj, spec)
			check(err)
			fmt.Fprintf(e.out, "container group %s/%s deployed (version %d, %d replicas, %s)\n", proj, g.Name, g.Version, g.DesiredReplicas, g.CurrentState.Status)
			if g.GatewayURL != "" {
				fmt.Fprintf(e.out, "  gateway: %s\n", g.GatewayURL)
			}
		default:
			fatalf("unknown kind %q (use group or queue)", meta.Kind)
		}
	}
}

func cmdList(e *env) {
	gs, err := e.client().Groups(e.ctx, e.project)
	check(err)
	if e.output == "json" {
		e.json(gs)
		return
	}
	var rows [][]string
	for _, g := range gs {
		c := g.CurrentState.InstanceStatusCounts
		rows = append(rows, []string{
			g.Name, g.CurrentState.Status, fmt.Sprintf("%d/%d", c.Ready, g.DesiredReplicas), g.Priority, g.Container.Image,
			gpuReq(g.Container.Resources), strconv.Itoa(g.Version), orDash(g.GatewayURL),
		})
	}
	e.table("NAME\tSTATUS\tREADY\tPRIORITY\tIMAGE\tGPU\tVER\tGATEWAY", rows)
}

func gpuReq(r api.Resources) string {
	if !r.WantsGPU() {
		return "cpu"
	}
	s := strconv.Itoa(r.GPUs()) + "x "
	switch {
	case len(r.GPUClasses) > 0:
		s += strings.Join(r.GPUClasses, "|")
	case r.MinVRAMMB > 0:
		s += fmt.Sprintf(">=%dGB", r.MinVRAMMB/1024)
	default:
		s += "any"
	}
	return s
}

func cmdGet(e *env, args []string) {
	need(args, 1, "get GROUP")
	g, err := e.client().Group(e.ctx, e.project, args[0])
	check(err)
	if e.output == "json" {
		e.json(g)
		return
	}
	c := g.CurrentState.InstanceStatusCounts
	fmt.Fprintf(e.out, "%s/%s  (%s)  owner=%s  version=%d\n", g.Project, g.Name, g.CurrentState.Status, g.Owner, g.Version)
	fmt.Fprintf(e.out, "  image:     %s\n", g.Container.Image)
	fmt.Fprintf(e.out, "  resources: %d vCPU, %d MB RAM, %s, %d GB disk, priority %s\n", g.Container.Resources.CPU, g.Container.Resources.MemoryMB, gpuReq(g.Container.Resources), g.Container.Resources.StorageGB, g.Priority)
	fmt.Fprintf(e.out, "  replicas:  desired %d | ready %d running %d creating %d downloading %d allocating %d stopping %d\n",
		g.DesiredReplicas, c.Ready, c.Running, c.Creating, c.Downloading, c.Allocating, c.Stopping)
	if g.CurrentState.Description != "" {
		fmt.Fprintf(e.out, "  note:      %s\n", g.CurrentState.Description)
	}
	if g.GatewayURL != "" {
		auth := "public"
		if g.Networking.Auth {
			auth = "auth required"
		}
		fmt.Fprintf(e.out, "  gateway:   %s (%s, %s)\n", g.GatewayURL, g.Networking.LoadBalancer, auth)
	}
	if q := g.QueueConnection; q != nil {
		st := g.QueueStats
		fmt.Fprintf(e.out, "  queue:     %s -> :%d%s", q.QueueName, q.Port, q.Path)
		if st != nil {
			fmt.Fprintf(e.out, "  (pending %d, running %d, done %d, failed %d)", st.Pending, st.Running, st.Succeeded, st.Failed)
		}
		fmt.Fprintln(e.out)
	}
	if a := g.QueueAutoscaler; a != nil {
		fmt.Fprintf(e.out, "  autoscale: %d..%d replicas, %d jobs/replica, every %ds\n", a.MinReplicas, a.MaxReplicas, a.DesiredQueueLength, a.PollingPeriod)
	}
	fmt.Fprintln(e.out)
	printInstances(e, g.Instances)
}

func printInstances(e *env, insts []api.Instance) {
	var rows [][]string
	for _, in := range insts {
		state := in.State
		if in.State == api.InstDownloading && in.PullingProgress > 0 {
			state = fmt.Sprintf("downloading %.0f%%", in.PullingProgress)
		}
		ready := "no"
		if in.Ready {
			ready = "yes"
		}
		util := "-"
		if in.GPUName != "" {
			util = fmt.Sprintf("%.0f%% %.0fC", in.Metrics.GPUUtil, in.Metrics.GPUTempC)
		}
		rows = append(rows, []string{in.ID, state, ready, orDash(in.NodeName), orDash(in.GPUName), util, orDash(in.Country),
			strconv.Itoa(in.Version), strconv.Itoa(in.DeletionCost), ago(in.RunningTime), orDash(in.Message)})
	}
	e.table("INSTANCE\tSTATE\tREADY\tNODE\tGPU\tUTIL\tCOUNTRY\tVER\tDEL.COST\tUP\tMESSAGE", rows)
}

func cmdPs(e *env, args []string) {
	need(args, 1, "ps GROUP")
	insts, err := e.client().Instances(e.ctx, e.project, args[0])
	check(err)
	if e.output == "json" {
		e.json(insts)
		return
	}
	printInstances(e, insts)
}

func cmdLogs(e *env, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow")
	inst := fs.String("instance", "", "only this instance")
	since := fs.String("since", "", "e.g. 10m or RFC3339")
	q := fs.String("q", "", "substring filter")
	limit := fs.Int("n", 200, "number of lines")
	pos := parse(fs, args)
	need(pos, 1, "logs GROUP [-f]")
	v := url.Values{"limit": {strconv.Itoa(*limit)}}
	if *inst != "" {
		v.Set("instance", *inst)
	}
	if *since != "" {
		v.Set("since", *since)
	}
	if *q != "" {
		v.Set("q", *q)
	}
	print := func(l api.LogEntry) {
		if e.output == "json" {
			b, _ := json.Marshal(l)
			fmt.Fprintln(e.out, string(b))
			return
		}
		id := l.InstanceID
		if len(id) > 10 {
			id = id[len(id)-6:]
		}
		fmt.Fprintf(e.out, "%s %s %s\n", l.Time.Local().Format("15:04:05"), id, l.Line)
	}
	c := e.client()
	if *follow {
		err := c.FollowLogs(e.ctx, e.project, pos[0], v, print)
		if e.ctx.Err() == nil {
			check(err)
		}
		return
	}
	logs, err := c.Logs(e.ctx, e.project, pos[0], v)
	check(err)
	for _, l := range logs {
		print(l)
	}
}

func cmdEvents(e *env, args []string) {
	need(args, 1, "events GROUP")
	evs, err := e.client().Events(e.ctx, e.project, args[0], 100)
	check(err)
	if e.output == "json" {
		e.json(evs)
		return
	}
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		inst := ev.InstanceID
		if inst == "" {
			inst = "-"
		}
		fmt.Fprintf(e.out, "%s  %-22s %s\n", ev.Time.Local().Format("Jan 02 15:04:05"), inst, ev.Message)
	}
}

func cmdScale(e *env, args []string) {
	need(args, 2, "scale GROUP N")
	n, err := strconv.Atoi(args[1])
	if err != nil {
		fatalf("replicas must be a number")
	}
	g, err := e.client().PatchGroup(e.ctx, e.project, args[0], map[string]any{"replicas": n})
	check(err)
	fmt.Fprintf(e.out, "%s scaled to %d replicas\n", g.Name, g.DesiredReplicas)
}

func cmdURL(e *env, args []string) {
	rotate := len(args) > 1 && args[len(args)-1] == "--rotate"
	if rotate {
		args = args[:len(args)-1]
	}
	need(args, 1, "url GROUP [--rotate]")
	g, err := e.client().Group(e.ctx, e.project, args[0])
	check(err)
	if rotate {
		check(e.client().Do(e.ctx, "POST", "/v1/projects/"+e.project+"/containers/"+args[0]+"/rotate-gateway-token", nil, g))
	}
	if g.GatewayURL == "" {
		fatalf("%s has no gateway (set networking.port)", g.Name)
	}
	fmt.Fprintln(e.out, g.GatewayURL)
	if g.Networking.Auth {
		fmt.Fprintf(os.Stderr, "auth: header 'OGM-Api-Key: <key>', 'Authorization: Bearer <key>', or ?ogm_token=<key>\n")
		fmt.Fprintf(os.Stderr, "shareable token for this endpoint only (revoke: ogm url %s --rotate): %s\n", g.Name, g.GatewayToken)
	}
}

func cmdCurl(e *env, args []string) {
	need(args, 1, "curl GROUP [PATH] [curl args...]")
	g, err := e.client().Group(e.ctx, e.project, args[0])
	check(err)
	if g.GatewayURL == "" {
		fatalf("%s has no gateway", g.Name)
	}
	path := ""
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		path = strings.TrimPrefix(rest[0], "/")
		rest = rest[1:]
	}
	cargs := append([]string{"-sS", "-H", "OGM-Api-Key: " + e.cfg.APIKey}, rest...)
	cargs = append(cargs, g.GatewayURL+path)
	cmd := exec.CommandContext(e.ctx, "curl", cargs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

func cmdQueue(e *env, args []string) {
	need(args, 1, "queue create|ls|rm|get ...")
	c := e.client()
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("queue create", flag.ExitOnError)
		attempts := fs.Int("max-attempts", 0, "attempts per job (default 3, max 20)")
		timeout := fs.Int("timeout", 0, "per-attempt timeout seconds (default 3600)")
		backoff := fs.Int("backoff", 0, "retry backoff base seconds (default 2)")
		retry4xx := fs.Bool("retry-4xx", false, "also retry HTTP 4xx responses")
		pos := parse(fs, args[1:])
		need(pos, 1, "queue create NAME")
		q, err := c.CreateQueue(e.ctx, e.project, pos[0], api.QueueSettings{MaxAttempts: *attempts, JobTimeoutS: *timeout, RetryBackoffBaseS: *backoff, RetryOn4xx: *retry4xx})
		check(err)
		fmt.Fprintf(e.out, "queue %s created (max %d attempts, %ds timeout)\n", q.Name, q.Settings.MaxAttempts, q.Settings.JobTimeoutS)
	case "ls", "list":
		qs, err := c.Queues(e.ctx, e.project)
		check(err)
		if e.output == "json" {
			e.json(qs)
			return
		}
		var rows [][]string
		for _, q := range qs {
			rows = append(rows, []string{q.Name, strconv.Itoa(q.Stats.Pending), strconv.Itoa(q.Stats.Running), strconv.Itoa(q.Stats.Succeeded),
				strconv.Itoa(q.Stats.Failed), strconv.Itoa(q.Stats.Cancelled), orDash(strings.Join(q.Groups, ","))})
		}
		e.table("QUEUE\tPENDING\tRUNNING\tSUCCEEDED\tFAILED\tCANCELLED\tGROUPS", rows)
	case "get":
		need(args, 2, "queue get NAME")
		q, err := c.Queue(e.ctx, e.project, args[1])
		check(err)
		e.json(q)
	case "rm", "delete":
		fs := flag.NewFlagSet("queue rm", flag.ExitOnError)
		force := fs.Bool("force", false, "detach container groups and delete")
		pos := parse(fs, args[1:])
		need(pos, 1, "queue rm NAME [--force]")
		path := "/v1/projects/" + e.project + "/queues/" + pos[0]
		if *force {
			path += "?force=1"
		}
		check(c.Do(e.ctx, "DELETE", path, nil, nil))
		fmt.Fprintf(e.out, "queue %s deleted\n", pos[0])
	default:
		fatalf("usage: ogm queue create|ls|get|rm")
	}
}

func cmdJob(e *env, args []string) {
	need(args, 1, "job submit|get|ls|cancel ...")
	c := e.client()
	switch args[0] {
	case "submit":
		fs := flag.NewFlagSet("job submit", flag.ExitOnError)
		input := fs.String("input", "", "job input JSON")
		file := fs.String("file", "", "read input JSON from file")
		batch := fs.String("batch", "", "JSONL file: one input per line")
		webhook := fs.String("webhook", "", "webhook URL for completion")
		wait := fs.Bool("wait", false, "wait for the result")
		pos := parse(fs, args[1:])
		need(pos, 1, "job submit QUEUE --input JSON")
		if *batch != "" {
			f, err := os.Open(*batch)
			check(err)
			defer f.Close()
			var jps []api.JobPrototype
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 16<<20)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				if !json.Valid([]byte(line)) {
					fatalf("invalid JSON line: %s", line)
				}
				jps = append(jps, api.JobPrototype{Input: json.RawMessage(line), Webhook: *webhook})
			}
			jobs, err := c.SubmitJobs(e.ctx, e.project, pos[0], jps)
			check(err)
			fmt.Fprintf(e.out, "submitted %d jobs to %s\n", len(jobs), pos[0])
			return
		}
		in := *input
		if *file != "" {
			b, err := os.ReadFile(*file)
			check(err)
			in = string(b)
		}
		if in == "" {
			in = "{}"
		}
		if !json.Valid([]byte(in)) {
			fatalf("--input must be valid JSON")
		}
		j, err := c.SubmitJob(e.ctx, e.project, pos[0], api.JobPrototype{Input: json.RawMessage(in), Webhook: *webhook})
		check(err)
		if !*wait {
			if e.output == "json" {
				e.json(j)
			} else {
				fmt.Fprintln(e.out, j.ID)
			}
			return
		}
		for j.Status == api.JobPending || j.Status == api.JobRunning {
			j, err = c.Job(e.ctx, e.project, pos[0], j.ID, 30)
			check(err)
		}
		printJob(e, j)
		if j.Status != api.JobSucceeded {
			os.Exit(1)
		}
	case "get":
		fs := flag.NewFlagSet("job get", flag.ExitOnError)
		wait := fs.Bool("wait", false, "wait for completion")
		pos := parse(fs, args[1:])
		need(pos, 2, "job get QUEUE ID")
		w := 0
		if *wait {
			w = 60
		}
		j, err := c.Job(e.ctx, e.project, pos[0], pos[1], w)
		check(err)
		printJob(e, j)
	case "ls", "list":
		fs := flag.NewFlagSet("job ls", flag.ExitOnError)
		status := fs.String("status", "", "filter by status")
		limit := fs.Int("n", 50, "max jobs")
		pos := parse(fs, args[1:])
		need(pos, 1, "job ls QUEUE")
		jobs, err := c.Jobs(e.ctx, e.project, pos[0], *status, *limit)
		check(err)
		if e.output == "json" {
			e.json(jobs)
			return
		}
		var rows [][]string
		for _, j := range jobs {
			t := j.CreateTime
			rows = append(rows, []string{j.ID, j.Status, fmt.Sprintf("%d/%d", j.Attempts, j.MaxAttempts), orDash(j.InstanceID), ago(&t), orDash(truncate(j.Error, 60))})
		}
		e.table("JOB\tSTATUS\tATTEMPTS\tINSTANCE\tAGE\tERROR", rows)
	case "cancel":
		need(args, 3, "job cancel QUEUE ID")
		check(c.CancelJob(e.ctx, e.project, args[1], args[2]))
		fmt.Fprintf(e.out, "%s cancelled\n", args[2])
	default:
		fatalf("usage: ogm job submit|get|ls|cancel")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func printJob(e *env, j *api.Job) {
	if e.output == "json" {
		e.json(j)
		return
	}
	fmt.Fprintf(e.out, "job %s: %s (attempt %d/%d)\n", j.ID, j.Status, j.Attempts, j.MaxAttempts)
	if j.Error != "" {
		fmt.Fprintf(e.out, "error: %s\n", j.Error)
	}
	for _, ev := range j.Events {
		extra := ""
		if ev.InstanceID != "" {
			extra = " on " + ev.InstanceID
		}
		if ev.Error != "" {
			extra += ": " + ev.Error
		}
		fmt.Fprintf(e.out, "  %s %s%s\n", ev.Time.Local().Format(time.TimeOnly), ev.Action, extra)
	}
	if len(j.Output) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, j.Output, "", "  ") == nil {
			fmt.Fprintf(e.out, "output:\n%s\n", pretty.String())
		}
	}
}
