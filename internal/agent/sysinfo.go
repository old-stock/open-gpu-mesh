package agent

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// GPUSource reports GPUs and their live stats.
type GPUSource interface {
	GPUs(ctx context.Context) []api.GPU
}

// NvidiaSMI queries nvidia-smi.
type NvidiaSMI struct{}

// GPUs implements GPUSource.
func (NvidiaSMI) GPUs(ctx context.Context) []api.GPU {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,uuid,name,memory.total,utilization.gpu,memory.used,temperature.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	gpus := parseNvidiaSMI(out)
	// Unified-memory GPUs (e.g. GB10 / DGX Spark, Jetson) report memory as [N/A]: the GPU
	// shares system RAM, so usable "VRAM" is system memory and used = total - available.
	for i := range gpus {
		if gpus[i].Unified {
			total, avail := memInfoMB()
			gpus[i].VRAMMB = total
			gpus[i].MemUsedMB = total - avail
		}
	}
	return gpus
}

// memInfoMB returns MemTotal and MemAvailable from /proc/meminfo (0, 0 if unavailable).
func memInfoMB() (total, avail int) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "MemTotal:":
			total = kb / 1024
		case "MemAvailable:":
			avail = kb / 1024
		}
	}
	return total, avail
}

func parseNvidiaSMI(out []byte) []api.GPU {
	var gpus []api.GPU
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		if len(f) < 7 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		mem, _ := strconv.Atoi(f[3])
		util, _ := strconv.ParseFloat(f[4], 64)
		used, _ := strconv.Atoi(f[5])
		temp, _ := strconv.ParseFloat(f[6], 64)
		gpus = append(gpus, api.GPU{Index: idx, UUID: f[1], Name: f[2], VRAMMB: mem, UtilPct: util, MemUsedMB: used, TempC: temp,
			Unified: strings.Contains(f[3], "N/A")})
	}
	return gpus
}

// FakeGPUs is a static GPU list for development ("RTX 4090:24564,RTX 3090:24576").
type FakeGPUs struct{ List []api.GPU }

// ParseFakeGPUs parses "Name:VRAM_MB,Name:VRAM_MB".
func ParseFakeGPUs(s string) FakeGPUs {
	var f FakeGPUs
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, vram := part, 24576
		if j := strings.LastIndex(part, ":"); j > 0 {
			name = part[:j]
			if v, err := strconv.Atoi(part[j+1:]); err == nil {
				vram = v
			}
		}
		f.List = append(f.List, api.GPU{Index: i, UUID: "GPU-fake-" + strconv.Itoa(i), Name: name, VRAMMB: vram, TempC: 40})
	}
	return f
}

// GPUs implements GPUSource.
func (f FakeGPUs) GPUs(context.Context) []api.GPU {
	out := make([]api.GPU, len(f.List))
	copy(out, f.List)
	return out
}

func cpuCount() int { return runtime.NumCPU() }

func memoryMB() int {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					kb, _ := strconv.Atoi(f[1])
					return kb / 1024
				}
			}
		}
	}
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		b, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return int(b >> 20)
	}
	return 8192
}

// runningProcesses lists process names (Linux/macOS, plus Windows via WSL interop).
func runningProcesses(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if b, err := exec.CommandContext(ctx, "ps", "-eo", "comm").Output(); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if i := strings.LastIndex(l, "/"); i >= 0 {
				l = l[i+1:]
			}
			if l != "" {
				out[strings.ToLower(l)] = true
			}
		}
	}
	if _, err := exec.LookPath("tasklist.exe"); err == nil {
		if b, err := exec.CommandContext(ctx, "tasklist.exe", "/fo", "csv", "/nh").Output(); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				f := strings.Split(l, ",")
				if len(f) > 0 {
					name := strings.ToLower(strings.Trim(strings.TrimSpace(f[0]), `"`))
					if name != "" {
						out[name] = true
						out[strings.TrimSuffix(name, ".exe")] = true
					}
				}
			}
		}
	}
	return out
}
