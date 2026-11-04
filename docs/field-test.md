# Field test: two homelab GPU nodes (2026-11-03)

First run of ogm on real GPUs that were already busy with their owners' own work. The point was to prove ogm can share machines politely rather than needing dedicated ones.

## Setup

| Node | Hardware | State at test time | Agent limits |
|---|---|---|---|
| node A | x86_64, RTX 3090 24 GB, 64 GB RAM | ~22 GB VRAM used by the owner's own LLM services; also ran the ogm server | 8 vCPU, 16 GB |
| node B | aarch64, NVIDIA GB10 (DGX Spark, unified 120 GB) | ~114 GB memory used by the owner's own model | 4 vCPU, 2 GB |

The server listened on node A's LAN address. Agents used the Docker runtime with the NVIDIA Container Toolkit. All ogm state lived in a temp directory, and nothing outside ogm-labelled containers was touched. Afterwards both machines were torn down to their original state.

## Custom containers

| Image | How it was built | Spec |
|---|---|---|
| `registry.ogm/default/gpu-hello:v1` | [examples/custom/gpu-hello](../examples/custom/gpu-hello), a stdlib Python app. Built natively on each node and pushed with `ogm push --multiarch` from both | 2 replicas, gateway + auth, queue `hello-work`, `gpu_shared` with 256 MB |
| `registry.ogm/default/tiny-llm:v1` | [examples/custom/tiny-llm](../examples/custom/tiny-llm): llama.cpp CUDA with Qwen2.5-0.5B Q4 baked in (amd64 only) | high priority, `gpu_shared` with 1.2 GB, OpenAI-compatible API |

## Results

| Check | Result |
|---|---|
| Real `docker push` into the built-in registry (containerd image store on node A, classic store on node B) | ✅ |
| `--multiarch` merge into one OCI index (amd64 + arm64, attestation entry dropped) | ✅ |
| Pull through the agent's localhost proxy with the node token | ✅ |
| Scheduling across architectures; arch-aware placement keeps the amd64-only image off the ARM node (`1 image arch`) | ✅ |
| GB10 unified memory detected (was 0 VRAM before the fix) | ✅ |
| Gateway: 401 without a key, alternating x86/ARM backends, SSE, share token works on the gateway and is refused by the API | ✅ |
| IMDS from inside containers (`host.docker.internal`) | ✅ |
| Queue: 20 jobs, 20 succeeded, split 14 (GB10, 944 ms avg) / 6 (x86 host, 2505 ms avg) | ✅ |
| Priority preemption: `tiny-llm` (high) interrupted `gpu-hello` on node A and the stop was graceful | ✅ |
| LLM through the gateway: 446–449 tok/s, 122 ms round trip, streaming OK, about 0.7 GB VRAM | ✅ |
| Node offline: instance interrupted after the 20 s grace period; a job submitted during the outage ran when the node returned | ✅ |
| Agent `kill -9`: container kept running and was re-attached by the new agent; gateway back within about 2 s | ✅ (fixed during the test) |
| Agent `SIGHUP` in-place upgrade: same PID, workloads untouched | ✅ (added during the test) |
| Unclaimed leftover container reaped 60 s after reconnect | ✅ |
| Server restart: agents reconnect, workloads unaffected | ✅ |
| GPU sharing: `tiny-llm` and a `gpu-hello` replica share the 3090 next to the owner's ~22 GB of models | ✅ (added during the test) |

## What the test changed in ogm

1. **Unified-memory GPUs.** `nvidia-smi` reports `[N/A]` for memory on the GB10. VRAM now comes from `/proc/meminfo`.
2. **Owner VRAM.** `min_vram_mb` is checked against free VRAM, and the scheduler explains when it can't place (`vram in use by owner`).
3. **Agent restarts killed workloads.** Containers are now adopted after a crash, SIGHUP re-execs in place, and leftovers are reaped.
4. **Exclusive GPUs wasted capacity.** Added `gpu_shared` with VRAM budgets and per-slice billing.
5. **No registry for custom images.** Added the built-in OCI registry, `ogm push [--multiarch]`, `ogm images`, `image_pull_policy` with local fallback, and arch-aware placement.

## Onboarding audit (`ogm assess`)

A second run audited three machines against a temporary hub on node A: node A, node B, and node C (a second GB10 whose Docker has no NVIDIA runtime).

| | node A (RTX 3090) | node B (GB10) | node C (GB10, no NVIDIA runtime) |
|---|---|---|---|
| CPU | 16 cores / 32 threads | 10 cores / 20 threads | 10 cores / 20 threads |
| Memory in use by owner | 9 of 62 GB | 113 of 119 GB (unified) | 114 of 119 GB (unified) |
| Hub (LAN) | 0 ms | 941 / 941 Mbps | 941 / 944 Mbps |
| Internet | 860 down / 40 up Mbps | none (no internet) | none (no internet) |
| Suggested memory to share | 53 GB | 4.8 GB | 3.8 GB |
| Tier | compute (upload < 50 Mbps) | compute (no internet) | compute (no internet) |
| Flagged | 21 of 24 GB VRAM in use by owner | no internet access | missing NVIDIA Container Toolkit |

Quick mode took about 25–35 seconds per machine. The first pass found three recommender bugs, fixed before the run above:
- server/ML machines were offered memory their own models were using;
- a machine with no internet was reported as "UDP blocked" and downgraded to burst;
- LAN speed to the hub counted towards the anchor tier.

