package server

import (
	"net/http"
	"sync"

	"github.com/old-stock/open-gpu-mesh/internal/mcp"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// registerMCPRoutes mounts the Model Context Protocol endpoint (Streamable
// HTTP) at /v1/mcp. Tools run as in-process sub-requests against the API
// handler with the caller's own key, so auth, project membership and
// validation are exactly those of the REST API. ?project= sets the default
// project for project-scoped tools.
func (s *Server) registerMCPRoutes(mux *http.ServeMux) {
	sessions := mcp.NewSessions()
	var (
		once sync.Once
		root http.Handler
	)
	api := func() http.Handler {
		once.Do(func() { root = s.Handler() })
		return root
	}
	h := s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		srv := mcp.New(&mcp.HandlerCaller{Handler: api, APIKey: apiKeyFrom(r)}, r.URL.Query().Get("project"))
		srv.Logger = s.log
		srv.HandleHTTP(w, r, sessions, u.ID)
	})
	mux.HandleFunc("POST /v1/mcp", h)
	mux.HandleFunc("DELETE /v1/mcp", h)
	mux.HandleFunc("GET /v1/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST, DELETE")
		problem(w, http.StatusMethodNotAllowed, "method not allowed", "MCP endpoint: send JSON-RPC with POST (no server-initiated SSE stream)")
	})
}
