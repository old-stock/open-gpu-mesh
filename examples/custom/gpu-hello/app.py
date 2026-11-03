"""gpu-hello: a tiny stdlib-only app for testing ogm on real nodes.

GET  /          instance + node + GPU info (nvidia-smi is injected by the NVIDIA toolkit)
GET  /health    readiness
GET  /stream    SSE: 5 ticks
POST /work      queue job: {"n": rounds} -> sha256 chain + where it ran
GET  /imds      proxies $OGM_IMDS_URL/v1/metadata (proves the metadata service works)
"""
import hashlib, json, os, platform, socket, subprocess, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT", "8080"))
START = time.time()


def gpus():
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=name,memory.total,memory.used,utilization.gpu,temperature.gpu",
             "--format=csv,noheader"], capture_output=True, text=True, timeout=10)
        return [l.strip() for l in out.stdout.splitlines() if l.strip()] or [out.stderr.strip()]
    except Exception as e:  # no GPU passed through
        return [f"nvidia-smi unavailable: {e}"]


def info():
    return {
        "app": "gpu-hello",
        "hostname": socket.gethostname(),
        "arch": platform.machine(),
        "instance": os.environ.get("OGM_INSTANCE_ID"),
        "replica_index": os.environ.get("OGM_REPLICA_INDEX"),
        "node": os.environ.get("OGM_MACHINE_ID"),
        "gpu_name": os.environ.get("OGM_GPU_NAME"),
        "gpus": gpus(),
        "uptime_s": round(time.time() - START, 1),
    }


class H(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/health":
            return self._json(200, {"ok": True})
        if self.path == "/stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for i in range(5):
                self.wfile.write(f"data: tick {i} from {os.environ.get('OGM_INSTANCE_ID')}\n\n".encode())
                self.wfile.flush()
                time.sleep(0.3)
            return
        if self.path == "/imds":
            url = os.environ.get("OGM_IMDS_URL", "") + "/v1/metadata"
            try:
                req = urllib.request.Request(url, headers={"Metadata": "true"})
                return self._json(200, json.load(urllib.request.urlopen(req, timeout=5)))
            except Exception as e:
                return self._json(502, {"error": str(e), "url": url})
        return self._json(200, info())

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        rounds = min(int(body.get("n", 100000)), 5_000_000)
        t = time.time()
        h = b"ogm"
        for _ in range(rounds):
            h = hashlib.sha256(h).digest()
        self._json(200, {"digest": h.hex(), "rounds": rounds, "took_ms": round((time.time() - t) * 1000),
                         "job": self.headers.get("OGM-Job-Id"), "attempt": self.headers.get("OGM-Job-Attempt"),
                         "ran_on": {k: info()[k] for k in ("hostname", "arch", "instance", "gpu_name")}})

    def log_message(self, fmt, *args):
        print(f"{self.address_string()} {fmt % args}", flush=True)


if __name__ == "__main__":
    print(f"gpu-hello listening on :{PORT} arch={platform.machine()} gpus={gpus()}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
