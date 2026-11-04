package mcp

import (
	"context"
	"encoding/json"
	"io/fs"
	"sort"
	"strings"

	"github.com/old-stock/open-gpu-mesh/docs"
)

// operation is one REST API operation.
type operation struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// builtinOperations mirrors the routes registered in internal/server and
// documented in docs/api.md. It is used when the hub serves no /openapi.json.
var builtinOperations = []operation{
	{"GET", "/v1/me", "Current user (role, credits, contribution/consumption)"},
	{"POST", "/v1/me/rotate-key", "Rotate your API key"},
	{"GET", "/v1/me/ledger", "Credit ledger (?limit=)"},
	{"POST", "/v1/invites", "Create an invite {uses, role, ttl_hours} (admin)"},
	{"POST", "/v1/join", "Redeem an invite {code, name} -> user + api_key"},
	{"GET", "/v1/users", "List users (leaderboard)"},
	{"PATCH", "/v1/users/{name}", "Change role / grant credits {role, grant_credits} (admin)"},
	{"GET", "/v1/projects", "List projects"},
	{"POST", "/v1/projects", "Create a project {name}"},
	{"DELETE", "/v1/projects/{project}", "Delete a project"},
	{"POST", "/v1/projects/{project}/members", "Add a member {user}"},
	{"GET", "/v1/projects/{project}/events", "Project events"},
	{"GET", "/v1/projects/{project}/containers", "List container groups"},
	{"POST", "/v1/projects/{project}/containers", "Create a container group"},
	{"GET", "/v1/projects/{project}/containers/{group}", "Get a container group"},
	{"PUT", "/v1/projects/{project}/containers/{group}", "Create or replace a container group (declarative upsert)"},
	{"PATCH", "/v1/projects/{project}/containers/{group}", "Merge-patch a container group (RFC 7386)"},
	{"DELETE", "/v1/projects/{project}/containers/{group}", "Delete a container group"},
	{"POST", "/v1/projects/{project}/containers/{group}/start", "Start a group"},
	{"POST", "/v1/projects/{project}/containers/{group}/stop", "Stop a group"},
	{"POST", "/v1/projects/{project}/containers/{group}/rotate-gateway-token", "Rotate the gateway share token"},
	{"GET", "/v1/projects/{project}/containers/{group}/instances", "List instances (?all=1 includes terminated)"},
	{"GET", "/v1/projects/{project}/containers/{group}/instances/{instance}", "Get an instance"},
	{"PATCH", "/v1/projects/{project}/containers/{group}/instances/{instance}", "Set instance deletion_cost"},
	{"POST", "/v1/projects/{project}/containers/{group}/instances/{instance}/{action}", "reallocate | recreate | restart an instance"},
	{"GET", "/v1/projects/{project}/containers/{group}/events", "Group events (?limit=)"},
	{"GET", "/v1/projects/{project}/containers/{group}/logs", "Logs (?since=10m&instance=&q=&limit=&follow=1)"},
	{"GET", "/v1/projects/{project}/queues", "List queues"},
	{"POST", "/v1/projects/{project}/queues", "Create a queue {name, description, settings}"},
	{"GET", "/v1/projects/{project}/queues/{queue}", "Get a queue"},
	{"PATCH", "/v1/projects/{project}/queues/{queue}", "Update a queue"},
	{"DELETE", "/v1/projects/{project}/queues/{queue}", "Delete a queue (?force=1)"},
	{"GET", "/v1/projects/{project}/queues/{queue}/jobs", "List jobs (?status=&limit=)"},
	{"POST", "/v1/projects/{project}/queues/{queue}/jobs", `Submit a job, or {"jobs":[...]} for a batch`},
	{"GET", "/v1/projects/{project}/queues/{queue}/jobs/{job}", "Get a job (?wait=SECONDS long-polls, max 60)"},
	{"DELETE", "/v1/projects/{project}/queues/{queue}/jobs/{job}", "Cancel a job"},
	{"GET", "/v1/projects/{project}/secrets", "List secret names (values are write-only)"},
	{"PUT", "/v1/projects/{project}/secrets/{name}", "Set a secret {value}"},
	{"DELETE", "/v1/projects/{project}/secrets/{name}", "Delete a secret"},
	{"GET", "/v1/projects/{project}/volumes", "List persistent volumes"},
	{"DELETE", "/v1/projects/{project}/volumes/{group}", "Delete a group's volumes (?replica=N); 409 if in use"},
	{"GET", "/v1/projects/{project}/files", "List files (?prefix=&limit=)"},
	{"PUT", "/v1/projects/{project}/files/{path}", "Upload a file (raw body)"},
	{"GET", "/v1/projects/{project}/files/{path}", "Download a file"},
	{"DELETE", "/v1/projects/{project}/files/{path}", "Delete a file"},
	{"POST", "/v1/projects/{project}/file-tokens/{path}", "Presigned URL {method: GET|PUT, expires_in_s}"},
	{"GET", "/v1/nodes", "List nodes"},
	{"POST", "/v1/nodes", "Create a node join token {name} -> {token, install_command}"},
	{"GET", "/v1/nodes/{node}", "Get a node"},
	{"PATCH", "/v1/nodes/{node}", "Rename / pause a node {name, paused}"},
	{"DELETE", "/v1/nodes/{node}", "Remove a node"},
	{"GET", "/v1/nodes/{node}/assessment", "Latest onboarding audit of a node"},
	{"GET", "/v1/gpu-classes", "GPU classes and credit prices"},
	{"PATCH", "/v1/gpu-classes/{id}", "Set credits_per_hour (admin)"},
	{"GET", "/v1/availability", "Free vs total GPUs per class"},
	{"GET", "/v1/cluster", "Cluster summary"},
	{"GET", "/v1/events", "Cluster-wide events (admin)"},
	{"GET", "/v1/webhook-secret", "Webhook signing secret"},
	{"POST", "/v1/webhook-secret", "Rotate the webhook signing secret (admin)"},
	{"GET", "/v1/projects/{project}/containers/{group}/metrics", "Per-minute gateway requests, errors and latency (?minutes=60)"},
	{"POST", "/v1/keys", "Create a scoped API key {name, project, role: developer|viewer|admin, expires_in_hours}; secret returned once"},
	{"GET", "/v1/keys", "List your API keys (?all=1 for admins)"},
	{"DELETE", "/v1/keys/{id}", "Revoke an API key"},
	{"GET", "/v1/projects/{project}/kv", "List KV tables"},
	{"GET", "/v1/projects/{project}/kv/{table}", "List keys (?prefix=&limit=&after=&values=1)"},
	{"DELETE", "/v1/projects/{project}/kv/{table}", "Delete a KV table"},
	{"GET", "/v1/projects/{project}/kv/{table}/{key}", "Get a value (raw body; ETag / OGM-Version headers)"},
	{"PUT", "/v1/projects/{project}/kv/{table}/{key}", "Put a value (raw body; ?ttl=S, If-Match: version, If-None-Match: *) -> {key, version}"},
	{"DELETE", "/v1/projects/{project}/kv/{table}/{key}", "Delete a key (If-Match: version)"},
	{"POST", "/v1/projects/{project}/kv/{table}/{key...}", "Atomic counter increment (?by=N&ttl=S)"},
	{"GET", "/v1/projects/{project}/schedules", "List cron schedules"},
	{"POST", "/v1/projects/{project}/schedules", "Create a schedule {name, schedule, enabled, job|http|scale}"},
	{"GET", "/v1/projects/{project}/schedules/{name}", "Get a schedule"},
	{"PUT", "/v1/projects/{project}/schedules/{name}", "Create or replace a schedule"},
	{"DELETE", "/v1/projects/{project}/schedules/{name}", "Delete a schedule"},
	{"POST", "/v1/projects/{project}/schedules/{name}/run", "Run a schedule now"},
	{"POST", "/v1/projects/{project}/volumes/{group}/snapshots", "Snapshot a replica's volumes {replica, name} -> 202"},
	{"GET", "/v1/projects/{project}/snapshots", "List snapshots (?group=)"},
	{"GET", "/v1/projects/{project}/snapshots/{id}", "Get a snapshot"},
	{"DELETE", "/v1/projects/{project}/snapshots/{id}", "Delete a snapshot"},
	{"POST", "/v1/projects/{project}/snapshots/{id}/restore", "Restore a snapshot {group, replica, node} (overwrites volumes)"},
	{"GET", "/v1/projects/{project}/s3-credentials", "List S3 access keys"},
	{"POST", "/v1/projects/{project}/s3-credentials", "Create an S3 access key pair (secret returned once)"},
	{"DELETE", "/v1/projects/{project}/s3-credentials/{access_key}", "Delete an S3 access key"},
	{"GET", "/metrics", "Prometheus metrics (admin)"},
	{"POST", "/v1/mcp", "MCP Streamable HTTP endpoint (JSON-RPC)"},
	{"GET", "/.well-known/jwks.json", "JWKS for instance identity tokens"},
	{"GET", "/healthz", "Liveness check"},
}

