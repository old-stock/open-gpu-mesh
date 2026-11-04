package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Caller performs one ogm REST API request. body is JSON-encoded when non-nil.
// A non-nil error means the request could not be made at all; API errors are
// reported through status (>= 400) and the problem-details respBody.
type Caller interface {
	Call(ctx context.Context, method, path string, body any) (status int, respBody []byte, err error)
}

// Request is a full API request: Body is JSON-encoded unless it is a []byte
// (sent raw, e.g. a KV value). Headers are extra request headers.
type Request struct {
	Method, Path string
	Body         any
	Headers      map[string]string
}

// Response is a full API response including headers (ETag, OGM-Version...).
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// RawCaller is optionally implemented by a Caller that supports raw bodies,
// request headers and response headers. Both built-in Callers implement it.
type RawCaller interface {
	Do(ctx context.Context, req Request) (Response, error)
}

func buildRequest(ctx context.Context, base, key string, r Request) (*http.Request, error) {
	var rd io.Reader = http.NoBody
	isJSON := false
	switch b := r.Body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd, isJSON = bytes.NewReader(enc), true
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, base+r.Path, rd)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("OGM-Api-Key", key)
	}
	req.Header.Set("Accept", "application/json")
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// HTTPCaller calls a remote hub with an API key (used by `ogm mcp` on stdio).
type HTTPCaller struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewHTTPCaller returns a Caller for the hub at baseURL.
func NewHTTPCaller(baseURL, apiKey string) *HTTPCaller {
	// Job long-polls wait up to 60 s server-side.
	return &HTTPCaller{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Call implements Caller.
func (c *HTTPCaller) Call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	r, err := c.Do(ctx, Request{Method: method, Path: path, Body: body})
	return r.Status, r.Body, err
}

// Do implements RawCaller.
func (c *HTTPCaller) Do(ctx context.Context, r Request) (Response, error) {
	req, err := buildRequest(ctx, c.BaseURL, c.APIKey, r)
	if err != nil {
		return Response{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}, err
}

// HandlerCaller calls an http.Handler in-process (used by the hub so MCP
// tools go through exactly the same routes, auth and validation as the API).
type HandlerCaller struct {
	Handler func() http.Handler
	APIKey  string
}

// Call implements Caller.
func (c *HandlerCaller) Call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	r, err := c.Do(ctx, Request{Method: method, Path: path, Body: body})
	return r.Status, r.Body, err
}

// Do implements RawCaller.
func (c *HandlerCaller) Do(ctx context.Context, r Request) (Response, error) {
	req, err := buildRequest(ctx, "http://ogm.internal", c.APIKey, r)
	if err != nil {
		return Response{}, err
	}
	req.RemoteAddr = "127.0.0.1:0"
	rec := &recorder{header: http.Header{}, code: http.StatusOK}
	c.Handler().ServeHTTP(rec, req)
	return Response{Status: rec.code, Header: rec.header, Body: rec.buf.Bytes()}, nil
}

// do performs a full request, falling back to Call (no headers, JSON-only)
// for Callers that don't implement RawCaller.
func (s *Server) do(ctx context.Context, r Request) (Response, error) {
	if rc, ok := s.Caller.(RawCaller); ok {
		return rc.Do(ctx, r)
	}
	body := r.Body
	if b, ok := body.([]byte); ok {
		body = string(b)
	}
	st, b, err := s.Caller.Call(ctx, r.Method, r.Path, body)
	return Response{Status: st, Header: http.Header{}, Body: b}, err
}

// recorder is a minimal ResponseWriter (like httptest.ResponseRecorder,
// without pulling the test package into the binary).
type recorder struct {
	header      http.Header
	code        int
	wroteHeader bool
	buf         bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.code, r.wroteHeader = code, true
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	if r.buf.Len() > 16<<20 {
		return len(b), nil // drop; tool output is truncated far below this
	}
	return r.buf.Write(b)
}
func (r *recorder) Flush() {}
