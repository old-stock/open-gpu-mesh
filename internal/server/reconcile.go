package server

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const (
	// GroupSucceeded is set when all replicas of a run-to-completion group exited successfully.
	GroupSucceeded = "succeeded"

	stopGrace          = 30  // seconds of SIGTERM grace for graceful stops
	stopConfirmTimeout = 120 // seconds before an unconfirmed stop is forced
	startTimeout       = 45 * time.Minute
	reallocBan         = 30 * time.Minute
)

// instanceSpec renders the agent-facing spec for an instance.
func (s *Server) instanceSpec(g *store.GroupRow, in *store.InstanceRow) *proto.InstanceSpec {
	sp := g.Spec
	env := map[string]string{}
	for k, v := range sp.Container.Env {
		env[k] = v
	}
	env["OGM_INSTANCE_ID"] = in.ID
	env["OGM_CONTAINER_GROUP"] = g.Name
	env["OGM_CONTAINER_GROUP_ID"] = g.ID
	env["OGM_PROJECT"] = g.Project
	env["OGM_MACHINE_ID"] = in.NodeID
	env["OGM_SERVER_URL"] = s.cfg.PublicURL
	env["OGM_REPLICA_INDEX"] = strconv.Itoa(in.ReplicaIndex)
	env["OGM_REPLICAS"] = strconv.Itoa(g.DesiredReplicas)
	ports := map[int]bool{}
	if sp.Networking != nil {
		ports[sp.Networking.Port] = true
	}
	if sp.QueueConnection != nil {
		ports[sp.QueueConnection.Port] = true
	}
	for _, p := range []*api.Probe{sp.StartupProbe, sp.ReadinessProbe, sp.LivenessProbe} {
		if p == nil {
			continue
		}
		if p.HTTP != nil {
			ports[p.HTTP.Port] = true
		}
		if p.TCP != nil {
			ports[p.TCP.Port] = true
		}
	}
	var pl []int
	for p := range ports {
		if p > 0 {
			pl = append(pl, p)
		}
	}
	sort.Ints(pl)
	return &proto.InstanceSpec{
		ID: in.ID, Project: g.Project, Group: g.Name, GroupID: g.ID, Version: in.Version,
		Image: sp.Container.Image, Command: sp.Container.Command, Env: env, RegistryAuth: sp.Container.RegistryAuth, PullPolicy: sp.Container.ImagePullPolicy,
		Resources: sp.Container.Resources, GPUIndexes: in.GPUIndexes, Ports: pl,
		StartupProbe: sp.StartupProbe, ReadinessProbe: sp.ReadinessProbe, LivenessProbe: sp.LivenessProbe,
		RestartPolicy: sp.RestartPolicy, IMDSSecret: in.IMDSSecret, DeletionCost: in.DeletionCost,
	}
}

var stateEvent = map[string]string{
	api.InstDownloading: "Instance Downloading",
	api.InstCreating:    "Instance Creating",
	api.InstRunning:     "Instance Running",
}

