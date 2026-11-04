// Package api holds the public JSON types shared by the server, agent and CLI.
package api

import (
	"encoding/json"
	"time"
)

// Version is the ogm release version. Overridden at build time via -ldflags.
var Version = "0.1.0-dev"

// ---------- Container groups ----------

// Priority tiers, highest first.
const (
	PriorityHigh   = "high"
	PriorityMedium = "medium"
	PriorityLow    = "low"
	PriorityBatch  = "batch"
)

// PriorityRank returns a sortable rank (higher = more important).
func PriorityRank(p string) int {
	switch p {
	case PriorityHigh:
		return 4
	case PriorityMedium:
		return 3
	case PriorityLow:
		return 2
	case PriorityBatch:
		return 1
	}
	return 0
}

// ContainerGroupSpec is the user-editable definition of a container group.
type ContainerGroupSpec struct {
	Name                    string            `json:"name" yaml:"name"`
	DisplayName             string            `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	Project                 string            `json:"project,omitempty" yaml:"project,omitempty"`
	Replicas                int               `json:"replicas" yaml:"replicas"`
	Priority                string            `json:"priority,omitempty" yaml:"priority,omitempty"`
	RestartPolicy           string            `json:"restart_policy,omitempty" yaml:"restart_policy,omitempty"`
	AutostartPolicy         *bool             `json:"autostart_policy,omitempty" yaml:"autostart_policy,omitempty"`
	CountryCodes            []string          `json:"country_codes,omitempty" yaml:"country_codes,omitempty"`
	NodeSelector            map[string]string `json:"node_selector,omitempty" yaml:"node_selector,omitempty"`
	Container               ContainerSpec     `json:"container" yaml:"container"`
	Networking              *Networking       `json:"networking,omitempty" yaml:"networking,omitempty"`
	StartupProbe            *Probe            `json:"startup_probe,omitempty" yaml:"startup_probe,omitempty"`
	ReadinessProbe          *Probe            `json:"readiness_probe,omitempty" yaml:"readiness_probe,omitempty"`
	LivenessProbe           *Probe            `json:"liveness_probe,omitempty" yaml:"liveness_probe,omitempty"`
	QueueConnection         *QueueConnection  `json:"queue_connection,omitempty" yaml:"queue_connection,omitempty"`
	QueueAutoscaler         *QueueAutoscaler  `json:"queue_autoscaler,omitempty" yaml:"queue_autoscaler,omitempty"`
	HTTPAutoscaler          *HTTPAutoscaler   `json:"http_autoscaler,omitempty" yaml:"http_autoscaler,omitempty"`
	ScalingActions          []ScalingAction   `json:"scaling_actions,omitempty" yaml:"scaling_actions,omitempty"`
	ScheduledScalingEnabled bool              `json:"scheduled_scaling_enabled,omitempty" yaml:"scheduled_scaling_enabled,omitempty"`
	Readme                  string            `json:"readme,omitempty" yaml:"readme,omitempty"`
	// AllowUntrustedNodes lets a group that uses secrets run on nodes not trusted for
	// its project (their operators can read the secrets), and lets its instances get
	// hub-audience identity tokens there.
	AllowUntrustedNodes bool `json:"allow_untrusted_nodes,omitempty" yaml:"allow_untrusted_nodes,omitempty"`
}

// ContainerSpec describes the container image and its resource needs.
type ContainerSpec struct {
	Image           string            `json:"image" yaml:"image"`
	Command         []string          `json:"command,omitempty" yaml:"command,omitempty"`
	Env             map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	RegistryAuth    *RegistryAuth     `json:"registry_auth,omitempty" yaml:"registry_auth,omitempty"`
	ImagePullPolicy string            `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty"` // always|if_not_present|never
	Resources       Resources         `json:"resources" yaml:"resources"`
	// Ports are extra container ports reachable by other groups in the project through links
	// ({{link:GROUP:PORT}} in their env). Never exposed publicly.
	Ports []int `json:"ports,omitempty" yaml:"ports,omitempty"`
	// Volumes are persistent per-replica volumes. A replica with volumes is pinned to the node
	// that holds its data and waits for that node if it is offline.
	Volumes []Volume `json:"volumes,omitempty" yaml:"volumes,omitempty"`
}

