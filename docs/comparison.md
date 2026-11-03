# ogm vs SaladCloud

SaladCloud's Container Engine is the closest product to ogm: a scheduler that runs containers on consumer GPUs, a load-balancing gateway, job queues, and an autoscaler. This comparison draws only on SaladCloud's **public** docs, OpenAPI spec, and open-source repos (`salad-cloud-docs`, `salad-cloud-job-queue-worker`, `kelpie`, `salad-cloud-imds-sdk-go`). The field-by-field mapping is in [SPEC.md §1.1](../SPEC.md#11-their-feature-surface-and-our-equivalent).

## Where ogm is better

| Problem | SaladCloud (documented behaviour) | ogm |
|---|---|---|
| Who runs it | A managed service you pay per GPU-hour | Self-hosted, free. Your friends' GPUs, settled in credits |
| Node networking | The gateway reaches nodes over IPv6/WireGuard | Nodes dial out over WSS. Works behind any NAT, no IPv6 needed |
| Queue worker | A worker binary (x86_64 only) must be baked into your image | Nothing to add to your image. The agent delivers jobs over HTTP |
| Retries | 3 fixed retries, no backoff, may hit the same instance | 1–20 attempts, exponential backoff with jitter, retries go to a different instance |
| Interrupted nodes | Detected when the job lease expires (minutes) | Job requeued the moment the node's tunnel drops |
| Cancel | Doesn't stop a running job | Aborts the in-flight request in the container |
| Bad input | Retried like any other failure | 4xx = bad input, fails fast (configurable) |
| Large outputs | Outputs over 10 MB fail silently | 64 MB default (configurable), explicit `output_too_large` error |
| Changing queue/gateway settings | Fixed at creation (recreate the group) | Every setting can be changed live. Only runtime fields trigger a rolling update |
| Gateway timeouts | Capped at 100 s; websockets drop after 100 s idle and can't authenticate | Up to 1 h to first byte, no limit on streaming; websockets authenticate with `?ogm_token=` |
| Busy instances | — | `single_connection_limit` queues requests instead of rejecting them, and waits through cold starts |
| Sharing an endpoint | Org API key | Rotatable per-endpoint share token that can't touch the management API |
| Preemption | No warning before an instance is stopped | SIGTERM + IMDS `termination-notice` with a deadline (priority preemption, owner pause) |
| Custom images | Push to a third-party registry, configure credentials | `ogm push` into the cluster's own registry; multi-arch merge across x86 and ARM pushes |
| GPU sharing | One instance per GPU | Optional `gpu_shared` with VRAM budgets, billed by slice |
| Agent upgrades | — | In-place re-exec; running containers are re-attached, not restarted |
| Sharded batch | — | Stable `OGM_REPLICA_INDEX`; finished shards are never re-run |
| Fairness | Price | Fair-share scheduling by credit balance inside each priority tier |
| Owner controls | Salad app settings | Share only N GPUs, availability windows, pause when a game is running, auto-yield on GPU load, refuse priorities, local pause/resume |
| GPU selection | GPU class UUIDs | Readable slugs (`rtx-4090`), `min_vram_mb`, multi-GPU `gpu_count` |
| Observability | Log forwarding to third parties; portal | Built-in log store with live tail, per-instance GPU util/VRAM/temperature, event stream |
| Deploy UX | API/portal; separate create vs update | `ogm deploy -f app.yaml` declarative upsert, multi-document, `${ENV}` expansion |

## Where SaladCloud is still better

Some of these are on the ogm roadmap ([SPEC.md §6](../SPEC.md#6-milestones)).

- **Supply.** SaladCloud has thousands of nodes you can rent right now. ogm only has whatever your friends contribute.
- **Contributor onboarding.** SaladCloud installs with a Windows app in one click. ogm contributors need Docker and the NVIDIA toolkit, inside WSL2 on Windows. A tray app is planned for v0.3.
- **Image distribution.** SaladCloud pulls each image once, then compresses and caches it centrally. ogm nodes pull from the registry themselves; a pull-through mirror is planned for v0.2.
- **Isolation and attestation.** SaladCloud runs a hardened Linux VM with intrusion detection. ogm relies on Docker hardening, optional gVisor, and trust within the group.
- **Availability.** SaladCloud's control plane is managed and redundant. ogm is a single server (Postgres HA is planned for v0.2).
- **Extras.** SaladCloud also offers a web terminal/SSH (`ogm exec` is planned for v0.2), gRPC probes (use an exec probe with `grpc_health_probe` meanwhile), Kelpie long-running jobs with S3 sync (`task` jobs are planned for v0.2), log forwarders, and hosted inference/transcription APIs.

## Moving from SaladCloud

The core shapes match closely, so most specs translate field by field:

| SaladCloud | ogm |
|---|---|
| `container.resources.gpu_classes: [uuid]` | `gpu_classes: [rtx-4090]` or `min_vram_mb` |
| `storage_amount` (bytes) | `storage_gb` |
| `networking.protocol/port/auth` | `networking.port/auth` |
| `queue_connection.path/port/queue_name` | same |
| `queue_autoscaler.*` | same field names and formula |
| `Salad-Api-Key` | `OGM-Api-Key` (or Bearer) |
| IMDS `169.254.169.254:80` | `$OGM_IMDS_URL` (same endpoints + termination notice) |
| `SALAD_MACHINE_ID`, `SALAD_CONTAINER_GROUP_ID` | `OGM_MACHINE_ID`, `OGM_CONTAINER_GROUP_ID` |
| Webhook verification | Same Standard Webhooks scheme. Only the secret changes |

A SaladCloud-compatible API shim (`/api/public/organizations/...`), so that existing SDKs work unchanged, is planned for v0.2.
