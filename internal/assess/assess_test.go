package assess

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

func TestParseStunResponse(t *testing.T) {
	txid := []byte("abcdefghijkl")
	b := make([]byte, 20+12)
	binary.BigEndian.PutUint16(b[0:], 0x0101)
	binary.BigEndian.PutUint16(b[2:], 12)
	binary.BigEndian.PutUint32(b[4:], stunMagic)
	copy(b[8:], txid)
	// XOR-MAPPED-ADDRESS 203.0.113.7:54321
	binary.BigEndian.PutUint16(b[20:], 0x0020)
	binary.BigEndian.PutUint16(b[22:], 8)
	b[25] = 0x01
	binary.BigEndian.PutUint16(b[26:], 54321^(stunMagic>>16))
	binary.BigEndian.PutUint32(b[28:], binary.BigEndian.Uint32([]byte{203, 0, 113, 7})^stunMagic)
	got, ok := parseStunResponse(b, txid)
	if !ok || got != netip.MustParseAddrPort("203.0.113.7:54321") {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := parseStunResponse(b, []byte("wrongtxid123")); ok {
		t.Fatal("accepted a response for another transaction")
	}
}

func base() *api.Assessment {
	return &api.Assessment{
		CPU:     api.AssessCPU{Cores: 8, Threads: 16},
		Memory:  api.AssessMemory{TotalMB: 64 * 1024},
		Disks:   []api.AssessDisk{{Path: "/", Role: "root+docker", TotalGB: 2000, FreeGB: 1000, WriteMBps: 2000}},
		GPUs:    []api.GPU{{Name: "RTX 4090", VRAMMB: 24564}},
		Runtime: api.AssessRuntime{Docker: "27.0", NvidiaRuntime: true},
		Network: api.AssessNetwork{HubDownMbps: 500, HubUpMbps: 200, HubRTTms: 20, NAT: "endpoint-independent"},
	}
}

func TestRecommendWorkstation(t *testing.T) {
	a := base()
	a.Profiles = normalizeProfiles([]string{"cad,gaming"})
	recommend(a)
	s := a.Suggested
	if s.MaxCPU != 8 || s.MaxMemoryMB != 32*1024 || s.MaxDiskGB != 800 {
		t.Fatalf("limits: %+v", s)
	}
	for _, want := range []string{"--yield-gpu-util 30", "sldworks", "--max-cpu 8"} {
		if !strings.Contains(s.AgentFlags, want) {
			t.Fatalf("flags %q missing %q", s.AgentFlags, want)
		}
	}
	if a.Tier != "compute" { // not known to be always on
		t.Fatalf("tier %s %v", a.Tier, a.TierReasons)
	}
}

func TestTiers(t *testing.T) {
	a := base()
	a.Profiles = []string{"server"}
	recommend(a)
	if a.Tier != "anchor" {
		t.Fatalf("server with fast uplink: tier %s %v", a.Tier, a.TierReasons)
	}

	a = base()
	a.Profiles, a.Laptop = []string{"general"}, true
	recommend(a)
	if a.Tier != "burst" || a.Suggested.Window == "" {
		t.Fatalf("laptop: tier %s window %q", a.Tier, a.Suggested.Window)
	}

	a = base()
	a.Profiles, a.GPUs, a.Runtime.Docker = []string{"general"}, nil, ""
	a.CPU.Threads, a.Memory.TotalMB = 1, 2048
	recommend(a)
	if a.Tier != "unsuitable" || a.Suggested.MaxGPUs != -1 {
		t.Fatalf("tiny box: tier %s gpus %d", a.Tier, a.Suggested.MaxGPUs)
	}
	var sawDocker bool
	for _, r := range a.Recommendations {
		sawDocker = sawDocker || (r.Level == "error" && strings.Contains(r.Text, "Docker"))
	}
	if !sawDocker {
		t.Fatal("missing Docker error")
	}
}

func TestProfiles(t *testing.T) {
	if got := normalizeProfiles([]string{"CAD", "gamer,ai", "bogus"}); strings.Join(got, ",") != "gaming,ml,workstation" {
		t.Fatalf("got %v", got)
	}
	if !matchProc("steam.exe", "steam") || matchProc("vscode-helper", "code") || !matchProc("code", "code") {
		t.Fatal("matchProc")
	}
}

func TestServerKeepsCurrentUsage(t *testing.T) {
	a := base()
	a.Profiles = []string{"ml", "server"}
	a.Memory = api.AssessMemory{TotalMB: 119 * 1024, AvailableMB: 5 * 1024}
	a.GPUs = []api.GPU{{Name: "NVIDIA GB10", VRAMMB: 119 * 1024, MemUsedMB: 113 * 1024, Unified: true}}
	a.Network.NAT = "no-internet"
	recommend(a)
	if a.Suggested.MaxMemoryMB > 8*1024 {
		t.Fatalf("shares %d MB on a box with 5 GB free", a.Suggested.MaxMemoryMB)
	}
	if a.Tier != "compute" {
		t.Fatalf("no-internet LAN node should be compute (not burst, not anchor): %s %v", a.Tier, a.TierReasons)
	}
}

func TestTierNeedsInternetTest(t *testing.T) {
	a := base()
	a.Profiles = []string{"server"}
	a.Network.NAT = "" // --internet=false
	recommend(a)
	if a.Tier != "compute" || !strings.Contains(strings.Join(a.TierReasons, " "), "internet not tested") {
		t.Fatalf("untested internet: tier %s %v", a.Tier, a.TierReasons)
	}
}
