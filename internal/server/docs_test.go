package server

import (
	"context"
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"go.yaml.in/yaml/v3"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/apidocs"
)

// The API docs drift gate. Routes are extracted statically from this package's source: every
// mux.HandleFunc / mux.Handle call whose pattern is a string literal, a "+" concatenation, or
// a local variable/constant holding one (e.g. base := "/v1/projects/{project}/queues"). A
// pattern that can't be resolved fails the test rather than being skipped.

var update = flag.Bool("update", false, "rewrite docs/api-surface.md from internal/apidocs/openapi.yaml")

type route struct {
	Method, Path string // Path normalized: {name...} -> {name}
	Pos          string
}

func (r route) key() string { return r.Method + " " + r.Path }

var wildcardRest = regexp.MustCompile(`\{([A-Za-z0-9_]+)\.\.\.\}`)

// sourceRoutes parses the non-test .go files of this package and returns every registered route.
func sourceRoutes(t *testing.T) []route {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pkgConsts := map[string]string{}
	var parsed []*ast.File
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, af)
	}
	// Package-level string constants/variables.
	for _, af := range parsed {
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if v, ok := evalString(vs.Values[i], pkgConsts); ok {
							pkgConsts[n.Name] = v
						}
					}
				}
			}
		}
	}
	var out []route
	for _, af := range parsed {
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Function-local string variables (assigned once, as in `base := "..."`).
			env := map[string]string{}
			for k, v := range pkgConsts {
				env[k] = v
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						if v, ok := evalString(as.Rhs[i], env); ok {
							env[id.Name] = v
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mux" {
					return true
				}
				pos := fset.Position(call.Pos()).String()
				pat, ok := evalString(call.Args[0], env)
				if !ok {
					t.Errorf("%s: can't resolve the route pattern statically; use a string literal or a local base string", pos)
					return true
				}
				method, path, ok := strings.Cut(pat, " ")
				if !ok || method == "" || !strings.HasPrefix(path, "/") {
					t.Errorf("%s: route %q must be \"METHOD /path\"", pos, pat)
					return true
				}
				out = append(out, route{Method: method, Path: wildcardRest.ReplaceAllString(path, "{$1}"), Pos: pos})
				return true
			})
		}
	}
	return out
}

func evalString(e ast.Expr, env map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok1 := evalString(x.X, env)
		b, ok2 := evalString(x.Y, env)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return evalString(x.X, env)
	case *ast.Ident:
		v, ok := env[x.Name]
		return v, ok
	}
	return "", false
}

func loadSpec(t *testing.T) (*yaml.Node, []apidocs.Operation) {
	t.Helper()
	doc, err := apidocs.Parse("test", "http://hub.test")
	if err != nil {
		t.Fatal(err)
	}
	return doc, apidocs.Operations(doc)
}

// TestDocsCoverAllRoutes is the drift gate: routes and documented operations must match.
func TestDocsCoverAllRoutes(t *testing.T) {
	routes := sourceRoutes(t)
	if len(routes) < 100 {
		t.Fatalf("only %d routes found in the source; the extractor is probably broken", len(routes))
	}
	_, ops := loadSpec(t)
	documented := map[string]apidocs.Operation{}
	for _, op := range ops {
		documented[op.Method+" "+op.Path] = op
	}
	registered := map[string]route{}
	for _, r := range routes {
		if prev, dup := registered[r.key()]; dup {
			t.Errorf("route %s registered twice (%s and %s)", r.key(), prev.Pos, r.Pos)
		}
		registered[r.key()] = r
		op, ok := documented[r.key()]
		switch {
		case !ok:
			t.Errorf("undocumented route %s (%s): add it to internal/apidocs/openapi.yaml", r.key(), r.Pos)
		case op.Protocol:
			t.Errorf("%s is a mux route but marked x-ogm-protocol in openapi.yaml", r.key())
		}
	}
	for _, op := range ops {
		k := op.Method + " " + op.Path
		if _, ok := registered[k]; !ok && !op.Protocol {
			t.Errorf("openapi.yaml documents %s (%s) but no such route is registered (mark protocol endpoints x-ogm-protocol: true)", k, op.OperationID)
		}
	}
	t.Logf("%d routes registered, %d operations documented", len(routes), len(ops))
}