// applyReport merges an agent status report into the instance row. Caller holds s.mu.
func (s *Server) applyReport(nodeID string, in *store.InstanceRow, ir proto.InstanceReport) {
	if in.State == api.InstTerminated {
		return
	}
	if ir.State == api.InstTerminated {
		reason := ir.Message
		if in.State == api.InstStopping && in.StopReason != "" {
			reason = in.StopReason
		} else if g, err := s.st.GroupByID(in.GroupID); err == nil && ir.ExitCode != nil {
			if g.Spec.RestartPolicy == "never" || (g.Spec.RestartPolicy == "on_failure" && *ir.ExitCode == 0) {
				reason = "completed"
			}
		}
		if ir.Event != "" {
			s.st.AddEvent(in.Project, in.GroupID, in.ID, nodeID, "instance", ir.Event)
		}
		s.terminate(in, reason, ir.ExitCode)
		return
	}
	prevState, prevReady := in.State, in.Ready
	if in.State != api.InstStopping && in.State != api.InstAllocating {
		if ir.Version == 0 || ir.Version == in.Version {
			in.State = ir.State
		}
	}
	in.Started = ir.Started
	in.Ready = ir.Ready && in.State == api.InstRunning
	in.PullProgress = ir.PullProgress
	in.Metrics.CPUPercent = ir.Metrics.CPUPercent
	in.Metrics.MemoryMB = ir.Metrics.MemoryMB
	in.Metrics.GPUUtil = ir.Metrics.GPUUtil
	in.Metrics.GPUMemoryMB = ir.Metrics.GPUMemoryMB
	in.Metrics.GPUTempC = ir.Metrics.GPUTempC
	in.Metrics.InflightJobs = ir.Metrics.InflightJobs
	if ir.ExitCode != nil {
		in.ExitCode = sql.NullInt64{Int64: int64(*ir.ExitCode), Valid: true}
	}
	if ir.Message != "" {
		in.Message = ir.Message
	}
	now := store.Now()
	if in.State == api.InstRunning && !in.RunningAt.Valid {
		in.RunningAt = sql.NullInt64{Int64: now, Valid: true}
		in.BilledUntil = sql.NullInt64{Int64: now, Valid: true}
	}
	if in.State != prevState {
		if ev, ok := stateEvent[in.State]; ok {
			s.st.AddEvent(in.Project, in.GroupID, in.ID, nodeID, "instance."+in.State, ev)
		}
	}
	if in.Ready != prevReady && in.State == api.InstRunning {
		if in.Ready {
			s.st.AddEvent(in.Project, in.GroupID, in.ID, nodeID, "instance.ready", "Instance Ready (Readiness Probe Passed)")
		} else {
			s.st.AddEvent(in.Project, in.GroupID, in.ID, nodeID, "instance.not_ready", "Instance Not Ready (Readiness Probe Failure)")
		}
	}
	if ir.Event != "" {
		s.st.AddEvent(in.Project, in.GroupID, in.ID, nodeID, "instance", ir.Event)
	}
	_ = s.st.SaveInstance(in)
}

// terminate marks an instance gone and requeues its jobs. Caller holds s.mu.
func (s *Server) terminate(in *store.InstanceRow, reason string, exitCode *int) {
	if in.State == api.InstTerminated {
		return
	}
	in.State = api.InstTerminated
	in.Ready = false
	if reason == "" {
		reason = "terminated"
	}
	in.StopReason = reason
	if exitCode != nil {
		in.ExitCode = sql.NullInt64{Int64: int64(*exitCode), Valid: true}
	}
	_ = s.st.SaveInstance(in)
	msg := "Instance Terminated (" + reason + ")"
	if exitCode != nil {
		msg = fmt.Sprintf("Instance Exited:%d (%s)", *exitCode, reason)
	}
	s.st.AddEvent(in.Project, in.GroupID, in.ID, in.NodeID, "instance.terminated", msg)
	jobs, _ := s.st.JobsWhere(`status='running' AND instance_id=?`, "", 0, in.ID)
	for _, j := range jobs {
		if ns := s.hub.get(in.NodeID); ns != nil {
			ns.mu.Lock()
			delete(ns.jobs, j.ID)
			ns.mu.Unlock()
		}
		s.jobAttemptFailed(j.ID, j.Attempts, in.ID, "instance terminated: "+reason, true)
	}
	s.gw.invalidate()
}

// requestStop begins a graceful stop. Caller holds s.mu.
func (s *Server) requestStop(in *store.InstanceRow, reason, event string, grace int) {
	if in.State == api.InstStopping || in.State == api.InstTerminated {
		return
	}
	if in.NodeID == "" || in.State == api.InstAllocating {
		s.terminate(in, reason, nil)
		return
	}
	in.State = api.InstStopping
	in.Ready = false
	in.StopReason = reason
	in.StopRequestedAt = sql.NullInt64{Int64: store.Now(), Valid: true}
	_ = s.st.SaveInstance(in)
	if event != "" {
		s.st.AddEvent(in.Project, in.GroupID, in.ID, in.NodeID, "instance.stopping", event)
	}
	if !s.hub.send(in.NodeID, proto.TypeInstanceStop, proto.InstanceStop{ID: in.ID, GraceS: grace, Reason: reason}) {
		s.terminate(in, reason, nil)
	}
	s.gw.invalidate()
}

