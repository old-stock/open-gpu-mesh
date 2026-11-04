package cli

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/examples"
	"github.com/old-stock/open-gpu-mesh/internal/api"
)

func pathEsc(s string) string { return url.PathEscape(s) }

func (e *env) proj() string { return "/v1/projects/" + pathEsc(e.project) }

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format(time.DateTime)
}

// ---------- API keys ----------

func cmdKeys(e *env, args []string) {
	c := e.client()
	if len(args) == 0 || args[0] == "ls" || args[0] == "list" {
		var out struct {
			Items []struct {
				ID, Name, Project, Role, User string
				ExpireTime                    *time.Time `json:"expire_time"`
				LastUsed                      *time.Time `json:"last_used_time"`
			} `json:"items"`
		}
		path := "/v1/keys"
		if len(args) > 1 && args[1] == "--all" {
			path += "?all=1"
		}
		check(c.Do(e.ctx, "GET", path, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, k := range out.Items {
			rows = append(rows, []string{k.ID, k.Name, orDash(k.Project), k.Role, k.User, fmtTime(k.ExpireTime), fmtTime(k.LastUsed)})
		}
		e.table("ID\tNAME\tPROJECT\tROLE\tUSER\tEXPIRES\tLAST USED", rows)
		return
	}
	switch args[0] {
	case "create":
		f := flag.NewFlagSet("keys create", flag.ExitOnError)
		project := f.String("project", "", "limit the key to one project (default: all your projects)")
		role := f.String("role", "developer", "developer | viewer | admin")
		expires := f.Duration("expires", 0, "lifetime, e.g. 720h (default: never)")
		pos := parse(f, args[1:])
		need(pos, 1, "keys create NAME [--project P] [--role developer|viewer|admin] [--expires 720h]")
		var out map[string]any
		check(c.Do(e.ctx, "POST", "/v1/keys", map[string]any{"name": pos[0], "project": *project, "role": *role,
			"expires_in_hours": int(expires.Hours())}, &out))
		if e.output == "json" {
			e.json(out)
			return
		}
		fmt.Fprintf(e.out, "key %s (%s) created. Save it now, it is not shown again:\n  %s\n", out["id"], out["role"], out["key"])
	case "rm", "delete", "revoke":
		need(args, 2, "keys rm ID")
		check(c.Do(e.ctx, "DELETE", "/v1/keys/"+pathEsc(args[1]), nil, nil))
		fmt.Fprintf(e.out, "key %s revoked\n", args[1])
	default:
		fatalf("usage: ogm keys [ls [--all]] | create NAME [...] | rm ID")
	}
}

// ---------- KV ----------

func cmdKV(e *env, args []string) {
	need(args, 1, "kv tables | ls TABLE | get TABLE KEY | put TABLE KEY [VALUE] | rm TABLE KEY | incr TABLE KEY | drop TABLE")
	c := e.client()
	base := e.proj() + "/kv"
	switch args[0] {
	case "tables":
		var out api.List[struct {
			Name  string `json:"name"`
			Keys  int    `json:"keys"`
			Bytes int64  `json:"bytes"`
		}]
		check(c.Do(e.ctx, "GET", base, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, t := range out.Items {
			rows = append(rows, []string{t.Name, strconv.Itoa(t.Keys), strconv.FormatInt(t.Bytes, 10)})
		}
		e.table("TABLE\tKEYS\tBYTES", rows)
	case "ls", "list":
		f := flag.NewFlagSet("kv ls", flag.ExitOnError)
		prefix := f.String("prefix", "", "only keys with this prefix")
		limit := f.Int("limit", 100, "max keys")
		vals := f.Bool("values", false, "include values")
		pos := parse(f, args[1:])
		need(pos, 1, "kv ls TABLE [--prefix P] [--limit N] [--values]")
		q := url.Values{"prefix": {*prefix}, "limit": {strconv.Itoa(*limit)}}
		if *vals {
			q.Set("values", "1")
		}
		var out struct {
			Items []struct {
				Key        string     `json:"key"`
				Version    int64      `json:"version"`
				Size       int        `json:"size"`
				Value      *string    `json:"value"`
				ExpireTime *time.Time `json:"expire_time"`
			} `json:"items"`
			NextAfter string `json:"next_after"`
		}
		check(c.Do(e.ctx, "GET", base+"/"+pathEsc(pos[0])+"?"+q.Encode(), nil, &out))
		if e.output == "json" {
			e.json(out)
			return
		}
		var rows [][]string
		for _, it := range out.Items {
			r := []string{it.Key, strconv.FormatInt(it.Version, 10), strconv.Itoa(it.Size), fmtTime(it.ExpireTime)}
			if *vals && it.Value != nil {
				r = append(r, truncateStr(*it.Value, 60))
			}
			rows = append(rows, r)
		}
		h := "KEY\tVERSION\tSIZE\tEXPIRES"
		if *vals {
			h += "\tVALUE"
		}
		e.table(h, rows)
		if out.NextAfter != "" {
			fmt.Fprintf(os.Stderr, "(more: --prefix with after=%s via the API)\n", out.NextAfter)
		}
	case "get":
		need(args, 3, "kv get TABLE KEY")
		resp, err := c.Raw(e.ctx, "GET", base+"/"+pathEsc(args[1])+"/"+args[2], nil, nil)
		check(err)
		defer resp.Body.Close()
		_, _ = io.Copy(e.out, resp.Body)
	case "put", "set":
		f := flag.NewFlagSet("kv put", flag.ExitOnError)
		file := f.String("file", "", "read the value from a file (- = stdin)")
		ttl := f.Duration("ttl", 0, "expire after, e.g. 10m")
		ifVersion := f.Int64("if-version", 0, "compare-and-set: only write if the current version matches")
		create := f.Bool("create-only", false, "fail if the key exists")
		ctype := f.String("content-type", "", "content type to store")
		pos := parse(f, args[1:])
		need(pos, 2, "kv put TABLE KEY [VALUE | --file F] [--ttl 10m] [--if-version N] [--create-only]")
		var body io.Reader
		switch {
		case *file == "-":
			body = os.Stdin
		case *file != "":
			fh, err := os.Open(*file)
			check(err)
			defer fh.Close()
			body = fh
		case len(pos) > 2:
			body = strings.NewReader(pos[2])
		default:
			body = os.Stdin
		}
		hdr := http.Header{}
		if *ifVersion > 0 {
			hdr.Set("If-Match", strconv.FormatInt(*ifVersion, 10))
		}
		if *create {
			hdr.Set("If-None-Match", "*")
		}
		if *ctype != "" {
			hdr.Set("Content-Type", *ctype)
		}
		path := base + "/" + pathEsc(pos[0]) + "/" + pos[1]
		if *ttl > 0 {
			path += "?ttl=" + strconv.Itoa(int(ttl.Seconds()))
		}
		resp, err := c.Raw(e.ctx, "PUT", path, body, hdr)
		check(err)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if e.output == "json" {
			e.json(out)
			return
		}
		fmt.Fprintf(e.out, "%s/%s = version %v\n", pos[0], pos[1], out["version"])
	case "rm", "delete":
		need(args, 3, "kv rm TABLE KEY")
		check(c.Do(e.ctx, "DELETE", base+"/"+pathEsc(args[1])+"/"+args[2], nil, nil))
		fmt.Fprintf(e.out, "deleted %s/%s\n", args[1], args[2])
	case "incr":
		f := flag.NewFlagSet("kv incr", flag.ExitOnError)
		by := f.Int("by", 1, "increment (negative to decrement)")
		pos := parse(f, args[1:])
		need(pos, 2, "kv incr TABLE KEY [--by N]")
		var out map[string]any
		check(c.Do(e.ctx, "POST", base+"/"+pathEsc(pos[0])+"/"+pos[1]+"?by="+strconv.Itoa(*by), nil, &out))
		fmt.Fprintln(e.out, out["value"])
	case "drop":
		need(args, 2, "kv drop TABLE")
		var out map[string]any
		check(c.Do(e.ctx, "DELETE", base+"/"+pathEsc(args[1]), nil, &out))
		fmt.Fprintf(e.out, "dropped %s (%v keys)\n", args[1], out["deleted"])
	default:
		fatalf("usage: ogm kv tables|ls|get|put|rm|incr|drop")
	}
}

func truncateStr(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------- schedules ----------

func cmdSchedule(e *env, args []string) {
	c := e.client()
	base := e.proj() + "/schedules"
	if len(args) == 0 || args[0] == "ls" || args[0] == "list" {
		var out struct {
			Items []struct {
				Name       string          `json:"name"`
				Schedule   string          `json:"schedule"`
				Enabled    *bool           `json:"enabled"`
				Job        json.RawMessage `json:"job"`
				HTTP       json.RawMessage `json:"http"`
				Scale      json.RawMessage `json:"scale"`
				NextRun    *time.Time      `json:"next_run_time"`
				LastRun    *time.Time      `json:"last_run_time"`
				LastStatus string          `json:"last_status"`
				Runs       int             `json:"runs"`
			} `json:"items"`
		}
		check(c.Do(e.ctx, "GET", base, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, s := range out.Items {
			action := "job"
			if len(s.HTTP) > 0 && string(s.HTTP) != "null" {
				action = "http"
			} else if len(s.Scale) > 0 && string(s.Scale) != "null" {
				action = "scale"
			}
			en := "yes"
			if s.Enabled != nil && !*s.Enabled {
				en = "no"
			}
			rows = append(rows, []string{s.Name, s.Schedule, action, en, fmtTime(s.NextRun), fmtTime(s.LastRun), orDash(s.LastStatus), strconv.Itoa(s.Runs)})
		}
		e.table("NAME\tSCHEDULE\tACTION\tENABLED\tNEXT\tLAST\tSTATUS\tRUNS", rows)
		return
	}
	switch args[0] {
	case "create", "set":
		f := flag.NewFlagSet("schedule create", flag.ExitOnError)
		cron := f.String("cron", "", `5-field cron, e.g. "*/5 * * * *" (server time zone)`)
		queue := f.String("queue", "", "action: submit a job to this queue")
		input := f.String("input", "{}", "job input JSON (with --queue)")
		group := f.String("http", "", "action: call this container group's gateway")
		method := f.String("method", "POST", "HTTP method (with --http)")
		path := f.String("path", "/", "HTTP path (with --http)")
		body := f.String("body", "", "HTTP body (with --http)")
		scale := f.String("scale", "", "action: set this group's replicas")
		replicas := f.Int("replicas", 0, "replica count (with --scale)")
		disabled := f.Bool("disabled", false, "create disabled")
		pos := parse(f, args[1:])
		need(pos, 1, `schedule create NAME --cron "0 * * * *" (--queue Q [--input JSON] | --http GROUP [--path /x] | --scale GROUP --replicas N)`)
		spec := map[string]any{"name": pos[0], "schedule": *cron, "enabled": !*disabled}
		switch {
		case *queue != "":
			if !json.Valid([]byte(*input)) {
				fatalf("--input is not valid JSON")
			}
			spec["job"] = map[string]any{"queue": *queue, "input": json.RawMessage(*input)}
		case *group != "":
			spec["http"] = map[string]any{"group": *group, "method": *method, "path": *path, "body": *body}
		case *scale != "":
			spec["scale"] = map[string]any{"group": *scale, "replicas": *replicas}
		}
		check(c.Do(e.ctx, "PUT", base+"/"+pathEsc(pos[0]), spec, nil))
		fmt.Fprintf(e.out, "schedule %s saved\n", pos[0])
	case "get":
		need(args, 2, "schedule get NAME")
		var out any
		check(c.Do(e.ctx, "GET", base+"/"+pathEsc(args[1]), nil, &out))
		e.json(out)
	case "run":
		need(args, 2, "schedule run NAME")
		var out map[string]string
		check(c.Do(e.ctx, "POST", base+"/"+pathEsc(args[1])+"/run", nil, &out))
		fmt.Fprintln(e.out, out["status"])
	case "rm", "delete":
		need(args, 2, "schedule rm NAME")
		check(c.Do(e.ctx, "DELETE", base+"/"+pathEsc(args[1]), nil, nil))
		fmt.Fprintf(e.out, "schedule %s deleted\n", args[1])
	default:
		fatalf("usage: ogm schedule [ls] | create NAME ... | get NAME | run NAME | rm NAME")
	}
}

// ---------- snapshots ----------

type snapshotInfo struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Group         string     `json:"group"`
	Replica       int        `json:"replica"`
	Volumes       []string   `json:"volumes"`
	NodeID        string     `json:"node_id"`
	Status        string     `json:"status"`
	Bytes         int64      `json:"bytes"`
	Error         string     `json:"error"`
	RestoreStatus string     `json:"restore_status"`
	CreateTime    time.Time  `json:"create_time"`
	FinishTime    *time.Time `json:"finish_time"`
}

func (e *env) waitSnapshot(id string, restore bool) snapshotInfo {
	c := e.client()
	for {
		var sn snapshotInfo
		check(c.Do(e.ctx, "GET", e.proj()+"/snapshots/"+pathEsc(id), nil, &sn))
		if !restore && sn.Status != "pending" {
			return sn
		}
		if restore && sn.RestoreStatus != "restoring" {
			return sn
		}
		select {
		case <-e.ctx.Done():
			os.Exit(130)
		case <-time.After(time.Second):
		}
	}
}

func cmdSnapshot(e *env, args []string) {
	c := e.client()
	if len(args) == 0 || args[0] == "ls" || args[0] == "list" {
		path := e.proj() + "/snapshots"
		if len(args) > 1 {
			path += "?group=" + url.QueryEscape(args[1])
		}
		var out api.List[snapshotInfo]
		check(c.Do(e.ctx, "GET", path, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, s := range out.Items {
			st := s.Status
			if s.Error != "" {
				st += ": " + truncateStr(s.Error, 40)
			}
			rows = append(rows, []string{s.ID, orDash(s.Name), s.Group, strconv.Itoa(s.Replica), st, humanBytes(s.Bytes), orDash(s.RestoreStatus), s.CreateTime.Local().Format(time.DateTime)})
		}
		e.table("ID\tNAME\tGROUP\tREPLICA\tSTATUS\tSIZE\tRESTORE\tCREATED", rows)
		return
	}
	switch args[0] {
	case "create":
		f := flag.NewFlagSet("snapshot create", flag.ExitOnError)
		replica := f.Int("replica", 0, "replica index")
		name := f.String("name", "", "label")
		wait := f.Bool("wait", true, "wait until the snapshot is stored")
		pos := parse(f, args[1:])
		need(pos, 1, "snapshot create GROUP [--replica N] [--name LABEL] [--wait=false]")
		var sn snapshotInfo
		check(c.Do(e.ctx, "POST", e.proj()+"/volumes/"+pathEsc(pos[0])+"/snapshots", map[string]any{"replica": *replica, "name": *name}, &sn))
		if *wait {
			sn = e.waitSnapshot(sn.ID, false)
		}
		if e.output == "json" {
			e.json(sn)
			return
		}
		if sn.Status == "failed" {
			fatalf("snapshot %s failed: %s", sn.ID, sn.Error)
		}
		fmt.Fprintf(e.out, "snapshot %s %s (%s)\n", sn.ID, sn.Status, humanBytes(sn.Bytes))
	case "restore":
		f := flag.NewFlagSet("snapshot restore", flag.ExitOnError)
		group := f.String("group", "", "target group (default: the snapshot's)")
		replica := f.Int("replica", -1, "target replica (default: the snapshot's)")
		node := f.String("node", "", "target node name or id (default: where the replica lives) — use to migrate")
		wait := f.Bool("wait", true, "wait for the restore to finish")
		pos := parse(f, args[1:])
		need(pos, 1, "snapshot restore ID [--group G] [--replica N] [--node NODE]")
		body := map[string]any{"group": *group, "node": *node}
		if *replica >= 0 {
			body["replica"] = *replica
		}
		var out map[string]any
		check(c.Do(e.ctx, "POST", e.proj()+"/snapshots/"+pathEsc(pos[0])+"/restore", body, &out))
		if *wait {
			sn := e.waitSnapshot(pos[0], true)
			if strings.HasPrefix(sn.RestoreStatus, "failed") {
				fatalf("restore %s", sn.RestoreStatus)
			}
			fmt.Fprintf(e.out, "snapshot %s restored into %v replica %v on %v\n", pos[0], out["group"], out["replica"], out["node_id"])
			return
		}
		e.json(out)
	case "rm", "delete":
		need(args, 2, "snapshot rm ID")
		check(c.Do(e.ctx, "DELETE", e.proj()+"/snapshots/"+pathEsc(args[1]), nil, nil))
		fmt.Fprintf(e.out, "snapshot %s deleted\n", args[1])
	default:
		fatalf("usage: ogm snapshot [ls [GROUP]] | create GROUP | restore ID | rm ID")
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---------- metrics ----------

func cmdMetrics(e *env, args []string) {
	f := flag.NewFlagSet("metrics", flag.ExitOnError)
	minutes := f.Int("minutes", 60, "window")
	pos := parse(f, args)
	need(pos, 1, "metrics GROUP [--minutes 60]")
	var out struct {
		Points []struct {
			Time     time.Time `json:"time"`
			Requests int       `json:"requests"`
			Err4xx   int       `json:"errors_4xx"`
			Err5xx   int       `json:"errors_5xx"`
			P50      float64   `json:"p50_ms"`
			P95      float64   `json:"p95_ms"`
			Ready    int       `json:"ready_replicas"`
			Desired  int       `json:"desired_replicas"`
			CPU      float64   `json:"cpu_percent"`
			Mem      float64   `json:"memory_mb"`
		} `json:"points"`
		Summary map[string]any `json:"summary"`
	}
	check(e.client().Do(e.ctx, "GET", e.proj()+"/containers/"+pathEsc(pos[0])+"/metrics?minutes="+strconv.Itoa(*minutes), nil, &out))
	if e.output == "json" {
		e.json(out)
		return
	}
	var rows [][]string
	for _, p := range out.Points {
		rows = append(rows, []string{p.Time.Local().Format("15:04"), strconv.Itoa(p.Requests), strconv.Itoa(p.Err4xx), strconv.Itoa(p.Err5xx),
			fmt.Sprint(p.P50), fmt.Sprint(p.P95), fmt.Sprintf("%d/%d", p.Ready, p.Desired), fmt.Sprint(p.CPU), fmt.Sprint(p.Mem)})
	}
	e.table("MINUTE\tREQS\t4XX\t5XX\tP50 MS\tP95 MS\tREADY\tCPU %\tMEM MB", rows)
	fmt.Fprintf(e.out, "\ntotal: %v requests, %v 4xx, %v 5xx, p50 %v ms, p95 %v ms, p99 %v ms\n",
		out.Summary["requests"], out.Summary["errors_4xx"], out.Summary["errors_5xx"], out.Summary["p50_ms"], out.Summary["p95_ms"], out.Summary["p99_ms"])
}

// ---------- quotas ----------

func cmdQuota(e *env, args []string) {
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	type quota struct {
		CPU       int `json:"cpu"`
		MemoryMB  int `json:"memory_mb"`
		GPUs      int `json:"gpus"`
		Instances int `json:"instances"`
		StorageMB int `json:"storage_mb"`
	}
	var out struct {
		Project string `json:"project"`
		Limits  quota  `json:"limits"`
		Usage   struct {
			CPU          int   `json:"cpu"`
			MemoryMB     int   `json:"memory_mb"`
			GPUs         int   `json:"gpus"`
			Instances    int   `json:"instances"`
			StorageBytes int64 `json:"storage_bytes"`
		} `json:"usage"`
	}
	switch sub {
	case "show", "get":
		parse(flag.NewFlagSet("quota", flag.ExitOnError), args)
		check(e.client().Do(e.ctx, "GET", e.proj()+"/quota", nil, &out))
	case "set":
		f := flag.NewFlagSet("quota set", flag.ExitOnError)
		var q quota
		f.IntVar(&q.CPU, "cpu", 0, "vCPUs (0 = unlimited)")
		f.IntVar(&q.MemoryMB, "memory-mb", 0, "memory in MB")
		f.IntVar(&q.GPUs, "gpus", 0, "GPUs")
		f.IntVar(&q.Instances, "instances", 0, "instances")
		f.IntVar(&q.StorageMB, "storage-mb", 0, "files + snapshots + S3, in MB")
		parse(f, args)
		check(e.client().Do(e.ctx, "PUT", e.proj()+"/quota", q, &out))
	default:
		fatalf("usage: ogm quota [show] | set [--cpu N] [--memory-mb N] [--gpus N] [--instances N] [--storage-mb N]")
	}
	if e.output == "json" {
		e.json(out)
		return
	}
	lim := func(n int) string {
		if n == 0 {
			return "unlimited"
		}
		return strconv.Itoa(n)
	}
	u := out.Usage
	e.table("RESOURCE\tUSED (PEAK)\tLIMIT", [][]string{
		{"cpu", strconv.Itoa(u.CPU), lim(out.Limits.CPU)},
		{"memory_mb", strconv.Itoa(u.MemoryMB), lim(out.Limits.MemoryMB)},
		{"gpus", strconv.Itoa(u.GPUs), lim(out.Limits.GPUs)},
		{"instances", strconv.Itoa(u.Instances), lim(out.Limits.Instances)},
		{"storage_mb", fmt.Sprintf("%.1f", float64(u.StorageBytes)/(1<<20)), lim(out.Limits.StorageMB)},
	})
}

// ---------- templates ----------

var templateSecretRe = regexp.MustCompile(`\{\{\s*secret:([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*\}\}`)

func templateNames() []string {
	ents, _ := fs.ReadDir(examples.FS, ".")
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
		}
	}
	sort.Strings(out)
	return out
}

func templateSummary(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	if sc.Scan() {
		return strings.TrimSpace(strings.TrimPrefix(sc.Text(), "#"))
	}
	return ""
}

func cmdTemplate(e *env, args []string) {
	if len(args) == 0 || args[0] == "ls" || args[0] == "list" {
		var rows [][]string
		for _, n := range templateNames() {
			b, _ := examples.FS.ReadFile(n + ".yaml")
			rows = append(rows, []string{n, truncateStr(templateSummary(b), 90)})
		}
		e.table("TEMPLATE\tDESCRIPTION", rows)
		return
	}
	switch args[0] {
	case "show":
		need(args, 2, "template show NAME")
		b, err := examples.FS.ReadFile(args[1] + ".yaml")
		if err != nil {
			fatalf("no template %q (see `ogm template ls`)", args[1])
		}
		_, _ = e.out.Write(b)
	case "deploy":
		f := flag.NewFlagSet("template deploy", flag.ExitOnError)
		name := f.String("name", "", "rename the group (single-group templates only)")
		gen := f.Bool("generate-secrets", true, "create any missing {{secret:...}} with a random value")
		dry := f.Bool("dry-run", false, "print the documents without deploying")
		pos := parse(f, args[1:])
		need(pos, 1, "template deploy NAME [--name GROUP] [--generate-secrets=false] [--dry-run]")
		b, err := examples.FS.ReadFile(pos[0] + ".yaml")
		if err != nil {
			fatalf("no template %q (see `ogm template ls`)", pos[0])
		}
		raw := ExpandVars(string(b), os.Getenv)
		if *name != "" {
			if strings.Count(raw, "\n---") > 0 {
				fatalf("--name only works for single-group templates")
			}
			raw = regexp.MustCompile(`(?m)^name:\s*\S+`).ReplaceAllString(raw, "name: "+*name)
		}
		if *gen && !*dry {
			c := e.client()
			var have api.List[api.Secret]
			check(c.Do(e.ctx, "GET", e.proj()+"/secrets", nil, &have))
			exists := map[string]bool{}
			for _, s := range have.Items {
				exists[s.Name] = true
			}
			for _, m := range templateSecretRe.FindAllStringSubmatch(raw, -1) {
				if exists[m[1]] {
					continue
				}
				buf := make([]byte, 18)
				_, _ = rand.Read(buf)
				check(c.Do(e.ctx, "PUT", e.proj()+"/secrets/"+pathEsc(m[1]), map[string]string{"value": hex.EncodeToString(buf)}, nil))
				exists[m[1]] = true
				fmt.Fprintf(e.out, "secret %s created with a random value\n", m[1])
			}
		}
		deployRaw(e, []byte(raw), pos[0]+".yaml", e.project, *dry)
	default:
		fatalf("usage: ogm template [ls] | show NAME | deploy NAME")
	}
}
