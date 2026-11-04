// Package assess audits a machine during onboarding: hardware inventory, short CPU/disk
// benchmarks, network speed (to the hub and the internet), NAT type, and usage-profile-based
// recommendations for what the owner should keep for themselves.
package assess

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/agent"
	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// Options controls an assessment run.
type Options struct {
	Server   string   // hub URL; enables the hub latency/speed test when set together with Token
	Token    string   // node token (ogmn_...) or user API key, sent as a Bearer token
	Profiles []string // owner usage profiles; empty = detect from running/installed apps
	Internet bool     // also test against public endpoints (Cloudflare speed test, STUN)
	Disk     bool     // run the disk write benchmark
	Quick    bool     // shorter tests (about half the time)
	DiskPath string   // where to write the disk benchmark file (default: system temp dir)
	GPUs     agent.GPUSource
	Log      io.Writer // progress lines; nil = silent
}

// Run performs the assessment. Individual checks that fail are recorded in Errors; Run
// itself never fails.
func Run(ctx context.Context, o Options) *api.Assessment {
	start := time.Now()
	a := &api.Assessment{Time: start.UTC(), Version: api.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Errors: map[string]string{}}
	logf := func(f string, args ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, f+"\n", args...)
		}
	}
	fail := func(check string, err error) {
		a.Errors[check] = err.Error()
		logf("    %s: %v", check, err)
	}
	if o.GPUs == nil {
		o.GPUs = agent.NvidiaSMI{}
	}
	benchDur := 2 * time.Second
	netDur := 6 * time.Second
	if o.Quick {
		benchDur, netDur = time.Second, 3*time.Second
	}

	logf("==> system")
	probeSystem(ctx, a)
	a.GPUs = o.GPUs.GPUs(ctx)
	if a.GPUs == nil {
		a.GPUs = []api.GPU{}
	}
	if len(a.GPUs) > 0 {
		a.GPUDriver = nvidiaDriver(ctx)
	}
	probeRuntime(ctx, a)

	logf("==> cpu benchmark (%s)", 2*benchDur)
	a.CPU.SingleMBps, a.CPU.MultiMBps = cpuBench(ctx, benchDur, a.CPU.Threads)

	if o.Disk {
		dir := o.DiskPath
		if dir == "" {
			dir = os.TempDir()
		}
		logf("==> disk write benchmark (%s)", dir)
		size := int64(512 << 20)
		if o.Quick {
			size = 256 << 20
		}
		if mbps, err := diskBench(dir, size); err != nil {
			fail("disk_benchmark", err)
		} else {
			attachDiskSpeed(a, dir, mbps)
		}
	}

	hc := &http.Client{Timeout: 0}
	if o.Server != "" && o.Token != "" {
		logf("==> hub network test (%s)", strings.TrimRight(o.Server, "/"))
		hub := hubTarget(o.Server, o.Token)
		if rtt, err := measureRTT(ctx, hc, hub); err != nil {
			fail("hub_rtt", err)
		} else {
			a.Network.HubRTTms = rtt
			if a.Network.HubDownMbps, err = measureDown(ctx, hc, hub, netDur); err != nil {
				fail("hub_download", err)
			}
			if a.Network.HubUpMbps, err = measureUp(ctx, hc, hub, netDur); err != nil {
				fail("hub_upload", err)
			}
		}
	}
	if o.Internet {
		logf("==> internet network test (speed.cloudflare.com)")
		cf := cloudflareTarget()
		if rtt, err := measureRTT(ctx, hc, cf); err != nil {
			fail("internet_rtt", err)
		} else {
			a.Network.InternetRTTms = rtt
			if a.Network.InternetDownMbps, err = measureDown(ctx, hc, cf, netDur); err != nil {
				fail("internet_download", err)
			}
			if a.Network.InternetUpMbps, err = measureUp(ctx, hc, cf, netDur); err != nil {
				fail("internet_upload", err)
			}
		}
		logf("==> NAT type (STUN)")
		a.Network.NAT = natType(ctx)
		if a.Network.NAT == "udp-blocked" && a.Network.InternetRTTms == 0 {
			a.Network.NAT = "no-internet" // nothing external answered at all, not just UDP
		}
		a.Network.IPv6 = hasIPv6()
	}

	logf("==> usage profile")
	apps, detected := detectApps(ctx)
	a.DetectedApps = apps
	a.Profiles = normalizeProfiles(o.Profiles)
	if len(a.Profiles) == 0 {
		a.Profiles = detected
	}
	if len(a.Profiles) == 0 {
		a.Profiles = []string{"general"}
	}
	recommend(a)
	if len(a.Errors) == 0 {
		a.Errors = nil
	}
	a.DurationSec = round1(time.Since(start).Seconds())
	return a
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
