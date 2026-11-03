// Package server implements the ogm control plane: REST API, tunnel hub, scheduler,
// job queues, autoscaler, gateway, credits and dashboard.
package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// Config configures a server.
type Config struct {
	DataDir           string        // where ogm.db and files live
	Listen            string        // e.g. ":7777"
	PublicURL         string        // externally reachable base URL, e.g. https://gpu.example.com
	GatewayDomain     string        // optional wildcard domain for {group}-{project}.domain routing
	ClusterName       string        // shown in the dashboard
	StarterCredits    float64       // credits granted to new users
	DistDir           string        // optional dir with ogm-{os}-{arch} binaries for /dl
	OfflineGrace      time.Duration // how long a disconnected node keeps its instances
	ReconcileInterval time.Duration
	LogRetention      time.Duration
	EventRetention    time.Duration
	Logger            *slog.Logger
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":7777"
	}
	if c.ClusterName == "" {
		c.ClusterName = "open-gpu-mesh"
	}
	if c.StarterCredits == 0 {
		c.StarterCredits = 10
	}
	if c.OfflineGrace == 0 {
		c.OfflineGrace = 20 * time.Second
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = time.Second
	}
	if c.LogRetention == 0 {
		c.LogRetention = 7 * 24 * time.Hour
	}
	if c.EventRetention == 0 {
		c.EventRetention = 30 * 24 * time.Hour
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.PublicURL == "" {
		host := c.Listen
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		c.PublicURL = "http://" + host
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
}

// Server is the control plane.
type Server struct {
	cfg    Config
	st     *store.Store
	log    *slog.Logger
	mu     sync.Mutex // serialises scheduling decisions (lock order: mu -> jobMu)
	jobMu  sync.Mutex // guards job state transitions
	hub    *Hub
	regDir string // built-in image registry storage
	gw     *Gateway
	logs   *logBroker

	jwtPriv ed25519.PrivateKey
	jwtPub  ed25519.PublicKey
	jwtKid  string

	poke     chan struct{}
	pokeJobs chan struct{}
	http     *http.Client

	ownsStore bool
}

// New opens the data dir and builds a server.
func New(cfg Config) (*Server, error) {
	cfg.defaults()
	var st *store.Store
	var err error
	if cfg.DataDir == "" {
		st, err = store.Open(":memory:")
	} else {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, "files"), 0o755); err != nil {
			return nil, err
		}
		st, err = store.Open(filepath.Join(cfg.DataDir, "ogm.db"))
	}
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:       cfg,
		st:        st,
		log:       cfg.Logger,
		poke:      make(chan struct{}, 1),
		pokeJobs:  make(chan struct{}, 1),
		http:      &http.Client{Timeout: 15 * time.Second},
		logs:      newLogBroker(),
		ownsStore: true,
	}
	s.hub = newHub(s)
	s.gw = newGateway(s)
	if err := s.initKeys(); err != nil {
		return nil, err
	}
	if _, err := st.DB.Exec(registrySchema); err != nil {
		return nil, fmt.Errorf("registry schema: %w", err)
	}
	if cfg.DataDir != "" {
		s.regDir = filepath.Join(cfg.DataDir, "registry")
	} else if s.regDir, err = os.MkdirTemp("", "ogm-registry-"); err != nil {
		return nil, err
	}
	return s, nil
}

// Store exposes the underlying store (for tests and bootstrap).
func (s *Server) Store() *store.Store { return s.st }

// Config returns the effective config.
func (s *Server) Config() Config { return s.cfg }

func (s *Server) initKeys() error {
	seedB64, err := s.st.SettingOr("jwt_seed", func() string {
		seed := make([]byte, ed25519.SeedSize)
		_, _ = rand.Read(seed)
		return base64.StdEncoding.EncodeToString(seed)
	})
	if err != nil {
		return err
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("corrupt jwt_seed setting")
	}
	s.jwtPriv = ed25519.NewKeyFromSeed(seed)
	s.jwtPub = s.jwtPriv.Public().(ed25519.PublicKey)
	s.jwtKid = store.Hash(string(s.jwtPub))[:16]
	if _, err := s.st.SettingOr("webhook_secret", func() string {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return base64.StdEncoding.EncodeToString(b)
	}); err != nil {
		return err
	}
	_, err = s.st.SettingOr("files_key", func() string { return store.NewSecret(32) })
	return err
}

