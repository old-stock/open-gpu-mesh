<!-- Code generated from internal/apidocs/openapi.yaml by `go generate ./internal/server`. DO NOT EDIT. -->

# API surface

Every operation of the hub's HTTP API (134 operations). The full reference, with parameters, bodies and examples, is served by every hub at `/docs` (Scalar) and `/openapi.json`; see also [api.md](api.md). Protocol endpoints (gateway, OCI registry, S3) are marked *protocol*. CI fails when this file is stale or a route is undocumented.

## Account & users

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/me` | `getMe` | Get the current user | `ogm status` |
| `POST` | `/v1/me/rotate-key` | `rotateMyKey` | Rotate your personal API key |  |
| `GET` | `/v1/me/ledger` | `getMyLedger` | List your credit transactions | `ogm credits` |
| `POST` | `/v1/invites` | `createInvite` | Create an invite code | `ogm invite [--uses N] [--admin]` |
| `POST` | `/v1/join` | `joinCluster` | Join the cluster with an invite *(public)* | `ogm join SERVER INVITE_CODE --name NAME` |
| `GET` | `/v1/users` | `listUsers` | List users (leaderboard) | `ogm users` |
| `PATCH` | `/v1/users/{name}` | `updateUser` | Change a user's role or grant credits |  |

## Projects

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects` | `listProjects` | List projects |  |
| `POST` | `/v1/projects` | `createProject` | Create a project |  |
| `DELETE` | `/v1/projects/{project}` | `deleteProject` | Delete a project |  |
| `POST` | `/v1/projects/{project}/members` | `addProjectMember` | Add a member to a project |  |
| `GET` | `/v1/projects/{project}/quota` | `getProjectQuota` | Get a project's quota and usage | `ogm quota show` |
| `PUT` | `/v1/projects/{project}/quota` | `setProjectQuota` | Set a project's quota | `ogm quota set --cpu N --memory-mb N --gpus N --instances N --storage-mb N` |

## Container groups

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/containers` | `listContainerGroups` | List container groups | `ogm ls` |
| `POST` | `/v1/projects/{project}/containers` | `createContainerGroup` | Create a container group |  |
| `GET` | `/v1/projects/{project}/containers/{group}` | `getContainerGroup` | Get a container group | `ogm get GROUP` |
| `PUT` | `/v1/projects/{project}/containers/{group}` | `putContainerGroup` | Create or replace a container group | `ogm deploy -f app.yaml` |
| `PATCH` | `/v1/projects/{project}/containers/{group}` | `patchContainerGroup` | Update a container group (merge patch) | `ogm scale GROUP N` |
| `DELETE` | `/v1/projects/{project}/containers/{group}` | `deleteContainerGroup` | Delete a container group | `ogm rm GROUP` |
| `POST` | `/v1/projects/{project}/containers/{group}/rotate-gateway-token` | `rotateGatewayToken` | Rotate the gateway share token | `ogm url GROUP --rotate` |
| `POST` | `/v1/projects/{project}/containers/{group}/start` | `startContainerGroup` | Start a container group | `ogm start GROUP` |
| `POST` | `/v1/projects/{project}/containers/{group}/stop` | `stopContainerGroup` | Stop a container group | `ogm stop GROUP` |

## Instances

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/containers/{group}/instances` | `listInstances` | List a group's instances | `ogm ps GROUP` |
| `GET` | `/v1/projects/{project}/containers/{group}/instances/{instance}` | `getInstance` | Get an instance |  |
| `PATCH` | `/v1/projects/{project}/containers/{group}/instances/{instance}` | `updateInstance` | Set an instance's deletion cost |  |
| `POST` | `/v1/projects/{project}/containers/{group}/instances/{instance}/{action}` | `instanceAction` | Restart, recreate or reallocate an instance | `ogm restart\|recreate\|reallocate GROUP INSTANCE` |

## Logs & events

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/containers/{group}/events` | `listGroupEvents` | List a group's events | `ogm events GROUP` |
| `GET` | `/v1/projects/{project}/containers/{group}/logs` | `getGroupLogs` | Read or follow a group's logs | `ogm logs GROUP [-f] [--instance ID] [--since 10m] [-q TEXT]` |
| `GET` | `/v1/projects/{project}/events` | `listProjectEvents` | List a project's events |  |
| `GET` | `/v1/events` | `listClusterEvents` | List all cluster events |  |

## Gateway

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/gw/{project}/{group}/{path}` | `gatewayGet` | Call a group through the gateway *(protocol)* | `ogm curl GROUP [PATH] [curl args...]` |
| `POST` | `/gw/{project}/{group}/{path}` | `gatewayPost` | Call a group through the gateway (POST) *(protocol)* |  |

