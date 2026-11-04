package assess

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/agent"
	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// run executes a command with a timeout and returns trimmed stdout ("" on any error).
func run(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func probeSystem(ctx context.Context, a *api.Assessment) {
	a.Kernel = run(ctx, "uname", "-r")
	a.CPU.Threads = runtime.NumCPU()
	a.CPU.Cores = a.CPU.Threads

	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(l, ":")
			if ok && strings.TrimSpace(k) == "model name" {
				a.CPU.Model = strings.TrimSpace(v)
				break
			}
		}
	}
	if a.CPU.Model == "" { // arm64 Linux has no "model name"; lscpu knows
		for _, l := range strings.Split(run(ctx, "lscpu"), "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "Model name" {
				a.CPU.Model = strings.TrimSpace(v)
				break
			}
		}
	}
	if out := run(ctx, "lscpu", "-p=CORE,SOCKET"); out != "" {
		seen := map[string]bool{}
		for _, l := range strings.Split(out, "\n") {
			if l != "" && !strings.HasPrefix(l, "#") {
				seen[l] = true
			}
		}
		if len(seen) > 0 {
			a.CPU.Cores = len(seen)
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			a.CPU.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "MemTotal:":
				a.Memory.TotalMB = kb / 1024
			case "MemAvailable:":
				a.Memory.AvailableMB = kb / 1024
			case "SwapTotal:":
				a.Memory.SwapMB = kb / 1024
			}
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s, _ := strconv.ParseFloat(f[0], 64)
			a.UptimeHours = round1(s / 3600)
		}
	}
	if runtime.GOOS == "darwin" {
		a.CPU.Model = run(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		if n, err := strconv.Atoi(run(ctx, "sysctl", "-n", "hw.physicalcpu")); err == nil {
			a.CPU.Cores = n
		}
		if n, err := strconv.ParseInt(run(ctx, "sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
			a.Memory.TotalMB = int(n >> 20)
		}
		if f := strings.Fields(strings.Trim(run(ctx, "sysctl", "-n", "vm.loadavg"), "{} ")); len(f) > 0 {
			a.CPU.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
		// kern.boottime: "{ sec = 1730000000, usec = 0 } Mon Nov  3 ..."
		if _, rest, ok := strings.Cut(run(ctx, "sysctl", "-n", "kern.boottime"), "sec = "); ok {
			if sec, err := strconv.ParseInt(strings.TrimRight(strings.Fields(rest)[0], ","), 10, 64); err == nil {
				a.UptimeHours = round1(time.Since(time.Unix(sec, 0)).Hours())
			}
		}
	}

	if b, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft") {
		a.Environment = "wsl2"
	} else if _, err := os.Stat("/.dockerenv"); err == nil {
		a.Environment = "container"
	} else if v := run(ctx, "systemd-detect-virt", "--vm"); v != "" && v != "none" {
		a.Environment = "vm:" + v
	}
	if m, _ := filepath.Glob("/sys/class/power_supply/BAT*"); len(m) > 0 {
		a.Laptop = true
	} else if runtime.GOOS == "darwin" && strings.Contains(run(ctx, "pmset", "-g", "batt"), "InternalBattery") {
		a.Laptop = true
	}

	root := "/"
	if runtime.GOOS == "windows" {
		root = os.Getenv("SystemDrive") + `\`
	}
	if total, free, err := diskUsage(root); err == nil {
		a.Disks = append(a.Disks, api.AssessDisk{Path: root, Role: "root", TotalGB: total, FreeGB: free})
	}
}

func probeRuntime(ctx context.Context, a *api.Assessment) {
	a.Runtime.Docker = run(ctx, "docker", "version", "-f", "{{.Server.Version}}")
	if a.Runtime.Docker == "" {
		return
	}
	rts := run(ctx, "docker", "info", "-f", "{{json .Runtimes}}")
	a.Runtime.NvidiaRuntime = strings.Contains(rts, `"nvidia"`)
	a.Runtime.GVisor = strings.Contains(rts, `"runsc"`)
	if a.Runtime.GVisor && a.GPUDriver != "" {
		a.Runtime.GVisorGPU = strings.Contains(run(ctx, "runsc", "nvproxy", "list-supported-drivers"), a.GPUDriver)
	}
	if dir := run(ctx, "docker", "info", "-f", "{{.DockerRootDir}}"); dir != "" {
		if total, free, err := diskUsage(dir); err == nil {
			if len(a.Disks) > 0 && sameFS(a.Disks[0].Path, dir) {
				a.Disks[0].Role = "root+docker"
			} else {
				a.Disks = append(a.Disks, api.AssessDisk{Path: dir, Role: "docker", TotalGB: total, FreeGB: free})
			}
		}
	}
}

func nvidiaDriver(ctx context.Context) string {
	out := run(ctx, "nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader")
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first)
}

// attachDiskSpeed records a benchmark result on the disk entry that holds dir.
func attachDiskSpeed(a *api.Assessment, dir string, mbps float64) {
	for i := range a.Disks {
		if sameFS(a.Disks[i].Path, dir) {
			a.Disks[i].WriteMBps = mbps
			return
		}
	}
	total, free, _ := diskUsage(dir)
	a.Disks = append(a.Disks, api.AssessDisk{Path: dir, Role: "benchmark", TotalGB: total, FreeGB: free, WriteMBps: mbps})
}

// sharingDisk is the filesystem ogm workloads use (Docker's, else root).
func sharingDisk(a *api.Assessment) *api.AssessDisk {
	for i := range a.Disks {
		if strings.Contains(a.Disks[i].Role, "docker") {
			return &a.Disks[i]
		}
	}
	if len(a.Disks) > 0 {
		return &a.Disks[0]
	}
	return nil
}

// knownApps maps process-name fragments to a usage profile.
var knownApps = []struct{ match, name, profile string }{
	{"steam", "Steam", "gaming"}, {"epicgameslauncher", "Epic Games", "gaming"}, {"battle.net", "Battle.net", "gaming"},
	{"riotclient", "Riot Client", "gaming"}, {"valorant", "Valorant", "gaming"}, {"leagueclient", "League of Legends", "gaming"},
	{"eadesktop", "EA app", "gaming"}, {"galaxyclient", "GOG Galaxy", "gaming"}, {"upc", "Ubisoft Connect", "gaming"},
	{"acad", "AutoCAD", "workstation"}, {"sldworks", "SolidWorks", "workstation"}, {"fusion360", "Fusion 360", "workstation"},
	{"revit", "Revit", "workstation"}, {"inventor", "Inventor", "workstation"}, {"rhino", "Rhino", "workstation"},
	{"freecad", "FreeCAD", "workstation"}, {"3dsmax", "3ds Max", "workstation"}, {"maya", "Maya", "workstation"},
	{"houdini", "Houdini", "workstation"}, {"xtop", "Creo", "workstation"}, {"cnext", "CATIA", "workstation"},
	{"blender", "Blender", "creative"}, {"resolve", "DaVinci Resolve", "creative"}, {"adobe premiere", "Premiere Pro", "creative"},
	{"afterfx", "After Effects", "creative"}, {"photoshop", "Photoshop", "creative"}, {"obs64", "OBS", "creative"}, {"obs", "OBS", "creative"},
	{"code", "VS Code", "dev"}, {"idea", "IntelliJ", "dev"}, {"pycharm", "PyCharm", "dev"}, {"goland", "GoLand", "dev"},
	{"ollama", "Ollama", "ml"}, {"lm studio", "LM Studio", "ml"}, {"llama-server", "llama.cpp", "ml"}, {"vllm", "vLLM", "ml"},
}

// pauseProcesses are process names worth pausing on, per profile (only when actually seen,
// except for workstation/creative apps, which run only while being used).
var pauseProcesses = map[string][]string{
	"gaming":      {"valorant", "leagueclient"},
	"workstation": {"acad", "sldworks", "fusion360", "revit", "inventor", "rhino", "3dsmax", "maya", "houdini"},
	"creative":    {"blender", "resolve", "afterfx"},
}

// detectApps returns human names of recognised apps and the profiles they imply.
func detectApps(ctx context.Context) (apps, profiles []string) {
	procs := agent.RunningProcesses(ctx)
	seenApp, seenProf := map[string]bool{}, map[string]bool{}
	for p := range procs {
		for _, k := range knownApps {
			if matchProc(p, k.match) && !seenApp[k.name] {
				seenApp[k.name], seenProf[k.profile] = true, true
			}
		}
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".steam"), filepath.Join(home, ".local/share/Steam"), "/mnt/c/Program Files (x86)/Steam"} {
		if _, err := os.Stat(p); err == nil && !seenApp["Steam"] {
			seenApp["Steam (installed)"], seenProf["gaming"] = true, true
			break
		}
	}
	for k := range seenApp {
		apps = append(apps, k)
	}
	for k := range seenProf {
		profiles = append(profiles, k)
	}
	sort.Strings(apps)
	sort.Strings(profiles)
	return apps, profiles
}

// matchProc matches short names exactly (avoids "code" matching "vscode-server-helper"
// noise or "upc" matching random words) and longer ones as prefixes.
func matchProc(proc, frag string) bool {
	proc = strings.TrimSuffix(proc, ".exe")
	if len(frag) <= 4 {
		return proc == frag
	}
	return strings.HasPrefix(proc, frag)
}