// Bootstrap ensures an admin user and default project exist. Returns the admin API key
// if one was created now (empty if the cluster was already initialised).
func (s *Server) Bootstrap(adminName string) (string, error) {
	users, err := s.st.Users()
	if err != nil {
		return "", err
	}
	if len(users) > 0 {
		return "", nil
	}
	if adminName == "" {
		adminName = "admin"
	}
	u, key, err := s.st.CreateUser(adminName, "admin", s.cfg.StarterCredits)
	if err != nil {
		return "", err
	}
	if err := s.createProject("default", u.ID); err != nil {
		return "", err
	}
	return key, nil
}

// Poke asks the reconciler to run soon.
func (s *Server) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// PokeJobs asks the dispatcher to run soon.
func (s *Server) PokeJobs() {
	select {
	case s.pokeJobs <- struct{}{}:
	default:
	}
}

// Run starts background loops and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) {
	go s.loop(ctx, s.cfg.ReconcileInterval, s.poke, s.reconcile)
	go s.loop(ctx, 500*time.Millisecond, s.pokeJobs, s.dispatchJobs)
	go s.loop(ctx, 5*time.Second, nil, s.autoscale)
	go s.loop(ctx, 60*time.Second, nil, s.accrueCredits)
	go s.loop(ctx, 2*time.Second, nil, s.deliverWebhooks)
	go s.loop(ctx, 10*time.Minute, nil, s.gc)
	<-ctx.Done()
}

func (s *Server) loop(ctx context.Context, every time.Duration, poke chan struct{}, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("background loop panic", "panic", r)
				}
			}()
			fn()
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-poke:
		}
	}
}

// ListenAndServe runs the HTTP server and background loops until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go s.Run(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.hub.closeAll()
		_ = srv.Shutdown(sctx)
	}()
	s.log.Info("ogm server listening", "addr", ln.Addr().String(), "public_url", s.cfg.PublicURL)
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close releases resources.
func (s *Server) Close() error {
	s.hub.closeAll()
	if s.ownsStore {
		return s.st.Close()
	}
	return nil
}

func (s *Server) gatewayURL(project, group string) string {
	if s.cfg.GatewayDomain != "" {
		scheme := "https"
		if strings.HasPrefix(s.cfg.PublicURL, "http://") {
			scheme = "http"
		}
		return fmt.Sprintf("%s://%s-%s.%s/", scheme, group, project, s.cfg.GatewayDomain)
	}
	return fmt.Sprintf("%s/gw/%s/%s/", s.cfg.PublicURL, project, group)
}

func (s *Server) gc() {
	now := time.Now()
	_, _ = s.st.DB.Exec(`DELETE FROM logs WHERE time < ?`, now.Add(-s.cfg.LogRetention).UnixMilli())
	_, _ = s.st.DB.Exec(`DELETE FROM events WHERE time < ?`, now.Add(-s.cfg.EventRetention).UnixMilli())
	_, _ = s.st.DB.Exec(`DELETE FROM instances WHERE state='terminated' AND updated < ?`, now.Add(-24*time.Hour).UnixMilli())
	_, _ = s.st.DB.Exec(`DELETE FROM node_bans WHERE until < ?`, now.UnixMilli())
	_, _ = s.st.DB.Exec(`DELETE FROM webhook_deliveries WHERE done=1 AND created < ?`, now.Add(-72*time.Hour).UnixMilli())
	// Per-queue job retention for terminal jobs.
	qs, _ := s.st.Queues("")
	for _, q := range qs {
		h := q.Settings.RetentionHours
		if h <= 0 {
			h = 24 * 7
		}
		_, _ = s.st.DB.Exec(`DELETE FROM jobs WHERE queue_id=? AND status IN ('succeeded','failed','cancelled') AND updated < ?`,
			q.ID, now.Add(-time.Duration(h)*time.Hour).UnixMilli())
	}
}