// Volume is a persistent named volume mounted into every replica (one volume per replica).
type Volume struct {
	Name  string `json:"name" yaml:"name"`
	Mount string `json:"mount" yaml:"mount"`
}

// HTTPAutoscaler scales a gateway-enabled group on concurrent requests ("serverless"):
// requests to a group at zero replicas start one and wait for it.
type HTTPAutoscaler struct {
	MinReplicas           int `json:"min_replicas" yaml:"min_replicas"`
	MaxReplicas           int `json:"max_replicas" yaml:"max_replicas"`
	TargetConcurrency     int `json:"target_concurrency,omitempty" yaml:"target_concurrency,omitempty"`             // in-flight requests per replica (default 10)
	StableWindowSeconds   int `json:"stable_window_seconds,omitempty" yaml:"stable_window_seconds,omitempty"`       // averaging window (default 60)
	ScaleDownDelaySeconds int `json:"scale_down_delay_seconds,omitempty" yaml:"scale_down_delay_seconds,omitempty"` // low demand this long before scaling down (default 300)
}

// RegistryAuth holds private registry credentials.
type RegistryAuth struct {
	Server   string `json:"server,omitempty" yaml:"server,omitempty"`
	Username string `json:"username" yaml:"username"`
	Password string `json:"password" yaml:"password"`
}

// Resources requested per instance.
type Resources struct {
	CPU        int      `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	MemoryMB   int      `json:"memory_mb,omitempty" yaml:"memory_mb,omitempty"`
	GPUClasses []string `json:"gpu_classes,omitempty" yaml:"gpu_classes,omitempty"`
	MinVRAMMB  int      `json:"min_vram_mb,omitempty" yaml:"min_vram_mb,omitempty"`
	GPUCount   int      `json:"gpu_count,omitempty" yaml:"gpu_count,omitempty"`
	StorageGB  int      `json:"storage_gb,omitempty" yaml:"storage_gb,omitempty"`
	ShmMB      int      `json:"shm_mb,omitempty" yaml:"shm_mb,omitempty"`
	// GPUShared lets several shared instances use one GPU, each budgeted at MinVRAMMB.
	GPUShared bool `json:"gpu_shared,omitempty" yaml:"gpu_shared,omitempty"`
}

// WantsGPU reports whether the resources require a GPU.
func (r Resources) WantsGPU() bool {
	return r.GPUCount > 0 || len(r.GPUClasses) > 0 || r.MinVRAMMB > 0
}

// GPUs returns the number of GPUs needed (0 for CPU-only).
func (r Resources) GPUs() int {
	if !r.WantsGPU() {
		return 0
	}
	if r.GPUCount <= 0 {
		return 1
	}
	return r.GPUCount
}

// Load balancer algorithms.
const (
	LBRoundRobin = "round_robin"
	LBLeastConn  = "least_number_of_connections"
)

// Networking configures the container gateway. All fields are mutable.
type Networking struct {
	Port                    int      `json:"port" yaml:"port"`
	Auth                    bool     `json:"auth" yaml:"auth"`
	LoadBalancer            string   `json:"load_balancer,omitempty" yaml:"load_balancer,omitempty"`
	SingleConnectionLimit   bool     `json:"single_connection_limit,omitempty" yaml:"single_connection_limit,omitempty"`
	MaxConcurrency          int      `json:"max_concurrency,omitempty" yaml:"max_concurrency,omitempty"` // hard cap of in-flight requests per instance; extra requests wait (0 = unlimited)
	CustomDomains           []string `json:"custom_domains,omitempty" yaml:"custom_domains,omitempty"`   // hostnames (CNAME/A to the hub) routed to this group; TLS via the hub's --tls
	ClientRequestTimeoutMS  int      `json:"client_request_timeout_ms,omitempty" yaml:"client_request_timeout_ms,omitempty"`
	ServerResponseTimeoutMS int      `json:"server_response_timeout_ms,omitempty" yaml:"server_response_timeout_ms,omitempty"`
}

// Probe is a health check run by the agent next to the container.
type Probe struct {
	HTTP                *HTTPProbe `json:"http,omitempty" yaml:"http,omitempty"`
	TCP                 *TCPProbe  `json:"tcp,omitempty" yaml:"tcp,omitempty"`
	Exec                *ExecProbe `json:"exec,omitempty" yaml:"exec,omitempty"`
	InitialDelaySeconds int        `json:"initial_delay_seconds,omitempty" yaml:"initial_delay_seconds,omitempty"`
	PeriodSeconds       int        `json:"period_seconds,omitempty" yaml:"period_seconds,omitempty"`
	TimeoutSeconds      int        `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	SuccessThreshold    int        `json:"success_threshold,omitempty" yaml:"success_threshold,omitempty"`
	FailureThreshold    int        `json:"failure_threshold,omitempty" yaml:"failure_threshold,omitempty"`
}

