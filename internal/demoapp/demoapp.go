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
	"os/signal"
	"strconv"
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
