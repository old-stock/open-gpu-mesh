# API

> **Full reference:** every hub serves an interactive API reference at **`https://YOUR-HUB/docs`** (Scalar), generated from the OpenAPI 3.1 document at **`/openapi.json`** (also `/openapi.yaml`, and a plain-text index for LLMs at `/llms.txt`). It documents every operation with parameters, request bodies, responses, examples and the equivalent `ogm` CLI command. The source is [`internal/apidocs/openapi.yaml`](../internal/apidocs/openapi.yaml); CI fails if any route is undocumented, so it always matches the server. [api-surface.md](api-surface.md) lists every route on one page. This file is the narrative overview.

Base URL: your server, e.g. `https://gpu.example.com`. Every endpoint below is what the CLI and dashboard use. The Go SDK lives in `internal/client`.

## Auth and errors

Send your key as `OGM-Api-Key: ogm_...` or `Authorization: Bearer ogm_...`.

Errors use [RFC 7807](https://www.rfc-editor.org/rfc/rfc7807) problem details:

```json
{ "title": "validation failed", "status": 400, "errors": { "container.image": ["required"] } }
```

## Endpoints

```
GET    /v1/me                 POST /v1/me/rotate-key       GET /v1/me/ledger
POST   /v1/invites {uses, role, ttl_hours}  (admin)        POST /v1/join {code, name} → user + api_key
GET    /v1/users              PATCH /v1/users/{name} {role, grant_credits} (admin)
GET    /v1/projects           POST /v1/projects {name}     DELETE /v1/projects/{p}
POST   /v1/projects/{p}/members {user}

GET    /v1/projects/{p}/containers                     POST (create)
GET    /v1/projects/{p}/containers/{g}                 PUT (upsert)  PATCH (merge patch)  DELETE
POST   /v1/projects/{p}/containers/{g}/start | stop | rotate-gateway-token
GET    /v1/projects/{p}/containers/{g}/instances[?all=1]
GET    /v1/projects/{p}/containers/{g}/instances/{id}  PATCH {deletion_cost}
POST   /v1/projects/{p}/containers/{g}/instances/{id}/reallocate | recreate | restart
GET    /v1/projects/{p}/containers/{g}/events?limit=
GET    /v1/projects/{p}/containers/{g}/logs?since=10m&instance=&q=&limit=&follow=1

GET    /v1/projects/{p}/queues                         POST {name, description, settings}
GET    /v1/projects/{p}/queues/{q}                     PATCH  DELETE[?force=1]
GET    /v1/projects/{p}/queues/{q}/jobs?status=&limit= POST (job or {"jobs":[...]})
GET    /v1/projects/{p}/queues/{q}/jobs/{id}[?wait=30s] DELETE (cancel)

GET    /v1/projects/{p}/secrets                        names + dates only (values are write-only)
PUT    /v1/projects/{p}/secrets/{name} {value}         DELETE /v1/projects/{p}/secrets/{name}
GET    /v1/projects/{p}/volumes                        [{group, replica, node_id, node_name, volumes, in_use}]
DELETE /v1/projects/{p}/volumes/{g}[?replica=N]        409 if in use or the node is offline

GET    /v1/projects/{p}/files?prefix=                  PUT | GET | DELETE /v1/projects/{p}/files/{path...}
POST   /v1/projects/{p}/file-tokens/{path...} {method: GET|PUT, expires_in_s} → {url, expires}

GET    /v1/nodes              POST /v1/nodes {name} → {token, install_command}
GET    /v1/nodes/{id}         PATCH {name, paused}  DELETE
GET    /v1/nodes/{id}/assessment                       latest onboarding audit (`ogm assess`)
POST   /v1/agent/assessment   (node token) store this node's audit
GET    /v1/speedtest/ping     GET /v1/speedtest/down?bytes=N   POST /v1/speedtest/up   (node token or API key)
GET    /v1/gpu-classes        PATCH /v1/gpu-classes/{id} {credits_per_hour} (admin)
GET    /v1/availability
GET    /v1/cluster            GET /v1/events
GET    /v1/webhook-secret     POST /v1/webhook-secret (rotate)
GET    /.well-known/jwks.json
GET    /install.sh            GET /dl/{os}-{arch}       GET /healthz
```

- **PUT** on a container group is declarative: it creates the group or replaces its spec.
- **PATCH** takes an [RFC 7386](https://www.rfc-editor.org/rfc/rfc7386) merge patch, e.g. `{"replicas": 5}` or `{"networking": {"single_connection_limit": true}}`. Set a field to `null` to remove it.
- **Logs with `follow=1`** return Server-Sent Events: one `data: {LogEntry}` per line, with historical lines first.
- **`?wait=30s` on a job** long-polls until the job finishes or the wait elapses.

## Registry

`/v2/` is an [OCI Distribution](https://github.com/opencontainers/distribution-spec) registry. Repositories are named `{project}/{name}`.
- **Auth:** an API key as `Authorization: Bearer`, or as the Basic-auth password (so `docker login` works). Node tokens are read-only.
- **Supported:** chunked and monolithic blob uploads, cross-repo mounts, range reads, manifests and indexes by tag or digest, `tags/list`, `_catalog`, and manifest deletion.
- **`OGM-Merge-Platforms: 1`** on a manifest PUT merges it into the tag's existing platforms. `ogm push --multiarch` uses this.

## Gateway

| Mode | URL |
|---|---|
| Path | `{server}/gw/{project}/{group}/...` (prefix stripped; `X-Forwarded-Prefix` set) |
| Host | `https://{group}-{project}.{gateway-domain}/...` (needs `--gateway-domain` + wildcard DNS) |

**Auth** (when `networking.auth` is on) accepts either:
- the API key of any member of the project, or
- the group's **share token** (`gateway_token` on the group, `ogmg_...`). The share token works only for this group's gateway, never for the management API. Rotate it with `POST .../rotate-gateway-token`.

It can be sent three ways:
- the `OGM-Api-Key` header
- `Authorization: Bearer`, so OpenAI-style SDKs work by setting `api_key`
- `?ogm_token=`, for browsers and websockets. The gateway strips the token before forwarding.

**Behaviour:**
- Websockets, SSE, and chunked streaming pass through.
- Responses carry `X-OGM-Instance-Id` and `X-OGM-Node-Id` headers.
- Requests wait for a ready instance during cold starts, up to `client_request_timeout_ms`.
- A request is retried once on a different instance if the connection to the backend fails, or if it is an idempotent request with no body.

| Status | Meaning |
|---|---|
| 401 | missing/invalid credentials |
| 502 | instance unreachable (after retry) |
| 503 | group has no instances that can become ready |
| 504 | no instance freed up within `client_request_timeout_ms` |
| 524 | app didn't send headers within `server_response_timeout_ms` |

## Queue job delivery (what your container sees)

```
POST http://127.0.0.1:{queue_connection.port}{queue_connection.path}
Content-Type: application/json
OGM-Job-Id: job_...
OGM-Job-Attempt: 2

<job input JSON>
```

| Your response | Result |
|---|---|
| 2xx + JSON | succeeded, `output` = your JSON |
| 2xx + non-JSON | succeeded, `output` = `{"text": "..."}` |
| 408, 429, 5xx, timeout, crash | attempt failed → retried with backoff on another instance if possible |
| other 4xx | failed immediately (unless queue `retry_on_4xx`) |

If the job is cancelled while it runs, the request context is cancelled and your server sees the client disconnect.

## Webhooks

Jobs with a `webhook` URL get a POST when they finish:

```json
{ "type": "queue.job.succeeded", "timestamp": "...", "data": { "id": "job_...", "status": "succeeded", "input": {}, "output": {}, "events": [], "attempts": 1, "queue_name": "sdxl", "project_name": "default" } }
```

Signatures follow [Standard Webhooks](https://www.standardwebhooks.com/), so any svix-compatible library verifies them. The headers are `webhook-id`, `webhook-timestamp`, and `webhook-signature: v1,<base64 HMAC-SHA256(id.timestamp.body)>`. The key comes from `GET /v1/webhook-secret?project=P` and is different for each project (default: `default`, or the project a scoped key is bound to). Failed deliveries are retried 5 times: after 5 s, 30 s, 2 min, 10 min, and 1 h.

```python
from standardwebhooks import Webhook
Webhook(secret).verify(body, headers)
```

## IMDS

Inside a container, `$OGM_IMDS_URL` points at the node agent. Every request needs the header `Metadata: true`.

| Request | Response |
|---|---|
| `GET /v1/status` | `{"ready": true, "started": true}` |
| `GET /v1/token[?audience=A]` | `{"jwt": "..."}`: EdDSA JWT (`sub` = instance id; `aud` = A, default `ogm-workload`; claims `project`, `container_group`, `node_id`; 1 h). Only `audience=ogm-hub` tokens are accepted by the hub API. Verify against `{server}/.well-known/jwks.json` to prove to your own backend that a request comes from your workload. |
| `GET /v1/deletion-cost` / `PUT {"deletion_cost": N}` | Instances with a higher deletion cost are removed last on scale-down. Raise it while busy. |
| `POST /v1/reallocate {"reason": "..."}` | Move me to a different node (this node is banned for the group for 30 min). |
| `POST /v1/restart`, `POST /v1/recreate` | restart the container / recreate it fresh |
| `GET /v1/termination-notice` | `{"pending": true, "deadline": "...", "reason": "node paused by owner"}`. Poll this, or handle SIGTERM, to checkpoint before preemption or an owner pause. |
| `GET /v1/metadata` | instance, group, node, GPU info |

```sh
curl -s -H 'Metadata: true' "$OGM_IMDS_URL/v1/termination-notice"
```
