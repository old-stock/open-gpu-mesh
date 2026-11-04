package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
	"github.com/old-stock/open-gpu-mesh/internal/tunnel"
)

// Hub tracks connected agents.
type Hub struct {
	s     *Server
	mu    sync.RWMutex
	nodes map[string]*nodeSession
}

type nodeSession struct {
	nodeID      string
	name        string
	sess        *yamux.Session
	codec       *proto.Codec
	connectedAt time.Time
	hello       proto.Hello

	mu   sync.Mutex
	jobs map[string]int // job id -> attempt, in flight on this session
}

func newHub(s *Server) *Hub { return &Hub{s: s, nodes: map[string]*nodeSession{}} }

func (h *Hub) get(nodeID string) *nodeSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nodes[nodeID]
}

func (h *Hub) online() map[string]*nodeSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]*nodeSession, len(h.nodes))
	for k, v := range h.nodes {
		out[k] = v
	}
	return out
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.nodes {
		n.sess.Close()
	}
}

// send delivers a control message to a node, returning false if it is not connected.
func (h *Hub) send(nodeID, typ string, data any) bool {
	ns := h.get(nodeID)
	if ns == nil {
		return false
	}
	if err := ns.codec.Send(typ, "", data); err != nil {
		h.s.log.Warn("send to node failed", "node", nodeID, "type", typ, "err", err)
		ns.sess.Close()
		return false
	}
	return true
}

// dial opens a proxy stream to an instance port on a node.
func (h *Hub) dial(ctx context.Context, nodeID, instanceID string, port int) (net.Conn, error) {
	ns := h.get(nodeID)
	if ns == nil {
		return nil, &tunnel.DialError{Err: errors.New("node offline")}
	}
	return tunnel.OpenProxy(ctx, ns.sess, instanceID, port)
}

// handleConnect is GET /v1/agent/connect (WebSocket).
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	node, err := s.st.NodeByToken(tok)
	if err != nil || tok == "" {
		http.Error(w, "invalid node token", http.StatusUnauthorized)
		return
	}
	sess, err := tunnel.Accept(w, r)
	if err != nil {
		s.log.Warn("tunnel accept failed", "node", node.ID, "err", err)
		return
	}
	defer sess.Close()
	ctl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	codec := proto.NewCodec(ctl)
	_ = ctl.SetReadDeadline(time.Now().Add(30 * time.Second))
	env, err := codec.Recv()
	if err != nil || env.Type != proto.TypeHello {
		s.log.Warn("agent did not say hello", "node", node.ID, "err", err)
		return
	}
	_ = ctl.SetReadDeadline(time.Time{})
	var hello proto.Hello
	if err := json.Unmarshal(env.Data, &hello); err != nil {
		return
	}
	ns := &nodeSession{nodeID: node.ID, name: node.Name, sess: sess, codec: codec, connectedAt: time.Now(), hello: hello, jobs: map[string]int{}}

	s.hub.mu.Lock()
	if old := s.hub.nodes[node.ID]; old != nil {
		old.sess.Close()
	}
	s.hub.nodes[node.ID] = ns
	s.hub.mu.Unlock()

	s.onHello(node, ns, hello)
	go s.acceptAgentStreams(ns)
	_ = codec.Send(proto.TypeWelcome, "", proto.Welcome{NodeID: node.ID, NodeName: node.Name, ServerTime: time.Now()})
	s.log.Info("node connected", "node", node.Name, "id", node.ID, "gpus", len(hello.GPUs), "runtime", hello.Runtime)
	s.Poke()
	s.PokeJobs()

	for {
		env, err := codec.Recv()
		if err != nil {
			break
		}
		s.handleAgentMessage(ns, env)
	}

	s.hub.mu.Lock()
	current := s.hub.nodes[node.ID] == ns
	if current {
		delete(s.hub.nodes, node.ID)
	}
	s.hub.mu.Unlock()
	if current {
		s.onDisconnect(ns)
	}
}

func (s *Server) nodeInfoFromHello(h proto.Hello) store.NodeInfo {
	for i := range h.GPUs {
		cls, _ := s.st.EnsureGPUClass(h.GPUs[i].Name, h.GPUs[i].VRAMMB)
		h.GPUs[i].Class = cls
	}
	return store.NodeInfo{
		Runtime: h.Runtime, Version: h.Version, OS: h.OS, Arch: h.Arch, CPU: h.CPU, MemoryMB: h.MemoryMB, DiskGB: h.DiskGB,
		GPUs: h.GPUs, Country: strings.ToLower(h.Country), Labels: h.Labels, AllowPriorities: h.AllowPriorities, Window: h.Window,
		InWindow: true, CachedImages: h.CachedImages,
	}
}

