package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/mcp"
)

// mcpHTTP is a tiny Streamable HTTP MCP client for tests.
type mcpHTTP struct {
	t    *testing.T
	url  string
	key  string
	sid  string
	next int
}

func (m *mcpHTTP) post(body string) (*http.Response, map[string]any) {
	m.t.Helper()
	req, _ := http.NewRequest("POST", m.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.key != "" {
		req.Header.Set("Authorization", "Bearer "+m.key)
	}
	if m.sid != "" {
		req.Header.Set("Mcp-Session-Id", m.sid)
		req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp, out
}

func (m *mcpHTTP) call(method string, params any) map[string]any {
	m.t.Helper()
	m.next++
	p, _ := json.Marshal(params)
	resp, out := m.post(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, m.next, method, p))
	if resp.StatusCode != 200 || out["result"] == nil {
		m.t.Fatalf("%s: status %d, %v", method, resp.StatusCode, out)
	}
	return out["result"].(map[string]any)
}

// tool calls an MCP tool and returns its text and isError flag.
func (m *mcpHTTP) tool(name string, args map[string]any) (string, bool) {
	m.t.Helper()
	r := m.call("tools/call", map[string]any{"name": name, "arguments": args})
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	return text, r["isError"] == true
}

func TestMCPHTTP(t *testing.T) {
	cl := newCluster(t)
	cl.addAgent("cpu-box", "")
	eventually(t, 10*time.Second, "node online", func() (bool, string) {
		ns, _ := cl.c.Nodes(cl.ctx)
		return len(ns) == 1 && ns[0].Status == "online", fmt.Sprint(ns)
	})

	// Unauthenticated -> 401.
	anon := &mcpHTTP{t: t, url: cl.url + "/v1/mcp"}
	if resp, _ := anon.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
	bad := &mcpHTTP{t: t, url: cl.url + "/v1/mcp", key: "ogm_wrong"}
	if resp, _ := bad.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 401 {
		t.Fatalf("bad key status = %d", resp.StatusCode)
	}
	if resp, err := http.Get(cl.url + "/v1/mcp"); err != nil || resp.StatusCode != 405 {
		t.Fatalf("GET /v1/mcp = %v %v", resp.StatusCode, err)
	}

	m := &mcpHTTP{t: t, url: cl.url + "/v1/mcp", key: cl.key}
	resp, out := m.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`)
	m.sid = resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || m.sid == "" {
		t.Fatalf("initialize: %d sid=%q %v", resp.StatusCode, m.sid, out)
	}
	if v := out["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", v)
	}
	if resp, _ := m.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != 202 {
		t.Fatalf("initialized notification = %d", resp.StatusCode)
	}

	tl := m.call("tools/list", map[string]any{})["tools"].([]any)
	if len(tl) < 30 {
		t.Fatalf("tools/list returned %d tools", len(tl))
	}

	// cluster_status sees the node and the caller.
	if txt, isErr := m.tool("cluster_status", nil); isErr || !strings.Contains(txt, `"admin"`) {
		t.Fatalf("cluster_status: %v %s", isErr, txt)
	}

	// deploy_group with a CPU-only demo spec, then list_groups shows it.
	spec, _ := json.Marshal(cpuSpec("mcp-web", 1))
	var specObj map[string]any
	_ = json.Unmarshal(spec, &specObj)
	if txt, isErr := m.tool("deploy_group", map[string]any{"spec": specObj}); isErr {
		t.Fatalf("deploy_group: %s", txt)
	}
	txt, isErr := m.tool("list_groups", map[string]any{})
	if isErr || !strings.Contains(txt, `"mcp-web"`) {
		t.Fatalf("list_groups: %s", txt)
	}
	cl.waitReady("mcp-web", 1)
	if txt, isErr := m.tool("list_instances", map[string]any{"group": "mcp-web"}); isErr || !strings.Contains(txt, "cpu-box") && !strings.Contains(txt, "node") {
		t.Fatalf("list_instances: %s", txt)
	}
	if txt, isErr := m.tool("scale_group", map[string]any{"group": "mcp-web", "replicas": 0}); isErr {
		t.Fatalf("scale_group: %s", txt)
	}
	if txt, _ := m.tool("get_group", map[string]any{"group": "mcp-web"}); !strings.Contains(txt, `"replicas": 0`) {
		t.Fatalf("get_group after scale: %s", txt)
	}
	// Validation errors come back as isError with problem details.
	if txt, isErr := m.tool("deploy_group", map[string]any{"spec": map[string]any{"name": "Bad_Name"}}); !isErr || !strings.Contains(txt, "400") {
		t.Fatalf("invalid deploy: %v %s", isErr, txt)
	}

	// Secrets: set + list never echo the value.
	const secret = "s3cr3t-value-xyz"
	if txt, isErr := m.tool("set_secret", map[string]any{"name": "HF_TOKEN", "value": secret}); isErr || strings.Contains(txt, secret) {
		t.Fatalf("set_secret: %v %s", isErr, txt)
	}
	txt, _ = m.tool("list_secrets", nil)
	if !strings.Contains(txt, "HF_TOKEN") || strings.Contains(txt, secret) {
		t.Fatalf("list_secrets: %s", txt)
	}

	// Queues/jobs round trip (no consumer: job stays pending).
	if txt, isErr := m.tool("create_queue", map[string]any{"name": "mq"}); isErr {
		t.Fatalf("create_queue: %s", txt)
	}
	txt, isErr = m.tool("submit_job", map[string]any{"queue": "mq", "input": map[string]any{"prompt": "hi"}})
	if isErr || !strings.Contains(txt, "pending") {
		t.Fatalf("submit_job: %s", txt)
	}

	// Discovery tools.
	if txt, isErr := m.tool("api_operations", map[string]any{"filter": "queues"}); isErr || !strings.Contains(txt, "/queues") {
		t.Fatalf("api_operations: %s", txt)
	}
	if txt, isErr := m.tool("api_get", map[string]any{"path": "/v1/availability"}); isErr {
		t.Fatalf("api_get: %s", txt)
	}
	if txt, isErr := m.tool("api_get", map[string]any{"path": "/v1/mcp"}); !isErr {
		t.Fatalf("api_get /v1/mcp should be refused: %s", txt)
	}

	// Resources.
	rr := m.call("resources/read", map[string]any{"uri": "ogm://docs/api"})
	if c := rr["contents"].([]any)[0].(map[string]any); !strings.Contains(c["text"].(string), "/v1/projects") {
		t.Fatal("ogm://docs/api content missing")
	}

	// KV: put, get (value + version), CAS conflict, incr.
	if txt, isErr := m.tool("kv_put", map[string]any{"table": "cfg", "key": "models/default", "value": `{"model":"llama"}`, "content_type": "application/json"}); isErr || !strings.Contains(txt, `"version": 1`) {
		t.Fatalf("kv_put: %s", txt)
	}
	txt, isErr = m.tool("kv_get", map[string]any{"table": "cfg", "key": "models/default"})
	if isErr || !strings.Contains(txt, `"version": "1"`) || !strings.Contains(txt, `llama`) {
		t.Fatalf("kv_get: %s", txt)
	}
	if txt, isErr := m.tool("kv_put", map[string]any{"table": "cfg", "key": "models/default", "value": "x", "if_version": 7}); !isErr || !strings.Contains(txt, "412") {
		t.Fatalf("kv_put CAS should fail: %v %s", isErr, txt)
	}
	if txt, isErr := m.tool("kv_put", map[string]any{"table": "cfg", "key": "models/default", "value": "x", "create_only": true}); !isErr {
		t.Fatalf("kv_put create_only should fail: %s", txt)
	}
	m.tool("kv_incr", map[string]any{"table": "counters", "key": "hits"})
	if txt, isErr := m.tool("kv_incr", map[string]any{"table": "counters", "key": "hits", "by": 5}); isErr || !strings.Contains(txt, `"value": 6`) {
		t.Fatalf("kv_incr: %s", txt)
	}
	if txt, _ := m.tool("kv_list_tables", nil); !strings.Contains(txt, "counters") || !strings.Contains(txt, "cfg") {
		t.Fatalf("kv_list_tables: %s", txt)
	}
	if txt, isErr := m.tool("kv_delete", map[string]any{"table": "cfg", "key": "models/default"}); isErr {
		t.Fatalf("kv_delete: %s", txt)
	}

	// Schedules: a scale action, fired manually.
	if txt, isErr := m.tool("put_schedule", map[string]any{"name": "nightly", "schedule": "0 3 * * *",
		"scale": map[string]any{"group": "mcp-web", "replicas": 2}}); isErr {
		t.Fatalf("put_schedule: %s", txt)
	}
	if txt, isErr := m.tool("run_schedule", map[string]any{"name": "nightly"}); isErr || !strings.Contains(txt, "ok") {
		t.Fatalf("run_schedule: %s", txt)
	}
	eventually(t, 5*time.Second, "schedule scaled group", func() (bool, string) {
		g, err := cl.c.Group(cl.ctx, "default", "mcp-web")
		if err != nil {
			return false, err.Error()
		}
		return g.DesiredReplicas == 2, fmt.Sprint(g.DesiredReplicas)
	})
	if txt, _ := m.tool("list_schedules", nil); !strings.Contains(txt, "nightly") {
		t.Fatalf("list_schedules: %s", txt)
	}
	if txt, isErr := m.tool("put_schedule", map[string]any{"name": "bad", "schedule": "* * * * *"}); !isErr {
		t.Fatalf("put_schedule without action should fail: %s", txt)
	}
	if txt, isErr := m.tool("delete_schedule", map[string]any{"name": "nightly"}); isErr {
		t.Fatalf("delete_schedule: %s", txt)
	}

	// Metrics and templates.
	if txt, isErr := m.tool("group_metrics", map[string]any{"group": "mcp-web", "minutes": 30}); isErr || !strings.Contains(txt, `"summary"`) {
		t.Fatalf("group_metrics: %s", txt)
	}
	if txt, isErr := m.tool("list_templates", nil); isErr || !strings.Contains(txt, `"hello"`) {
		t.Fatalf("list_templates: %s", txt)
	}
	if txt, isErr := m.tool("get_template", map[string]any{"name": "hello"}); isErr || !strings.Contains(txt, "image") {
		t.Fatalf("get_template: %s", txt)
	}
	if _, isErr := m.tool("get_template", map[string]any{"name": "../go.mod"}); !isErr {
		t.Fatal("get_template should refuse unknown names")
	}

	// API keys: create a viewer key, use it against the API, then revoke it.
	txt, isErr = m.tool("create_key", map[string]any{"name": "ci", "project": "default", "role": "viewer", "expires_in_hours": 1})
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if isErr || json.Unmarshal([]byte(txt), &created) != nil || created.Key == "" {
		t.Fatalf("create_key: %s", txt)
	}
	viewer := &mcpHTTP{t: t, url: cl.url + "/v1/mcp", key: created.Key}
	if txt, isErr := viewer.tool("list_groups", map[string]any{}); isErr || !strings.Contains(txt, "mcp-web") {
		t.Fatalf("viewer list_groups: %s", txt)
	}
	if txt, isErr := viewer.tool("stop_group", map[string]any{"group": "mcp-web"}); !isErr {
		t.Fatalf("viewer key should not stop a group: %s", txt)
	}
	if txt, _ := m.tool("list_keys", nil); !strings.Contains(txt, created.ID) || strings.Contains(txt, created.Key) {
		t.Fatalf("list_keys: %s", txt)
	}
	if txt, isErr := m.tool("revoke_key", map[string]any{"id": created.ID}); isErr {
		t.Fatalf("revoke_key: %s", txt)
	}
	if resp, _ := viewer.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 401 {
		t.Fatalf("revoked key status = %d", resp.StatusCode)
	}

	if txt, isErr := m.tool("delete_group", map[string]any{"group": "mcp-web"}); isErr {
		t.Fatalf("delete_group: %s", txt)
	}

	// Unknown session -> 404.
	m2 := &mcpHTTP{t: t, url: cl.url + "/v1/mcp", key: cl.key, sid: "mcp_bogus"}
	if resp, _ := m2.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != 404 {
		t.Fatalf("bogus session status = %d", resp.StatusCode)
	}
}

func TestMCPStdio(t *testing.T) {
	cl := newCluster(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(cl.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- mcp.ServeStdio(ctx, inR, outW, mcp.NewHTTPCaller(cl.url, cl.key), "default")
		outW.Close()
	}()
	rd := bufio.NewReader(outR)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() map[string]any {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("non-JSON on stdout: %q", line)
		}
		return m
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if v := recv()["result"].(map[string]any)["protocolVersion"]; v != "2024-11-05" {
		t.Fatalf("protocolVersion = %v", v)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	spec, _ := json.Marshal(cpuSpec("stdio-web", 0))
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"deploy_group","arguments":{"spec":` + string(spec) + `}}}`)
	if r := recv()["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("deploy_group: %v", r)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_groups","arguments":{}}}`)
	r := recv()["result"].(map[string]any)
	if txt := r["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(txt, "stdio-web") {
		t.Fatalf("list_groups: %s", txt)
	}
	send(`{"jsonrpc":"2.0","id":4,"method":"nope"}`)
	if e := recv()["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Fatalf("unknown method = %v", e)
	}
	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStdio did not exit on EOF")
	}
}
