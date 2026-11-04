package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
)

type stopReq struct {
	grace  time.Duration
	reason string
	event  string
}

// instance runs one container through its lifecycle on this node.
type instance struct {
	a *Agent

	mu           sync.Mutex
	spec         proto.InstanceSpec
	state        string
	ready        bool
	started      bool
	exitCode     *int
	message      string
	pullProgress float64
	ctr          Container
	deletionCost int
	notice       *terminationNotice
	inflight     int
	metrics      api.InstanceMetrics

	stopCh     chan stopReq
	restartCh  chan struct{}
	recreateCh chan proto.InstanceSpec
	done       chan struct{}
}

type terminationNotice struct {
	Pending  bool      `json:"pending"`
	Deadline time.Time `json:"deadline"`
	Reason   string    `json:"reason"`
}

func newInstance(a *Agent, spec proto.InstanceSpec) *instance {
	return &instance{
		a: a, spec: spec, state: api.InstDownloading, deletionCost: spec.DeletionCost,
		stopCh: make(chan stopReq, 1), restartCh: make(chan struct{}, 1), recreateCh: make(chan proto.InstanceSpec, 1),
		done: make(chan struct{}),
	}
}

func (i *instance) report(event string) {
	i.mu.Lock()
	r := i.reportLocked()
	r.Event = event
	i.mu.Unlock()
	i.a.send(proto.TypeInstanceStatus, r)
}

func (i *instance) reportLocked() proto.InstanceReport {
	m := i.metrics
	m.InflightJobs = i.inflight
	return proto.InstanceReport{
		ID: i.spec.ID, State: i.state, Ready: i.ready, Started: i.started, ExitCode: i.exitCode, Message: i.message,
		PullProgress: i.pullProgress, Metrics: m, Version: i.spec.Version,
	}
}

func (i *instance) set(f func()) {
	i.mu.Lock()
	f()
	i.mu.Unlock()
}

func (i *instance) container() Container {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ctr
}

func (i *instance) isReady() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ready && i.state == api.InstRunning
}

