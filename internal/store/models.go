package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
)

// ---------- users ----------

// UserRow is a users row.
type UserRow struct {
	ID, Name, Role string
	Credits        float64
	Created        int64
}

const userCols = `id,name,role,credits,created`

func scanUser(sc interface{ Scan(...any) error }) (*UserRow, error) {
	u := &UserRow{}
	err := sc.Scan(&u.ID, &u.Name, &u.Role, &u.Credits, &u.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// CreateUser inserts a user and returns its plaintext API key.
func (s *Store) CreateUser(name, role string, credits float64) (*UserRow, string, error) {
	key := "ogm_" + NewSecret(20)
	u := &UserRow{ID: NewID("usr"), Name: name, Role: role, Credits: credits, Created: Now()}
	_, err := s.DB.Exec(`INSERT INTO users(id,name,role,api_key_hash,credits,created) VALUES(?,?,?,?,?,?)`,
		u.ID, u.Name, u.Role, Hash(key), u.Credits, u.Created)
	if err != nil {
		return nil, "", err
	}
	if credits != 0 {
		_, _ = s.DB.Exec(`INSERT INTO ledger(user_id,delta,reason,time) VALUES(?,?,?,?)`, u.ID, credits, "starter credits", Now())
	}
	return u, key, nil
}

// UserByKey resolves an API key.
func (s *Store) UserByKey(key string) (*UserRow, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE api_key_hash=?`, Hash(key)))
}

// UserByID loads a user.
func (s *Store) UserByID(id string) (*UserRow, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// UserByName loads a user by name.
func (s *Store) UserByName(name string) (*UserRow, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE name=?`, name))
}

// Users lists all users.
func (s *Store) Users() ([]*UserRow, error) {
	rows, err := s.DB.Query(`SELECT ` + userCols + ` FROM users ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserRow
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RotateUserKey issues a new API key.
func (s *Store) RotateUserKey(id string) (string, error) {
	key := "ogm_" + NewSecret(20)
	_, err := s.DB.Exec(`UPDATE users SET api_key_hash=? WHERE id=?`, Hash(key), id)
	return key, err
}

// AddCredits writes a ledger entry and updates the balance.
func (s *Store) AddCredits(userID string, delta float64, reason, ref string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET credits=credits+? WHERE id=?`, delta, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ledger(user_id,delta,reason,ref,time) VALUES(?,?,?,?,?)`, userID, delta, reason, ref, Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- nodes ----------

// NodeInfo is the JSON blob stored per node (latest hello/heartbeat).
type NodeInfo struct {
	Runtime         string            `json:"runtime"`
	Version         string            `json:"version"`
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	CPU             int               `json:"cpu"`
	MemoryMB        int               `json:"memory_mb"`
	DiskGB          int               `json:"disk_gb"`
	GPUs            []api.GPU         `json:"gpus"`
	Country         string            `json:"country"`
	Labels          map[string]string `json:"labels"`
	AllowPriorities []string          `json:"allow_priorities"`
	Window          string            `json:"window"`
	InWindow        bool              `json:"in_window"`
	CachedImages    []string          `json:"cached_images"`
}

// NodeRow is a nodes row.
type NodeRow struct {
	ID, Name, OwnerID, Status string
	Paused, AdminPaused       bool
	PauseReason               string
	Info                      NodeInfo
	LastSeen, ConnectedSince  sql.NullInt64
	OnlineSeconds             int64
	TrackedSeconds            int64
	Created                   int64
}

const nodeCols = `id,name,owner_id,status,paused,admin_paused,pause_reason,info,last_seen,connected_since,online_seconds,tracked_seconds,created`

func scanNode(sc interface{ Scan(...any) error }) (*NodeRow, error) {
	n := &NodeRow{}
	var info string
	err := sc.Scan(&n.ID, &n.Name, &n.OwnerID, &n.Status, &n.Paused, &n.AdminPaused, &n.PauseReason, &info,
		&n.LastSeen, &n.ConnectedSince, &n.OnlineSeconds, &n.TrackedSeconds, &n.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(info), &n.Info)
	return n, nil
}

// CreateNode registers a node and returns its plaintext join token.
func (s *Store) CreateNode(name, ownerID string) (*NodeRow, string, error) {
	token := "ogmn_" + NewSecret(24)
	n := &NodeRow{ID: NewID("node"), Name: name, OwnerID: ownerID, Status: "pending", Created: Now()}
	_, err := s.DB.Exec(`INSERT INTO nodes(id,name,owner_id,token_hash,status,created) VALUES(?,?,?,?,?,?)`,
		n.ID, n.Name, n.OwnerID, Hash(token), n.Status, n.Created)
	return n, token, err
}

// NodeByToken resolves a node token.
func (s *Store) NodeByToken(token string) (*NodeRow, error) {
	return scanNode(s.DB.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE token_hash=?`, Hash(token)))
}