## Queues & jobs

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/queues` | `listQueues` | List queues | `ogm queue ls` |
| `POST` | `/v1/projects/{project}/queues` | `createQueue` | Create a queue | `ogm queue create NAME [--max-attempts N] [--timeout S]` |
| `GET` | `/v1/projects/{project}/queues/{queue}` | `getQueue` | Get a queue | `ogm queue get NAME` |
| `PATCH` | `/v1/projects/{project}/queues/{queue}` | `updateQueue` | Update a queue |  |
| `DELETE` | `/v1/projects/{project}/queues/{queue}` | `deleteQueue` | Delete a queue | `ogm queue rm NAME [--force]` |
| `GET` | `/v1/projects/{project}/queues/{queue}/jobs` | `listJobs` | List jobs | `ogm job ls QUEUE [--status S]` |
| `POST` | `/v1/projects/{project}/queues/{queue}/jobs` | `submitJobs` | Submit a job (or a batch) | `ogm job submit QUEUE [--input JSON \| --file F \| --batch F.jsonl] [--wait]` |
| `GET` | `/v1/projects/{project}/queues/{queue}/jobs/{job}` | `getJob` | Get a job (optionally wait for it) | `ogm job get QUEUE ID [--wait]` |
| `DELETE` | `/v1/projects/{project}/queues/{queue}/jobs/{job}` | `cancelJob` | Cancel a job | `ogm job cancel QUEUE ID` |
| `GET` | `/v1/webhook-secret` | `getWebhookSecret` | Get the webhook signing key |  |
| `POST` | `/v1/webhook-secret` | `rotateWebhookSecret` | Rotate the webhook signing key |  |

## Schedules

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/schedules` | `listSchedules` | List schedules | `ogm schedule ls` |
| `POST` | `/v1/projects/{project}/schedules` | `createSchedule` | Create a schedule |  |
| `PUT` | `/v1/projects/{project}/schedules/{name}` | `putSchedule` | Create or replace a schedule | `ogm schedule create NAME --cron "*/5 * * * *" (--queue Q \| --http GROUP \| --scale GROUP --replicas N)` |
| `GET` | `/v1/projects/{project}/schedules/{name}` | `getSchedule` | Get a schedule | `ogm schedule get NAME` |
| `DELETE` | `/v1/projects/{project}/schedules/{name}` | `deleteSchedule` | Delete a schedule | `ogm schedule rm NAME` |
| `POST` | `/v1/projects/{project}/schedules/{name}/run` | `runSchedule` | Run a schedule now | `ogm schedule run NAME` |

## Secrets

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/secrets` | `listSecrets` | List secrets | `ogm secret ls` |
| `PUT` | `/v1/projects/{project}/secrets/{name}` | `putSecret` | Set a secret | `ogm secret set NAME [VALUE \| --from-file F]` |
| `DELETE` | `/v1/projects/{project}/secrets/{name}` | `deleteSecret` | Delete a secret | `ogm secret rm NAME` |

## Volumes & snapshots

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/volumes` | `listVolumes` | List persistent volumes | `ogm volumes` |
| `DELETE` | `/v1/projects/{project}/volumes/{group}` | `deleteVolumes` | Delete a group's volumes | `ogm volumes rm GROUP [--replica N]` |
| `POST` | `/v1/projects/{project}/volumes/{group}/snapshots` | `createSnapshot` | Snapshot a replica's volumes | `ogm snapshot create GROUP [--replica N]` |
| `GET` | `/v1/projects/{project}/snapshots` | `listSnapshots` | List snapshots | `ogm snapshot ls` |
| `GET` | `/v1/projects/{project}/snapshots/{id}` | `getSnapshot` | Get a snapshot |  |
| `DELETE` | `/v1/projects/{project}/snapshots/{id}` | `deleteSnapshot` | Delete a snapshot | `ogm snapshot rm ID` |
| `POST` | `/v1/projects/{project}/snapshots/{id}/restore` | `restoreSnapshot` | Restore a snapshot | `ogm snapshot restore ID [--node NODE]` |