// HTTPProbe performs a GET; 2xx/3xx is success.
type HTTPProbe struct {
	Path    string            `json:"path" yaml:"path"`
	Port    int               `json:"port" yaml:"port"`
	Scheme  string            `json:"scheme,omitempty" yaml:"scheme,omitempty"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// TCPProbe succeeds if a TCP connection can be opened.
type TCPProbe struct {
	Port int `json:"port" yaml:"port"`
}

// ExecProbe runs a command in the container; exit 0 is success.
type ExecProbe struct {
	Command []string `json:"command" yaml:"command"`
}

// QueueConnection attaches a group to a job queue. Mutable at any time.
type QueueConnection struct {
	QueueName   string `json:"queue_name" yaml:"queue_name"`
	Port        int    `json:"port" yaml:"port"`
	Path        string `json:"path,omitempty" yaml:"path,omitempty"`
	Concurrency int    `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
}

// QueueAutoscaler scales replicas from queue depth (SaladCloud-compatible semantics).
type QueueAutoscaler struct {
	MinReplicas           int `json:"min_replicas" yaml:"min_replicas"`
	MaxReplicas           int `json:"max_replicas" yaml:"max_replicas"`
	DesiredQueueLength    int `json:"desired_queue_length" yaml:"desired_queue_length"`
	PollingPeriod         int `json:"polling_period,omitempty" yaml:"polling_period,omitempty"`
	MaxUpscalePerMinute   int `json:"max_upscale_per_minute,omitempty" yaml:"max_upscale_per_minute,omitempty"`
	MaxDownscalePerMinute int `json:"max_downscale_per_minute,omitempty" yaml:"max_downscale_per_minute,omitempty"`
}

// ScalingAction sets replicas on a cron schedule.
type ScalingAction struct {
	Schedule string `json:"schedule" yaml:"schedule"`
	Replicas int    `json:"replicas" yaml:"replicas"`
}

// Container group statuses.
const (
	GroupPending   = "pending"
	GroupDeploying = "deploying"
	GroupRunning   = "running"
	GroupStopped   = "stopped"
	GroupFailed    = "failed"
)

// ContainerGroup is the API representation of a deployed group.
type ContainerGroup struct {
	ContainerGroupSpec
	ID              string      `json:"id"`
	Owner           string      `json:"owner"`
	Version         int         `json:"version"`
	CurrentState    GroupState  `json:"current_state"`
	DesiredReplicas int         `json:"desired_replicas"`
	GatewayURL      string      `json:"gateway_url,omitempty"`
	GatewayToken    string      `json:"gateway_token,omitempty"` // shareable gateway-only credential (when networking.auth)
	PendingChange   bool        `json:"pending_change"`
	CreateTime      time.Time   `json:"create_time"`
	UpdateTime      time.Time   `json:"update_time"`
	Instances       []Instance  `json:"instances,omitempty"`
	QueueStats      *QueueStats `json:"queue_stats,omitempty"`
}

