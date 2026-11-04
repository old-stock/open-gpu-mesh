// Package demoapp is a tiny inference-server stand-in used by `ogm dev`, tests and docs.
// It listens on $PORT (default 8080) and simulates GPU work.
package demoapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// Run starts the demo server and blocks until SIGTERM.
func Run(args []string) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	startupDelay, _ := time.ParseDuration(os.Getenv("STARTUP_DELAY"))
	started := time.Now()
	var served atomic.Int64
	id := os.Getenv("OGM_INSTANCE_ID")
	gpu := os.Getenv("OGM_GPU_NAME")
	host, _ := os.Hostname()

	info := func() map[string]any {
		return map[string]any{
			"app": "ogm-demo", "instance_id": id, "gpu": gpu, "hostname": host, "group": os.Getenv("OGM_CONTAINER_GROUP"),
			"uptime_s": int(time.Since(started).Seconds()), "requests_served": served.Load(),
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		writeJSON(w, 200, info())
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(started) < startupDelay {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	work := func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		var in struct {
			Prompt  string `json:"prompt"`
			SleepMS int    `json:"sleep_ms"`
			Fail    bool   `json:"fail"`
			Status  int    `json:"status"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
		if in.SleepMS == 0 {
			in.SleepMS = 200
		}
		select {
		case <-time.After(time.Duration(in.SleepMS) * time.Millisecond):
		case <-r.Context().Done():
			log.Printf("job %s cancelled", r.Header.Get("OGM-Job-Id"))
			return
		}
		if in.Fail {
			st := in.Status
			if st == 0 {
				st = 500
			}
			writeJSON(w, st, map[string]any{"error": "simulated failure", "instance_id": id})
			return
		}
		sum := sha256.Sum256([]byte(in.Prompt))
		log.Printf("generated image for %q (job %s attempt %s)", in.Prompt, r.Header.Get("OGM-Job-Id"), r.Header.Get("OGM-Job-Attempt"))
		writeJSON(w, 200, map[string]any{
			"prompt": in.Prompt, "image_sha256": hex.EncodeToString(sum[:]), "instance_id": id, "gpu": gpu,
			"took_ms": in.SleepMS,
		})
	}
	mux.HandleFunc("POST /generate", work)
	mux.HandleFunc("POST /", work)
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 5
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 1; i <= n; i++ {
			fmt.Fprintf(w, "data: token %d from %s\n\n", i, id)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			t, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			_ = c.Write(r.Context(), t, append([]byte("echo: "), b...))
		}
	})
	// Neocloud feature demos. /env only reveals DEMO_* variables (e.g. a secret you set as
	// DEMO_TOKEN={{secret:token}} to check it arrived).
	mux.HandleFunc("GET /env", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if !strings.HasPrefix(name, "DEMO_") {
			http.Error(w, "only DEMO_* variables", http.StatusForbidden)
			return
		}
		writeJSON(w, 200, map[string]string{name: os.Getenv(name)})
	})
	// /fetch?env=DEMO_API&path=/ calls http://$DEMO_API/path: shows {{link:GROUP:PORT}} working.
	mux.HandleFunc("GET /fetch", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("env")
		if !strings.HasPrefix(name, "DEMO_") || os.Getenv(name) == "" {
			http.Error(w, "set ?env=DEMO_* to a variable holding host:port", http.StatusBadRequest)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+os.Getenv(name)+r.URL.Query().Get("path"), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	// /kv/KEY stores small values under $OGM_VOLUME_DATA (process runtime) or /data: shows a
	// persistent volume surviving restarts.
	dataDir := os.Getenv("OGM_VOLUME_DATA")
	if dataDir == "" {
		dataDir = "/data"
	}
	mux.HandleFunc("PUT /kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		k := r.PathValue("key")
		if strings.ContainsAny(k, `/\.`) {
			http.Error(w, "bad key", 400)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err := os.WriteFile(filepath.Join(dataDir, k), b, 0o600); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		k := r.PathValue("key")
		if strings.ContainsAny(k, `/\.`) {
			http.Error(w, "bad key", 400)
			return
		}
		b, err := os.ReadFile(filepath.Join(dataDir, k))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		_, _ = w.Write(b)
	})
	// /hub-incr?key=K increments counter K in the hub KV (table "demo") using the instance's
	// identity token from IMDS: no API key in the container.
	mux.HandleFunc("GET /hub-incr", func(w http.ResponseWriter, r *http.Request) {
		imds, hub := os.Getenv("OGM_IMDS_URL"), os.Getenv("OGM_SERVER_URL")
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, imds+"/v1/token?audience=ogm-hub", nil)
		req.Header.Set("Metadata", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		var tok struct {
			JWT string `json:"jwt"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&tok)
		resp.Body.Close()
		if tok.JWT == "" {
			http.Error(w, "no token from IMDS", 502)
			return
		}
		u := hub + "/v1/projects/" + os.Getenv("OGM_PROJECT") + "/kv/demo/" + r.URL.Query().Get("key")
		req, _ = http.NewRequestWithContext(r.Context(), http.MethodPost, u, nil)
		req.Header.Set("Authorization", "Bearer "+tok.JWT)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	// /gpu lists the GPUs visible to the container. nvidia-smi is injected by the
	// NVIDIA container runtime, but it needs glibc, so in a scratch image fall back
	// to the driver's /proc entries, which the runtime also mounts.
	mux.HandleFunc("GET /gpu", func(w http.ResponseWriter, r *http.Request) {
		out, err := exec.CommandContext(r.Context(), "nvidia-smi", "-L").CombinedOutput()
		if err == nil {
			_, _ = w.Write(out)
			return
		}
		infos, _ := filepath.Glob("/proc/driver/nvidia/gpus/*/information")
		sort.Strings(infos)
		if len(infos) == 0 {
			http.Error(w, "no GPU visible (nvidia-smi: "+err.Error()+")", http.StatusNotFound)
			return
		}
		for i, f := range infos {
			b, _ := os.ReadFile(f)
			model, uuid := "", ""
			for _, ln := range strings.Split(string(b), "\n") {
				k, v, ok := strings.Cut(ln, ":")
				if !ok {
					continue
				}
				switch strings.TrimSpace(k) {
				case "Model":
					model = strings.TrimSpace(v)
				case "GPU UUID":
					uuid = strings.TrimSpace(v)
				}
			}
			fmt.Fprintf(w, "GPU %d: %s (UUID: %s)\n", i, model, uuid)
		}
	})
	mux.HandleFunc("GET /imds", func(w http.ResponseWriter, r *http.Request) {
		u := os.Getenv("OGM_IMDS_URL")
		if u == "" {
			http.Error(w, "OGM_IMDS_URL not set", 500)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u+"/v1/metadata", nil)
		req.Header.Set("Metadata", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, resp.Body)
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("SIGTERM received, draining")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("ogm demo app listening on :%s (instance %s, gpu %q)", port, id, gpu)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
