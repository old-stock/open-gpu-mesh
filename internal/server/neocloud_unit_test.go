package server

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

func TestHTTPAutoscaleTarget(t *testing.T) {
	a := api.HTTPAutoscaler{MinReplicas: 0, MaxReplicas: 5, TargetConcurrency: 10, StableWindowSeconds: 60}
	flat := func(v float64, n int) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	cases := []struct {
		name    string
		a       api.HTTPAutoscaler
		samples []float64
		want    int
	}{
		{"idle scales to zero", a, flat(0, 60), 0},
		{"no samples", a, nil, 0},
		{"steady 25 -> 3", a, flat(25, 60), 3},
		{"exactly target", a, flat(10, 60), 1},
		{"capped at max", a, flat(500, 60), 5},
		{"min floor", api.HTTPAutoscaler{MinReplicas: 2, MaxReplicas: 5, TargetConcurrency: 10, StableWindowSeconds: 60}, flat(0, 60), 2},
		{"burst uses panic window", a, append(flat(0, 54), flat(40, 6)...), 4},
		{"tail of burst keeps stable avg", a, append(flat(30, 54), flat(0, 6)...), 3},
	}
	for _, c := range cases {
		if got := HTTPAutoscaleTarget(c.a, c.samples); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestSecretSealRoundTrip(t *testing.T) {
	key := func() []byte { k := make([]byte, 32); _, _ = rand.Read(k); return k }
	s := &Server{secretKey: key()}
	sealed, err := s.sealSecret([]byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(sealed), []byte("hunter2")) {
		t.Fatal("plaintext in sealed value")
	}
	again, _ := s.sealSecret([]byte("hunter2"))
	if again == sealed {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
	plain, err := s.openSecret(sealed)
	if err != nil || string(plain) != "hunter2" {
		t.Fatalf("open: %q %v", plain, err)
	}
	other := &Server{secretKey: key()}
	if _, err := other.openSecret(sealed); err == nil {
		t.Fatal("opened with the wrong key")
	}
}

func TestVolumeName(t *testing.T) {
	if got := volumeName("default", "db", "data", 2); got != "ogm-vol-default_db_data_2" {
		t.Fatal(got)
	}
}

func TestVolumeNameNoCollision(t *testing.T) {
	if volumeName("a-b", "c", "data", 0) == volumeName("a", "b-c", "data", 0) {
		t.Fatal("volume names collide across projects")
	}
}

func TestGatewayBase(t *testing.T) {
	c := Config{PublicURL: "https://gpu.example.com", GatewayListen: ":7778"}
	if got := c.gatewayBase(); got != "http://gpu.example.com:7778" {
		t.Fatal(got)
	}
	c.GatewayURL = "https://apps.example.com/"
	if got := c.gatewayBase(); got != "https://apps.example.com" {
		t.Fatal(got)
	}
	if (&Config{PublicURL: "https://gpu.example.com"}).gatewayBase() != "" {
		t.Fatal("gateway base without a gateway listener")
	}
}
