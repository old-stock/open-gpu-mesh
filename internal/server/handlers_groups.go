package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

func (s *Server) groupAPI(g *store.GroupRow, withInstances bool) api.ContainerGroup {
	out := api.ContainerGroup{
		ContainerGroupSpec: g.Spec, ID: g.ID, Version: g.Version, DesiredReplicas: g.DesiredReplicas,
		CreateTime: store.T(g.Created), UpdateTime: store.T(g.Updated),
	}
	out.Project = g.Project
	if g.Spec.Networking != nil && g.Spec.Networking.Auth {
		out.GatewayToken = g.GatewayToken
	}
	if u, err := s.st.UserByID(g.OwnerID); err == nil {
		out.Owner = u.Name
	}
	// Never echo registry passwords.
	if out.Container.RegistryAuth != nil {
		ra := *out.Container.RegistryAuth
		ra.Password = "********"
		out.Container.RegistryAuth = &ra
	}
	if g.Spec.Networking != nil {
		out.GatewayURL = s.gatewayURL(g.Project, g.Name)
	}
	out.CurrentState.Status = g.Status
	out.CurrentState.StartTime = store.TP(g.StartTime)
	out.CurrentState.FinishTime = store.TP(g.FinishTime)
	insts, _ := s.st.LiveInstances(g.ID)
	c := &out.CurrentState.InstanceStatusCounts
	for _, in := range insts {
		switch in.State {
		case api.InstAllocating:
			c.Allocating++
		case api.InstDownloading:
			c.Downloading++
		case api.InstCreating:
			c.Creating++
		case api.InstRunning:
			c.Running++
			if in.Ready {
				c.Ready++
			}
		case api.InstStopping:
			c.Stopping++
		}
		if in.Version != g.Version {
			out.PendingChange = true
		}
		if in.State == api.InstAllocating && in.Message != "" && out.CurrentState.Description == "" {
			out.CurrentState.Description = in.Message
		}
		if withInstances {
			out.Instances = append(out.Instances, s.instanceAPI(in, g))
		}
	}
	if g.Spec.QueueConnection != nil {
		if q, err := s.st.Queue(g.Project, g.Spec.QueueConnection.QueueName); err == nil {
			st, _ := s.st.QueueStats(q.ID)
			out.QueueStats = &st
		}
	}
	return out
}

func (s *Server) instanceAPI(in *store.InstanceRow, g *store.GroupRow) api.Instance {
	out := api.Instance{
		ID: in.ID, Project: in.Project, NodeID: in.NodeID, GPUIndexes: in.GPUIndexes, State: in.State, Ready: in.Ready,
		Started: in.Started, DeletionCost: in.DeletionCost, Version: in.Version, ReplicaIndex: in.ReplicaIndex, Message: in.Message,
		PullingProgress: in.PullProgress, Metrics: in.Metrics, StopReason: in.StopReason,
		CreateTime: store.T(in.Created), UpdateTime: store.T(in.Updated), RunningTime: store.TP(in.RunningAt),
	}
	if g != nil {
		out.Group = g.Name
	}
	if in.ExitCode.Valid {
		c := int(in.ExitCode.Int64)
		out.ExitCode = &c
	}
	out.Metrics.InflightConns = s.gw.Inflight(in.ID)
	if in.NodeID != "" {
		if n, err := s.st.NodeByID(in.NodeID); err == nil {
			out.NodeName, out.Country = n.Name, n.Info.Country
			for _, gp := range n.Info.GPUs {
				if len(in.GPUIndexes) > 0 && gp.Index == in.GPUIndexes[0] {
					out.GPUName = gp.Name
				}
			}
		}
	}
	return out
}

// runtimeFingerprint captures fields whose change requires recreating containers.
func runtimeFingerprint(sp api.ContainerGroupSpec) string {
	ports := []int{}
	if sp.Networking != nil {
		ports = append(ports, sp.Networking.Port)
	}
	if sp.QueueConnection != nil {
		ports = append(ports, sp.QueueConnection.Port)
	}
	sort.Ints(ports)
	return store.JSON(map[string]any{
		"container": sp.Container, "startup": sp.StartupProbe, "readiness": sp.ReadinessProbe, "liveness": sp.LivenessProbe,
		"restart": sp.RestartPolicy, "ports": ports,
	})
}

