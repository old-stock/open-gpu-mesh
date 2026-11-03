// Package proto defines the agent<->server control protocol carried over the tunnel.
//
// The agent opens a WebSocket to /v1/agent/connect and runs a yamux client over it.
// The first stream the agent opens is the control stream: newline-delimited JSON Envelopes.
// The server opens further streams for data (gateway proxying); each starts with a
// newline-terminated JSON StreamHeader, answered by a single ack byte from the agent.
package proto

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// Message types.
const (
	// agent -> server
	TypeHello          = "hello"
	TypeHeartbeat      = "heartbeat"
	TypeInstanceStatus = "instance.status"
	TypeJobResult      = "job.result"
	TypeLogs           = "logs"
	TypeIMDS           = "imds.request" // expects reply
	// server -> agent
	TypeWelcome          = "welcome"
	TypeInstanceStart    = "instance.start"
	TypeInstanceStop     = "instance.stop"
	TypeInstanceRestart  = "instance.restart"
	TypeInstanceRecreate = "instance.recreate"
	TypeJobAssign        = "job.assign"
	TypeJobCancel        = "job.cancel"
	TypeNodeConfig       = "node.config"
	// both
	TypeReply = "reply"
)

// Envelope frames every control message.
type Envelope struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Hello is the first message from the agent.
type Hello struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	Runtime         string            `json:"runtime"`
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	CPU             int               `json:"cpu"`
	MemoryMB        int               `json:"memory_mb"`
	DiskGB          int               `json:"disk_gb"`
	GPUs            []api.GPU         `json:"gpus"`
	Country         string            `json:"country,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	AllowPriorities []string          `json:"allow_priorities,omitempty"`
	Window          string            `json:"window,omitempty"`
	Paused          bool              `json:"paused"`
	PauseReason     string            `json:"pause_reason,omitempty"`
	Instances       []InstanceReport  `json:"instances"`
	CachedImages    []string          `json:"cached_images,omitempty"`
}

// Welcome acknowledges a hello.
type Welcome struct {
	NodeID     string    `json:"node_id"`
	NodeName   string    `json:"node_name"`
	ServerTime time.Time `json:"server_time"`
}

// Heartbeat is sent periodically by the agent.
type Heartbeat struct {
	GPUs         []api.GPU        `json:"gpus"`
	Paused       bool             `json:"paused"`
	PauseReason  string           `json:"pause_reason,omitempty"`
	InWindow     bool             `json:"in_window"`
	Instances    []InstanceReport `json:"instances"`
	CachedImages []string         `json:"cached_images,omitempty"`
}

// InstanceReport is the agent's view of an instance.
type InstanceReport struct {
	ID           string              `json:"id"`
	State        string              `json:"state"`
	Ready        bool                `json:"ready"`
	Started      bool                `json:"started"`
	ExitCode     *int                `json:"exit_code,omitempty"`
	Message      string              `json:"message,omitempty"`
	Event        string              `json:"event,omitempty"` // optional system event to record
	PullProgress float64             `json:"pull_progress,omitempty"`
	Metrics      api.InstanceMetrics `json:"metrics"`
	Version      int                 `json:"version"`
}

// InstanceSpec is everything an agent needs to run an instance.
type InstanceSpec struct {
	ID             string            `json:"id"`
	Project        string            `json:"project"`
	Group          string            `json:"group"`
	GroupID        string            `json:"group_id"`
	Version        int               `json:"version"`
	Image          string            `json:"image"`
	Command        []string          `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	RegistryAuth   *api.RegistryAuth `json:"registry_auth,omitempty"`
	PullPolicy     string            `json:"pull_policy,omitempty"`
	Resources      api.Resources     `json:"resources"`
	GPUIndexes     []int             `json:"gpu_indexes,omitempty"`
	Ports          []int             `json:"ports,omitempty"`
	StartupProbe   *api.Probe        `json:"startup_probe,omitempty"`
	ReadinessProbe *api.Probe        `json:"readiness_probe,omitempty"`
	LivenessProbe  *api.Probe        `json:"liveness_probe,omitempty"`
	RestartPolicy  string            `json:"restart_policy"`
	IMDSSecret     string            `json:"imds_secret"`
	DeletionCost   int               `json:"deletion_cost"`
}