// run drives the instance until it terminates.
func (i *instance) run(ctx context.Context) {
	defer close(i.done)
	defer i.a.removeInstance(i.spec.ID, i)
	restarts := 0
	pull := true
	var adopted Container
	if ad, ok := i.a.rt.(Adopter); ok {
		adopted, _ = ad.Adopt(ctx, i.spec.ID, i.spec.Version)
	}
	for {
		spec := i.snapshotSpec()
		spec.Image = i.a.reg.Rewrite(spec.Image) // registry.ogm/... -> 127.0.0.1:PORT/...
		if pull && adopted == nil {
			switch policy := spec.PullPolicy; {
			case policy == "never" || (policy == "if_not_present" && i.a.rt.HasImage(ctx, spec.Image)):
				pull = false
			}
		}
		if pull && adopted == nil {
			i.set(func() {
				i.state, i.ready, i.started, i.pullProgress, i.message = api.InstDownloading, false, false, 0, ""
			})
			i.report("")
			var lastSent time.Time
			err := i.a.rt.Pull(ctx, spec.Image, spec.RegistryAuth, func(p float64) {
				i.set(func() { i.pullProgress = p })
				if time.Since(lastSent) > 3*time.Second {
					lastSent = time.Now()
					i.report("")
				}
			})
			if err != nil && ctx.Err() == nil && i.a.rt.HasImage(ctx, spec.Image) {
				// Registry unreachable or image only built locally: run what we have.
				i.a.log.Warn("image pull failed; using local copy", "image", spec.Image, "err", err)
				i.report("Image Pull Failed, Using Local Copy (" + truncate(err.Error(), 120) + ")")
				err = nil
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				i.terminate(nil, "image pull failed: "+err.Error(), "Instance Failed (image pull: "+truncate(err.Error(), 200)+")")
				return
			}
		}
		links, err := i.openLinks(ctx, spec)
		if err != nil {
			i.terminate(nil, "links: "+err.Error(), "Instance Failed (link listener: "+truncate(err.Error(), 200)+")")
			return
		}
		var ctr Container
		if adopted != nil {
			ctr, adopted = adopted, nil
			i.a.log.Info("re-attached to running container", "id", spec.ID)
			i.report("Instance Re-attached (agent restarted, workload kept running)")
		} else {
			i.set(func() { i.state = api.InstCreating; i.pullProgress = 100 })
			i.report("")
			ctr, err = i.a.rt.Create(ctx, i.startSpec(spec, links))
		}
		if err != nil {
			links.close()
			if ctx.Err() != nil {
				return
			}
			i.terminate(nil, "create failed: "+err.Error(), "Instance Failed (create: "+truncate(err.Error(), 200)+")")
			return
		}
		hasStartup := spec.StartupProbe != nil
		hasReadiness := spec.ReadinessProbe != nil
		i.set(func() {
			i.ctr = ctr
			i.state = api.InstRunning
			i.started = !hasStartup
			i.ready = !hasStartup && !hasReadiness
			i.exitCode = nil
		})
		i.report("")

		rctx, rcancel := context.WithCancel(ctx)
		go ctr.Logs(rctx, func(stream, line string) { i.a.logs.add(spec.ID, stream, line) })
		go i.statsLoop(rctx, ctr)
		probeFail := make(chan string, 2)
		go i.probes(rctx, ctr, spec, probeFail)
		exitCh := make(chan int, 1)
		go func() {
			code, err := ctr.Wait(rctx)
			if err == nil {
				exitCh <- code
			}
		}()

		var next string // "restart", "recreate", "stop"
		var stop stopReq
		select {
		case code := <-exitCh:
			c := code
			i.set(func() { i.exitCode = &c; i.ready = false })
			ev := fmt.Sprintf("Instance Exited:%d", code)
			switch {
			case code == 0:
				ev += " (Exited Successfully)"
			case code == 137:
				ev += " (Likely Out of System Memory)"
			default:
				ev += " (Error)"
			}
			if spec.RestartPolicy == "never" || (spec.RestartPolicy == "on_failure" && code == 0) {
				rcancel()
				links.close()
				_ = ctr.Remove(context.Background())
				i.terminate(&c, "exited", ev)
				return
			}
			i.report(ev)
			next = "restart"
		case why := <-probeFail:
			i.report(why)
			next = "restart"
		case stop = <-i.stopCh:
			next = "stop"
		case <-i.restartCh:
			i.report("Instance Restarted")
			next = "restart-now"
		case ns := <-i.recreateCh:
			i.set(func() { i.spec = ns })
			next = "recreate"
		case <-ctx.Done():
			next = "shutdown"
		}
		rcancel()
		links.close()
		if next == "shutdown" && i.a.Detached() {
			return // leave the container running for the next agent process to adopt
		}
		i.set(func() { i.ready = false })
		grace := 10 * time.Second
		if next == "stop" {
			grace = stop.grace
		}
		_ = ctr.Stop(context.Background(), grace)
		_ = ctr.Remove(context.Background())
		i.set(func() { i.ctr = nil })
		switch next {
		case "stop":
			i.terminate(nil, stop.reason, stop.event)
			return
		case "shutdown":
			return
		case "recreate":
			pull, restarts = true, 0
			continue
		case "restart-now":
			pull = false
			continue
		}
		// Crash-loop backoff: 1s, 2s, 4s ... 60s.
		pull = false
		restarts++
		d := time.Second << min(restarts-1, 6)
		if d > time.Minute {
			d = time.Minute
		}
		i.set(func() { i.message = fmt.Sprintf("restarting in %s (restart #%d)", d, restarts) })
		select {
		case <-time.After(d):
		case stop = <-i.stopCh:
			i.terminate(nil, stop.reason, stop.event)
			return
		case <-ctx.Done():
			return
		}
		i.report("Instance Restarted")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (i *instance) snapshotSpec() proto.InstanceSpec {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.spec
}

func (i *instance) startSpec(spec proto.InstanceSpec, links *linkSet) StartSpec {
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = links.expand(v)
	}
	env["OGM_IMDS_URL"] = i.a.imdsURL(spec.IMDSSecret)
	if g := i.a.gpuNames(spec.GPUIndexes); g != "" {
		env["OGM_GPU_NAME"] = g
	}
	env["OGM_NODE_COUNTRY"] = i.a.cfg.Country
	return StartSpec{
		Name: spec.ID, Image: spec.Image, Command: spec.Command, Env: env, Resources: spec.Resources,
		GPUIndexes: spec.GPUIndexes, Ports: spec.Ports, Volumes: spec.Volumes,
		Labels: map[string]string{"dev.ogm.instance": spec.ID, "dev.ogm.group": spec.Group, "dev.ogm.project": spec.Project,
			"dev.ogm.version": strconv.Itoa(spec.Version)},
	}
}

func (i *instance) terminate(code *int, reason, event string) {
	i.set(func() {
		i.state = api.InstTerminated
		i.ready = false
		if code != nil {
			i.exitCode = code
		}
		i.message = reason
	})
	i.report(event)
}

// stop requests a graceful stop with a termination notice.
func (i *instance) stop(grace time.Duration, reason, event string) {
	i.set(func() {
		i.notice = &terminationNotice{Pending: true, Deadline: time.Now().Add(grace), Reason: reason}
		i.ready = false
	})
	select {
	case i.stopCh <- stopReq{grace: grace, reason: reason, event: event}:
	default:
	}
}

func (i *instance) statsLoop(ctx context.Context, ctr Container) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cpu, mem, err := ctr.Stats(ctx)
			if err == nil {
				i.set(func() { i.metrics.CPUPercent, i.metrics.MemoryMB = cpu, mem })
			}
		}
	}
}

