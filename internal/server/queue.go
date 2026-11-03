package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/store"
	"github.com/old-stock/open-gpu-mesh/internal/webhook"
)

const (
	defaultMaxAttempts = 3
	defaultJobTimeoutS = 3600
	defaultMaxOutput   = 64 << 20
	maxMaxAttempts     = 20
)

func queueDefaults(qs api.QueueSettings) api.QueueSettings {
	if qs.MaxAttempts <= 0 {
		qs.MaxAttempts = defaultMaxAttempts
	}
	if qs.MaxAttempts > maxMaxAttempts {
		qs.MaxAttempts = maxMaxAttempts
	}
	if qs.RetryBackoffBaseS <= 0 {
		qs.RetryBackoffBaseS = 2
	}
	if qs.RetryBackoffMaxS <= 0 {
		qs.RetryBackoffMaxS = 300
	}
	if qs.JobTimeoutS <= 0 {
		qs.JobTimeoutS = defaultJobTimeoutS
	}
	if qs.MaxOutputBytes <= 0 {
		qs.MaxOutputBytes = defaultMaxOutput
	}
	if qs.RetentionHours <= 0 {
		qs.RetentionHours = 24 * 7
	}
	return qs
}

func backoff(qs api.QueueSettings, attempt int) time.Duration {
	d := time.Duration(qs.RetryBackoffBaseS) * time.Second
	for i := 1; i < attempt && d < time.Duration(qs.RetryBackoffMaxS)*time.Second; i++ {
		d *= 2
	}
	if max := time.Duration(qs.RetryBackoffMaxS) * time.Second; d > max {
		d = max
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

type slot struct {
	inst  *store.InstanceRow
	group *store.GroupRow
	free  int
}

// dispatchJobs assigns due pending jobs to ready instances with free slots.
func (s *Server) dispatchJobs() {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	now := store.Now()

	// Server-side safety net: running jobs that blew way past their timeout.
	stale, _ := s.st.JobsWhere(`status='running' AND updated < ? - (timeout_s + 90) * 1000`, "", 0, now)
	for _, j := range stale {
		s.jobAttemptFailedLocked(j, j.Attempts, j.InstanceID, "job timed out (no result from agent)", true)
	}

	queues, err := s.st.Queues("")
	if err != nil || len(queues) == 0 {
		return
	}
	groups, _ := s.st.Groups("")
	byQueue := map[string][]*store.GroupRow{}
	for _, g := range groups {
		if g.Spec.QueueConnection == nil || (g.Status != api.GroupRunning && g.Status != api.GroupDeploying) {
			continue
		}
		k := g.Project + "/" + g.Spec.QueueConnection.QueueName
		byQueue[k] = append(byQueue[k], g)
	}
	busy := map[string]int{}
	rows, err := s.st.DB.Query(`SELECT instance_id, COUNT(*) FROM jobs WHERE status='running' GROUP BY instance_id`)
	if err == nil {
		for rows.Next() {
			var id string
			var n int
			if rows.Scan(&id, &n) == nil {
				busy[id] = n
			}
		}
		rows.Close()
	}
	online := s.hub.online()

	for _, q := range queues {
		gs := byQueue[q.Project+"/"+q.Name]
		if len(gs) == 0 {
			continue
		}
		var slots []*slot
		total := 0
		for _, g := range gs {
			insts, _ := s.st.InstancesWhere(`group_id=? AND state='running' AND ready=1`, g.ID)
			for _, in := range insts {
				if online[in.NodeID] == nil {
					continue
				}
				free := g.Spec.QueueConnection.Concurrency - busy[in.ID]
				if free > 0 {
					slots = append(slots, &slot{inst: in, group: g, free: free})
					total += free
				}
			}
		}
		if total == 0 {
			continue
		}
		jobs, _ := s.st.JobsWhere(`queue_id=? AND status='pending' AND next_attempt_at<=?`, "seq", total, q.ID, now)
		qs := queueDefaults(q.Settings)
		for _, j := range jobs {
			// Prefer a different instance than the last failed attempt, then the most free slots.
			better := func(a, b *slot) bool {
				ad, bd := a.inst.ID != j.LastInstance, b.inst.ID != j.LastInstance
				if ad != bd {
					return ad
				}
				return a.free > b.free
			}
			var pick *slot
			for _, sl := range slots {
				if sl.free > 0 && (pick == nil || better(sl, pick)) {
					pick = sl
				}
			}
			if pick == nil {
				break
			}
			pick.free--
			j.Status = api.JobRunning
			j.Attempts++
			j.InstanceID = pick.inst.ID
			j.Events = append(j.Events, api.JobEvent{Action: "started", Time: time.Now().UTC(), InstanceID: pick.inst.ID, Attempt: j.Attempts})
			_ = s.st.SaveJob(j)
			ns := online[pick.inst.NodeID]
			ns.mu.Lock()
			ns.jobs[j.ID] = j.Attempts
			ns.mu.Unlock()
			qc := pick.group.Spec.QueueConnection
			ok := s.hub.send(pick.inst.NodeID, proto.TypeJobAssign, proto.JobAssign{
				JobID: j.ID, Attempt: j.Attempts, InstanceID: pick.inst.ID, Port: qc.Port, Path: qc.Path,
				Input: j.Input, TimeoutS: j.TimeoutS, MaxOutputBytes: qs.MaxOutputBytes,
			})
			if !ok {
				ns.mu.Lock()
				delete(ns.jobs, j.ID)
				ns.mu.Unlock()
				j.Status = api.JobPending
				j.Attempts--
				j.InstanceID = ""
				j.Events = j.Events[:len(j.Events)-1]
				_ = s.st.SaveJob(j)
			}
		}
	}
}

// onJobResult handles an agent's report for a job attempt.
func (s *Server) onJobResult(jr proto.JobResult) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, err := s.st.Job(jr.JobID)
	if err != nil || j.Status != api.JobRunning || j.Attempts != jr.Attempt || j.InstanceID != jr.InstanceID {
		return // stale
	}
	if jr.Succeeded {
		j.Status = api.JobSucceeded
		j.Output = jr.Output
		j.Error = ""
		j.Events = append(j.Events, api.JobEvent{Action: "succeeded", Time: time.Now().UTC(), InstanceID: jr.InstanceID, Attempt: jr.Attempt})
		_ = s.st.SaveJob(j)
		s.enqueueJobWebhook(j)
		s.PokeJobs()
		return
	}
	msg := jr.Error
	if jr.HTTPStatus != 0 {
		msg = fmt.Sprintf("HTTP %d: %s", jr.HTTPStatus, jr.Error)
		if len(jr.Output) > 0 {
			j.Output = jr.Output
		}
	}
	retry := jr.Retryable
	if !retry && jr.HTTPStatus >= 400 && jr.HTTPStatus < 500 {
		if q, err := s.st.QueueByID(j.QueueID); err == nil && q.Settings.RetryOn4xx {
			retry = true
		}
	}
	s.jobAttemptFailedLocked(j, jr.Attempt, jr.InstanceID, msg, retry)
	s.PokeJobs()
}

// jobAttemptFailed records a failed attempt and schedules a retry if allowed.
func (s *Server) jobAttemptFailed(jobID string, attempt int, instanceID, msg string, retryable bool) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, err := s.st.Job(jobID)
	if err != nil {
		return
	}
	s.jobAttemptFailedLocked(j, attempt, instanceID, msg, retryable)
}

