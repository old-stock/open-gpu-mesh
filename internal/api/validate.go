package api

import (
	"fmt"
	"regexp"
	"strings"
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// ValidName reports whether s is a valid DNS-label style resource name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Normalize fills defaults on a group spec in place.
func (s *ContainerGroupSpec) Normalize() {
	if s.Priority == "" {
		s.Priority = PriorityMedium
	}
	if s.RestartPolicy == "" {
		s.RestartPolicy = "always"
	}
	if s.DisplayName == "" {
		s.DisplayName = s.Name
	}
	r := &s.Container.Resources
	if r.CPU == 0 {
		r.CPU = 1
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = 1024
	}
	if r.ShmMB == 0 {
		r.ShmMB = 64
	}
	if r.StorageGB == 0 {
		r.StorageGB = 10
	}
	if s.Container.ImagePullPolicy == "" {
		// Kubernetes semantics: mutable tags are re-pulled, pinned ones are cached.
		img := s.Container.Image
		if strings.Contains(img, "@sha256:") || (strings.Contains(img[strings.LastIndex(img, "/")+1:], ":") && !strings.HasSuffix(img, ":latest")) {
			s.Container.ImagePullPolicy = "if_not_present"
		} else {
			s.Container.ImagePullPolicy = "always"
		}
	}
	if r.WantsGPU() && r.GPUCount == 0 {
		r.GPUCount = 1
	}
	for i, c := range s.CountryCodes {
		s.CountryCodes[i] = strings.ToLower(c)
	}
	for i, c := range r.GPUClasses {
		r.GPUClasses[i] = Slugify(c)
	}
	if n := s.Networking; n != nil {
		if n.LoadBalancer == "" {
			n.LoadBalancer = LBLeastConn
		}
		if n.ClientRequestTimeoutMS == 0 {
			n.ClientRequestTimeoutMS = 100_000
		}
		if n.ServerResponseTimeoutMS == 0 {
			n.ServerResponseTimeoutMS = 100_000
		}
	}
	if q := s.QueueConnection; q != nil {
		if q.Path == "" {
			q.Path = "/"
		}
		if q.Concurrency == 0 {
			q.Concurrency = 1
		}
	}
	if a := s.HTTPAutoscaler; a != nil {
		if a.TargetConcurrency == 0 {
			a.TargetConcurrency = 10
		}
		if a.StableWindowSeconds == 0 {
			a.StableWindowSeconds = 60
		}
		if a.ScaleDownDelaySeconds == 0 {
			a.ScaleDownDelaySeconds = 300
		}
	}
	if a := s.QueueAutoscaler; a != nil {
		if a.PollingPeriod == 0 {
			a.PollingPeriod = 30
		}
		if a.DesiredQueueLength == 0 {
			a.DesiredQueueLength = 1
		}
		if a.MaxUpscalePerMinute == 0 {
			a.MaxUpscalePerMinute = 10
		}
		if a.MaxDownscalePerMinute == 0 {
			a.MaxDownscalePerMinute = 10
		}
	}
}

// Validate returns field errors, or nil.
func (s *ContainerGroupSpec) Validate() map[string][]string {
	errs := map[string][]string{}
	add := func(k, f string, a ...any) { errs[k] = append(errs[k], fmt.Sprintf(f, a...)) }
	if !ValidName(s.Name) {
		add("name", "must match %s", nameRe)
	}
	if s.Replicas < 0 || s.Replicas > 1000 {
		add("replicas", "must be between 0 and 1000")
	}
	if PriorityRank(s.Priority) == 0 {
		add("priority", "must be one of high, medium, low, batch")
	}
	switch s.RestartPolicy {
	case "always", "on_failure", "never":
	default:
		add("restart_policy", "must be one of always, on_failure, never")
	}
	if s.Container.Image == "" {
		add("container.image", "is required")
	}
	if r := s.Container.Resources; r.GPUShared && (r.MinVRAMMB <= 0 || r.GPUs() != 1) {
		add("container.resources.gpu_shared", "requires min_vram_mb (the VRAM budget) and a single GPU")
	}
	switch s.Container.ImagePullPolicy {
	case "", "always", "if_not_present", "never":
	default:
		add("container.image_pull_policy", "must be one of always, if_not_present, never")
	}
	r := s.Container.Resources
	if r.CPU < 1 || r.CPU > 256 {
		add("container.resources.cpu", "must be between 1 and 256")
	}
	if r.MemoryMB < 128 || r.MemoryMB > 1<<21 {
		add("container.resources.memory_mb", "must be between 128 and 2097152")
	}
	if r.GPUCount < 0 || r.GPUCount > 16 {
		add("container.resources.gpu_count", "must be between 0 and 16")
	}
	if r.StorageGB < 1 || r.StorageGB > 4096 {
		add("container.resources.storage_gb", "must be between 1 and 4096")
	}
	for _, c := range s.CountryCodes {
		if len(c) != 2 {
			add("country_codes", "%q is not an ISO 3166-1 alpha-2 code", c)
		}
	}
	if n := s.Networking; n != nil {
		if n.Port < 1 || n.Port > 65535 {
			add("networking.port", "must be 1-65535")
		}
		if n.LoadBalancer != LBLeastConn && n.LoadBalancer != LBRoundRobin {
			add("networking.load_balancer", "must be round_robin or least_number_of_connections")
		}
		if n.ClientRequestTimeoutMS < 1 || n.ClientRequestTimeoutMS > 3_600_000 {
			add("networking.client_request_timeout_ms", "must be 1-3600000")
		}
		if n.ServerResponseTimeoutMS < 1 || n.ServerResponseTimeoutMS > 3_600_000 {
			add("networking.server_response_timeout_ms", "must be 1-3600000")
		}
	}
	for name, p := range map[string]*Probe{"startup_probe": s.StartupProbe, "readiness_probe": s.ReadinessProbe, "liveness_probe": s.LivenessProbe} {
		if p == nil {
			continue
		}
		n := 0
		if p.HTTP != nil {
			n++
		}
		if p.TCP != nil {
			n++
		}
		if p.Exec != nil {
			n++
		}
		if n != 1 {
			add(name, "exactly one of http, tcp, exec is required")
		}
	}
	if q := s.QueueConnection; q != nil {
		if !ValidName(q.QueueName) {
			add("queue_connection.queue_name", "invalid queue name")
		}
		if q.Port < 1 || q.Port > 65535 {
			add("queue_connection.port", "must be 1-65535")
		}
		if q.Concurrency < 1 || q.Concurrency > 256 {
			add("queue_connection.concurrency", "must be 1-256")
		}
	}
	if a := s.QueueAutoscaler; a != nil {
		if s.QueueConnection == nil {
			add("queue_autoscaler", "requires queue_connection")
		}
		if a.MinReplicas < 0 || a.MaxReplicas < 1 || a.MinReplicas > a.MaxReplicas || a.MaxReplicas > 1000 {
			add("queue_autoscaler", "need 0 <= min_replicas <= max_replicas <= 1000, max_replicas >= 1")
		}
		if a.PollingPeriod < 5 || a.PollingPeriod > 1800 {
			add("queue_autoscaler.polling_period", "must be 5-1800 seconds")
		}
	}
	if a := s.HTTPAutoscaler; a != nil {
		if s.Networking == nil {
			add("http_autoscaler", "requires networking (it scales on gateway traffic)")
		}
		if s.QueueAutoscaler != nil {
			add("http_autoscaler", "can't be combined with queue_autoscaler")
		}
		if a.MinReplicas < 0 || a.MaxReplicas < 1 || a.MinReplicas > a.MaxReplicas || a.MaxReplicas > 1000 {
			add("http_autoscaler", "need 0 <= min_replicas <= max_replicas <= 1000, max_replicas >= 1")
		}
		if a.TargetConcurrency < 1 || a.TargetConcurrency > 10000 {
			add("http_autoscaler.target_concurrency", "must be 1-10000")
		}
		if a.StableWindowSeconds < 6 || a.StableWindowSeconds > 600 {
			add("http_autoscaler.stable_window_seconds", "must be 6-600")
		}
		if a.ScaleDownDelaySeconds < 1 || a.ScaleDownDelaySeconds > 86400 {
			add("http_autoscaler.scale_down_delay_seconds", "must be 1-86400")
		}
	}
	if n := s.Networking; n != nil && (n.MaxConcurrency < 0 || n.MaxConcurrency > 100000) {
		add("networking.max_concurrency", "must be 0-100000")
	}
	if n := s.Networking; n != nil {
		if len(n.CustomDomains) > 20 {
			add("networking.custom_domains", "at most 20 domains")
		}
		seen := map[string]bool{}
		for i, d := range n.CustomDomains {
			d = strings.ToLower(strings.TrimSuffix(d, "."))
			n.CustomDomains[i] = d
			if !ValidHostname(d) {
				add("networking.custom_domains", "%q is not a valid hostname", d)
			}
			if seen[d] {
				add("networking.custom_domains", "%q listed twice", d)
			}
			seen[d] = true
		}
	}
	for _, p := range s.Container.Ports {
		if p < 1 || p > 65535 {
			add("container.ports", "%d is not a valid port", p)
		}
	}
	seenVol := map[string]bool{}
	for i, v := range s.Container.Volumes {
		k := fmt.Sprintf("container.volumes[%d]", i)
		if !ValidName(v.Name) || seenVol[v.Name] {
			add(k+".name", "must be a unique lower-case name (a-z, 0-9, -)")
		}
		seenVol[v.Name] = true
		if !strings.HasPrefix(v.Mount, "/") || v.Mount == "/" {
			add(k+".mount", "must be an absolute path other than /")
		}
	}
	for k, v := range s.Container.Env {
		for _, ref := range ParseRefs(v) {
			switch ref.Kind {
			case "secret":
				if !ValidSecretName(ref.Name) {
					add("container.env."+k, "invalid secret name %q", ref.Name)
				}
			default:
				if !ValidName(ref.Name) || ref.Port < 1 || ref.Port > 65535 {
					add("container.env."+k, "link must look like {{%s:GROUP:PORT}}", ref.Kind)
				}
			}
		}
	}
	for i, sa := range s.ScalingActions {
		if len(strings.Fields(sa.Schedule)) != 5 {
			add(fmt.Sprintf("scaling_actions[%d].schedule", i), "must be a 5-field cron expression")
		}
		if sa.Replicas < 0 || sa.Replicas > 1000 {
			add(fmt.Sprintf("scaling_actions[%d].replicas", i), "must be 0-1000")
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// Slugify turns "NVIDIA GeForce RTX 4090" into "rtx-4090".
func Slugify(name string) string {
	n := strings.ToLower(name)
	for _, p := range []string{"nvidia ", "geforce ", "amd ", "radeon ", "laptop gpu", "tesla "} {
		n = strings.ReplaceAll(n, p, "")
	}
	var b strings.Builder
	dash := false
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

var (
	refRe        = regexp.MustCompile(`\{\{\s*(secret|link|link_host|link_port):([^}\s]+)\s*\}\}`)
	secretNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ValidSecretName reports whether s is a valid secret name (letters, digits, _ . -).
func ValidSecretName(s string) bool { return secretNameRe.MatchString(s) }

// Ref is a {{secret:NAME}} or {{link:GROUP:PORT}} placeholder in an env value.
type Ref struct {
	Raw  string // the full placeholder text
	Kind string // secret | link | link_host | link_port
	Name string // secret name or target group
	Port int    // link target port
}

// ParseRefs finds placeholders in s.
func ParseRefs(s string) []Ref {
	var out []Ref
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		r := Ref{Raw: m[0], Kind: m[1], Name: m[2]}
		if r.Kind != "secret" {
			name, port, _ := strings.Cut(m[2], ":")
			r.Name = name
			fmt.Sscanf(port, "%d", &r.Port)
		}
		out = append(out, r)
	}
	return out
}

var hostnameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// ValidHostname reports whether d is a fully qualified DNS hostname (no wildcard, no port).
func ValidHostname(d string) bool { return len(d) <= 253 && hostnameRe.MatchString(d) }

// DefaultReplicas applies the documented default of 1 replica to a spec whose
// source document had no "replicas" key. Autoscaled groups keep 0: their
// autoscaler decides.
func (s *ContainerGroupSpec) DefaultReplicas(present bool) {
	if !present && s.Replicas == 0 && s.QueueAutoscaler == nil && s.HTTPAutoscaler == nil {
		s.Replicas = 1
	}
}
