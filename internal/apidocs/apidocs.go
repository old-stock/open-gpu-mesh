// Package apidocs embeds the hub's OpenAPI document (openapi.yaml) and renders it as JSON,
// as an llms.txt index and as the docs/api-surface.md route table.
//
// openapi.yaml is the single source of truth. internal/server/docs_test.go fails when a route
// registered on the hub is missing from it, so the served reference cannot drift.
package apidocs

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed openapi.yaml
var spec []byte

// Placeholders in openapi.yaml replaced at render time.
const (
	VersionPlaceholder = "__OGM_VERSION__"
	HubPlaceholder     = "__OGM_HUB__"
	// DefaultHub is used when the hub has no public URL.
	DefaultHub = "http://localhost:7777"
)

// Raw returns the embedded document with its placeholders untouched.
func Raw() []byte { return bytes.Clone(spec) }

// YAML returns the document with the version and hub URL filled in.
func YAML(version, hub string) []byte {
	if hub == "" {
		hub = DefaultHub
	}
	r := strings.NewReplacer(VersionPlaceholder, scrub(version), HubPlaceholder, scrub(strings.TrimRight(hub, "/")))
	return []byte(r.Replace(string(spec)))
}

// scrub keeps a substituted value inside its double-quoted YAML scalar.
func scrub(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, s)
}

// Parse returns the rendered document as a YAML node tree (a DocumentNode).
func Parse(version, hub string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(YAML(version, hub), &doc); err != nil {
		return nil, fmt.Errorf("openapi.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("openapi.yaml: top level is not a mapping")
	}
	return &doc, nil
}

// JSON renders the document as JSON, keeping the YAML key order (so the reference lists
// operations in the order they are written). Each operation's x-ogm-cli command is also
// appended to its description, since renderers don't show unknown extensions.
func JSON(version, hub string) ([]byte, error) {
	doc, err := Parse(version, hub)
	if err != nil {
		return nil, err
	}
	for _, op := range operationNodes(doc) {
		cli := mapGet(op.node, "x-ogm-cli")
		if cli == nil || cli.Value == "" {
			continue
		}
		line := "**CLI:** `" + cli.Value + "`"
		if d := mapGet(op.node, "description"); d != nil {
			d.Value = strings.TrimRight(d.Value, "\n") + "\n\n" + line + "\n"
			d.Style = 0
		} else {
			op.node.Content = append(op.node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "description"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: line})
		}
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, doc); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func writeJSON(buf *bytes.Buffer, n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode:
		return writeJSON(buf, n.Content[0])
	case yaml.AliasNode:
		return writeJSON(buf, n.Alias)
	case yaml.MappingNode:
		buf.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				buf.WriteByte(',')
			}
			k, _ := json.Marshal(n.Content[i].Value)
			buf.Write(k)
			buf.WriteByte(':')
			if err := writeJSON(buf, n.Content[i+1]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, c); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case yaml.ScalarNode:
		var v any
		switch n.ShortTag() {
		case "!!null":
			v = nil
		case "!!bool", "!!int":
			if err := n.Decode(&v); err != nil {
				return fmt.Errorf("line %d: %w", n.Line, err)
			}
		case "!!float":
			var f float64
			if err := n.Decode(&f); err != nil {
				return fmt.Errorf("line %d: %w", n.Line, err)
			}
			if math.IsInf(f, 0) || math.IsNaN(f) {
				return fmt.Errorf("line %d: %q has no JSON form", n.Line, n.Value)
			}
			v = f
		default: // !!str, !!timestamp, !!binary: keep the text as written
			v = n.Value
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
	default:
		return fmt.Errorf("line %d: unsupported YAML node", n.Line)
	}
	return nil
}

// Operation is one documented operation.
type Operation struct {
	Method      string // upper case
	Path        string
	OperationID string
	Summary     string
	Description string
	Tags        []string
	CLI         string // x-ogm-cli
	Protocol    bool   // x-ogm-protocol: dispatched outside the route mux
	Public      bool   // security: [] (no credential needed)
}

// Tag is a documented tag.
type Tag struct{ Name, Description string }

// HTTPMethods are the OpenAPI path-item keys that hold operations, in display order.
var HTTPMethods = []string{"get", "head", "post", "put", "patch", "delete", "options", "trace"}

type opNode struct {
	method, path string
	node         *yaml.Node
}

func operationNodes(doc *yaml.Node) []opNode {
	var out []opNode
	paths := mapGet(doc.Content[0], "paths")
	if paths == nil {
		return nil
	}
	for i := 0; i+1 < len(paths.Content); i += 2 {
		path, item := paths.Content[i].Value, paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			m := item.Content[j].Value
			for _, hm := range HTTPMethods {
				if m == hm {
					out = append(out, opNode{method: strings.ToUpper(m), path: path, node: item.Content[j+1]})
				}
			}
		}
	}
	return out
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// Operations lists every operation in document order.
func Operations(doc *yaml.Node) []Operation {
	var out []Operation
	for _, on := range operationNodes(doc) {
		op := Operation{Method: on.method, Path: on.path}
		if n := mapGet(on.node, "operationId"); n != nil {
			op.OperationID = n.Value
		}
		if n := mapGet(on.node, "summary"); n != nil {
			op.Summary = n.Value
		}
		if n := mapGet(on.node, "description"); n != nil {
			op.Description = n.Value
		}
		if n := mapGet(on.node, "x-ogm-cli"); n != nil {
			op.CLI = n.Value
		}
		if n := mapGet(on.node, "x-ogm-protocol"); n != nil {
			op.Protocol = n.Value == "true"
		}
		if n := mapGet(on.node, "security"); n != nil && n.Kind == yaml.SequenceNode && len(n.Content) == 0 {
			op.Public = true
		}
		if n := mapGet(on.node, "tags"); n != nil {
			for _, t := range n.Content {
				op.Tags = append(op.Tags, t.Value)
			}
		}
		out = append(out, op)
	}
	return out
}