// GroupState summarises the live state.
type GroupState struct {
	Status               string              `json:"status"`
	Description          string              `json:"description,omitempty"`
	StartTime            *time.Time          `json:"start_time,omitempty"`
	FinishTime           *time.Time          `json:"finish_time,omitempty"`
	InstanceStatusCounts InstanceStatusCount `json:"instance_status_counts"`
}

// InstanceStatusCount counts instances by state.
type InstanceStatusCount struct {
	Allocating  int `json:"allocating_count"`
	Downloading int `json:"downloading_count"`
	Creating    int `json:"creating_count"`
	Running     int `json:"running_count"`
	Ready       int `json:"ready_count"`
	Stopping    int `json:"stopping_count"`
}

// ---------- Instances ----------

// Instance states.
const (
	InstAllocating  = "allocating"
	InstDownloading = "downloading"
	InstCreating    = "creating"
	InstRunning     = "running"
	InstStopping    = "stopping"
	InstTerminated  = "terminated"
)

// Instance is one replica placed on a node.
type Instance struct {
	ID              string          `json:"id"`
	Group           string          `json:"container_group"`
	Project         string          `json:"project"`
	NodeID          string          `json:"node_id,omitempty"`
	NodeName        string          `json:"node_name,omitempty"`
	GPUName         string          `json:"gpu_name,omitempty"`
	GPUIndexes      []int           `json:"gpu_indexes,omitempty"`
	Country         string          `json:"country,omitempty"`
	State           string          `json:"state"`
	Ready           bool            `json:"ready"`
	Started         bool            `json:"started"`
	DeletionCost    int             `json:"deletion_cost"`
	Version         int             `json:"version"`
	ReplicaIndex    int             `json:"replica_index"`
	ExitCode        *int            `json:"exit_code,omitempty"`
	Message         string          `json:"message,omitempty"`
	PullingProgress float64         `json:"pulling_progress,omitempty"`
	Metrics         InstanceMetrics `json:"metrics"`
	StopReason      string          `json:"stop_reason,omitempty"`
	CreateTime      time.Time       `json:"create_time"`
	UpdateTime      time.Time       `json:"update_time"`
	RunningTime     *time.Time      `json:"running_time,omitempty"`
}

// InstanceMetrics are refreshed by agent heartbeats.
type InstanceMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryMB      float64 `json:"memory_usage_mb"`
	GPUUtil       float64 `json:"gpu_util_percent"`
	GPUMemoryMB   float64 `json:"gpu_memory_mb"`
	GPUTempC      float64 `json:"gpu_temp_c"`
	InflightJobs  int     `json:"inflight_jobs"`
	InflightConns int     `json:"inflight_gateway_requests"`
}

// ---------- Nodes ----------

// GPU describes one GPU on a node.
type GPU struct {
	Index     int     `json:"index"`
	UUID      string  `json:"uuid,omitempty"`
	Name      string  `json:"name"`
	Class     string  `json:"class,omitempty"`
	VRAMMB    int     `json:"vram_mb"`
	UtilPct   float64 `json:"util_percent"`
	MemUsedMB int     `json:"mem_used_mb"`
	TempC     float64 `json:"temp_c"`
	Unified   bool    `json:"unified_memory,omitempty"` // VRAM is shared system RAM (GB10, Jetson)
}

