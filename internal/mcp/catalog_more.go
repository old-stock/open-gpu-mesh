package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/old-stock/open-gpu-mesh/examples"
)

// Tools for API keys, KV, schedules, snapshots, metrics, S3 credentials and
// deploy templates. Appended to the catalog in init so tools/list keeps a
// stable order.
func init() { tools = append(tools, moreTools...) }

// kvKeyPath escapes each "/"-separated segment of a KV key (keys may contain "/").
func kvKeyPath(k string) string {
	parts := strings.Split(k, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func kvTablePath(s *Server, a args) string { return pp(s, a) + "/kv/" + esc(a.str("table")) }

var (
	argTable = pStr("KV table name (1-64 of A-Z a-z 0-9 _ . -).")
	argKey   = pStr("Key (1-1024 bytes; may contain '/').")
	argSched = pStr("Schedule name.")
	argSnap  = pStr("Snapshot id (snap_...).")
)

// templates lists the embedded example deploy files.
func templates() []map[string]string {
	ents, _ := fs.ReadDir(examples.FS, ".")
	var out []map[string]string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, _ := examples.FS.ReadFile(e.Name())
		desc := ""
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				desc = strings.TrimSpace(strings.TrimLeft(line, "#"))
				if desc != "" {
					break
				}
				continue
			}
			if line != "" {
				break
			}
		}
		out = append(out, map[string]string{"name": strings.TrimSuffix(e.Name(), ".yaml"), "description": desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

var moreTools = []tool{
	// ---- metrics ----
	{
		Name: "group_metrics", Title: "Container group metrics", Scoped: true, ReadOnly: true,
		Description: "Gateway traffic metrics for a container group: per-minute requests, 4xx/5xx errors and p50/p95/p99 latency, plus a summary over the window.",
		Props:       props("group", argGroup, "minutes", pInt("Window in minutes (default 60, max 1440).")), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", fmt.Sprintf("%s/metrics?minutes=%d", groupPath(s, a), a.num("minutes", 60)), nil)
		},
	},

	// ---- quotas ----
	{
		Name: "get_quota", Title: "Get project quota", Scoped: true, ReadOnly: true,
		Description: "The project's resource limits (cpu, memory_mb, gpus, instances, storage_mb; 0 = unlimited) and current usage. Compute usage counts every group at its peak replica count (autoscaler/schedule maximum); storage counts files, snapshots and S3 objects.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/quota", nil)
		},
	},
	{
		Name: "set_quota", Title: "Set project quota", Scoped: true, Idempotent: true,
		Description: "Set a project's resource limits (cluster admins only). Omitted or 0 fields are unlimited. Deploys and schedules that would exceed a limit are rejected with 403 quota exceeded; uploads past storage_mb fail.",
		Props: props("cpu", pInt("vCPUs."), "memory_mb", pInt("Memory in MB."), "gpus", pInt("GPUs."),
			"instances", pInt("Instances."), "storage_mb", pInt("Stored MB (files + snapshots + S3).")),
		Run: func(ctx context.Context, s *Server, a args) result {
			body := map[string]int{}
			for _, k := range []string{"cpu", "memory_mb", "gpus", "instances", "storage_mb"} {
				body[k] = a.num(k, 0)
			}
			return s.call(ctx, "PUT", pp(s, a)+"/quota", body)
		},
	},

	// ---- templates ----
	{
		Name: "list_templates", Title: "List deploy templates", ReadOnly: true,
		Description: "Built-in example deploy files (YAML) such as hello, llm-ollama, vllm-openai, comfyui, postgres, redis, sdxl-queue. Read one with get_template.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return jsonResult(map[string]any{"templates": templates()})
		},
	},
	{
		Name: "get_template", Title: "Get deploy template", ReadOnly: true,
		Description: "The YAML text of a built-in example deploy file. To deploy it, convert each YAML document to a JSON container group spec (adjust name/resources as needed) and call deploy_group; documents with `kind: queue` map to create_queue. ${ENV} placeholders must be filled in.",
		Props:       props("name", pStr("Template name from list_templates, e.g. hello.")), Required: []string{"name"},
		Run: func(ctx context.Context, s *Server, a args) result {
			n := path.Base(strings.TrimSuffix(a.str("name"), ".yaml"))
			b, err := examples.FS.ReadFile(n + ".yaml")
			if err != nil {
				return errorf("no template %q; call list_templates", n)
			}
			return result{text: string(b)}
		},
	},

	// ---- API keys ----
	{
		Name: "list_keys", Title: "List API keys", ReadOnly: true,
		Description: "Your scoped API keys (id, name, project, role, expiry, last used). Secrets are never listed. Admins can pass all=true to see every user's keys.",
		Props:       props("all", pBool("Admins only: include other users' keys.")),
		Run: func(ctx context.Context, s *Server, a args) result {
			p := "/v1/keys"
			if a.boolean("all") {
				p += "?all=1"
			}
			return s.call(ctx, "GET", p, nil)
		},
	},
	{
		Name: "create_key", Title: "Create API key",
		Description: "Create a scoped API key (for CI, an app or a teammate). The secret (`key`, ogmk_...) is returned ONLY in this result; give it to the user to store safely. Roles: developer (default; deploy/manage), viewer (read-only), admin (admins only). Optionally restrict to one project and set an expiry.",
		Props: props("name", pStr("Label for the key (max 100 chars)."),
			"project", pStr("Restrict the key to this project (default: unrestricted)."),
			"role", pEnum("Permission level (default developer).", "developer", "viewer", "admin"),
			"expires_in_hours", pInt("Expiry in hours (0 or omitted = never).")),
		Required: []string{"name"},
		Run: func(ctx context.Context, s *Server, a args) result {
			body := map[string]any{"name": a.str("name")}
			for _, k := range []string{"project", "role"} {
				if v := a.str(k); v != "" {
					body[k] = v
				}
			}
			if n := a.num("expires_in_hours", 0); n > 0 {
				body["expires_in_hours"] = n
			}
			return s.call(ctx, "POST", "/v1/keys", body)
		},
	},
	{
		Name: "revoke_key", Title: "Revoke API key", Destructive: true, Idempotent: true,
		Description: "Revoke (delete) a scoped API key by id. Anything using it stops working immediately.",
		Props:       props("id", pStr("Key id (key_...) from list_keys.")), Required: []string{"id"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", "/v1/keys/"+esc(a.str("id")), nil)
		},
	},

	// ---- KV ----
	{
		Name: "kv_list_tables", Title: "List KV tables", Scoped: true, ReadOnly: true,
		Description: "Tables in the project's key-value store with key counts and total bytes.",
		Run:         func(ctx context.Context, s *Server, a args) result { return s.call(ctx, "GET", pp(s, a)+"/kv", nil) },
	},
	{
		Name: "kv_list", Title: "List KV keys", Scoped: true, ReadOnly: true,
		Description: "Keys in a KV table, sorted, with version, size and expiry. Paginate with after=next_after. values=true includes the values.",
		Props: props("table", argTable, "prefix", pStr("Only keys starting with this prefix."),
			"limit", pInt("Max keys (default 100, max 1000)."), "after", pStr("Return keys after this one (pagination)."),
			"values", pBool("Include values.")),
		Required: []string{"table"},
		Run: func(ctx context.Context, s *Server, a args) result {
			q := url.Values{}
			q.Set("limit", fmt.Sprint(a.num("limit", 100)))
			if v := a.str("prefix"); v != "" {
				q.Set("prefix", v)
			}
			if v := a.str("after"); v != "" {
				q.Set("after", v)
			}
			if a.boolean("values") {
				q.Set("values", "1")
			}
			return s.call(ctx, "GET", kvTablePath(s, a)+"?"+q.Encode(), nil)
		},
	},
	{
		Name: "kv_get", Title: "Get KV value", Scoped: true, ReadOnly: true,
		Description: "Read one KV value. Returns {key, version, expires, content_type, value}; binary values are base64 (encoding: base64). Pass version to kv_put if_version for compare-and-set.",
		Props:       props("table", argTable, "key", argKey), Required: []string{"table", "key"},
		Run: func(ctx context.Context, s *Server, a args) result {
			r, err := s.do(ctx, Request{Method: "GET", Path: kvTablePath(s, a) + "/" + kvKeyPath(a.str("key"))})
			if err != nil || r.Status >= 400 {
				return render(r.Status, r.Body, err)
			}
			out := map[string]any{"key": a.str("key")}
			if v := r.Header.Get("OGM-Version"); v != "" {
				out["version"] = v
			} else if e := strings.Trim(r.Header.Get("ETag"), `"`); e != "" {
				out["version"] = e
			}
			if e := r.Header.Get("OGM-Expires"); e != "" {
				out["expires"] = e
			}
			if ct := r.Header.Get("Content-Type"); ct != "" {
				out["content_type"] = ct
			}
			if utf8.Valid(r.Body) {
				out["value"] = string(r.Body)
			} else {
				out["value"], out["encoding"] = base64.StdEncoding.EncodeToString(r.Body), "base64"
			}
			return jsonResult(out)
		},
	},
	{
		Name: "kv_put", Title: "Put KV value", Scoped: true, Idempotent: true,
		Description: "Write a KV value (string, max 1 MiB). Optional ttl_seconds; if_version for compare-and-set (412 if the stored version differs); create_only to fail if the key exists. Returns {key, version}.",
		Props: props("table", argTable, "key", argKey, "value", pStr("Value to store (text; JSON is fine)."),
			"content_type", pStr("Content-Type stored with the value (default text/plain)."),
			"ttl_seconds", pInt("Expire the key after this many seconds."),
			"if_version", pInt("Only write if the current version equals this."),
			"create_only", pBool("Only write if the key does not exist.")),
		Required: []string{"table", "key", "value"},
		Run: func(ctx context.Context, s *Server, a args) result {
			v, ok := a["value"].(string)
			if !ok {
				b, _ := json.Marshal(a["value"])
				v = string(b)
			}
			ct := a.str("content_type")
			if ct == "" {
				ct = "text/plain"
			}
			h := map[string]string{"Content-Type": ct}
			if a.has("if_version") {
				h["If-Match"] = fmt.Sprintf(`"%d"`, a.num("if_version", 0))
			}
			if a.boolean("create_only") {
				h["If-None-Match"] = "*"
			}
			p := kvTablePath(s, a) + "/" + kvKeyPath(a.str("key"))
			if t := a.num("ttl_seconds", 0); t > 0 {
				p += fmt.Sprintf("?ttl=%d", t)
			}
			r, err := s.do(ctx, Request{Method: "PUT", Path: p, Body: []byte(v), Headers: h})
			return render(r.Status, r.Body, err)
		},
	},
	{
		Name: "kv_delete", Title: "Delete KV key", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Delete one KV key (optionally only if its version matches), or a whole table when key is omitted and delete_table is true.",
		Props: props("table", argTable, "key", argKey, "if_version", pInt("Only delete if the current version equals this."),
			"delete_table", pBool("Delete every key in the table (requires key to be omitted).")),
		Required: []string{"table"},
		Run: func(ctx context.Context, s *Server, a args) result {
			if a.str("key") == "" {
				if !a.boolean("delete_table") {
					return errorf("give key, or delete_table=true to delete the whole table")
				}
				return s.call(ctx, "DELETE", kvTablePath(s, a), nil)
			}
			var h map[string]string
			if a.has("if_version") {
				h = map[string]string{"If-Match": fmt.Sprintf(`"%d"`, a.num("if_version", 0))}
			}
			r, err := s.do(ctx, Request{Method: "DELETE", Path: kvTablePath(s, a) + "/" + kvKeyPath(a.str("key")), Headers: h})
			return render(r.Status, r.Body, err)
		},
	},
	{
		Name: "kv_incr", Title: "Increment KV counter", Scoped: true,
		Description: "Atomically add to an integer counter (created at 0 if missing). Returns {key, value, version}. Keys containing '/' are not supported here.",
		Props:       props("table", argTable, "key", argKey, "by", pInt("Amount to add (default 1; may be negative)."), "ttl_seconds", pInt("Set an expiry.")),
		Required:    []string{"table", "key"},
		Run: func(ctx context.Context, s *Server, a args) result {
			q := url.Values{}
			q.Set("by", fmt.Sprint(a.num("by", 1)))
			if t := a.num("ttl_seconds", 0); t > 0 {
				q.Set("ttl", fmt.Sprint(t))
			}
			return s.call(ctx, "POST", kvTablePath(s, a)+"/"+kvKeyPath(a.str("key"))+"?"+q.Encode(), nil)
		},
	},

	// ---- schedules ----
	{
		Name: "list_schedules", Title: "List schedules", Scoped: true, ReadOnly: true,
		Description: "Cron schedules in a project with their action, enabled flag, last run and status.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/schedules", nil)
		},
	},
	{
		Name: "put_schedule", Title: "Create or update schedule", Scoped: true, Idempotent: true,
		Description: "Create or replace a cron schedule (5-field cron, server time zone) with exactly one action: job (submit {queue, input, metadata}), http (call {group, method, path, body, headers} through the gateway) or scale (set {group, replicas}). E.g. scale a group to 0 at night: schedule \"0 22 * * *\", scale {group:\"web\", replicas:0}.",
		Props: props("name", argSched, "schedule", pStr("5-field cron, e.g. \"*/15 * * * *\"."),
			"enabled", pBool("Default true."), "comment", pStr("Optional note."),
			"job", map[string]any{"type": "object", "description": "Submit a job.", "additionalProperties": false, "required": []string{"queue"},
				"properties": map[string]any{"queue": argQueue, "input": map[string]any{"description": "Job input (any JSON)."}, "metadata": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}}},
			"http", map[string]any{"type": "object", "description": "Call a group's HTTP endpoint.", "additionalProperties": false, "required": []string{"group"},
				"properties": map[string]any{"group": argGroup, "method": pStr("Default POST."), "path": pStr("Default /."), "body": pStr("Request body."), "headers": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}}},
			"scale", map[string]any{"type": "object", "description": "Set a group's replicas.", "additionalProperties": false, "required": []string{"group", "replicas"},
				"properties": map[string]any{"group": argGroup, "replicas": pInt("Replica count.")}},
		),
		Required: []string{"name", "schedule"},
		Run: func(ctx context.Context, s *Server, a args) result {
			body := map[string]any{"name": a.str("name"), "schedule": a.str("schedule")}
			if a.has("enabled") {
				body["enabled"] = a.boolean("enabled")
			}
			if c := a.str("comment"); c != "" {
				body["comment"] = c
			}
			n := 0
			for _, k := range []string{"job", "http", "scale"} {
				if o, ok := a.object(k); ok {
					body[k] = o
					n++
				}
			}
			if n != 1 {
				return errorf("give exactly one action: job, http or scale")
			}
			return s.call(ctx, "PUT", pp(s, a)+"/schedules/"+esc(a.str("name")), body)
		},
	},
	{
		Name: "delete_schedule", Title: "Delete schedule", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Delete a cron schedule.",
		Props:       props("name", argSched), Required: []string{"name"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", pp(s, a)+"/schedules/"+esc(a.str("name")), nil)
		},
	},
	{
		Name: "run_schedule", Title: "Run schedule now", Scoped: true,
		Description: "Fire a schedule's action immediately (for testing), regardless of its cron time. Returns the run status.",
		Props:       props("name", argSched), Required: []string{"name"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "POST", pp(s, a)+"/schedules/"+esc(a.str("name"))+"/run", nil)
		},
	},

	// ---- snapshots ----
	{
		Name: "create_snapshot", Title: "Snapshot a volume", Scoped: true,
		Description: "Start a snapshot of a group replica's persistent volumes (archived into the project file store). Returns 202 with a pending snapshot; poll get_snapshot until status is ready. The node holding the volume must be online.",
		Props:       props("group", argGroup, "replica", pInt("Replica index (default 0)."), "name", pStr("Optional label.")), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			body := map[string]any{"replica": a.num("replica", 0)}
			if n := a.str("name"); n != "" {
				body["name"] = n
			}
			return s.call(ctx, "POST", pp(s, a)+"/volumes/"+esc(a.str("group"))+"/snapshots", body)
		},
	},
	{
		Name: "list_snapshots", Title: "List snapshots", Scoped: true, ReadOnly: true,
		Description: "Volume snapshots in a project, newest first, with status, size and restore status.",
		Props:       props("group", pStr("Only snapshots of this group.")),
		Run: func(ctx context.Context, s *Server, a args) result {
			p := pp(s, a) + "/snapshots"
			if g := a.str("group"); g != "" {
				p += "?group=" + url.QueryEscape(g)
			}
			return s.call(ctx, "GET", p, nil)
		},
	},
	{
		Name: "get_snapshot", Title: "Get snapshot", Scoped: true, ReadOnly: true,
		Description: "One snapshot's status (pending/ready/failed), size, node and last restore status.",
		Props:       props("id", argSnap), Required: []string{"id"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/snapshots/"+esc(a.str("id")), nil)
		},
	},
	{
		Name: "restore_snapshot", Title: "Restore snapshot", Scoped: true, Destructive: true,
		Description: "Restore a ready snapshot into a group replica's volumes, OVERWRITING their current contents. Defaults to the snapshot's own group/replica and the replica's pinned node; can target another group, replica or node (id or name) to clone or migrate data.",
		Props: props("id", argSnap, "group", pStr("Target group (default: the snapshot's group)."),
			"replica", pInt("Target replica (default: the snapshot's replica)."), "node", pStr("Target node id or name.")),
		Required: []string{"id"},
		Run: func(ctx context.Context, s *Server, a args) result {
			body := map[string]any{}
			if g := a.str("group"); g != "" {
				body["group"] = g
			}
			if a.has("replica") {
				body["replica"] = a.num("replica", 0)
			}
			if n := a.str("node"); n != "" {
				body["node"] = n
			}
			return s.call(ctx, "POST", pp(s, a)+"/snapshots/"+esc(a.str("id"))+"/restore", body)
		},
	},
	{
		Name: "delete_snapshot", Title: "Delete snapshot", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Delete a snapshot and its archive.",
		Props:       props("id", argSnap), Required: []string{"id"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", pp(s, a)+"/snapshots/"+esc(a.str("id")), nil)
		},
	},

	// ---- S3 credentials ----
	{
		Name: "list_s3_credentials", Title: "List S3 credentials", Scoped: true, ReadOnly: true,
		Description: "S3 access keys for the project's S3-compatible endpoint (secrets are never listed).",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/s3-credentials", nil)
		},
	},
	{
		Name: "create_s3_credentials", Title: "Create S3 credentials", Scoped: true,
		Description: "Create an S3 access key pair for the project (endpoint, region, access_key_id, secret_access_key). The secret is returned ONLY in this result; give it to the user to store safely.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "POST", pp(s, a)+"/s3-credentials", map[string]any{})
		},
	},
	{
		Name: "delete_s3_credentials", Title: "Delete S3 credentials", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Delete an S3 access key; clients using it lose access immediately.",
		Props:       props("access_key", pStr("Access key id (OGMS3...).")), Required: []string{"access_key"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", pp(s, a)+"/s3-credentials/"+esc(a.str("access_key")), nil)
		},
	},
}