func (s *Server) jobAttemptFailedLocked(j *store.JobRow, attempt int, instanceID, msg string, retryable bool) {
	if j.Status != api.JobRunning || j.Attempts != attempt {
		return
	}
	if instanceID == "" {
		instanceID = j.InstanceID
	}
	j.LastInstance = instanceID
	j.InstanceID = ""
	j.Error = msg
	now := time.Now().UTC()
	j.Events = append(j.Events, api.JobEvent{Action: "attempt_failed", Time: now, InstanceID: instanceID, Attempt: attempt, Error: msg})
	if retryable && j.Attempts < j.MaxAttempts {
		q, _ := s.st.QueueByID(j.QueueID)
		qs := queueDefaults(api.QueueSettings{})
		if q != nil {
			qs = queueDefaults(q.Settings)
		}
		j.Status = api.JobPending
		j.NextAttemptAt = now.Add(backoff(qs, j.Attempts)).UnixMilli()
		_ = s.st.SaveJob(j)
		return
	}
	j.Status = api.JobFailed
	j.Events = append(j.Events, api.JobEvent{Action: "failed", Time: now, Attempt: attempt, Error: msg})
	_ = s.st.SaveJob(j)
	s.enqueueJobWebhook(j)
}

// cancelJob cancels a pending or running job.
func (s *Server) cancelJob(j *store.JobRow) error {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, err := s.st.Job(j.ID)
	if err != nil {
		return err
	}
	switch j.Status {
	case api.JobPending:
	case api.JobRunning:
		if in, err := s.st.Instance(j.InstanceID); err == nil {
			s.hub.send(in.NodeID, proto.TypeJobCancel, proto.JobCancel{JobID: j.ID})
			if ns := s.hub.get(in.NodeID); ns != nil {
				ns.mu.Lock()
				delete(ns.jobs, j.ID)
				ns.mu.Unlock()
			}
		}
	default:
		return fmt.Errorf("job is already %s", j.Status)
	}
	j.Status = api.JobCancelled
	j.Events = append(j.Events, api.JobEvent{Action: "cancelled", Time: time.Now().UTC(), InstanceID: j.InstanceID, Attempt: j.Attempts})
	j.InstanceID = ""
	_ = s.st.SaveJob(j)
	s.enqueueJobWebhook(j)
	return nil
}