var camelCase = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// TestDocsSpecWellFormed lints the document: OpenAPI validity (kin-openapi, which also
// validates examples against their schemas and rejects unknown keys), resolvable $refs, unique
// camelCase operationIds, declared tags, and the house rules for every operation.
func TestDocsSpecWellFormed(t *testing.T) {
	doc, ops := loadSpec(t)

	loader := openapi3.NewLoader()
	kdoc, err := loader.LoadFromData(apidocs.YAML("test", "http://hub.test"))
	if err != nil {
		t.Fatalf("kin-openapi load: %v", err)
	}
	if err := kdoc.Validate(context.Background()); err != nil {
		t.Errorf("kin-openapi validate: %v", err)
	}

	// Every $ref resolves.
	var root any
	if err := doc.Decode(&root); err != nil {
		t.Fatal(err)
	}
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if _, err := resolveRef(root, ref); err != nil {
					t.Errorf("%s: $ref %s: %v", at, ref, err)
				}
			}
			for k, c := range x {
				walk(c, at+"/"+k)
			}
		case []any:
			for i, c := range x {
				walk(c, at+"/"+strconv.Itoa(i))
			}
		}
	}
	walk(root, "#")

	tags := map[string]bool{}
	for _, tg := range apidocs.Tags(doc) {
		if tg.Description == "" {
			t.Errorf("tag %q has no description", tg.Name)
		}
		tags[tg.Name] = true
	}
	// Scalar hides tags that are in no x-tagGroups group.
	grouped := map[string]bool{}
	top := root.(map[string]any)
	groups, _ := top["x-tagGroups"].([]any)
	for _, g := range groups {
		for _, tg := range g.(map[string]any)["tags"].([]any) {
			if !tags[tg.(string)] {
				t.Errorf("x-tagGroups names unknown tag %q", tg)
			}
			grouped[tg.(string)] = true
		}
	}
	for tg := range tags {
		if !grouped[tg] {
			t.Errorf("tag %q is in no x-tagGroups group (Scalar would hide it)", tg)
		}
	}

	paths := top["paths"].(map[string]any)
	ids := map[string]string{}
	usedTags := map[string]bool{}
	for _, op := range ops {
		k := op.Method + " " + op.Path
		if !camelCase.MatchString(op.OperationID) {
			t.Errorf("%s: operationId %q is not camelCase", k, op.OperationID)
		}
		if prev, dup := ids[op.OperationID]; dup {
			t.Errorf("operationId %q used by %s and %s", op.OperationID, prev, k)
		}
		ids[op.OperationID] = k
		if strings.TrimSpace(op.Summary) == "" || strings.TrimSpace(op.Description) == "" {
			t.Errorf("%s: summary and description are required", k)
		}
		if len(op.Tags) == 0 {
			t.Errorf("%s: no tags", k)
		}
		for _, tg := range op.Tags {
			if !tags[tg] {
				t.Errorf("%s: tag %q is not declared in the top-level tags", k, tg)
			}
			usedTags[tg] = true
		}
		if op.CLI != "" && !strings.HasPrefix(op.CLI, "ogm ") {
			t.Errorf("%s: x-ogm-cli %q must start with \"ogm \"", k, op.CLI)
		}
		item := paths[op.Path].(map[string]any)
		o := item[strings.ToLower(op.Method)].(map[string]any)
		if resp, _ := o["responses"].(map[string]any); len(resp) == 0 {
			t.Errorf("%s: no responses", k)
		}
		// Path parameters declared at path or operation level must match the template.
		declared := map[string]bool{}
		for _, ps := range [][]any{asSlice(item["parameters"]), asSlice(o["parameters"])} {
			for _, p := range ps {
				pm := p.(map[string]any)
				if ref, ok := pm["$ref"].(string); ok {
					r, err := resolveRef(root, ref)
					if err != nil {
						continue
					}
					pm = r.(map[string]any)
				}
				if pm["in"] == "path" {
					declared[pm["name"].(string)] = true
				}
			}
		}
		for _, m := range pathParam.FindAllStringSubmatch(op.Path, -1) {
			if !declared[m[1]] {
				t.Errorf("%s: path parameter {%s} is not declared", k, m[1])
			}
			delete(declared, m[1])
		}
		for p := range declared {
			t.Errorf("%s: declares path parameter %q that is not in the path", k, p)
		}
	}
	for tg := range tags {
		if !usedTags[tg] {
			t.Errorf("tag %q has no operations", tg)
		}
	}
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

