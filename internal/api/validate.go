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
