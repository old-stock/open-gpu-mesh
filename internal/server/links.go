package server

import (
	"context"
	"math/rand"
	"net"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/tunnel"
)

// linkWait is how long a link connection waits for a ready instance of the target group
// (covers start-up ordering and scale-from-zero).
var linkWait = 90 * time.Second

// acceptAgentStreams handles agent-initiated streams: container-to-container links.
func (s *Server) acceptAgentStreams(ns *nodeSession) {
	for {
		st, err := ns.sess.AcceptStream()
		if err != nil {
			return
		}
		go s.handleLink(ns, st)
	}
}

// handleLink relays a link stream from an instance on ns to a ready instance of the target
// group in the same project. Only links declared in the caller's env ({{link:...}}) are allowed.
func (s *Server) handleLink(ns *nodeSession, st net.Conn) {
	fail := func() {
		_, _ = st.Write([]byte{proto.AckFail})
		st.Close()
	}
	h, err := tunnel.ReadHeader(st)
	if err != nil || h.Kind != "link" {
		st.Close()
		return
	}
	caller, err := s.st.Instance(h.InstanceID)
	if err != nil || caller.NodeID != ns.nodeID || caller.State == api.InstTerminated {
		fail()
		return
	}
	cg, err := s.st.GroupByID(caller.GroupID)
	if err != nil || !declaresLink(cg.Spec, h.Group, h.Port) {
		fail()
		return
	}
	tg, err := s.st.Group(caller.Project, h.Group)
	if err != nil || !exposesPort(tg.Spec, h.Port) {
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), linkWait)
	defer cancel()
	woke := false
	for {
		targets, _ := s.st.InstancesWhere(`group_id=? AND state='running' AND ready=1`, tg.ID)
		rand.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
		for _, t := range targets {
			dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
			up, err := s.hub.dial(dctx, t.NodeID, t.ID, h.Port)
			dcancel()
			if err != nil {
				continue
			}
			if _, err := st.Write([]byte{proto.AckOK}); err != nil {
				up.Close()
				st.Close()
				return
			}
			tunnel.Splice(st, up)
			return
		}
		if len(targets) > 0 {
			fail() // ready instances refused the port: waiting will not help
			return
		}
		if !woke {
			woke = true
			if tg.Spec.HTTPAutoscaler != nil {
				s.scaleFromZero(tg.ID)
			}
		}
		select {
		case <-ctx.Done():
			fail()
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func declaresLink(sp api.ContainerGroupSpec, group string, port int) bool {
	for _, v := range sp.Container.Env {
		for _, r := range api.ParseRefs(v) {
			if r.Kind != "secret" && r.Name == group && r.Port == port {
				return true
			}
		}
	}
	return false
}

// exposesPort reports whether instances of a group publish port (container.ports, the
// networking port or the queue port).
func exposesPort(sp api.ContainerGroupSpec, port int) bool {
	for _, p := range sp.Container.Ports {
		if p == port {
			return true
		}
	}
	return (sp.Networking != nil && sp.Networking.Port == port) || (sp.QueueConnection != nil && sp.QueueConnection.Port == port)
}
