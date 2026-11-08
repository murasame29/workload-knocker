# Configuration

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `HTTP_PORT` | `8080` | HTTP port for `/metrics`, health probes and `/api/v1/*` |
| `CLUSTER_CONFIG_PATH` | `/etc/workload-knocker/config.yaml` | Cluster registry file |
| `CONTROLLER_NAMESPACE` | `workload-control` | Namespace of the `workload-state` ConfigMap in managed clusters |
| `CLUSTER_RECONCILE_TIMEOUT` | `2s` | Reconcile timeout when a chat (Slack or Discord) command arrives |
| `RECONCILE_INTERVAL` | `1m` | Period of the background ScaledObject reconcile of every cluster, bounded per cluster by `CLUSTER_RECONCILE_TIMEOUT`. `0` disables it; a negative value fails at startup. Chart: `settings.reconcileInterval` |
| `STOP_ALL_TIMEOUT` | `10s` | Per-cluster timeout for `/api/v1/stop-all` |
| `SHUTDOWN_TIMEOUT` | `5s` | Graceful shutdown timeout per component |
| `OPERATION_MODE` | `full` | `full`, `scheduled-stop` or `off`; any other value fails at startup (see [Operation modes](#operation-modes-and-nightly-stop)) |
| `AUTHZ_CONFIG_PATH` | unset | Authorization policy file. Unset and no file at `/etc/workload-knocker/authz.yaml`: authorization is disabled and everyone may start and stop everything (one info log at startup). Set, or the default file exists: it must parse or startup fails (see [Authorization](#authorization)) |
| `API_TOKEN` | empty | Bearer token for `/api/v1/*`. When empty, `/api/v1/*` responds `503` (fail closed) |
| `SLACK_APP_TOKEN` | required with Slack | Slack app-level token (`xapp-...`); required only when a cluster configures `frontends.slack` |
| `SLACK_BOT_TOKEN` | required with Slack | Slack bot token (`xoxb-...`); required only when a cluster configures `frontends.slack` |
| `DISCORD_BOT_TOKEN` | required with Discord | Discord bot token; required only when a cluster configures `frontends.discord` |
| `DISCORD_GUILD_ID` | empty | Guild (server) ID. Set: commands are registered for that guild and appear instantly. Empty: commands are registered globally (can take a while to propagate) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON with `timestamp` / `message` keys; see [Audit log](audit-log.md) |

`WORKLOAD_ENV` is deprecated and ignored (a warning is logged).

## Version

`workload-knocker --version` (or `-version`) prints one line and exits before reading any configuration:

```text
workload-knocker version=0.1.0 commit=<sha> build_date=<RFC3339 UTC>
```

Release images get the three values from build arguments. A binary built with `go build` or
`go install` falls back to the Go build info (module version and VCS revision/time) and shows
`dev` / `unknown` for what is not available. The same fields are logged at startup.

## Cluster registry

The registry is read at startup and hot reloaded afterwards (see [Hot reload](#hot-reload)).

```yaml
controller:
  namespace: workload-control
clusters:
  - env: stg
    name: example-staging
    provider:
      type: eks
      eks:
        region: ap-northeast-1
        assumeRoleArn: arn:aws:iam::123456789012:role/cross-account   # optional
    frontends:
      slack:
        slashCommand: /workload-knocker-stg
  - env: local
    name: kind-local
    provider:
      type: kubeconfig
      kubeconfig:
        path: /etc/kubeconfig/kubeconfig
        context: kind-local                                             # optional
    frontends:
      slack:
        slashCommand: /workload-knocker-local
```

| Field | Required | Description |
|---|---|---|
| `controller.namespace` | yes | Namespace for the state ConfigMap |
| `clusters[].env` | yes | Unique identifier; used as the `env` metric label |
| `clusters[].name` | yes | Cluster name; `eks:DescribeCluster` argument and `cluster` metric label |
| `clusters[].provider.type` | yes | `eks` or `kubeconfig` |
| `clusters[].provider.eks.region` | eks | AWS region |
| `clusters[].provider.eks.assumeRoleArn` | no | Role to assume for cross-account clusters |
| `clusters[].provider.kubeconfig.path` | kubeconfig | Kubeconfig file path |
| `clusters[].provider.kubeconfig.context` | no | Context; defaults to current-context |
| `clusters[].frontends.slack.slashCommand` | one frontend | Unique slash command registered in the Slack app |
| `clusters[].frontends.discord.command` | one frontend | Unique Discord slash command name: 1-32 characters of `a-z`, `0-9`, `-`, `_`, without a leading slash |

Each cluster must configure at least one entry under `frontends`. A frontend is enabled when at least
one cluster configures it, and only enabled frontends are started.

> **Breaking change:** the former `clusters[].slack.slashCommand` moved to
> `clusters[].frontends.slack.slashCommand`. The old `slack` key is rejected at startup.

## Authorization

A policy file decides who may start or stop which workload. See
[`example/authz.example.yaml`](../../example/authz.example.yaml).

```yaml
version: 1            # required, must be 1
default: deny         # deny (default) or allow, applied when no rule matches
rules:
  - env: stg                      # exact env or glob; "*" = every env
    namespaces: ["team-*"]        # globs, required
    workloads: ["checkout-*"]     # globs on the workload name, default ["*"]
    actions: [start]              # start and/or stop, default both
    subjects:
      slack:
        users: [U01ALICE00]       # Slack user IDs
        groups: [S01PLATFORM]     # Slack usergroup IDs
      discord:
        users: ["123456789012345678"]
        roles: ["223456789012345678"]
      api: true                   # the REST API caller
```

- Globs use Go `path.Match` syntax (`*`, `?`, `[a-z]`); they are case sensitive and `*` does not cross `/`.
- Any rule that matches env, namespace, workload and action and lists the subject allows the request.
  **There are no deny rules**: you cannot carve an exception out of a broader allow, so keep
  allow rules narrow and `default: deny`.
- A merged `foo-api` / `foo-app` / `foo-worker` entry is checked per member workload.
- Unknown keys, malformed globs, IDs of the wrong shape (for example a usergroup ID under `users`) and a rule
  without any subject are rejected when the file is loaded.
- The file may be YAML or JSON.

### Where it applies

| Surface | Behavior |
|---|---|
| Slack namespaces and workloads | Only namespaces and workloads the user may start or stop are listed. With none, the user gets a clear message. |
| Slack plan and apply | Every start and stop is checked again at plan time and at apply time. Entries the user may not perform are rejected with a reason, whatever the modal submitted. |
| `POST /api/v1/start` | Checked per workload for subject `api`. Refused workloads are returned in `forbidden`; if nothing was started and something was refused, the status is `403`. A refusal does not reveal whether the workload exists. |
| `POST /api/v1/stop-all` | Stops only workloads the `api` subject may stop. Running workloads that were kept are listed per cluster in `skipped` and counted in `total_skipped`. |

### Operation mode in chat

| `OPERATION_MODE` | Chat start | Chat stop |
|---|---|---|
| `full` | yes | yes |
| `scheduled-stop` | yes | no: stop entries are rejected with a message that the nightly job stops workloads |
| `off` | yes | yes |

The mode applies on top of the policy. The API side is described in [Operation modes](#operation-modes-and-nightly-stop).

### Subject resolution

| Source | Subject |
|---|---|
| Slack | The user ID plus the usergroups the user belongs to. Only usergroups named in the policy are looked up (`usergroups.users.list`) and cached for 60 seconds, so a membership change can take up to a minute to apply. |
| REST API | The fixed subject `api`. |
| Discord | The user ID plus the role IDs Discord sends with the interaction's member data (no extra API call). Built again on every interaction, so a role change applies on the next click. Role rules do not apply in DMs, which carry no roles. |

Usergroup lookups need the `usergroups:read` bot scope (listed in `slack-manifest.yaml`; reinstall the app
if you created it before this scope was added). It is only used when a rule lists `slack.groups`. If a lookup fails the controller logs a warning and matches the user ID only for that
group. It never fails open: a user allowed only through a group is denied until the lookup succeeds.

## Hot reload

The cluster registry and the authorization policy are watched with fsnotify. The parent directory is
watched, so editors' atomic renames and Kubernetes ConfigMap updates (the `..data` symlink swap) are both seen.
Events are debounced for 500 ms. `workload_knocker_config_reload_total{target,result}` counts attempts.

- A valid change is applied atomically and a summary is logged. In-flight requests finish on the previous config.
- An invalid change (parse or validation error, deleted file) is rejected: the previous config stays active, an
  error is logged and `result="error"` is incremented.
- Registry: clusters can be added, removed or changed. New or changed clusters are built and bootstrapped
  (30 s timeout each) before the swap. If a changed cluster cannot be bootstrapped its old connection is kept
  and a warning is logged. An added cluster that fails to bootstrap is kept with `cluster_reachable=0`, like at
  startup. Removed clusters disappear from slash command resolution and their `workload_knocker_activation`
  and `workload_knocker_cluster_reachable` series are deleted. Slash commands are resolved against the live registry.
  Discord: the controller compares the registry's command set with the registered one every 15 seconds and, when it changed, bulk-overwrites the application commands again (a failure is logged and retried). Global commands can take longer to appear in clients; a guild ID makes this instant. Enabling Discord for the first time still needs a restart.
- Authorization: a policy file that appears after startup enables authorization. Deleting the file keeps the last policy.
- A registry change that needs a restart is rejected with a clear error and the old config stays: changing
  `controller.namespace`, enabling a frontend that was not enabled at startup, or enabling one without its tokens.
- Environment variables (tokens, `OPERATION_MODE`, timeouts, paths) are never hot reloaded.

In Kubernetes, a ConfigMap change reaches the Pod after the kubelet sync, usually within about a minute. The chart
therefore mounts the registry and the policy as one projected volume at `/etc/workload-knocker` (a `subPath`
mount would never update) and does not put a checksum annotation on the Pod template for either ConfigMap, so a
change does not restart the Pod. Run `kubectl rollout restart` only for the restart-required cases above.
Because that directory is read-only and managed, mount extra files such as kubeconfigs elsewhere.

## Discord

Discord runs over the gateway websocket, so no public HTTP endpoint is needed.

```yaml
clusters:
  - env: stg
    name: example-staging
    provider: {type: eks, eks: {region: ap-northeast-1}}
    frontends:
      discord:
        command: workload-knocker-stg
```

Setup:

1. In the [Discord Developer Portal](https://discord.com/developers/applications) create an application and a bot, and copy the bot token into a Secret (`bot-token`).
2. No privileged gateway intents are needed. The bot uses the `Guilds` intent only.
3. Invite the bot with the OAuth2 scopes `applications.commands` and `bot`. The `bot` scope needs no extra permissions: replies are ephemeral interaction responses, so the bot does not need to read or send messages.
4. Set `DISCORD_BOT_TOKEN` (chart: `discord.secretName`, optional `discord.guildId`). Prefer a guild ID while testing: guild commands are available immediately, global commands are not.

On startup the controller bulk-overwrites the application commands of the chosen scope (guild or global) with the configured commands. Other commands of the same application in that scope are removed, so use a dedicated application. Startup fails with a clear error when registration fails (usually a missing `applications.commands` scope).

Flow (every message is ephemeral and only the invoking user can use its components):

1. The slash command shows a namespace select menu (25 per page, Previous/Next buttons).
2. Choosing a namespace shows the workloads in a multi-select (25 per page). Running entries are pre-selected; selections are kept across pages. Merging of `-api` / `-app` / `-worker` and the 🟢 / 🟡 / 🔴 markers match Slack.
3. **Apply** shows the plan preview with **Confirm** and **Cancel**.
4. **Confirm** applies the plan and replaces the message with the result. The operation user is the Discord user ID.

Per-interaction state (selection, page, plan) is kept in memory, keyed by a random token in each component's `custom_id` (`wk1:<kind>:<token>:<arg>`, well under Discord's 100-character limit). Flows expire after 15 minutes without use and are lost on restart; the message then asks the user to run the command again. This is why the controller must keep a single replica. The controller answers each interaction within Discord's 3-second window by deferring the slow steps (command start and Confirm).

Authorization and operation mode apply as for Slack. Namespaces and workloads the user's ID or roles may not operate are not listed, and plan and Confirm check the policy again, so a stale menu, a changed role or a forged component cannot widen access. In `scheduled-stop` mode the menu says stopping is disabled and a stop is rejected with the same message as Slack. Rules use `subjects.discord.users` (user IDs) and `subjects.discord.roles` (role IDs); enable Discord Developer Mode to copy IDs.

The chart renders `DISCORD_BOT_TOKEN` and `DISCORD_GUILD_ID` only when `discord.secretName` is set. Likewise `SLACK_APP_TOKEN` and `SLACK_BOT_TOKEN` are rendered only when `slack.secretName` is set. Set at least one of the two; the chart fails otherwise.

## Managed cluster access

For EKS, run the controller with an IAM role (IRSA), register that role in the cluster's
Access Entry with `kubernetes_groups: ["workload-knocker"]`, and apply
[`example/managed-cluster-rbac.yaml`](../../example/managed-cluster-rbac.yaml). The controller
needs `get/list/watch` on `scaledobjects`, `get` on `deployments`, `statefulsets` and their `scale`
subresources, and `get/create/update` on ConfigMaps in `controller.namespace`.

When a ScaledObject is discovered that has no entry in the state ConfigMap, the controller reads
its scale target (`Deployment` by default, or `StatefulSet`). A target with replicas above 0 is
registered as running (`workload_knocker_activation` 1, saved to the state ConfigMap) so KEDA does
not scale it down; otherwise, or if the target cannot be read (a warning is logged), it is
registered as stopped. Saved state always wins over the live replicas.

ScaledObjects are re-listed every `RECONCILE_INTERVAL`: a new one gets its series within one
interval and a deleted one loses it, without a chat command.

## Slack workload modal

- Namespaces and workloads are sorted a to z.
- `foo-api`, `foo-app` and `foo-worker` are merged into one `foo` checkbox when at least two of
  those suffixes exist. A partially running group is shown as 🟡 and checked.
- Slack allows 10 options per checkbox block and 100 blocks per modal, so at most 90 merged
  entries are shown. Hidden entries are excluded from the plan and are never stopped by accident.

## Operation modes and nightly stop

The Helm chart sets the controller's `OPERATION_MODE` and `API_TOKEN` environment variables.

| `mode` | API start | API stop / stop-all | `nightlyStop` |
| --- | --- | --- | --- |
| `full` (default) | yes | yes | optional |
| `scheduled-stop` | yes | no (`403`; the nightly CronJob stops workloads) | required (`enabled: true`) |
| `off` | yes | no | must not be enabled |

`api.tokenSecret.name` is required in every mode so that `/api/v1/*` is never unauthenticated.
Create the Secret before installing:

```sh
kubectl create secret generic workload-knocker-api \
  --namespace workload-knocker \
  --from-literal=api-token='<random-token>'
```

`nightlyStop` renders a CronJob that sends `POST /api/v1/stop-all` to the in-cluster Service with
`Authorization: Bearer <token>` and `X-Workload-Knocker-Source: nightly-stop` (the audit log source), and fails on any non-2xx response.

| Value | Default |
| --- | --- |
| `nightlyStop.enabled` | `false` |
| `nightlyStop.schedule` / `timeZone` | `0 22 * * *` / `Asia/Tokyo` |
| `nightlyStop.suspend` | `false` |
| `nightlyStop.startingDeadlineSeconds` | `300` |
| `nightlyStop.concurrencyPolicy` | `Forbid` |
| `nightlyStop.successfulJobsHistoryLimit` / `failedJobsHistoryLimit` | `1` / `3` |
| `nightlyStop.backoffLimit` | `2` |
| `nightlyStop.requestTimeoutSeconds` / `curlRetries` | `60` / `3` |
| `nightlyStop.image` | `curlimages/curl`, pinned by digest |

Invalid combinations (unknown `mode`, `scheduled-stop` without the CronJob, `off` with the
CronJob, empty `api.tokenSecret.name`) fail at `helm template`/`helm install`.

## HTTP API

### Authentication

`/api/v1/*` requires `Authorization: Bearer <token>`, where the token is the controller's `API_TOKEN`.
`/metrics`, `/livez`, `/healthz` and `/readyz` are unauthenticated.

```sh
curl -X POST "http://workload-knocker.workload-knocker.svc:8080/api/v1/start" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"env":"stg","namespace":"my-namespace","workloads":["a"]}'
```

Checks run in this order: authentication, operation mode, request validation.

| Status | Meaning |
|---|---|
| `401` | Missing or invalid token (`WWW-Authenticate: Bearer`) |
| `403` | The operation is not allowed in the current `OPERATION_MODE` |
| `503` | `API_TOKEN` is not set, so the API is disabled |


### `POST /api/v1/start`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a", "b"], "user": "ci-bot"}
```

`user` is optional (default `rest-api`). Returns `200` with `started`, `not_found` and `forbidden` lists,
`400` for invalid input, `403` when every requested workload is refused by the [authorization policy](#authorization),
`404` for an unknown `env` or when no workload was found.

### `POST /api/v1/stop-all`

Allowed only in `full` mode (`403` otherwise). Stops the workloads the `api` subject may stop in all managed
clusters (best effort); running workloads kept by the authorization policy are reported in `skipped`
per cluster and in `total_skipped`. Returns `200` with a per-cluster breakdown even when some clusters fail.

### Probes

`/livez` and `/healthz` return 200 once the process is up. `/readyz` returns 200 after the
initial sync of all clusters.

## Metrics

| Metric | Type | Labels |
|---|---|---|
| `workload_knocker_activation` | gauge | `env`, `cluster`, `namespace`, `workload` |
| `workload_knocker_cluster_reachable` | gauge | `cluster` |
| `workload_knocker_token_refresh_total` | counter | `cluster`, `result` |
| `workload_knocker_operations_total` | counter | `env`, `cluster`, `operation`, `result` |
| `workload_knocker_operation_duration_seconds` | histogram | `env`, `cluster`, `operation` |
| `workload_knocker_config_reload_total` | counter | `target` (`registry`, `authz`), `result` (`success`, `error`) |
| `workload_knocker_reconcile_total` | counter | `cluster`, `result` (`success`, `error`) |

`operation` is `start`, `stop` or `stop_all`. `result` is `success`, `error` or `not_found`
(`error` is only reported by `stop_all`). User IDs and workload names are never used as labels
to keep cardinality bounded.
