package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/old-stock/open-gpu-mesh/internal/regproxy"
)

// cmdPush pushes a local Docker image into the cluster registry:
//
//	ogm push my-app:dev            -> registry.ogm/{project}/my-app:dev
//	ogm push my-app:dev web:v2     -> registry.ogm/{project}/web:v2
func cmdPush(e *env, args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	project := fs.String("project", e.project, "project (repository namespace)")
	multi := fs.Bool("multiarch", false, "merge into the tag's existing platforms (push the same tag from x86 and ARM machines)")
	platform := fs.String("platform", "", "push this platform of a multi-platform local image, e.g. linux/arm64 (Docker's containerd image store); implies --multiarch")
	pos := parse(fs, args)
	if len(pos) < 1 {
		fatalf("usage: ogm push [--multiarch] [--platform linux/arm64] LOCAL_IMAGE[:TAG] [NAME[:TAG]]")
	}
	e.client() // ensure logged in
	src := pos[0]
	name := src
	if len(pos) > 1 {
		name = pos[1]
	}
	name = name[strings.LastIndex(name, "/")+1:] // drop any registry/namespace
	if !strings.Contains(name, ":") {
		name += ":latest"
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fatalf("docker CLI not found; push needs a local Docker to read the image")
	}
	var extra []http.Header
	if *multi || *platform != "" {
		extra = append(extra, http.Header{"Ogm-Merge-Platforms": {"1"}})
	}
	p, err := regproxy.Start(e.cfg.Server, e.cfg.APIKey, 0, extra...)
	check(err)
	defer p.Close()
	dst := p.Addr + "/" + *project + "/" + name
	run := func(args ...string) {
		cmd := exec.CommandContext(e.ctx, "docker", args...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fatalf("docker %s: %v", strings.Join(args, " "), err)
		}
	}
	run("tag", src, dst)
	defer exec.Command("docker", "rmi", dst).Run() //nolint:errcheck // removes only the temporary tag
	if *platform != "" {
		run("push", "--platform", *platform, dst)
	} else {
		run("push", dst)
	}
	ref := regproxy.Prefix + *project + "/" + name
	fmt.Fprintf(e.out, "pushed %s\nuse it in a spec:  container: { image: %s }\n", ref, ref)
}

// cmdImages lists repositories and tags in the cluster registry.
func cmdImages(e *env, args []string) {
	e.client() // ensure logged in
	get := func(path string, v any) {
		req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet, strings.TrimRight(e.cfg.Server, "/")+path, nil)
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
		resp, err := http.DefaultClient.Do(req)
		check(err)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			fatalf("registry: %s", resp.Status)
		}
		check(json.NewDecoder(resp.Body).Decode(v))
	}
	var cat struct{ Repositories []string }
	get("/v2/_catalog", &cat)
	type row struct {
		Image string   `json:"image"`
		Tags  []string `json:"tags"`
	}
	var rows []row
	for _, r := range cat.Repositories {
		var t struct{ Tags []string }
		get("/v2/"+r+"/tags/list", &t)
		rows = append(rows, row{regproxy.Prefix + r, t.Tags})
	}
	if e.output == "json" {
		e.json(rows)
		return
	}
	tw := tabwriter.NewWriter(e.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "IMAGE\tTAGS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\n", r.Image, strings.Join(r.Tags, ", "))
	}
	tw.Flush()
}

// cmdRegistryProxy runs the localhost proxy in the foreground, for using plain `docker push/pull`.
func cmdRegistryProxy(e *env, args []string) {
	fs := flag.NewFlagSet("registry-proxy", flag.ExitOnError)
	port := fs.Int("port", 5078, "local port")
	parse(fs, args)
	e.client()
	p, err := regproxy.Start(e.cfg.Server, e.cfg.APIKey, *port)
	check(err)
	defer p.Close()
	fmt.Fprintf(e.out, "cluster registry available at %s\n  docker tag my-app %s/%s/my-app:dev && docker push %s/%s/my-app:dev\nCtrl-C to stop.\n",
		p.Addr, p.Addr, e.project, p.Addr, e.project)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	select {
	case <-ch:
	case <-e.ctx.Done():
	}
}