func stateRank(st string) int {
	switch st {
	case api.InstAllocating:
		return 0
	case api.InstDownloading:
		return 1
	case api.InstCreating:
		return 2
	case api.InstRunning:
		return 3
	}
	return 4
}

// reconcile drives actual state toward desired state.
func (s *Server) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	online := s.hub.online()

	nodes, err := s.st.Nodes(false)
	if err != nil {
		s.log.Error("reconcile: nodes", "err", err)
		return
	}
	// 1. Nodes offline past the grace period lose their instances.
	for _, n := range nodes {
		if online[n.ID] != nil {
			continue
		}
		if n.LastSeen.Valid && now.Sub(store.T(n.LastSeen.Int64)) < s.cfg.OfflineGrace {
			continue
		}
		insts, _ := s.st.InstancesWhere(`node_id=? AND state!='terminated'`, n.ID)
		for _, in := range insts {
			reason := "node offline"
			if in.State == api.InstStopping {
				reason = in.StopReason
			} else {
				s.st.AddEvent(in.Project, in.GroupID, in.ID, n.ID, "instance.interrupted", "Instance Interrupted (Node Offline)")
			}
			s.terminate(in, reason, nil)
		}
	}
	// 2. Force stops that were never confirmed; reallocate instances stuck starting.
	stuck, _ := s.st.InstancesWhere(`state IN ('stopping','downloading','creating')`)
	for _, in := range stuck {
		if in.State == api.InstStopping && in.StopRequestedAt.Valid && now.Sub(store.T(in.StopRequestedAt.Int64)) > stopConfirmTimeout*time.Second {
			s.terminate(in, in.StopReason, nil)
			continue
		}
		if in.State != api.InstStopping && in.AssignedAt.Valid && now.Sub(store.T(in.AssignedAt.Int64)) > startTimeout {
			s.banNode(in.NodeID, in.GroupID)
			s.st.AddEvent(in.Project, in.GroupID, in.ID, in.NodeID, "instance.reallocated", "Instance Reallocated by Platform (start timeout)")
			s.requestStop(in, "start timeout", "", 5)
		}
	}

	// 3. Per-group replica management.
	groups, err := s.st.Groups("")
	if err != nil {
		return
	}
	for _, g := range groups {
		s.reconcileGroup(g)
	}

	// 4. Placement.
	s.place(nodes, online)
}

