// Package agent implements the ogm node agent that runs on contributor machines.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/old-stock/open-gpu-mesh/internal/regproxy"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/tunnel"
)

// Config configures an agent.
type Config struct {
	Server          string
	Token           string
	Name            string
	Runtime         Runtime
	GPUs            GPUSource
	MaxGPUs         int // 0 = all
	MaxCPU          int
	MaxMemoryMB     int
	MaxDiskGB       int
	Country         string
	Labels          map[string]string
	RegistryPort    int // localhost port for the cluster-registry proxy (0 = 5077; -1 = disabled)
	AllowPriorities []string
	Window          string   // "22:00-08:00,12:00-13:00" local time; empty = always
	YieldGPUUtil    float64  // pause when an unassigned GPU is busier than this (%); 0 = off
	PauseOnProcess  []string // pause while any of these processes run (e.g. steam, valorant)
	IMDSListen      string   // default ":47474" (docker) or "127.0.0.1:0" (process)
	ControlSocket   string   // unix socket for `ogm agent pause|resume|status`
	HeartbeatEvery  time.Duration
	StopGrace       time.Duration // grace when pausing
	Logger          *slog.Logger
}

// Agent is a running node agent.
type Agent struct {
	cfg Config
	rt  Runtime
	log *slog.Logger
	reg *regproxy.Proxy

	reapOnce sync.Once
	detach   atomic.Bool

	mu        sync.Mutex
	codec     *proto.Codec
	sess      *yamux.Session
	nodeID    string
	instances map[string]*instance
	jobs      map[string]context.CancelFunc
	pending   map[string]chan proto.Envelope
	reqSeq    int

	pauseMu     sync.Mutex
	manualPause bool
	serverPause bool
	autoPause   string // reason for automatic pause (window / yield / process)
	yieldSince  time.Time
	quietSince  time.Time
	lastGPUs    []api.GPU

	imdsAddr string
	logs     *logShipper
	ctx      context.Context
}

