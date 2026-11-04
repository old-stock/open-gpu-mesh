package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
	"github.com/old-stock/open-gpu-mesh/internal/tunnel"
)

// Gateway load-balances HTTP traffic to group instances through agent tunnels.
type Gateway struct {
	s         *Server
	mu        sync.Mutex
	inflight  map[string]int // instance id -> active requests
	rr        map[string]int // group id -> round robin counter
	cache     map[string]*backendSet
	waiters   map[string]chan struct{}
	transport *http.Transport
	// HTTP autoscaler demand tracking (per group): requests in flight and requests queued.
	ginflight map[string]int
	gwaiting  map[string]int
	samples   map[string][]float64 // 1 s demand samples, newest last
	lowSince  map[string]time.Time
	domains   map[string]string // custom domain -> group id
	domainsAt time.Time
}

type backend struct {
	inst, node string
	port       int
}

type backendSet struct {
	ready    []backend
	starting int // live instances that are not ready yet
	loaded   time.Time
}

var (
	errNoBackends      = errors.New("no ready instances")
	errQueueTimeout    = errors.New("client request timeout: all instances busy")
	errResponseTimeout = errors.New("server response timeout")
)

func newGateway(s *Server) *Gateway {
	gw := &Gateway{s: s, inflight: map[string]int{}, rr: map[string]int{}, cache: map[string]*backendSet{}, waiters: map[string]chan struct{}{},
		ginflight: map[string]int{}, gwaiting: map[string]int{}, samples: map[string][]float64{}, lowSince: map[string]time.Time{}}
	gw.transport = &http.Transport{
		DialContext:           gw.dial,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	return gw
}

// dial resolves "inst_xxx.p8080.ogm:80" to a tunnel stream.
func (gw *Gateway) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)
	parts := strings.Split(host, ".")
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "p") {
		return nil, &tunnel.DialError{Err: fmt.Errorf("bad backend address %q", addr)}
	}
	port, err := strconv.Atoi(parts[1][1:])
	if err != nil {
		return nil, &tunnel.DialError{Err: err}
	}
	in, err := gw.s.st.Instance(parts[0])
	if err != nil {
		return nil, &tunnel.DialError{Err: err}
	}
	return gw.s.hub.dial(ctx, in.NodeID, in.ID, port)
}

// invalidate drops cached backend sets and wakes queued requests.
func (gw *Gateway) invalidate() {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.cache = map[string]*backendSet{}
	for k, ch := range gw.waiters {
		close(ch)
		delete(gw.waiters, k)
	}
}

func (gw *Gateway) wake(groupID string) {
	if ch, ok := gw.waiters[groupID]; ok {
		close(ch)
		delete(gw.waiters, groupID)
	}
}

// Inflight returns the active gateway requests for an instance.
func (gw *Gateway) Inflight(instID string) int {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return gw.inflight[instID]
}

func (gw *Gateway) backendsLocked(g *store.GroupRow) *backendSet {
	if bs, ok := gw.cache[g.ID]; ok && time.Since(bs.loaded) < 2*time.Second {
		return bs
	}
	bs := &backendSet{loaded: time.Now()}
	insts, _ := gw.s.st.LiveInstances(g.ID)
	online := gw.s.hub.online()
	port := g.Spec.Networking.Port
	for _, in := range insts {
		if in.State == api.InstStopping {
			continue
		}
		if in.State == api.InstRunning && in.Ready && online[in.NodeID] != nil {
			bs.ready = append(bs.ready, backend{inst: in.ID, node: in.NodeID, port: port})
		} else {
			bs.starting++
		}
	}
	gw.cache[g.ID] = bs
	return bs
}

func (gw *Gateway) acquire(ctx context.Context, g *store.GroupRow, tried map[string]bool, deadline time.Time) (backend, error) {
	n := g.Spec.Networking
	limit := n.MaxConcurrency
	if n.SingleConnectionLimit {
		limit = 1
	}
	woke, waiting := false, false
	defer func() {
		if waiting {
			gw.mu.Lock()
			gw.gwaiting[g.ID]--
			gw.mu.Unlock()
		}
	}()
	for {
		gw.mu.Lock()
		bs := gw.backendsLocked(g)
		var candidates, avail []backend
		for _, b := range bs.ready {
			if tried[b.inst] {
				continue
			}
			candidates = append(candidates, b)
			if limit == 0 || gw.inflight[b.inst] < limit {
				avail = append(avail, b)
			}
		}
		if len(avail) > 0 {
			var pick backend
			if n.LoadBalancer == api.LBRoundRobin {
				gw.rr[g.ID]++
				pick = avail[gw.rr[g.ID]%len(avail)]
			} else {
				pick = avail[0]
				start := gw.rr[g.ID] % len(avail)
				gw.rr[g.ID]++
				best := -1
				for i := range avail {
					b := avail[(start+i)%len(avail)]
					if best < 0 || gw.inflight[b.inst] < best {
						pick, best = b, gw.inflight[b.inst]
					}
				}
			}
			gw.inflight[pick.inst]++
			gw.ginflight[g.ID]++
			gw.mu.Unlock()
			return pick, nil
		}
		if !waiting {
			waiting = true
			gw.gwaiting[g.ID]++
		}
		// Nothing available. Scale an autoscaled group up from zero; otherwise fail fast if
		// nothing will ever become available.
		if len(candidates) == 0 && bs.starting == 0 && !woke {
			if g.Spec.HTTPAutoscaler != nil {
				gw.mu.Unlock()
				woke = true
				gw.s.scaleFromZero(g.ID)
				continue
			}
			gw.mu.Unlock()
			return backend{}, errNoBackends
		}
		ch, ok := gw.waiters[g.ID]
		if !ok {
			ch = make(chan struct{})
			gw.waiters[g.ID] = ch
		}
		gw.mu.Unlock()
		wait := time.Until(deadline)
		if wait <= 0 {
			if len(candidates) == 0 {
				return backend{}, errNoBackends
			}
			return backend{}, errQueueTimeout
		}
		if wait > 2*time.Second {
			wait = 2 * time.Second // re-check periodically (cache TTL)
		}
		t := time.NewTimer(wait)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return backend{}, ctx.Err()
		}
		t.Stop()
	}
}