// ---------- probes ----------

type probeState struct {
	p                   *api.Probe
	successes, failures int
}

func defaults(p *api.Probe, period, timeout, success, failure int) api.Probe {
	q := *p
	if q.PeriodSeconds <= 0 {
		q.PeriodSeconds = period
	}
	if q.TimeoutSeconds <= 0 {
		q.TimeoutSeconds = timeout
	}
	if q.SuccessThreshold <= 0 {
		q.SuccessThreshold = success
	}
	if q.FailureThreshold <= 0 {
		q.FailureThreshold = failure
	}
	return q
}

func runProbe(ctx context.Context, ctr Container, p api.Probe) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	switch {
	case p.HTTP != nil:
		addr := ctr.HostAddr(p.HTTP.Port)
		if addr == "" {
			return false
		}
		scheme := "http"
		if p.HTTP.Scheme == "https" {
			scheme = "https"
		}
		path := p.HTTP.Path
		if path == "" || path[0] != '/' {
			path = "/" + path
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+path, nil)
		if err != nil {
			return false
		}
		for k, v := range p.HTTP.Headers {
			req.Header.Set(k, v)
		}
		resp, err := probeClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	case p.TCP != nil:
		addr := ctr.HostAddr(p.TCP.Port)
		if addr == "" {
			return false
		}
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	case p.Exec != nil:
		code, err := ctr.Exec(ctx, p.Exec.Command)
		return err == nil && code == 0
	}
	return true
}

var probeClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// probes runs startup, then readiness + liveness probes, updating state.
func (i *instance) probes(ctx context.Context, ctr Container, spec proto.InstanceSpec, fail chan<- string) {
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	if sp := spec.StartupProbe; sp != nil {
		p := defaults(sp, 3, 10, 2, 15)
		if !sleep(time.Duration(p.InitialDelaySeconds) * time.Second) {
			return
		}
		st := probeState{p: &p}
		for {
			if runProbe(ctx, ctr, p) {
				st.successes++
				st.failures = 0
				if st.successes >= p.SuccessThreshold {
					break
				}
			} else {
				st.failures++
				st.successes = 0
				if st.failures >= p.FailureThreshold {
					if ctx.Err() == nil {
						fail <- "Instance Interrupted (Startup Probe Failure)"
					}
					return
				}
			}
			if !sleep(time.Duration(p.PeriodSeconds) * time.Second) {
				return
			}
		}
		i.set(func() {
			i.started = true
			if spec.ReadinessProbe == nil {
				i.ready = true
			}
		})
		i.report("Instance Startup Probe Passed")
	}
	var wg sync.WaitGroup
	if rp := spec.ReadinessProbe; rp != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := defaults(rp, 1, 1, 1, 3)
			if !sleep(time.Duration(p.InitialDelaySeconds) * time.Second) {
				return
			}
			st := probeState{p: &p}
			for {
				ok := runProbe(ctx, ctr, p)
				if ctx.Err() != nil {
					return
				}
				changed := false
				if ok {
					st.successes++
					st.failures = 0
					if st.successes >= p.SuccessThreshold {
						i.mu.Lock()
						if !i.ready && i.notice == nil {
							i.ready, changed = true, true
						}
						i.mu.Unlock()
					}
				} else {
					st.failures++
					st.successes = 0
					if st.failures >= p.FailureThreshold {
						i.mu.Lock()
						if i.ready {
							i.ready, changed = false, true
						}
						i.mu.Unlock()
					}
				}
				if changed {
					i.report("")
				}
				if !sleep(time.Duration(p.PeriodSeconds) * time.Second) {
					return
				}
			}
		}()
	}
	if lp := spec.LivenessProbe; lp != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := defaults(lp, 10, 30, 1, 3)
			if !sleep(time.Duration(p.InitialDelaySeconds) * time.Second) {
				return
			}
			failures := 0
			for {
				if runProbe(ctx, ctr, p) {
					failures = 0
				} else if ctx.Err() == nil {
					failures++
					if failures >= p.FailureThreshold {
						fail <- "Instance Interrupted (Liveness Probe Failure)"
						return
					}
				}
				if !sleep(time.Duration(p.PeriodSeconds) * time.Second) {
					return
				}
			}
		}()
	}
	wg.Wait()
}
