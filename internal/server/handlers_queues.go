package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

func (s *Server) queueAPI(q *store.QueueRow) api.Queue {
	st, _ := s.st.QueueStats(q.ID)
	out := api.Queue{
		ID: q.ID, Name: q.Name, Project: q.Project, DisplayName: q.DisplayName, Description: q.Description,
		Settings: queueDefaults(q.Settings), Stats: st, Groups: []string{},
		CreateTime: store.T(q.Created), UpdateTime: store.T(q.Updated),
	}
	gs, _ := s.st.Groups(q.Project)
	for _, g := range gs {
		if g.Spec.QueueConnection != nil && g.Spec.QueueConnection.QueueName == q.Name {
			out.Groups = append(out.Groups, g.Name)
		}
	}
	return out
}

func (s *Server) registerQueueRoutes(mux *http.ServeMux) {
	base := "/v1/projects/{project}/queues"

	mux.HandleFunc("GET "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		qs, err := s.st.Queues(p)
		if err != nil {
			storeErr(w, err, "queues")
			return
		}
		out := []api.Queue{}
		for _, q := range qs {
			out = append(out, s.queueAPI(q))
		}
		writeJSON(w, http.StatusOK, api.List[api.Queue]{Items: out})
	}))

	mux.HandleFunc("POST "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		var body struct {
			Name        string            `json:"name"`
			DisplayName string            `json:"display_name"`
			Description string            `json:"description"`
			Settings    api.QueueSettings `json:"settings"`
		}
		if !decode(w, r, &body) {
			return
		}
		if !api.ValidName(body.Name) {
			validationProblem(w, map[string][]string{"name": {"must be a lowercase DNS label"}})
			return
		}
		if body.Settings.MaxAttempts > maxMaxAttempts {
			validationProblem(w, map[string][]string{"settings.max_attempts": {"must be <= 20"}})
			return
		}
		now := store.Now()
		q := &store.QueueRow{ID: store.NewID("q"), Project: p, Name: body.Name, DisplayName: body.DisplayName,
			Description: body.Description, Settings: body.Settings, Created: now, Updated: now}
		if q.DisplayName == "" {
			q.DisplayName = q.Name
		}
		if err := s.st.InsertQueue(q); err != nil {
			storeErr(w, err, "queue "+body.Name)
			return
		}
		writeJSON(w, http.StatusCreated, s.queueAPI(q))
	}))

	getQ := func(w http.ResponseWriter, r *http.Request, p string) (*store.QueueRow, bool) {
		q, err := s.st.Queue(p, r.PathValue("queue"))
		if err != nil {
			storeErr(w, err, "queue")
			return nil, false
		}
		return q, true
	}

	mux.HandleFunc("GET "+base+"/{queue}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		if q, ok := getQ(w, r, p); ok {
			writeJSON(w, http.StatusOK, s.queueAPI(q))
		}
	}))

	mux.HandleFunc("PATCH "+base+"/{queue}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, ok := getQ(w, r, p)
		if !ok {
			return
		}
		var body struct {
			DisplayName *string            `json:"display_name"`
			Description *string            `json:"description"`
			Settings    *api.QueueSettings `json:"settings"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.DisplayName != nil {
			q.DisplayName = *body.DisplayName
		}
		if body.Description != nil {
			q.Description = *body.Description
		}
		if body.Settings != nil {
			q.Settings = *body.Settings
		}
		if err := s.st.SaveQueue(q); err != nil {
			storeErr(w, err, "queue")
			return
		}
		writeJSON(w, http.StatusOK, s.queueAPI(q))
	}))

	mux.HandleFunc("DELETE "+base+"/{queue}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, ok := getQ(w, r, p)
		if !ok {
			return
		}
		qa := s.queueAPI(q)
		if len(qa.Groups) > 0 && r.URL.Query().Get("force") != "1" {
			problem(w, http.StatusConflict, "queue in use",
				"queue is attached to container groups "+store.JSON(qa.Groups)+"; detach them or pass ?force=1 (their queue_connection will be removed)")
			return
		}
		s.mu.Lock()
		for _, name := range qa.Groups {
			if g, err := s.st.Group(p, name); err == nil {
				g.Spec.QueueConnection = nil
				g.Spec.QueueAutoscaler = nil
				_ = s.st.SaveGroup(g)
				s.st.AddEvent(p, g.ID, "", "", "group.updated", "Queue connection removed (queue "+q.Name+" deleted)")
			}
		}
		s.mu.Unlock()
		s.jobMu.Lock()
		_, _ = s.st.DB.Exec(`DELETE FROM jobs WHERE queue_id=?`, q.ID)
		_, _ = s.st.DB.Exec(`DELETE FROM queues WHERE id=?`, q.ID)
		s.jobMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET "+base+"/{queue}/jobs", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, ok := getQ(w, r, p)
		if !ok {
			return
		}
		where, args := "queue_id=?", []any{q.ID}
		if st := r.URL.Query().Get("status"); st != "" {
			where += " AND status=?"
			args = append(args, st)
		}
		limit := qint(r, "limit", 100)
		if limit > 1000 {
			limit = 1000
		}
		order := "seq DESC"
		if r.URL.Query().Get("order") == "asc" {
			order = "seq"
		}
		jobs, err := s.st.JobsWhere(where, order, limit, args...)
		if err != nil {
			storeErr(w, err, "jobs")
			return
		}
		out := []api.Job{}
		for _, j := range jobs {
			out = append(out, s.jobAPI(j, q))
		}
		writeJSON(w, http.StatusOK, api.List[api.Job]{Items: out})
	}))

	// POST accepts a single JobPrototype or {"jobs":[...]} for batch submission.
	mux.HandleFunc("POST "+base+"/{queue}/jobs", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, ok := getQ(w, r, p)
		if !ok {
			return
		}
		var raw map[string]json.RawMessage
		if !decode(w, r, &raw) {
			return
		}
		var protos []api.JobPrototype
		batch := false
		if js, ok := raw["jobs"]; ok {
			batch = true
			if err := json.Unmarshal(js, &protos); err != nil {
				problem(w, http.StatusBadRequest, "invalid jobs array", err.Error())
				return
			}
		} else {
			var jp api.JobPrototype
			b, _ := json.Marshal(raw)
			_ = json.Unmarshal(b, &jp)
			protos = []api.JobPrototype{jp}
		}
		if len(protos) == 0 || len(protos) > 10000 {
			validationProblem(w, map[string][]string{"jobs": {"between 1 and 10000 jobs per request"}})
			return
		}
		qs := queueDefaults(q.Settings)
		now := time.Now()
		rows := make([]*store.JobRow, 0, len(protos))
		for _, jp := range protos {
			ma := jp.MaxAttempts
			if ma <= 0 {
				ma = qs.MaxAttempts
			}
			if ma > maxMaxAttempts {
				ma = maxMaxAttempts
			}
			to := jp.TimeoutS
			if to <= 0 {
				to = qs.JobTimeoutS
			}
			rows = append(rows, &store.JobRow{
				ID: store.NewID("job"), QueueID: q.ID, Status: api.JobPending, Input: rawOr(jp.Input, "{}"), Metadata: jp.Metadata,
				Webhook: jp.Webhook, MaxAttempts: ma, TimeoutS: to, NextAttemptAt: now.UnixMilli(),
				Events: []api.JobEvent{{Action: "created", Time: now.UTC()}}, Created: now.UnixMilli(), Updated: now.UnixMilli(),
			})
		}
		s.jobMu.Lock()
		err := s.st.InsertJobs(rows)
		s.jobMu.Unlock()
		if err != nil {
			storeErr(w, err, "jobs")
			return
		}
		s.PokeJobs()
		if batch {
			out := make([]api.Job, 0, len(rows))
			for _, j := range rows {
				out = append(out, s.jobAPI(j, q))
			}
			writeJSON(w, http.StatusCreated, api.List[api.Job]{Items: out})
			return
		}
		writeJSON(w, http.StatusCreated, s.jobAPI(rows[0], q))
	}))

	getJob := func(w http.ResponseWriter, r *http.Request, p string) (*store.QueueRow, *store.JobRow, bool) {
		q, ok := getQ(w, r, p)
		if !ok {
			return nil, nil, false
		}
		j, err := s.st.Job(r.PathValue("job"))
		if err != nil || j.QueueID != q.ID {
			problem(w, http.StatusNotFound, "not found", "job not found")
			return nil, nil, false
		}
		return q, j, true
	}

	mux.HandleFunc("GET "+base+"/{queue}/jobs/{job}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, j, ok := getJob(w, r, p)
		if !ok {
			return
		}
		// Long-poll: ?wait=30 blocks until the job reaches a terminal state (max 60s).
		if wait := qint(r, "wait", 0); wait > 0 {
			if wait > 60 {
				wait = 60
			}
			deadline := time.Now().Add(time.Duration(wait) * time.Second)
			for (j.Status == api.JobPending || j.Status == api.JobRunning) && time.Now().Before(deadline) {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
				if jj, err := s.st.Job(j.ID); err == nil {
					j = jj
				}
			}
		}
		writeJSON(w, http.StatusOK, s.jobAPI(j, q))
	}))

	mux.HandleFunc("DELETE "+base+"/{queue}/jobs/{job}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		_, j, ok := getJob(w, r, p)
		if !ok {
			return
		}
		if err := s.cancelJob(j); err != nil {
			problem(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
}
