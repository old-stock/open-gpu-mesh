package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// cmdSecret manages project secrets, referenced from env as {{secret:NAME}}.
func cmdSecret(e *env, args []string) {
	need(args, 1, "secret set|ls|rm ...")
	c := e.client()
	base := "/v1/projects/" + url.PathEscape(e.project) + "/secrets"
	switch args[0] {
	case "set", "put":
		fs := flag.NewFlagSet("secret set", flag.ExitOnError)
		file := fs.String("from-file", "", "read the value from a file (- = stdin)")
		pos := parse(fs, args[1:])
		need(pos, 1, "secret set NAME [VALUE | --from-file F]   (no VALUE: read one line from stdin)")
		var val string
		switch {
		case *file == "-":
			b, err := io.ReadAll(os.Stdin)
			check(err)
			val = string(b)
		case *file != "":
			b, err := os.ReadFile(*file)
			check(err)
			val = string(b)
		case len(pos) > 1:
			val = pos[1]
		default:
			if isTerminal(os.Stdin) {
				fmt.Fprintf(os.Stderr, "value for %s: ", pos[0])
			}
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			val = strings.TrimRight(line, "\r\n")
		}
		check(c.Do(e.ctx, "PUT", base+"/"+url.PathEscape(pos[0]), map[string]string{"value": val}, nil))
		fmt.Fprintf(e.out, "secret %s set (use it in env as {{secret:%s}})\n", pos[0], pos[0])
	case "ls", "list":
		var out api.List[api.Secret]
		check(c.Do(e.ctx, "GET", base, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, s := range out.Items {
			rows = append(rows, []string{s.Name, s.UpdateTime.Local().Format(time.DateTime)})
		}
		e.table("SECRET\tUPDATED", rows)
	case "rm", "delete":
		need(args, 2, "secret rm NAME")
		check(c.Do(e.ctx, "DELETE", base+"/"+url.PathEscape(args[1]), nil, nil))
		fmt.Fprintf(e.out, "secret %s deleted\n", args[1])
	default:
		fatalf("usage: ogm secret set|ls|rm")
	}
}

type volumeInfo struct {
	Group    string   `json:"group"`
	Replica  int      `json:"replica"`
	NodeID   string   `json:"node_id"`
	NodeName string   `json:"node_name"`
	Volumes  []string `json:"volumes"`
	InUse    bool     `json:"in_use"`
}

// cmdVolumes lists and deletes persistent volumes (one set per group replica, pinned to a node).
func cmdVolumes(e *env, args []string) {
	c := e.client()
	base := "/v1/projects/" + url.PathEscape(e.project) + "/volumes"
	if len(args) == 0 || args[0] == "ls" || args[0] == "list" {
		var out api.List[volumeInfo]
		check(c.Do(e.ctx, "GET", base, nil, &out))
		if e.output == "json" {
			e.json(out.Items)
			return
		}
		var rows [][]string
		for _, v := range out.Items {
			use := "no"
			if v.InUse {
				use = "yes"
			}
			rows = append(rows, []string{v.Group, strconv.Itoa(v.Replica), orDash(v.NodeName), use, strings.Join(v.Volumes, ",")})
		}
		e.table("GROUP\tREPLICA\tNODE\tIN USE\tVOLUMES", rows)
		return
	}
	switch args[0] {
	case "rm", "delete":
		fs := flag.NewFlagSet("volumes rm", flag.ExitOnError)
		replica := fs.Int("replica", -1, "only this replica's volumes (default: all replicas)")
		pos := parse(fs, args[1:])
		need(pos, 1, "volumes rm GROUP [--replica N]")
		path := base + "/" + url.PathEscape(pos[0])
		if *replica >= 0 {
			path += "?replica=" + strconv.Itoa(*replica)
		}
		check(c.Do(e.ctx, "DELETE", path, nil, nil))
		fmt.Fprintf(e.out, "volumes of %s deleted\n", pos[0])
	default:
		fatalf("usage: ogm volumes [ls] | rm GROUP [--replica N]")
	}
}
