package server

// Per-project quotas. An admin caps what one project may claim from the shared
// fleet: vCPUs, memory, GPUs and instances (counted at each group's peak
// replica count, so autoscalers and schedules can never exceed them), plus
// stored bytes (files, snapshots and S3 objects). Zero means unlimited. The quota
// stored for project "*" is the default for every project without its own, so
// creating more projects cannot be used to escape a limit.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const defaultQuotaKey = "*"

const quotaSchema = `CREATE TABLE IF NOT EXISTS project_quotas (project TEXT PRIMARY KEY, data TEXT NOT NULL, updated INTEGER NOT NULL);`

// Quota limits one project. Zero fields are unlimited.
type Quota struct {
	CPU       int `json:"cpu"`
	MemoryMB  int `json:"memory_mb"`
	GPUs      int `json:"gpus"`
	Instances int `json:"instances"`
	StorageMB int `json:"storage_mb"`
}

// QuotaUsage is a project's current claim against its quota.
type QuotaUsage struct {
	CPU          int   `json:"cpu"`
	MemoryMB     int   `json:"memory_mb"`
	GPUs         int   `json:"gpus"`
	Instances    int   `json:"instances"`
	StorageBytes int64 `json:"storage_bytes"`
}

// QuotaStatus is returned by GET /v1/projects/{p}/quota.
type QuotaStatus struct {
	Project    string     `json:"project"`
	Limits     Quota      `json:"limits"`
	Usage      QuotaUsage `json:"usage"`
	UpdateTime *time.Time `json:"update_time,omitempty"`
}

func (s *Server) initQuotas() error {
	_, err := s.st.DB.Exec(quotaSchema)
	return err
}

func (s *Server) quota(project string) (Quota, *time.Time) {
	var q Quota
	var data string
	var updated int64
	if err := s.st.DB.QueryRow(`SELECT data, updated FROM project_quotas WHERE project=?`, project).Scan(&data, &updated); err != nil {
		if project == defaultQuotaKey || s.st.DB.QueryRow(`SELECT data, updated FROM project_quotas WHERE project=?`, defaultQuotaKey).Scan(&data, &updated) != nil {
			return q, nil
		}
	}
	_ = json.Unmarshal([]byte(data), &q)
	t := store.T(updated)
	return q, &t
}

// peakReplicas is the most instances a spec can ever ask for.
func peakReplicas(sp api.ContainerGroupSpec) int {
	n := sp.Replicas
	if a := sp.QueueAutoscaler; a != nil && a.MaxReplicas > n {
		n = a.MaxReplicas
	}
	if a := sp.HTTPAutoscaler; a != nil && a.MaxReplicas > n {
		n = a.MaxReplicas
	}
	if sp.ScheduledScalingEnabled {
		for _, a := range sp.ScalingActions {
			if a.Replicas > n {
				n = a.Replicas
			}
		}
	}
	return n
}

// footprint is what one group claims at replicas instances.
func footprint(sp api.ContainerGroupSpec, replicas int) QuotaUsage {
	r := sp.Container.Resources
	gpus := r.GPUCount
	if gpus == 0 && (len(r.GPUClasses) > 0 || r.MinVRAMMB > 0) {
		gpus = 1
	}
	return QuotaUsage{CPU: r.CPU * replicas, MemoryMB: r.MemoryMB * replicas, GPUs: gpus * replicas, Instances: replicas}
}

func (u *QuotaUsage) add(o QuotaUsage) {
	u.CPU += o.CPU
	u.MemoryMB += o.MemoryMB
	u.GPUs += o.GPUs
	u.Instances += o.Instances
}

// computeUsage sums every group in the project at its peak, using override for
// the group named skip (nil = leave it out).
func (s *Server) computeUsage(project, skip string, override *QuotaUsage) QuotaUsage {
	var u QuotaUsage
	groups, _ := s.st.Groups(project)
	for _, g := range groups {
		if g.Name == skip {
			continue
		}
		peak := peakReplicas(g.Spec)
		if g.DesiredReplicas > peak {
			peak = g.DesiredReplicas
		}
		u.add(footprint(g.Spec, peak))
	}
	if override != nil {
		u.add(*override)
	}
	return u
}

func (s *Server) storageUsage(project string) int64 {
	var files, objs, parts int64
	_ = s.st.DB.QueryRow(`SELECT COALESCE(SUM(size),0) FROM files WHERE project=?`, project).Scan(&files)
	_ = s.st.DB.QueryRow(`SELECT COALESCE(SUM(o.size),0) FROM s3_objects o JOIN s3_buckets b ON b.name=o.bucket WHERE b.project=?`, project).Scan(&objs)
	_ = s.st.DB.QueryRow(`SELECT COALESCE(SUM(p.size),0) FROM s3_parts p JOIN s3_uploads u ON u.upload_id=p.upload_id JOIN s3_buckets b ON b.name=u.bucket WHERE b.project=?`, project).Scan(&parts)
	return files + objs + parts
}

