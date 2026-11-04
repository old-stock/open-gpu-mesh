package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/agent"
	"github.com/old-stock/open-gpu-mesh/internal/assess"
)

// cmdAssess audits this machine: hardware, benchmarks, network speed and NAT type, and
// recommends agent limits for the owner's usage profile.
func cmdAssess(e *env, args []string) {
	fs := flag.NewFlagSet("assess", flag.ExitOnError)
	server := fs.String("server", os.Getenv("OGM_SERVER"), "hub URL for the latency/speed test (default: your login)")
	token := fs.String("token", os.Getenv("OGM_NODE_TOKEN"), "node token: also stores the result on the hub")
	profile := fs.String("profile", "", "your usage, comma-separated: "+strings.Join(assess.Profiles, ", ")+" (default: detect, or ask)")
	internet := fs.Bool("internet", true, "also measure internet speed (speed.cloudflare.com) and NAT type (public STUN servers)")
	disk := fs.Bool("disk", true, "run the disk write benchmark")
	diskPath := fs.String("disk-path", "", "directory for the disk benchmark (default: system temp dir)")
	quick := fs.Bool("quick", false, "shorter tests")
	submit := fs.Bool("submit", true, "store the result on the hub (needs --token)")
	fake := fs.String("fake-gpus", "", "simulate GPUs (testing)")
	flagsOnly := fs.Bool("flags-only", false, "print only the suggested agent flags on stdout (report goes to stderr); used by the installer")
	parse(fs, args)

	// Hub credentials: a node token, else the CLI login's API key (speed test only).
	srv, cred := *server, *token
	if cred == "" && e.cfg.APIKey != "" && (srv == "" || srv == e.cfg.Server) {
		srv, cred = e.cfg.Server, e.cfg.APIKey
	}
	var profiles []string
	if *profile != "" {
		for _, p := range strings.Split(*profile, ",") {
			if !assess.ValidProfile(p) {
				fatalf("unknown profile %q (choose from %s)", p, strings.Join(assess.Profiles, ", "))
			}
			profiles = append(profiles, p)
		}
	} else if e.output != "json" && isTerminal(os.Stdin) {
		profiles = askProfiles()
	}

	opts := assess.Options{Server: srv, Token: cred, Profiles: profiles, Internet: *internet, Disk: *disk,
		DiskPath: *diskPath, Quick: *quick}
	if *fake != "" {
		opts.GPUs = agent.ParseFakeGPUs(*fake)
	}
	if e.output != "json" {
		opts.Log = os.Stderr
	}
	a := assess.Run(e.ctx, opts)

	switch {
	case *flagsOnly:
		assess.Render(os.Stderr, a)
		fmt.Fprintln(e.out, a.Suggested.AgentFlags)
	case e.output == "json":
		e.json(a)
	default:
		assess.Render(e.out, a)
	}
	if *submit && strings.HasPrefix(*token, "ogmn_") && srv != "" {
		body, _ := json.Marshal(a)
		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(srv, "/")+"/v1/agent/assessment", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+*token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "ogm: could not store the assessment on the hub: %v\n", err)
		case resp.StatusCode >= 300:
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "ogm: could not store the assessment on the hub: HTTP %d\n", resp.StatusCode)
		default:
			resp.Body.Close()
			if e.output != "json" {
				fmt.Fprintln(os.Stderr, "Stored on the hub; see it with `ogm node assessment <node>` or in the dashboard.")
			}
		}
	}
}

func askProfiles() []string {
	fmt.Fprintln(os.Stderr, "How do you use this machine? (numbers, comma-separated; Enter = detect automatically)")
	for i, p := range assess.Profiles {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, p)
	}
	fmt.Fprint(os.Stderr, "> ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	var out []string
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' }) {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(assess.Profiles) {
			out = append(out, assess.Profiles[n-1])
		} else if assess.ValidProfile(f) {
			out = append(out, f)
		}
	}
	return out
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
