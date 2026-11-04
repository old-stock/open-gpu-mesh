package cli

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/assess"
	"github.com/old-stock/open-gpu-mesh/internal/client"
)

func cmdJoin(e *env, args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	name := fs.String("name", "", "your user name (lowercase)")
	pos := parse(fs, args)
	need(pos, 2, "join SERVER INVITE_CODE --name NAME")
	if *name == "" {
		fatalf("--name is required")
	}
	c := client.New(pos[0], "")
	var u api.User
	check(c.Do(e.ctx, "POST", "/v1/join", map[string]string{"code": pos[1], "name": *name}, &u))
	check(saveConfig(Config{Server: strings.TrimRight(pos[0], "/"), APIKey: u.APIKey, Project: u.Name}))
	fmt.Fprintf(e.out, "welcome, %s! You have %.1f starter credits.\n", u.Name, u.Credits)
	fmt.Fprintf(e.out, "your API key: %s (saved to %s)\n", u.APIKey, configPath())
	fmt.Fprintf(e.out, "your personal project is %q; the shared project is \"default\".\n", u.Name)
	fmt.Fprintf(e.out, "next: contribute a GPU with `ogm node add my-pc`, or deploy with `ogm deploy -f app.yaml`.\n")
}

func cmdLogin(e *env, args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	key := fs.String("key", "", "API key")
	project := fs.String("default-project", "default", "default project")
	pos := parse(fs, args)
	need(pos, 1, "login SERVER --key API_KEY")
	if *key == "" {
		fatalf("--key is required")
	}
	c := client.New(pos[0], *key)
	u, err := c.Me(e.ctx)
	check(err)
	check(saveConfig(Config{Server: strings.TrimRight(pos[0], "/"), APIKey: *key, Project: *project}))
	fmt.Fprintf(e.out, "logged in to %s as %s (%s)\n", pos[0], u.Name, u.Role)
}

func cmdStatus(e *env) {
	c := e.client()
	ci, err := c.Cluster(e.ctx)
	check(err)
	me, err := c.Me(e.ctx)
	check(err)
	if e.output == "json" {
		e.json(map[string]any{"cluster": ci, "me": me})
		return
	}
	fmt.Fprintf(e.out, "%s (ogm %s) at %s\n", ci.Name, ci.Version, ci.PublicURL)
	fmt.Fprintf(e.out, "  nodes:     %d online / %d total\n", ci.NodesOnline, ci.Nodes)
	fmt.Fprintf(e.out, "  GPUs:      %d online, %d free, %.0f GB VRAM pooled\n", ci.GPUs, ci.GPUsFree, ci.TotalVRAMGB)
	fmt.Fprintf(e.out, "  members:   %d\n", ci.Users)
	fmt.Fprintf(e.out, "  workloads: %d groups, %d instances running, %d jobs pending\n", ci.Groups, ci.Instances, ci.JobsPending)
	fmt.Fprintf(e.out, "you: %s (%s), %.2f credits (contributed %.2f, consumed %.2f), %d node(s); project %q\n",
		me.Name, me.Role, me.Credits, me.Contributed, me.Consumed, me.Nodes, e.project)
}

func cmdInvite(e *env, args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	uses := fs.Int("uses", 1, "how many people can use this invite")
	admin := fs.Bool("admin", false, "invite as admin")
	ttl := fs.Int("ttl-hours", 168, "expiry in hours")
	parse(fs, args)
	role := "member"
	if *admin {
		role = "admin"
	}
	var inv api.Invite
	check(e.client().Do(e.ctx, "POST", "/v1/invites", map[string]any{"role": role, "uses": *uses, "ttl_hours": *ttl}, &inv))
	if e.output == "json" {
		e.json(inv)
		return
	}
	fmt.Fprintf(e.out, "invite code: %s (%d uses, %s, expires %s)\n", inv.Code, inv.UsesLeft, inv.Role, inv.Expires.Local().Format("Jan 2 15:04"))
	fmt.Fprintf(e.out, "share this with your friends:\n  %s\n", inv.JoinCmd)
}

func cmdUsers(e *env) {
	var l api.List[api.User]
	check(e.client().Do(e.ctx, "GET", "/v1/users", nil, &l))
	if e.output == "json" {
		e.json(l.Items)
		return
	}
	var rows [][]string
	for i, u := range l.Items {
		rows = append(rows, []string{strconv.Itoa(i + 1), u.Name, u.Role, strconv.Itoa(u.Nodes), fmt.Sprintf("%.2f", u.Contributed), fmt.Sprintf("%.2f", u.Consumed), fmt.Sprintf("%.2f", u.Credits)})
	}
	e.table("#\tUSER\tROLE\tNODES\tCONTRIBUTED\tCONSUMED\tBALANCE", rows)
}

