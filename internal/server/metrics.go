package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// Metrics: Prometheus exposition at GET /metrics, and per-group per-minute time series
// (CloudWatch-style) at GET /v1/projects/{p}/containers/{g}/metrics. Gateway series are
// kept in memory for 24 h; a hub restart starts them fresh.

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type groupMetrics struct {
	project, group string
	byCode         map[int]int64 // status code -> count
	hist           []int64       // cumulative per latencyBuckets (+Inf at end)
	sum            float64
	minutes        []minuteBucket // ring, newest last
}

type minuteBucket struct {
	T        int64     `json:"-"`
	Requests int       `json:"requests"`
	Err4xx   int       `json:"errors_4xx"`
	Err5xx   int       `json:"errors_5xx"`
	lat      []float64 // seconds, capped sample for percentiles
	Ready    int       `json:"ready_replicas"`
	Desired  int       `json:"desired_replicas"`
	CPU      float64   `json:"cpu_percent"`
	MemMB    float64   `json:"memory_mb"`
	GPUUtil  float64   `json:"gpu_util_percent"`
}

type metricsStore struct {
	mu     sync.Mutex
	groups map[string]*groupMetrics // group id
}

func newMetrics() *metricsStore { return &metricsStore{groups: map[string]*groupMetrics{}} }

func (m *metricsStore) get(g *store.GroupRow) *groupMetrics {
	gm := m.groups[g.ID]
	if gm == nil {
		gm = &groupMetrics{project: g.Project, group: g.Name, byCode: map[int]int64{}, hist: make([]int64, len(latencyBuckets)+1)}
		m.groups[g.ID] = gm
	}
	return gm
}

func (gm *groupMetrics) bucket(now time.Time) *minuteBucket {
	t := now.Truncate(time.Minute).Unix()
	if n := len(gm.minutes); n > 0 && gm.minutes[n-1].T == t {
		return &gm.minutes[n-1]
	}
	gm.minutes = append(gm.minutes, minuteBucket{T: t})
	if len(gm.minutes) > 1440 {
		gm.minutes = gm.minutes[len(gm.minutes)-1440:]
	}
	return &gm.minutes[len(gm.minutes)-1]
}

func (m *metricsStore) observe(g *store.GroupRow, code int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	gm := m.get(g)
	sec := d.Seconds()
	gm.byCode[code]++
	gm.sum += sec
	i := sort.SearchFloat64s(latencyBuckets, sec)
	gm.hist[i]++
	b := gm.bucket(time.Now())
	b.Requests++
	switch {
	case code >= 500:
		b.Err5xx++
	case code >= 400:
		b.Err4xx++
	}
	if len(b.lat) < 2000 {
		b.lat = append(b.lat, sec)
	}
}

// sampleGroups records replicas and resource usage once a minute.
func (s *Server) sampleGroups() {
	groups, err := s.st.Groups("")
	if err != nil {
		return
	}
	now := time.Now()
	type agg struct {
		ready    int
		cpu, mem float64
		gpu      float64
		gpuCount int
	}
	for _, g := range groups {
		insts, _ := s.st.LiveInstances(g.ID)
		var a agg
		for _, in := range insts {
			if in.Ready {
				a.ready++
			}
			a.cpu += in.Metrics.CPUPercent
			a.mem += in.Metrics.MemoryMB
			if len(in.GPUIndexes) > 0 {
				a.gpu += in.Metrics.GPUUtil
				a.gpuCount++
			}
		}
		s.metrics.mu.Lock()
		b := s.metrics.get(g).bucket(now)
		b.Ready, b.Desired, b.CPU, b.MemMB = a.ready, g.DesiredReplicas, round1(a.cpu), round1(a.mem)
		if a.gpuCount > 0 {
			b.GPUUtil = round1(a.gpu / float64(a.gpuCount))
		}
		s.metrics.mu.Unlock()
	}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	i := int(math.Ceil(p*float64(len(c)))) - 1
	if i < 0 {
		i = 0
	}
	return math.Round(c[i]*1e4) / 10 // ms with 0.1 precision
}