func runAPIOperations(ctx context.Context, s *Server, a args) result {
	ops, source := s.operations(ctx)
	if f := strings.ToLower(a.str("filter")); f != "" {
		var keep []operation
		for _, o := range ops {
			if strings.Contains(strings.ToLower(o.Method+" "+o.Path+" "+o.Summary), f) {
				keep = append(keep, o)
			}
		}
		ops = keep
	}
	if ops == nil {
		ops = []operation{}
	}
	return jsonResult(map[string]any{"source": source, "count": len(ops), "operations": ops})
}

// operations reads GET /openapi.json from the hub, falling back to the built-in table.
func (s *Server) operations(ctx context.Context) ([]operation, string) {
	status, b, err := s.Caller.Call(ctx, "GET", "/openapi.json", nil)
	if err == nil && status == 200 {
		if ops := parseOpenAPI(b); len(ops) > 0 {
			return ops, "openapi"
		}
	}
	return builtinOperations, "builtin"
}

func parseOpenAPI(b []byte) []operation {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	var ops []operation
	for path, item := range doc.Paths {
		for method, raw := range item {
			m := strings.ToUpper(method)
			switch m {
			case "GET", "PUT", "POST", "PATCH", "DELETE", "HEAD", "OPTIONS":
			default:
				continue
			}
			var op struct {
				Summary     string `json:"summary"`
				Description string `json:"description"`
				OperationID string `json:"operationId"`
			}
			_ = json.Unmarshal(raw, &op)
			sum := op.Summary
			if sum == "" {
				sum = firstLine(op.Description)
			}
			if sum == "" {
				sum = op.OperationID
			}
			ops = append(ops, operation{Method: m, Path: path, Summary: sum})
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
	return ops
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- resources ----

type docResource struct {
	URI, File, Name, Title, Description string
}

var docResources = []docResource{
	{"ogm://docs/spec-reference", "spec-reference.md", "spec-reference", "Container group & queue spec reference", "Every field of a container group and queue spec (what deploy_group accepts), with defaults and examples."},
	{"ogm://docs/api", "api.md", "api", "REST API reference", "REST endpoints, auth, errors, gateway, queue job delivery, webhooks and IMDS."},
	{"ogm://docs/quickstart", "quickstart.md", "quickstart", "Quickstart", "From zero to a GPU endpoint, with troubleshooting."},
}

func resourceList() []any {
	var out []any
	for _, r := range docResources {
		if _, err := fs.Stat(docs.FS, r.File); err != nil {
			continue
		}
		out = append(out, map[string]any{"uri": r.URI, "name": r.Name, "title": r.Title, "description": r.Description, "mimeType": "text/markdown"})
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func readResource(uri string) (string, bool) {
	for _, r := range docResources {
		if r.URI == uri {
			b, err := docs.FS.ReadFile(r.File)
			if err != nil {
				return "", false
			}
			return string(b), true
		}
	}
	return "", false
}
