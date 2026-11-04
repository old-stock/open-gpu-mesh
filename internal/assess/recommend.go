package assess

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// Profiles are the owner usage profiles `ogm assess --profile` accepts.
var Profiles = []string{"general", "gaming", "workstation", "creative", "dev", "ml", "server"}

var profileAliases = map[string]string{"cad": "workstation", "game": "gaming", "gamer": "gaming", "ai": "ml",
	"video": "creative", "homelab": "server", "always-on": "server", "developer": "dev", "light": "general"}

// reserve is what the owner keeps for themselves under a profile.
type reserve struct {
	cores, memGB, diskGB int
	why                  string
}

var reserves = map[string]reserve{
	"general":     {2, 4, 50, "everyday use (browser, office, updates)"},
	"gaming":      {4, 16, 200, "games and their updates (keep at least 200 GB free)"},
	"workstation": {8, 32, 100, "CAD/workstation apps (keep 8 cores and 32 GB RAM free)"},
	"creative":    {8, 16, 150, "video/3D work and scratch space"},
	"dev":         {4, 16, 100, "builds, containers and IDEs"},
	"ml":          {2, 8, 100, "your own models (ogm already avoids VRAM your models are using)"},
	"server":      {2, 4, 50, "the services this machine already runs"},
}

// normalizeProfiles lower-cases, resolves aliases, drops unknowns and duplicates.
func normalizeProfiles(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		for _, q := range strings.Split(p, ",") {
			q = strings.ToLower(strings.TrimSpace(q))
			if a, ok := profileAliases[q]; ok {
				q = a
			}
			if _, ok := reserves[q]; ok && !seen[q] {
				seen[q] = true
				out = append(out, q)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ValidProfile reports whether p (or its alias) is a known profile.
func ValidProfile(p string) bool { return len(normalizeProfiles([]string{p})) == 1 }

func recommend(a *api.Assessment) {
	add := func(level, topic, f string, args ...any) {
		a.Recommendations = append(a.Recommendations, api.Recommendation{Level: level, Topic: topic, Text: fmt.Sprintf(f, args...)})
	}

	// Owner reservation = the most demanding of the selected profiles.
	keep := reserve{}
	var whys []string
	for _, p := range a.Profiles {
		r := reserves[p]
		keep.cores, keep.memGB, keep.diskGB = max(keep.cores, r.cores), max(keep.memGB, r.memGB), max(keep.diskGB, r.diskGB)
		whys = append(whys, r.why)
	}
	// Servers and ML boxes already run things: keep what they use right now, plus headroom.
	if has(a.Profiles, "server") || has(a.Profiles, "ml") {
		if a.Memory.AvailableMB > 0 {
			if used := (a.Memory.TotalMB-a.Memory.AvailableMB)/1024 + 2; used > keep.memGB {
				keep.memGB = used
				whys = append(whys, fmt.Sprintf("%d GB RAM is in use right now", used-2))
			}
		}
		if busy := int(a.CPU.Load1+0.999) + 1; busy > keep.cores && a.CPU.Load1 > 0.5 {
			keep.cores = busy
		}
	}
	add("info", "usage", "Keeping %d cores, %d GB RAM and %d GB disk for you: %s.", keep.cores, keep.memGB, keep.diskGB, strings.Join(whys, "; "))

	s := &a.Suggested
	totalMemGB := a.Memory.TotalMB / 1024

	// CPU
	s.MaxCPU = a.CPU.Threads - keep.cores
	if s.MaxCPU < 1 {
		s.MaxCPU = 1
		add("warn", "cpu", "This machine has %d threads, fewer than the %d you should keep. Share it only outside your working hours (--window).", a.CPU.Threads, keep.cores)
	}
	// Memory
	s.MaxMemoryMB = a.Memory.TotalMB - keep.memGB*1024
	if s.MaxMemoryMB < 2048 {
		s.MaxMemoryMB = max(a.Memory.TotalMB/4, 1024)
		add("warn", "memory", "Only %d GB RAM: after keeping %d GB for yourself there is little left. Sharing %d MB; consider a --window so workloads run when you're away.", totalMemGB, keep.memGB, s.MaxMemoryMB)
	}
	if a.Environment == "wsl2" {
		add("info", "memory", "WSL2 sees %d GB. WSL caps memory at 50%% of the PC by default; raise `memory=` in %%UserProfile%%\\.wslconfig to share more.", totalMemGB)
	}
	// Disk
	if d := sharingDisk(a); d != nil {
		s.MaxDiskGB = d.FreeGB - keep.diskGB
		if s.MaxDiskGB < 20 {
			add("warn", "disk", "%s has %d GB free; after keeping %d GB for you only %d GB is left for container images. Free up space or add a disk.", d.Path, d.FreeGB, keep.diskGB, max(s.MaxDiskGB, 0))
			s.MaxDiskGB = max(s.MaxDiskGB, 10)
		}
		if d.WriteMBps > 0 && d.WriteMBps < 150 {
			add("warn", "disk", "Disk writes at %.0f MB/s (likely a hard drive or a busy disk): large image pulls and model downloads will be slow.", d.WriteMBps)
		}
	}
	// GPU
	switch {
	case len(a.GPUs) == 0:
		s.MaxGPUs = -1
		add("info", "gpu", "No NVIDIA GPU found: this will be a CPU-only node and runs workloads with gpu_count: 0.")
	default:
		names := []string{}
		for _, g := range a.GPUs {
			names = append(names, fmt.Sprintf("%s (%d GB)", g.Name, g.VRAMMB/1024))
		}
		add("info", "gpu", "GPUs: %s, driver %s.", strings.Join(names, ", "), orDash(a.GPUDriver))
		for _, g := range a.GPUs {
			if g.VRAMMB > 0 && g.MemUsedMB*2 > g.VRAMMB {
				add("info", "gpu", "GPU %d: %d of %d GB is in use by your own work; ogm only places GPU workloads that fit in the free %d GB.", g.Index, g.MemUsedMB/1024, g.VRAMMB/1024, (g.VRAMMB-g.MemUsedMB)/1024)
			}
		}
		if has(a.Profiles, "ml") && len(a.GPUs) > 1 {
			s.MaxGPUs = len(a.GPUs) - 1
			add("info", "gpu", "Keeping one GPU for your own models (--max-gpus %d).", s.MaxGPUs)
		}
		if a.Runtime.Docker != "" && !a.Runtime.NvidiaRuntime {
			add("error", "runtime", "Docker has no NVIDIA runtime: install the NVIDIA Container Toolkit, then `sudo nvidia-ctk runtime configure --runtime=docker && sudo systemctl restart docker`.")
		}
		if a.Runtime.GVisor && !a.Runtime.GVisorGPU {
			add("warn", "runtime", "gVisor is installed but doesn't support NVIDIA driver %s; GPU workloads can't use --gvisor until the driver or runsc is updated.", orDash(a.GPUDriver))
		}
	}
	if a.Runtime.Docker == "" {
		add("error", "runtime", "Docker isn't installed or reachable. Install Docker Engine: https://docs.docker.com/engine/install/")
	}
	if a.Runtime.GVisor && (len(a.GPUs) == 0 || a.Runtime.GVisorGPU) {
		add("info", "runtime", "gVisor is available: run the agent with --gvisor for stronger isolation.")
	}

	// Usage-driven pausing
	if has(a.Profiles, "gaming") {
		s.PauseOnProcess = append(s.PauseOnProcess, pauseProcesses["gaming"]...)
		if len(a.GPUs) > 0 {
			add("info", "usage", "Gaming: the agent yields the GPU when it gets busy (--yield-gpu-util 30). Add your games to --pause-on-process to pause everything while you play.")
		} else {
			add("info", "usage", "Gaming: add your games to --pause-on-process so sharing pauses while you play.")
		}
		add("info", "usage", "Some anti-cheat systems dislike virtualization; if a game complains, pause sharing (`ogm agent pause`) while you play.")
	}
	for _, p := range []string{"workstation", "creative"} {
		if has(a.Profiles, p) {
			s.PauseOnProcess = append(s.PauseOnProcess, pauseProcesses[p]...)
		}
	}
	if len(s.PauseOnProcess) > 0 {
		s.PauseOnProcess = dedupe(s.PauseOnProcess)
	}
	if a.Laptop {
		s.Window = "23:00-07:00"
		add("warn", "usage", "This is a laptop: share it only while it's plugged in, and preferably overnight (suggested --window %s).", s.Window)
	}
	if a.CPU.Load1 > float64(a.CPU.Threads)*0.5 {
		add("info", "cpu", "The machine was busy during the test (load %.1f), so benchmark and speed results may be low.", a.CPU.Load1)
	}

	// Network
	n := a.Network
	// The bottleneck is the slower of the two paths (a hub on the same LAN measures LAN speed).
	up, down, rtt := minNonZero(n.HubUpMbps, n.InternetUpMbps), minNonZero(n.HubDownMbps, n.InternetDownMbps), n.HubRTTms
	if up == 0 && down == 0 {
		add("warn", "network", "No speed test ran (no hub token and --internet=false), so the network tier is a guess.")
	} else {
		add("info", "network", "Measured %.0f Mbps down / %.0f Mbps up.", down, up)
		if up < 10 {
			add("warn", "network", "Upload under 10 Mbps: fine for batch jobs, but serving responses or hosting storage from here will be slow.")
		}
		if down < 25 {
			add("warn", "network", "Download under 25 Mbps: pulling multi-GB images and models will take a long time; prefer small images.")
		}
	}
	if rtt > 150 {
		add("info", "network", "%.0f ms to the hub: fine for jobs, noticeable for interactive endpoints.", rtt)
	}
	switch n.NAT {
	case "symmetric":
		add("warn", "network", "Symmetric NAT: direct connections to other nodes will need a relay. Enabling UPnP or forwarding a UDP port on your router helps.")
	case "udp-blocked":
		add("warn", "network", "Outbound UDP looks blocked: peer-to-peer traffic will fall back to slower relays.")
	case "no-internet":
		add("warn", "network", "This machine couldn't reach the internet (only the hub). Images can still come from the hub's registry, but workloads that download models or call external APIs won't work here.")
	case "none":
		add("info", "network", "This machine has a public IP (no NAT): it can act as a relay for others.")
	}

	// Tier
	a.Tier, a.TierReasons = tier(a, up, down, rtt)
	s.AgentFlags = agentFlags(a)
}

func tier(a *api.Assessment, up, down, rtt float64) (string, []string) {
	if a.CPU.Threads < 2 || a.Memory.TotalMB < 3500 {
		return "unsuitable", []string{"fewer than 2 threads or under 4 GB RAM"}
	}
	var why []string
	burst := false
	if a.Laptop {
		burst, why = true, append(why, "laptop (intermittent, battery)")
	}
	if up > 0 && up < 10 {
		burst, why = true, append(why, fmt.Sprintf("upload %.0f Mbps < 10", up))
	}
	if a.Network.NAT == "udp-blocked" {
		burst, why = true, append(why, "UDP blocked")
	}
	if burst {
		return "burst", why
	}
	free := 0
	if d := sharingDisk(a); d != nil {
		free = d.FreeGB
	}
	alwaysOn := has(a.Profiles, "server") || a.UptimeHours >= 72
	tested := a.Network.InternetDownMbps > 0 || a.Network.NAT != ""
	online := tested && a.Network.NAT != "no-internet"
	if up >= 50 && down >= 100 && free >= 200 && alwaysOn && online && (rtt == 0 || rtt < 150) && a.Network.NAT != "symmetric" {
		why = append(why, fmt.Sprintf("%.0f/%.0f Mbps", down, up), fmt.Sprintf("%d GB free", free))
		if has(a.Profiles, "server") {
			why = append(why, "always-on server profile")
		} else {
			why = append(why, fmt.Sprintf("up %.0f h", a.UptimeHours))
		}
		return "anchor", why
	}
	switch {
	case !tested:
		why = append(why, "internet not tested (anchors need a measured internet connection; drop --internet=false)")
	case !online:
		why = append(why, "no internet access (speeds are to the hub only)")
	}
	if up < 50 {
		why = append(why, "upload below the 50 Mbps anchor threshold")
	}
	if !alwaysOn {
		why = append(why, "not known to be always on (use --profile server if it is)")
	}
	if free < 200 {
		why = append(why, "under 200 GB free for storage")
	}
	return "compute", why
}

func agentFlags(a *api.Assessment) string {
	s := a.Suggested
	f := []string{"--max-cpu " + strconv.Itoa(s.MaxCPU), "--max-memory-mb " + strconv.Itoa(s.MaxMemoryMB), "--max-disk-gb " + strconv.Itoa(s.MaxDiskGB)}
	if s.MaxGPUs > 0 {
		f = append(f, "--max-gpus "+strconv.Itoa(s.MaxGPUs))
	}
	if has(a.Profiles, "gaming") && len(a.GPUs) > 0 {
		f = append(f, "--yield-gpu-util 30")
	}
	if len(s.PauseOnProcess) > 0 {
		f = append(f, "--pause-on-process "+strings.Join(s.PauseOnProcess, ","))
	}
	if s.Window != "" {
		f = append(f, "--window "+s.Window)
	}
	if a.Runtime.GVisor && (len(a.GPUs) == 0 || a.Runtime.GVisorGPU) {
		f = append(f, "--gvisor")
	}
	return strings.Join(f, " ")
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func minNonZero(a, b float64) float64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}
