package server

import (
	"crypto/rand"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const (
	speedMaxDown = 100 << 20 // bytes per download request
	speedMaxUp   = 100 << 20 // bytes per upload request
)

// speedBuf is incompressible data served by the download test.
var speedBuf = func() []byte { b := make([]byte, 1<<20); _, _ = rand.Read(b); return b }()

// bearerNode resolves a node token from the Authorization header (nil if absent/invalid).
func (s *Server) bearerNode(r *http.Request) *store.NodeRow {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(tok, "ogmn_") {
		return nil
	}
	n, err := s.st.NodeByToken(tok)
	if err != nil {
		return nil
	}
	return n
}

// nodeOrUser allows a node token or a user API key (speed tests run before and after joining).
func (s *Server) nodeOrUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.bearerNode(r) != nil {
			h(w, r)
			return
		}
		if key := apiKeyFrom(r); key != "" {
			if _, err := s.st.UserByKey(key); err == nil {
				h(w, r)
				return
			}
		}
		problem(w, http.StatusUnauthorized, "unauthorized", "node token or API key required")
	}
}

func (s *Server) registerAssessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/speedtest/ping", s.nodeOrUser(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /v1/speedtest/down", s.nodeOrUser(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if n <= 0 || n > speedMaxDown {
			n = speedMaxDown
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.Header().Set("Cache-Control", "no-store")
		for n > 0 {
			k := min(n, int64(len(speedBuf)))
			if _, err := w.Write(speedBuf[:k]); err != nil {
				return
			}
			n -= k
		}
	}))
	mux.HandleFunc("POST /v1/speedtest/up", s.nodeOrUser(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, speedMaxUp))
		writeJSON(w, http.StatusOK, map[string]int64{"bytes": n})
	}))

	// The agent host submits its onboarding audit with its node token.
	mux.HandleFunc("POST /v1/agent/assessment", func(w http.ResponseWriter, r *http.Request) {
		n := s.bearerNode(r)
		if n == nil {
			problem(w, http.StatusUnauthorized, "unauthorized", "node token required")
			return
		}
		var a api.Assessment
		if !decode(w, r, &a) {
			return
		}
		if err := s.st.SaveAssessment(n.ID, a); err != nil {
			storeErr(w, err, "assessment")
			return
		}
		s.st.AddEvent("", "", "", n.ID, "node.assessed", "Node "+n.Name+" assessed: tier "+a.Tier)
		writeJSON(w, http.StatusOK, map[string]string{"node_id": n.ID, "tier": a.Tier})
	})
	mux.HandleFunc("GET /v1/nodes/{node}/assessment", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		a, err := s.st.Assessment(r.PathValue("node"))
		if err != nil {
			storeErr(w, err, "assessment")
			return
		}
		writeJSON(w, http.StatusOK, a)
	}))
}