## Neocloud services on a five-node fleet (2026-11-04)

A third run tested every cluster service end to end on five Linux machines, using the automated live suite in [internal/e2e/live_test.go](../internal/e2e/live_test.go): `go test ./internal/e2e -run TestLive`, configured with `OGM_LIVE_*` variables. The owner's 3090 stayed reserved for other work, so its agent ran with `--no-gpu` and took only CPU, memory and storage jobs.

| Node | Hardware | Runtime | Agent limits |
|---|---|---|---|
| node A | x86_64 storage server, 12 threads, 62 GB | Docker | 8 vCPU, 16 GB; also ran the hub |
| node B | x86_64, 32 threads, 62 GB (RTX 3090 not offered) | Docker, `--no-gpu` | 8 vCPU, 8 GB |
| node C | aarch64 GB10 (no GPU offered) | Docker, `--no-gpu` | 2 vCPU, 1.5 GB |
| node D | aarch64 GB10 | Docker, 1 GPU | 2 vCPU, 1.5 GB |
| node E | x86_64 laptop | process runtime (no Docker) | 4 vCPU, 4 GB |

All 21 checks pass, and a second run straight afterwards, on the leftover state, passes too:

| Check | Result |
|---|---|
| Fleet: 5 nodes, 3 amd64 + 2 arm64; audit tiers (untested internet → compute, laptop → burst) | ✅ |
| Registry: `demo` and `postgres` tags hold amd64 + arm64 (arm64 added from an x86 host with `ogm push --platform`) | ✅ |
| A container on every host, both runtimes: ready in ≤ 1 s, gateway round trip 5–12 ms | ✅ |
| Lifecycle: logs, events, rolling update, restart, recreate, reallocate | ✅ |
| Links: laptop (amd64, process) → hub → GB10 (arm64, Docker), about 10 ms per request | ✅ |
| Postgres with a secret password and a volume; psql client on an arm64 node over a link; snapshot (6.7 MB in 2 s); migration to another node by restoring the snapshot, with data intact | ✅ |
| HTTP autoscaler: cold start about 200 ms, 0 → 3 → 0 replicas across three nodes, 100/100 requests OK | ✅ |
| Queue autoscaler: 24 jobs in 18 s, spread across all four Docker nodes on both architectures | ✅ |
| `max_concurrency: 1` serializes requests | ✅ |
| KV and instance identity: containers on two nodes incremented a shared counter with IMDS tokens and no API keys | ✅ |
| Schedules: a cron scale-up fired on its own, and an HTTP action returned 200 | ✅ |
| Files: 3 MB through a presigned URL in about 100 ms | ✅ |
| S3 API: aws-sdk-go-v2 put/get of 8 MB, bucket create/delete | ✅ |
| Scoped keys: a viewer key reads and cannot write | ✅ |
| Metrics: per-minute p50/p95/p99 and Prometheus `/metrics` | ✅ |
| Custom domain routed by the `Host` header | ✅ |
| Quotas: a third replica over `cpu: 2` rejected; an upload over `storage_mb` rejected | ✅ |
| MCP over HTTP: 58 tools, `tools/call` works | ✅ |
| Docs: `/docs`, `/openapi.json`, `/llms.txt` | ✅ |
| GPU container on the GB10 sees its GPU | ✅ |
| Owner pause: the workload is ready on another node within a second | ✅ |

What this run changed in ogm:

1. **`replicas` defaulted to 0** when left out, although the docs said 1, so the Postgres and Redis templates would have started nothing. It now defaults to 1, through both the API and the CLI.
2. **`ogm push` with Docker's containerd image store** pushed only the host platform. The new `--platform linux/arm64` flag pushes the other platform.
3. **`?wait=30s`** (as documented) was silently ignored; only `?wait=30` worked. Both forms are accepted now, and the same goes for KV `?ttl=`.
4. **Project-scoped keys** could not list their own project or the nodes, which left the dashboard blank. `/v1/me` now reports the credential's type, project and role.
5. **The demo app's `/gpu`** failed in a `FROM scratch` image, because the injected `nvidia-smi` needs glibc. It now falls back to `/proc/driver/nvidia`.
6. **Live-suite hygiene:** resources are cleaned up per subtest, so small nodes don't fill up. A stale Postgres volume with an old password is removed before the test starts.

## Known issues and follow-ups

- **GB10 pricing.** The default credit price is based on VRAM, so unified memory makes the GB10 cost about 5 credits/h. Set it with `PATCH /v1/gpu-classes/gb10`. A better default for unified memory is needed.
- **Heavy GPU work on the GB10 wasn't tested.** It had only about 6 GB of memory free next to the owner's model.
- **Shared-GPU accounting is a scheduler budget, not a hard limit.** CUDA has no per-container VRAM cap without MIG/MPS, so a shared instance that overruns its budget can OOM its neighbours.
- **GB10 model name.** `/proc/driver/nvidia` reports the GB10's model as "Unknown"; `nvidia-smi` shows it correctly.
- **Metrics live in hub memory** (24 h) and reset when the hub restarts. Scrape `/metrics` with Prometheus for history.
- **IMDS listens on `:47474` on all interfaces.** The per-instance secret in the URL protects it, but binding to the Docker bridge only would be tighter.