// New builds an agent.
func New(cfg Config) (*Agent, error) {
	if cfg.Server == "" || cfg.Token == "" {
		return nil, errors.New("server and token are required")
	}
	if cfg.Runtime == nil {
		return nil, errors.New("runtime is required")
	}
	if cfg.GPUs == nil {
		cfg.GPUs = NvidiaSMI{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HeartbeatEvery == 0 {
		cfg.HeartbeatEvery = 5 * time.Second
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 30 * time.Second
	}
	if cfg.IMDSListen == "" {
		if cfg.Runtime.Name() == "docker" {
			cfg.IMDSListen = ":47474"
		} else {
			cfg.IMDSListen = "127.0.0.1:0"
		}
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	a := &Agent{cfg: cfg, rt: cfg.Runtime, log: cfg.Logger, instances: map[string]*instance{}, jobs: map[string]context.CancelFunc{}, pending: map[string]chan proto.Envelope{}}
	a.logs = newLogShipper(a)
	return a, nil
}

// Detach makes the next shutdown leave containers running, for an in-place upgrade: the
// next agent process re-attaches to them (see Adopter).
func (a *Agent) Detach() { a.detach.Store(true) }

// Detached reports whether Detach was called.
func (a *Agent) Detached() bool { return a.detach.Load() }

// Run connects to the server and serves until ctx is cancelled, reconnecting as needed.
func (a *Agent) Run(ctx context.Context) error {
	a.ctx = ctx
	if err := a.rt.Cleanup(ctx); err != nil {
		a.log.Warn("runtime cleanup failed (is the runtime available?)", "err", err)
	}
	if err := a.startIMDS(ctx); err != nil {
		return fmt.Errorf("imds: %w", err)
	}
	if a.cfg.RegistryPort >= 0 {
		port := a.cfg.RegistryPort
		if port == 0 {
			port = 5077
		}
		if p, err := regproxy.Start(a.cfg.Server, a.cfg.Token, port); err != nil {
			a.log.Warn("registry proxy unavailable; registry.ogm/ images will fail", "err", err)
		} else {
			a.reg = p
			defer p.Close()
			a.log.Info("cluster registry proxy", "addr", p.Addr)
		}
	}
	if a.cfg.ControlSocket != "" {
		go a.serveControl(ctx)
	}
	go a.logs.run(ctx)
	go a.monitor(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("disconnected from server; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	// Shutdown: stop all instances quickly.
	a.mu.Lock()
	insts := make([]*instance, 0, len(a.instances))
	for _, in := range a.instances {
		insts = append(insts, in)
	}
	a.mu.Unlock()
	for _, in := range insts {
		<-in.done
	}
	return nil
}

func (a *Agent) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sess, err := tunnel.Dial(dctx, a.cfg.Server, a.cfg.Token)
	cancel()
	if err != nil {
		return err
	}
	defer sess.Close()
	ctl, err := sess.OpenStream()
	if err != nil {
		return err
	}
	codec := proto.NewCodec(ctl)
	if err := codec.Send(proto.TypeHello, "", a.hello(ctx)); err != nil {
		return err
	}
	a.mu.Lock()
	a.codec, a.sess = codec, sess
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.codec == codec {
			a.codec, a.sess = nil, nil
		}
		for id, ch := range a.pending {
			close(ch)
			delete(a.pending, id)
		}
		a.mu.Unlock()
	}()

	go a.acceptStreams(sess)
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		t := time.NewTicker(a.cfg.HeartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				a.send(proto.TypeHeartbeat, a.heartbeat())
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sess.Close()
	}()
	for {
		env, err := codec.Recv()
		if err != nil {
			return err
		}
		a.handle(env)
	}
}

func (a *Agent) hello(ctx context.Context) proto.Hello {
	h := proto.Hello{
		Name: a.cfg.Name, Version: api.Version, Runtime: a.rt.Name(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPU: cpuCount(), MemoryMB: memoryMB(), DiskGB: diskGB("/"), GPUs: a.gpus(ctx), Country: a.cfg.Country,
		Labels: a.cfg.Labels, AllowPriorities: a.cfg.AllowPriorities, Window: a.cfg.Window,
		CachedImages: a.rt.CachedImages(),
	}
	if a.cfg.MaxCPU > 0 && a.cfg.MaxCPU < h.CPU {
		h.CPU = a.cfg.MaxCPU
	}
	if a.cfg.MaxMemoryMB > 0 && a.cfg.MaxMemoryMB < h.MemoryMB {
		h.MemoryMB = a.cfg.MaxMemoryMB
	}
	if a.cfg.MaxDiskGB > 0 && a.cfg.MaxDiskGB < h.DiskGB {
		h.DiskGB = a.cfg.MaxDiskGB
	}
	h.Paused, h.PauseReason = a.paused()
	a.mu.Lock()
	for _, in := range a.instances {
		in.mu.Lock()
		h.Instances = append(h.Instances, in.reportLocked())
		in.mu.Unlock()
	}
	a.mu.Unlock()
	return h
}

func (a *Agent) gpus(ctx context.Context) []api.GPU {
	g := a.cfg.GPUs.GPUs(ctx)
	sort.Slice(g, func(i, j int) bool { return g[i].Index < g[j].Index })
	if a.cfg.MaxGPUs > 0 && len(g) > a.cfg.MaxGPUs {
		g = g[:a.cfg.MaxGPUs]
	}
	a.pauseMu.Lock()
	a.lastGPUs = g
	a.pauseMu.Unlock()
	return g
}

func (a *Agent) gpuNames(idx []int) string {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()
	var names []string
	for _, i := range idx {
		for _, g := range a.lastGPUs {
			if g.Index == i {
				names = append(names, g.Name)
			}
		}
	}
	return strings.Join(names, ",")
}

func (a *Agent) heartbeat() proto.Heartbeat {
	a.pauseMu.Lock()
	gpus := append([]api.GPU(nil), a.lastGPUs...)
	a.pauseMu.Unlock()
	hb := proto.Heartbeat{GPUs: gpus, InWindow: inWindow(a.cfg.Window, time.Now()), CachedImages: a.rt.CachedImages()}
	hb.Paused, hb.PauseReason = a.paused()
	a.mu.Lock()
	for _, in := range a.instances {
		in.mu.Lock()
		// Per-instance GPU metrics from the assigned GPUs.
		var util, mem, temp float64
		for _, idx := range in.spec.GPUIndexes {
			for _, g := range gpus {
				if g.Index == idx {
					util += g.UtilPct
					mem += float64(g.MemUsedMB)
					if g.TempC > temp {
						temp = g.TempC
					}
				}
			}
		}
		if n := len(in.spec.GPUIndexes); n > 0 {
			in.metrics.GPUUtil, in.metrics.GPUMemoryMB, in.metrics.GPUTempC = util/float64(n), mem, temp
		}
		hb.Instances = append(hb.Instances, in.reportLocked())
		in.mu.Unlock()
	}
	a.mu.Unlock()
	return hb
}

// send writes a control message if connected.
func (a *Agent) send(typ string, data any) {
	a.mu.Lock()
	c := a.codec
	a.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.Send(typ, "", data); err != nil {
		a.log.Debug("send failed", "type", typ, "err", err)
	}
}

// request sends a message and waits for the server's reply.
func (a *Agent) request(ctx context.Context, typ string, data any) (proto.Envelope, error) {
	a.mu.Lock()
	c := a.codec
	if c == nil {
		a.mu.Unlock()
		return proto.Envelope{}, errors.New("not connected to server")
	}
	a.reqSeq++
	id := strconv.Itoa(a.reqSeq)
	ch := make(chan proto.Envelope, 1)
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()
	if err := c.Send(typ, id, data); err != nil {
		return proto.Envelope{}, err
	}
	select {
	case env, ok := <-ch:
		if !ok {
			return env, errors.New("connection lost")
		}
		if env.Error != "" {
			return env, errors.New(env.Error)
		}
		return env, nil
	case <-ctx.Done():
		return proto.Envelope{}, ctx.Err()
	case <-time.After(15 * time.Second):
		return proto.Envelope{}, errors.New("server did not reply")
	}
}

func (a *Agent) handle(env proto.Envelope) {
	switch env.Type {
	case proto.TypeWelcome:
		var w proto.Welcome
		_ = json.Unmarshal(env.Data, &w)
		a.mu.Lock()
		a.nodeID = w.NodeID
		a.mu.Unlock()
		a.log.Info("connected to server", "node_id", w.NodeID, "name", w.NodeName)
		// Containers from a previous agent process that the server didn't re-claim within a
		// minute of reconnecting are leftovers: remove them.
		if ad, ok := a.rt.(Adopter); ok {
			a.reapOnce.Do(func() {
				go func() {
					select {
					case <-time.After(time.Minute):
						if n := ad.ReapOrphans(context.Background()); n > 0 {
							a.log.Info("removed unclaimed containers from previous run", "count", n)
						}
					case <-a.ctx.Done():
					}
				}()
			})
		}
	case proto.TypeReply:
		a.mu.Lock()
		ch := a.pending[env.ID]
		a.mu.Unlock()
		if ch != nil {
			ch <- env
		}
	case proto.TypeInstanceStart:
		var ref proto.InstanceRef
		if json.Unmarshal(env.Data, &ref) == nil && ref.Spec != nil {
			a.startInstance(*ref.Spec)
		}
	case proto.TypeInstanceStop:
		var st proto.InstanceStop
		if json.Unmarshal(env.Data, &st) == nil {
			if in := a.instance(st.ID); in != nil {
				in.stop(time.Duration(st.GraceS)*time.Second, st.Reason, "")
			} else {
				a.send(proto.TypeInstanceStatus, proto.InstanceReport{ID: st.ID, State: api.InstTerminated, Message: st.Reason})
			}
		}
	case proto.TypeInstanceRestart:
		var ref proto.InstanceRef
		if json.Unmarshal(env.Data, &ref) == nil {
			if in := a.instance(ref.ID); in != nil {
				select {
				case in.restartCh <- struct{}{}:
				default:
				}
			}
		}
	case proto.TypeInstanceRecreate:
		var ref proto.InstanceRef
		if json.Unmarshal(env.Data, &ref) == nil && ref.Spec != nil {
			if in := a.instance(ref.ID); in != nil {
				select {
				case in.recreateCh <- *ref.Spec:
				default:
				}
			} else {
				a.startInstance(*ref.Spec)
			}
		}
	case proto.TypeVolumeDelete:
		var vd proto.VolumeDelete
		if json.Unmarshal(env.Data, &vd) == nil {
			if vr, ok := a.rt.(VolumeRemover); ok {
				go func() {
					if err := vr.RemoveVolumes(a.ctx, vd.Names); err != nil {
						a.log.Warn("volume delete failed", "names", vd.Names, "err", err)
					} else {
						a.log.Info("volumes deleted", "names", vd.Names)
					}
				}()
			}
		}
	case proto.TypeVolumeSnapshot, proto.TypeVolumeRestore:
		var vo proto.VolumeOp
		if json.Unmarshal(env.Data, &vo) == nil {
			op := "snapshot"
			if env.Type == proto.TypeVolumeRestore {
				op = "restore"
			}
			go a.handleVolumeOp(op, vo)
		}
	case proto.TypeJobAssign:
		var ja proto.JobAssign
		if json.Unmarshal(env.Data, &ja) == nil {
			go a.runJob(ja)
		}
	case proto.TypeJobCancel:
		var jc proto.JobCancel
		if json.Unmarshal(env.Data, &jc) == nil {
			a.mu.Lock()
			cancel := a.jobs[jc.JobID]
			a.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	case proto.TypeNodeConfig:
		var nc proto.NodeConfig
		if json.Unmarshal(env.Data, &nc) == nil && nc.Paused != nil {
			a.pauseMu.Lock()
			a.serverPause = *nc.Paused
			a.pauseMu.Unlock()
			a.applyPause()
		}
	}
}

func (a *Agent) instance(id string) *instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.instances[id]
}

func (a *Agent) removeInstance(id string, in *instance) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.instances[id] == in {
		delete(a.instances, id)
	}
}

func (a *Agent) startInstance(spec proto.InstanceSpec) {
	if p, why := a.paused(); p {
		a.send(proto.TypeInstanceStatus, proto.InstanceReport{ID: spec.ID, State: api.InstTerminated, Message: "node paused: " + why})
		return
	}
	a.mu.Lock()
	if existing := a.instances[spec.ID]; existing != nil {
		a.mu.Unlock()
		select {
		case existing.recreateCh <- spec:
		default:
		}
		return
	}
	in := newInstance(a, spec)
	a.instances[spec.ID] = in
	a.mu.Unlock()
	a.log.Info("starting instance", "id", spec.ID, "group", spec.Project+"/"+spec.Group, "image", spec.Image, "gpus", spec.GPUIndexes)
	go in.run(a.ctx)
}

// acceptStreams handles server-initiated data streams (gateway proxying).
func (a *Agent) acceptStreams(sess *yamux.Session) {
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go a.handleStream(st)
	}
}

func (a *Agent) handleStream(st net.Conn) {
	h, err := tunnel.ReadHeader(st)
	if err != nil || h.Kind != "proxy" {
		st.Close()
		return
	}
	in := a.instance(h.InstanceID)
	var ctr Container
	if in != nil {
		ctr = in.container()
	}
	if ctr == nil {
		_, _ = st.Write([]byte{proto.AckFail})
		st.Close()
		return
	}
	addr := ctr.HostAddr(h.Port)
	if addr == "" {
		_, _ = st.Write([]byte{proto.AckFail})
		st.Close()
		return
	}
	up, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		_, _ = st.Write([]byte{proto.AckFail})
		st.Close()
		return
	}
	if _, err := st.Write([]byte{proto.AckOK}); err != nil {
		up.Close()
		st.Close()
		return
	}
	tunnel.Splice(st, up)
}

// ---------- pause / window / yield ----------

func (a *Agent) paused() (bool, string) {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()
	switch {
	case a.manualPause:
		return true, "paused by owner"
	case a.serverPause:
		return true, "paused by admin"
	case a.autoPause != "":
		return true, a.autoPause
	}
	return false, ""
}

// SetPaused pauses or resumes the node manually.
func (a *Agent) SetPaused(p bool) {
	a.pauseMu.Lock()
	a.manualPause = p
	a.pauseMu.Unlock()
	a.applyPause()
}

// applyPause stops all instances gracefully when paused and tells the server.
func (a *Agent) applyPause() {
	p, why := a.paused()
	a.send(proto.TypeHeartbeat, a.heartbeat())
	if !p {
		return
	}
	a.mu.Lock()
	insts := make([]*instance, 0, len(a.instances))
	for _, in := range a.instances {
		insts = append(insts, in)
	}
	a.mu.Unlock()
	for _, in := range insts {
		in.stop(a.cfg.StopGrace, "node paused: "+why, "Instance Interrupted (Node Paused: "+why+")")
	}
}

func (a *Agent) monitor(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		gpus := a.gpus(ctx)
		reason := ""
		if !inWindow(a.cfg.Window, time.Now()) {
			reason = "outside availability window " + a.cfg.Window
		}
		if reason == "" && len(a.cfg.PauseOnProcess) > 0 {
			procs := runningProcesses(ctx)
			for _, p := range a.cfg.PauseOnProcess {
				if procs[strings.ToLower(p)] {
					reason = "owner is using " + p
					break
				}
			}
		}
		if reason == "" && a.cfg.YieldGPUUtil > 0 {
			assigned := map[int]bool{}
			a.mu.Lock()
			for _, in := range a.instances {
				for _, idx := range in.spec.GPUIndexes {
					assigned[idx] = true
				}
			}
			a.mu.Unlock()
			busy := false
			for _, g := range gpus {
				if !assigned[g.Index] && g.UtilPct >= a.cfg.YieldGPUUtil {
					busy = true
				}
			}
			a.pauseMu.Lock()
			now := time.Now()
			if busy {
				if a.yieldSince.IsZero() {
					a.yieldSince = now
				}
				a.quietSince = time.Time{}
			} else {
				a.yieldSince = time.Time{}
				if a.quietSince.IsZero() {
					a.quietSince = now
				}
			}
			wasYield := strings.HasPrefix(a.autoPause, "owner GPU")
			switch {
			case busy && now.Sub(a.yieldSince) >= 20*time.Second:
				reason = "owner GPU activity detected"
			case wasYield && !a.quietSince.IsZero() && now.Sub(a.quietSince) < 5*time.Minute:
				reason = a.autoPause // stay yielded until 5 min of quiet
			}
			a.pauseMu.Unlock()
		}
		a.pauseMu.Lock()
		changed := a.autoPause != reason
		a.autoPause = reason
		a.pauseMu.Unlock()
		if changed {
			if reason != "" {
				a.log.Info("pausing node", "reason", reason)
			} else {
				a.log.Info("resuming node")
			}
			a.applyPause()
		}
	}
}