func cmdCredits(e *env) {
	c := e.client()
	me, err := c.Me(e.ctx)
	check(err)
	var l api.List[api.LedgerEntry]
	check(c.Do(e.ctx, "GET", "/v1/me/ledger?limit=30", nil, &l))
	if e.output == "json" {
		e.json(map[string]any{"me": me, "ledger": l.Items})
		return
	}
	fmt.Fprintf(e.out, "balance: %.4f credits  (contributed %.4f, consumed %.4f)\n\n", me.Credits, me.Contributed, me.Consumed)
	var rows [][]string
	for _, x := range l.Items {
		rows = append(rows, []string{x.Time.Local().Format("Jan 02 15:04"), fmt.Sprintf("%+.4f", x.Delta), x.Reason, orDash(x.Ref)})
	}
	e.table("TIME\tDELTA\tREASON\tREF", rows)
}

func cmdNodes(e *env) {
	ns, err := e.client().Nodes(e.ctx)
	check(err)
	if e.output == "json" {
		e.json(ns)
		return
	}
	var rows [][]string
	for _, n := range ns {
		var gpus []string
		for _, g := range n.GPUs {
			gpus = append(gpus, fmt.Sprintf("%s(%dG)", g.Name, g.VRAMMB/1024))
		}
		tier := "-"
		if n.Assessment != nil {
			tier = n.Assessment.Tier
		}
		status := n.Status
		if n.Paused {
			status += " (paused)"
		}
		rows = append(rows, []string{n.ID, n.Name, n.Owner, status, orDash(strings.Join(gpus, ",")), strconv.Itoa(n.CPU),
			strconv.Itoa(n.MemoryMB / 1024), orDash(n.Country), strconv.Itoa(n.Instances), fmt.Sprintf("%.0f%%", n.UptimeRatio*100), tier, ago(n.LastSeen)})
	}
	e.table("ID\tNAME\tOWNER\tSTATUS\tGPUS\tCPU\tRAM(G)\tCOUNTRY\tINST\tUPTIME\tTIER\tSEEN", rows)
}

func cmdNode(e *env, args []string) {
	need(args, 1, "node add|pause|resume|trust|untrust|rm|assessment ...")
	c := e.client()
	switch args[0] {
	case "add":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		t, err := c.CreateNode(e.ctx, name)
		check(err)
		if e.output == "json" {
			e.json(t)
			return
		}
		fmt.Fprintf(e.out, "node %s (%s) created. On the GPU machine, run ONE of:\n\n", t.Name, t.NodeID)
		fmt.Fprintf(e.out, "  # Linux with Docker + NVIDIA Container Toolkit (installs a systemd service):\n  %s\n\n", t.InstallCmd)
		fmt.Fprintf(e.out, "  # or run the agent directly:\n  ogm agent --server %s --token %s\n\n", c.BaseURL, t.Token)
		fmt.Fprintf(e.out, "  # or with Docker only:\n  docker run -d --restart=always --gpus all -v /var/run/docker.sock:/var/run/docker.sock \\\n    ghcr.io/old-stock/open-gpu-mesh agent --server %s --token %s\n", c.BaseURL, t.Token)
	case "pause", "resume":
		need(args, 2, "node "+args[0]+" NODE_ID")
		check(c.Do(e.ctx, "PATCH", "/v1/nodes/"+args[1], map[string]bool{"paused": args[0] == "pause"}, nil))
		fmt.Fprintf(e.out, "node %s %sd\n", args[1], args[0])
	case "trust", "untrust":
		need(args, 2, "node "+args[0]+" NODE_ID")
		check(c.Do(e.ctx, "PATCH", "/v1/nodes/"+args[1], map[string]bool{"trusted": args[0] == "trust"}, nil))
		if args[0] == "trust" {
			fmt.Fprintf(e.out, "node %s is trusted: groups that use secrets may run on it\n", args[1])
		} else {
			fmt.Fprintf(e.out, "node %s is no longer trusted\n", args[1])
		}
	case "assessment", "audit":
		need(args, 2, "node assessment NODE_ID")
		var a api.Assessment
		check(c.Do(e.ctx, "GET", "/v1/nodes/"+args[1]+"/assessment", nil, &a))
		if e.output == "json" {
			e.json(a)
			return
		}
		fmt.Fprintf(e.out, "Assessed %s (ogm %s, %.0fs)\n", a.Time.Local().Format("Jan 02 15:04"), a.Version, a.DurationSec)
		assess.Render(e.out, &a)
	case "rm", "delete":
		need(args, 2, "node rm NODE_ID")
		check(c.Do(e.ctx, "DELETE", "/v1/nodes/"+args[1], nil, nil))
		fmt.Fprintf(e.out, "node %s removed\n", args[1])
	default:
		fatalf("usage: ogm node add|pause|resume|rm|assessment")
	}
}