func resolveRef(root any, ref string) (any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, errNotLocal
	}
	cur := root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, errUnresolved
		}
		if cur, ok = m[part]; !ok {
			return nil, errUnresolved
		}
	}
	return cur, nil
}

type docErr string

func (e docErr) Error() string { return string(e) }

const (
	errNotLocal   = docErr("only local refs (#/...) are allowed")
	errUnresolved = docErr("does not resolve")
)

// TestDocsAPISurfaceUpToDate fails when docs/api-surface.md is stale. Regenerate with
// `go generate ./internal/server` (or `go test ./internal/server -run TestDocsAPISurfaceUpToDate -update`).
func TestDocsAPISurfaceUpToDate(t *testing.T) {
	want, err := apidocs.Surface()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join("..", "..", "docs", "api-surface.md")
	if *update {
		if err := os.WriteFile(p, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != string(want) {
		t.Fatalf("docs/api-surface.md is stale; run `go generate ./internal/server`")
	}
}

// TestDocRoutesServe checks the doc routes on a real server handler (and that the dashboard
// catch-all "GET /" doesn't shadow them).
func TestDocRoutesServe(t *testing.T) {
	srv, err := New(Config{DataDir: t.TempDir(), PublicURL: "http://hub.test:7777"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/openapi.json")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("/openapi.json: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Servers []struct {
			Variables map[string]struct {
				Default string `json:"default"`
			} `json:"variables"`
		} `json:"servers"`
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("/openapi.json is not JSON: %v", err)
	}
	if doc.OpenAPI != "3.1.0" || doc.Info.Version != api.Version {
		t.Errorf("openapi %q version %q, want 3.1.0 / %q", doc.OpenAPI, doc.Info.Version, api.Version)
	}
	if len(doc.Servers) == 0 || doc.Servers[0].Variables["hub"].Default != "http://hub.test:7777" {
		t.Errorf("server URL not substituted: %+v", doc.Servers)
	}
	var me struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(doc.Paths["/v1/me"]["get"], &me)
	if !strings.Contains(me.Description, "**CLI:** `ogm status`") {
		t.Errorf("x-ogm-cli not appended to the description: %q", me.Description)
	}
	if strings.Contains(rec.Body.String(), apidocs.VersionPlaceholder) || strings.Contains(rec.Body.String(), apidocs.HubPlaceholder) {
		t.Error("placeholders left in /openapi.json")
	}

	if rec := get("/openapi.yaml"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "openapi: 3.1.0") {
		t.Errorf("/openapi.yaml: %d", rec.Code)
	}
	if rec := get("/docs"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="api-reference"`) ||
		!strings.Contains(rec.Body.String(), `data-url="/openapi.json"`) {
		t.Errorf("/docs: %d %.200s", rec.Code, rec.Body.String())
	}
	if rec := get("/reference"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs" {
		t.Errorf("/reference: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = get("/llms.txt")
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "# open-gpu-mesh API") ||
		!strings.Contains(rec.Body.String(), "- GET /v1/me — Get the current user") ||
		!strings.Contains(rec.Body.String(), "http://hub.test:7777/openapi.json") {
		t.Errorf("/llms.txt: %d %.300s", rec.Code, rec.Body.String())
	}

	// Every documented operation appears in llms.txt.
	_, ops := loadSpec(t)
	var missing []string
	for _, op := range ops {
		if !strings.Contains(rec.Body.String(), "- "+op.Method+" "+op.Path+" — ") {
			missing = append(missing, op.Method+" "+op.Path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("llms.txt misses %v", missing)
	}
}

// schemaTypes maps component schemas to the Go types they describe. TestDocsSchemasMatchTypes
// fails when a JSON field is added to (or removed from) one of these types without updating
// openapi.yaml.
var schemaTypes = map[string]any{
	"ContainerGroupSpec":  api.ContainerGroupSpec{},
	"ContainerSpec":       api.ContainerSpec{},
	"Volume":              api.Volume{},
	"RegistryAuth":        api.RegistryAuth{},
	"Resources":           api.Resources{},
	"Networking":          api.Networking{},
	"Probe":               api.Probe{},
	"HTTPProbe":           api.HTTPProbe{},
	"TCPProbe":            api.TCPProbe{},
	"ExecProbe":           api.ExecProbe{},
	"QueueConnection":     api.QueueConnection{},
	"QueueAutoscaler":     api.QueueAutoscaler{},
	"HTTPAutoscaler":      api.HTTPAutoscaler{},
	"ScalingAction":       api.ScalingAction{},
	"ContainerGroup":      api.ContainerGroup{},
	"GroupState":          api.GroupState{},
	"InstanceStatusCount": api.InstanceStatusCount{},
	"Instance":            api.Instance{},
	"InstanceMetrics":     api.InstanceMetrics{},
	"GPU":                 api.GPU{},
	"Node":                api.Node{},
	"NodeToken":           api.NodeToken{},
	"Assessment":          api.Assessment{},
	"GPUClass":            api.GPUClass{},
	"ClusterInfo":         api.ClusterInfo{},
	"QueueSettings":       api.QueueSettings{},
	"QueueStats":          api.QueueStats{},
	"Queue":               api.Queue{},
	"JobPrototype":        api.JobPrototype{},
	"JobEvent":            api.JobEvent{},
	"Job":                 api.Job{},
	"Event":               api.Event{},
	"LogEntry":            api.LogEntry{},
	"User":                api.User{},
	"LedgerEntry":         api.LedgerEntry{},
	"Invite":              api.Invite{},
	"Project":             api.Project{},
	"FileInfo":            api.FileInfo{},
	"Secret":              api.Secret{},
	"Problem":             api.Problem{},
	"APIKey":              APIKey{},
	"VolumeInfo":          VolumeInfo{},
	"Snapshot":            Snapshot{},
	"ScheduleSpec":        ScheduleSpec{},
	"Schedule":            Schedule{},
	"KVItem":              KVItem{},
	"KVTable":             KVTable{},
	"MetricPoint":         MetricPoint{},
	"S3Credential":        S3Credential{},
	"Quota":               Quota{},
	"QuotaUsage":          QuotaUsage{},
	"QuotaStatus":         QuotaStatus{},
}

// jsonFields returns the JSON field names of a struct type, flattening embedded structs.
func jsonFields(t reflect.Type, out map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			jsonFields(f.Type, out)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
}

// schemaProps returns a component schema's property names, flattening allOf.
func schemaProps(root any, s map[string]any, out map[string]bool) {
	if ref, ok := s["$ref"].(string); ok {
		if r, err := resolveRef(root, ref); err == nil {
			schemaProps(root, r.(map[string]any), out)
		}
	}
	for _, sub := range asSlice(s["allOf"]) {
		schemaProps(root, sub.(map[string]any), out)
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k := range props {
			out[k] = true
		}
	}
}

func TestDocsSchemasMatchTypes(t *testing.T) {
	doc, _ := loadSpec(t)
	var root any
	if err := doc.Decode(&root); err != nil {
		t.Fatal(err)
	}
	schemas := root.(map[string]any)["components"].(map[string]any)["schemas"].(map[string]any)
	names := make([]string, 0, len(schemaTypes))
	for n := range schemaTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s, ok := schemas[name].(map[string]any)
		if !ok {
			t.Errorf("components.schemas.%s is missing", name)
			continue
		}
		have := map[string]bool{}
		schemaProps(root, s, have)
		want := map[string]bool{}
		jsonFields(reflect.TypeOf(schemaTypes[name]), want)
		for f := range want {
			if !have[f] {
				t.Errorf("schema %s lacks field %q of %T", name, f, schemaTypes[name])
			}
		}
		for f := range have {
			if !want[f] {
				t.Errorf("schema %s documents field %q that %T doesn't have", name, f, schemaTypes[name])
			}
		}
	}
}
