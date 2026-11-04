package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/old-stock/open-gpu-mesh/internal/agent"
	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/assess"
)

// TestAssess runs the onboarding audit against a real hub: latency + parallel speed test
// with a node token, then stores the result and reads it back through the API.
func TestAssess(t *testing.T) {
	cl := newCluster(t)
	tok, err := cl.c.CreateNode(cl.ctx, "audited")
	if err != nil {
		t.Fatal(err)
	}

	// Unauthenticated speed tests are refused.
	resp, err := http.Get(cl.url + "/v1/speedtest/down?bytes=10")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous speed test: got %d", resp.StatusCode)
	}

	a := assess.Run(cl.ctx, assess.Options{Server: cl.url, Token: tok.Token, Profiles: []string{"gaming"}, Quick: true,
		GPUs: agent.ParseFakeGPUs("RTX 4090:24564")})
	if a.Network.HubRTTms <= 0 || a.Network.HubDownMbps <= 0 || a.Network.HubUpMbps <= 0 {
		t.Fatalf("hub network not measured: %+v errors=%v", a.Network, a.Errors)
	}
	if a.CPU.SingleMBps <= 0 || a.Suggested.AgentFlags == "" || a.Tier == "" {
		t.Fatalf("incomplete assessment: %+v", a)
	}

	body, _ := json.Marshal(a)
	req, _ := http.NewRequest("POST", cl.url+"/v1/agent/assessment", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("submit: %d", resp.StatusCode)
	}
	var got api.Assessment
	if err := cl.c.Do(cl.ctx, "GET", "/v1/nodes/"+tok.NodeID+"/assessment", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Tier != a.Tier || got.Network.HubDownMbps != a.Network.HubDownMbps || len(got.GPUs) != 1 {
		t.Fatalf("stored assessment differs: %+v", got)
	}
	var n api.Node
	if err := cl.c.Do(cl.ctx, "GET", "/v1/nodes/"+tok.NodeID, nil, &n); err != nil {
		t.Fatal(err)
	}
	if n.Assessment == nil || n.Assessment.Tier != a.Tier {
		t.Fatalf("node API missing assessment: %+v", n.Assessment)
	}
	t.Logf("hub: %.1f ms, %.0f/%.0f Mbps; tier %s; flags %s", a.Network.HubRTTms, a.Network.HubDownMbps, a.Network.HubUpMbps, a.Tier, a.Suggested.AgentFlags)
}
