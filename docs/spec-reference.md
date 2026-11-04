# Spec reference

`ogm deploy -f FILE` accepts YAML or JSON. A file can hold several documents separated by `---`. The deploy command expands `${ENV_VAR}` (braced form only) from your shell; bare `$VAR`, `$(...)` and `$((...))` in commands pass through untouched, and `$${X}` gives a literal `${X}`. A document with `kind: queue` defines a queue, and one with `kind: schedule` defines a [schedule](services.md#schedules). Any other document is a container group. Deploying is an upsert: the same name updates the existing group or queue in place.

## Container group

| Field | Type | Default | Live update? | Notes |
|---|---|---|---|---|
| `name` | string | required | — | DNS label: lowercase, digits, `-`; max 63 characters |
| `display_name`, `readme` | string | | yes | shown in the dashboard |
| `project` | string | CLI `--project` | — | |
| `replicas` | int 0–1000 | 1 | yes | ignored while `queue_autoscaler` is set |
| `priority` | `high` \| `medium` \| `low` \| `batch` | `medium` | yes | Higher priorities preempt lower ones (with a termination notice). Also sets the credit price multiplier: 1.5 / 1.0 / 0.75 / 0.5 |
| `restart_policy` | `always` \| `on_failure` \| `never` | `always` | rolling | With `on_failure` or `never`, an exited container is **completed** and not replaced. The group ends as `succeeded` once all replicas complete. |
| `autostart_policy` | bool | true | yes | false = create stopped |
| `country_codes` | [string] | any | yes | ISO alpha-2 codes, matched against the agent's `--country` |
| `node_selector` | map | | yes | Matched against agent `--label k=v`. The built-in keys `arch` (`amd64`, `arm64`) and `os` are always available. |
| `container.image` | string | required | rolling | Any registry, or `registry.ogm/{project}/{name}:{tag}` for the [built-in registry](#built-in-image-registry) |
| `container.image_pull_policy` | `always` \| `if_not_present` \| `never` | `always` for `:latest`/untagged, else `if_not_present` | rolling | If a pull fails but the image exists locally, the local copy is used (with an event) |
| `container.command` | [string] | image default | rolling | replaces the image ENTRYPOINT+CMD |
| `container.env` | map | | rolling | Values may reference `{{secret:NAME}}` and `{{link:GROUP:PORT}}`; see [secrets](#secrets) and [links](#links-between-groups) |
| `container.ports` | [int] | | rolling | Extra ports other groups may reach through `{{link:...}}` (the networking, queue and probe ports are always published) |
| `container.volumes` | `[{name, mount}]` | | rolling | Persistent volumes; see [volumes](#persistent-volumes) |
| `container.registry_auth` | `{server, username, password}` | | rolling | The API returns the password masked. Sending the masked value back keeps the stored password. |
| `container.resources.cpu` | int | 1 | rolling | vCPUs (hard limit) |
| `container.resources.memory_mb` | int | 1024 | rolling | hard limit |
| `container.resources.gpu_count` | int | 0 | rolling | 0 = CPU-only |
| `container.resources.gpu_classes` | [slug] | any | rolling | Any-of match, e.g. `rtx-4090`. `ogm gpus` lists classes. Setting this implies `gpu_count: 1`. |
| `container.resources.min_vram_mb` | int | | rolling | Matches any GPU with at least this much VRAM. Implies `gpu_count: 1`. |
| `container.resources.storage_gb` | int | 10 | rolling | node must have this much free disk |
| `container.resources.shm_mb` | int | 64 | rolling | `/dev/shm` (raise for PyTorch dataloaders/vLLM) |
| `container.resources.gpu_shared` | bool | false | rolling | Share one GPU with other shared instances. Requires `min_vram_mb`, which becomes this instance's VRAM budget. Placement counts VRAM the owner is using plus pending reservations. Credits are charged for the VRAM slice only. |
| `networking.port` | int | | rolling (port) / yes (rest) | Enables the gateway. |
| `networking.auth` | bool | false | yes | Accepts a member API key or the group's share token, via `OGM-Api-Key`, `Authorization: Bearer`, or `?ogm_token=`. |
| `networking.load_balancer` | `least_number_of_connections` \| `round_robin` | least | yes | |
| `networking.single_connection_limit` | bool | false | yes | One request per instance at a time; the rest wait in a queue. Same as `max_concurrency: 1`. |
| `networking.max_concurrency` | int 0–100000 | 0 (unlimited) | yes | Hard cap of requests in flight per instance; extra requests wait (up to `client_request_timeout_ms`) |
| `networking.custom_domains` | [hostname] | | yes | Extra hostnames routed to this group (point their DNS at the hub). Each domain may belong to one group only. With `--tls` the hub gets certificates for them automatically. See [services](services.md#custom-domains-and-tls). |
| `networking.client_request_timeout_ms` | int | 100000 | yes | max time a request may wait for a free or ready instance (504) |
| `networking.server_response_timeout_ms` | int ≤ 3600000 | 100000 | yes | Time until the app sends response headers (524). Streaming after that point has no limit. |
| `startup_probe`, `readiness_probe`, `liveness_probe` | probe | | rolling | see below |
| `queue_connection` | `{queue_name, port, path, concurrency}` | | yes | `concurrency` = jobs in flight per instance (default 1) |
| `queue_autoscaler` | see below | | yes | |
| `http_autoscaler` | see below | | yes | Needs `networking`; not combined with `queue_autoscaler` |
| `scaling_actions` | `[{schedule, replicas}]` | | yes | 5-field cron in the server's timezone. The most recent action sets `replicas`. |
| `scheduled_scaling_enabled` | bool | false | yes | Must be `true` for `scaling_actions` to apply. It lets you pause a schedule without deleting it. |

**Placement and GPU memory.** `min_vram_mb` is checked against the VRAM a GPU actually has free, so work never lands on a GPU the owner's own models already fill. GPUs with unified memory (NVIDIA GB10/DGX Spark, Jetson) report system RAM as VRAM. Images in the built-in registry are only scheduled on nodes whose architecture the image provides.

"Rolling" means the change bumps `version`. Instances are then replaced gradually: at most `max(1, 25%)` are being replaced at once, and each new instance must become ready before the next old one stops.

### Probes

```yaml
readiness_probe:
  http: { path: /health, port: 8080, scheme: http, headers: { X-Probe: "1" } }
  # or  tcp:  { port: 8080 }
  # or  exec: { command: ["sh", "-c", "test -f /ready"] }
  initial_delay_seconds: 0
  period_seconds: 1
  timeout_seconds: 1
  success_threshold: 1
  failure_threshold: 3
```

Defaults when a field is omitted:

| Probe | period_seconds | timeout_seconds | success_threshold | failure_threshold |
|---|---|---|---|---|
| startup | 3 | 10 | 2 | 15 |
| readiness | 1 | 1 | 1 | 3 |
| liveness | 10 | 30 | 1 | 3 |

Slow model loads usually need a startup probe with a large `failure_threshold`, e.g. `period_seconds: 10, failure_threshold: 120` for 20 minutes.

- **startup:** the other probes wait until this one passes once. If it fails `failure_threshold` times in a row, the container is restarted.
- **readiness:** the instance receives gateway traffic and queue jobs only while this probe passes. With no readiness probe, an instance is ready once it is running (and once the startup probe passes, if there is one).
- **liveness:** failing `failure_threshold` times restarts the container (crash-loop backoff from 1 s up to 60 s).

### Queue autoscaler

```yaml
queue_autoscaler:
  min_replicas: 0            # 0 = scale to zero when idle
  max_replicas: 20
  desired_queue_length: 3    # target (pending + running) jobs per replica (default 1)
  polling_period: 30         # seconds between evaluations (default 30)
  max_upscale_per_minute: 10 # default 10
  max_downscale_per_minute: 2 # default 10
```

`target = clamp(ceil(queue_length / desired_queue_length), min, max)`, then limited to the up/down rate. This is the same formula SaladCloud uses, so existing configs carry over.

### HTTP autoscaler

Scales on gateway traffic, including from zero: a request to a group with no instances raises the replica count to 1 and waits (up to `client_request_timeout_ms`) for the instance to become ready.

```yaml
http_autoscaler:
  min_replicas: 0               # 0 = scale to zero when idle
  max_replicas: 10
  target_concurrency: 10        # requests in flight (plus queued) per replica (default 10)
  stable_window_seconds: 60     # averaging window, 6-600 (default 60)
  scale_down_delay_seconds: 300 # demand must stay lower this long before scaling down (default 300)
```

Demand is sampled every second. `want = clamp(max(ceil(avg_window / target), ceil(avg_last_6s / target)), min, max)`. The 6-second term reacts to bursts. Scale-up is immediate; scale-down waits for `scale_down_delay_seconds`. While this is set, `replicas` is managed by the autoscaler.

### Secrets

```sh
ogm secret set pg-password          # prompts; or: ogm secret set NAME VALUE | --from-file F
ogm secret ls                       # names and dates only; values can't be read back
```

Reference a secret in any `container.env` value or in `container.registry_auth.password` as `{{secret:NAME}}`. Values are encrypted at rest (AES-256-GCM, key in `secrets.key` next to the database, so a copy of the database alone reveals nothing) and expanded only when an instance is started on a node. If a referenced secret doesn't exist, the instance waits with a message instead of starting; changing a secret applies to instances started afterwards (use `ogm recreate` to roll it out).

The node owner can still see a running container's environment. See the trust model in the README.

### Links between groups

```yaml
env:
  DATABASE_URL: postgres://app:{{secret:pg-password}}@{{link:postgres:5432}}/app
  REDIS_HOST: "{{link_host:redis:6379}}"
  REDIS_PORT: "{{link_port:redis:6379}}"
```

`{{link:GROUP:PORT}}` becomes a `host:port` address local to the instance's node. Connections to it are relayed through the hub to a ready instance of `GROUP` in the same project, wherever it runs, over the agents' existing outbound tunnels: no VPN, port forwarding or public IP. The target group must publish the port (`container.ports`, its networking port or its queue port). Only links declared in a group's own env are allowed. If the target has no ready instance yet, a connection waits up to 90 s; a target with an `http_autoscaler` is woken from zero.

Traffic goes through the hub, so for large transfers put the groups near the hub or use the file store.

### Persistent volumes

```yaml
container:
  volumes:
    - { name: data, mount: /var/lib/postgresql/data }
```

Each replica gets its own volume per entry (Docker named volume `ogm-vol-{project}_{group}_{name}_{replica}`). The first time a replica runs, it is pinned to that node; later instances of that replica (restarts, updates, stop/start) always go back to the same node and see the same data. If the node is offline, the replica waits for it rather than starting empty elsewhere. Deleting the group keeps the volumes, and redeploying the same name reuses them.

```sh
ogm volumes                         # group, replica, node, in use
ogm volumes rm GROUP [--replica N]  # delete the data (group must be stopped/removed, node online)
```

Volumes live on one volunteer's disk with no replication: keep backups of anything you can't lose. With the process runtime (dev only), volumes are directories exported as `$OGM_VOLUME_<NAME>`.

### Environment inside every container

| Variable | |
|---|---|
| `OGM_INSTANCE_ID`, `OGM_CONTAINER_GROUP`, `OGM_CONTAINER_GROUP_ID`, `OGM_PROJECT` | identity |
| `OGM_MACHINE_ID`, `OGM_NODE_COUNTRY`, `OGM_GPU_NAME` | where it runs |
| `OGM_REPLICA_INDEX`, `OGM_REPLICAS` | Stable index 0..N-1 for sharding work. A replacement instance inherits the index it replaces. In run-to-completion groups, the index of a completed instance is never handed out again. |
| `OGM_IMDS_URL` | instance metadata service (see [api.md](api.md#imds)) |
| `OGM_SERVER_URL` | cluster URL |

## Built-in image registry

Every cluster includes an OCI registry, so custom images need no Docker Hub or GHCR account:

```sh
docker build -t my-app:dev .
ogm push my-app:dev                 # -> registry.ogm/{project}/my-app:dev
ogm push --multiarch my-app:dev     # from x86 AND from ARM: the tag becomes a multi-platform image
ogm push --platform linux/arm64 postgres:17-alpine   # add another platform of a local multi-platform image
                                    # (Docker's containerd image store; pull it first with --platform)
ogm images
```

Use `image: registry.ogm/{project}/my-app:dev` in a spec. Agents pull through a localhost proxy authenticated with their node token, so the Docker daemons need no config. Repositories are scoped by project: members can push and pull their projects' images, and nodes can pull only. To use plain `docker push` with other tools, run `ogm registry-proxy` (it listens on `127.0.0.1:5078`).

## Queue

```yaml
kind: queue
name: sdxl
description: SDXL jobs
settings:
  max_attempts: 3            # 1-20
  retry_backoff_base_s: 2    # delay = base * 2^(attempt-1), capped, ±20% jitter
  retry_backoff_max_s: 300
  job_timeout_s: 3600        # per attempt; jobs can override with timeout_s
  retry_on_4xx: false        # 4xx (except 408/429) = bad input, fail immediately
  max_output_bytes: 67108864 # 64 MB
  retention_hours: 168       # finished jobs are deleted after this (default 7 days)
```

All settings can be changed at any time with `PATCH` or by redeploying. Deleting a queue that groups are still attached to returns 409 unless you pass `?force=1`, which detaches them explicitly.

### Job

```json
POST /v1/projects/{p}/queues/{q}/jobs
{ "input": {...}, "metadata": {...}, "webhook": "https://...", "max_attempts": 5, "timeout_s": 600 }
```

Send `{"jobs":[...]}` to submit up to 10,000 jobs in one batch. Jobs move through `pending → running → succeeded | failed | cancelled`. Each job carries an `events` array recording every attempt: the instance it ran on and the error, if any.
