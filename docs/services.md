# Cluster services

Besides containers, queues, files and the [S3 API](s3.md), every hub provides the services below. Each one is available through the REST API (full reference at `/docs` on your hub), the CLI, and the [MCP server](mcp.md).

| Service | What it replaces | CLI |
|---|---|---|
| [Scoped API keys](#scoped-api-keys) | IAM access keys | `ogm keys` |
| [Instance identity](#instance-identity) | IAM instance roles | — |
| [Key-value store](#key-value-store) | DynamoDB / Redis for small state | `ogm kv` |
| [Schedules](#schedules) | EventBridge Scheduler / cron | `ogm schedule` |
| [Volume snapshots](#volume-snapshots-and-migration) | EBS snapshots | `ogm snapshot` |
| [Metrics](#metrics) | CloudWatch metrics | `ogm metrics`, `/metrics` |
| [Custom domains and TLS](#custom-domains-and-tls) | Route 53 + ACM | spec field |
| [Project quotas](#project-quotas) | Service Quotas | `ogm quota` |
| [Templates](#templates) | Marketplace AMIs | `ogm template` |

## Scoped API keys

Your user key can do everything you can. For CI jobs, scripts and other people, create a key limited to one project and a role:

```sh
ogm keys create ci --project ml --role developer --expires 720h   # prints ogmk_... once
ogm keys                                                          # list (secrets never shown again)
ogm keys rm key_abc123
```

| Role | Can |
|---|---|
| `viewer` | read anything in the project (GET only) |
| `developer` | deploy, scale and delete groups; queues, jobs, files, KV, secrets, schedules, snapshots |
| `admin` | everything the creating user can do in the project |

A key bound to a project is rejected for every other project, for `/v1/keys`, and for cluster administration. No scoped key can rotate your personal key or manage users and invites, and only an `admin` key can delete a project or add members. A key can never have more rights than the user who created it. `last_used_time` shows whether a key is still in use.

## Instance identity

Every container can fetch a signed JWT from the metadata service. A token minted for the audience `ogm-hub` (`$OGM_IMDS_URL/v1/token?audience=ogm-hub`) is accepted by the hub as a Bearer credential for the container's own project, with a narrow scope:

- KV: read and write
- files: read and write
- queue jobs: create and read
- containers: read

So a workload can use cluster storage without any secret in its spec:

```sh
TOKEN=$(curl -s "$OGM_IMDS_URL/v1/token?audience=ogm-hub" | jq -r .jwt)
curl -X POST -H "Authorization: Bearer $TOKEN" "$OGM_SERVER_URL/v1/projects/$OGM_PROJECT/kv/counters/visits"
```

Tokens last one hour and are rejected after the instance stops. To prove the workload's identity to your own services, fetch a token without `audience` (it defaults to `ogm-workload`) or with your service's name, and verify it against `/.well-known/jwks.json`, checking `aud`. Only `ogm-hub` tokens work against the hub, so a token you hand to another service never grants hub access. Send `ogm-hub` tokens only to the hub.

A node operator can request tokens for the containers running on their machine and sees their decrypted secrets. Only schedule sensitive workloads on nodes you trust (`node_selector`/labels).

## Key-value store

This is a per-project store for small state: configuration, counters, locks, leader election and job bookkeeping. Values are opaque bytes, up to 1 MiB each, and keys may contain `/`.

```
GET    /v1/projects/{p}/kv                     tables with key counts and sizes
GET    /v1/projects/{p}/kv/{table}?prefix=&after=&limit=&values=1
GET    /v1/projects/{p}/kv/{table}/{key}       raw value; ETag and OGM-Version headers
PUT    /v1/projects/{p}/kv/{table}/{key}?ttl=SECONDS
POST   /v1/projects/{p}/kv/{table}/{key}?by=N  atomic increment, returns the new value
DELETE /v1/projects/{p}/kv/{table}/{key}
DELETE /v1/projects/{p}/kv/{table}             drop the whole table
```

Every write bumps the key's version, and you can make a write conditional on it:

| Header | Effect |
|---|---|
| `If-Match: "7"` | write or delete only if the current version is 7; otherwise 412 |
| `If-None-Match: *` | create only if the key does not exist; otherwise 412 (a lock) |

```sh
ogm kv put config model llama-3.1-8b
ogm kv put locks leader "$HOSTNAME" --ttl 30s --create-only    # take a lock that expires
ogm kv incr counters requests
ogm kv ls config
```

Expired keys disappear from reads at once, and the hub deletes them within a minute. For larger objects, use [files](api.md) or [S3](s3.md).

## Schedules

A schedule runs one of three actions on a 5-field cron expression, in the hub's time zone:

```yaml
kind: schedule
name: nightly-report
schedule: "0 3 * * *"
job:   { queue: reports, input: { day: yesterday } }   # enqueue a job
# http: { group: api, method: POST, path: /tasks/cleanup }   # call a group through the gateway
# scale: { group: workers, replicas: 4 }                     # set a group's replicas
```

```sh
ogm deploy -f schedule.yaml        # or: ogm schedule create NAME --cron "..." --queue Q
ogm schedule                       # next run, last run, last status, run count
ogm schedule run nightly-report    # fire now
ogm schedule rm nightly-report
```

Set `enabled: false` to pause a schedule. Runs missed while the hub was down are not backfilled; the next run is computed from the time the hub restarts. An `http` action wakes a group scaled to zero just like a real request does.

## Volume snapshots and migration

A snapshot is a compressed tar of all of one replica's [volumes](spec-reference.md#persistent-volumes), stored in the project's file store under `.snapshots/`:

```sh
ogm snapshot create pg                    # replica 0; --replica N for others
ogm snapshot ls
ogm snapshot restore snap_abc             # back onto the same replica
ogm snapshot restore snap_abc --node B    # onto another node: this moves the replica there
ogm snapshot rm snap_abc
```

Snapshots work while the group is running, but stop databases first if you need a crash-consistent copy. Restoring requires the replica to be stopped. Restoring to another node re-pins the replica, so its next start happens on the new node with the restored data. Use this to evacuate a machine whose owner is leaving. Snapshots are as large as the data they hold, so watch your file store quota.

## Metrics

The hub keeps per-minute metrics for every group for 24 hours: requests, 4xx/5xx errors, p50/p95/p99 gateway latency, ready and desired replicas, and average CPU, memory and GPU use.

```sh
ogm metrics api --minutes 60
curl -H "Authorization: Bearer $KEY" $HUB/v1/projects/default/containers/api/metrics?minutes=60
```

`GET /metrics` serves the same data, plus node and queue gauges, in Prometheus text format. It requires an admin key that is not bound to a project.

## Custom domains and TLS

Point any hostname at your hub and route it to a group:

```yaml
networking:
  port: 8080
  custom_domains: [app.example.com]
```

A domain can belong to only one group, and it must not be inside the hub's own gateway domain. Start the hub with `--tls --acme-email you@example.com` to get Let's Encrypt certificates automatically, both for the hub's domain and for every custom domain that a group claims. This needs ports 80 and 443 reachable from the internet.

## Project quotas

A cluster admin can cap what each project may claim from the shared fleet:

```sh
ogm quota set --project ml --cpu 32 --memory-mb 131072 --gpus 4 --instances 20 --storage-mb 512000
ogm quota --project ml        # limits and current usage
```

Compute usage counts every group at its **peak**: the largest of `replicas`, the autoscaler's `max_replicas`, and any scheduled replica count. An autoscaler can therefore never push a project over its quota at 3 a.m. The following are rejected with `403 quota exceeded`:

- creating a group, updating it, or scaling it with `PATCH` past a limit
- a schedule's `scale` action (the run's `last_status` shows the reason)
- uploads to files or S3 past `storage_mb` (overwriting a file counts only the difference)

A limit of 0 means unlimited. Groups that were already running before a quota was lowered keep running, but they cannot grow.

The quota set for project `*` is the default for every project that has no quota of its own, including projects created later:

```sh
ogm quota set --project '*' --cpu 8 --memory-mb 16384 --gpus 1 --instances 4 --storage-mb 20480
```

Any member can create projects, so on a shared hub set a default quota, or each new project starts unlimited.

## Security notes

- **Uploaded files and S3 objects** are served with `Content-Security-Policy: sandbox` and `X-Content-Type-Options: nosniff`. HTML, SVG and other types a browser could render as a page are sent as downloads, so an upload can never run script on the hub's origin. Signed file URLs last at most 7 days.
- **Use a gateway domain for untrusted workloads.** Without `--gateway-domain`, groups are served under `/gw/{project}/{group}/` on the hub's own origin, where a public group's pages share an origin with the dashboard. With a gateway domain, each group gets its own `{group}-{project}.<domain>` origin.
- **Links** accept connections only from the container they belong to. Containers on one node otherwise share Docker's default bridge network, so put workloads that must not see each other on different nodes.
- **Node operators** can see what runs on their machines (see [Instance identity](#instance-identity)).
- **Webhook signing keys** are per project: `GET /v1/webhook-secret?project=P`.

## Templates

Templates are ready-made, tested services. Any missing secrets they reference are generated:

```sh
ogm template                      # postgres, redis, s3-seaweedfs, stack-postgres-adminer
ogm template show postgres
ogm template deploy postgres --name db
```

Other groups connect with `{{link:db:5432}}` and `{{secret:...}}`, as the template's comments show.