func (gw *Gateway) release(groupID string, b backend) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.inflight[b.inst] > 0 {
		gw.inflight[b.inst]--
	}
	if gw.inflight[b.inst] == 0 {
		delete(gw.inflight, b.inst)
	}
	if gw.ginflight[groupID] > 0 {
		gw.ginflight[groupID]--
	}
	gw.wake(groupID)
}

// noCloseBody lets us retry a request after the transport "closes" the body on dial failure.
type noCloseBody struct{ io.Reader }

func (noCloseBody) Close() error { return nil }

type gwRoundTripper struct {
	gw *Gateway
	g  *store.GroupRow
}

func (rt *gwRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	n := rt.g.Spec.Networking
	queueDeadline := time.Now().Add(time.Duration(n.ClientRequestTimeoutMS) * time.Millisecond)
	respTimeout := time.Duration(n.ServerResponseTimeoutMS) * time.Millisecond
	idempotent := req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions
	origBody := req.Body
	if origBody != nil && origBody != http.NoBody {
		req.Body = noCloseBody{origBody}
		defer origBody.Close()
	}
	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		b, err := rt.gw.acquire(req.Context(), rt.g, tried, queueDeadline)
		if err != nil {
			if lastErr != nil && errors.Is(err, errNoBackends) {
				return nil, lastErr
			}
			return nil, err
		}
		ctx, cancel := context.WithCancel(req.Context())
		fired := make(chan struct{})
		timer := time.AfterFunc(respTimeout, func() { close(fired); cancel() })
		out := req.Clone(ctx)
		out.URL.Scheme = "http"
		out.URL.Host = fmt.Sprintf("%s.p%d.ogm", b.inst, b.port)
		resp, err := rt.gw.transport.RoundTrip(out)
		timedOut := !timer.Stop()
		if err != nil {
			cancel()
			rt.gw.release(rt.g.ID, b)
			select {
			case <-fired:
				timedOut = true
			default:
			}
			if timedOut {
				return nil, errResponseTimeout
			}
			lastErr = err
			tried[b.inst] = true
			var de *tunnel.DialError
			if errors.As(err, &de) || (idempotent && (req.Body == nil || req.Body == http.NoBody)) {
				continue
			}
			return nil, err
		}
		release := func() { cancel(); rt.gw.release(rt.g.ID, b) }
		resp.Header.Set("X-OGM-Instance-Id", b.inst)
		resp.Header.Set("X-OGM-Node-Id", b.node)
		if rw, ok := resp.Body.(io.ReadWriteCloser); ok {
			resp.Body = &releaseRWBody{ReadWriteCloser: rw, release: release}
		} else {
			resp.Body = &releaseBody{ReadCloser: resp.Body, release: release}
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errNoBackends
	}
	return nil, lastErr
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

type releaseRWBody struct {
	io.ReadWriteCloser
	once    sync.Once
	release func()
}

func (b *releaseRWBody) Close() error {
	err := b.ReadWriteCloser.Close()
	b.once.Do(b.release)
	return err
}

// serve proxies a request to group g. prefix is stripped from the path (path routing).
func (gw *Gateway) serve(w http.ResponseWriter, r *http.Request, g *store.GroupRow, prefix string) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	defer func() {
		if rec.code == 0 {
			rec.code = 200
		}
		gw.s.metrics.observe(g, rec.code, time.Since(start))
	}()
	gw.serveInner(rec, r, g, prefix)
}

