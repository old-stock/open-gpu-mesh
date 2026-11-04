# MCP server

ogm includes a [Model Context Protocol](https://modelcontextprotocol.io) server. With it, an AI assistant such as Claude Code, Claude Desktop or Cursor can run your cluster for you: check capacity, deploy and scale container groups, read logs, submit jobs and manage secrets. Every tool call is an ordinary API request made with **your** API key, so the assistant has exactly your permissions.

There are two ways to connect:

| Transport | How | Use it when |
|---|---|---|
| **stdio** | `ogm mcp` runs locally and uses your saved CLI login | you have the `ogm` binary on your machine |
| **Streamable HTTP** | `POST https://HUB/v1/mcp` with your API key | remote or hosted clients, or nothing to install |

The server speaks MCP `2025-06-18`. It also accepts `2025-03-26` and `2024-11-05`.

## Claude Code

```sh
ogm login https://gpu.example.com --key ogm_...    # once
claude mcp add ogm -- ogm mcp
```

To pin a project, use `claude mcp add ogm -- ogm mcp --project team`. Without it, tools use the CLI's current project.

To skip the local binary and connect straight to the hub:

```sh
claude mcp add --transport http ogm https://gpu.example.com/v1/mcp --header "Authorization: Bearer ogm_..."
```

## Claude Desktop, Cursor and other clients (stdio)

Add this to `claude_desktop_config.json`, `.cursor/mcp.json` or your client's equivalent:

```json
{
  "mcpServers": {
    "ogm": {
      "command": "ogm",
      "args": ["mcp"],
      "env": { "OGM_SERVER": "https://gpu.example.com", "OGM_API_KEY": "ogm_...", "OGM_PROJECT": "default" }
    }
  }
}
```

You can leave out `env` if you have already run `ogm login`. If the client can't find `ogm` on its `PATH`, give the full path to the binary.

## Remote (HTTP)

```json
{
  "mcpServers": {
    "ogm": {
      "type": "http",
      "url": "https://gpu.example.com/v1/mcp",
      "headers": { "Authorization": "Bearer ogm_..." }
    }
  }
}
```

- **Auth:** the normal API key, sent as `Authorization: Bearer` or `OGM-Api-Key`. Requests without a key get `401`.
- **Project:** add `?project=NAME` to the URL to change the default project. Each project-scoped tool also takes a `project` argument.
- **Sessions:** `initialize` returns an `Mcp-Session-Id` header. The session belongs to the user who created it. An unknown or expired session gets `404`, and the client should send `initialize` again. `DELETE /v1/mcp` ends a session.
- **Responses** are plain `application/json`, and JSON-RPC batches are accepted. `GET /v1/mcp` returns `405` because the server never opens a stream of its own.

## Tools

All results are the API's JSON, pretty-printed. Output over about 100 KB is cut off, with a note asking for a narrower query. API errors come back as tool errors (`isError: true`) that carry the [problem details](api.md#auth-and-errors), so the assistant can see what went wrong.

| Tool | What it does | Notes |
|---|---|---|
| `cluster_status` | Cluster summary, plus the caller and the default project | read-only |
| `whoami` | Current user, role and credits | read-only |
| `credits` | Balance and recent ledger entries | read-only |
| `list_projects` | Projects you can access | read-only |
| `list_groups` | Container groups in a project | read-only |
| `get_group` | One group: spec, state, gateway URL | read-only |
| `deploy_group` | Create or replace a group from a full spec (`PUT`) | idempotent |
| `scale_group` | Set `replicas` | |
| `start_group` / `stop_group` | Start or stop a group | |
| `delete_group` | Delete a group | **destructive** |
| `list_instances` | A group's instances (`include_terminated`) | read-only |
| `group_logs` | Logs (`since`, `query`, `instance`, `limit`) | read-only |
| `group_events` | Lifecycle events | read-only |
| `restart_instance` | `restart`, `recreate` or `reallocate` one instance | |
| `list_queues` / `create_queue` | Job queues | |
| `submit_job` | Submit a job. `wait` (≤ 60 s) returns the finished job. | |
| `get_job` | Job status and output. `wait` long-polls. | read-only |
| `list_jobs` | Jobs in a queue (`status`, `limit`) | read-only |
| `cancel_job` | Cancel a job | **destructive** |
| `list_nodes` | Contributor machines and their GPUs | read-only |
| `node_assessment` | A node's onboarding audit | read-only |
| `list_gpu_classes` | GPU classes, prices and availability | read-only |
| `list_secrets` | Secret names only. Values are write-only. | read-only |
| `set_secret` | Set a secret. The value is never echoed. | |
| `delete_secret` | Delete a secret | **destructive** |
| `list_volumes` / `delete_volume` | Persistent volumes | delete is **destructive** |
| `list_files` | Files in the project object store | read-only |
| `get_quota` / `set_quota` | Project limits and peak usage; setting needs a cluster admin | |
| `group_metrics` | Per-minute requests, 4xx/5xx and p50/p95/p99 latency (`minutes`) | read-only |
| `list_templates` / `get_template` | Built-in example deploy files (YAML). The assistant converts one to a spec and calls `deploy_group`. | read-only |
| `list_keys` | Your scoped API keys (no secrets) | read-only |
| `create_key` | Create a scoped key (`developer`, `viewer` or `admin`; optional project and expiry). The secret appears only in this result. | |
| `revoke_key` | Revoke a key | **destructive** |
| `kv_list_tables` / `kv_list` | KV tables, and keys (`prefix`, `after`, `limit`, `values`) | read-only |
| `kv_get` | Value plus `version` and expiry | read-only |
| `kv_put` | Write a value (`ttl_seconds`, `if_version` compare-and-set, `create_only`) | |
| `kv_delete` | Delete a key (optionally only at `if_version`), or a whole table with `delete_table` | **destructive** |
| `kv_incr` | Atomic counter | |
| `list_schedules` | Cron schedules and their last run | read-only |
| `put_schedule` | Create or replace a schedule with one action: `job`, `http` or `scale` | idempotent |
| `run_schedule` | Fire a schedule now | |
| `delete_schedule` | Delete a schedule | **destructive** |
| `create_snapshot` | Snapshot a replica's volumes (async; poll `get_snapshot`) | |
| `list_snapshots` / `get_snapshot` | Snapshots and their status | read-only |
| `restore_snapshot` | Restore into a group replica, overwriting its volumes | **destructive** |
| `delete_snapshot` | Delete a snapshot | **destructive** |
| `list_s3_credentials` | S3 access keys (no secrets) | read-only |
| `create_s3_credentials` | New S3 key pair. The secret appears only in this result. | |
| `delete_s3_credentials` | Delete an S3 key | **destructive** |
| `api_operations` | Every REST operation (from `/openapi.json` if the hub serves it, else a built-in table) | read-only |
| `api_get` | `GET` any `/v1/...` path, for things without a dedicated tool | read-only |

Each tool carries the MCP annotations `readOnlyHint`, `destructiveHint` and `idempotentHint`. Clients use them to decide when to ask before running a tool.

## Resources

| URI | Content |
|---|---|
| `ogm://docs/spec-reference` | [Spec reference](spec-reference.md): every field `deploy_group` accepts |
| `ogm://docs/api` | [API reference](api.md) |
| `ogm://docs/quickstart` | [Quickstart](quickstart.md) |

## Troubleshooting

- **"not logged in"** from `ogm mcp`: run `ogm login`, or set `OGM_SERVER` and `OGM_API_KEY` in the client's `env`.
- **Debug logs:** set `OGM_MCP_DEBUG=1`. Logs go to stderr only, because stdout carries the protocol.
- **Secrets shown once:** `create_key` and `create_s3_credentials` return their secret in the tool result, because handing it to you is the point of the call. Those secrets stay in the conversation transcript.
- **Following logs live** (`follow=1`) isn't supported over MCP. Call `group_logs` again with `since`.