// NodeByID loads a node.
func (s *Store) NodeByID(id string) (*NodeRow, error) {
	return scanNode(s.DB.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
}

// NodeByName loads a node by its name.
func (s *Store) NodeByName(name string) (*NodeRow, error) {
	return scanNode(s.DB.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE name=?`, name))
}

// Nodes lists nodes, optionally only online ones.
func (s *Store) Nodes(onlineOnly bool) ([]*NodeRow, error) {
	q := `SELECT ` + nodeCols + ` FROM nodes`
	if onlineOnly {
		q += ` WHERE status='online'`
	}
	q += ` ORDER BY created`
	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NodeRow
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpdateNodeInfo stores the latest info blob.
func (s *Store) UpdateNodeInfo(id string, info NodeInfo, paused bool, reason string) error {
	_, err := s.DB.Exec(`UPDATE nodes SET info=?, paused=?, pause_reason=?, last_seen=? WHERE id=?`, JSON(info), paused, reason, Now(), id)
	return err
}

// SetNodeStatus marks a node online/offline.
func (s *Store) SetNodeStatus(id, status string) error {
	now := Now()
	if status == "online" {
		_, err := s.DB.Exec(`UPDATE nodes SET status=?, last_seen=?, connected_since=? WHERE id=?`, status, now, now, id)
		return err
	}
	_, err := s.DB.Exec(`UPDATE nodes SET status=?, connected_since=NULL WHERE id=?`, status, id)
	return err
}

// ---------- GPU classes ----------

// GPUClassRow is a gpu_classes row.
type GPUClassRow struct {
	ID, Name       string
	VRAMMB         int
	CreditsPerHour float64
}

// EnsureGPUClass creates a class for a GPU model if missing.
func (s *Store) EnsureGPUClass(name string, vramMB int) (string, error) {
	id := api.Slugify(name)
	if id == "" {
		id = "unknown-gpu"
	}
	cph := float64(vramMB) / 24576.0
	if cph < 0.1 {
		cph = 0.1
	}
	cph = float64(int(cph*100+0.5)) / 100
	_, err := s.DB.Exec(`INSERT INTO gpu_classes(id,name,vram_mb,credits_per_hour) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, id, name, vramMB, cph)
	return id, err
}

// GPUClasses lists classes.
func (s *Store) GPUClasses() ([]*GPUClassRow, error) {
	rows, err := s.DB.Query(`SELECT id,name,vram_mb,credits_per_hour FROM gpu_classes ORDER BY vram_mb DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GPUClassRow
	for rows.Next() {
		g := &GPUClassRow{}
		if err := rows.Scan(&g.ID, &g.Name, &g.VRAMMB, &g.CreditsPerHour); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---------- container groups ----------

// AutoscaleState persists autoscaler bookkeeping.
type AutoscaleState struct {
	LastPoll       int64 `json:"last_poll"`
	LastScheduleAt int64 `json:"last_schedule_at"`
	ScheduledBase  *int  `json:"scheduled_base,omitempty"`
}

// GroupRow is a container_groups row.
type GroupRow struct {
	ID, Project, Name, OwnerID string
	Spec                       api.ContainerGroupSpec
	Version                    int
	Status                     string
	DesiredReplicas            int
	Autoscale                  AutoscaleState
	GatewayToken               string
	StartTime, FinishTime      sql.NullInt64
	Created, Updated           int64
}

const groupCols = `id,project,name,owner_id,spec,version,status,desired_replicas,autoscale_state,gateway_token,start_time,finish_time,created,updated`

func scanGroup(sc interface{ Scan(...any) error }) (*GroupRow, error) {
	g := &GroupRow{}
	var spec, as string
	err := sc.Scan(&g.ID, &g.Project, &g.Name, &g.OwnerID, &spec, &g.Version, &g.Status, &g.DesiredReplicas, &as,
		&g.GatewayToken, &g.StartTime, &g.FinishTime, &g.Created, &g.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(spec), &g.Spec)
	_ = json.Unmarshal([]byte(as), &g.Autoscale)
	return g, nil
}

// InsertGroup creates a group.
func (s *Store) InsertGroup(g *GroupRow) error {
	_, err := s.DB.Exec(`INSERT INTO container_groups(`+groupCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Project, g.Name, g.OwnerID, JSON(g.Spec), g.Version, g.Status, g.DesiredReplicas, JSON(g.Autoscale),
		g.GatewayToken, g.StartTime, g.FinishTime, g.Created, g.Updated)
	return err
}

// SaveGroup updates all mutable columns.
func (s *Store) SaveGroup(g *GroupRow) error {
	g.Updated = Now()
	_, err := s.DB.Exec(`UPDATE container_groups SET spec=?, version=?, status=?, desired_replicas=?, autoscale_state=?, gateway_token=?, start_time=?, finish_time=?, updated=? WHERE id=?`,
		JSON(g.Spec), g.Version, g.Status, g.DesiredReplicas, JSON(g.Autoscale), g.GatewayToken, g.StartTime, g.FinishTime, g.Updated, g.ID)
	return err
}

// Group loads a group by project/name.
func (s *Store) Group(project, name string) (*GroupRow, error) {
	return scanGroup(s.DB.QueryRow(`SELECT `+groupCols+` FROM container_groups WHERE project=? AND name=?`, project, name))
}

// GroupByID loads a group by id.
func (s *Store) GroupByID(id string) (*GroupRow, error) {
	return scanGroup(s.DB.QueryRow(`SELECT `+groupCols+` FROM container_groups WHERE id=?`, id))
}

// Groups lists groups; empty project lists all.
func (s *Store) Groups(project string) ([]*GroupRow, error) {
	q := `SELECT ` + groupCols + ` FROM container_groups`
	var args []any
	if project != "" {
		q += ` WHERE project=?`
		args = append(args, project)
	}
	q += ` ORDER BY created`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GroupRow
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteGroup removes a group row.
func (s *Store) DeleteGroup(id string) error {
	_, err := s.DB.Exec(`DELETE FROM container_groups WHERE id=?`, id)
	return err
}

// ---------- instances ----------

// InstanceRow is an instances row.
type InstanceRow struct {
	ID, GroupID, Project, NodeID string
	GPUIndexes                   []int
	Version                      int
	State                        string
	Ready, Started               bool
	DeletionCost                 int
	ExitCode                     sql.NullInt64
	Message                      string
	PullProgress                 float64
	Metrics                      api.InstanceMetrics
	StopReason                   string
	IMDSSecret                   string
	Created, Updated             int64
	AssignedAt, RunningAt        sql.NullInt64
	StopRequestedAt              sql.NullInt64
	BilledUntil                  sql.NullInt64
	ReplicaIndex                 int
}

const instCols = `id,group_id,project,node_id,gpu_indexes,version,state,ready,started,deletion_cost,exit_code,message,pull_progress,metrics,stop_reason,imds_secret,created,updated,assigned_at,running_at,stop_requested_at,billed_until,replica_index`

func scanInstance(sc interface{ Scan(...any) error }) (*InstanceRow, error) {
	i := &InstanceRow{}
	var gpus, metrics string
	err := sc.Scan(&i.ID, &i.GroupID, &i.Project, &i.NodeID, &gpus, &i.Version, &i.State, &i.Ready, &i.Started, &i.DeletionCost,
		&i.ExitCode, &i.Message, &i.PullProgress, &metrics, &i.StopReason, &i.IMDSSecret, &i.Created, &i.Updated,
		&i.AssignedAt, &i.RunningAt, &i.StopRequestedAt, &i.BilledUntil, &i.ReplicaIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(gpus), &i.GPUIndexes)
	_ = json.Unmarshal([]byte(metrics), &i.Metrics)
	return i, nil
}

// InsertInstance creates an instance row.
func (s *Store) InsertInstance(i *InstanceRow) error {
	_, err := s.DB.Exec(`INSERT INTO instances(`+instCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		i.ID, i.GroupID, i.Project, i.NodeID, JSON(i.GPUIndexes), i.Version, i.State, i.Ready, i.Started, i.DeletionCost,
		i.ExitCode, i.Message, i.PullProgress, JSON(i.Metrics), i.StopReason, i.IMDSSecret, i.Created, i.Updated,
		i.AssignedAt, i.RunningAt, i.StopRequestedAt, i.BilledUntil, i.ReplicaIndex)
	return err
}

// SaveInstance writes all mutable columns.
func (s *Store) SaveInstance(i *InstanceRow) error {
	i.Updated = Now()
	_, err := s.DB.Exec(`UPDATE instances SET node_id=?, gpu_indexes=?, version=?, state=?, ready=?, started=?, deletion_cost=?, exit_code=?, message=?, pull_progress=?, metrics=?, stop_reason=?, updated=?, assigned_at=?, running_at=?, stop_requested_at=?, billed_until=? WHERE id=?`,
		i.NodeID, JSON(i.GPUIndexes), i.Version, i.State, i.Ready, i.Started, i.DeletionCost, i.ExitCode, i.Message, i.PullProgress,
		JSON(i.Metrics), i.StopReason, i.Updated, i.AssignedAt, i.RunningAt, i.StopRequestedAt, i.BilledUntil, i.ID)
	return err
}

// Instance loads an instance.
func (s *Store) Instance(id string) (*InstanceRow, error) {
	return scanInstance(s.DB.QueryRow(`SELECT `+instCols+` FROM instances WHERE id=?`, id))
}

// InstanceBySecret resolves an IMDS secret.
func (s *Store) InstanceBySecret(secret string) (*InstanceRow, error) {
	return scanInstance(s.DB.QueryRow(`SELECT `+instCols+` FROM instances WHERE imds_secret=?`, secret))
}

// InstancesWhere runs a filtered instance query (where clause without WHERE).
func (s *Store) InstancesWhere(where string, args ...any) ([]*InstanceRow, error) {
	q := `SELECT ` + instCols + ` FROM instances`
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY created`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*InstanceRow
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// LiveInstances returns non-terminated instances of a group.
func (s *Store) LiveInstances(groupID string) ([]*InstanceRow, error) {
	return s.InstancesWhere(`group_id=? AND state!='terminated'`, groupID)
}

// ---------- queues ----------

// QueueRow is a queues row.
type QueueRow struct {
	ID, Project, Name, DisplayName, Description string
	Settings                                    api.QueueSettings
	Created, Updated                            int64
}

const queueCols = `id,project,name,display_name,description,settings,created,updated`

func scanQueue(sc interface{ Scan(...any) error }) (*QueueRow, error) {
	q := &QueueRow{}
	var st string
	err := sc.Scan(&q.ID, &q.Project, &q.Name, &q.DisplayName, &q.Description, &st, &q.Created, &q.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(st), &q.Settings)
	return q, nil
}

// InsertQueue creates a queue.
func (s *Store) InsertQueue(q *QueueRow) error {
	_, err := s.DB.Exec(`INSERT INTO queues(`+queueCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		q.ID, q.Project, q.Name, q.DisplayName, q.Description, JSON(q.Settings), q.Created, q.Updated)
	return err
}

// SaveQueue updates a queue.
func (s *Store) SaveQueue(q *QueueRow) error {
	q.Updated = Now()
	_, err := s.DB.Exec(`UPDATE queues SET display_name=?, description=?, settings=?, updated=? WHERE id=?`,
		q.DisplayName, q.Description, JSON(q.Settings), q.Updated, q.ID)
	return err
}

// Queue loads a queue by project/name.
func (s *Store) Queue(project, name string) (*QueueRow, error) {
	return scanQueue(s.DB.QueryRow(`SELECT `+queueCols+` FROM queues WHERE project=? AND name=?`, project, name))
}

// QueueByID loads a queue by id.
func (s *Store) QueueByID(id string) (*QueueRow, error) {
	return scanQueue(s.DB.QueryRow(`SELECT `+queueCols+` FROM queues WHERE id=?`, id))
}

// Queues lists queues in a project ("" = all).
func (s *Store) Queues(project string) ([]*QueueRow, error) {
	q := `SELECT ` + queueCols + ` FROM queues`
	var args []any
	if project != "" {
		q += ` WHERE project=?`
		args = append(args, project)
	}
	rows, err := s.DB.Query(q+` ORDER BY created`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*QueueRow
	for rows.Next() {
		x, err := scanQueue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// QueueStats counts jobs per status.
func (s *Store) QueueStats(queueID string) (api.QueueStats, error) {
	var st api.QueueStats
	rows, err := s.DB.Query(`SELECT status, COUNT(*) FROM jobs WHERE queue_id=? GROUP BY status`, queueID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return st, err
		}
		switch k {
		case api.JobPending:
			st.Pending = n
		case api.JobRunning:
			st.Running = n
		case api.JobSucceeded:
			st.Succeeded = n
		case api.JobFailed:
			st.Failed = n
		case api.JobCancelled:
			st.Cancelled = n
		}
	}
	return st, rows.Err()
}

// ---------- jobs ----------

// JobRow is a jobs row.
type JobRow struct {
	ID, QueueID, Status      string
	Input, Output, Metadata  json.RawMessage
	Webhook                  string
	Attempts, MaxAttempts    int
	TimeoutS                 int
	NextAttemptAt            int64
	InstanceID, LastInstance string
	Error                    string
	Events                   []api.JobEvent
	Seq                      int64
	Created, Updated         int64
}

const jobCols = `id,queue_id,status,input,output,metadata,webhook,attempts,max_attempts,timeout_s,next_attempt_at,instance_id,last_instance_id,error,events,seq,created,updated`

func scanJob(sc interface{ Scan(...any) error }) (*JobRow, error) {
	j := &JobRow{}
	var in string
	var out, meta sql.NullString
	var ev string
	err := sc.Scan(&j.ID, &j.QueueID, &j.Status, &in, &out, &meta, &j.Webhook, &j.Attempts, &j.MaxAttempts, &j.TimeoutS,
		&j.NextAttemptAt, &j.InstanceID, &j.LastInstance, &j.Error, &ev, &j.Seq, &j.Created, &j.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.Input = json.RawMessage(in)
	if out.Valid {
		j.Output = json.RawMessage(out.String)
	}
	if meta.Valid {
		j.Metadata = json.RawMessage(meta.String)
	}
	_ = json.Unmarshal([]byte(ev), &j.Events)
	return j, nil
}

func nullRaw(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

// InsertJobs inserts jobs in one transaction.
func (s *Store) InsertJobs(jobs []*JobRow) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	_ = tx.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM jobs`).Scan(&seq)
	for _, j := range jobs {
		seq++
		j.Seq = seq
		if _, err := tx.Exec(`INSERT INTO jobs(`+jobCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			j.ID, j.QueueID, j.Status, string(j.Input), nullRaw(j.Output), nullRaw(j.Metadata), j.Webhook, j.Attempts, j.MaxAttempts,
			j.TimeoutS, j.NextAttemptAt, j.InstanceID, j.LastInstance, j.Error, JSON(j.Events), j.Seq, j.Created, j.Updated); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveJob updates a job.
func (s *Store) SaveJob(j *JobRow) error {
	j.Updated = Now()
	_, err := s.DB.Exec(`UPDATE jobs SET status=?, output=?, attempts=?, next_attempt_at=?, instance_id=?, last_instance_id=?, error=?, events=?, updated=? WHERE id=?`,
		j.Status, nullRaw(j.Output), j.Attempts, j.NextAttemptAt, j.InstanceID, j.LastInstance, j.Error, JSON(j.Events), j.Updated, j.ID)
	return err
}

// Job loads a job.
func (s *Store) Job(id string) (*JobRow, error) {
	return scanJob(s.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

// JobsWhere runs a filtered job query.
func (s *Store) JobsWhere(where, order string, limit int, args ...any) ([]*JobRow, error) {
	q := `SELECT ` + jobCols + ` FROM jobs`
	if where != "" {
		q += ` WHERE ` + where
	}
	if order == "" {
		order = "seq"
	}
	q += ` ORDER BY ` + order
	if limit > 0 {
		q += ` LIMIT ` + itoa(limit)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------- logs ----------

// InsertLogs stores log lines for instances (instance->group/project/node resolved by caller).
func (s *Store) InsertLogs(nodeID string, lines []proto.LogLine, meta func(instanceID string) (project, groupID string, ok bool)) error {
	if len(lines) == 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT INTO logs(project,group_id,instance_id,node_id,stream,line,time) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, l := range lines {
		p, g, ok := meta(l.InstanceID)
		if !ok {
			continue
		}
		line := l.Line
		if len(line) > 16<<10 {
			line = line[:16<<10] + "…"
		}
		if _, err := st.Exec(p, g, l.InstanceID, nodeID, l.Stream, strings.TrimRight(line, "\r\n"), l.Time.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveAssessment stores a node's latest onboarding audit.
func (s *Store) SaveAssessment(nodeID string, a api.Assessment) error {
	_, err := s.DB.Exec(`INSERT INTO node_assessments(node_id,data,time) VALUES(?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET data=excluded.data, time=excluded.time`, nodeID, JSON(a), Now())
	return err
}

// Assessment loads a node's latest onboarding audit.
func (s *Store) Assessment(nodeID string) (*api.Assessment, error) {
	var data string
	err := s.DB.QueryRow(`SELECT data FROM node_assessments WHERE node_id=?`, nodeID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a := &api.Assessment{}
	return a, json.Unmarshal([]byte(data), a)
}
