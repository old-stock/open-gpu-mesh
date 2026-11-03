// Package client is a small Go SDK for the ogm REST API.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// Client talks to an ogm server.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New creates a client.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Do performs a JSON request. body may be nil; out may be nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		if r, ok := body.(io.Reader); ok {
			rd = r
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("OGM-Api-Key", c.APIKey)
	}
	if body != nil {
		if _, ok := body.(io.Reader); !ok {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		p := &api.Problem{}
		if json.Unmarshal(b, p) != nil || p.Title == "" {
			p = &api.Problem{Title: resp.Status, Status: resp.StatusCode, Detail: strings.TrimSpace(string(b))}
		}
		return p
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func p(project string) string { return "/v1/projects/" + url.PathEscape(project) }

// Me returns the current user.
func (c *Client) Me(ctx context.Context) (*api.User, error) {
	var u api.User
	return &u, c.Do(ctx, "GET", "/v1/me", nil, &u)
}

// Cluster returns cluster info.
func (c *Client) Cluster(ctx context.Context) (*api.ClusterInfo, error) {
	var ci api.ClusterInfo
	return &ci, c.Do(ctx, "GET", "/v1/cluster", nil, &ci)
}

// CreateNode creates a node join token.
func (c *Client) CreateNode(ctx context.Context, name string) (*api.NodeToken, error) {
	var t api.NodeToken
	return &t, c.Do(ctx, "POST", "/v1/nodes", map[string]string{"name": name}, &t)
}

// Nodes lists nodes.
func (c *Client) Nodes(ctx context.Context) ([]api.Node, error) {
	var l api.List[api.Node]
	return l.Items, c.Do(ctx, "GET", "/v1/nodes", nil, &l)
}

// Deploy upserts a container group (PUT).
func (c *Client) Deploy(ctx context.Context, project string, spec api.ContainerGroupSpec) (*api.ContainerGroup, error) {
	var g api.ContainerGroup
	return &g, c.Do(ctx, "PUT", p(project)+"/containers/"+url.PathEscape(spec.Name), spec, &g)
}

// CreateGroup creates a container group (POST).
func (c *Client) CreateGroup(ctx context.Context, project string, spec api.ContainerGroupSpec) (*api.ContainerGroup, error) {
	var g api.ContainerGroup
	return &g, c.Do(ctx, "POST", p(project)+"/containers", spec, &g)
}

// PatchGroup applies a JSON merge patch.
func (c *Client) PatchGroup(ctx context.Context, project, name string, patch any) (*api.ContainerGroup, error) {
	var g api.ContainerGroup
	return &g, c.Do(ctx, "PATCH", p(project)+"/containers/"+url.PathEscape(name), patch, &g)
}

// Group fetches a group with its instances.
func (c *Client) Group(ctx context.Context, project, name string) (*api.ContainerGroup, error) {
	var g api.ContainerGroup
	return &g, c.Do(ctx, "GET", p(project)+"/containers/"+url.PathEscape(name), nil, &g)
}

// Groups lists groups.
func (c *Client) Groups(ctx context.Context, project string) ([]api.ContainerGroup, error) {
	var l api.List[api.ContainerGroup]
	return l.Items, c.Do(ctx, "GET", p(project)+"/containers", nil, &l)
}

// GroupAction posts start/stop.
func (c *Client) GroupAction(ctx context.Context, project, name, action string) error {
	return c.Do(ctx, "POST", p(project)+"/containers/"+url.PathEscape(name)+"/"+action, nil, nil)
}

// DeleteGroup deletes a group.
func (c *Client) DeleteGroup(ctx context.Context, project, name string) error {
	return c.Do(ctx, "DELETE", p(project)+"/containers/"+url.PathEscape(name), nil, nil)
}

// Instances lists live instances.
func (c *Client) Instances(ctx context.Context, project, group string) ([]api.Instance, error) {
	var l api.List[api.Instance]
	return l.Items, c.Do(ctx, "GET", p(project)+"/containers/"+url.PathEscape(group)+"/instances", nil, &l)
}

// InstanceAction posts reallocate/recreate/restart.
func (c *Client) InstanceAction(ctx context.Context, project, group, id, action string) error {
	return c.Do(ctx, "POST", p(project)+"/containers/"+url.PathEscape(group)+"/instances/"+id+"/"+action, nil, nil)
}

// Events lists group events (newest first).
func (c *Client) Events(ctx context.Context, project, group string, limit int) ([]api.Event, error) {
	var l api.List[api.Event]
	return l.Items, c.Do(ctx, "GET", fmt.Sprintf("%s/containers/%s/events?limit=%d", p(project), url.PathEscape(group), limit), nil, &l)
}

// Logs returns recent log lines.
func (c *Client) Logs(ctx context.Context, project, group string, q url.Values) ([]api.LogEntry, error) {
	var l api.List[api.LogEntry]
	return l.Items, c.Do(ctx, "GET", p(project)+"/containers/"+url.PathEscape(group)+"/logs?"+q.Encode(), nil, &l)
}

// FollowLogs streams log lines until ctx is cancelled.
func (c *Client) FollowLogs(ctx context.Context, project, group string, q url.Values, fn func(api.LogEntry)) error {
	q.Set("follow", "1")
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+p(project)+"/containers/"+url.PathEscape(group)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("OGM-Api-Key", c.APIKey)
	hc := *c.HTTP
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e api.LogEntry
		if json.Unmarshal([]byte(line[6:]), &e) == nil {
			fn(e)
		}
	}
	return sc.Err()
}

// CreateQueue creates a queue.
func (c *Client) CreateQueue(ctx context.Context, project, name string, settings api.QueueSettings) (*api.Queue, error) {
	var q api.Queue
	return &q, c.Do(ctx, "POST", p(project)+"/queues", map[string]any{"name": name, "settings": settings}, &q)
}

// Queues lists queues.
func (c *Client) Queues(ctx context.Context, project string) ([]api.Queue, error) {
	var l api.List[api.Queue]
	return l.Items, c.Do(ctx, "GET", p(project)+"/queues", nil, &l)
}

// Queue gets a queue.
func (c *Client) Queue(ctx context.Context, project, name string) (*api.Queue, error) {
	var q api.Queue
	return &q, c.Do(ctx, "GET", p(project)+"/queues/"+url.PathEscape(name), nil, &q)
}

// SubmitJob creates one job.
func (c *Client) SubmitJob(ctx context.Context, project, queue string, jp api.JobPrototype) (*api.Job, error) {
	var j api.Job
	return &j, c.Do(ctx, "POST", p(project)+"/queues/"+url.PathEscape(queue)+"/jobs", jp, &j)
}

// SubmitJobs creates jobs in one batch.
func (c *Client) SubmitJobs(ctx context.Context, project, queue string, jps []api.JobPrototype) ([]api.Job, error) {
	var l api.List[api.Job]
	return l.Items, c.Do(ctx, "POST", p(project)+"/queues/"+url.PathEscape(queue)+"/jobs", map[string]any{"jobs": jps}, &l)
}

// Job gets a job; wait > 0 long-polls up to that many seconds for completion.
func (c *Client) Job(ctx context.Context, project, queue, id string, wait int) (*api.Job, error) {
	var j api.Job
	return &j, c.Do(ctx, "GET", fmt.Sprintf("%s/queues/%s/jobs/%s?wait=%d", p(project), url.PathEscape(queue), id, wait), nil, &j)
}

// Jobs lists jobs.
func (c *Client) Jobs(ctx context.Context, project, queue, status string, limit int) ([]api.Job, error) {
	var l api.List[api.Job]
	return l.Items, c.Do(ctx, "GET", fmt.Sprintf("%s/queues/%s/jobs?status=%s&limit=%d", p(project), url.PathEscape(queue), status, limit), nil, &l)
}

// CancelJob cancels a job.
func (c *Client) CancelJob(ctx context.Context, project, queue, id string) error {
	return c.Do(ctx, "DELETE", p(project)+"/queues/"+url.PathEscape(queue)+"/jobs/"+id, nil, nil)
}