// decodeSpec decodes a container group spec, defaulting replicas to 1 when the
// body leaves the field out.
func decodeSpec(w http.ResponseWriter, r *http.Request, sp *api.ContainerGroupSpec) bool {
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return false
	}
	if err := json.Unmarshal(raw, sp); err != nil {
		problem(w, http.StatusBadRequest, "invalid json", err.Error())
		return false
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	_, present := keys["replicas"]
	sp.DefaultReplicas(present)
	return true
}

func initialReplicas(sp api.ContainerGroupSpec) int {
	if sp.QueueAutoscaler != nil {
		r := sp.Replicas
		if r < sp.QueueAutoscaler.MinReplicas {
			r = sp.QueueAutoscaler.MinReplicas
		}
		if r > sp.QueueAutoscaler.MaxReplicas {
			r = sp.QueueAutoscaler.MaxReplicas
		}
		return r
	}
	return sp.Replicas
}

func (s *Server) checkSpecRefs(w http.ResponseWriter, sp *api.ContainerGroupSpec) bool {
	// The gateway routes {group}-{project}.<domain>; names may contain "-", so refuse a
	// group whose label is already served by a different (group, project) pair.
	label := sp.Name + "-" + sp.Project
	all, _ := s.st.Groups("")
	for _, g := range all {
		if g.Project != sp.Project && g.Name+"-"+g.Project == label {
			validationProblem(w, map[string][]string{"name": {"the gateway hostname " + label + " is already used by group " + g.Name + " in project " + g.Project + "; choose another name"}})
			return false
		}
	}
	if sp.Networking != nil && len(sp.Networking.CustomDomains) > 0 {
		for _, d := range sp.Networking.CustomDomains {
			if s.reservedHost(d) {
				validationProblem(w, map[string][]string{"networking.custom_domains": {d + " is reserved for the hub itself"}})
				return false
			}
		}
		groups, _ := s.st.Groups("")
		for _, g := range groups {
			if g.Project == sp.Project && g.Name == sp.Name || g.Spec.Networking == nil {
				continue
			}
			for _, d := range g.Spec.Networking.CustomDomains {
				for _, want := range sp.Networking.CustomDomains {
					if d == want {
						validationProblem(w, map[string][]string{"networking.custom_domains": {want + " is already used by another container group"}})
						return false
					}
				}
			}
		}
		if s.cfg.GatewayDomain != "" {
			for _, d := range sp.Networking.CustomDomains {
				if strings.HasSuffix(d, "."+s.cfg.GatewayDomain) {
					validationProblem(w, map[string][]string{"networking.custom_domains": {d + " is inside the gateway domain; it is already routed as {group}-{project}." + s.cfg.GatewayDomain}})
					return false
				}
			}
		}
	}
	if v := s.checkGroupQuota(*sp); v != nil {
		quotaProblem(w, v)
		return false
	}
	if sp.QueueConnection != nil {
		if _, err := s.st.Queue(sp.Project, sp.QueueConnection.QueueName); err != nil {
			validationProblem(w, map[string][]string{"queue_connection.queue_name": {"queue " + sp.QueueConnection.QueueName + " does not exist in project " + sp.Project}})
			return false
		}
	}
	return true
}

func (s *Server) createGroup(w http.ResponseWriter, u *store.UserRow, project string, sp api.ContainerGroupSpec) {
	sp.Project = project
	sp.Normalize()
	if errs := sp.Validate(); errs != nil {
		validationProblem(w, errs)
		return
	}
	if !s.checkSpecRefs(w, &sp) {
		return
	}
	now := store.Now()
	g := &store.GroupRow{
		ID: store.NewID("cg"), Project: project, Name: sp.Name, OwnerID: u.ID, Spec: sp, Version: 1,
		Status: api.GroupDeploying, DesiredReplicas: initialReplicas(sp), Created: now, Updated: now,
		GatewayToken: "ogmg_" + store.NewSecret(16),
	}
	if sp.AutostartPolicy != nil && !*sp.AutostartPolicy {
		g.Status = api.GroupStopped
	} else {
		g.StartTime = sql.NullInt64{Int64: now, Valid: true}
	}
	s.mu.Lock()
	err := s.st.InsertGroup(g)
	s.mu.Unlock()
	if err != nil {
		storeErr(w, err, "container group "+sp.Name)
		return
	}
	s.st.AddEvent(project, g.ID, "", "", "group.created", "Container Group Created")
	if g.Status == api.GroupDeploying {
		s.st.AddEvent(project, g.ID, "", "", "group.started", "Container Group Started")
	}
	s.Poke()
	writeJSON(w, http.StatusCreated, s.groupAPI(g, false))
}

