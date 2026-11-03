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

## Known issues and follow-ups

- **GB10 pricing.** The default credit price is based on VRAM, so unified memory makes the GB10 cost about 5 credits/h. Set it with `PATCH /v1/gpu-classes/gb10`. A better default for unified memory is needed.
- **Heavy GPU work on the GB10 wasn't tested.** It had only about 6 GB of memory free next to the owner's model.
- **Shared-GPU accounting is a scheduler budget, not a hard limit.** CUDA has no per-container VRAM cap without MIG/MPS, so a shared instance that overruns its budget can OOM its neighbours.
- **IMDS listens on `:47474` on all interfaces.** The per-instance secret in the URL protects it, but binding to the Docker bridge only would be tighter.
