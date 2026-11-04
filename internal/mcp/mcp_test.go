package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeCall struct {
	Method, Path string
	Body         any
}

// fakeCaller records calls and answers from a route table.
type fakeCaller struct {
	mu     sync.Mutex
	calls  []fakeCall
	routes map[string]struct {
		status int
		body   string
	}
}

func (f *fakeCaller) Call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{method, path, body})
	if r, ok := f.routes[method+" "+path]; ok {
		return r.status, []byte(r.body), nil
	}
	return 404, []byte(`{"title":"not found","status":404,"detail":"no route ` + path + `"}`), nil
}

func (f *fakeCaller) route(key string, status int, body string) {
	if f.routes == nil {
		f.routes = map[string]struct {
			status int
			body   string
		}{}
	}
	f.routes[key] = struct {
		status int
		body   string
	}{status, body}
}

func rpc(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	out := s.Handle(context.Background(), []byte(msg))
	if out == nil {
		t.Fatalf("no response to %s", msg)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad response %s: %v", out, err)
	}
	return m
}

func resultOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	return r
}

func toolText(t *testing.T, r map[string]any) string {
	t.Helper()
	c := r["content"].([]any)
	return c[0].(map[string]any)["text"].(string)
}

func TestInitialize(t *testing.T) {
	s := New(&fakeCaller{}, "")
	r := resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	if r["protocolVersion"] != "2025-03-26" {
		t.Fatalf("version not echoed: %v", r["protocolVersion"])
	}
	if r["serverInfo"].(map[string]any)["name"] != "ogm" {
		t.Fatalf("serverInfo = %v", r["serverInfo"])
	}
	caps := r["capabilities"].(map[string]any)
	for _, k := range []string{"tools", "resources", "prompts"} {
		if caps[k] == nil {
			t.Fatalf("missing capability %s", k)
		}
	}
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`))
	if r["protocolVersion"] != ProtocolVersion {
		t.Fatalf("unsupported version should get latest, got %v", r["protocolVersion"])
	}
	if out := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); out != nil {
		t.Fatalf("notification got a response: %s", out)
	}
	if r := resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":"p","method":"ping"}`)); len(r) != 0 {
		t.Fatalf("ping = %v", r)
	}
}

func TestToolsListSchemas(t *testing.T) {
	s := New(&fakeCaller{}, "")
	r := resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	list := r["tools"].([]any)
	if len(list) < 30 {
		t.Fatalf("only %d tools", len(list))
	}
	seen := map[string]bool{}
	for _, x := range list {
		tl := x.(map[string]any)
		name := tl["name"].(string)
		seen[name] = true
		if tl["description"] == "" {
			t.Errorf("%s: no description", name)
		}
		sc, ok := tl["inputSchema"].(map[string]any)
		if !ok || sc["type"] != "object" || sc["properties"] == nil {
			t.Errorf("%s: bad inputSchema %v", name, tl["inputSchema"])
		}
		ann := tl["annotations"].(map[string]any)
		if _, ok := ann["readOnlyHint"].(bool); !ok {
			t.Errorf("%s: no readOnlyHint", name)
		}
	}
	for _, want := range []string{"cluster_status", "deploy_group", "delete_group", "group_logs", "submit_job", "get_job", "set_secret", "api_operations", "api_get", "list_files", "node_assessment"} {
		if !seen[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	dg := findTool("delete_group")
	if !dg.Destructive || dg.ReadOnly {
		t.Fatal("delete_group must be destructive")
	}
}

func TestErrors(t *testing.T) {
	s := New(&fakeCaller{}, "")
	m := rpc(t, s, `{"jsonrpc":"2.0","id":7,"method":"bogus/method"}`)
	if e := m["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Fatalf("error = %v", e)
	}
	if m["id"].(float64) != 7 {
		t.Fatalf("id = %v", m["id"])
	}
	m = rpc(t, s, `{not json`)
	if e := m["error"].(map[string]any); e["code"].(float64) != -32700 {
		t.Fatalf("parse error = %v", e)
	}
	m = rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`)
	if e := m["error"].(map[string]any); e["code"].(float64) != -32602 {
		t.Fatalf("unknown tool = %v", e)
	}
	// Batch: two requests and a notification -> two responses.
	out := s.Handle(context.Background(), []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"nope"}]`))
	var batch []map[string]any
	if err := json.Unmarshal(out, &batch); err != nil || len(batch) != 2 {
		t.Fatalf("batch = %s", out)
	}
}