// InstanceStop asks the agent to stop (and remove) an instance.
type InstanceStop struct {
	ID     string `json:"id"`
	GraceS int    `json:"grace_s"`
	Reason string `json:"reason"`
}

// InstanceRef identifies an instance (restart/recreate).
type InstanceRef struct {
	ID   string        `json:"id"`
	Spec *InstanceSpec `json:"spec,omitempty"` // for recreate with new version
}

// JobAssign dispatches a queue job to an instance on the node.
type JobAssign struct {
	JobID          string          `json:"job_id"`
	Attempt        int             `json:"attempt"`
	InstanceID     string          `json:"instance_id"`
	Port           int             `json:"port"`
	Path           string          `json:"path"`
	Input          json.RawMessage `json:"input"`
	TimeoutS       int             `json:"timeout_s"`
	MaxOutputBytes int             `json:"max_output_bytes"`
}

// JobResult reports a job attempt outcome.
type JobResult struct {
	JobID      string          `json:"job_id"`
	Attempt    int             `json:"attempt"`
	InstanceID string          `json:"instance_id"`
	Succeeded  bool            `json:"succeeded"`
	Cancelled  bool            `json:"cancelled,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	Retryable  bool            `json:"retryable"`
	DurationMS int64           `json:"duration_ms"`
}

// JobCancel aborts an in-flight job.
type JobCancel struct {
	JobID string `json:"job_id"`
}

// LogBatch carries container output lines.
type LogBatch struct {
	Entries []LogLine `json:"entries"`
}

// LogLine is a single log line.
type LogLine struct {
	InstanceID string    `json:"i"`
	Stream     string    `json:"s"`
	Line       string    `json:"l"`
	Time       time.Time `json:"t"`
}

// IMDS operations forwarded to the server.
const (
	IMDSToken           = "token"
	IMDSSetDeletionCost = "set_deletion_cost"
	IMDSReallocate      = "reallocate"
	IMDSRecreate        = "recreate"
	IMDSRestart         = "restart"
	IMDSMetadata        = "metadata"
)

// IMDSRequest asks the server to perform an IMDS operation on behalf of an instance.
type IMDSRequest struct {
	InstanceID   string `json:"instance_id"`
	Op           string `json:"op"`
	DeletionCost int    `json:"deletion_cost,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// IMDSReply is the server's reply.
type IMDSReply struct {
	JWT      string         `json:"jwt,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NodeConfig pushes owner-editable settings from the server (e.g. pause via API).
type NodeConfig struct {
	Paused *bool `json:"paused,omitempty"`
}

// StreamHeader opens a data stream.
type StreamHeader struct {
	Kind       string `json:"kind"` // "proxy"
	InstanceID string `json:"instance_id"`
	Port       int    `json:"port"`
}

// Stream ack bytes.
const (
	AckOK   byte = 0
	AckFail byte = 1
)

// Codec reads and writes envelopes on a stream. Writes are goroutine-safe.
type Codec struct {
	mu  sync.Mutex
	w   io.Writer
	r   *bufio.Reader
	enc *json.Encoder
}

// NewCodec wraps a bidirectional stream.
func NewCodec(rw io.ReadWriter) *Codec {
	return &Codec{w: rw, r: bufio.NewReaderSize(rw, 64<<10), enc: json.NewEncoder(rw)}
}

// Send writes a typed message.
func (c *Codec) Send(typ, id string, data any) error {
	env := Envelope{Type: typ, ID: id}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		env.Data = b
	}
	return c.SendEnvelope(env)
}

// SendEnvelope writes a raw envelope.
func (c *Codec) SendEnvelope(env Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(env)
}

// Recv reads the next envelope. Lines up to 64 MiB are accepted.
func (c *Codec) Recv() (Envelope, error) {
	var env Envelope
	line, err := readLine(c.r, 64<<20)
	if err != nil {
		return env, err
	}
	err = json.Unmarshal(line, &env)
	return env, err
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, io.ErrShortBuffer
		}
		if !isPrefix {
			return buf, nil
		}
	}
}