func (s *Server) onHello(node *store.NodeRow, ns *nodeSession, h proto.Hello) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.nodeInfoFromHello(h)
	_ = s.st.UpdateNodeInfo(node.ID, info, h.Paused || node.AdminPaused, pauseReason(h.PauseReason, node.AdminPaused))
	_ = s.st.SetNodeStatus(node.ID, "online")
	if node.Status != "online" {
		s.st.AddEvent("", "", "", node.ID, "node.online", "Node "+node.Name+" connected")
	}
	if node.AdminPaused && !h.Paused {
		paused := true
		_ = ns.codec.Send(proto.TypeNodeConfig, "", proto.NodeConfig{Paused: &paused})
	}

	// Reconcile what the agent runs against what we think it runs.
	reported := map[string]proto.InstanceReport{}
	for _, ir := range h.Instances {
		reported[ir.ID] = ir
	}
	dbInst, _ := s.st.InstancesWhere(`node_id=? AND state!='terminated'`, node.ID)
	known := map[string]bool{}
	for _, in := range dbInst {
		known[in.ID] = true
		if ir, ok := reported[in.ID]; ok {
			s.applyReport(node.ID, in, ir)
			continue
		}
		if in.State == api.InstStopping {
			s.terminate(in, in.StopReason, nil)
			continue
		}
		// The agent lost this instance (restart, crash). Re-send the start so it comes back
		// on the same node if the node still has capacity, otherwise reschedule.
		g, err := s.st.GroupByID(in.GroupID)
		var spec *proto.InstanceSpec
		if err == nil {
			spec, err = s.instanceSpec(g, in)
		}
		if err == nil && (g.Status == api.GroupRunning || g.Status == api.GroupDeploying) {
			in.State = api.InstDownloading
			in.Ready, in.Started = false, false
			_ = s.st.SaveInstance(in)
			_ = ns.codec.Send(proto.TypeInstanceStart, "", proto.InstanceRef{ID: in.ID, Spec: spec})
		} else {
			s.terminate(in, "node restarted", nil)
		}
	}
	for id := range reported {
		if !known[id] {
			_ = ns.codec.Send(proto.TypeInstanceStop, "", proto.InstanceStop{ID: id, GraceS: 5, Reason: "unknown to server"})
		}
	}
}

func pauseReason(agentReason string, admin bool) string {
	if admin {
		return "paused by admin"
	}
	return agentReason
}

func (s *Server) onDisconnect(ns *nodeSession) {
	s.log.Info("node disconnected", "node", ns.name, "id", ns.nodeID)
	_ = s.st.SetNodeStatus(ns.nodeID, "offline")
	s.st.AddEvent("", "", "", ns.nodeID, "node.offline", "Node "+ns.name+" disconnected")
	// Requeue in-flight jobs immediately: their results can never arrive on this session.
	ns.mu.Lock()
	jobs := ns.jobs
	ns.jobs = map[string]int{}
	ns.mu.Unlock()
	for id, attempt := range jobs {
		s.jobAttemptFailed(id, attempt, "", "node disconnected while running job", true)
	}
	s.gw.invalidate()
	s.Poke()
	s.PokeJobs()
}

func (s *Server) handleAgentMessage(ns *nodeSession, env proto.Envelope) {
	switch env.Type {
	case proto.TypeHeartbeat:
		var hb proto.Heartbeat
		if json.Unmarshal(env.Data, &hb) == nil {
			s.onHeartbeat(ns, hb)
		}
	case proto.TypeVolumeOpDone:
		var d proto.VolumeOpDone
		if json.Unmarshal(env.Data, &d) == nil {
			s.onVolumeOpDone(ns, d)
		}
	case proto.TypeInstanceStatus:
		var ir proto.InstanceReport
		if json.Unmarshal(env.Data, &ir) == nil {
			s.mu.Lock()
			if in, err := s.st.Instance(ir.ID); err == nil && in.NodeID == ns.nodeID {
				s.applyReport(ns.nodeID, in, ir)
			}
			s.mu.Unlock()
			s.gw.invalidate()
			s.Poke()
			s.PokeJobs()
		}
	case proto.TypeJobResult:
		var jr proto.JobResult
		if json.Unmarshal(env.Data, &jr) == nil {
			ns.mu.Lock()
			delete(ns.jobs, jr.JobID)
			ns.mu.Unlock()
			s.onJobResult(jr)
		}
	case proto.TypeLogs:
		var lb proto.LogBatch
		if json.Unmarshal(env.Data, &lb) == nil {
			s.ingestLogs(ns.nodeID, lb.Entries)
		}
	case proto.TypeIMDS:
		var req proto.IMDSRequest
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		go func() {
			reply, err := s.handleIMDS(ns.nodeID, req)
			out := proto.Envelope{Type: proto.TypeReply, ID: env.ID}
			if err != nil {
				out.Error = err.Error()
			} else {
				out.Data, _ = json.Marshal(reply)
			}
			_ = ns.codec.SendEnvelope(out)
		}()
	}
}

func (s *Server) onHeartbeat(ns *nodeSession, hb proto.Heartbeat) {
	node, err := s.st.NodeByID(ns.nodeID)
	if err != nil {
		return
	}
	info := node.Info
	for i := range hb.GPUs {
		cls, _ := s.st.EnsureGPUClass(hb.GPUs[i].Name, hb.GPUs[i].VRAMMB)
		hb.GPUs[i].Class = cls
	}
	if len(hb.GPUs) > 0 {
		info.GPUs = hb.GPUs
	}
	info.InWindow = hb.InWindow
	if hb.CachedImages != nil {
		info.CachedImages = hb.CachedImages
	}
	paused := hb.Paused || node.AdminPaused
	_ = s.st.UpdateNodeInfo(ns.nodeID, info, paused, pauseReason(hb.PauseReason, node.AdminPaused))
	// Uptime accounting (for reliability scoring).
	_, _ = s.st.DB.Exec(`UPDATE nodes SET online_seconds=online_seconds+5, tracked_seconds=tracked_seconds+5 WHERE id=?`, ns.nodeID)
	if paused != node.Paused {
		kind := "node.resumed"
		msg := "Node " + node.Name + " resumed"
		if paused {
			kind, msg = "node.paused", "Node "+node.Name+" paused: "+pauseReason(hb.PauseReason, node.AdminPaused)
		}
		s.st.AddEvent("", "", "", ns.nodeID, kind, msg)
		s.Poke()
	}
	s.mu.Lock()
	for _, ir := range hb.Instances {
		if in, err := s.st.Instance(ir.ID); err == nil && in.NodeID == ns.nodeID {
			s.applyReport(ns.nodeID, in, ir)
		}
	}
	s.mu.Unlock()
}