func (s *Server) jobAPI(j *store.JobRow, q *store.QueueRow) api.Job {
	out := api.Job{
		ID: j.ID, Status: j.Status, Input: j.Input, Output: j.Output, Metadata: j.Metadata, Webhook: j.Webhook,
		Attempts: j.Attempts, MaxAttempts: j.MaxAttempts, TimeoutS: j.TimeoutS, Error: j.Error, InstanceID: j.InstanceID,
		Events: j.Events, CreateTime: store.T(j.Created), UpdateTime: store.T(j.Updated),
	}
	if out.Events == nil {
		out.Events = []api.JobEvent{}
	}
	if q != nil {
		out.Queue, out.Project = q.Name, q.Project
	}
	if j.Status == api.JobPending && j.NextAttemptAt > store.Now() {
		t := store.T(j.NextAttemptAt)
		out.NextAttemptAt = &t
	}
	return out
}

// ---------- webhooks ----------

func (s *Server) enqueueJobWebhook(j *store.JobRow) {
	if j.Webhook == "" {
		return
	}
	q, _ := s.st.QueueByID(j.QueueID)
	data := s.jobAPI(j, q)
	payload := map[string]any{
		"type":      "queue.job." + j.Status,
		"timestamp": time.Now().UTC(),
		"data": map[string]any{
			"id": data.ID, "status": data.Status, "input": data.Input, "output": data.Output, "metadata": data.Metadata,
			"events": data.Events, "error": data.Error, "attempts": data.Attempts, "create_time": data.CreateTime,
			"update_time": data.UpdateTime, "webhook": data.Webhook, "project_name": data.Project, "queue_name": data.Queue,
		},
	}
	_, _ = s.st.DB.Exec(`INSERT INTO webhook_deliveries(id,url,payload,next_attempt_at,created) VALUES(?,?,?,?,?)`,
		store.NewID("msg"), j.Webhook, store.JSON(payload), store.Now(), store.Now())
}

var webhookBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour}

func (s *Server) deliverWebhooks() {
	type d struct {
		id, url, payload string
		attempts         int
	}
	rows, err := s.st.DB.Query(`SELECT id,url,payload,attempts FROM webhook_deliveries WHERE done=0 AND next_attempt_at<=? ORDER BY next_attempt_at LIMIT 50`, store.Now())
	if err != nil {
		return
	}
	var due []d
	for rows.Next() {
		var x d
		if rows.Scan(&x.id, &x.url, &x.payload, &x.attempts) == nil {
			due = append(due, x)
		}
	}
	rows.Close()
	if len(due) == 0 {
		return
	}
	secret, _ := s.st.GetSetting("webhook_secret")
	var wg sync.WaitGroup
	for _, x := range due {
		wg.Add(1)
		go func(x d) {
			defer wg.Done()
			err := s.postWebhook(secret, x.id, x.url, []byte(x.payload))
			if err == nil {
				_, _ = s.st.DB.Exec(`UPDATE webhook_deliveries SET done=1, attempts=attempts+1 WHERE id=?`, x.id)
				return
			}
			n := x.attempts + 1
			if n >= len(webhookBackoff)+1 {
				_, _ = s.st.DB.Exec(`UPDATE webhook_deliveries SET done=1, attempts=?, last_error=? WHERE id=?`, n, err.Error(), x.id)
				return
			}
			_, _ = s.st.DB.Exec(`UPDATE webhook_deliveries SET attempts=?, last_error=?, next_attempt_at=? WHERE id=?`,
				n, err.Error(), time.Now().Add(webhookBackoff[n-1]).UnixMilli(), x.id)
		}(x)
	}
	wg.Wait()
}

func (s *Server) postWebhook(secret, id, url string, body []byte) error {
	h, err := webhook.Headers(secret, id, time.Now(), body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = h
	req.Header.Set("User-Agent", "ogm-webhooks/"+api.Version)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// marshal helper used by handlers.
func rawOr(v json.RawMessage, def string) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage(def)
	}
	return v
}
