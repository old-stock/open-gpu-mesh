# open-gpu-mesh (ogm) — Specification v0.1

> Get a hundred friends, pool your GPUs, run cool things. One binary. No cloud account.

`ogm` is a self-hosted, community-scale distributed GPU container platform. One person runs the
**server** (a $5 VPS is enough). Friends run the **agent** on their gaming PCs / homelab boxes.
Anyone in the group can deploy **container groups** (N replicas of a Docker image) that get
scheduled across everyone's idle GPUs, exposed through a load-balancing **container gateway**,
fed by **job queues**, and autoscaled — the core of SaladCloud's Container Engine (SCE), but
open source, self-hostable, and fixing the sharp edges SaladCloud documents in its own docs.

---

## 1. Competitive analysis: SaladCloud SCE (from public docs, OpenAPI spec, and OSS repos)

Sources: `SaladTechnologies/salad-cloud-docs` (OpenAPI `api-specs/salad-cloud.yaml` v0.9.0-alpha.17,
`salad-cloud-imds.yaml`, `s4.yml`), `salad-cloud-job-queue-worker`, `kelpie`, `kelpie-api`,
`salad-cloud-imds-sdk-go`, `virtual-kubelet-saladcloud`.

### 1.1 Their feature surface (and our equivalent)

| SaladCloud capability | Details from their spec/docs | ogm equivalent |
|---|---|---|
| Container groups CRUD, start/stop | `/organizations/{o}/projects/{p}/containers[...]`, version++, `pending_change`, `autostart_policy`, `restart_policy` (always/on_failure/never), `readme` | `/v1/projects/{p}/containers[...]` same shape, + declarative `ogm deploy -f app.yaml` upsert |
| Resources | cpu 1–16, memory 1–60 GB, `gpu_classes[]` (UUIDs), `storage_amount` ≤250 GB, `shm_size` | same + **`min_vram_mb`**, **`gpu_count`**, GPU classes by human slug (`rtx-4090`) |
| Priorities | high/medium/low/batch, preemption by higher priority; can't lower priority for 60 min | same tiers + **fair-share credits** tiebreaker inside a tier; priority changes free |
| Country targeting | `country_codes[]` ISO alpha-2 | `country_codes[]` + arbitrary node **labels** selector |
| Instances | list/get/patch(deletion_cost)/reallocate/recreate/restart; states allocating→downloading→creating→running→stopping; cpu/mem usage; pulling progress; ssh info | same states + **GPU util/VRAM/temperature** per instance |
| Probes | startup/liveness/readiness: http/tcp/grpc/exec, thresholds | startup/liveness/readiness: http/tcp/exec (grpc via exec) — run by the agent locally |
| Container Gateway | HTTPS LB, round_robin / least_number_of_connections, `single_connection_limit`, client/server timeouts ≤100 s, auth via `Salad-Api-Key`, IPv6 required, 1 GB max request, built on Cloudflare; **settings immutable after creation**; websockets die after 100 s idle and can't auth | Reverse tunnel gateway: no inbound ports/IPv6 needed on nodes; RR / least-conn / **single-conn w/ queue**; **mutable settings**; **timeouts configurable without a 100 s cap**; streaming + websockets; auth via header **or `?ogm_token=` for websockets** |
| Job Queues | Managed FIFO; HTTP job → POST to container port/path; worker binary (x86_64 only) must be baked into image; 3 retries, **no backoff**, retries may hit same instance; interruptions noticed only after lease expiry (minutes); **cancel doesn't stop running job**; response ≤10 MB, oversize = silent 4x fail; queue connection immutable; deleting queue silently detaches groups | **Agent-side dispatch: no binary in your image**; configurable `max_attempts`, **exponential backoff**, **avoid-same-instance on retry**; **instant requeue on node disconnect**; **cancel aborts the in-flight request**; per-job timeout; failure reason recorded; output ≤ 64 MB (configurable); queue connection **mutable**; delete refused while attached (or `?force`) |
| Queue autoscaler | desired_queue_length, min/max replicas, max up/down per minute, polling period | same formula + scale-to-zero with **cold-start hint**; also scale on gateway in-flight |
| Scheduled scaling | `scaling-actions[]` cron → replicas | same |
| Deletion cost | IMDS PUT `/v1/deletion-cost` or API patch; lowest deleted first on scale-in | same |
| IMDS | `169.254.169.254`: status, token (JWT, JWKS), deletion-cost, reallocate, recreate, restart | `$OGM_IMDS_URL`: same endpoints + **`/v1/termination-notice`** (graceful preemption/pause notice — SaladCloud gives *no* notice) + `/v1/metadata` |
| Webhooks | Standard Webhooks (svix-compatible HMAC-SHA256), per-org secret | identical scheme (svix-compatible) |
| System events | group started/stopped/replicas/updated; instance allocated/downloading/…/interrupted (priority/offline/disk)/exited codes; probe events | same event vocabulary |
| Logs | Log explorer `POST /log-entries` query; external: Axiom, Datadog, New Relic, Splunk, HTTP, TCP | built-in log store + query + **live tail (SSE)**; external HTTP/TCP forwarding |
| Storage (S4) | `storage-api.salad.com/organizations/{o}/files/{name}` PUT/GET/DELETE/list, signed URLs | `/v1/files/{path}` PUT/GET/DELETE/list + HMAC signed URLs, stored on the server |
| Long-running jobs (Kelpie) | separate service: command/args/env, S3 sync before/during/after, heartbeats, max_failures, checkpoints, webhook | Roadmap: `kind: task` queue jobs using `/v1/files` for checkpoint sync (v0.2) |
| Quotas | replicas quota, reallocations/recreates/restarts per minute | per-user replica quota + credit floor |
| GPU classes / availability | list classes w/ prices per priority; availability API | auto-discovered GPU classes from nodes, live availability (online/free counts per class) |
| SSH/terminal | portal web terminal + SSH | `ogm exec` over the tunnel (v0.2) |
| Image caching | central cache, pull once | nodes pull directly; optional pull-through registry mirror config (v0.2) |
| Inference endpoints, transcription, AI gateway | Salad-hosted products | out of scope (build them *on* ogm as recipes) |

