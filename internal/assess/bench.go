package assess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// cpuBench measures SHA-256 throughput (MB/s) on one thread, then on all threads. It is a
// rough, comparable score, not a substitute for a real benchmark suite.
func cpuBench(ctx context.Context, d time.Duration, threads int) (single, multi float64) {
	buf := make([]byte, 1<<20)
	_, _ = rand.Read(buf)
	work := func(stop *atomic.Bool, n *atomic.Int64) {
		h := sha256.New()
		for !stop.Load() {
			h.Write(buf)
			n.Add(1)
		}
	}
	measure := func(workers int) float64 {
		var stop atomic.Bool
		var n atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); work(&stop, &n) }()
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
		stop.Store(true)
		wg.Wait()
		return round1(float64(n.Load()) / time.Since(start).Seconds())
	}
	single = measure(1)
	if threads > 1 {
		multi = measure(threads)
	} else {
		multi = single
	}
	return single, multi
}

// diskBench writes size bytes of incompressible data to a temp file in dir, fsyncs, and
// returns MB/s. The file is removed afterwards.
func diskBench(dir string, size int64) (float64, error) {
	f, err := os.CreateTemp(dir, ".ogm-assess-*")
	if err != nil {
		return 0, err
	}
	name := f.Name()
	defer os.Remove(filepath.Clean(name))
	defer f.Close()
	chunk := make([]byte, 4<<20)
	_, _ = rand.Read(chunk)
	start := time.Now()
	for written := int64(0); written < size; written += int64(len(chunk)) {
		if _, err := f.Write(chunk); err != nil {
			return 0, err
		}
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return round1(float64(size) / (1 << 20) / time.Since(start).Seconds()), nil
}
