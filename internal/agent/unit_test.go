package agent

import (
	"testing"
	"time"
)

func TestInWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	cases := []struct {
		spec string
		t    time.Time
		want bool
	}{
		{"", at(3, 0), true},
		{"09:00-17:00", at(12, 0), true},
		{"09:00-17:00", at(17, 0), false},
		{"22:00-07:00", at(23, 30), true},
		{"22:00-07:00", at(6, 59), true},
		{"22:00-07:00", at(12, 0), false},
		{"01:00-02:00, 22:00-23:00", at(22, 15), true},
		{"garbage", at(12, 0), false},
	}
	for _, c := range cases {
		if got := inWindow(c.spec, c.t); got != c.want {
			t.Errorf("inWindow(%q, %v) = %v", c.spec, c.t.Format("15:04"), got)
		}
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	out := []byte("0, GPU-aaa, NVIDIA GeForce RTX 4090, 24564, 37, 1200, 55\n1, GPU-bbb, NVIDIA GeForce RTX 3090, 24576, 0, 3, 41\nbad line\n")
	g := parseNvidiaSMI(out)
	if len(g) != 2 || g[0].Name != "NVIDIA GeForce RTX 4090" || g[0].VRAMMB != 24564 || g[0].UtilPct != 37 || g[1].UUID != "GPU-bbb" || g[1].TempC != 41 {
		t.Fatalf("%+v", g)
	}
}

func TestParseFakeGPUs(t *testing.T) {
	f := ParseFakeGPUs("RTX 4090:24564, RTX 3060")
	if len(f.List) != 2 || f.List[0].VRAMMB != 24564 || f.List[1].Name != "RTX 3060" || f.List[1].VRAMMB != 24576 {
		t.Fatalf("%+v", f.List)
	}
}

func TestParseNvidiaSMIUnified(t *testing.T) {
	g := parseNvidiaSMI([]byte("0, GPU-ccc, NVIDIA GB10, [N/A], 0, [N/A], 45\n"))
	if len(g) != 1 || !g[0].Unified || g[0].Name != "NVIDIA GB10" {
		t.Fatalf("%+v", g)
	}
}