// Node is a contributor machine running the agent.
type Node struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Owner           string            `json:"owner"`
	Status          string            `json:"status"` // online|offline|pending
	Paused          bool              `json:"paused"`
	PauseReason     string            `json:"pause_reason,omitempty"`
	Runtime         string            `json:"runtime,omitempty"`
	Version         string            `json:"version,omitempty"`
	OS              string            `json:"os,omitempty"`
	Arch            string            `json:"arch,omitempty"`
	CPU             int               `json:"cpu"`
	MemoryMB        int               `json:"memory_mb"`
	DiskGB          int               `json:"disk_gb"`
	GPUs            []GPU             `json:"gpus"`
	Country         string            `json:"country,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	AllowPriorities []string          `json:"allow_priorities,omitempty"`
	Window          string            `json:"window,omitempty"`
	Instances       int               `json:"instances"`
	LastSeen        *time.Time        `json:"last_seen,omitempty"`
	ConnectedSince  *time.Time        `json:"connected_since,omitempty"`
	UptimeRatio     float64           `json:"uptime_ratio"`
	CreateTime      time.Time         `json:"create_time"`
	Assessment      *Assessment       `json:"assessment,omitempty"` // latest onboarding audit
	Trusted         bool              `json:"trusted"`              // admin-marked: may run any project's secrets
}

// Assessment is the onboarding audit of a machine (`ogm assess`): what it has, how fast it
// is, how well connected it is, and what the owner should keep for themselves.
type Assessment struct {
	Time            time.Time         `json:"time"`
	Version         string            `json:"version"`
	DurationSec     float64           `json:"duration_seconds"`
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	Kernel          string            `json:"kernel,omitempty"`
	Environment     string            `json:"environment,omitempty"` // wsl2 | container | vm | ""
	Laptop          bool              `json:"laptop,omitempty"`
	UptimeHours     float64           `json:"uptime_hours,omitempty"`
	CPU             AssessCPU         `json:"cpu"`
	Memory          AssessMemory      `json:"memory"`
	Disks           []AssessDisk      `json:"disks"`
	GPUs            []GPU             `json:"gpus"`
	GPUDriver       string            `json:"gpu_driver,omitempty"`
	Runtime         AssessRuntime     `json:"runtime"`
	Network         AssessNetwork     `json:"network"`
	Profiles        []string          `json:"profiles"`
	DetectedApps    []string          `json:"detected_apps,omitempty"`
	Tier            string            `json:"tier"` // anchor | compute | burst | unsuitable
	TierReasons     []string          `json:"tier_reasons,omitempty"`
	Suggested       AssessLimits      `json:"suggested"`
	Recommendations []Recommendation  `json:"recommendations"`
	Errors          map[string]string `json:"errors,omitempty"` // check name -> why it was skipped/failed
}

// AssessCPU describes the processor and a short hashing benchmark.
type AssessCPU struct {
	Model      string  `json:"model,omitempty"`
	Cores      int     `json:"cores"`   // physical cores (= threads if unknown)
	Threads    int     `json:"threads"` // logical CPUs
	Load1      float64 `json:"load1,omitempty"`
	SingleMBps float64 `json:"single_thread_mbps,omitempty"` // SHA-256 throughput, one thread
	MultiMBps  float64 `json:"multi_thread_mbps,omitempty"`  // SHA-256 throughput, all threads
}

// AssessMemory describes RAM.
type AssessMemory struct {
	TotalMB     int `json:"total_mb"`
	AvailableMB int `json:"available_mb,omitempty"`
	SwapMB      int `json:"swap_mb,omitempty"`
}

// AssessDisk describes a filesystem that matters to ogm (root, Docker data).
type AssessDisk struct {
	Path      string  `json:"path"`
	Role      string  `json:"role"` // root | docker
	TotalGB   int     `json:"total_gb"`
	FreeGB    int     `json:"free_gb"`
	WriteMBps float64 `json:"write_mbps,omitempty"` // sequential write with fsync
}

// AssessRuntime describes the container runtime.
type AssessRuntime struct {
	Docker        string `json:"docker,omitempty"` // server version, "" if unavailable
	NvidiaRuntime bool   `json:"nvidia_runtime"`
	GVisor        bool   `json:"gvisor"`
	GVisorGPU     bool   `json:"gvisor_gpu"` // runsc nvproxy supports the installed driver
}

// AssessNetwork holds connectivity measurements.
type AssessNetwork struct {
	HubRTTms         float64 `json:"hub_rtt_ms,omitempty"`
	HubDownMbps      float64 `json:"hub_down_mbps,omitempty"`
	HubUpMbps        float64 `json:"hub_up_mbps,omitempty"`
	InternetDownMbps float64 `json:"internet_down_mbps,omitempty"`
	InternetUpMbps   float64 `json:"internet_up_mbps,omitempty"`
	InternetRTTms    float64 `json:"internet_rtt_ms,omitempty"`
	NAT              string  `json:"nat,omitempty"` // none | endpoint-independent | symmetric | udp-blocked | no-internet | unknown
	IPv6             bool    `json:"ipv6"`
}

// AssessLimits are suggested `ogm agent` limits.
type AssessLimits struct {
	MaxCPU         int      `json:"max_cpu"`
	MaxMemoryMB    int      `json:"max_memory_mb"`
	MaxDiskGB      int      `json:"max_disk_gb"`
	MaxGPUs        int      `json:"max_gpus"` // 0 = all, -1 = none
	PauseOnProcess []string `json:"pause_on_process,omitempty"`
	Window         string   `json:"window,omitempty"`
	AgentFlags     string   `json:"agent_flags"`
}

// Recommendation is one piece of advice from the assessment.
type Recommendation struct {
	Level string `json:"level"` // info | warn | error
	Topic string `json:"topic"` // cpu | memory | disk | gpu | network | runtime | usage
	Text  string `json:"text"`
}

// Secret is a project secret (the value is only ever returned to containers, never by the API).
type Secret struct {
	Name       string    `json:"name"`
	CreateTime time.Time `json:"create_time"`
	UpdateTime time.Time `json:"update_time"`
}

// NodeToken is returned once when a node join token is created.
type NodeToken struct {
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	Token      string `json:"token"`
	InstallCmd string `json:"install_command"`
}

// GPUClass groups GPUs by model.
type GPUClass struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	VRAMMB        int                `json:"vram_mb"`
	CreditsPerHr  float64            `json:"credits_per_hour"`
	Online        int                `json:"online"`
	Free          int                `json:"free"`
	InUse         int                `json:"in_use"`
	HighDemand    bool               `json:"is_high_demand"`
	PricePriority map[string]float64 `json:"prices"`
}

// ---------- Queues & jobs ----------

// QueueSettings tune retry behaviour. All optional.
type QueueSettings struct {
	MaxAttempts       int  `json:"max_attempts,omitempty"`
	RetryBackoffBaseS int  `json:"retry_backoff_base_s,omitempty"`
	RetryBackoffMaxS  int  `json:"retry_backoff_max_s,omitempty"`
	JobTimeoutS       int  `json:"job_timeout_s,omitempty"`
	RetryOn4xx        bool `json:"retry_on_4xx,omitempty"`
	MaxOutputBytes    int  `json:"max_output_bytes,omitempty"`
	RetentionHours    int  `json:"retention_hours,omitempty"`
}

// Queue is a named FIFO job queue in a project.
type Queue struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Project     string        `json:"project"`
	DisplayName string        `json:"display_name,omitempty"`
	Description string        `json:"description,omitempty"`
	Settings    QueueSettings `json:"settings"`
	Stats       QueueStats    `json:"stats"`
	Groups      []string      `json:"container_groups"`
	CreateTime  time.Time     `json:"create_time"`
	UpdateTime  time.Time     `json:"update_time"`
}

// QueueStats counts jobs by status.
type QueueStats struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// Job statuses.
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// JobPrototype is the body for creating a job.
type JobPrototype struct {
	Input       json.RawMessage `json:"input"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Webhook     string          `json:"webhook,omitempty"`
	MaxAttempts int             `json:"max_attempts,omitempty"`
	TimeoutS    int             `json:"timeout_s,omitempty"`
}

