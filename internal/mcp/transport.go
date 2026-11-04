package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ServeStdio runs an MCP server over newline-delimited JSON-RPC until in hits
// EOF or ctx is cancelled. Requests are handled concurrently (a job long-poll
// does not block ping); responses are written one per line. Nothing but
// JSON-RPC is ever written to out.
func ServeStdio(ctx context.Context, in io.Reader, out io.Writer, caller Caller, project string) error {
	return New(caller, project).ServeStdio(ctx, in, out)
}

// ServeStdio is the method form of the package-level ServeStdio.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wmu      sync.Mutex
		wg       sync.WaitGroup
		inflight sync.Map // request id (raw JSON) -> context.CancelFunc
	)
	write := func(b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = out.Write(append(b, '\n'))
	}
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		br := bufio.NewReaderSize(in, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					readErr <- ctx.Err()
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line := <-lines:
			var peek request
			if json.Unmarshal(line, &peek) == nil && peek.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(peek.Params, &p) == nil {
					if c, ok := inflight.Load(string(p.RequestID)); ok {
						c.(context.CancelFunc)()
					}
				}
				continue
			}
			rctx, rcancel := context.WithCancel(ctx)
			id := string(peek.ID)
			if id != "" {
				inflight.Store(id, rcancel)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer rcancel()
				if id != "" {
					defer inflight.Delete(id)
				}
				if resp := s.Handle(rctx, line); resp != nil && rctx.Err() == nil {
					write(resp)
				}
			}()
		}
	}
}

// Sessions tracks Streamable HTTP sessions (Mcp-Session-Id), each bound to
// the principal (user) that created it.
type Sessions struct {
	TTL time.Duration
	mu  sync.Mutex
	m   map[string]*session
}

type session struct {
	principal string
	lastSeen  time.Time
}

// NewSessions returns an empty session table with a 24 h idle TTL.
func NewSessions() *Sessions { return &Sessions{TTL: 24 * time.Hour, m: map[string]*session{}} }

func (ss *Sessions) create(principal string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := "mcp_" + hex.EncodeToString(b)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := time.Now()
	if len(ss.m) > 1000 {
		for k, v := range ss.m {
			if now.Sub(v.lastSeen) > ss.TTL {
				delete(ss.m, k)
			}
		}
	}
	ss.m[id] = &session{principal: principal, lastSeen: now}
	return id
}

func (ss *Sessions) valid(id, principal string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.m[id]
	if !ok || s.principal != principal || time.Since(s.lastSeen) > ss.TTL {
		return false
	}
	s.lastSeen = time.Now()
	return true
}

func (ss *Sessions) remove(id, principal string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if s, ok := ss.m[id]; ok && s.principal == principal {
		delete(ss.m, id)
		return true
	}
	return false
}

// HandleHTTP serves the MCP Streamable HTTP transport for an already
// authenticated principal: POST carries JSON-RPC (answered as
// application/json), DELETE ends a session, GET is 405 (no server-initiated
// stream).
func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request, sessions *Sessions, principal string) {
	httpErr := func(status int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(errResp(nil, codeInvalidRequest, msg))
	}
	sid := r.Header.Get("Mcp-Session-Id")
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		if sid == "" || !sessions.remove(sid, principal) {
			httpErr(http.StatusNotFound, "unknown session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		httpErr(http.StatusMethodNotAllowed, "this server does not offer a server-initiated SSE stream; use POST")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supported(v) {
		httpErr(http.StatusBadRequest, "unsupported MCP-Protocol-Version "+v)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		httpErr(http.StatusBadRequest, err.Error())
		return
	}
	init := containsInitialize(body)
	if sid != "" && !init && !sessions.valid(sid, principal) {
		// Per spec: an unknown/expired session gets 404 and the client re-initializes.
		httpErr(http.StatusNotFound, "unknown or expired Mcp-Session-Id; send initialize again")
		return
	}
	resp := s.Handle(r.Context(), body)
	if init && resp != nil && initOK(resp) {
		w.Header().Set("Mcp-Session-Id", sessions.create(principal))
	}
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func containsInitialize(body []byte) bool {
	body = bytes.TrimSpace(body)
	var msgs []request
	if len(body) > 0 && body[0] == '[' {
		_ = json.Unmarshal(body, &msgs)
	} else {
		var m request
		if json.Unmarshal(body, &m) == nil {
			msgs = []request{m}
		}
	}
	for _, m := range msgs {
		if m.Method == "initialize" {
			return true
		}
	}
	return false
}

// initOK reports whether an initialize response (or a batch holding one) succeeded.
func initOK(resp []byte) bool {
	var one response
	if json.Unmarshal(resp, &one) == nil {
		return one.Error == nil
	}
	return true
}
