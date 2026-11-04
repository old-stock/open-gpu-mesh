package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// DockerRuntime talks to the Docker Engine API over its unix socket.
type DockerRuntime struct {
	Socket      string // default /var/run/docker.sock (or $DOCKER_HOST unix://...)
	GPURuntime  string // "nvidia" (DeviceRequests) — the default
	ExtraSecOpt []string
	OCIRuntime  string // optional, e.g. "runsc" for gVisor

	once    sync.Once
	client  *http.Client
	mu      sync.Mutex
	cached  map[string]bool
	orphans map[string]orphan // instance id -> running container from a previous agent process
	nets    map[string]bool   // project networks known to exist
}

const dockerLabel = "dev.ogm.managed"

func (d *DockerRuntime) init() {
	d.once.Do(func() {
		sock := d.Socket
		if sock == "" {
			if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
				sock = strings.TrimPrefix(h, "unix://")
			} else {
				sock = "/var/run/docker.sock"
			}
		}
		d.Socket = sock
		d.client = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", sock)
			},
		}}
		d.cached = map[string]bool{}
	})
}

// Name implements Runtime.
func (d *DockerRuntime) Name() string { return "docker" }

// IMDSHost implements Runtime.
func (d *DockerRuntime) IMDSHost() string { return "host.docker.internal" }

// LinkHost implements LinkHoster: link listeners bind to the default bridge gateway (what
// host.docker.internal resolves to on Linux) so only local containers reach them. Docker
// Desktop runs containers in a VM whose host.docker.internal reaches the host's loopback.
func (d *DockerRuntime) LinkHost(ctx context.Context) (string, string) {
	resp, err := d.do(ctx, http.MethodGet, "/networks/bridge", nil, nil)
	if err == nil {
		var nw struct {
			IPAM struct {
				Config []struct{ Gateway string }
			}
		}
		_ = json.NewDecoder(resp.Body).Decode(&nw)
		resp.Body.Close()
		for _, c := range nw.IPAM.Config {
			if ip := net.ParseIP(c.Gateway); ip != nil && localIP(ip) {
				return c.Gateway, "host.docker.internal"
			}
		}
	}
	return "127.0.0.1", "host.docker.internal"
}

func localIP(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// RemoveVolumes implements VolumeRemover. Only volumes created by ogm are removed.
func (d *DockerRuntime) RemoveVolumes(ctx context.Context, names []string) error {
	for _, n := range names {
		if !strings.HasPrefix(n, "ogm-vol-") {
			continue
		}
		resp, err := d.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(n), nil, nil)
		var de *dockerError
		if errors.As(err, &de) && de.Status == http.StatusNotFound {
			continue
		}
		if err != nil {
			return err
		}
		resp.Body.Close()
	}
	return nil
}

func (d *DockerRuntime) do(ctx context.Context, method, path string, body any, hdr http.Header) (*http.Response, error) {
	d.init()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.43"+path, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return nil, &dockerError{Status: resp.StatusCode, Msg: m.Message}
	}
	return resp, nil
}

type dockerError struct {
	Status int
	Msg    string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Msg) }

