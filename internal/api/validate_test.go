package api

import "testing"

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"NVIDIA GeForce RTX 4090":            "rtx-4090",
		"NVIDIA GeForce RTX 3060 Laptop GPU": "rtx-3060",
		"NVIDIA A100-SXM4-80GB":              "a100-sxm4-80gb",
		"Tesla T4":                           "t4",
		"  weird // name ":                   "weird-name",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	for s, ok := range map[string]bool{"hello": true, "my-app-2": true, "": false, "-bad": false, "Bad": false, "a_b": false} {
		if ValidName(s) != ok {
			t.Errorf("ValidName(%q) != %v", s, ok)
		}
	}
}

func TestNormalizeValidate(t *testing.T) {
	s := ContainerGroupSpec{Name: "app", Container: ContainerSpec{Image: "ogm/demo"}}
	s.Normalize()
	if errs := s.Validate(); errs != nil {
		t.Fatalf("valid spec rejected: %v", errs)
	}
	if s.Priority == "" || s.Container.Resources.CPU == 0 || s.Container.Resources.MemoryMB == 0 {
		t.Fatalf("defaults not filled: %+v", s)
	}
	bad := ContainerGroupSpec{Name: "Bad Name", Priority: "urgent"}
	bad.Normalize()
	errs := bad.Validate()
	for _, k := range []string{"name", "container.image", "priority"} {
		if len(errs[k]) == 0 {
			t.Errorf("expected error for %s, got %v", k, errs)
		}
	}
}

func TestParseRefs(t *testing.T) {
	refs := ParseRefs("postgres://app:{{secret:db.pass}}@{{ link:db:5432 }}/app?h={{link_host:cache:6379}}&x={{nope:1}}")
	if len(refs) != 3 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Kind != "secret" || refs[0].Name != "db.pass" {
		t.Errorf("secret ref = %+v", refs[0])
	}
	if refs[1].Kind != "link" || refs[1].Name != "db" || refs[1].Port != 5432 || refs[1].Raw != "{{ link:db:5432 }}" {
		t.Errorf("link ref = %+v", refs[1])
	}
	if refs[2].Kind != "link_host" || refs[2].Name != "cache" || refs[2].Port != 6379 {
		t.Errorf("link_host ref = %+v", refs[2])
	}
}

func TestValidateNeocloudFields(t *testing.T) {
	base := func() ContainerGroupSpec {
		return ContainerGroupSpec{Name: "web", Replicas: 1, Container: ContainerSpec{Image: "nginx", Resources: Resources{CPU: 1, MemoryMB: 256}},
			Networking: &Networking{Port: 80}}
	}
	bad := map[string]func(*ContainerGroupSpec){
		"volume mount /":  func(s *ContainerGroupSpec) { s.Container.Volumes = []Volume{{Name: "data", Mount: "/"}} },
		"volume relative": func(s *ContainerGroupSpec) { s.Container.Volumes = []Volume{{Name: "data", Mount: "data"}} },
		"duplicate volume": func(s *ContainerGroupSpec) {
			s.Container.Volumes = []Volume{{Name: "d", Mount: "/a"}, {Name: "d", Mount: "/b"}}
		},
		"bad link port":      func(s *ContainerGroupSpec) { s.Container.Env = map[string]string{"X": "{{link:db:99999}}"} },
		"bad secret name":    func(s *ContainerGroupSpec) { s.Container.Env = map[string]string{"X": "{{secret:-bad/}}"} },
		"autoscaler no net":  func(s *ContainerGroupSpec) { s.Networking = nil; s.HTTPAutoscaler = &HTTPAutoscaler{MaxReplicas: 2} },
		"autoscaler min>max": func(s *ContainerGroupSpec) { s.HTTPAutoscaler = &HTTPAutoscaler{MinReplicas: 3, MaxReplicas: 2} },
		"negative max conc":  func(s *ContainerGroupSpec) { s.Networking.MaxConcurrency = -1 },
		"bad container port": func(s *ContainerGroupSpec) { s.Container.Ports = []int{0} },
	}
	for name, mut := range bad {
		s := base()
		mut(&s)
		s.Normalize()
		if errs := s.Validate(); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	s := base()
	s.Container.Volumes = []Volume{{Name: "data", Mount: "/var/lib/data"}}
	s.Container.Env = map[string]string{"DB": "{{link:db:5432}}", "PW": "{{secret:pw}}"}
	s.HTTPAutoscaler = &HTTPAutoscaler{MaxReplicas: 3}
	s.Normalize()
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("valid spec rejected: %v", errs)
	}
	if s.HTTPAutoscaler.TargetConcurrency != 10 || s.HTTPAutoscaler.ScaleDownDelaySeconds != 300 {
		t.Errorf("defaults not applied: %+v", s.HTTPAutoscaler)
	}
}
