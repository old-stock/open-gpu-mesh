package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
	"github.com/old-stock/open-gpu-mesh/internal/webhook"
)

// logBroker fans out live log lines to followers (SSE).
type logBroker struct {
	mu   sync.Mutex
	subs map[string]map[chan api.LogEntry]struct{} // group id -> subscribers
}

func newLogBroker() *logBroker { return &logBroker{subs: map[string]map[chan api.LogEntry]struct{}{}} }

func (b *logBroker) subscribe(groupID string) chan api.LogEntry {
	ch := make(chan api.LogEntry, 1024)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[groupID] == nil {
		b.subs[groupID] = map[chan api.LogEntry]struct{}{}
	}
	b.subs[groupID][ch] = struct{}{}
	return ch
}

func (b *logBroker) unsubscribe(groupID string, ch chan api.LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs[groupID], ch)
}

func (b *logBroker) publish(e api.LogEntry, groupID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[groupID] {
		select {
		case ch <- e:
		default: // slow consumer: drop
		}
	}
}

func (s *Server) ingestLogs(nodeID string, lines []proto.LogLine) {
	type meta struct{ project, group, groupName string }
	cache := map[string]*meta{}
	lookup := func(id string) *meta {
		if m, ok := cache[id]; ok {
			return m
		}
		in, err := s.st.Instance(id)
		if err != nil || in.NodeID != nodeID {
			cache[id] = nil
			return nil
		}
		g, _ := s.st.GroupByID(in.GroupID)
		m := &meta{project: in.Project, group: in.GroupID}
		if g != nil {
			m.groupName = g.Name
		}
		cache[id] = m
		return m
	}
	// Resolve metadata before InsertLogs opens its transaction (single DB connection).
	for _, l := range lines {
		lookup(l.InstanceID)
	}
	_ = s.st.InsertLogs(nodeID, lines, func(id string) (string, string, bool) {
		m := lookup(id)
		if m == nil {
			return "", "", false
		}
		return m.project, m.group, true
	})
	for _, l := range lines {
		if m := lookup(l.InstanceID); m != nil {
			s.logs.publish(api.LogEntry{Project: m.project, Group: m.groupName, InstanceID: l.InstanceID, NodeID: nodeID, Stream: l.Stream, Line: strings.TrimRight(l.Line, "\r\n"), Time: l.Time}, m.group)
		}
	}
}

