package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func pp(s *Server, a args) string { return "/v1/projects/" + esc(s.project(a)) }

func groupPath(s *Server, a args) string { return pp(s, a) + "/containers/" + esc(a.str("group")) }

func queuePath(s *Server, a args) string { return pp(s, a) + "/queues/" + esc(a.str("queue")) }

var (
	argGroup = pStr("Container group name.")
	argQueue = pStr("Queue name.")
	argJob   = pStr("Job id (job_...).")
	argNode  = pStr("Node id or name.")
)

// tools is the curated tool catalog, in the order tools/list returns it.
var tools = []tool{
	// ---- overview ----
	{
		Name: "cluster_status", Title: "Cluster status", ReadOnly: true,
		Description: "Overview of the ogm cluster: nodes online, GPUs, running instances and capacity, plus the calling user (role, credit balance) and the default project. Call this first to orient yourself.",
		Run: func(ctx context.Context, s *Server, a args) result {
			cl, e := s.fetch(ctx, "/v1/cluster")
			if e != nil {
				return *e
			}
			me, e := s.fetch(ctx, "/v1/me")
			if e != nil {
				return *e
			}
			return jsonResult(map[string]any{"cluster": cl, "me": me, "default_project": s.Project})
		},
	},
	{
		Name: "whoami", Title: "Current user", ReadOnly: true,
		Description: "The authenticated user: name, role (admin/member), credit balance, credits contributed and consumed, number of nodes owned.",
		Run:         func(ctx context.Context, s *Server, a args) result { return s.call(ctx, "GET", "/v1/me", nil) },
	},
	{
		Name: "credits", Title: "Credits and ledger", ReadOnly: true,
		Description: "Credit balance and the most recent ledger entries (contributions earned by your nodes, usage charged for your workloads, grants).",
		Props:       props("limit", pInt("Ledger entries to return, newest first (default 50).")),
		Run: func(ctx context.Context, s *Server, a args) result {
			me, e := s.fetch(ctx, "/v1/me")
			if e != nil {
				return *e
			}
			led, e := s.fetch(ctx, fmt.Sprintf("/v1/me/ledger?limit=%d", a.num("limit", 50)))
			if e != nil {
				return *e
			}
			return jsonResult(map[string]any{"user": me, "ledger": led})
		},
	},
	{
		Name: "list_projects", Title: "List projects", ReadOnly: true,
		Description: "Projects you can access. Container groups, queues, secrets, volumes and files all live inside a project.",
		Run:         func(ctx context.Context, s *Server, a args) result { return s.call(ctx, "GET", "/v1/projects", nil) },
	},

	// ---- container groups ----
	{
		Name: "list_groups", Title: "List container groups", Scoped: true, ReadOnly: true,
		Description: "List container groups (deployments) in a project with their image, replicas, status, instance counts and gateway URL.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/containers", nil)
		},
	},
	{
		Name: "get_group", Title: "Get container group", Scoped: true, ReadOnly: true,
		Description: "Full details of one container group: spec, version, current state, instance status counts, gateway URL and share token.",
		Props:       props("group", argGroup), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result { return s.call(ctx, "GET", groupPath(s, a), nil) },
	},
	{
		Name: "deploy_group", Title: "Deploy container group", Scoped: true, Idempotent: true,
		Description: "Create or replace a container group from a full spec (declarative PUT upsert by spec.name). Changing image, command, env, resources or probes triggers a rolling update; replicas/priority/networking options apply live. " +
			"Minimal CPU example: {\"name\":\"web\",\"replicas\":1,\"container\":{\"image\":\"nginx:alpine\",\"resources\":{\"cpu\":1,\"memory_mb\":512}},\"networking\":{\"port\":80,\"auth\":true}}. " +
			"GPU: set container.resources.gpu_count, gpu_classes (e.g. [\"rtx-4090\"], see list_gpu_classes) or min_vram_mb. Env values may use {{secret:NAME}}. Read resource ogm://docs/spec-reference for every field.",
		Props: props("spec", map[string]any{
			"type":                 "object",
			"description":          "Container group spec (same JSON as `ogm deploy -f`). Required: name, container.image.",
			"additionalProperties": true,
			"required":             []string{"name", "container"},
			"properties": map[string]any{
				"name":     pStr("DNS label: lowercase letters, digits, '-'; max 63."),
				"replicas": pInt("Desired instances, 0-1000 (default 1)."),
				"priority": pEnum("Scheduling priority / price multiplier.", "high", "medium", "low", "batch"),
				"container": map[string]any{
					"type": "object", "additionalProperties": true, "required": []string{"image"},
					"properties": map[string]any{
						"image":   pStr("Image reference, e.g. nginx:alpine or registry.ogm/{project}/{name}:{tag}."),
						"command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces ENTRYPOINT+CMD."},
						"env":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Environment; values may reference {{secret:NAME}} and {{link:GROUP:PORT}}."},
						"resources": map[string]any{
							"type": "object", "additionalProperties": true,
							"properties": map[string]any{
								"cpu":         pInt("vCPUs (default 1)."),
								"memory_mb":   pInt("Memory limit in MB (default 1024)."),
								"gpu_count":   pInt("GPUs (0 = CPU-only)."),
								"gpu_classes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Any-of GPU class slugs."},
								"min_vram_mb": pInt("Minimum free VRAM per GPU."),
								"storage_gb":  pInt("Free disk needed (default 10)."),
							},
						},
					},
				},
				"networking":       pObj("Gateway: {port, auth, load_balancer, max_concurrency, ...}. Setting port exposes the group via the gateway."),
				"readiness_probe":  pObj("{http:{path,port}} | {tcp:{port}} | {exec:{command}} plus period/threshold fields."),
				"queue_connection": pObj("Pull jobs from a queue: {queue_name, port, path, concurrency}."),
			},
		}), Required: []string{"spec"},
		Run: func(ctx context.Context, s *Server, a args) result {
			spec, ok := a.object("spec")
			if !ok {
				return errorf("spec must be a JSON object (a container group spec)")
			}
			name, _ := spec["name"].(string)
			if name == "" {
				return errorf("spec.name is required")
			}
			if sp, _ := spec["project"].(string); sp != "" && a.str("project") == "" {
				a["project"] = sp
			}
			delete(spec, "project")
			return s.call(ctx, "PUT", pp(s, a)+"/containers/"+esc(name), spec)
		},
	},
	{
		Name: "scale_group", Title: "Scale container group", Scoped: true, Idempotent: true,
		Description: "Set the desired replica count of a container group (JSON merge patch {\"replicas\": N}). Ignored while a queue autoscaler controls the group.",
		Props:       props("group", argGroup, "replicas", pInt("Desired replicas, 0-1000.")), Required: []string{"group", "replicas"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "PATCH", groupPath(s, a), map[string]any{"replicas": a.num("replicas", 0)})
		},
	},
	{
		Name: "start_group", Title: "Start container group", Scoped: true, Idempotent: true,
		Description: "Start a stopped container group (instances are scheduled again).",
		Props:       props("group", argGroup), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "POST", groupPath(s, a)+"/start", nil)
		},
	},
	{
		Name: "stop_group", Title: "Stop container group", Scoped: true, Idempotent: true,
		Description: "Stop a container group: all instances are terminated but the group and its spec are kept (start_group resumes it). Stops credit usage.",
		Props:       props("group", argGroup), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "POST", groupPath(s, a)+"/stop", nil)
		},
	},
	{
		Name: "delete_group", Title: "Delete container group", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Permanently delete a container group and terminate its instances. Persistent volumes are kept until delete_volume.",
		Props:       props("group", argGroup), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", groupPath(s, a), nil)
		},
	},
	{
		Name: "list_instances", Title: "List instances", Scoped: true, ReadOnly: true,
		Description: "Instances (replicas) of a container group: state, readiness, node, GPU, restarts and timestamps.",
		Props:       props("group", argGroup, "include_terminated", pBool("Also include terminated instances.")), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			p := groupPath(s, a) + "/instances"
			if a.boolean("include_terminated") {
				p += "?all=1"
			}
			return s.call(ctx, "GET", p, nil)
		},
	},
	{
		Name: "group_logs", Title: "Container logs", Scoped: true, ReadOnly: true,
		Description: "Recent stdout/stderr lines from a container group's instances, oldest first. Filter by time window, substring and instance.",
		Props: props(
			"group", argGroup,
			"since", pStr("Duration like 10m or 2h, or an RFC 3339 time (default: no limit)."),
			"query", pStr("Only lines containing this substring."),
			"instance", pStr("Only this instance id."),
			"limit", pInt("Max lines, newest kept (default 200, max 10000)."),
		), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			q := url.Values{}
			q.Set("limit", fmt.Sprint(a.num("limit", 200)))
			if v := a.str("since"); v != "" {
				q.Set("since", v)
			}
			if v := a.str("query"); v != "" {
				q.Set("q", v)
			}
			if v := a.str("instance"); v != "" {
				q.Set("instance", v)
			}
			return s.call(ctx, "GET", groupPath(s, a)+"/logs?"+q.Encode(), nil)
		},
	},
	{
		Name: "group_events", Title: "Container group events", Scoped: true, ReadOnly: true,
		Description: "Lifecycle events for a container group, newest first: scheduling decisions, image pulls, probe failures, restarts, preemptions, scaling.",
		Props:       props("group", argGroup, "limit", pInt("Max events (default 50).")), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", fmt.Sprintf("%s/events?limit=%d", groupPath(s, a), a.num("limit", 50)), nil)
		},
	},
	{
		Name: "restart_instance", Title: "Restart instance", Scoped: true,
		Description: "Act on one instance: restart (restart the container in place), recreate (fresh container on the same node) or reallocate (move to a different node).",
		Props: props("group", argGroup, "instance", pStr("Instance id."),
			"action", pEnum("What to do (default restart).", "restart", "recreate", "reallocate")),
		Required: []string{"group", "instance"},
		Run: func(ctx context.Context, s *Server, a args) result {
			act := a.str("action")
			if act == "" {
				act = "restart"
			}
			if act != "restart" && act != "recreate" && act != "reallocate" {
				return errorf("action must be restart, recreate or reallocate")
			}
			return s.call(ctx, "POST", groupPath(s, a)+"/instances/"+esc(a.str("instance"))+"/"+act, nil)
		},
	},

	// ---- queues & jobs ----
	{
		Name: "list_queues", Title: "List queues", Scoped: true, ReadOnly: true,
		Description: "Job queues in a project with their settings and pending/running/finished job counts.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/queues", nil)
		},
	},
	{
		Name: "create_queue", Title: "Create queue", Scoped: true,
		Description: "Create a job queue. Container groups consume it via spec.queue_connection; submit work with submit_job.",
		Props: props("name", pStr("Queue name (DNS label)."), "description", pStr("Optional description."),
			"settings", map[string]any{"type": "object", "additionalProperties": false, "description": "Optional settings.",
				"properties": map[string]any{
					"max_attempts":         pInt("Attempts per job before it fails."),
					"job_timeout_s":        pInt("Per-attempt timeout in seconds."),
					"retry_backoff_base_s": pInt("Initial retry backoff."),
					"retry_backoff_max_s":  pInt("Max retry backoff."),
					"retry_on_4xx":         pBool("Retry jobs your app answers with 4xx."),
					"max_output_bytes":     pInt("Max stored output size."),
					"retention_hours":      pInt("How long finished jobs are kept."),
				}}),
		Required: []string{"name"},
		Run: func(ctx context.Context, s *Server, a args) result {
			body := map[string]any{"name": a.str("name")}
			if d := a.str("description"); d != "" {
				body["description"] = d
			}
			if st, ok := a.object("settings"); ok {
				body["settings"] = st
			}
			return s.call(ctx, "POST", pp(s, a)+"/queues", body)
		},
	},
	{
		Name: "submit_job", Title: "Submit job", Scoped: true,
		Description: "Submit a job to a queue. input is any JSON value; it is POSTed to the consuming container. Set wait (seconds, max 60) to block until the job finishes and return its output.",
		Props: props("queue", argQueue,
			"input", map[string]any{"description": "Job input (any JSON value)."},
			"metadata", pObj("Optional metadata stored with the job."),
			"webhook", pStr("Optional URL to POST the result to when the job finishes."),
			"max_attempts", pInt("Override the queue's max attempts."),
			"timeout_s", pInt("Override the per-attempt timeout."),
			"wait", pInt("Seconds to wait for completion (0-60, default 0 = return immediately)."),
		), Required: []string{"queue", "input"},
		Run: func(ctx context.Context, s *Server, a args) result {
			in, _ := a.raw("input")
			body := map[string]any{"input": in}
			if m, ok := a.object("metadata"); ok {
				body["metadata"] = m
			}
			if w := a.str("webhook"); w != "" {
				body["webhook"] = w
			}
			if n := a.num("max_attempts", 0); n > 0 {
				body["max_attempts"] = n
			}
			if n := a.num("timeout_s", 0); n > 0 {
				body["timeout_s"] = n
			}
			status, b, err := s.Caller.Call(ctx, "POST", queuePath(s, a)+"/jobs", body)
			wait := clampWait(a.num("wait", 0))
			if err != nil || status >= 400 || wait == 0 {
				return render(status, b, err)
			}
			var j struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(b, &j) != nil || j.ID == "" {
				return render(status, b, err)
			}
			return s.call(ctx, "GET", fmt.Sprintf("%s/jobs/%s?wait=%d", queuePath(s, a), esc(j.ID), wait), nil)
		},
	},
	{
		Name: "get_job", Title: "Get job", Scoped: true, ReadOnly: true,
		Description: "A job's status (pending/running/succeeded/failed/cancelled), input, output, attempts and events. Set wait (seconds, max 60) to long-poll until it finishes.",
		Props:       props("queue", argQueue, "job", argJob, "wait", pInt("Seconds to wait for a terminal state (0-60).")), Required: []string{"queue", "job"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", fmt.Sprintf("%s/jobs/%s?wait=%d", queuePath(s, a), esc(a.str("job")), clampWait(a.num("wait", 0))), nil)
		},
	},
	{
		Name: "list_jobs", Title: "List jobs", Scoped: true, ReadOnly: true,
		Description: "Jobs in a queue, newest first, optionally filtered by status.",
		Props: props("queue", argQueue,
			"status", pEnum("Only jobs in this status.", "pending", "running", "succeeded", "failed", "cancelled"),
			"limit", pInt("Max jobs (default 50, max 1000).")), Required: []string{"queue"},
		Run: func(ctx context.Context, s *Server, a args) result {
			q := url.Values{}
			q.Set("limit", fmt.Sprint(a.num("limit", 50)))
			if st := a.str("status"); st != "" {
				q.Set("status", st)
			}
			return s.call(ctx, "GET", queuePath(s, a)+"/jobs?"+q.Encode(), nil)
		},
	},
	{
		Name: "cancel_job", Title: "Cancel job", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Cancel a pending or running job (a running attempt sees its request cancelled).",
		Props:       props("queue", argQueue, "job", argJob), Required: []string{"queue", "job"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", queuePath(s, a)+"/jobs/"+esc(a.str("job")), nil)
		},
	},

	// ---- nodes & GPUs ----
	{
		Name: "list_nodes", Title: "List nodes", ReadOnly: true,
		Description: "Contributor machines: status (online/offline), owner, GPUs with VRAM, CPU/memory/disk, labels, paused state, uptime ratio and running instances.",
		Run:         func(ctx context.Context, s *Server, a args) result { return s.call(ctx, "GET", "/v1/nodes", nil) },
	},
	{
		Name: "node_assessment", Title: "Node assessment", ReadOnly: true,
		Description: "The latest onboarding audit of a node (`ogm assess`): hardware inventory, CPU/disk benchmarks, hub and internet speed, NAT type, tier (anchor/compute/burst) and recommended limits.",
		Props:       props("node", argNode), Required: []string{"node"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", "/v1/nodes/"+esc(a.str("node"))+"/assessment", nil)
		},
	},
	{
		Name: "list_gpu_classes", Title: "GPU classes and availability", ReadOnly: true,
		Description: "GPU classes (slugs usable in container.resources.gpu_classes) with credit prices per hour, plus current availability (free vs total GPUs per class).",
		Run: func(ctx context.Context, s *Server, a args) result {
			gc, e := s.fetch(ctx, "/v1/gpu-classes")
			if e != nil {
				return *e
			}
			av, e := s.fetch(ctx, "/v1/availability")
			if e != nil {
				return *e
			}
			return jsonResult(map[string]any{"gpu_classes": gc, "availability": av})
		},
	},

	// ---- secrets ----
	{
		Name: "list_secrets", Title: "List secrets", Scoped: true, ReadOnly: true,
		Description: "Secret names and timestamps in a project. Values are write-only and never returned. Reference a secret in container env as {{secret:NAME}}.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/secrets", nil)
		},
	},
	{
		Name: "set_secret", Title: "Set secret", Scoped: true, Idempotent: true,
		Description: "Create or overwrite a project secret (stored encrypted, max 64 KiB). The value is never echoed back. Use it in env as {{secret:NAME}}.",
		Props:       props("name", pStr("Secret name: letters, digits, _ . - (max 128)."), "value", pStr("Secret value.")),
		Required:    []string{"name", "value"},
		Run: func(ctx context.Context, s *Server, a args) result {
			v, _ := a["value"].(string)
			r := s.call(ctx, "PUT", pp(s, a)+"/secrets/"+esc(a.str("name")), map[string]string{"value": v})
			if v != "" && strings.Contains(r.text, v) {
				r.text = strings.ReplaceAll(r.text, v, "[redacted]")
			}
			return r
		},
	},
	{
		Name: "delete_secret", Title: "Delete secret", Scoped: true, Destructive: true, Idempotent: true,
		Description: "Delete a project secret. Groups referencing it can no longer start new instances.",
		Props:       props("name", pStr("Secret name.")), Required: []string{"name"},
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "DELETE", pp(s, a)+"/secrets/"+esc(a.str("name")), nil)
		},
	},

	// ---- volumes & files ----
	{
		Name: "list_volumes", Title: "List volumes", Scoped: true, ReadOnly: true,
		Description: "Persistent volumes in a project: group, replica, the node each is pinned to, volume names, and whether in use.",
		Run: func(ctx context.Context, s *Server, a args) result {
			return s.call(ctx, "GET", pp(s, a)+"/volumes", nil)
		},
	},
	{
		Name: "delete_volume", Title: "Delete volume", Scoped: true, Destructive: true,
		Description: "Delete a group's persistent volume data on its node (all replicas, or one). Refused (409) while in use or while the node is offline. Data is unrecoverable.",
		Props:       props("group", argGroup, "replica", pInt("Only this replica index.")), Required: []string{"group"},
		Run: func(ctx context.Context, s *Server, a args) result {
			p := pp(s, a) + "/volumes/" + esc(a.str("group"))
			if a.has("replica") {
				p += fmt.Sprintf("?replica=%d", a.num("replica", 0))
			}
			return s.call(ctx, "DELETE", p, nil)
		},
	},
	{
		Name: "list_files", Title: "List files", Scoped: true, ReadOnly: true,
		Description: "Files in the project's built-in object store (model weights, outputs): path, size, sha256, content type.",
		Props:       props("prefix", pStr("Only paths starting with this prefix."), "limit", pInt("Max entries (default 1000).")),
		Run: func(ctx context.Context, s *Server, a args) result {
			q := url.Values{}
			if v := a.str("prefix"); v != "" {
				q.Set("prefix", v)
			}
			if a.has("limit") {
				q.Set("limit", fmt.Sprint(a.num("limit", 1000)))
			}
			p := pp(s, a) + "/files"
			if len(q) > 0 {
				p += "?" + q.Encode()
			}
			return s.call(ctx, "GET", p, nil)
		},
	},

	// ---- discovery ----
	{
		Name: "api_operations", Title: "List API operations", ReadOnly: true,
		Description: "List the ogm REST API operations (method, path, summary). Uses the hub's /openapi.json when available, otherwise a built-in table. Pair with api_get to read endpoints that have no dedicated tool.",
		Props:       props("filter", pStr("Only operations whose method, path or summary contains this text (case-insensitive).")),
		Run:         runAPIOperations,
	},
	{
		Name: "api_get", Title: "GET any API path", ReadOnly: true,
		Description: "Read-only escape hatch: perform a GET on any ogm API path under /v1/ (query string allowed), e.g. /v1/projects/default/events or /v1/nodes/NODE. Use api_operations to discover paths. Streaming (follow=1) is not supported.",
		Props:       props("path", pStr("Path starting with /v1/, e.g. /v1/availability.")), Required: []string{"path"},
		Run: func(ctx context.Context, s *Server, a args) result {
			p, err := safeGetPath(a.str("path"))
			if err != nil {
				return errorf("%v", err)
			}
			return s.call(ctx, "GET", p, nil)
		},
	},
}

func clampWait(w int) int {
	if w < 0 {
		return 0
	}
	if w > 60 {
		return 60
	}
	return w
}

// safeGetPath validates an api_get path: under /v1/, no traversal, no
// streaming, and not the MCP endpoint itself or node-only routes.
func safeGetPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/v1/") {
		return "", fmt.Errorf("path must start with /v1/")
	}
	u, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("invalid path: %v", err)
	}
	if strings.Contains(u.Path, "..") || u.Host != "" || u.Scheme != "" {
		return "", fmt.Errorf("invalid path")
	}
	for _, bad := range []string{"/v1/mcp", "/v1/agent/", "/v1/speedtest/", "/v1/signed/"} {
		if u.Path == bad || strings.HasPrefix(u.Path, bad) {
			return "", fmt.Errorf("%s is not available through api_get", bad)
		}
	}
	q := u.Query()
	q.Del("follow")
	u.RawQuery = q.Encode()
	return u.RequestURI(), nil
}