func (s *Server) reconcileGroup(g *store.GroupRow) {
	live, err := s.st.LiveInstances(g.ID)
	if err != nil {
		return
	}
	if g.Status == api.GroupStopped || g.Status == api.GroupFailed || g.Status == api.GroupPending {
		for _, in := range live {
			s.requestStop(in, "container group stopped", "", stopGrace)
		}
		return
	}
	var active []*store.InstanceRow
	for _, in := range live {
		if in.State != api.InstStopping {
			active = append(active, in)
		}
	}
	var completed int
	_ = s.st.DB.QueryRow(`SELECT COUNT(*) FROM instances WHERE group_id=? AND state='terminated' AND stop_reason='completed' AND version=? AND updated>=?`,
		g.ID, g.Version, nullOr(g.StartTime)).Scan(&completed)
	desired := g.DesiredReplicas
	need := desired - len(active) - completed
	now := store.Now()
	if need > 0 {
		// Stable replica indexes (OGM_REPLICA_INDEX): the lowest index not held by an active
		// instance or by one that already completed this run, so sharded batch work is never redone.
		taken := map[int]bool{}
		for _, in := range active {
			taken[in.ReplicaIndex] = true
		}
		if completed > 0 {
			if rows, err := s.st.DB.Query(`SELECT replica_index FROM instances WHERE group_id=? AND state='terminated' AND stop_reason='completed' AND version=? AND updated>=?`,
				g.ID, g.Version, nullOr(g.StartTime)); err == nil {
				var idxs []int
				for rows.Next() {
					var x int
					if rows.Scan(&x) == nil {
						idxs = append(idxs, x)
					}
				}
				rows.Close()
				for _, x := range idxs {
					taken[x] = true
				}
			}
		}
		next := 0
		for i := 0; i < need; i++ {
			for taken[next] {
				next++
			}
			taken[next] = true
			in := &store.InstanceRow{
				ID: store.NewID("inst"), GroupID: g.ID, Project: g.Project, Version: g.Version, State: api.InstAllocating,
				IMDSSecret: store.NewSecret(16), Created: now, Updated: now, ReplicaIndex: next,
			}
			if err := s.st.InsertInstance(in); err == nil {
				s.st.AddEvent(g.Project, g.ID, in.ID, "", "instance.allocating", "Instance Allocating")
				active = append(active, in)
			}
		}
	} else if need < 0 {
		victims := append([]*store.InstanceRow(nil), active...)
		sort.SliceStable(victims, func(a, b int) bool {
			va, vb := victims[a], victims[b]
			if va.DeletionCost != vb.DeletionCost {
				return va.DeletionCost < vb.DeletionCost
			}
			if va.Ready != vb.Ready {
				return !va.Ready
			}
			if stateRank(va.State) != stateRank(vb.State) {
				return stateRank(va.State) < stateRank(vb.State)
			}
			return va.Created > vb.Created
		})
		removed := map[string]bool{}
		for _, in := range victims[:-need] {
			s.requestStop(in, "scaled down", "Instance Stopping (scale down)", stopGrace)
			removed[in.ID] = true
		}
		var keep []*store.InstanceRow
		for _, in := range active {
			if !removed[in.ID] {
				keep = append(keep, in)
			}
		}
		active = keep
	}

	// Rolling update toward the current version, in place on the same node.
	var old []*store.InstanceRow
	inProgress := 0
	for _, in := range active {
		if in.Version < g.Version {
			old = append(old, in)
		} else if !in.Ready {
			inProgress++
		}
	}
	if len(old) > 0 {
		budget := desired / 4
		if budget < 1 {
			budget = 1
		}
		budget -= inProgress
		for _, in := range old {
			if in.State == api.InstAllocating {
				in.Version = g.Version
				_ = s.st.SaveInstance(in)
				continue
			}
			if budget <= 0 {
				break
			}
			budget--
			in.Version = g.Version
			in.State = api.InstDownloading
			in.Ready, in.Started = false, false
			in.RunningAt = sql.NullInt64{}
			_ = s.st.SaveInstance(in)
			s.st.AddEvent(in.Project, in.GroupID, in.ID, in.NodeID, "instance.updating", fmt.Sprintf("Instance Updating (Version %d)", g.Version))
			if !s.hub.send(in.NodeID, proto.TypeInstanceRecreate, proto.InstanceRef{ID: in.ID, Spec: s.instanceSpec(g, in)}) {
				s.terminate(in, "node offline during update", nil)
			}
		}
		s.gw.invalidate()
	}

	// Group status.
	running := 0
	for _, in := range active {
		if in.State == api.InstRunning {
			running++
		}
	}
	status := g.Status
	switch {
	case desired > 0 && completed >= desired && len(active) == 0:
		status = GroupSucceeded
	case running > 0:
		status = api.GroupRunning
	case desired == 0:
		status = api.GroupRunning
	default:
		status = api.GroupDeploying
	}
	if status != g.Status {
		g.Status = status
		if status == GroupSucceeded {
			g.FinishTime = sql.NullInt64{Int64: now, Valid: true}
		}
		_ = s.st.SaveGroup(g)
	}
}

