package server

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

func TestQueueAutoscaleTarget(t *testing.T) {
	a := api.QueueAutoscaler{MinReplicas: 0, MaxReplicas: 10, DesiredQueueLength: 4, PollingPeriod: 60, MaxUpscalePerMinute: 100, MaxDownscalePerMinute: 100}
	cases := []struct{ cur, q, want int }{
		{0, 0, 0}, {0, 1, 1}, {0, 4, 1}, {0, 5, 2}, {3, 100, 10}, {10, 0, 0},
	}
	for _, c := range cases {
		if got := QueueAutoscaleTarget(a, c.cur, c.q); got != c.want {
			t.Errorf("cur=%d q=%d: got %d want %d", c.cur, c.q, got, c.want)
		}
	}
	a.MaxUpscalePerMinute, a.MaxDownscalePerMinute, a.PollingPeriod = 2, 1, 30
	if got := QueueAutoscaleTarget(a, 0, 100); got != 1 {
		t.Errorf("rate-limited upscale: got %d want 1", got)
	}
	if got := QueueAutoscaleTarget(a, 8, 0); got != 7 {
		t.Errorf("rate-limited downscale: got %d want 7", got)
	}
	a.MinReplicas = 2
	if got := QueueAutoscaleTarget(a, 2, 0); got != 2 {
		t.Errorf("min replicas: got %d", got)
	}
}

func TestMergePatch(t *testing.T) {
	var target, patch, want any
	json.Unmarshal([]byte(`{"a":"b","c":{"d":"e","f":"g"},"keep":1}`), &target)
	json.Unmarshal([]byte(`{"a":"z","c":{"f":null},"new":[1]}`), &patch)
	json.Unmarshal([]byte(`{"a":"z","c":{"d":"e"},"keep":1,"new":[1]}`), &want)
	if got := mergePatch(target, patch); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestBackoff(t *testing.T) {
	qs := api.QueueSettings{RetryBackoffBaseS: 2, RetryBackoffMaxS: 30}
	for attempt, base := range map[int]float64{1: 2, 2: 4, 3: 8, 4: 16, 5: 30, 10: 30} {
		d := backoff(qs, attempt).Seconds()
		if d < base*0.8-0.01 || d > base*1.2+0.01 {
			t.Errorf("attempt %d: %.2fs outside jitter range of %.0fs", attempt, d, base)
		}
	}
}

func TestLastFire(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 30, 0, 0, time.Local) // Wednesday
	got, ok := lastFire("0 9 * * 1-5", now)
	if !ok || !got.Equal(time.Date(2026, 11, 4, 9, 0, 0, 0, time.Local)) {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := lastFire("not a cron", now); ok {
		t.Fatal("bad cron accepted")
	}
}

func TestRuntimeFingerprint(t *testing.T) {
	a := api.ContainerGroupSpec{Name: "x", Container: api.ContainerSpec{Image: "img:1"}, Networking: &api.Networking{Port: 8080}}
	b := a
	b.Networking = &api.Networking{Port: 8080, Auth: true, LoadBalancer: "round_robin"}
	b.Replicas = 9
	if runtimeFingerprint(a) != runtimeFingerprint(b) {
		t.Error("networking/replicas change should not bump version")
	}
	b.Networking = &api.Networking{Port: 9090}
	if runtimeFingerprint(a) == runtimeFingerprint(b) {
		t.Error("port change must recreate containers")
	}
	b.Container.Image = "img:2"
	if runtimeFingerprint(a) == runtimeFingerprint(b) {
		t.Error("image change should bump version")
	}
}

func TestMatchingGPUsRespectsOwnerVRAM(t *testing.T) {
	gpus := []api.GPU{
		{Index: 0, Name: "RTX 3090", VRAMMB: 24576, MemUsedMB: 22222}, // owner runs a local LLM
		{Index: 1, Name: "RTX 4090", VRAMMB: 24564, MemUsedMB: 300},
	}
	free := map[int]bool{0: true, 1: true}
	m := matchingGPUs(gpus, api.Resources{GPUCount: 1, MinVRAMMB: 8000}, free)
	if len(m) != 1 || m[0].Index != 1 {
		t.Fatalf("expected only the idle 4090, got %+v", m)
	}
	// Small requests still fit beside the owner's workload.
	if m := matchingGPUs(gpus, api.Resources{GPUCount: 1, MinVRAMMB: 2000}, free); len(m) != 2 {
		t.Fatalf("2 GB request should fit both, got %d", len(m))
	}
	// Eligibility (free == nil) ignores current usage: the node could fit it later.
	if m := matchingGPUs(gpus, api.Resources{GPUCount: 1, MinVRAMMB: 8000}, nil); len(m) != 2 {
		t.Fatalf("eligibility should ignore usage, got %d", len(m))
	}
}

func TestFitsShared(t *testing.T) {
	c := &nodeCap{
		node: &store.NodeRow{Info: store.NodeInfo{CPU: 16, MemoryMB: 65536, GPUs: []api.GPU{
			{Index: 0, Name: "RTX 3090", VRAMMB: 24576, MemUsedMB: 22000}, // owner's LLM: 2.5 GB left
			{Index: 1, Name: "RTX 4090", VRAMMB: 24576, MemUsedMB: 500},
		}}},
		freeGPU: map[int]bool{0: true, 1: true}, freeCPU: 16, freeMemMB: 65536, shared: map[int]bool{}, sharedPending: map[int]int{},
	}
	small := api.Resources{CPU: 1, MemoryMB: 256, GPUCount: 1, MinVRAMMB: 1000, GPUShared: true}
	// First small shared instance goes to the tightest fit (the 3090's leftover 2.5 GB).
	idx, ok := fits(c, small)
	if !ok || idx[0] != 0 {
		t.Fatalf("first: %v %v", idx, ok)
	}
	c.claimGPUs(idx, small, false)
	// Second one still fits there (2.5 - 1.0 pending = 1.5 GB).
	if idx, _ = fits(c, small); idx[0] != 0 {
		t.Fatalf("second should pack onto the shared 3090, got %v", idx)
	}
	c.claimGPUs(idx, small, false)
	// Third doesn't fit on the 3090 anymore (0.5 GB left) and spills onto the 4090.
	if idx, _ = fits(c, small); idx[0] != 1 {
		t.Fatalf("third should spill to the 4090, got %v", idx)
	}
	c.claimGPUs(idx, small, false)
	// An exclusive request can't take either GPU now.
	if _, ok := fits(c, api.Resources{CPU: 1, MemoryMB: 256, GPUCount: 1}); ok {
		t.Fatal("exclusive request must not land on shared GPUs")
	}
}
