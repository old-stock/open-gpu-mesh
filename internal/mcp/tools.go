package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxResultBytes caps tool output text (~100 KB) so a big list doesn't flood the model's context.
const maxResultBytes = 100_000

// tool is one curated MCP tool.
type tool struct {
	Name        string
	Title       string
	Description string
	Props       map[string]any // JSON Schema properties
	Required    []string
	Scoped      bool // adds the optional "project" argument
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Run         func(ctx context.Context, s *Server, a args) result
}

// result is what a tool produces before it is rendered as MCP content.
type result struct {
	text    string
	isError bool
}

// args wraps tools/call arguments with typed accessors.
type args map[string]any

func (a args) str(k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}

func (a args) num(k string, def int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func (a args) has(k string) bool { _, ok := a[k]; return ok && a[k] != nil }

func (a args) boolean(k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(v)
		return b
	}
	return false
}

// object returns an object argument, also accepting a JSON-encoded string
// (some clients stringify nested objects).
func (a args) object(k string) (map[string]any, bool) {
	switch v := a[k].(type) {
	case map[string]any:
		return v, true
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return m, true
		}
	}
	return nil, false
}

// raw returns any JSON value argument (object, array, string, number...).
func (a args) raw(k string) (any, bool) {
	v, ok := a[k]
	return v, ok && v != nil
}

func (s *Server) project(a args) string {
	if p := a.str("project"); p != "" {
		return p
	}
	return s.Project
}

func esc(s string) string { return url.PathEscape(s) }

func errorf(f string, x ...any) result { return result{text: fmt.Sprintf(f, x...), isError: true} }

func missing(names ...string) result {
	return errorf("missing required argument(s): %s", strings.Join(names, ", "))
}

// call performs one API request and renders the response as a tool result.
func (s *Server) call(ctx context.Context, method, path string, body any) result {
	status, b, err := s.Caller.Call(ctx, method, path, body)
	return render(status, b, err)
}

func render(status int, b []byte, err error) result {
	if err != nil {
		return errorf("request failed: %v", err)
	}
	text := pretty(b)
	if status >= 400 {
		if text == "" {
			text = http.StatusText(status)
		}
		return result{text: fmt.Sprintf("API error (HTTP %d):\n%s", status, text), isError: true}
	}
	if text == "" {
		text = fmt.Sprintf(`{"ok": true, "status": %d}`, status)
	}
	return result{text: text}
}

func pretty(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") == nil {
		return buf.String()
	}
	return string(b)
}

// fetch performs a GET and decodes the JSON body (for composite tools).
func (s *Server) fetch(ctx context.Context, path string) (any, *result) {
	status, b, err := s.Caller.Call(ctx, "GET", path, nil)
	if err != nil || status >= 400 {
		r := render(status, b, err)
		return nil, &r
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		v = string(b)
	}
	return v, nil
}

func jsonResult(v any) result {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorf("encode: %v", err)
	}
	return result{text: string(b)}
}

func truncate(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	cut := maxResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n[output truncated: showing %d of %d bytes. Narrow the request (limit, since, prefix, status, query) to see the rest.]", cut, len(s))
}

func (s *Server) callTool(ctx context.Context, t *tool, a args) map[string]any {
	for _, k := range t.Required {
		if !a.has(k) || a.str(k) == "" && !isComposite(a[k]) {
			r := missing(k)
			return toolResult(r)
		}
	}
	r := t.Run(ctx, s, a)
	s.log().Debug("tool call", "tool", t.Name, "error", r.isError)
	return toolResult(r)
}

func isComposite(v any) bool {
	switch v.(type) {
	case map[string]any, []any, bool, float64:
		return true
	}
	return false
}

func toolResult(r result) map[string]any {
	out := map[string]any{"content": []any{map[string]any{"type": "text", "text": truncate(r.text)}}}
	if r.isError {
		out["isError"] = true
	}
	return out
}

// ---- schema helpers ----

func pStr(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func pInt(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func pBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func pEnum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}
func pObj(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc, "additionalProperties": true}
}

func props(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func (t *tool) schema() map[string]any {
	p := map[string]any{}
	for k, v := range t.Props {
		p[k] = v
	}
	if t.Scoped {
		p["project"] = pStr(`Project name. Defaults to the server's current project (usually "default").`)
	}
	sc := map[string]any{"type": "object", "properties": p, "additionalProperties": false}
	if len(t.Required) > 0 {
		sc["required"] = t.Required
	}
	return sc
}

func toolDescriptors() []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": t.schema(),
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"idempotentHint":  t.Idempotent || t.ReadOnly,
				"openWorldHint":   false,
			},
		})
	}
	return out
}

func findTool(name string) *tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}