// applySpecUpdate replaces a group's spec, bumping the version for runtime changes.
func (s *Server) applySpecUpdate(w http.ResponseWriter, g *store.GroupRow, sp api.ContainerGroupSpec) bool {
	sp.Name, sp.Project = g.Name, g.Project
	// Keep the stored registry password if the client echoed the mask.
	if sp.Container.RegistryAuth != nil && sp.Container.RegistryAuth.Password == "********" && g.Spec.Container.RegistryAuth != nil {
		sp.Container.RegistryAuth.Password = g.Spec.Container.RegistryAuth.Password
	}
	sp.Normalize()
	if errs := sp.Validate(); errs != nil {
		validationProblem(w, errs)
		return false
	}
	if !s.checkSpecRefs(w, &sp) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := g.Spec
	if runtimeFingerprint(old) != runtimeFingerprint(sp) {
		g.Version++
		s.st.AddEvent(g.Project, g.ID, "", "", "group.updated", "Container Group Updated (Version "+itoa(g.Version)+")")
	} else {
		s.st.AddEvent(g.Project, g.ID, "", "", "group.updated", "Container Group Settings Updated")
	}
	g.Spec = sp
	if sp.QueueAutoscaler == nil && !(sp.ScheduledScalingEnabled && len(sp.ScalingActions) > 0) {
		if g.DesiredReplicas != sp.Replicas {
			s.st.AddEvent(g.Project, g.ID, "", "", "group.replicas", "Container Group Desired Replica Count Updated from "+itoa(g.DesiredReplicas)+" to "+itoa(sp.Replicas))
		}
		g.DesiredReplicas = sp.Replicas
	} else if sp.QueueAutoscaler != nil {
		a := sp.QueueAutoscaler
		if g.DesiredReplicas < a.MinReplicas {
			g.DesiredReplicas = a.MinReplicas
		}
		if g.DesiredReplicas > a.MaxReplicas {
			g.DesiredReplicas = a.MaxReplicas
		}
	} else {
		g.Autoscale.LastScheduleAt = 0 // re-evaluate schedule now
	}
	if err := s.st.SaveGroup(g); err != nil {
		storeErr(w, err, "container group")
		return false
	}
	s.gw.invalidate()
	s.Poke()
	s.PokeJobs()
	return true
}

func itoa(n int) string { return strconv.Itoa(n) }

// mergePatch applies an RFC 7386 JSON merge patch.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}