// storageRemaining returns the bytes the project may still store, or -1 for unlimited.
func (s *Server) storageRemaining(project string) int64 {
	q, _ := s.quota(project)
	if q.StorageMB <= 0 {
		return -1
	}
	left := int64(q.StorageMB)<<20 - s.storageUsage(project)
	if left < 0 {
		left = 0
	}
	return left
}

// quotaViolations lists every limit that usage exceeds.
func quotaViolations(q Quota, u QuotaUsage) []string {
	var out []string
	chk := func(name string, used, limit int) {
		if limit > 0 && used > limit {
			out = append(out, fmt.Sprintf("%s: %d requested, project quota is %d", name, used, limit))
		}
	}
	chk("cpu", u.CPU, q.CPU)
	chk("memory_mb", u.MemoryMB, q.MemoryMB)
	chk("gpus", u.GPUs, q.GPUs)
	chk("instances", u.Instances, q.Instances)
	return out
}

// checkGroupQuota rejects a spec whose peak footprint, together with the rest of
// the project, exceeds the project's quota.
func (s *Server) checkGroupQuota(sp api.ContainerGroupSpec) []string {
	q, _ := s.quota(sp.Project)
	if q == (Quota{}) {
		return nil
	}
	mine := footprint(sp, peakReplicas(sp))
	return quotaViolations(q, s.computeUsage(sp.Project, sp.Name, &mine))
}

// checkScaleQuota is used when something other than a spec change (a schedule)
// sets a group's replica count.
func (s *Server) checkScaleQuota(g *store.GroupRow, replicas int) []string {
	q, _ := s.quota(g.Project)
	if q == (Quota{}) {
		return nil
	}
	peak := peakReplicas(g.Spec)
	if replicas > peak {
		peak = replicas
	}
	mine := footprint(g.Spec, peak)
	return quotaViolations(q, s.computeUsage(g.Project, g.Name, &mine))
}

func quotaProblem(w http.ResponseWriter, v []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(api.Problem{Type: "https://ogm.dev/errors/quota_exceeded", Title: "quota exceeded", Status: http.StatusForbidden,
		Detail: "this change would exceed the project's quota; ask a cluster admin to raise it", Errors: map[string][]string{"quota": v}})
}

func (s *Server) registerQuotaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{project}/quota", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		q, t := s.quota(p)
		usage := s.computeUsage(p, "", nil)
		usage.StorageBytes = s.storageUsage(p)
		writeJSON(w, http.StatusOK, QuotaStatus{Project: p, Limits: q, Usage: usage, UpdateTime: t})
	}))
	mux.HandleFunc("PUT /v1/projects/{project}/quota", s.admin(func(w http.ResponseWriter, r *http.Request, u *store.UserRow) {
		p := r.PathValue("project")
		var owner string
		if p == defaultQuotaKey {
			owner = "*"
		} else if err := s.st.DB.QueryRow(`SELECT owner_id FROM projects WHERE name=?`, p).Scan(&owner); err != nil {
			problem(w, http.StatusNotFound, "not found", "project "+p+" not found")
			return
		}
		var q Quota
		if !decode(w, r, &q) {
			return
		}
		if q.CPU < 0 || q.MemoryMB < 0 || q.GPUs < 0 || q.Instances < 0 || q.StorageMB < 0 {
			validationProblem(w, map[string][]string{"quota": {"limits must be >= 0 (0 = unlimited)"}})
			return
		}
		if _, err := s.st.DB.Exec(`INSERT INTO project_quotas(project,data,updated) VALUES(?,?,?)
			ON CONFLICT(project) DO UPDATE SET data=excluded.data, updated=excluded.updated`, p, store.JSON(q), store.Now()); err != nil {
			storeErr(w, err, "quota")
			return
		}
		s.st.AddEvent(p, "", "", "", "project.quota", fmt.Sprintf("Project quota set by %s: cpu=%d memory_mb=%d gpus=%d instances=%d storage_mb=%d",
			u.Name, q.CPU, q.MemoryMB, q.GPUs, q.Instances, q.StorageMB))
		qq, t := s.quota(p)
		usage := s.computeUsage(p, "", nil)
		usage.StorageBytes = s.storageUsage(p)
		writeJSON(w, http.StatusOK, QuotaStatus{Project: p, Limits: qq, Usage: usage, UpdateTime: t})
	}))
}