## KV store

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/kv` | `listKVTables` | List KV tables | `ogm kv tables` |
| `GET` | `/v1/projects/{project}/kv/{table}` | `listKVKeys` | List keys in a table | `ogm kv ls TABLE` |
| `DELETE` | `/v1/projects/{project}/kv/{table}` | `dropKVTable` | Delete a whole table | `ogm kv drop TABLE` |
| `GET` | `/v1/projects/{project}/kv/{table}/{key}` | `getKVValue` | Get a value | `ogm kv get TABLE KEY` |
| `PUT` | `/v1/projects/{project}/kv/{table}/{key}` | `putKVValue` | Put a value | `ogm kv put TABLE KEY VALUE [--ttl 10m] [--if-version N]` |
| `POST` | `/v1/projects/{project}/kv/{table}/{key}` | `incrementKVCounter` | Atomically increment a counter | `ogm kv incr TABLE KEY` |
| `DELETE` | `/v1/projects/{project}/kv/{table}/{key}` | `deleteKVKey` | Delete a key | `ogm kv rm TABLE KEY` |

## Files

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/files` | `listFiles` | List files | `ogm files ls [PREFIX]` |
| `PUT` | `/v1/projects/{project}/files/{path}` | `uploadFile` | Upload a file | `ogm files put LOCAL [REMOTE]` |
| `GET` | `/v1/projects/{project}/files/{path}` | `downloadFile` | Download a file | `ogm files get REMOTE [LOCAL]` |
| `DELETE` | `/v1/projects/{project}/files/{path}` | `deleteFile` | Delete a file | `ogm files rm REMOTE` |
| `POST` | `/v1/projects/{project}/file-tokens/{path}` | `createFileToken` | Create a signed file URL | `ogm files sign REMOTE [--put]` |
| `GET` | `/v1/signed/{project}/{path}` | `getSignedFile` | Download a file with a signed URL *(public)* |  |
| `PUT` | `/v1/signed/{project}/{path}` | `putSignedFile` | Upload a file with a signed URL *(public)* |  |

## S3 credentials

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `POST` | `/v1/projects/{project}/s3-credentials` | `createS3Credential` | Create an S3 access key | `ogm s3 credentials create [--save]` |
| `GET` | `/v1/projects/{project}/s3-credentials` | `listS3Credentials` | List S3 access keys | `ogm s3 credentials ls` |
| `DELETE` | `/v1/projects/{project}/s3-credentials/{access_key}` | `deleteS3Credential` | Revoke an S3 access key | `ogm s3 credentials rm ACCESS_KEY` |

## S3 API

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/s3/` | `s3ListBuckets` | ListBuckets *(protocol)* | `ogm s3 ls` |
| `PUT` | `/s3/{bucket}` | `s3CreateBucket` | CreateBucket *(protocol)* | `ogm s3 mb BUCKET` |
| `HEAD` | `/s3/{bucket}` | `s3HeadBucket` | HeadBucket *(protocol)* |  |
| `GET` | `/s3/{bucket}` | `s3ListObjects` | ListObjectsV2 / ListObjects and bucket subresources *(protocol)* | `ogm s3 ls BUCKET[/PREFIX] [-r]` |
| `POST` | `/s3/{bucket}` | `s3DeleteObjects` | DeleteObjects *(protocol)* |  |
| `DELETE` | `/s3/{bucket}` | `s3DeleteBucket` | DeleteBucket *(protocol)* | `ogm s3 rb BUCKET` |
| `PUT` | `/s3/{bucket}/{key}` | `s3PutObject` | PutObject / CopyObject / UploadPart *(protocol)* | `ogm s3 cp LOCAL s3://BUCKET/KEY` |
| `GET` | `/s3/{bucket}/{key}` | `s3GetObject` | GetObject / ListParts *(protocol)* | `ogm s3 cp s3://BUCKET/KEY LOCAL` |
| `HEAD` | `/s3/{bucket}/{key}` | `s3HeadObject` | HeadObject *(protocol)* |  |
| `DELETE` | `/s3/{bucket}/{key}` | `s3DeleteObject` | DeleteObject / AbortMultipartUpload *(protocol)* | `ogm s3 rm s3://BUCKET/KEY` |
| `POST` | `/s3/{bucket}/{key}` | `s3MultipartUpload` | CreateMultipartUpload / CompleteMultipartUpload *(protocol)* |  |

## API keys

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `POST` | `/v1/keys` | `createAPIKey` | Create a scoped API key | `ogm keys create NAME [--project P] [--role developer\|viewer\|admin] [--expires 720h]` |
| `GET` | `/v1/keys` | `listAPIKeys` | List your API keys | `ogm keys ls` |
| `DELETE` | `/v1/keys/{id}` | `deleteAPIKey` | Revoke an API key | `ogm keys rm ID` |

## Nodes & assessment

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/nodes` | `listNodes` | List nodes | `ogm nodes` |
| `POST` | `/v1/nodes` | `createNode` | Create a node join token | `ogm node add NAME` |
| `GET` | `/v1/nodes/{node}` | `getNode` | Get a node |  |
| `PATCH` | `/v1/nodes/{node}` | `updateNode` | Rename, pause or resume a node | `ogm node pause\|resume NODE_ID` |
| `DELETE` | `/v1/nodes/{node}` | `deleteNode` | Remove a node | `ogm node rm NODE_ID` |
| `GET` | `/v1/nodes/{node}/assessment` | `getNodeAssessment` | Get a node's assessment | `ogm node assessment NODE` |
| `GET` | `/v1/speedtest/ping` | `speedtestPing` | Speed test - latency probe | `ogm assess` |
| `GET` | `/v1/speedtest/down` | `speedtestDownload` | Speed test - download |  |
| `POST` | `/v1/speedtest/up` | `speedtestUpload` | Speed test - upload |  |

## GPUs & availability

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/gpu-classes` | `listGPUClasses` | List GPU classes | `ogm gpus` |
| `PATCH` | `/v1/gpu-classes/{id}` | `updateGPUClass` | Set a GPU class's price |  |
| `GET` | `/v1/availability` | `getAvailability` | Get GPU availability |  |

