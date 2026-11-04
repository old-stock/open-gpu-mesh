package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

// Schedules run an action on a cron schedule (EventBridge Scheduler / Cloud Scheduler):
// submit a job to a queue, call an HTTP path on a container group, or set a group's replicas.

const schedulesSchema = `CREATE TABLE IF NOT EXISTS schedules(
  project TEXT NOT NULL, name TEXT NOT NULL, spec TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
  last_run INTEGER NOT NULL DEFAULT 0, last_status TEXT NOT NULL DEFAULT '', last_fire INTEGER NOT NULL DEFAULT 0,
  runs INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL, updated INTEGER NOT NULL, PRIMARY KEY(project, name));`

// ScheduleSpec is the user-facing definition.
type ScheduleSpec struct {
	Name     string         `json:"name"`
	Schedule string         `json:"schedule"`          // 5-field cron, server time zone
	Enabled  *bool          `json:"enabled,omitempty"` // default true
	Job      *ScheduleJob   `json:"job,omitempty"`
	HTTP     *ScheduleHTTP  `json:"http,omitempty"`
	Scale    *ScheduleScale `json:"scale,omitempty"`
	Comment  string         `json:"comment,omitempty"`
}

// ScheduleJob submits a job to a queue.
type ScheduleJob struct {
	Queue    string            `json:"queue"`
	Input    json.RawMessage   `json:"input,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ScheduleHTTP calls a container group through the gateway (no auth needed; internal).
type ScheduleHTTP struct {
	Group   string            `json:"group"`
	Method  string            `json:"method,omitempty"` // default POST
	Path    string            `json:"path,omitempty"`   // default /
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ScheduleScale sets a group's replica count.
type ScheduleScale struct {
	Group    string `json:"group"`
	Replicas int    `json:"replicas"`
}

// Schedule is the API representation.
type Schedule struct {
	ScheduleSpec
	Project    string     `json:"project"`
	NextRun    *time.Time `json:"next_run_time,omitempty"`
	LastRun    *time.Time `json:"last_run_time,omitempty"`
	LastStatus string     `json:"last_status,omitempty"`
	Runs       int        `json:"runs"`
	CreateTime time.Time  `json:"create_time"`
	UpdateTime time.Time  `json:"update_time"`
}

var schedNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (sp *ScheduleSpec) validate() map[string][]string {
	errs := map[string][]string{}
	if !schedNameRe.MatchString(sp.Name) {
		errs["name"] = append(errs["name"], "DNS label: lowercase letters, digits and -, max 63")
	}
	if _, err := cronParser.Parse(sp.Schedule); err != nil {
		errs["schedule"] = append(errs["schedule"], "invalid 5-field cron: "+err.Error())
	}
	n := 0
	if sp.Job != nil {
		n++
		if sp.Job.Queue == "" {
			errs["job.queue"] = append(errs["job.queue"], "required")
		}
	}
	if sp.HTTP != nil {
		n++
		if sp.HTTP.Group == "" {
			errs["http.group"] = append(errs["http.group"], "required")
		}
		if sp.HTTP.Path != "" && sp.HTTP.Path[0] != '/' {
			errs["http.path"] = append(errs["http.path"], "must start with /")
		}
	}
	if sp.Scale != nil {
		n++
		if sp.Scale.Group == "" {
			errs["scale.group"] = append(errs["scale.group"], "required")
		}
		if sp.Scale.Replicas < 0 || sp.Scale.Replicas > 1000 {
			errs["scale.replicas"] = append(errs["scale.replicas"], "0-1000")
		}
	}
	if n != 1 {
		errs["action"] = append(errs["action"], "exactly one of job, http or scale is required")
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func (s *Server) initSchedules() error {
	_, err := s.st.DB.Exec(schedulesSchema)
	return err
}

type schedRow struct {
	project           string
	spec              ScheduleSpec
	enabled           bool
	lastRun, lastFire int64
	lastStatus        string
	runs              int
	created, updated  int64
}

func (s *Server) loadSchedules(where string, args ...any) []schedRow {
	rows, err := s.st.DB.Query(`SELECT project,spec,enabled,last_run,last_fire,last_status,runs,created,updated FROM schedules `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []schedRow
	for rows.Next() {
		var r schedRow
		var spec string
		if rows.Scan(&r.project, &spec, &r.enabled, &r.lastRun, &r.lastFire, &r.lastStatus, &r.runs, &r.created, &r.updated) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(spec), &r.spec)
		out = append(out, r)
	}
	return out
}

func (r schedRow) api() Schedule {
	sc := Schedule{ScheduleSpec: r.spec, Project: r.project, LastStatus: r.lastStatus, Runs: r.runs,
		CreateTime: store.T(r.created), UpdateTime: store.T(r.updated)}
	en := r.enabled
	sc.Enabled = &en
	if r.lastRun > 0 {
		t := store.T(r.lastRun)
		sc.LastRun = &t
	}
	if en {
		if c, err := cronParser.Parse(r.spec.Schedule); err == nil {
			t := c.Next(time.Now()).UTC()
			sc.NextRun = &t
		}
	}
	return sc
}

// runSchedules fires schedules whose most recent cron time is newer than the last firing.
// A schedule created after its last fire time waits for the next one (no backfill).
func (s *Server) runSchedules() {
	now := time.Now()
	for _, r := range s.loadSchedules(`WHERE enabled=1`) {
		last, ok := lastFire(r.spec.Schedule, now)
		if !ok || last.UnixMilli() <= r.lastFire || last.UnixMilli() < r.created {
			continue
		}
		_, _ = s.st.DB.Exec(`UPDATE schedules SET last_fire=? WHERE project=? AND name=?`, last.UnixMilli(), r.project, r.spec.Name)
		go s.fireSchedule(r.project, r.spec, "schedule")
	}
}

func (s *Server) fireSchedule(project string, sp ScheduleSpec, why string) string {
	status := s.execSchedule(project, sp)
	_, _ = s.st.DB.Exec(`UPDATE schedules SET last_run=?, last_status=?, runs=runs+1 WHERE project=? AND name=?`,
		store.Now(), status, project, sp.Name)
	s.st.AddEvent(project, "", "", "", "schedule.run", fmt.Sprintf("Schedule %s ran (%s): %s", sp.Name, why, status))
	return status
}

func (s *Server) execSchedule(project string, sp ScheduleSpec) string {
	switch {
	case sp.Job != nil:
		q, err := s.st.Queue(project, sp.Job.Queue)
		if err != nil {
			return "error: queue " + sp.Job.Queue + " not found"
		}
		qs := queueDefaults(q.Settings)
		now := time.Now()
		input := string(sp.Job.Input)
		if input == "" {
			input = "{}"
		}
		mdm := map[string]string{"ogm_schedule": sp.Name}
		for k, v := range sp.Job.Metadata {
			mdm[k] = v
		}
		md, _ := json.Marshal(mdm)
		j := &store.JobRow{ID: store.NewID("job"), QueueID: q.ID, Status: api.JobPending, Input: json.RawMessage(input), Metadata: md,
			MaxAttempts: qs.MaxAttempts, TimeoutS: qs.JobTimeoutS, NextAttemptAt: now.UnixMilli(),
			Events: []api.JobEvent{{Action: "created", Time: now.UTC()}}, Created: now.UnixMilli(), Updated: now.UnixMilli()}
		s.jobMu.Lock()
		err = s.st.InsertJobs([]*store.JobRow{j})
		s.jobMu.Unlock()
		if err != nil {
			return "error: " + err.Error()
		}
		s.PokeJobs()
		return "ok: job " + j.ID
	case sp.HTTP != nil:
		g, err := s.st.Group(project, sp.HTTP.Group)
		if err != nil || g.Spec.Networking == nil {
			return "error: group " + sp.HTTP.Group + " not found or has no networking"
		}
		method, path := sp.HTTP.Method, sp.HTTP.Path
		if method == "" {
			method = http.MethodPost
		}
		if path == "" {
			path = "/"
		}
		timeout := time.Duration(g.Spec.Networking.ClientRequestTimeoutMS+g.Spec.Networking.ServerResponseTimeoutMS) * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, "http://ogm"+path, bytes.NewReader([]byte(sp.HTTP.Body)))
		for k, v := range sp.HTTP.Headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("OGM-Schedule", sp.Name)
		resp, err := (&gwRoundTripper{gw: s.gw, g: g}).RoundTrip(req)
		if err != nil {
			return "error: " + err.Error()
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Sprintf("error: HTTP %d", resp.StatusCode)
		}
		return fmt.Sprintf("ok: HTTP %d", resp.StatusCode)
	case sp.Scale != nil:
		s.mu.Lock()
		defer s.mu.Unlock()
		g, err := s.st.Group(project, sp.Scale.Group)
		if err != nil {
			return "error: group " + sp.Scale.Group + " not found"
		}
		if v := s.checkScaleQuota(g, sp.Scale.Replicas); v != nil {
			return "error: quota exceeded: " + strings.Join(v, "; ")
		}
		if g.DesiredReplicas != sp.Scale.Replicas {
			s.st.AddEvent(project, g.ID, "", "", "group.replicas",
				fmt.Sprintf("Container Group Desired Replica Count Updated from %d to %d (schedule %s)", g.DesiredReplicas, sp.Scale.Replicas, sp.Name))
			g.DesiredReplicas = sp.Scale.Replicas
			_ = s.st.SaveGroup(g)
			s.Poke()
		}
		return fmt.Sprintf("ok: %s replicas = %d", g.Name, sp.Scale.Replicas)
	}
	return "error: no action"
}

func (s *Server) registerScheduleRoutes(mux *http.ServeMux) {
	base := "/v1/projects/{project}/schedules"
	get := func(w http.ResponseWriter, p, name string) (schedRow, bool) {
		rs := s.loadSchedules(`WHERE project=? AND name=?`, p, name)
		if len(rs) == 0 {
			problem(w, http.StatusNotFound, "not found", "schedule "+name+" not found")
			return schedRow{}, false
		}
		return rs[0], true
	}
	upsert := func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p, name string, create bool) {
		var sp ScheduleSpec
		if !decode(w, r, &sp) {
			return
		}
		if name != "" {
			if sp.Name != "" && sp.Name != name {
				validationProblem(w, map[string][]string{"name": {"does not match the URL"}})
				return
			}
			sp.Name = name
		}
		if errs := sp.validate(); errs != nil {
			validationProblem(w, errs)
			return
		}
		en := sp.Enabled == nil || *sp.Enabled
		sp.Enabled = nil
		b, _ := json.Marshal(sp)
		now := store.Now()
		q := `INSERT INTO schedules(project,name,spec,enabled,created,updated) VALUES(?,?,?,?,?,?)`
		if !create {
			q += ` ON CONFLICT(project,name) DO UPDATE SET spec=excluded.spec, enabled=excluded.enabled, updated=excluded.updated`
		}
		if _, err := s.st.DB.Exec(q, p, sp.Name, string(b), en, now, now); err != nil {
			storeErr(w, err, "schedule")
			return
		}
		s.st.AddEvent(p, "", "", "", "schedule.updated", "Schedule "+sp.Name+" saved by "+u.Name)
		rs, _ := get(w, p, sp.Name)
		status := http.StatusOK
		if create {
			status = http.StatusCreated
		}
		writeJSON(w, status, rs.api())
	}
	mux.HandleFunc("GET "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		out := []Schedule{}
		for _, r := range s.loadSchedules(`WHERE project=? ORDER BY name`, p) {
			out = append(out, r.api())
		}
		writeJSON(w, http.StatusOK, api.List[Schedule]{Items: out})
	}))
	mux.HandleFunc("POST "+base, s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		upsert(w, r, u, p, "", true)
	}))
	mux.HandleFunc("PUT "+base+"/{name}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		upsert(w, r, u, p, r.PathValue("name"), false)
	}))
	mux.HandleFunc("GET "+base+"/{name}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		if rs, ok := get(w, p, r.PathValue("name")); ok {
			writeJSON(w, http.StatusOK, rs.api())
		}
	}))
	mux.HandleFunc("DELETE "+base+"/{name}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		res, _ := s.st.DB.Exec(`DELETE FROM schedules WHERE project=? AND name=?`, p, r.PathValue("name"))
		if n, _ := res.RowsAffected(); n == 0 {
			problem(w, http.StatusNotFound, "not found", "schedule not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST "+base+"/{name}/run", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		rs, ok := get(w, p, r.PathValue("name"))
		if !ok {
			return
		}
		status := s.fireSchedule(p, rs.spec, "manual by "+u.Name)
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
	}))
}