// JobEvent records a lifecycle transition.
type JobEvent struct {
	Action     string    `json:"action"`
	Time       time.Time `json:"time"`
	InstanceID string    `json:"instance_id,omitempty"`
	Attempt    int       `json:"attempt,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Job is a unit of work in a queue.
type Job struct {
	ID            string          `json:"id"`
	Queue         string          `json:"queue"`
	Project       string          `json:"project"`
	Status        string          `json:"status"`
	Input         json.RawMessage `json:"input"`
	Output        json.RawMessage `json:"output,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	Webhook       string          `json:"webhook,omitempty"`
	Attempts      int             `json:"attempts"`
	MaxAttempts   int             `json:"max_attempts"`
	TimeoutS      int             `json:"timeout_s"`
	Error         string          `json:"error,omitempty"`
	InstanceID    string          `json:"instance_id,omitempty"`
	NextAttemptAt *time.Time      `json:"next_attempt_at,omitempty"`
	Events        []JobEvent      `json:"events"`
	CreateTime    time.Time       `json:"create_time"`
	UpdateTime    time.Time       `json:"update_time"`
}

// ---------- Misc ----------

// Event is a system event.
type Event struct {
	ID         int64     `json:"id"`
	Project    string    `json:"project,omitempty"`
	Group      string    `json:"container_group,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	NodeID     string    `json:"node_id,omitempty"`
	Kind       string    `json:"kind"`
	Message    string    `json:"message"`
	Time       time.Time `json:"time"`
}

// LogEntry is one line of container output.
type LogEntry struct {
	ID         int64     `json:"id"`
	Project    string    `json:"project,omitempty"`
	Group      string    `json:"container_group,omitempty"`
	InstanceID string    `json:"instance_id"`
	NodeID     string    `json:"node_id,omitempty"`
	Stream     string    `json:"stream"`
	Line       string    `json:"line"`
	Time       time.Time `json:"time"`
}

// User is a cluster member.
type User struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	Credits     float64   `json:"credits"`
	CreateTime  time.Time `json:"create_time"`
	APIKey      string    `json:"api_key,omitempty"` // only on creation
	Nodes       int       `json:"nodes,omitempty"`
	Contributed float64   `json:"contributed_credits,omitempty"`
	Consumed    float64   `json:"consumed_credits,omitempty"`
	// Credential describes the credential used for this request (GET /v1/me only).
	Credential *Credential `json:"credential,omitempty"`
}

// Credential is the kind and scope of the credential behind a request.
type Credential struct {
	Type    string `json:"type"`              // user_key | api_key | instance
	KeyID   string `json:"key_id,omitempty"`  // api_key
	Project string `json:"project,omitempty"` // "" = every project the user can access
	Role    string `json:"role"`              // admin | developer | viewer | instance, or the user's role for user keys
	Group   string `json:"group,omitempty"`   // instance: the calling container group
}

// LedgerEntry is a credits transaction.
type LedgerEntry struct {
	ID     int64     `json:"id"`
	User   string    `json:"user"`
	Delta  float64   `json:"delta"`
	Reason string    `json:"reason"`
	Ref    string    `json:"ref,omitempty"`
	Time   time.Time `json:"time"`
}

// Invite lets someone join the cluster.
type Invite struct {
	Code     string     `json:"code"`
	Role     string     `json:"role"`
	UsesLeft int        `json:"uses_left"`
	Expires  *time.Time `json:"expires,omitempty"`
	JoinCmd  string     `json:"join_command,omitempty"`
}

// Project namespaces resources.
type Project struct {
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	Members    []string  `json:"members"`
	CreateTime time.Time `json:"create_time"`
}

// FileInfo describes a stored file.
type FileInfo struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"content_type"`
	CreateTime  time.Time `json:"create_time"`
}

// ClusterInfo is the public summary of the cluster.
type ClusterInfo struct {
	Name          string  `json:"name"`
	Version       string  `json:"version"`
	Nodes         int     `json:"nodes"`
	NodesOnline   int     `json:"nodes_online"`
	GPUs          int     `json:"gpus"`
	GPUsFree      int     `json:"gpus_free"`
	Users         int     `json:"users"`
	Groups        int     `json:"container_groups"`
	Instances     int     `json:"instances_running"`
	JobsPending   int     `json:"jobs_pending"`
	GatewayDomain string  `json:"gateway_domain,omitempty"`
	PublicURL     string  `json:"public_url"`
	TotalVRAMGB   float64 `json:"total_vram_gb"`
}

// Problem is an RFC 7807 error body.
type Problem struct {
	Type   string              `json:"type,omitempty"`
	Title  string              `json:"title"`
	Status int                 `json:"status"`
	Detail string              `json:"detail,omitempty"`
	Errors map[string][]string `json:"errors,omitempty"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Title + ": " + p.Detail
	}
	return p.Title
}

// List wraps collection responses.
type List[T any] struct {
	Items []T `json:"items"`
}