// handleLogs: GET /v1/projects/{project}/containers/{group}/logs?since=&instance=&q=&limit=&follow=1
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, u *store.UserRow, project string) {
	g, err := s.st.Group(project, r.PathValue("group"))
	if err != nil {
		storeErr(w, err, "container group")
		return
	}
	q := r.URL.Query()
	where := []string{"group_id=?"}
	args := []any{g.ID}
	if inst := q.Get("instance"); inst != "" {
		where = append(where, "instance_id=?")
		args = append(args, inst)
	}
	if since := q.Get("since"); since != "" {
		if d, err := time.ParseDuration(since); err == nil {
			where = append(where, "time>=?")
			args = append(args, time.Now().Add(-d).UnixMilli())
		} else if t, err := time.Parse(time.RFC3339, since); err == nil {
			where = append(where, "time>=?")
			args = append(args, t.UnixMilli())
		}
	}
	if text := q.Get("q"); text != "" {
		where = append(where, "line LIKE ?")
		args = append(args, "%"+text+"%")
	}
	limit := qint(r, "limit", 500)
	if limit > 10000 {
		limit = 10000
	}
	rows, err := s.st.DB.Query(`SELECT id,instance_id,node_id,stream,line,time FROM (SELECT * FROM logs WHERE `+strings.Join(where, " AND ")+
		` ORDER BY id DESC LIMIT `+fmt.Sprint(limit)+`) ORDER BY id`, args...)
	if err != nil {
		storeErr(w, err, "logs")
		return
	}
	var out []api.LogEntry
	for rows.Next() {
		var e api.LogEntry
		var t int64
		if rows.Scan(&e.ID, &e.InstanceID, &e.NodeID, &e.Stream, &e.Line, &t) == nil {
			e.Time = store.T(t)
			e.Project, e.Group = project, g.Name
			out = append(out, e)
		}
	}
	rows.Close()
	if q.Get("follow") != "1" {
		if out == nil {
			out = []api.LogEntry{}
		}
		writeJSON(w, http.StatusOK, api.List[api.LogEntry]{Items: out})
		return
	}
	// Server-sent events: backlog, then live.
	fl, ok := w.(http.Flusher)
	if !ok {
		problem(w, http.StatusInternalServerError, "streaming unsupported", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.logs.subscribe(g.ID)
	defer s.logs.unsubscribe(g.ID, ch)
	for _, e := range out {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fl.Flush()
	inst, text := q.Get("instance"), q.Get("q")
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case e := <-ch:
			if (inst != "" && e.InstanceID != inst) || (text != "" && !strings.Contains(e.Line, text)) {
				continue
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// ---------- IMDS ----------

func (s *Server) handleIMDS(nodeID string, req proto.IMDSRequest) (*proto.IMDSReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.st.Instance(req.InstanceID)
	if err != nil || in.NodeID != nodeID || in.State == api.InstTerminated {
		return nil, errors.New("instance not found")
	}
	g, err := s.st.GroupByID(in.GroupID)
	if err != nil {
		return nil, errors.New("container group not found")
	}
	switch req.Op {
	case proto.IMDSToken:
		now := time.Now()
		jwt, err := webhook.SignJWT(s.jwtPriv, s.jwtKid, map[string]any{
			"iss": s.cfg.PublicURL, "sub": in.ID, "aud": "ogm-workload",
			"project": in.Project, "container_group": g.Name, "container_group_id": g.ID, "node_id": in.NodeID,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
		if err != nil {
			return nil, err
		}
		return &proto.IMDSReply{JWT: jwt}, nil
	case proto.IMDSSetDeletionCost:
		in.DeletionCost = req.DeletionCost
		_ = s.st.SaveInstance(in)
		return &proto.IMDSReply{}, nil
	case proto.IMDSReallocate:
		s.banNode(in.NodeID, in.GroupID)
		msg := "Instance Reallocated by Workload"
		if req.Reason != "" {
			msg += ": " + req.Reason
		}
		s.st.AddEvent(in.Project, in.GroupID, in.ID, in.NodeID, "instance.reallocated", msg)
		s.requestStop(in, "reallocated", "", 5)
		s.Poke()
		return &proto.IMDSReply{}, nil
	case proto.IMDSRestart, proto.IMDSRecreate:
		s.st.AddEvent(in.Project, in.GroupID, in.ID, in.NodeID, "instance."+req.Op, "Instance "+strings.Title(req.Op)+"ed by Workload")
		return &proto.IMDSReply{}, nil // the agent performs restart/recreate locally
	case proto.IMDSMetadata:
		n, _ := s.st.NodeByID(nodeID)
		md := map[string]any{
			"instance_id": in.ID, "container_group": g.Name, "container_group_id": g.ID, "project": in.Project,
			"version": in.Version, "priority": g.Spec.Priority, "deletion_cost": in.DeletionCost, "server_url": s.cfg.PublicURL,
		}
		if n != nil {
			md["node_id"], md["node_name"], md["country"] = n.ID, n.Name, n.Info.Country
			var gpus []api.GPU
			for _, gp := range n.Info.GPUs {
				for _, idx := range in.GPUIndexes {
					if gp.Index == idx {
						gpus = append(gpus, gp)
					}
				}
			}
			md["gpus"] = gpus
		}
		return &proto.IMDSReply{Metadata: md}, nil
	}
	return nil, fmt.Errorf("unknown imds op %q", req.Op)
}