## Metrics

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/projects/{project}/containers/{group}/metrics` | `getGroupMetrics` | Get a group's metrics | `ogm metrics GROUP [--minutes 60]` |
| `GET` | `/metrics` | `getPrometheusMetrics` | Prometheus metrics |  |

## Registry

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v2/` | `registryPing` | Registry API version check *(protocol)* |  |
| `GET` | `/v2/_catalog` | `registryCatalog` | List repositories *(protocol)* | `ogm images` |
| `GET` | `/v2/{project}/{name}/tags/list` | `registryListTags` | List tags *(protocol)* |  |
| `GET` | `/v2/{project}/{name}/manifests/{reference}` | `registryGetManifest` | Get a manifest *(protocol)* |  |
| `HEAD` | `/v2/{project}/{name}/manifests/{reference}` | `registryHeadManifest` | Check a manifest *(protocol)* |  |
| `PUT` | `/v2/{project}/{name}/manifests/{reference}` | `registryPutManifest` | Push a manifest *(protocol)* | `ogm push [--multiarch] LOCAL_IMAGE[:TAG] [NAME[:TAG]]` |
| `DELETE` | `/v2/{project}/{name}/manifests/{reference}` | `registryDeleteManifest` | Delete a manifest or tag *(protocol)* |  |
| `GET` | `/v2/{project}/{name}/blobs/{digest}` | `registryGetBlob` | Download a blob *(protocol)* |  |
| `HEAD` | `/v2/{project}/{name}/blobs/{digest}` | `registryHeadBlob` | Check a blob *(protocol)* |  |
| `POST` | `/v2/{project}/{name}/blobs/uploads/` | `registryStartUpload` | Start a blob upload *(protocol)* |  |
| `PATCH` | `/v2/{project}/{name}/blobs/uploads/{uuid}` | `registryUploadChunk` | Upload a blob chunk *(protocol)* |  |
| `PUT` | `/v2/{project}/{name}/blobs/uploads/{uuid}` | `registryCompleteUpload` | Complete a blob upload *(protocol)* |  |
| `GET` | `/v2/{project}/{name}/blobs/uploads/{uuid}` | `registryUploadStatus` | Get upload progress *(protocol)* |  |
| `DELETE` | `/v2/{project}/{name}/blobs/uploads/{uuid}` | `registryCancelUpload` | Cancel a blob upload *(protocol)* |  |

## MCP

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `POST` | `/v1/mcp` | `mcpRequest` | MCP JSON-RPC request | `ogm mcp` |
| `DELETE` | `/v1/mcp` | `mcpEndSession` | End an MCP session |  |
| `GET` | `/v1/mcp` | `mcpOpenStream` | MCP server-sent stream (not supported) *(public)* |  |

## Agent (internal)

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/agent/connect` | `agentConnect` | Agent control tunnel (WebSocket) |  |
| `POST` | `/v1/agent/assessment` | `submitAssessment` | Submit a node assessment | `ogm assess --token NODE_TOKEN` |

## Misc

| Method | Path | Operation | Summary | CLI |
|---|---|---|---|---|
| `GET` | `/v1/cluster` | `getCluster` | Get the cluster summary *(public)* | `ogm status` |
| `GET` | `/.well-known/jwks.json` | `getJWKS` | Get the instance-identity JWKS *(public)* |  |
| `GET` | `/healthz` | `healthz` | Health check *(public)* |  |
| `GET` | `/install.sh` | `getInstallScript` | Get the agent installer *(public)* |  |
| `GET` | `/dl/{target}` | `downloadBinary` | Download the ogm binary *(public)* |  |
| `GET` | `/` | `getDashboard` | Web dashboard *(public)* |  |
| `GET` | `/docs` | `getDocs` | API reference (this page) *(public)* |  |
| `GET` | `/reference` | `getReference` | Redirect to the API reference *(public)* |  |
| `GET` | `/openapi.json` | `getOpenAPIJSON` | OpenAPI document (JSON) *(public)* |  |
| `GET` | `/openapi.yaml` | `getOpenAPIYAML` | OpenAPI document (YAML) *(public)* |  |
| `GET` | `/llms.txt` | `getLLMsTxt` | LLM-friendly API index *(public)* |  |