// MetricPoint is one minute of a group's series.
type MetricPoint struct {
	Time time.Time `json:"time"`
	minuteBucket
	P50ms float64 `json:"p50_ms"`
	P95ms float64 `json:"p95_ms"`
	P99ms float64 `json:"p99_ms"`
}

func (s *Server) registerMetricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{project}/containers/{group}/metrics", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		g, err := s.st.Group(p, r.PathValue("group"))
		if err != nil {
			problem(w, http.StatusNotFound, "not found", "container group not found")
			return
		}
		minutes := qint(r, "minutes", 60)
		if minutes < 1 || minutes > 1440 {
			minutes = 1440
		}
		since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()
		out := []MetricPoint{}
		var total, e4, e5 int
		var lat []float64
		s.metrics.mu.Lock()
		if gm := s.metrics.groups[g.ID]; gm != nil {
			for _, b := range gm.minutes {
				if b.T < since {
					continue
				}
				out = append(out, MetricPoint{Time: time.Unix(b.T, 0).UTC(), minuteBucket: b,
					P50ms: percentile(b.lat, .5), P95ms: percentile(b.lat, .95), P99ms: percentile(b.lat, .99)})
				total, e4, e5 = total+b.Requests, e4+b.Err4xx, e5+b.Err5xx
				lat = append(lat, b.lat...)
			}
		}
		s.metrics.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"group": g.Name, "minutes": minutes, "points": out,
			"summary": map[string]any{"requests": total, "errors_4xx": e4, "errors_5xx": e5,
				"p50_ms": percentile(lat, .5), "p95_ms": percentile(lat, .95), "p99_ms": percentile(lat, .99)},
		})
	}))

	// Prometheus scrape endpoint: admin users' keys (a viewer-scoped admin key is ideal).
	mux.HandleFunc("GET /metrics", s.authed(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		real, err := s.st.UserByID(u.ID)
		if err != nil || real.Role != "admin" {
			problem(w, http.StatusForbidden, "forbidden", "metrics need a key of an admin user (a viewer key is enough)")
			return
		}
		if sc := scopeFrom(r.Context()); sc != nil && sc.Project != "" {
			problem(w, http.StatusForbidden, "forbidden", "metrics need an unscoped (all-projects) key")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.writePrometheus(w)
	}))
}

func promLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func (s *Server) writePrometheus(w io.Writer) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f+"\n", a...) }
	p("# HELP ogm_info Hub build info.\n# TYPE ogm_info gauge")
	p(`ogm_info{version="%s"} 1`, promLabel(api.Version))

	nodes, _ := s.st.Nodes(false)
	online := s.hub.online()
	p("# HELP ogm_node_up Whether the node's agent is connected.\n# TYPE ogm_node_up gauge")
	p("# HELP ogm_node_gpus GPUs shared by the node.\n# TYPE ogm_node_gpus gauge")
	for _, n := range nodes {
		up := 0
		if online[n.ID] != nil {
			up = 1
		}
		p(`ogm_node_up{node="%s",node_id="%s",country="%s"} %d`, promLabel(n.Name), n.ID, promLabel(n.Info.Country), up)
		p(`ogm_node_gpus{node="%s"} %d`, promLabel(n.Name), len(n.Info.GPUs))
		for _, g := range n.Info.GPUs {
			p(`ogm_node_gpu_utilization_percent{node="%s",gpu="%d",name="%s"} %g`, promLabel(n.Name), g.Index, promLabel(g.Name), g.UtilPct)
			p(`ogm_node_gpu_memory_used_mb{node="%s",gpu="%d"} %d`, promLabel(n.Name), g.Index, g.MemUsedMB)
		}
	}

	groups, _ := s.st.Groups("")
	p("# HELP ogm_group_desired_replicas Desired replicas.\n# TYPE ogm_group_desired_replicas gauge")
	p("# HELP ogm_group_instances Instances by state.\n# TYPE ogm_group_instances gauge")
	for _, g := range groups {
		lbl := fmt.Sprintf(`project="%s",group="%s"`, promLabel(g.Project), promLabel(g.Name))
		p(`ogm_group_desired_replicas{%s} %d`, lbl, g.DesiredReplicas)
		insts, _ := s.st.LiveInstances(g.ID)
		states := map[string]int{}
		ready := 0
		var cpu, mem float64
		for _, in := range insts {
			states[in.State]++
			if in.Ready {
				ready++
			}
			cpu += in.Metrics.CPUPercent
			mem += in.Metrics.MemoryMB
		}
		for _, st := range []string{api.InstAllocating, api.InstDownloading, api.InstCreating, api.InstRunning, api.InstStopping} {
			p(`ogm_group_instances{%s,state="%s"} %d`, lbl, st, states[st])
		}
		p(`ogm_group_ready_instances{%s} %d`, lbl, ready)
		p(`ogm_group_cpu_percent{%s} %g`, lbl, round1(cpu))
		p(`ogm_group_memory_mb{%s} %g`, lbl, round1(mem))
	}

	s.metrics.mu.Lock()
	ids := make([]string, 0, len(s.metrics.groups))
	for id := range s.metrics.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	p("# HELP ogm_gateway_requests_total Gateway requests by status code.\n# TYPE ogm_gateway_requests_total counter")
	for _, id := range ids {
		gm := s.metrics.groups[id]
		codes := make([]int, 0, len(gm.byCode))
		for c := range gm.byCode {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			p(`ogm_gateway_requests_total{project="%s",group="%s",code="%d"} %d`, promLabel(gm.project), promLabel(gm.group), c, gm.byCode[c])
		}
	}
	p("# HELP ogm_gateway_request_duration_seconds Gateway request latency (to response end).\n# TYPE ogm_gateway_request_duration_seconds histogram")
	for _, id := range ids {
		gm := s.metrics.groups[id]
		lbl := fmt.Sprintf(`project="%s",group="%s"`, promLabel(gm.project), promLabel(gm.group))
		var cum, count int64
		for i, le := range latencyBuckets {
			cum += gm.hist[i]
			p(`ogm_gateway_request_duration_seconds_bucket{%s,le="%g"} %d`, lbl, le, cum)
		}
		count = cum + gm.hist[len(latencyBuckets)]
		p(`ogm_gateway_request_duration_seconds_bucket{%s,le="+Inf"} %d`, lbl, count)
		p(`ogm_gateway_request_duration_seconds_sum{%s} %g`, lbl, gm.sum)
		p(`ogm_gateway_request_duration_seconds_count{%s} %d`, lbl, count)
	}
	s.metrics.mu.Unlock()

	s.gw.mu.Lock()
	p("# HELP ogm_gateway_inflight Requests in flight per group.\n# TYPE ogm_gateway_inflight gauge")
	for _, g := range groups {
		if g.Spec.Networking != nil {
			p(`ogm_gateway_inflight{project="%s",group="%s"} %d`, promLabel(g.Project), promLabel(g.Name), s.gw.ginflight[g.ID]+s.gw.gwaiting[g.ID])
		}
	}
	s.gw.mu.Unlock()

	queues, _ := s.st.Queues("")
	p("# HELP ogm_queue_jobs Jobs by status.\n# TYPE ogm_queue_jobs gauge")
	for _, q := range queues {
		st, _ := s.st.QueueStats(q.ID)
		lbl := fmt.Sprintf(`project="%s",queue="%s"`, promLabel(q.Project), promLabel(q.Name))
		p(`ogm_queue_jobs{%s,status="pending"} %d`, lbl, st.Pending)
		p(`ogm_queue_jobs{%s,status="running"} %d`, lbl, st.Running)
		p(`ogm_queue_jobs{%s,status="succeeded"} %d`, lbl, st.Succeeded)
		p(`ogm_queue_jobs{%s,status="failed"} %d`, lbl, st.Failed)
	}
	users, _ := s.st.Users()
	p("# HELP ogm_user_credits Credit balance.\n# TYPE ogm_user_credits gauge")
	for _, u := range users {
		p(`ogm_user_credits{user="%s"} %g`, promLabel(u.Name), u.Credits)
	}
}

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) {
	if r.code == 0 {
		r.code = c
	}
	r.ResponseWriter.WriteHeader(c)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