func nullOr(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

func (s *Server) banNode(nodeID, groupID string) {
	if nodeID == "" {
		return
	}
	_, _ = s.st.DB.Exec(`INSERT INTO node_bans(node_id,group_id,until) VALUES(?,?,?) ON CONFLICT(node_id,group_id) DO UPDATE SET until=excluded.until`,
		nodeID, groupID, time.Now().Add(reallocBan).UnixMilli())
}

// ---------- placement ----------

type nodeCap struct {
	node      *store.NodeRow
	freeGPU   map[int]bool
	freeCPU   int
	freeMemMB int
	freeDisk  int
	count     int
	perGroup  map[string]int
	insts     []*store.InstanceRow
	preempted bool
	// GPUs used by gpu_shared instances (not available to exclusive requests): VRAM reserved
	// by shared instances that aren't running yet (running ones show up in MemUsedMB).
	shared        map[int]bool
	sharedPending map[int]int
}

func (s *Server) capacities(nodes []*store.NodeRow, online map[string]*nodeSession) map[string]*nodeCap {
	caps := map[string]*nodeCap{}
	for _, n := range nodes {
		if online[n.ID] == nil {
			continue
		}
		c := &nodeCap{node: n, freeGPU: map[int]bool{}, freeCPU: n.Info.CPU, freeMemMB: n.Info.MemoryMB, freeDisk: n.Info.DiskGB, perGroup: map[string]int{},
			shared: map[int]bool{}, sharedPending: map[int]int{}}
		for _, g := range n.Info.GPUs {
			c.freeGPU[g.Index] = true
		}
		caps[n.ID] = c
	}
	insts, _ := s.st.InstancesWhere(`state!='terminated' AND node_id!=''`)
	groupCache := map[string]*store.GroupRow{}
	for _, in := range insts {
		c := caps[in.NodeID]
		if c == nil {
			continue
		}
		g := groupCache[in.GroupID]
		if g == nil {
			g, _ = s.st.GroupByID(in.GroupID)
			groupCache[in.GroupID] = g
		}
		if g == nil {
			continue
		}
		r := g.Spec.Container.Resources
		c.claimGPUs(in.GPUIndexes, r, in.State == api.InstRunning)
		c.freeCPU -= r.CPU
		c.freeMemMB -= r.MemoryMB
		c.freeDisk -= r.StorageGB
		c.count++
		c.perGroup[in.GroupID]++
		c.insts = append(c.insts, in)
		if in.State == api.InstStopping && in.StopReason == "preempted" {
			c.preempted = true
		}
	}
	return caps
}

// eligible checks static constraints (not capacity). Returns a reason when ineligible.
func (s *Server) eligible(n *store.NodeRow, g *store.GroupRow, banned map[string]bool) string {
	if n.Paused || n.AdminPaused {
		return "paused"
	}
	if !n.Info.InWindow {
		return "outside window"
	}
	if banned[n.ID] {
		return "banned"
	}
	sp := g.Spec
	if len(sp.CountryCodes) > 0 && !contains(sp.CountryCodes, n.Info.Country) {
		return "country"
	}
	for k, v := range sp.NodeSelector {
		got := n.Info.Labels[k]
		switch k { // built-in labels
		case "arch":
			got = n.Info.Arch
		case "os":
			got = n.Info.OS
		}
		if got != v {
			return "labels"
		}
	}
	if archs := s.imageArchs(sp.Container.Image); archs != nil && n.Info.Arch != "" && !archs[orDefault(n.Info.OS, "linux")+"/"+n.Info.Arch] {
		return "image arch"
	}
	if len(n.Info.AllowPriorities) > 0 && !contains(n.Info.AllowPriorities, sp.Priority) {
		return "priority not allowed by owner"
	}
	r := sp.Container.Resources
	if r.CPU > n.Info.CPU || r.MemoryMB > n.Info.MemoryMB || (n.Info.DiskGB > 0 && r.StorageGB > n.Info.DiskGB) {
		return "too small"
	}
	if r.GPUs() > 0 && len(matchingGPUs(n.Info.GPUs, r, nil)) < r.GPUs() {
		return "no matching gpu"
	}
	return ""
}

// matchingGPUs returns GPU indexes on the node that satisfy the request (optionally only free ones).
func matchingGPUs(gpus []api.GPU, r api.Resources, free map[int]bool) []api.GPU {
	var out []api.GPU
	for _, g := range gpus {
		if free != nil && !free[g.Index] {
			continue
		}
		if len(r.GPUClasses) > 0 && !contains(r.GPUClasses, g.Class) && !contains(r.GPUClasses, api.Slugify(g.Name)) {
			continue
		}
		if r.MinVRAMMB > 0 && g.VRAMMB < r.MinVRAMMB {
			continue
		}
		// During placement (free != nil) the GPU has no ogm instance, so any memory in use
		// belongs to the owner's own processes (a local LLM, a game...). Only place work
		// that fits in what is actually left, instead of OOMing next to it.
		if free != nil && r.MinVRAMMB > 0 && g.VRAMMB-g.MemUsedMB < r.MinVRAMMB {
			continue
		}
		out = append(out, g)
	}
	// Prefer the smallest GPUs that fit to keep big ones free.
	sort.SliceStable(out, func(a, b int) bool { return out[a].VRAMMB < out[b].VRAMMB })
	return out
}

// ownerVRAMBlocks reports whether a free GPU would match except for memory the owner's own
// processes are using (so the user sees why, rather than a generic "busy").
func ownerVRAMBlocks(c *nodeCap, r api.Resources) bool {
	for _, g := range c.node.Info.GPUs {
		if c.freeGPU[g.Index] && g.VRAMMB >= r.MinVRAMMB && g.VRAMMB-g.MemUsedMB < r.MinVRAMMB {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// claimGPUs records GPU use: exclusive instances take the whole GPU; shared ones mark it shared
// and, until running (when their usage appears in MemUsedMB), reserve their VRAM budget.
func (c *nodeCap) claimGPUs(idx []int, r api.Resources, running bool) {
	for _, i := range idx {
		delete(c.freeGPU, i)
		if r.GPUShared {
			if c.shared == nil {
				c.shared, c.sharedPending = map[int]bool{}, map[int]int{}
			}
			c.shared[i] = true
			if !running {
				c.sharedPending[i] += r.MinVRAMMB
			}
		}
	}
}

// fitsShared picks a GPU for a gpu_shared request: a free GPU or one already shared, with
// enough VRAM left after the owner's usage, running shared instances, and pending reservations.
func fitsShared(c *nodeCap, r api.Resources) ([]int, bool) {
	var best *api.GPU
	bestLeft := 0
	for i := range c.node.Info.GPUs {
		g := &c.node.Info.GPUs[i]
		if !c.freeGPU[g.Index] && !c.shared[g.Index] {
			continue
		}
		if len(r.GPUClasses) > 0 && !contains(r.GPUClasses, g.Class) && !contains(r.GPUClasses, api.Slugify(g.Name)) {
			continue
		}
		left := g.VRAMMB - g.MemUsedMB - c.sharedPending[g.Index]
		if left < r.MinVRAMMB {
			continue
		}
		// Pack onto already-shared GPUs first (keeps whole GPUs free), then the tightest fit.
		if best == nil || (c.shared[g.Index] && !c.shared[best.Index]) ||
			(c.shared[g.Index] == c.shared[best.Index] && left < bestLeft) {
			best, bestLeft = g, left
		}
	}
	if best == nil {
		return nil, false
	}
	return []int{best.Index}, true
}

func fits(c *nodeCap, r api.Resources) ([]int, bool) {
	if r.CPU > c.freeCPU || r.MemoryMB > c.freeMemMB || (c.node.Info.DiskGB > 0 && r.StorageGB > c.freeDisk) {
		return nil, false
	}
	n := r.GPUs()
	if n == 0 {
		return nil, true
	}
	if r.GPUShared {
		return fitsShared(c, r)
	}
	m := matchingGPUs(c.node.Info.GPUs, r, c.freeGPU)
	if len(m) < n {
		return nil, false
	}
	idx := make([]int, 0, n)
	for _, g := range m[:n] {
		idx = append(idx, g.Index)
	}
	sort.Ints(idx)
	return idx, true
}

func (s *Server) place(nodes []*store.NodeRow, online map[string]*nodeSession) {
	pending, err := s.st.InstancesWhere(`state='allocating'`)
	if err != nil || len(pending) == 0 {
		return
	}
	caps := s.capacities(nodes, online)
	groups := map[string]*store.GroupRow{}
	credits := map[string]float64{}
	users, _ := s.st.Users()
	for _, u := range users {
		credits[u.ID] = u.Credits
	}
	var work []*store.InstanceRow
	for _, in := range pending {
		g := groups[in.GroupID]
		if g == nil {
			g, _ = s.st.GroupByID(in.GroupID)
			if g == nil {
				s.terminate(in, "group deleted", nil)
				continue
			}
			groups[in.GroupID] = g
		}
		if g.Status != api.GroupRunning && g.Status != api.GroupDeploying {
			continue
		}
		work = append(work, in)
	}
	sort.SliceStable(work, func(a, b int) bool {
		ga, gb := groups[work[a].GroupID], groups[work[b].GroupID]
		ra, rb := api.PriorityRank(ga.Spec.Priority), api.PriorityRank(gb.Spec.Priority)
		if ra != rb {
			return ra > rb
		}
		if credits[ga.OwnerID] != credits[gb.OwnerID] {
			return credits[ga.OwnerID] > credits[gb.OwnerID]
		}
		return work[a].Created < work[b].Created
	})

	for _, in := range work {
		g := groups[in.GroupID]
		r := g.Spec.Container.Resources
		banned := s.bannedNodes(g.ID)
		var best *nodeCap
		var bestGPUs []int
		bestScore := -1e18
		reasons := map[string]int{}
		var preemptable []*nodeCap
		for _, c := range caps {
			if why := s.eligible(c.node, g, banned); why != "" {
				reasons[why]++
				continue
			}
			idx, ok := fits(c, r)
			if !ok {
				if r.MinVRAMMB > 0 && ownerVRAMBlocks(c, r) {
					reasons["vram in use by owner"]++
				} else {
					reasons["busy"]++
				}
				preemptable = append(preemptable, c)
				continue
			}
			score := s.score(c, g, idx)
			if score > bestScore {
				best, bestGPUs, bestScore = c, idx, score
			}
		}
		if best == nil {
			if s.tryPreempt(in, g, preemptable, groups) {
				continue
			}
			msg := "waiting for capacity"
			if len(reasons) > 0 {
				var parts []string
				for k, v := range reasons {
					parts = append(parts, fmt.Sprintf("%d %s", v, k))
				}
				sort.Strings(parts)
				msg += ": " + strings.Join(parts, ", ")
			} else {
				msg += ": no nodes online"
			}
			if in.Message != msg {
				in.Message = msg
				_ = s.st.SaveInstance(in)
			}
			continue
		}
		// Assign.
		in.NodeID = best.node.ID
		in.GPUIndexes = bestGPUs
		in.State = api.InstDownloading
		in.Message = ""
		in.AssignedAt = sql.NullInt64{Int64: store.Now(), Valid: true}
		_ = s.st.SaveInstance(in)
		if !s.hub.send(best.node.ID, proto.TypeInstanceStart, proto.InstanceRef{ID: in.ID, Spec: s.instanceSpec(g, in)}) {
			in.NodeID, in.GPUIndexes, in.State = "", nil, api.InstAllocating
			in.AssignedAt = sql.NullInt64{}
			_ = s.st.SaveInstance(in)
			continue
		}
		gpuName := ""
		for _, gp := range best.node.Info.GPUs {
			if len(bestGPUs) > 0 && gp.Index == bestGPUs[0] {
				gpuName = gp.Name
			}
		}
		msg := "Instance Allocated to node " + best.node.Name
		if gpuName != "" {
			msg += " (" + gpuName + ")"
		}
		s.st.AddEvent(in.Project, in.GroupID, in.ID, best.node.ID, "instance.allocated", msg)
		best.claimGPUs(bestGPUs, r, false)
		best.freeCPU -= r.CPU
		best.freeMemMB -= r.MemoryMB
		best.freeDisk -= r.StorageGB
		best.count++
		best.perGroup[g.ID]++
	}
}

func (s *Server) score(c *nodeCap, g *store.GroupRow, gpuIdx []int) float64 {
	score := 0.0
	if contains(c.node.Info.CachedImages, g.Spec.Container.Image) {
		score += 100
	}
	if c.node.TrackedSeconds > 0 {
		score += 50 * float64(c.node.OnlineSeconds) / float64(c.node.TrackedSeconds)
	} else {
		score += 25
	}
	score -= 5 * float64(c.count)
	score -= 20 * float64(c.perGroup[g.ID])
	// Bin-pack GPUs: prefer the smallest VRAM that fits.
	for _, idx := range gpuIdx {
		for _, gp := range c.node.Info.GPUs {
			if gp.Index == idx {
				score -= float64(gp.VRAMMB) / 4096
			}
		}
	}
	return score
}

func (s *Server) bannedNodes(groupID string) map[string]bool {
	out := map[string]bool{}
	rows, err := s.st.DB.Query(`SELECT node_id FROM node_bans WHERE group_id=? AND until>?`, groupID, store.Now())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// tryPreempt stops lower-priority instances on a node so `in` can fit next tick.
func (s *Server) tryPreempt(in *store.InstanceRow, g *store.GroupRow, candidates []*nodeCap, groups map[string]*store.GroupRow) bool {
	rank := api.PriorityRank(g.Spec.Priority)
	r := g.Spec.Container.Resources
	for _, c := range candidates {
		if c.preempted {
			continue
		}
		var victims []*store.InstanceRow
		for _, vi := range c.insts {
			if vi.State == api.InstStopping || vi.State == api.InstTerminated {
				continue
			}
			vg := groups[vi.GroupID]
			if vg == nil {
				vg, _ = s.st.GroupByID(vi.GroupID)
				groups[vi.GroupID] = vg
			}
			if vg == nil || api.PriorityRank(vg.Spec.Priority) >= rank {
				continue
			}
			victims = append(victims, vi)
		}
		if len(victims) == 0 {
			continue
		}
		sort.SliceStable(victims, func(a, b int) bool {
			ra := api.PriorityRank(groups[victims[a].GroupID].Spec.Priority)
			rb := api.PriorityRank(groups[victims[b].GroupID].Spec.Priority)
			if ra != rb {
				return ra < rb
			}
			return victims[a].DeletionCost < victims[b].DeletionCost
		})
		// Simulate freeing victims one by one until the request fits.
		sim := &nodeCap{node: c.node, freeGPU: map[int]bool{}, freeCPU: c.freeCPU, freeMemMB: c.freeMemMB, freeDisk: c.freeDisk,
			shared: map[int]bool{}, sharedPending: map[int]int{}}
		for k, v := range c.freeGPU {
			sim.freeGPU[k] = v
		}
		for k, v := range c.shared {
			sim.shared[k] = v
		}
		for k, v := range c.sharedPending {
			sim.sharedPending[k] = v
		}
		var chosen []*store.InstanceRow
		for _, vi := range victims {
			vr := groups[vi.GroupID].Spec.Container.Resources
			for _, idx := range vi.GPUIndexes {
				sim.freeGPU[idx] = true
			}
			sim.freeCPU += vr.CPU
			sim.freeMemMB += vr.MemoryMB
			sim.freeDisk += vr.StorageGB
			chosen = append(chosen, vi)
			if _, ok := fits(sim, r); ok {
				for _, v := range chosen {
					s.st.AddEvent(v.Project, v.GroupID, v.ID, v.NodeID, "instance.interrupted", "Instance Interrupted (Priority)")
					s.requestStop(v, "preempted", "", 10)
				}
				c.preempted = true
				in.Message = "preempting lower-priority workload on " + c.node.Name
				_ = s.st.SaveInstance(in)
				return true
			}
		}
	}
	return false
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