### 1.2 How they appear to be architected (inferred)

- **Supply side**: Windows app installs "Salad Enterprise Linux" in WSL2; it runs containers, coordinates with backend, Falco intrusion detection, requests work when idle.
- **Control plane** (`matrix-rest-api.salad.com`, JWKS at `/.well-known/workload-jwks.json`): REST public API, scheduler that allocates instances to nodes, reallocation policies (time-to-start, networking).
- **Image pipeline**: pull → compress → store in internal cache → distribute to nodes (no charge while downloading).
- **Job Queues**: separate gRPC service (`JobQueueWorkerService.AcceptJobs` server-stream with heartbeats, `CompleteJob`, `RejectJob`); worker authenticates with the IMDS JWT, gates on IMDS readiness, one job at a time, POSTs input to `localhost:{port}{path}`.
- **Gateway**: built on Cloudflare; WireGuard to nodes; nodes must accept IPv6.
- **Kelpie** (Cloudflare Workers + D1): job table with heartbeat/num_failures/machine_id; scaling rules min/max/idle_threshold; banning workers that fail too often.

### 1.3 Where ogm wins

1. **Self-hosted & free**: one binary, SQLite, runs on a tiny VPS. Your friends, your rules, no margin.
2. **Zero-config nodes**: agent dials *out* over WebSocket; gateway traffic rides a multiplexed tunnel back. No port-forwarding, no IPv6, no WireGuard setup.
3. **Nothing to bake into your image**: the agent itself is the queue worker and the IMDS host.
4. **Honest job semantics**: backoff, retry-elsewhere, instant interruption detection, real cancellation, failure reasons, mutable queue wiring.
5. **Graceful interruptions**: owners pausing (e.g. to game) or preemption trigger SIGTERM + a termination notice with a deadline via IMDS.
6. **Friend-fair**: a credits ledger (GPU-hours contributed vs consumed) drives fair-share scheduling. No money changes hands unless you want it to.
7. **Owner controls**: max GPUs, availability windows, auto-yield when the owner's own GPU load spikes, pause/resume.
8. **Mutable everything**: gateway LB settings, timeouts, queue connection, priority — change live.

---

## 2. Architecture