func (gw *Gateway) serveInner(w http.ResponseWriter, r *http.Request, g *store.GroupRow, prefix string) {
	n := g.Spec.Networking
	if n == nil {
		problem(w, http.StatusNotFound, "gateway not enabled", "container group "+g.Name+" has no networking.port configured")
		return
	}
	if n.Auth && !gw.authorize(r, g) {
		problem(w, http.StatusUnauthorized, "unauthorized", "this gateway requires an API key (OGM-Api-Key header, Bearer token, or ?ogm_token=)")
		return
	}
	// Strip credentials before forwarding.
	q := r.URL.Query()
	if q.Has("ogm_token") {
		q.Del("ogm_token")
		r.URL.RawQuery = q.Encode()
	}
	r.Header.Del("OGM-Api-Key")
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ogm") { // never forward hub credentials
		r.Header.Del("Authorization")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
			if prefix != "" {
				p := strings.TrimPrefix(pr.In.URL.Path, prefix)
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				pr.Out.URL.Path = p
				pr.Out.URL.RawPath = ""
				pr.Out.Header.Set("X-Forwarded-Prefix", strings.TrimSuffix(prefix, "/"))
			}
		},
		Transport:     &gwRoundTripper{gw: gw, g: g},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, errNoBackends):
				problem(w, http.StatusServiceUnavailable, "no ready instances", "container group "+g.Name+" has no ready instances")
			case errors.Is(err, errQueueTimeout):
				problem(w, http.StatusGatewayTimeout, "client request timeout", err.Error())
			case errors.Is(err, errResponseTimeout):
				problem(w, 524, "server response timeout", fmt.Sprintf("no response within %d ms", n.ServerResponseTimeoutMS))
			case errors.Is(err, context.Canceled):
				w.WriteHeader(499)
			default:
				problem(w, http.StatusBadGateway, "backend unreachable", err.Error())
			}
		},
	}
	proxy.ServeHTTP(w, r)
}

func (gw *Gateway) authorize(r *http.Request, g *store.GroupRow) bool {
	key := apiKeyFrom(r)
	if key == "" {
		key = r.URL.Query().Get("ogm_token")
	}
	if key == "" {
		return false
	}
	if g.GatewayToken != "" && key == g.GatewayToken {
		return true
	}
	u, sc, _ := gw.s.resolveCredential(key)
	if u == nil {
		return false
	}
	if sc != nil && sc.Project != "" {
		return sc.Project == g.Project
	}
	if u.Role == "admin" {
		return true
	}
	var n int
	_ = gw.s.st.DB.QueryRow(`SELECT COUNT(*) FROM projects p LEFT JOIN project_members m ON m.project=p.name AND m.user_id=?
		WHERE p.name=? AND (p.owner_id=? OR m.user_id IS NOT NULL)`, u.ID, g.Project, u.ID).Scan(&n)
	return n > 0
}

// handlePathGateway is /gw/{project}/{group}/...
func (s *Server) handlePathGateway(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/gw/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		problem(w, http.StatusNotFound, "not found", "use /gw/{project}/{group}/...")
		return
	}
	if len(parts) == 2 {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
		return
	}
	g, err := s.st.Group(parts[0], parts[1])
	if err != nil {
		problem(w, http.StatusNotFound, "not found", "container group not found")
		return
	}
	s.gw.serve(w, r, g, "/gw/"+parts[0]+"/"+parts[1])
}

// hostGateway routes {group}-{project}.{GatewayDomain}. Returns false if not a gateway host.
func (s *Server) hostGateway(w http.ResponseWriter, r *http.Request) bool {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.reservedHost(host) {
		return false
	}
	if g := s.groupForDomain(host); g != nil {
		s.gw.serve(w, r, g, "")
		return true
	}
	if s.cfg.GatewayDomain == "" {
		return false
	}
	suffix := "." + s.cfg.GatewayDomain
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	sub := strings.TrimSuffix(host, suffix)
	for i := 0; i < len(sub); i++ {
		if sub[i] != '-' {
			continue
		}
		if g, err := s.st.Group(sub[i+1:], sub[:i]); err == nil {
			s.gw.serve(w, r, g, "")
			return true
		}
	}
	problem(w, http.StatusNotFound, "not found", "no container group for host "+host)
	return true
}

// groupForDomain finds the group that lists host in networking.custom_domains (cached 5 s).
func (s *Server) groupForDomain(host string) *store.GroupRow {
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}
	s.gw.mu.Lock()
	if time.Since(s.gw.domainsAt) > 5*time.Second {
		s.gw.mu.Unlock()
		m := map[string]string{}
		if groups, err := s.st.Groups(""); err == nil {
			for _, g := range groups {
				if g.Spec.Networking != nil {
					for _, d := range g.Spec.Networking.CustomDomains {
						m[d] = g.ID
					}
				}
			}
		}
		s.gw.mu.Lock()
		s.gw.domains, s.gw.domainsAt = m, time.Now()
	}
	id := s.gw.domains[host]
	s.gw.mu.Unlock()
	if id == "" {
		return nil
	}
	g, err := s.st.GroupByID(id)
	if err != nil {
		return nil
	}
	return g
}
