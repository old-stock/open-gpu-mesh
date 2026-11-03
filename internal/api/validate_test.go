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
