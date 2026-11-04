package assess

import (
	"fmt"
	"io"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// Render writes a human-readable report.
func Render(w io.Writer, a *api.Assessment) {
	p := func(f string, args ...any) { fmt.Fprintf(w, f+"\n", args...) }
	env := a.OS + "/" + a.Arch
	if a.Environment != "" {
		env += " (" + a.Environment + ")"
	}
	if a.Laptop {
		env += ", laptop"
	}
	p("")
	p("Machine     %s, kernel %s, up %.0f h", env, orDash(a.Kernel), a.UptimeHours)
	p("CPU         %s: %d cores / %d threads, load %.1f", orDash(a.CPU.Model), a.CPU.Cores, a.CPU.Threads, a.CPU.Load1)
	p("            SHA-256 %.0f MB/s single, %.0f MB/s all threads", a.CPU.SingleMBps, a.CPU.MultiMBps)
	mem := fmt.Sprintf("%d GB total", a.Memory.TotalMB/1024)
	if a.Memory.AvailableMB > 0 {
		mem += fmt.Sprintf(", %d GB available", a.Memory.AvailableMB/1024)
	}
	if a.Memory.SwapMB > 0 {
		mem += fmt.Sprintf(", %d GB swap", a.Memory.SwapMB/1024)
	}
	p("Memory      %s", mem)
	for _, d := range a.Disks {
		speed := ""
		if d.WriteMBps > 0 {
			speed = fmt.Sprintf(", writes %.0f MB/s", d.WriteMBps)
		}
		p("Disk        %s (%s): %d GB free of %d GB%s", d.Path, d.Role, d.FreeGB, d.TotalGB, speed)
	}
	if len(a.GPUs) == 0 {
		p("GPU         none (CPU-only)")
	}
	for _, g := range a.GPUs {
		mem := fmt.Sprintf("%d GB VRAM, %d GB in use", g.VRAMMB/1024, g.MemUsedMB/1024)
		if g.Unified {
			mem = fmt.Sprintf("%d GB unified memory, %d GB in use", g.VRAMMB/1024, g.MemUsedMB/1024)
		}
		p("GPU %-7d %s: %s, driver %s", g.Index, g.Name, mem, orDash(a.GPUDriver))
	}
	rt := "Docker " + orDash(a.Runtime.Docker)
	if a.Runtime.NvidiaRuntime {
		rt += ", NVIDIA runtime"
	}
	if a.Runtime.GVisor {
		rt += ", gVisor"
		if a.Runtime.GVisorGPU {
			rt += " (GPU ok)"
		}
	}
	p("Runtime     %s", rt)
	n := a.Network
	if n.HubRTTms > 0 || n.HubDownMbps > 0 {
		p("Hub         %.0f ms, %.0f Mbps down / %.0f Mbps up", n.HubRTTms, n.HubDownMbps, n.HubUpMbps)
	}
	if n.InternetRTTms > 0 || n.InternetDownMbps > 0 {
		p("Internet    %.0f ms, %.0f Mbps down / %.0f Mbps up", n.InternetRTTms, n.InternetDownMbps, n.InternetUpMbps)
	}
	if n.NAT != "" {
		v6 := "no IPv6"
		if n.IPv6 {
			v6 = "IPv6"
		}
		p("NAT         %s, %s", n.NAT, v6)
	}
	if len(a.DetectedApps) > 0 {
		p("Detected    %s", strings.Join(a.DetectedApps, ", "))
	}
	p("Profile     %s", strings.Join(a.Profiles, ", "))
	p("")
	p("Tier        %s (%s)", strings.ToUpper(a.Tier), strings.Join(a.TierReasons, "; "))
	p("")
	for _, r := range a.Recommendations {
		mark := map[string]string{"info": "  -", "warn": "  !", "error": " !!"}[r.Level]
		p("%s %s", mark, r.Text)
	}
	for k, v := range a.Errors {
		p("  ? %s skipped: %s", k, v)
	}
	p("")
	p("Suggested agent flags:")
	p("  %s", a.Suggested.AgentFlags)
	p("")
}
