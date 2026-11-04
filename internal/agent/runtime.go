package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
)

// StartSpec describes a container to create.
type StartSpec struct {
	Name       string
	Image      string
	Command    []string
	Env        map[string]string
	Resources  api.Resources
	GPUIndexes []int
	Ports      []int
	Volumes    []proto.VolumeMount
	Labels     map[string]string
}

// Runtime runs containers.
type Runtime interface {
	Name() string
	Pull(ctx context.Context, image string, auth *api.RegistryAuth, progress func(pct float64)) error
	// HasImage reports whether the image is already present locally.
	HasImage(ctx context.Context, image string) bool
	Create(ctx context.Context, spec StartSpec) (Container, error)
	Cleanup(ctx context.Context) error
	CachedImages() []string
	// IMDSHost is the host name containers use to reach the agent.
	IMDSHost() string
}

// Adopter is implemented by runtimes whose containers outlive the agent process, so an
// agent restart or upgrade re-attaches to running workloads instead of recreating them.
type Adopter interface {
	Adopt(ctx context.Context, instanceID string, version int) (Container, bool)
	ReapOrphans(ctx context.Context) int
}

// Container is a running container.
type Container interface {
	ID() string
	// HostAddr returns the address the agent dials to reach a container port.
	HostAddr(port int) string
	Wait(ctx context.Context) (int, error)
	Stop(ctx context.Context, grace time.Duration) error
	Remove(ctx context.Context) error
	// Logs streams output until the container exits or ctx ends.
	Logs(ctx context.Context, fn func(stream, line string))
	Exec(ctx context.Context, cmd []string) (int, error)
	Stats(ctx context.Context) (cpuPct, memMB float64, err error)
}

// ---------- process runtime (development / demos) ----------

// ProcessRuntime runs the container command as a local process. The image is ignored,
// except the special image "ogm/demo" which runs this binary's built-in demo app.
// Each declared container port gets a free host port; the first is exported as $PORT
// and every mapping as $OGM_PORT_<containerPort>. Not isolated: dev use only.
//
// Volumes are directories under VolumeDir (default: $TMPDIR/ogm-volumes), exported as
// $OGM_VOLUME_<NAME> since a plain process cannot mount them at the requested path.
type ProcessRuntime struct {
	SelfExe   string
	VolumeDir string
}

func (p *ProcessRuntime) volumeDir() string {
	if p.VolumeDir != "" {
		return p.VolumeDir
	}
	return filepath.Join(os.TempDir(), "ogm-volumes")
}

// RemoveVolumes implements VolumeRemover.
func (p *ProcessRuntime) RemoveVolumes(_ context.Context, names []string) error {
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, `/\`) || n == "." || n == ".." {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.volumeDir(), n)); err != nil {
			return err
		}
	}
	return nil
}

func envName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 32
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, s)
}

// Name implements Runtime.
func (p *ProcessRuntime) Name() string { return "process" }

// Pull implements Runtime (simulated).
func (p *ProcessRuntime) Pull(ctx context.Context, image string, _ *api.RegistryAuth, progress func(float64)) error {
	for i := 1; i <= 4; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		progress(float64(i) * 25)
	}
	return nil
}

// Cleanup implements Runtime.
func (p *ProcessRuntime) Cleanup(context.Context) error { return nil }

// HasImage implements Runtime (process "images" are always local).
func (p *ProcessRuntime) HasImage(context.Context, string) bool { return true }

// CachedImages implements Runtime.
func (p *ProcessRuntime) CachedImages() []string { return nil }

// IMDSHost implements Runtime.
func (p *ProcessRuntime) IMDSHost() string { return "127.0.0.1" }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Create implements Runtime.
func (p *ProcessRuntime) Create(ctx context.Context, spec StartSpec) (Container, error) {
	cmdline := spec.Command
	if spec.Image == "ogm/demo" || spec.Image == "ghcr.io/old-stock/open-gpu-mesh-demo" {
		cmdline = append([]string{p.SelfExe, "demo-app"}, spec.Command...)
	} else if len(cmdline) > 0 && cmdline[0] == "ogm" {
		cmdline = append([]string{p.SelfExe}, cmdline[1:]...)
	}
	if len(cmdline) == 0 {
		return nil, errors.New("process runtime needs container.command (or image ogm/demo)")
	}
	pc := &procContainer{ports: map[int]int{}, spec: spec, cmdline: cmdline, logs: make(chan logLine, 4096), done: make(chan struct{})}
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	for i, port := range spec.Ports {
		hp, err := freePort()
		if err != nil {
			return nil, err
		}
		pc.ports[port] = hp
		env = append(env, fmt.Sprintf("OGM_PORT_%d=%d", port, hp))
		if i == 0 {
			env = append(env, fmt.Sprintf("PORT=%d", hp))
		}
	}
	for _, v := range spec.Volumes {
		dir := filepath.Join(p.volumeDir(), v.Source)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		env = append(env, "OGM_VOLUME_"+envName(v.Name)+"="+dir)
	}
	pc.env = env
	if err := pc.start(); err != nil {
		return nil, err
	}
	return pc, nil
}

type logLine struct{ stream, line string }

type procContainer struct {
	spec    StartSpec
	cmdline []string
	env     []string
	ports   map[int]int
	cmd     *exec.Cmd
	logs    chan logLine
	done    chan struct{}
	mu      sync.Mutex
	code    int
}

func (c *procContainer) start() error {
	cmd := exec.Command(c.cmdline[0], c.cmdline[1:]...)
	cmd.Env = c.env
	setProcessGroup(cmd)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	var wg sync.WaitGroup
	pump := func(name string, r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case c.logs <- logLine{name, sc.Text()}:
			default:
			}
		}
	}
	wg.Add(2)
	go pump("stdout", stdout)
	go pump("stderr", stderr)
	go func() {
		wg.Wait()
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					code = 128 + int(ws.Signal())
				}
			}
		}
		c.mu.Lock()
		c.code = code
		c.mu.Unlock()
		close(c.done)
	}()
	return nil
}

func (c *procContainer) ID() string { return "pid-" + strconv.Itoa(c.cmd.Process.Pid) }

func (c *procContainer) HostAddr(port int) string {
	if hp, ok := c.ports[port]; ok {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(hp))
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func (c *procContainer) Wait(ctx context.Context) (int, error) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *procContainer) Stop(ctx context.Context, grace time.Duration) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	terminateGroup(c.cmd.Process, false)
	select {
	case <-c.done:
		return nil
	case <-time.After(grace):
	case <-ctx.Done():
	}
	terminateGroup(c.cmd.Process, true)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (c *procContainer) Remove(ctx context.Context) error { return c.Stop(ctx, 0) }

func (c *procContainer) Logs(ctx context.Context, fn func(stream, line string)) {
	for {
		select {
		case l := <-c.logs:
			fn(l.stream, l.line)
		case <-ctx.Done():
			return
		case <-c.done:
			// drain
			for {
				select {
				case l := <-c.logs:
					fn(l.stream, l.line)
				default:
					return
				}
			}
		}
	}
}

func (c *procContainer) Exec(ctx context.Context, cmd []string) (int, error) {
	if len(cmd) == 0 {
		return -1, errors.New("empty command")
	}
	x := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	x.Env = c.env
	err := x.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func (c *procContainer) Stats(context.Context) (float64, float64, error) { return 0, 0, nil }
