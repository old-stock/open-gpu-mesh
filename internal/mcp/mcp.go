// Package mcp implements a Model Context Protocol server (JSON-RPC 2.0) that
// exposes the ogm API as tools and the ogm docs as resources.
//
// Two transports share one implementation:
//   - stdio (ServeStdio), used by `ogm mcp` with the CLI's saved login;
//   - Streamable HTTP (Server.HandleHTTP), mounted on the hub at /v1/mcp.
//
// Every tool talks to the REST API through a Caller, so there is exactly one
// implementation per operation: over HTTP for stdio, in-process on the hub.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// ProtocolVersion is the newest MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// SupportedVersions lists accepted protocol revisions, newest first.
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

func supported(v string) bool {
	for _, s := range SupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeNoResource     = -32002
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *request) isNotification() bool { return len(r.ID) == 0 }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server answers MCP requests. It is cheap to create; the hub makes one per
// HTTP request (bound to the caller's API key).
type Server struct {
	Caller  Caller
	Project string // default project for project-scoped tools
	Logger  *slog.Logger
}

// New returns a server that performs API calls with c. project defaults to "default".
func New(c Caller, project string) *Server {
	if project == "" {
		project = "default"
	}
	return &Server{Caller: c, Project: project, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s.Logger
}

// Handle processes one JSON-RPC message or batch and returns the encoded
// response, or nil when nothing should be sent back (notifications only).
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var msgs []json.RawMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return encode(errResp(nil, codeParseError, "parse error: "+err.Error()))
		}
		if len(msgs) == 0 {
			return encode(errResp(nil, codeInvalidRequest, "empty batch"))
		}
		var out []*response
		for _, m := range msgs {
			if r := s.handleOne(ctx, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return encode(out)
	}
	if r := s.handleOne(ctx, raw); r != nil {
		return encode(r)
	}
	return nil
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(errResp(nil, codeInternal, err.Error()))
	}
	return b
}

func errResp(id json.RawMessage, code int, msg string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) handleOne(ctx context.Context, raw json.RawMessage) *response {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errResp(nil, codeParseError, "parse error: "+err.Error())
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if req.Method == "" && len(req.ID) > 0 {
			// A response from the client (we never send requests); ignore.
			return nil
		}
		return errResp(req.ID, codeInvalidRequest, `invalid request: need "jsonrpc":"2.0" and a method`)
	}
	result, err := s.dispatch(ctx, &req)
	if req.isNotification() {
		if err != nil {
			s.log().Debug("notification failed", "method", req.Method, "err", err)
		}
		return nil
	}
	if err != nil {
		if re, ok := err.(*rpcError); ok {
			return &response{JSONRPC: "2.0", ID: req.ID, Error: re}
		}
		return errResp(req.ID, codeInternal, err.Error())
	}
	if result == nil {
		result = map[string]any{}
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) dispatch(ctx context.Context, req *request) (any, error) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := ProtocolVersion
		if supported(p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false},
				"prompts":   map[string]any{"listChanged": false},
			},
			"serverInfo":   map[string]any{"name": "ogm", "title": "open-gpu-mesh", "version": api.Version},
			"instructions": s.instructions(),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized", "notifications/cancelled", "notifications/progress", "notifications/roots/list_changed":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": toolDescriptors()}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
		t := findTool(p.Name)
		if t == nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		return s.callTool(ctx, t, p.Arguments), nil
	case "resources/list":
		return map[string]any{"resources": resourceList()}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.URI == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: uri required"}
		}
		text, ok := readResource(p.URI)
		if !ok {
			return nil, &rpcError{Code: codeNoResource, Message: "resource not found", Data: map[string]string{"uri": p.URI}}
		}
		return map[string]any{"contents": []any{map[string]any{"uri": p.URI, "mimeType": "text/markdown", "text": text}}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "logging/setLevel":
		return map[string]any{}, nil
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
}

func (s *Server) instructions() string {
	return fmt.Sprintf(`ogm (open-gpu-mesh) runs containers and job queues on a pool of volunteer GPU/CPU machines.
The default project is %q; every project-scoped tool takes an optional "project" argument.
Start with cluster_status, then list_groups / list_nodes. Deploy with deploy_group (a full container group spec,
upserted by name; read resource ogm://docs/spec-reference for every field). Use group_logs and group_events to debug,
list_gpu_classes for prices and availability. For anything without a dedicated tool, call api_operations to find the
endpoint and api_get to read it. Secret values are write-only: list_secrets returns names only.`, s.Project)
}