func (s *Server) registerGroupRoutes(mux *http.ServeMux) {
	base := "/v1/projects/{project}/containers"

	mux.HandleFunc("GET "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		gs, err := s.st.Groups(p)
		if err != nil {
			storeErr(w, err, "groups")
			return
		}
		out := []api.ContainerGroup{}
		for _, g := range gs {
			out = append(out, s.groupAPI(g, false))
		}
		writeJSON(w, http.StatusOK, api.List[api.ContainerGroup]{Items: out})
	}))

	mux.HandleFunc("POST "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var sp api.ContainerGroupSpec
		if !decodeSpec(w, r, &sp) {
			return
		}
		s.createGroup(w, u, p, sp)
	}))

	mux.HandleFunc("GET "+base+"/{group}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		writeJSON(w, http.StatusOK, s.groupAPI(g, true))
	}))

	// PUT = declarative upsert (used by `ogm deploy`).
	mux.HandleFunc("PUT "+base+"/{group}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var sp api.ContainerGroupSpec
		if !decodeSpec(w, r, &sp) {
			return
		}
		sp.Name = r.PathValue("group")
		g, err := s.st.Group(p, sp.Name)
		if err != nil {
			s.createGroup(w, u, p, sp)
			return
		}
		if s.applySpecUpdate(w, g, sp) {
			writeJSON(w, http.StatusOK, s.groupAPI(g, false))
		}
	}))

	mux.HandleFunc("PATCH "+base+"/{group}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		var patch any
		if !decode(w, r, &patch) {
			return
		}
		var cur any
		_ = json.Unmarshal([]byte(store.JSON(g.Spec)), &cur)
		merged := mergePatch(cur, patch)
		var sp api.ContainerGroupSpec
		if err := json.Unmarshal([]byte(store.JSON(merged)), &sp); err != nil {
			problem(w, http.StatusBadRequest, "invalid patch", err.Error())
			return
		}
		if s.applySpecUpdate(w, g, sp) {
			writeJSON(w, http.StatusOK, s.groupAPI(g, false))
		}
	}))

	mux.HandleFunc("DELETE "+base+"/{group}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		s.mu.Lock()
		insts, _ := s.st.LiveInstances(g.ID)
		for _, in := range insts {
			s.requestStop(in, "container group deleted", "", 10)
		}
		_ = s.st.DeleteGroup(g.ID)
		s.mu.Unlock()
		s.st.AddEvent(p, g.ID, "", "", "group.deleted", "Container Group Deleted")
		s.gw.invalidate()
		w.WriteHeader(http.StatusNoContent)
	}))

	// A gateway token lets outside callers (a website, a friend without an account) use one
	// group's endpoint without a personal API key. Rotating revokes the old token immediately.
	mux.HandleFunc("POST "+base+"/{group}/rotate-gateway-token", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		g.GatewayToken = "ogmg_" + store.NewSecret(16)
		g.Updated = store.Now()
		if err := s.st.SaveGroup(g); err != nil {
			storeErr(w, err, "container group")
			return
		}
		s.st.AddEvent(p, g.ID, "", "", "group.gateway_token_rotated", "Gateway Token Rotated by "+u.Name)
		writeJSON(w, http.StatusOK, s.groupAPI(g, false))
	}))

	mux.HandleFunc("POST "+base+"/{group}/start", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		s.mu.Lock()
		if g.Status == api.GroupStopped || g.Status == GroupSucceeded || g.Status == api.GroupFailed {
			g.Status = api.GroupDeploying
			now := store.Now()
			g.StartTime = sql.NullInt64{Int64: now, Valid: true}
			g.FinishTime = sql.NullInt64{}
			g.DesiredReplicas = initialReplicas(g.Spec)
			g.Autoscale = store.AutoscaleState{}
			_ = s.st.SaveGroup(g)
			s.st.AddEvent(p, g.ID, "", "", "group.started", "Container Group Started")
		}
		s.mu.Unlock()
		s.Poke()
		writeJSON(w, http.StatusAccepted, s.groupAPI(g, false))
	}))

	mux.HandleFunc("POST "+base+"/{group}/stop", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		s.mu.Lock()
		if g.Status != api.GroupStopped {
			g.Status = api.GroupStopped
			g.FinishTime = sql.NullInt64{Int64: store.Now(), Valid: true}
			_ = s.st.SaveGroup(g)
			s.st.AddEvent(p, g.ID, "", "", "group.stopped", "Container Group Stopped")
		}
		s.mu.Unlock()
		s.Poke()
		writeJSON(w, http.StatusAccepted, s.groupAPI(g, false))
	}))

	mux.HandleFunc("GET "+base+"/{group}/instances", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		var insts []*store.InstanceRow
		if r.URL.Query().Get("all") == "1" {
			insts, _ = s.st.InstancesWhere(`group_id=?`, g.ID)
		} else {
			insts, _ = s.st.LiveInstances(g.ID)
		}
		out := []api.Instance{}
		for _, in := range insts {
			out = append(out, s.instanceAPI(in, g))
		}
		writeJSON(w, http.StatusOK, api.List[api.Instance]{Items: out})
	}))

	instance := func(w http.ResponseWriter, r *http.Request, p string) (*store.GroupRow, *store.InstanceRow, bool) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return nil, nil, false
		}
		in, err := s.st.Instance(r.PathValue("instance"))
		if err != nil || in.GroupID != g.ID {
			problem(w, http.StatusNotFound, "not found", "instance not found")
			return nil, nil, false
		}
		return g, in, true
	}

	mux.HandleFunc("GET "+base+"/{group}/instances/{instance}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		if g, in, ok := instance(w, r, p); ok {
			writeJSON(w, http.StatusOK, s.instanceAPI(in, g))
		}
	}))

	mux.HandleFunc("PATCH "+base+"/{group}/instances/{instance}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, in, ok := instance(w, r, p)
		if !ok {
			return
		}
		var body struct {
			DeletionCost *int `json:"deletion_cost"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.DeletionCost != nil {
			s.mu.Lock()
			in.DeletionCost = *body.DeletionCost
			_ = s.st.SaveInstance(in)
			s.mu.Unlock()
		}
		writeJSON(w, http.StatusOK, s.instanceAPI(in, g))
	}))

	mux.HandleFunc("POST "+base+"/{group}/instances/{instance}/{action}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, in, ok := instance(w, r, p)
		if !ok {
			return
		}
		if in.NodeID == "" || in.State == api.InstTerminated {
			problem(w, http.StatusConflict, "conflict", "instance is not placed on a node")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.PathValue("action") {
		case "reallocate":
			s.banNode(in.NodeID, g.ID)
			s.st.AddEvent(p, g.ID, in.ID, in.NodeID, "instance.reallocated", "Instance Reallocated by User")
			s.requestStop(in, "reallocated", "", 5)
			s.Poke()
		case "recreate":
			spec, err := s.instanceSpec(g, in)
			if err != nil {
				problem(w, http.StatusConflict, "can't recreate", err.Error())
				return
			}
			in.State, in.Ready, in.Started = api.InstDownloading, false, false
			_ = s.st.SaveInstance(in)
			s.st.AddEvent(p, g.ID, in.ID, in.NodeID, "instance.recreated", "Instance Recreated")
			s.hub.send(in.NodeID, proto.TypeInstanceRecreate, proto.InstanceRef{ID: in.ID, Spec: spec})
		case "restart":
			s.st.AddEvent(p, g.ID, in.ID, in.NodeID, "instance.restarted", "Instance Restarted")
			s.hub.send(in.NodeID, proto.TypeInstanceRestart, proto.InstanceRef{ID: in.ID})
		default:
			problem(w, http.StatusNotFound, "not found", "unknown action (reallocate|recreate|restart)")
			return
		}
		s.gw.invalidate()
		w.WriteHeader(http.StatusAccepted)
	}))

	mux.HandleFunc("GET "+base+"/{group}/events", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			storeErr(w, err, "container group")
			return
		}
		writeJSON(w, http.StatusOK, api.List[api.Event]{Items: s.events(`group_id=?`, qint(r, "limit", 200), g.ID)})
	}))

	mux.HandleFunc("GET "+base+"/{group}/logs", s.projected(s.handleLogs))
}

func (s *Server) events(where string, limit int, args ...any) []api.Event {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	q := `SELECT e.id,e.project,COALESCE(g.name,''),e.instance_id,e.node_id,e.kind,e.message,e.time FROM events e LEFT JOIN container_groups g ON g.id=e.group_id`
	if where != "" {
		q += ` WHERE e.` + where
	}
	q += ` ORDER BY e.id DESC LIMIT ` + itoa(limit)
	rows, err := s.st.DB.Query(q, args...)
	out := []api.Event{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e api.Event
		var t int64
		if rows.Scan(&e.ID, &e.Project, &e.Group, &e.InstanceID, &e.NodeID, &e.Kind, &e.Message, &t) == nil {
			e.Time = store.T(t)
			out = append(out, e)
		}
	}
	return out
}

// reservedHost reports hostnames that must never be routed to a workload: the hub's
// own public host, its gateway domain, and the internal host MCP sub-requests use.
func (s *Server) reservedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "ogm.internal" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && strings.EqualFold(u.Hostname(), host) {
		return true
	}
	return s.cfg.GatewayDomain != "" && strings.EqualFold(s.cfg.GatewayDomain, host)
}
