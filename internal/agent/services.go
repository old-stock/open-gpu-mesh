package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/old-stock/open-gpu-mesh/internal/proto"
)

// ---------- IMDS ----------

func (a *Agent) startIMDS(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.IMDSListen)
	if err != nil {
		return err
	}
	a.imdsAddr = ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/i/{secret}/v1/{op}", a.handleIMDS)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (a *Agent) imdsURL(secret string) string {
	_, port, _ := net.SplitHostPort(a.imdsAddr)
	return fmt.Sprintf("http://%s/i/%s", net.JoinHostPort(a.rt.IMDSHost(), port), secret)
}

func imdsErr(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://ogm.dev/imds-errors/" + code, "code": code, "title": code, "status": status, "detail": detail})
}

func (a *Agent) handleIMDS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Metadata") != "true" {
		imdsErr(w, http.StatusBadRequest, "metadata_header_required", "set header 'Metadata: true'")
		return
	}
	secret := r.PathValue("secret")
	var in *instance
	a.mu.Lock()
	for _, x := range a.instances {
		if x.spec.IMDSSecret == secret {
			in = x
		}
	}
	a.mu.Unlock()
	if in == nil {
		imdsErr(w, http.StatusNotFound, "instance_not_found", "unknown instance")
		return
	}
	id := in.spec.ID
	ok := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		if v == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	forward := func(req proto.IMDSRequest) (*proto.IMDSReply, bool) {
		env, err := a.request(r.Context(), proto.TypeIMDS, req)
		if err != nil {
			imdsErr(w, http.StatusServiceUnavailable, "server_unavailable", err.Error())
			return nil, false
		}
		var rep proto.IMDSReply
		_ = json.Unmarshal(env.Data, &rep)
		return &rep, true
	}
	switch op := r.PathValue("op"); {
	case op == "status" && r.Method == http.MethodGet:
		in.mu.Lock()
		st := map[string]bool{"ready": in.ready, "started": in.started}
		in.mu.Unlock()
		ok(st)
	case op == "deletion-cost" && r.Method == http.MethodGet:
		in.mu.Lock()
		dc := in.deletionCost
		in.mu.Unlock()
		ok(map[string]int{"deletion_cost": dc})
	case op == "deletion-cost" && r.Method == http.MethodPut:
		var body struct {
			DeletionCost *int `json:"deletion_cost"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.DeletionCost == nil {
			imdsErr(w, http.StatusBadRequest, "invalid_body", `expected {"deletion_cost": <int>}`)
			return
		}
		if _, good := forward(proto.IMDSRequest{InstanceID: id, Op: proto.IMDSSetDeletionCost, DeletionCost: *body.DeletionCost}); !good {
			return
		}
		in.set(func() { in.deletionCost = *body.DeletionCost })
		ok(map[string]int{"deletion_cost": *body.DeletionCost})
	case op == "token" && r.Method == http.MethodGet:
		if rep, good := forward(proto.IMDSRequest{InstanceID: id, Op: proto.IMDSToken, Audience: r.URL.Query().Get("audience")}); good {
			ok(map[string]string{"jwt": rep.JWT})
		}
	case op == "metadata" && r.Method == http.MethodGet:
		if rep, good := forward(proto.IMDSRequest{InstanceID: id, Op: proto.IMDSMetadata}); good {
			ok(rep.Metadata)
		}
	case op == "termination-notice" && r.Method == http.MethodGet:
		in.mu.Lock()
		n := in.notice
		in.mu.Unlock()
		if n == nil {
			ok(map[string]any{"pending": false})
		} else {
			ok(n)
		}
	case op == "reallocate" && r.Method == http.MethodPost:
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
		if _, good := forward(proto.IMDSRequest{InstanceID: id, Op: proto.IMDSReallocate, Reason: body.Reason}); good {
			ok(nil)
		}
	case op == "restart" && r.Method == http.MethodPost:
		_, _ = forward(proto.IMDSRequest{InstanceID: id, Op: proto.IMDSRestart})
		select {
		case in.restartCh <- struct{}{}:
		default:
		}
		ok(nil)
	case op == "recreate" && r.Method == http.MethodPost:
		_, _ = forward(proto.IMDSRequest{InstanceID: id, Op: proto.IMDSRecreate})
		select {
		case in.recreateCh <- in.snapshotSpec():
		default:
		}
		ok(nil)
	default:
		imdsErr(w, http.StatusNotFound, "not_found", "unknown IMDS endpoint")
	}
}

// ---------- jobs ----------

var jobClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second}}

func (a *Agent) runJob(ja proto.JobAssign) {
	start := time.Now()
	res := proto.JobResult{JobID: ja.JobID, Attempt: ja.Attempt, InstanceID: ja.InstanceID}
	finish := func() {
		res.DurationMS = time.Since(start).Milliseconds()
		a.send(proto.TypeJobResult, res)
	}
	in := a.instance(ja.InstanceID)
	if in == nil || !in.isReady() {
		res.Error, res.Retryable = "instance not ready on node", true
		finish()
		return
	}
	ctr := in.container()
	addr := ""
	if ctr != nil {
		addr = ctr.HostAddr(ja.Port)
	}
	if addr == "" {
		res.Error, res.Retryable = fmt.Sprintf("port %d not published by container", ja.Port), true
		finish()
		return
	}
	timeout := time.Duration(ja.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	defer cancel()
	a.mu.Lock()
	a.jobs[ja.JobID] = cancel
	a.mu.Unlock()
	in.set(func() { in.inflight++ })
	defer func() {
		in.set(func() { in.inflight-- })
		a.mu.Lock()
		delete(a.jobs, ja.JobID)
		a.mu.Unlock()
	}()
	path := ja.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bytes.NewReader(ja.Input))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OGM-Job-Id", ja.JobID)
	req.Header.Set("OGM-Job-Attempt", strconv.Itoa(ja.Attempt))
	resp, err := jobClient.Do(req)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			res.Error = fmt.Sprintf("job timed out after %s", timeout)
		case errors.Is(ctx.Err(), context.Canceled):
			res.Cancelled, res.Error = true, "cancelled"
		default:
			res.Error = err.Error()
		}
		res.Retryable = !res.Cancelled
		finish()
		return
	}
	defer resp.Body.Close()
	max := ja.MaxOutputBytes
	if max <= 0 {
		max = 64 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		res.Error, res.Retryable = "reading response: "+err.Error(), true
		finish()
		return
	}
	res.HTTPStatus = resp.StatusCode
	if len(body) > max {
		res.Error, res.Retryable = fmt.Sprintf("output_too_large: response exceeded %d bytes", max), false
		finish()
		return
	}
	res.Output = toJSONOutput(body, resp.Header.Get("Content-Type"))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.Succeeded = true
	} else {
		res.Error = truncate(strings.TrimSpace(string(body)), 500)
		res.Retryable = resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429
	}
	finish()
}

func toJSONOutput(body []byte, ct string) json.RawMessage {
	if len(body) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	if utf8.Valid(body) {
		b, _ := json.Marshal(map[string]string{"text": string(body)})
		return b
	}
	b, _ := json.Marshal(map[string]string{"base64": base64.StdEncoding.EncodeToString(body), "content_type": ct})
	return b
}

// ---------- logs ----------

type logShipper struct {
	a   *Agent
	mu  sync.Mutex
	buf []proto.LogLine
	ch  chan struct{}
}

func newLogShipper(a *Agent) *logShipper { return &logShipper{a: a, ch: make(chan struct{}, 1)} }

func (l *logShipper) add(instanceID, stream, line string) {
	l.mu.Lock()
	if len(l.buf) < 20000 { // drop when disconnected for long
		l.buf = append(l.buf, proto.LogLine{InstanceID: instanceID, Stream: stream, Line: line, Time: time.Now().UTC()})
	}
	n := len(l.buf)
	l.mu.Unlock()
	if n >= 500 {
		select {
		case l.ch <- struct{}{}:
		default:
		}
	}
}

func (l *logShipper) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.ch:
		}
		l.a.mu.Lock()
		connected := l.a.codec != nil
		l.a.mu.Unlock()
		if !connected {
			continue
		}
		l.mu.Lock()
		batch := l.buf
		l.buf = nil
		l.mu.Unlock()
		for len(batch) > 0 {
			n := len(batch)
			if n > 1000 {
				n = 1000
			}
			l.a.send(proto.TypeLogs, proto.LogBatch{Entries: batch[:n]})
			batch = batch[n:]
		}
	}
}
