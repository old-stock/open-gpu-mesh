package server

import (
	"fmt"
	"math"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

const maxDemandSamples = 600

// HTTPAutoscaleTarget computes the replica count wanted for the observed demand (requests in
// flight plus queued), Knative-style: the stable window average, or the last 6 s if that is
// higher (panic mode reacts to bursts). Exported for tests.
func HTTPAutoscaleTarget(a api.HTTPAutoscaler, samples []float64) int {
	target := float64(a.TargetConcurrency)
	if target <= 0 {
		target = 10
	}
	avg := func(n int) float64 {
		if n > len(samples) {
			n = len(samples)
		}
		if n == 0 {
			return 0
		}
		sum := 0.0
		for _, v := range samples[len(samples)-n:] {
			sum += v
		}
		return sum / float64(n)
	}
	stable := int(math.Ceil(avg(a.StableWindowSeconds)/target - 1e-9))
	panic := int(math.Ceil(avg(6)/target - 1e-9))
	want := max(stable, panic)
	return min(max(want, a.MinReplicas), a.MaxReplicas)
}

// sampleDemand records one demand sample per group and returns a copy of the series.
func (gw *Gateway) sampleDemand(groupID string) []float64 {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	d := float64(gw.ginflight[groupID] + gw.gwaiting[groupID])
	ss := append(gw.samples[groupID], d)
	if len(ss) > maxDemandSamples {
		ss = ss[len(ss)-maxDemandSamples:]
	}
	gw.samples[groupID] = ss
	return append([]float64(nil), ss...)
}

// httpAutoscale runs every second for groups with an http_autoscaler: scale up as soon as
// demand needs it, scale down only after demand stayed lower for scale_down_delay_seconds.
func (s *Server) httpAutoscale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups, err := s.st.Groups("")
	if err != nil {
		return
	}
	now := time.Now()
	seen := map[string]bool{}
	for _, g := range groups {
		a := g.Spec.HTTPAutoscaler
		if a == nil || (g.Status != api.GroupRunning && g.Status != api.GroupDeploying) {
			continue
		}
		seen[g.ID] = true
		samples := s.gw.sampleDemand(g.ID)
		want := HTTPAutoscaleTarget(*a, samples)
		cur := g.DesiredReplicas
		s.gw.mu.Lock()
		if want >= cur {
			delete(s.gw.lowSince, g.ID)
		} else if _, ok := s.gw.lowSince[g.ID]; !ok {
			s.gw.lowSince[g.ID] = now
		}
		low, isLow := s.gw.lowSince[g.ID]
		s.gw.mu.Unlock()
		switch {
		case want > cur:
		case want < cur && isLow && now.Sub(low) >= time.Duration(a.ScaleDownDelaySeconds)*time.Second:
			s.gw.mu.Lock()
			delete(s.gw.lowSince, g.ID)
			s.gw.mu.Unlock()
		default:
			continue
		}
		last := samples[len(samples)-1]
		s.st.AddEvent(g.Project, g.ID, "", "", "group.replicas",
			fmt.Sprintf("Container Group Desired Replica Count Updated from %d to %d (http autoscaler, %.0f concurrent requests)", cur, want, last))
		g.DesiredReplicas = want
		_ = s.st.SaveGroup(g)
		s.Poke()
	}
	s.gw.mu.Lock()
	for id := range s.gw.samples {
		if !seen[id] {
			delete(s.gw.samples, id)
			delete(s.gw.lowSince, id)
		}
	}
	s.gw.mu.Unlock()
}

// scaleFromZero is called when a request (or link) arrives for an autoscaled group with no
// instances: it raises the desired replica count to at least one right away.
func (s *Server) scaleFromZero(groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.st.GroupByID(groupID)
	if err != nil || g.Spec.HTTPAutoscaler == nil || g.DesiredReplicas > 0 || g.Status == api.GroupStopped {
		return
	}
	want := max(1, g.Spec.HTTPAutoscaler.MinReplicas)
	s.st.AddEvent(g.Project, g.ID, "", "", "group.replicas",
		fmt.Sprintf("Container Group Desired Replica Count Updated from 0 to %d (http autoscaler, request arrived)", want))
	g.DesiredReplicas = want
	_ = s.st.SaveGroup(g)
	s.gw.mu.Lock()
	delete(s.gw.lowSince, g.ID)
	s.gw.mu.Unlock()
	s.Poke()
}