func TestToolsCall(t *testing.T) {
	f := &fakeCaller{}
	f.route("GET /v1/projects/default/containers", 200, `{"items":[{"name":"web"}]}`)
	f.route("PUT /v1/projects/team/containers/web", 200, `{"name":"web","replicas":2}`)
	f.route("PUT /v1/projects/default/secrets/TOKEN", 200, `{"name":"TOKEN"}`)
	f.route("POST /v1/projects/default/queues/q/jobs", 202, `{"id":"job_1","status":"pending"}`)
	f.route("GET /v1/projects/default/queues/q/jobs/job_1?wait=5", 200, `{"id":"job_1","status":"succeeded"}`)
	s := New(f, "default")

	r := resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_groups","arguments":{}}}`))
	if r["isError"] == true || !strings.Contains(toolText(t, r), `"name": "web"`) {
		t.Fatalf("list_groups = %v", r)
	}

	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"deploy_group","arguments":{"project":"team","spec":{"name":"web","replicas":2,"container":{"image":"nginx"}}}}}`))
	if r["isError"] == true {
		t.Fatalf("deploy = %v", toolText(t, r))
	}
	last := f.calls[len(f.calls)-1]
	if last.Method != "PUT" || last.Path != "/v1/projects/team/containers/web" || last.Body.(map[string]any)["replicas"].(float64) != 2 {
		t.Fatalf("deploy call = %+v", last)
	}

	// API errors become isError results with the problem details.
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_group","arguments":{"group":"missing"}}}`))
	if r["isError"] != true || !strings.Contains(toolText(t, r), "404") {
		t.Fatalf("get_group missing = %v", r)
	}
	// Missing required args.
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_group","arguments":{}}}`))
	if r["isError"] != true || !strings.Contains(toolText(t, r), "group") {
		t.Fatalf("missing arg = %v", r)
	}
	// Secrets never echo the value.
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"set_secret","arguments":{"name":"TOKEN","value":"hunter2"}}}`))
	if strings.Contains(toolText(t, r), "hunter2") {
		t.Fatal("secret echoed")
	}
	// submit_job with wait follows up with a long-poll.
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"submit_job","arguments":{"queue":"q","input":{"x":1},"wait":5}}}`))
	if !strings.Contains(toolText(t, r), "succeeded") {
		t.Fatalf("submit_job = %v", toolText(t, r))
	}
	// api_get refuses paths outside /v1 and strips follow.
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"api_get","arguments":{"path":"/etc/passwd"}}}`))
	if r["isError"] != true {
		t.Fatal("api_get allowed a non-/v1 path")
	}
	_ = rpc(t, s, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"api_get","arguments":{"path":"/v1/projects/default/containers/x/logs?follow=1&limit=5"}}}`)
	if p := f.calls[len(f.calls)-1].Path; strings.Contains(p, "follow") {
		t.Fatalf("follow not stripped: %s", p)
	}
	// api_operations falls back to the built-in table (no /openapi.json).
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"api_operations","arguments":{"filter":"secrets"}}}`))
	if txt := toolText(t, r); !strings.Contains(txt, `"source": "builtin"`) || !strings.Contains(txt, "/v1/projects/{project}/secrets") {
		t.Fatalf("api_operations = %s", txt)
	}
	f.route("GET /openapi.json", 200, `{"openapi":"3.1.0","paths":{"/v1/thing":{"get":{"summary":"Get the thing"}}}}`)
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"api_operations","arguments":{}}}`))
	if txt := toolText(t, r); !strings.Contains(txt, `"source": "openapi"`) || !strings.Contains(txt, "Get the thing") {
		t.Fatalf("api_operations openapi = %s", txt)
	}
}

func TestTruncate(t *testing.T) {
	big := strings.Repeat("é", maxResultBytes)
	out := truncate(big)
	if len(out) > maxResultBytes+300 || !strings.Contains(out, "output truncated") {
		t.Fatalf("truncate len %d", len(out))
	}
}

func TestResources(t *testing.T) {
	s := New(&fakeCaller{}, "")
	r := resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`))
	if len(r["resources"].([]any)) < 2 {
		t.Fatalf("resources = %v", r)
	}
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"ogm://docs/spec-reference"}}`))
	c := r["contents"].([]any)[0].(map[string]any)
	if c["mimeType"] != "text/markdown" || !strings.Contains(c["text"].(string), "container.image") {
		t.Fatalf("read = %v", c["mimeType"])
	}
	m := rpc(t, s, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ogm://nope"}}`)
	if m["error"] == nil {
		t.Fatal("expected error for unknown resource")
	}
	r = resultOf(t, rpc(t, s, `{"jsonrpc":"2.0","id":4,"method":"prompts/list"}`))
	if _, ok := r["prompts"].([]any); !ok {
		t.Fatalf("prompts = %v", r)
	}
}

func TestStdio(t *testing.T) {
	f := &fakeCaller{}
	f.route("GET /v1/nodes", 200, `{"items":[]}`)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}

{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_nodes","arguments":{}}}
`)
	var out strings.Builder
	if err := ServeStdio(context.Background(), in, &out, f, ""); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses, got %q", out.String())
	}
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["result"] == nil {
			t.Fatalf("bad line %q", l)
		}
	}
}

func TestHTTPSessions(t *testing.T) {
	sessions := NewSessions()
	f := &fakeCaller{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		New(f, "").HandleHTTP(w, r, sessions, r.Header.Get("X-User"))
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	post := func(user, sid, body string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(body))
		req.Header.Set("X-User", user)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("alice", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sid == "" {
		t.Fatal("no session id")
	}
	resp = post("alice", sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("notification status %d", resp.StatusCode)
	}
	resp = post("bob", sid, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("other user's session: %d", resp.StatusCode)
	}
	resp = post("alice", sid, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"result"`) {
		t.Fatalf("ping %d %s", resp.StatusCode, b)
	}
	req, _ := http.NewRequest("DELETE", ts.URL, nil)
	req.Header.Set("X-User", "alice")
	req.Header.Set("Mcp-Session-Id", sid)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Fatalf("delete %d", resp.StatusCode)
	}
	resp = post("alice", sid, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("after delete %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL)
	if resp.StatusCode != 405 {
		t.Fatalf("GET %d", resp.StatusCode)
	}
}
