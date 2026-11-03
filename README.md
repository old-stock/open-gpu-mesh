# open-gpu-mesh (`ogm`)

**Pool your friends' GPUs, wherever they are, and run containers on them.** One binary, one small server, no cloud bill.

Get a hundred friends to run an agent on their gaming PCs. Then anyone in the group can deploy a container image and get a load-balanced HTTPS endpoint, a job queue that autoscales, logs, and metrics. Workloads use idle GPUs across the group and move off a machine when its owner wants it back. Credits keep it fair: lending GPU time earns credits and using it spends them.

```
ogm deploy -f examples/llm-ollama.yaml
curl -H "OGM-Api-Key: $KEY" https://gpu.example.com/gw/me/ollama/api/generate -d '{"model":"llama3.1:8b","prompt":"hi"}'
```

It does what a managed distributed-GPU cloud such as SaladCloud does, but self-hosted, and it fixes the rough edges most people hit with those platforms (see [docs/comparison.md](docs/comparison.md)).

## Try it in 30 seconds (no GPU, no Docker)

```sh
make build
./bin/ogm dev          # server + 3 simulated GPU nodes + an example app, all in one process
```

Open http://127.0.0.1:7777 and paste the API key it prints. Then:

```sh
./bin/ogm ls
curl http://127.0.0.1:7777/gw/default/hello/
./bin/ogm job submit hello-jobs --input '{"prompt":"a cat in space"}' --wait
./bin/ogm logs hello -f
```

## Run a real mesh

**1. Server** (any small VPS, behind a TLS proxy such as Caddy):

```sh
ogm server --public-url https://gpu.example.com --data /var/lib/ogm
# or: docker run -p 7777:7777 -v ogm:/data -e OGM_PUBLIC_URL=https://gpu.example.com ghcr.io/old-stock/open-gpu-mesh
```

On first start it prints the admin API key.

**2. Invite friends:**

```sh
ogm login https://gpu.example.com --key ogm_...
ogm invite --uses 100            # prints: ogm join https://gpu.example.com inv_... --name <you>
```

**3. Friends contribute GPUs.** Requirements: Linux or Windows with WSL2, Docker, and the NVIDIA Container Toolkit.

```sh
ogm node add alice-4090          # prints a one-line installer
curl -fsSL https://gpu.example.com/install.sh | sh -s -- --token ogmn_...
```

The agent dials out over WebSocket, so the node needs no port-forwarding, public IP, or IPv6. Owners stay in control:

```sh
ogm agent ... --max-gpus 1 --window "23:00-08:00" --pause-on-process steam,valorant --yield-gpu-util 30
ogm agent pause          # gaming now; running work is moved elsewhere with a SIGTERM + termination notice
ogm agent resume
```

**4. Deploy.** See [examples/](examples) and the [spec reference](docs/spec-reference.md).

## What you get

| | |
|---|---|
| **Container groups** | Any OCI image, GPU class/VRAM/count requirements, country and label constraints, 4 priority tiers with preemption, startup/readiness/liveness probes (HTTP, TCP, exec), restart policies, rolling updates, run-to-completion batches with stable `OGM_REPLICA_INDEX` |
| **Gateway** | Per-group URL (`/gw/{project}/{group}/` or `{group}-{project}.your-domain`), least-connections or round-robin, single-connection mode with queueing, waits through cold starts, retries a different instance on connect failure, websockets and SSE, configurable timeouts with no 100 s cap, API-key / bearer / token auth |
| **Job queues** | No worker binary in your image: the agent POSTs each job to your HTTP endpoint. Configurable attempts with exponential backoff, retries go to a different machine, jobs requeue instantly when a node drops, cancel aborts the in-flight request, signed webhooks, long-poll `?wait=` |
| **Autoscaling** | Queue-length autoscaler (scale to zero) plus cron-scheduled scaling |
| **Instance metadata** | `OGM_IMDS_URL`: readiness status, signed identity JWT (JWKS published), deletion cost, reallocate/restart/recreate, termination notice |
| **Fair share** | Zero-sum credits ledger. Using GPU time pays the GPU's owner; balance breaks ties in scheduling; leaderboard |
| **Built-in registry** | `ogm push my-app:dev` and deploy `registry.ogm/...`, with no registry account. Pushes from x86 and ARM merge into one multi-arch tag, and the scheduler only places images on architectures they support |
| **Real home hardware** | Placement respects VRAM the owner's own models are using; GPUs can be shared (`gpu_shared` with a VRAM budget, billed per slice); unified-memory GPUs (DGX Spark/GB10) are supported; agents upgrade in place (`systemctl reload`) and survive crashes without restarting workloads |
| **Files** | Small built-in object store with presigned URLs for model weights and outputs |
| **Ops** | Web dashboard, CLI, Go SDK, REST API, live logs, events, per-instance GPU metrics |

## Docs

- [docs/quickstart.md](docs/quickstart.md): from zero to a GPU endpoint, with troubleshooting
- [docs/spec-reference.md](docs/spec-reference.md): every field of a container group and queue
- [docs/api.md](docs/api.md): REST API, gateway, IMDS, webhooks
- [docs/architecture.md](docs/architecture.md): how it works internally
- [docs/comparison.md](docs/comparison.md): feature-by-feature comparison with SaladCloud
- [SPEC.md](SPEC.md): the full design specification and roadmap

## Trust model

ogm is meant for **groups who trust each other**. Containers are hardened: no privileges, dropped capabilities, a PID limit, no host mounts, and optional gVisor. Even so, whoever owns a machine can inspect what runs on it. Don't send secrets you wouldn't share with the group.

## Development

```sh
make test        # unit + end-to-end tests (spins up a real server and agents in-process)
make dist        # cross-compile linux/darwin/windows × amd64/arm64 into dist/
make docker
```

Layout: `cmd/ogm` (entrypoint), `internal/server` (API, scheduler, gateway, queues), `internal/agent` (node agent, Docker runtime), `internal/tunnel` (WebSocket + yamux), `internal/store` (SQLite), `internal/cli`, `internal/client` (Go SDK), `internal/web` (dashboard).

## License

MIT, see [LICENSE](LICENSE).