func cmdGPUs(e *env) {
	var l api.List[api.GPUClass]
	check(e.client().Do(e.ctx, "GET", "/v1/gpu-classes", nil, &l))
	if e.output == "json" {
		e.json(l.Items)
		return
	}
	var rows [][]string
	for _, g := range l.Items {
		rows = append(rows, []string{g.ID, g.Name, strconv.Itoa(g.VRAMMB / 1024), strconv.Itoa(g.Online), strconv.Itoa(g.Free), strconv.Itoa(g.InUse),
			fmt.Sprintf("%.2f", g.PricePriority["high"]), fmt.Sprintf("%.2f", g.PricePriority["medium"]), fmt.Sprintf("%.2f", g.PricePriority["low"]), fmt.Sprintf("%.2f", g.PricePriority["batch"])})
	}
	e.table("CLASS\tNAME\tVRAM(G)\tONLINE\tFREE\tIN USE\tHIGH/h\tMED/h\tLOW/h\tBATCH/h", rows)
}

func cmdFiles(e *env, args []string) {
	need(args, 1, "files ls|put|get|rm|sign ...")
	c := e.client()
	base := "/v1/projects/" + e.project + "/files"
	switch args[0] {
	case "ls", "list":
		prefix := ""
		if len(args) > 1 {
			prefix = args[1]
		}
		var l api.List[api.FileInfo]
		check(c.Do(e.ctx, "GET", base+"?prefix="+url.QueryEscape(prefix), nil, &l))
		if e.output == "json" {
			e.json(l.Items)
			return
		}
		var rows [][]string
		for _, f := range l.Items {
			rows = append(rows, []string{f.Path, strconv.FormatInt(f.Size, 10), f.ContentType, f.CreateTime.Local().Format("Jan 02 15:04")})
		}
		e.table("PATH\tSIZE\tTYPE\tCREATED", rows)
	case "put":
		need(args, 2, "files put LOCAL [REMOTE]")
		remote := path.Base(args[1])
		if len(args) > 2 {
			remote = args[2]
		}
		f, err := os.Open(args[1])
		check(err)
		defer f.Close()
		var fi api.FileInfo
		hc := *c.HTTP
		hc.Timeout = 0
		c.HTTP = &hc
		check(c.Do(e.ctx, "PUT", base+"/"+remote, f, &fi))
		fmt.Fprintf(e.out, "uploaded %s (%d bytes, sha256 %s)\n", fi.Path, fi.Size, fi.SHA256[:12])
	case "get":
		need(args, 2, "files get REMOTE [LOCAL]")
		local := path.Base(args[1])
		if len(args) > 2 {
			local = args[2]
		}
		var w io.Writer = os.Stdout
		if local != "-" {
			f, err := os.Create(local)
			check(err)
			defer f.Close()
			w = f
		}
		check(c.Do(e.ctx, "GET", base+"/"+args[1], nil, w))
	case "rm", "delete":
		need(args, 2, "files rm REMOTE")
		check(c.Do(e.ctx, "DELETE", base+"/"+args[1], nil, nil))
		fmt.Fprintf(e.out, "deleted %s\n", args[1])
	case "sign":
		fs := flag.NewFlagSet("files sign", flag.ExitOnError)
		put := fs.Bool("put", false, "sign an upload URL instead of a download URL")
		exp := fs.Int("expires", 3600, "seconds")
		pos := parse(fs, args[1:])
		need(pos, 1, "files sign REMOTE")
		method := "GET"
		if *put {
			method = "PUT"
		}
		var out struct {
			URL string `json:"url"`
		}
		check(c.Do(e.ctx, "POST", "/v1/projects/"+e.project+"/file-tokens/"+pos[0], map[string]any{"method": method, "expires_in_s": *exp}, &out))
		fmt.Fprintln(e.out, out.URL)
	default:
		fatalf("usage: ogm files ls|put|get|rm|sign")
	}
}
