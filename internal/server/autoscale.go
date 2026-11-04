package server

import (
	"fmt"
	"math"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// lastFire returns the most recent time <= now that the schedule fired (within 8 days).
func lastFire(expr string, now time.Time) (time.Time, bool) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return time.Time{}, false
	}
	var last time.Time
	t := sched.Next(now.Add(-8 * 24 * time.Hour))
	for i := 0; i < 20000 && !t.IsZero() && !t.After(now); i++ {
		last = t
		t = sched.Next(t)
	}
	return last, !last.IsZero()
}

// QueueAutoscaleTarget computes the next replica count (exported for tests).
func QueueAutoscaleTarget(a api.QueueAutoscaler, current, queueLen int) int {
	dql := a.DesiredQueueLength
	if dql < 1 {
		dql = 1
	}
	target := int(math.Ceil(float64(queueLen) / float64(dql)))
	if target < a.MinReplicas {
		target = a.MinReplicas
	}
	if target > a.MaxReplicas {
		target = a.MaxReplicas
	}
	period := a.PollingPeriod
	if period <= 0 {
		period = 30
	}
	up := int(math.Ceil(float64(a.MaxUpscalePerMinute) * float64(period) / 60))
	down := int(math.Ceil(float64(a.MaxDownscalePerMinute) * float64(period) / 60))
	if up < 1 {
		up = 1
	}
	if down < 1 {
		down = 1
	}
	if target > current+up {
		target = current + up
	}
	if target < current-down {
		target = current - down
	}
	return target
}

func (s *Server) autoscale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups, err := s.st.Groups("")
	if err != nil {
		return
	}
	now := time.Now()
	for _, g := range groups {
		if g.Status != api.GroupRunning && g.Status != api.GroupDeploying {
			continue
		}
		if g.Spec.HTTPAutoscaler != nil {
			continue // httpAutoscale
		}
		desired := g.DesiredReplicas
		reason := ""
		changedState := false
		sp := g.Spec
		if sp.QueueAutoscaler != nil && sp.QueueConnection != nil {
			a := *sp.QueueAutoscaler
			if now.UnixMilli()-g.Autoscale.LastPoll >= int64(a.PollingPeriod)*1000 {
				g.Autoscale.LastPoll = now.UnixMilli()
				changedState = true
				qlen := 0
				if q, err := s.st.Queue(g.Project, sp.QueueConnection.QueueName); err == nil {
					st, _ := s.st.QueueStats(q.ID)
					qlen = st.Pending + st.Running
				}
				desired = QueueAutoscaleTarget(a, g.DesiredReplicas, qlen)
				reason = fmt.Sprintf("queue autoscaler, queue length %d", qlen)
			}
		} else if sp.ScheduledScalingEnabled && len(sp.ScalingActions) > 0 {
			var best time.Time
			bestReplicas := -1
			for _, sa := range sp.ScalingActions {
				if t, ok := lastFire(sa.Schedule, now); ok && t.After(best) {
					best, bestReplicas = t, sa.Replicas
				}
			}
			if bestReplicas >= 0 && best.UnixMilli() != g.Autoscale.LastScheduleAt {
				g.Autoscale.LastScheduleAt = best.UnixMilli()
				changedState = true
				desired = bestReplicas
				reason = "scheduled scaling action"
			}
		}
		if desired != g.DesiredReplicas {
			s.st.AddEvent(g.Project, g.ID, "", "", "group.replicas",
				fmt.Sprintf("Container Group Desired Replica Count Updated from %d to %d (%s)", g.DesiredReplicas, desired, reason))
			g.DesiredReplicas = desired
			changedState = true
			s.Poke()
		}
		if changedState {
			_ = s.st.SaveGroup(g)
		}
	}
}

// Priority credit multipliers.
var priorityMultiplier = map[string]float64{
	api.PriorityHigh:   1.5,
	api.PriorityMedium: 1.0,
	api.PriorityLow:    0.75,
	api.PriorityBatch:  0.5,
}

const cpuCreditsPerVCPUHour = 0.05

// instanceRate returns credits/hour for an instance on a node.
func instanceRate(g *store.GroupRow, in *store.InstanceRow, node *store.NodeRow, classes map[string]float64) float64 {
	rate := 0.0
	if len(in.GPUIndexes) > 0 && node != nil {
		for _, idx := range in.GPUIndexes {
			for _, gp := range node.Info.GPUs {
				if gp.Index == idx {
					c, ok := classes[gp.Class]
					if !ok {
						c = 1
					}
					// Shared GPUs are billed by the slice of VRAM reserved.
					if r := g.Spec.Container.Resources; r.GPUShared && gp.VRAMMB > 0 {
						c *= math.Min(1, float64(r.MinVRAMMB)/float64(gp.VRAMMB))
					}
					rate += c
				}
			}
		}
	} else {
		rate = cpuCreditsPerVCPUHour * float64(g.Spec.Container.Resources.CPU)
	}
	m, ok := priorityMultiplier[g.Spec.Priority]
	if !ok {
		m = 1
	}
	return rate * m
}

// accrueCredits moves credits from consumers to contributors for running instances.
func (s *Server) accrueCredits() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := store.Now()
	_, _ = s.st.DB.Exec(`UPDATE nodes SET tracked_seconds=tracked_seconds+60 WHERE status!='pending'`)
	insts, err := s.st.InstancesWhere(`state='running'`)
	if err != nil || len(insts) == 0 {
		return
	}
	cls, _ := s.st.GPUClasses()
	classes := map[string]float64{}
	for _, c := range cls {
		classes[c.ID] = c.CreditsPerHour
	}
	type key struct{ user, ref string }
	debit := map[key]float64{}
	credit := map[key]float64{}
	groups := map[string]*store.GroupRow{}
	nodes := map[string]*store.NodeRow{}
	for _, in := range insts {
		from := in.BilledUntil.Int64
		if !in.BilledUntil.Valid {
			from = in.RunningAt.Int64
		}
		if from == 0 || now <= from {
			continue
		}
		g := groups[in.GroupID]
		if g == nil {
			g, _ = s.st.GroupByID(in.GroupID)
			groups[in.GroupID] = g
		}
		n := nodes[in.NodeID]
		if n == nil {
			n, _ = s.st.NodeByID(in.NodeID)
			nodes[in.NodeID] = n
		}
		if g == nil || n == nil {
			continue
		}
		amt := instanceRate(g, in, n, classes) * float64(now-from) / 3_600_000
		in.BilledUntil.Int64, in.BilledUntil.Valid = now, true
		_ = s.st.SaveInstance(in)
		if g.OwnerID == n.OwnerID || amt <= 0 {
			continue
		}
		debit[key{g.OwnerID, g.Project + "/" + g.Name}] += amt
		credit[key{n.OwnerID, n.Name}] += amt
	}
	for k, v := range debit {
		_ = s.st.AddCredits(k.user, -round4(v), "usage", k.ref)
	}
	for k, v := range credit {
		_ = s.st.AddCredits(k.user, round4(v), "contribution", k.ref)
	}
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