// inWindow reports whether t falls in a "HH:MM-HH:MM,..." window list (empty = always).
func inWindow(spec string, t time.Time) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true
	}
	mins := t.Hour()*60 + t.Minute()
	for _, part := range strings.Split(spec, ",") {
		se := strings.Split(strings.TrimSpace(part), "-")
		if len(se) != 2 {
			continue
		}
		s, ok1 := hhmm(se[0])
		e, ok2 := hhmm(se[1])
		if !ok1 || !ok2 {
			continue
		}
		if s <= e && mins >= s && mins < e {
			return true
		}
		if s > e && (mins >= s || mins < e) { // wraps midnight
			return true
		}
	}
	return false
}

func hhmm(s string) (int, bool) {
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(p[0])
	m, err2 := strconv.Atoi(p[1])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// ---------- local control socket ----------

func (a *Agent) serveControl(ctx context.Context) {
	_ = os.Remove(a.cfg.ControlSocket)
	ln, err := net.Listen("unix", a.cfg.ControlSocket)
	if err != nil {
		a.log.Warn("control socket unavailable", "err", err)
		return
	}
	_ = os.Chmod(a.cfg.ControlSocket, 0o660)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) { a.SetPaused(true); a.writeStatus(w) })
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) { a.SetPaused(false); a.writeStatus(w) })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { a.writeStatus(w) })
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	_ = srv.Serve(ln)
}

// Status is the agent's local status.
type Status struct {
	NodeID      string                 `json:"node_id"`
	Connected   bool                   `json:"connected"`
	Paused      bool                   `json:"paused"`
	PauseReason string                 `json:"pause_reason,omitempty"`
	Runtime     string                 `json:"runtime"`
	GPUs        []api.GPU              `json:"gpus"`
	Instances   []proto.InstanceReport `json:"instances"`
}

// Status returns the current local status.
func (a *Agent) Status() Status {
	hb := a.heartbeat()
	a.mu.Lock()
	st := Status{NodeID: a.nodeID, Connected: a.codec != nil, Runtime: a.rt.Name(), GPUs: hb.GPUs, Instances: hb.Instances}
	a.mu.Unlock()
	st.Paused, st.PauseReason = a.paused()
	return st
}

func (a *Agent) writeStatus(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.Status())
}
