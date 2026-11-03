# Architecture

```
            ┌──────────────────────── ogm server (one box) ─────────────────────────┐
 users ───► │ REST API /v1   Dashboard /   Gateway (/gw/p/g/ or g-p.domain)         │
  CLI/SDK   │   │               │             │  reverse proxy, LB, auth, queueing │
            │   ▼               ▼             ▼                                    │
            │ Store (SQLite) ◄─ Reconciler/Scheduler ─ Autoscaler ─ Job dispatcher │
            │   ▲  logs, events, credits ledger, files, webhooks                   │
            │   └──── Tunnel hub (WebSocket + yamux, one session per node) ────────┤
            └──────────────────────────────▲───────────────────────────────────────┘
                                           │ outbound WSS from each node
          ┌──────────────── ogm agent (friend's PC) ────────────────┐
          │ control stream: hello/heartbeat/instance.*/job.*/logs    │
          │ runtime: docker (+ NVIDIA toolkit) | process (dev only)  │
          │ probes · IMDS server · job executor · log shipper · stats│
          └──────────────────────────────────────────────────────────┘
```

One Go binary (`ogm`) contains the server, agent, CLI, dev mode, and a demo app.

## Code map

| Package | Responsibility |
|---|---|
| `internal/api` | Public types (the JSON contract), validation, defaults |
| `internal/proto` | Agent ⇄ server control messages (newline-delimited JSON) |
| `internal/tunnel` | WebSocket → `net.Conn` → yamux; proxy stream header + splice |
| `internal/store` | SQLite schema and row models (WAL, single connection) |
| `internal/server` | HTTP API, hub, reconciler/scheduler, gateway, queues, autoscaler, credits, logs, IMDS backend |
| `internal/agent` | Agent session, Docker and process runtimes, instance lifecycle and probes, IMDS HTTP server, job executor, pause logic |
| `internal/webhook` | Standard Webhooks signing; EdDSA JWT |
| `internal/client` | Go SDK |
| `internal/cli` | Command-line interface, including `ogm dev` |
| `internal/web` | Embedded single-page dashboard (vanilla JS) |
| `internal/e2e` | End-to-end tests: real server and agents in-process, the demo app as workloads |

## Tunnel

1. The agent connects to `GET /v1/agent/connect` (WebSocket, `Authorization: Bearer ogmn_...`). The connection is wrapped in a **yamux client**.
2. The agent's first stream is the **control stream**:
   - agent → server: `hello` (node info, GPUs, running instances), `heartbeat` (every 5 s, with per-GPU and per-instance stats), `instance.status`, `job.result`, `logs`, `imds.request`
   - server → agent: `welcome`, `instance.start|stop|restart|recreate`, `job.assign|cancel`, `node.config`, and replies to requests
3. For gateway traffic the **server opens a stream** and writes `{"kind":"proxy","instance_id":"…","port":8080}\n`. The agent dials the container port on 127.0.0.1 and answers with one ack byte. After that both sides splice raw bytes. HTTP keep-alive, websockets, and SSE are just bytes on the stream.
4. **Reconnects:**
   - When an agent reconnects, its `hello` lists the instances it is still running. The server reconciles that list: it re-sends a start for any instance the agent lost and stops any instance the server no longer knows.
   - Instances on a disconnected node have a 20 s grace period before they are declared interrupted and replaced.
   - Jobs that were running on that node are requeued **immediately**.

The gateway's HTTP transport dials through this tunnel. Its connection pool is keyed per instance and port (`inst_x.p8080.ogm`), so keep-alive connections are reused across requests.

## Scheduling loop

The reconciler runs every second, and also whenever a "poke" fires: a node joins, an instance changes state, or a spec changes.

1. **Desired replicas:** taken from the queue autoscaler if configured, otherwise the latest scheduled action, otherwise `replicas`. Run-to-completion groups subtract instances that have already completed.
2. **Too few instances:** create `allocating` instances, each with the lowest free `replica_index`.
3. **Too many instances:** stop victims in this order: lowest deletion cost, then not ready, then not yet running, then newest. Stops are graceful.
4. **Version drift:** replace old-version instances gradually, at most `max(1, 25%)` at a time.
5. **Placement**, ordered by (priority, owner's credit balance, age):
   - **Filters:** node online, not paused, inside its availability window, priority allowed by the owner, country, labels, not banned for this group (after a reallocate), enough free GPU (class, `min_vram_mb`, count), CPU, memory, and disk.
   - **Score:** image already cached on the node, node uptime ratio, spread across nodes, smallest GPU that fits (keeps the big GPUs free for big jobs).
   - **No fit:** if the instance outranks something running on a node that would fit, preempt the lowest-priority, lowest-cost instance there (with a termination notice). Otherwise record why each node was rejected in the instance message.
6. **Timeouts:** an instance stuck downloading or creating for 45 minutes is reallocated elsewhere.

## Job dispatch

Every 500 ms, and on every poke, the dispatcher:
1. collects ready instances in groups attached to each queue that have free `concurrency` slots;
2. picks pending jobs with `next_attempt_at ≤ now` in FIFO order;
3. prefers an instance different from the job's last failed one;
4. sends `job.assign` to that instance's agent.

The agent POSTs the job input to the container and reports `job.result`. Attempt numbers make results idempotent: a late result from an attempt that was already requeued is ignored.

## Credits

Every 60 s, each running instance accrues `rate × elapsed`:
- **GPU instances:** `rate = GPU class credits/hour × gpu_count × priority multiplier`.
- **CPU-only instances:** 0.05 per vCPU-hour.

The ledger records one debit from the group owner and one matching credit to the node owner, so total credits stay constant. Running work on your own node is free. Balances feed the placement order within a priority tier.

## Storage

All state lives in SQLite (`{data}/ogm.db`, WAL mode). Files are stored under `{data}/files/`. The server is a single process. Losing it pauses scheduling and gateway traffic, but agents keep running their containers and re-sync when the server comes back. Back up by copying the data directory (or `sqlite3 ogm.db .backup`).

## Security

- **Containers:** no privileges, `no-new-privileges`, risky capabilities dropped (`NET_RAW`, `SYS_ADMIN`, `SYS_MODULE`, `SYS_PTRACE`, `MKNOD`), PID limit, CPU/memory limits, ports published on 127.0.0.1 only (reachable just through the tunnel), no host mounts, optional `runsc` (gVisor).
- **Credentials:** API keys and node tokens are stored as SHA-256 hashes. Per-instance IMDS URLs contain a 128-bit secret. JWTs are EdDSA. Webhooks are HMAC-signed. Signed file URLs use HMAC with an expiry.
- **Trust:** the node owner has root on their machine and can read container memory and environment. The design assumes a group of people who trust each other.