```
            ┌──────────────────────── ogm server (one box) ─────────────────────────┐
 users ───► │ REST API /v1   Dashboard /   Gateway (*.gw.domain or /gw/p/g/)        │
  CLI/SDK   │   │               │             │  reverse proxy, LB, auth          │
            │   ▼               ▼             ▼                                    │
            │ Store (SQLite) ◄─ Reconciler/Scheduler ─ Autoscaler ─ Queue dispatcher│
            │   ▲  logs, events, credits ledger, files (disk), webhooks             │
            │   └──── Tunnel hub (WebSocket + yamux, one session per node) ─────────┤
            └──────────────────────────────▲───────────────────────────────────────┘
                                           │ outbound WSS from each node
          ┌──────────────── ogm agent (friend's PC) ────────────────┐
          │ control stream: hello/heartbeat/instance.*/job.*/logs    │
          │ runtime: docker (+ NVIDIA toolkit) | process (dev only)  │
          │ probes, IMDS server, job executor, log shipper, GPU stats│
          └──────────────────────────────────────────────────────────┘
```

### 2.1 Components

- **`ogm server`** — HTTP server hosting API, dashboard, gateway, tunnel hub. Background loops: reconciler (2 s), autoscaler (per-group polling period), queue dispatcher (event-driven + 1 s tick), credits accrual (60 s), retention GC (10 min).
- **`ogm agent`** — runs on contributor machines. Connects to `wss://server/v1/agent/connect` with a node token. Executes instance specs via a runtime, runs probes, serves IMDS, executes queue jobs, ships logs/metrics.
- **`ogm` CLI** — talks to the REST API.
- **`ogm dev`** — server + N simulated agents (process runtime) in one process, for trying everything on a laptop.

### 2.2 Tunnel protocol

- Agent opens WebSocket → wraps as `net.Conn` → **yamux client**; server is yamux server.
- First stream opened by the agent = **control stream**: newline-delimited JSON `Envelope{type, id?, data}`.
- Server opens additional streams for **data**: first line is JSON `{"kind":"proxy","instance_id":"…","port":8080}`; agent dials the instance port and splices bytes. HTTP/1.1 keep-alive, websockets, SSE, chunked streaming all pass through untouched.
- Control messages:
  - agent→server: `hello{node info, gpus, version, running instances}`, `heartbeat{metrics, gpu stats, paused}`, `instance.status{id, state, ready, started, exit_code, message, metrics}`, `job.result{job_id, status, http_status, output, error}`, `logs{entries[]}`, `imds.request{instance_id, op, args}` (deletion-cost/reallocate/restart/recreate)
  - server→agent: `welcome{node_id, jwt signing pubkey}`, `instance.start{spec}`, `instance.stop{id, grace_s, reason}`, `instance.restart{id}`, `instance.recreate{id}`, `job.assign{job, instance_id, port, path, timeout_s}`, `job.cancel{job_id}`, `node.config{...}`