// Tags lists the top-level tags in document order.
func Tags(doc *yaml.Node) []Tag {
	var out []Tag
	if ts := mapGet(doc.Content[0], "tags"); ts != nil {
		for _, t := range ts.Content {
			var tag Tag
			if n := mapGet(t, "name"); n != nil {
				tag.Name = n.Value
			}
			if n := mapGet(t, "description"); n != nil {
				tag.Description = n.Value
			}
			out = append(out, tag)
		}
	}
	return out
}

// byTag groups operations under their first tag, in tag order.
func byTag(doc *yaml.Node) ([]Tag, map[string][]Operation) {
	groups := map[string][]Operation{}
	for _, op := range Operations(doc) {
		t := "Other"
		if len(op.Tags) > 0 {
			t = op.Tags[0]
		}
		groups[t] = append(groups[t], op)
	}
	tags := Tags(doc)
	known := map[string]bool{}
	for _, t := range tags {
		known[t.Name] = true
	}
	var extra []string
	for t := range groups {
		if !known[t] {
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	for _, t := range extra {
		tags = append(tags, Tag{Name: t})
	}
	return tags, groups
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	return strings.Join(strings.Fields(s), " ")
}

// LLMsTxt renders a plain-text index of the API following the llms.txt convention
// (https://llmstxt.org): a title, a summary, pointers, then one line per operation.
func LLMsTxt(version, hub string) ([]byte, error) {
	doc, err := Parse(version, hub)
	if err != nil {
		return nil, err
	}
	if hub == "" {
		hub = DefaultHub
	}
	hub = strings.TrimRight(hub, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "# open-gpu-mesh API\n\n")
	fmt.Fprintf(&b, "> REST API of the open-gpu-mesh hub at %s (version %s): run containers on a pool of shared GPUs, with job queues, an HTTPS gateway, autoscaling, secrets, volumes, a KV store, file and S3 storage, and an OCI registry.\n\n", hub, version)
	b.WriteString("Authenticate with `Authorization: Bearer <key>` (personal `ogm_…` key, scoped `ogmk_…` key, or an instance identity JWT) or the `OGM-Api-Key` header. ")
	b.WriteString("Errors are RFC 7807 problem+json. Lists are `{\"items\": [...]}`. Project-scoped resources live under `/v1/projects/{project}/`. ")
	b.WriteString("Every operation below is described in full (parameters, bodies, responses, examples) in the OpenAPI document.\n\n")
	b.WriteString("## Docs\n\n")
	fmt.Fprintf(&b, "- [OpenAPI 3.1 document (JSON)](%s/openapi.json): the complete, machine-readable API description\n", hub)
	fmt.Fprintf(&b, "- [OpenAPI document (YAML)](%s/openapi.yaml)\n", hub)
	fmt.Fprintf(&b, "- [Interactive API reference](%s/docs): human-readable, with a request runner\n", hub)
	fmt.Fprintf(&b, "- [MCP endpoint](%s/v1/mcp): Model Context Protocol server (Streamable HTTP, JSON-RPC over POST) exposing the API as tools; authenticate with the same key\n", hub)
	b.WriteString("\n")
	tags, groups := byTag(doc)
	for _, t := range tags {
		ops := groups[t.Name]
		if len(ops) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", t.Name)
		if d := firstSentence(t.Description); d != "" {
			fmt.Fprintf(&b, "%s\n\n", d)
		}
		for _, op := range ops {
			fmt.Fprintf(&b, "- %s %s — %s", op.Method, op.Path, op.Summary)
			if op.CLI != "" {
				fmt.Fprintf(&b, " (CLI: `%s`)", op.CLI)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// Surface renders docs/api-surface.md: a route index generated from the document.
func Surface() ([]byte, error) {
	doc, err := Parse("", "")
	if err != nil {
		return nil, err
	}
	cell := func(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
	var b strings.Builder
	b.WriteString("<!-- Code generated from internal/apidocs/openapi.yaml by `go generate ./internal/server`. DO NOT EDIT. -->\n\n")
	b.WriteString("# API surface\n\n")
	ops := Operations(doc)
	fmt.Fprintf(&b, "Every operation of the hub's HTTP API (%d operations). The full reference, with parameters, bodies and examples, is served by every hub at `/docs` (Scalar) and `/openapi.json`; see also [api.md](api.md). Protocol endpoints (gateway, OCI registry, S3) are marked *protocol*. CI fails when this file is stale or a route is undocumented.\n\n", len(ops))
	tags, groups := byTag(doc)
	for _, t := range tags {
		if len(groups[t.Name]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", t.Name)
		b.WriteString("| Method | Path | Operation | Summary | CLI |\n|---|---|---|---|---|\n")
		for _, op := range groups[t.Name] {
			sum := op.Summary
			if op.Protocol {
				sum += " *(protocol)*"
			}
			if op.Public {
				sum += " *(public)*"
			}
			cli := ""
			if op.CLI != "" {
				cli = "`" + cell(op.CLI) + "`"
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s |\n", op.Method, cell(op.Path), op.OperationID, cell(sum), cli)
		}
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}
