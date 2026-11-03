# Quickstart

## 0. Local sandbox

```sh
make build && ./bin/ogm dev --nodes 4
```

This runs the server and four simulated GPU nodes (RTX 4090, 3090, 3060, 4080) in one process. Containers run as local processes, so you need neither Docker nor a GPU. It also saves CLI credentials and deploys an example app with a queue. Open the dashboard URL it prints.

## 1. Server

Any Linux VPS with 1 vCPU and 1 GB RAM is enough. The server only routes traffic and keeps state in SQLite.

```sh
make dist && sudo install dist/ogm-linux-amd64 /usr/local/bin/ogm && sudo mkdir -p /opt/ogm && sudo cp -r dist /opt/ogm/
ogm server --public-url https://gpu.example.com --data /var/lib/ogm --dist-dir /opt/ogm/dist
```

- **TLS:** put a reverse proxy in front. With Caddy: `gpu.example.com { reverse_proxy 127.0.0.1:7777 }`. Websockets and SSE pass through without extra config.
- **Host-based gateway URLs** (optional): point the wildcard DNS `*.gw.example.com` at the server and pass `--gateway-domain gw.example.com`. Groups then get `https://{group}-{project}.gw.example.com`. Without this, groups use the path form `https://gpu.example.com/gw/{project}/{group}/`.
- **Agent binaries:** `--dist-dir` should contain the output of `make dist`. The install script downloads the agent from `/dl/{os}-{arch}`.
- **First start** prints the admin API key once. Save it.

## 2. Members

```sh
ogm login https://gpu.example.com --key ogm_...   # admin
ogm invite --uses 50                               # share the printed command
# friend:
ogm join https://gpu.example.com inv_... --name alice
```

Each member gets a personal project named after them and 10 starter credits.

## 3. GPU nodes

On each contributor machine:

- **Linux:** install Docker and the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/). Check it with `docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi`.
- **Windows:** install WSL2 with Ubuntu, then Docker Engine inside WSL plus the NVIDIA Container Toolkit. The Windows NVIDIA driver already exposes the GPU to WSL. Run the agent inside WSL. `--pause-on-process` can still see Windows games through `tasklist.exe`.

```sh
ogm node add alice-4090            # run by the member; prints the install line below
curl -fsSL https://gpu.example.com/install.sh | sh -s -- --token ogmn_...
```

The installer sets up a systemd service. To run the agent by hand:

```sh
ogm agent --server https://gpu.example.com --token ogmn_... \
  --country us --max-gpus 1 --window "00:00-09:00,22:00-24:00" \
  --pause-on-process steam,valorant.exe --yield-gpu-util 30 --label tier=fast
```

| Flag | Effect |
|---|---|
| `--max-gpus/--max-cpu/--max-memory-mb/--max-disk-gb` | share only part of the machine |
| `--window` | only accept work during these local-time windows |
| `--pause-on-process` | pause while any listed process runs |
| `--yield-gpu-util N` | pause when a GPU not assigned to ogm is above N% (you started gaming) |
| `--allow-priorities` | e.g. `high,medium` to refuse batch work |
| `--label k=v` | targeted by `node_selector` in specs |
| `--gvisor` | run containers under runsc for extra isolation |

`ogm agent pause|resume|status` controls a running agent. On pause, instances get SIGTERM, and `$OGM_IMDS_URL/v1/termination-notice` reports the deadline. The scheduler then moves the work to another machine.

## 4. Deploy

```sh
ogm deploy -f examples/llm-ollama.yaml
ogm get ollama                     # instances, GPUs, states
ogm logs ollama -f
ogm url ollama                     # URL + a shareable endpoint-only token
ogm curl ollama api/tags
```

Deploying the same file again updates the group in place. Changes to the image, environment, command, resources, probes, or ports roll out gradually, at most 25% of instances at a time. Changes to replicas, priority, gateway settings, autoscaler, or queue connection apply immediately without restarting anything.

## 5. Custom images

```sh
docker build -t my-model:v1 .
ogm push my-model:v1                     # stored in your cluster; use image: registry.ogm/<project>/my-model:v1
ogm push --multiarch my-model:v1         # run on an x86 box and an ARM box to make one multi-arch tag
```

## 6. Upgrading agents without downtime

Replace the binary, then send `SIGHUP` (`systemctl reload ogm-agent`). The agent re-executes itself and re-attaches to its running containers, so workloads keep serving. If the agent crashes, a restart re-attaches the same way. Containers the server no longer wants are removed a minute after reconnecting. `SIGTERM` (`systemctl stop`) still drains the node.

## 7. Queues

```sh
ogm deploy -f examples/sdxl-queue.yaml
ogm job submit sdxl --input '{"prompt":"a lighthouse"}' --wait
ogm job submit sdxl --batch prompts.jsonl          # one JSON input per line
ogm job ls sdxl --status failed
```

Your container only needs an HTTP endpoint. The agent POSTs each job's input as the request body and stores your JSON response as the output.
- **Retryable:** non-2xx responses, timeouts, and crashes are retried with exponential backoff, on a different machine where possible.
- **Not retryable:** 4xx responses (except 408 and 429), because the input itself is bad.

## Troubleshooting

| Symptom | Fix |
|---|---|
| Instance stuck in `allocating` | No node satisfies the constraints. `ogm get GROUP` shows how many nodes were rejected for each reason (for example `waiting for capacity: 1 country, 3 no matching gpu`). Check `ogm gpus`. |
| `downloading` takes forever | The image is large and is pulled on each node the first time. Later starts hit the cache, and the scheduler prefers nodes that already have the image. |
| Gateway 503 `no ready instances` | Instances exist but aren't ready yet. Check readiness/startup probes and `ogm logs`. |
| Gateway 524 | The app took longer than `server_response_timeout_ms` to send response headers. Raise it (up to 1 h). |
| Node shows `paused` | The owner's window, process watch, or GPU yield kicked in. See `pause_reason` in `ogm nodes`. |
| `docker: could not select device driver "nvidia"` on a node | Install the NVIDIA Container Toolkit and restart Docker. |