// Ping checks the daemon is reachable.
func (d *DockerRuntime) Ping(ctx context.Context) error {
	resp, err := d.do(ctx, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func splitImage(image string) (repo, tag string) {
	if i := strings.Index(image, "@"); i >= 0 {
		return image[:i], image[i+1:]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, "latest"
}

// Pull implements Runtime.
func (d *DockerRuntime) Pull(ctx context.Context, image string, auth *api.RegistryAuth, progress func(float64)) error {
	repo, tag := splitImage(image)
	hdr := http.Header{}
	if auth != nil && auth.Username != "" {
		server := auth.Server
		if server == "" {
			if i := strings.Index(repo, "/"); i > 0 && strings.ContainsAny(repo[:i], ".:") {
				server = repo[:i]
			}
		}
		b, _ := json.Marshal(map[string]string{"username": auth.Username, "password": auth.Password, "serveraddress": server})
		hdr.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(b))
	}
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	resp, err := d.do(ctx, http.MethodPost, "/images/create?"+q.Encode(), nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	type layer struct{ cur, total int64 }
	layers := map[string]*layer{}
	dec := json.NewDecoder(resp.Body)
	last := time.Time{}
	for {
		var m struct {
			ID             string `json:"id"`
			Status         string `json:"status"`
			Error          string `json:"error"`
			ProgressDetail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.ID != "" && m.ProgressDetail.Total > 0 && strings.HasPrefix(m.Status, "Downloading") {
			l := layers[m.ID]
			if l == nil {
				l = &layer{}
				layers[m.ID] = l
			}
			l.cur, l.total = m.ProgressDetail.Current, m.ProgressDetail.Total
		}
		if m.ID != "" && (m.Status == "Download complete" || m.Status == "Pull complete" || m.Status == "Already exists") {
			if l := layers[m.ID]; l != nil {
				l.cur = l.total
			}
		}
		if time.Since(last) > time.Second {
			last = time.Now()
			var cur, total int64
			for _, l := range layers {
				cur += l.cur
				total += l.total
			}
			if total > 0 {
				progress(99 * float64(cur) / float64(total))
			}
		}
	}
	progress(100)
	d.mu.Lock()
	d.cached[image] = true
	d.mu.Unlock()
	return nil
}

// HasImage implements Runtime.
func (d *DockerRuntime) HasImage(ctx context.Context, image string) bool {
	resp, err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// CachedImages implements Runtime.
func (d *DockerRuntime) CachedImages() []string {
	d.init()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.cached))
	for k := range d.cached {
		out = append(out, k)
	}
	return out
}

// Cleanup removes containers left behind by a previous agent run.
func (d *DockerRuntime) Cleanup(ctx context.Context) error {
	f, _ := json.Marshal(map[string][]string{"label": {dockerLabel + "=1"}})
	resp, err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(f)), nil, nil)
	if err != nil {
		return err
	}
	var list []struct {
		ID     string            `json:"Id"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	d.mu.Lock()
	if d.orphans == nil {
		d.orphans = map[string]orphan{}
	}
	d.mu.Unlock()
	for _, c := range list {
		// Running workloads survive an agent restart/upgrade: keep them for the server to
		// re-claim (Adopt). Anything else, or unclaimed ones later (ReapOrphans), is removed.
		if inst := c.Labels["dev.ogm.instance"]; c.State == "running" && inst != "" {
			v := -1 // unknown (containers from older agents): adopt whatever version the server wants
			if s, ok := c.Labels["dev.ogm.version"]; ok {
				v, _ = strconv.Atoi(s)
			}
			d.mu.Lock()
			d.orphans[inst] = orphan{id: c.ID, version: v}
			d.mu.Unlock()
			continue
		}
		if r, err := d.do(ctx, http.MethodDelete, "/containers/"+c.ID+"?force=1", nil, nil); err == nil {
			r.Body.Close()
		}
	}
	// Drop project networks no surviving container uses.
	if resp, err := d.do(ctx, http.MethodGet, "/networks?filters="+url.QueryEscape(string(f)), nil, nil); err == nil {
		var nets []struct{ Name string }
		_ = json.NewDecoder(resp.Body).Decode(&nets)
		resp.Body.Close()
		for _, n := range nets {
			d.pruneNetwork(ctx, n.Name)
		}
	}
	// Seed the image cache list.
	if resp, err := d.do(ctx, http.MethodGet, "/images/json", nil, nil); err == nil {
		var imgs []struct{ RepoTags []string }
		_ = json.NewDecoder(resp.Body).Decode(&imgs)
		resp.Body.Close()
		d.mu.Lock()
		for _, im := range imgs {
			for _, t := range im.RepoTags {
				d.cached[t] = true
				d.cached[strings.TrimSuffix(t, ":latest")] = true
			}
		}
		d.mu.Unlock()
	}
	return nil
}

// Create implements Runtime: creates and starts a container.
func (d *DockerRuntime) Create(ctx context.Context, spec StartSpec) (Container, error) {
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, p := range spec.Ports {
		k := strconv.Itoa(p) + "/tcp"
		exposed[k] = struct{}{}
		bindings[k] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}
	}
	labels := map[string]string{dockerLabel: "1"}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	r := spec.Resources
	pids := int64(4096)
	hostCfg := map[string]any{
		"PortBindings":  bindings,
		"Memory":        int64(r.MemoryMB) << 20,
		"NanoCpus":      int64(r.CPU) * 1_000_000_000,
		"ShmSize":       int64(r.ShmMB) << 20,
		"PidsLimit":     pids,
		"SecurityOpt":   append([]string{"no-new-privileges"}, d.ExtraSecOpt...),
		"CapDrop":       []string{"NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "MKNOD"},
		"ExtraHosts":    []string{"host.docker.internal:host-gateway"},
		"RestartPolicy": map[string]any{"Name": "no"},
		"LogConfig":     map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "50m", "max-file": "2"}},
	}
	if d.OCIRuntime != "" {
		hostCfg["Runtime"] = d.OCIRuntime
	}
	// Each project gets its own bridge network. Docker isolates bridge networks from
	// each other, so containers of different projects on one node cannot reach each
	// other; they still reach the agent (IMDS, links) through host.docker.internal.
	netName := ""
	if p := spec.Labels["dev.ogm.project"]; p != "" {
		netName = projectNetwork(p)
		if err := d.ensureNetwork(ctx, netName, p); err != nil {
			return nil, fmt.Errorf("project network %s: %w", netName, err)
		}
		hostCfg["NetworkMode"] = netName
	}
	if len(spec.Volumes) > 0 {
		var mounts []map[string]any
		for _, v := range spec.Volumes {
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": v.Source, "Target": v.Target,
				"VolumeOptions": map[string]any{"Labels": map[string]string{dockerLabel: "1", "dev.ogm.volume": v.Name}}})
		}
		hostCfg["Mounts"] = mounts
	}
	if len(spec.GPUIndexes) > 0 {
		ids := make([]string, len(spec.GPUIndexes))
		for i, g := range spec.GPUIndexes {
			ids[i] = strconv.Itoa(g)
		}
		hostCfg["DeviceRequests"] = []map[string]any{{
			"Driver": "nvidia", "DeviceIDs": ids, "Capabilities": [][]string{{"gpu"}},
		}}
	}
	body := map[string]any{
		"Image": spec.Image, "Env": env, "Labels": labels, "ExposedPorts": exposed, "HostConfig": hostCfg,
		"Hostname": strings.TrimPrefix(spec.Env["OGM_MACHINE_ID"], "node_"),
	}
	if len(spec.Command) > 0 {
		body["Entrypoint"] = spec.Command[:1]
		body["Cmd"] = spec.Command[1:]
	}
	resp, err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape("ogm-"+spec.Name), body, nil)
	var de *dockerError
	if errors.As(err, &de) && de.Status == http.StatusConflict {
		if r, err := d.do(ctx, http.MethodDelete, "/containers/ogm-"+spec.Name+"?force=1", nil, nil); err == nil {
			r.Body.Close()
		}
		resp, err = d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape("ogm-"+spec.Name), body, nil)
	}
	if errors.As(err, &de) && de.Status == http.StatusNotFound && netName != "" {
		// The project network was pruned between ensureNetwork and create (its last
		// container just went away, e.g. during a rolling update): recreate it once.
		d.mu.Lock()
		delete(d.nets, netName)
		d.mu.Unlock()
		if nerr := d.ensureNetwork(ctx, netName, spec.Labels["dev.ogm.project"]); nerr == nil {
			resp, err = d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape("ogm-"+spec.Name), body, nil)
		}
	}
	if err != nil {
		return nil, err
	}
	var cr struct {
		ID string `json:"Id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	c := &dockerContainer{d: d, id: cr.ID, ports: map[int]string{}, net: netName}
	if r, err := d.do(ctx, http.MethodPost, "/containers/"+cr.ID+"/start", nil, nil); err != nil {
		_ = c.Remove(context.Background())
		return nil, err
	} else {
		r.Body.Close()
	}
	c.resolvePorts(ctx)
	return c, nil
}

type orphan struct {
	id      string
	version int
}

// Adopt implements Adopter: re-attach to a container left running by a previous agent process.
func (d *DockerRuntime) Adopt(ctx context.Context, instanceID string, version int) (Container, bool) {
	d.mu.Lock()
	o, ok := d.orphans[instanceID]
	delete(d.orphans, instanceID)
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	c := &dockerContainer{d: d, id: o.id, ports: map[int]string{}, logsSince: time.Now().Unix()}
	if o.version >= 0 && o.version != version {
		_ = c.Remove(ctx)
		return nil, false
	}
	c.resolvePorts(ctx)
	return c, true
}

// ReapOrphans implements Adopter: remove containers nobody re-claimed.
func (d *DockerRuntime) ReapOrphans(ctx context.Context) int {
	d.mu.Lock()
	orphans := d.orphans
	d.orphans = map[string]orphan{}
	d.mu.Unlock()
	for _, o := range orphans {
		_ = (&dockerContainer{d: d, id: o.id}).Remove(ctx)
	}
	return len(orphans)
}

func (c *dockerContainer) resolvePorts(ctx context.Context) {
	d, cr := c.d, struct{ ID string }{c.id}
	if r, err := d.do(ctx, http.MethodGet, "/containers/"+cr.ID+"/json", nil, nil); err == nil {
		var insp struct {
			NetworkSettings struct {
				Ports map[string][]struct{ HostIP, HostPort string } `json:"Ports"`
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&insp)
		r.Body.Close()
		for k, v := range insp.NetworkSettings.Ports {
			p, _ := strconv.Atoi(strings.TrimSuffix(k, "/tcp"))
			for _, b := range v {
				if b.HostPort != "" {
					c.ports[p] = net.JoinHostPort("127.0.0.1", b.HostPort)
					break
				}
			}
		}
	}
}

type dockerContainer struct {
	d         *DockerRuntime
	id        string
	net       string // project network, removed with its last container
	ports     map[int]string
	logsSince int64 // adopted containers: don't re-ship old logs
}

func (c *dockerContainer) ID() string { return c.id }

// IPs implements AddrLister: the container's addresses on its Docker networks.
func (c *dockerContainer) IPs(ctx context.Context) []string {
	resp, err := c.d.do(ctx, http.MethodGet, "/containers/"+c.id+"/json", nil, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var info struct {
		NetworkSettings struct {
			IPAddress string
			Networks  map[string]struct {
				IPAddress         string
				GlobalIPv6Address string
			}
		}
	}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return nil
	}
	out := []string{}
	if a := info.NetworkSettings.IPAddress; a != "" {
		out = append(out, a)
	}
	for _, n := range info.NetworkSettings.Networks {
		for _, a := range []string{n.IPAddress, n.GlobalIPv6Address} {
			if a != "" {
				out = append(out, a)
			}
		}
	}
	return out
}

func (c *dockerContainer) HostAddr(port int) string {
	if a, ok := c.ports[port]; ok {
		return a
	}
	return ""
}

func (c *dockerContainer) Wait(ctx context.Context) (int, error) {
	resp, err := c.d.do(ctx, http.MethodPost, "/containers/"+c.id+"/wait?condition=not-running", nil, nil)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	var w struct {
		StatusCode int `json:"StatusCode"`
	}
	err = json.NewDecoder(resp.Body).Decode(&w)
	return w.StatusCode, err
}

func (c *dockerContainer) Stop(ctx context.Context, grace time.Duration) error {
	resp, err := c.d.do(ctx, http.MethodPost, fmt.Sprintf("/containers/%s/stop?t=%d", c.id, int(grace.Seconds())), nil, nil)
	if err == nil {
		resp.Body.Close()
	}
	var de *dockerError
	if errors.As(err, &de) && (de.Status == http.StatusNotModified || de.Status == http.StatusNotFound) {
		return nil
	}
	return err
}

func (c *dockerContainer) Remove(ctx context.Context) error {
	resp, err := c.d.do(ctx, http.MethodDelete, "/containers/"+c.id+"?force=1&v=1", nil, nil)
	if err == nil {
		resp.Body.Close()
	}
	if c.net != "" {
		// Docker can take a moment to release the endpoint after a forced removal.
		go func(name string) {
			for i := 0; i < 3; i++ {
				pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				gone := c.d.pruneNetwork(pctx, name)
				cancel()
				if gone {
					return
				}
				time.Sleep(2 * time.Second)
			}
		}(c.net)
	}
	var de *dockerError
	if errors.As(err, &de) && de.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *dockerContainer) Logs(ctx context.Context, fn func(stream, line string)) {
	resp, err := c.d.do(ctx, http.MethodGet, "/containers/"+c.id+"/logs?follow=1&stdout=1&stderr=1&since="+strconv.FormatInt(c.logsSince, 10), nil, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	// Multiplexed stream: 8-byte header [stream, 0,0,0, size(4)] + payload.
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	partial := map[byte]string{}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		text := partial[hdr[0]] + string(buf)
		lines := strings.Split(text, "\n")
		partial[hdr[0]] = lines[len(lines)-1]
		for _, l := range lines[:len(lines)-1] {
			fn(stream, l)
		}
	}
}

func (c *dockerContainer) Exec(ctx context.Context, cmd []string) (int, error) {
	resp, err := c.d.do(ctx, http.MethodPost, "/containers/"+c.id+"/exec", map[string]any{"Cmd": cmd, "AttachStdout": true, "AttachStderr": true}, nil)
	if err != nil {
		return -1, err
	}
	var ex struct {
		ID string `json:"Id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ex)
	resp.Body.Close()
	resp, err = c.d.do(ctx, http.MethodPost, "/exec/"+ex.ID+"/start", map[string]any{"Detach": false, "Tty": false}, nil)
	if err != nil {
		return -1, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	resp, err = c.d.do(ctx, http.MethodGet, "/exec/"+ex.ID+"/json", nil, nil)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	var st struct{ ExitCode int }
	err = json.NewDecoder(resp.Body).Decode(&st)
	return st.ExitCode, err
}

func (c *dockerContainer) Stats(ctx context.Context) (float64, float64, error) {
	resp, err := c.d.do(ctx, http.MethodGet, "/containers/"+c.id+"/stats?stream=false", nil, nil)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	var s struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs  int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
		} `json:"memory_stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, 0, err
	}
	cpu := 0.0
	cd := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sd := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if sd > 0 && cd > 0 {
		n := s.CPUStats.OnlineCPUs
		if n == 0 {
			n = 1
		}
		cpu = cd / sd * float64(n) * 100
	}
	return cpu, float64(s.MemoryStats.Usage) / (1 << 20), nil
}

// projectNetwork is the Docker network for a project's containers.
func projectNetwork(project string) string { return "ogm-net-" + project }

// ensureNetwork creates the project's bridge network if it does not exist yet.
func (d *DockerRuntime) ensureNetwork(ctx context.Context, name, project string) error {
	d.mu.Lock()
	known := d.nets[name]
	d.mu.Unlock()
	if known {
		return nil
	}
	if r, err := d.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil); err == nil {
		r.Body.Close()
	} else {
		r, err := d.do(ctx, http.MethodPost, "/networks/create", map[string]any{
			"Name": name, "Driver": "bridge", "CheckDuplicate": true,
			"Labels": map[string]string{dockerLabel: "1", "dev.ogm.project": project},
		}, nil)
		var de *dockerError
		if err != nil && !(errors.As(err, &de) && de.Status == http.StatusConflict) {
			return err
		}
		if err == nil {
			r.Body.Close()
		}
	}
	d.mu.Lock()
	if d.nets == nil {
		d.nets = map[string]bool{}
	}
	d.nets[name] = true
	d.mu.Unlock()
	return nil
}

// pruneNetwork removes a project network once no container uses it (Docker refuses
// while it is in use, which is fine), so idle projects don't hold address space.
// It reports whether the network is gone.
func (d *DockerRuntime) pruneNetwork(ctx context.Context, name string) bool {
	r, err := d.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil)
	var de *dockerError
	if err != nil && !(errors.As(err, &de) && de.Status == http.StatusNotFound) {
		return false
	}
	if err == nil {
		r.Body.Close()
	}
	d.mu.Lock()
	delete(d.nets, name)
	d.mu.Unlock()
	return true
}