- Node is *offline* when the session closes; instances get a 20 s grace window to reconnect (agent restarts/network blips) before being declared interrupted and rescheduled. In-flight jobs on a dropped session are requeued **immediately** (they can't report back anyway; dispatch is idempotent by job id+attempt).

### 2.3 Data model (SQLite)

- `users(id, name, role[admin|member], api_key_hash, credits, created)` ; `invites(code, role, uses_left, expires)`
- `projects(name, owner_id, created)` ; `project_members(project, user_id)`
- `nodes(id, name, owner_id, token_hash, status, info_json, labels_json, country, last_seen, paused, max_gpus, created)`
- `gpu_classes(id slug, name, vram_mb, credits_per_hour)` — auto-created from node GPU names
- `container_groups(id, project, name, display_name, spec_json, version, status, replicas, created, updated, start_time, priority_lowered_at)`
- `instances(id, group_id, project, node_id, gpu_index, version, state, ready, started, deletion_cost, exit_code, message, metrics_json, created, updated, assigned_at, running_at, imds_secret)`
- `queues(id, project, name, display_name, description, settings_json, created)`
- `jobs(id, queue_id, status, input, output, metadata, webhook, attempts, max_attempts, timeout_s, next_attempt_at, instance_id, last_instance_id, error, events_json, created, updated)`
- `events(id, project, group_id, instance_id, node_id, kind, message, time)`
- `logs(id, project, group_id, instance_id, node_id, stream, line, time)`
- `ledger(id, user_id, delta, reason, ref, time)`
- `files(project, path, size, sha256, content_type, created)` (bytes on disk under `data/files/`)
- `settings(key, value)` (webhook secret, jwt keys, cluster name, gateway domain…)

### 2.4 Scheduling

Reconciler tick:
1. For each group with `status ∈ {deploying, running}`: compute desired replicas (manual, or autoscaler, or scheduled action). Count live instances (not `stopping/terminated`).
   - Too few → create `allocating` instances.
   - Too many → choose victims: lowest `deletion_cost`, then not-ready, then not-yet-running, then newest → `stopping` (graceful).
   - Version drift (spec updated) → rolling replace: at most `max(1, 25%)` instances in replacement at a time, wait until new ones are ready.
2. Placement for every `allocating` instance, ordered by (priority desc, owner credit balance desc, created asc):
   - Candidate nodes: online, not paused, within availability window, agent capacity (GPU free/CPU/RAM/disk), GPU class ∈ requested (or VRAM ≥ `min_vram_mb`), `country_codes`, label selectors, not banned for this group (reallocate bans node for group for 30 min), owner's allowed priorities.
   - Score: prefer nodes already caching the image, then higher reliability (uptime ratio), fewer instances (spread), lower latency.
   - If none fit and request priority > some running instance's priority on a fitting node → **preempt** the lowest-priority/lowest-deletion-cost instance (graceful stop w/ termination notice, event `Instance Interrupted (Priority)`).
3. Send `instance.start` to the chosen node's agent; state `downloading` → `creating` → `running` driven by agent reports.

### 2.5 Gateway

- Routing: `https://{group}-{project}.{gateway_domain}/…` (wildcard DNS) or path mode `https://server/gw/{project}/{group}/…` (prefix stripped, `X-Forwarded-Prefix` set).
- Auth (if `networking.auth`): `OGM-Api-Key: <key>` / `Authorization: Bearer <key>` / `?ogm_token=` (stripped before forwarding). Any project member key or a per-group **gateway token**.
- Backends: instances that are `running && ready` (readiness defaults to startup/started when no probe).
- LB: `round_robin`, `least_number_of_connections` (default). `single_connection_limit`: per-instance semaphore of 1; waiting requests queue up to `client_request_timeout_ms`.
- Retries: idempotent requests (GET/HEAD/OPTIONS) or requests that failed before any byte was sent to the backend (dial/tunnel failure) are retried on a different instance (up to 2).
- Timeouts: `server_response_timeout_ms` (time to first response byte; default 100 s, max 1 h); streaming bodies unaffected after headers.
- Errors: 503 `no_ready_instances`, 504 `client_request_timeout`, 524 `server_response_timeout`, 502 `backend_unreachable` — JSON problem details.
- Headers added: `X-OGM-Instance-Id`, `X-OGM-Node-Id` on responses; `X-Forwarded-For/Proto/Host` on requests.

### 2.6 Job queues

- Create queue → attach to group via `queue_connection{queue_name, port, path, concurrency}` (mutable).
- Job lifecycle: `pending → running → succeeded|failed|cancelled`; events array `{action, time, instance_id?, attempt?, error?}`.
- Dispatch: for each queue with pending jobs whose `next_attempt_at ≤ now`, pick ready instances in attached groups with free slots (`concurrency`, default 1), preferring an instance other than `last_instance_id`. Send `job.assign`.
- Agent POSTs `input` JSON to `http://127.0.0.1:{port}{path}` with headers `OGM-Job-Id`, `OGM-Job-Attempt`, timeout = job `timeout_s` (default 3600). 2xx → succeeded, output = response JSON (or `{"text": …}` if not JSON). Non-2xx/timeout/conn error → attempt failed with reason.
- Retry: attempts < `max_attempts` (default 3, max 20) → `pending` with `next_attempt_at = now + min(base·2^(n-1), cap)` + jitter (queue settings `retry_backoff_base_s`=2, `retry_backoff_max_s`=300). Else `failed`.
- HTTP 4xx (except 408/429) from the app → non-retryable failure by default (`retry_on_4xx: false`) — the app said the input is bad.
- Cancel pending → cancelled. Cancel running → `job.cancel` to agent which aborts the request; status `cancelled`.
- Webhooks on terminal states: `queue.job.succeeded|failed|cancelled`, Standard Webhooks signature, 5 attempts with backoff.
- Output limit: 64 MB default (`max_output_bytes`); exceeding → non-retryable failure `output_too_large`.

### 2.7 Autoscaling

- Queue autoscaler (SaladCloud-compatible fields): every `polling_period` s: `target = ceil(queue_len / desired_queue_length)`, clamp `[min_replicas, max_replicas]`, rate-limit by `max_upscale_per_minute`/`max_downscale_per_minute`. `queue_len` = pending + running.
- Scheduled scaling: `scaling_actions[{schedule (cron), replicas}]` when `scheduled_scaling_enabled` — latest-fired action sets base replicas.
- Precedence: queue autoscaler if configured; else scheduled; else manual `replicas`.

### 2.8 IMDS (per instance)

`OGM_IMDS_URL=http://{host}:{port}/i/{imds_secret}` injected into the container env. All requests need header `Metadata: true`. (Also `SALAD_MACHINE_ID`-style env: `OGM_MACHINE_ID`, `OGM_INSTANCE_ID`, `OGM_CONTAINER_GROUP`, `OGM_PROJECT`, `OGM_NODE_COUNTRY`, `OGM_GPU_NAME`, plus `OGM_REPLICA_INDEX` (stable 0..N-1, reused by replacements, never reassigned from completed run-to-completion instances) and `OGM_REPLICAS`.)
- `GET /v1/status` → `{ready, started}`
- `GET /v1/token` → `{jwt}` EdDSA JWT (`sub`=instance id, claims: project, group, node, iat/exp 1 h); JWKS at server `/.well-known/jwks.json`
- `GET|PUT /v1/deletion-cost` → `{deletion_cost}`
- `POST /v1/reallocate {reason}`, `POST /v1/recreate`, `POST /v1/restart`
- `GET /v1/termination-notice` → `{pending: bool, deadline, reason}` (also delivered as SIGTERM)
- `GET /v1/metadata` → instance/group/node/gpu info

### 2.9 Credits

- Each GPU class has `credits_per_hour` (default 1.0 × VRAM_GB/24 rounded, admin-editable). Priority multipliers: high 1.5, medium 1.0, low 0.75, batch 0.5. CPU-only: 0.05/vCPU-hour.
- Every 60 s: running instances debit the group owner, credit the node owner (ledger rows). New users start with `starter_credits` (default 10).
- Optional enforcement: `min_balance` (default −∞ = off). When on, users below floor can only deploy `batch`.
- Fair share: placement ordering uses balance within the same priority tier.

### 2.10 Security model (honest)

- Containers run unprivileged with resource limits, no host mounts, `--security-opt no-new-privileges`, PID limit, optional gVisor/`runsc` runtime if installed. Agent never runs DinD.
- Env vars and registry creds go only to the node running the instance; stored in server DB (encrypt-at-rest with server key — v0.2).
- Node owners *can* inspect containers on their own hardware. ogm is for **trusted groups of friends**. Don't put secrets you wouldn't hand to the group in images/env.
- API keys hashed (SHA-256), node tokens hashed, JWTs EdDSA, webhook HMAC.

---

## 3. API surface (v1)

Auth: `OGM-Api-Key: <key>` or `Authorization: Bearer <key>`. Errors: RFC 7807 problem details `{type,title,status,detail,errors}`.

```
GET    /v1/me                                   GET /v1/me/ledger
POST   /v1/invites (admin)                      POST /v1/join {code,name} → {api_key}
GET    /v1/users (admin)
GET    /v1/projects   POST /v1/projects   DELETE /v1/projects/{p}   POST /v1/projects/{p}/members
GET    /v1/projects/{p}/containers              POST  (create)
GET    /v1/projects/{p}/containers/{g}          PATCH (merge-patch)   DELETE  PUT (upsert/declarative)
POST   /v1/projects/{p}/containers/{g}/start|stop|rotate-gateway-token
GET    /v1/projects/{p}/containers/{g}/instances
GET    /v1/projects/{p}/containers/{g}/instances/{id}   PATCH {deletion_cost}
POST   /v1/projects/{p}/containers/{g}/instances/{id}/reallocate|recreate|restart
GET    /v1/projects/{p}/containers/{g}/events
GET    /v1/projects/{p}/containers/{g}/logs?since=&instance=&q=&limit=&follow=1 (SSE)
GET    /v1/projects/{p}/queues                  POST
GET    /v1/projects/{p}/queues/{q}              PATCH DELETE(?force=1)
GET    /v1/projects/{p}/queues/{q}/jobs?status= POST (single or {"jobs":[…]} batch)
GET    /v1/projects/{p}/queues/{q}/jobs/{id}    DELETE (cancel)
GET    /v1/projects/{p}/files?prefix=           PUT/GET/DELETE /v1/projects/{p}/files/{path...}
POST   /v1/projects/{p}/file-tokens/{path...}   → {url, expires}   GET /v1/signed/{p}/{path}?exp=&sig=
GET    /v1/nodes          POST /v1/nodes (create join token)   PATCH/DELETE /v1/nodes/{id}
GET    /v1/gpu-classes    PATCH /v1/gpu-classes/{id} (admin)
GET    /v1/availability   (per class: online, free, in use)
GET    /v1/webhook-secret POST /v1/webhook-secret (rotate)
GET    /v1/cluster        (name, version, stats)
GET    /.well-known/jwks.json
GET    /v1/agent/connect  (WebSocket, node token)
GET    /install.sh        GET /dl/{os}-{arch}
```

## 4. Container group spec (YAML for `ogm deploy`)

```yaml
name: sdxl
project: default
replicas: 3
priority: low                # high|medium|low|batch
restart_policy: always        # always|on_failure|never
country_codes: [us, ca]
node_selector: {tier: fast}
container:
  image: ghcr.io/me/sdxl:latest
  command: ["python", "serve.py"]
  env: {MODEL: sdxl}
  registry_auth: {username: me, password: ${GHCR_TOKEN}}
  resources:
    cpu: 4
    memory_mb: 16384
    gpu_classes: [rtx-4090, rtx-3090]   # any-of; or:
    min_vram_mb: 20000
    gpu_count: 1
    storage_gb: 50
    shm_mb: 1024
networking:                  # container gateway
  port: 8080
  auth: true
  load_balancer: least_number_of_connections
  single_connection_limit: false
  client_request_timeout_ms: 100000
  server_response_timeout_ms: 600000
readiness_probe: {http: {path: /health, port: 8080}, period_seconds: 2}
startup_probe:   {http: {path: /health, port: 8080}, failure_threshold: 120, period_seconds: 5}
liveness_probe:  {tcp: {port: 8080}}
queue_connection: {queue_name: sdxl-jobs, port: 8080, path: /generate, concurrency: 1}
queue_autoscaler: {min_replicas: 0, max_replicas: 50, desired_queue_length: 4, polling_period: 30,
                   max_upscale_per_minute: 10, max_downscale_per_minute: 5}
scaling_actions: [{schedule: "0 9 * * 1-5", replicas: 10}, {schedule: "0 18 * * *", replicas: 2}]
scheduled_scaling_enabled: false
readme: "SDXL inference"
```

## 5. Agent

```
ogm agent --server https://gpu.example.com --token <node-token> \
  [--name bob-4090] [--runtime docker|process] [--max-gpus 1] [--max-cpu 8] [--max-memory-mb 24000] \
  [--max-disk-gb 100] [--country us] [--label tier=fast] [--window 00:00-09:00,22:00-24:00] \
  [--yield-gpu-util 30] [--allow-priorities high,medium,low,batch]
```
- Detects GPUs via `nvidia-smi --query-gpu=index,uuid,name,memory.total,utilization.gpu,temperature.gpu,memory.used`.
- Docker runtime via Engine API on `/var/run/docker.sock`: pull (with registry auth), create (DeviceRequests for the assigned GPU index, CPU/mem/shm/pids limits, port bind to 127.0.0.1:random), start, logs follow, stats, stop w/ grace, remove.
- **Auto-yield**: if a GPU shows utilization ≥ `--yield-gpu-util` from processes not belonging to ogm for 20 s (owner started gaming), the agent pauses itself: instances get a termination notice + SIGTERM with grace, node reports `paused`, resumes after 5 min of quiet.
- Local control: `ogm agent pause|resume|status` via a unix socket.

## 6. Milestones

- **v0.1 (this build)**: server, agent (docker + process runtimes), CLI, gateway, queues, autoscaler, scheduled scaling, IMDS, probes, logs, events, credits, files, webhooks, dashboard, `ogm dev`, install script, tests.
- **v0.2**: `ogm exec` terminal, long-running `task` jobs with checkpoint sync (Kelpie-equivalent), env encryption at rest, pull-through image mirror, external log forwarders (Axiom/Datadog/…), SaladCloud-compatible API shim (`/api/public/organizations/...`) so existing Salad SDKs work, multi-server HA (Postgres).
- **v0.3**: Windows tray app for contributors, WSL2 auto-setup, gVisor default, reputation & node verification benchmarks.
