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
| `ROLLOUT_POLL_INTERVAL` | `5s` | How often the scale targets are polled after Confirm (at least `1s`) |
| `ROLLOUT_WATCH_TIMEOUT` | `10m` | When rollout progress gives up and shows the last observed reason. Must be above the poll interval and below `15m` (Discord interaction token lifetime); other values fail at startup |
| `DEFAULT_RUN_DURATION` | `0` | Auto-stop delay of a start that gives no duration. `0` means no auto-stop (the previous behavior). Must be `0` or at least `15m`, and not above `MAX_RUN_DURATION`. Chart: `settings.defaultRunDuration` |
| `MAX_RUN_DURATION` | `24h` | Upper bound of a run duration and of the remaining run time after an extension. At least `15m`. Chart: `settings.maxRunDuration` |
| `STOP_REMINDER` | `15m` | How long before the auto-stop the reminder fires. `0` disables it. Delivery needs a notify channel (see [Run duration](#run-duration)). Chart: `settings.stopReminder` |
| `RESERVATION_WARMUP` | `5m` | How long before a [reservation](#reservations)'s time its workloads start; the `workload-knocker.io/warmup` annotation overrides it. Chart: `settings.reservationWarmup` |
| `RESERVATION_GRACE` | `15m` | How long after its start time a reservation still runs when the controller was down; later it is marked missed. Chart: `settings.reservationGrace` |
| `RESERVATION_REMINDER` | `15m` | How long before a reservation starts the reminder is posted to the notify channel. `0` disables it. Chart: `settings.reservationReminder` |
| `GITOPS_MANAGERS` | `argocd-controller,kustomize-controller,helm-controller` | Comma-separated field managers whose ownership of `spec.replicas` stops the `direct` actuator from scaling a workload (see [GitOps](gitops.md#direct-actuator)). Chart: `settings.gitopsManagers` |
| `SHUTDOWN_TIMEOUT` | `5s` | Graceful shutdown timeout per component |
| `OPERATION_MODE` | `full` | `full`, `scheduled-stop` or `off`; any other value fails at startup (see [Operation modes](#operation-modes-and-nightly-stop)) |
| `AUTHZ_CONFIG_PATH` | unset | Authorization policy file. Unset and no file at `/etc/workload-knocker/authz.yaml`: authorization is disabled and everyone may start and stop everything (one info log at startup). Set, or the default file exists: it must parse or startup fails (see [Authorization](#authorization)) |
| `API_TOKEN` | empty | Bearer token for `/api/v1/*`. When empty, `/api/v1/*` responds `503` (fail closed) |
| `SLACK_APP_TOKEN` | required with Slack | Slack app-level token (`xapp-...`); required only when a cluster configures `frontends.slack` |
| `SLACK_BOT_TOKEN` | required with Slack | Slack bot token (`xoxb-...`); required only when a cluster configures `frontends.slack` |
| `DISCORD_BOT_TOKEN` | required with Discord | Discord bot token; required only when a cluster configures `frontends.discord` |
| `DISCORD_GUILD_ID` | empty | Guild (server) ID. Set: commands are registered for that guild and appear instantly. Empty: commands are registered globally (can take a while to propagate) |
| `EXTERNAL_SCALER_PORT` | `9090` | gRPC port of the KEDA external scaler, served only while a cluster uses `actuator: keda-external`. Chart: `externalScaler.port` |
| `EXTERNAL_SCALER_TLS_CERT` / `EXTERNAL_SCALER_TLS_KEY` | empty | Certificate and key paths that enable TLS on that port (both or neither; re-read on every handshake). Chart: `externalScaler.tls.secretName` |
| `EXTERNAL_SCALER_ADDRESS_MATCH` | `workload-knocker` | A trigger is served when its `scalerAddress` contains this text. The chart sets it to the Service name |
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
| `clusters[].env` | yes | Unique identifier (letters, digits, `-`, `_`, `.`); used as the `env` metric label and as the state key `state.<env>.json` in the `workload-state` ConfigMap. Several entries may point at the same cluster; each keeps its own key. To mix actuators behind one command, prefer `actuators` |
| `clusters[].name` | yes | Cluster name; `eks:DescribeCluster` argument and `cluster` metric label |
| `clusters[].actuator` | no | How workloads are discovered and started or stopped. `keda-prometheus` (default) manages `ScaledObject`s with a `workload_knocker_activation` Prometheus trigger and publishes that gauge. `keda-external` serves KEDA's external scaler gRPC protocol without Prometheus (see [KEDA external scaler](#keda-external-scaler)). `direct` needs neither KEDA nor Prometheus (see [Direct actuator](#direct-actuator)). Unknown values are rejected. Changing it hot reloads the cluster. See [GitOps](gitops.md) |
| `clusters[].actuators` | no | List of actuators used together, for example `[keda-external, direct]`; each workload is driven by the actuator that discovered it, so one chat command covers all of them (see [Several actuators](#several-actuators)). Mutually exclusive with `actuator`; an entry may not repeat |
| `clusters[].provider.type` | yes | `eks` or `kubeconfig` |
| `clusters[].provider.eks.region` | eks | AWS region |
| `clusters[].provider.eks.assumeRoleArn` | no | Role to assume for cross-account clusters |
| `clusters[].provider.kubeconfig.path` | kubeconfig | Kubeconfig file path |
| `clusters[].provider.kubeconfig.context` | no | Context; defaults to current-context |
| `clusters[].frontends.slack.slashCommand` | one frontend | Unique slash command registered in the Slack app |
| `clusters[].frontends.discord.command` | one frontend | Unique Discord slash command name: 1-32 characters of `a-z`, `0-9`, `-`, `_`, without a leading slash |
| `clusters[].frontends.slack.notifyChannel` | no | Slack channel ID (for example `C0123ABCD`; a channel name is rejected) that receives public start, ready and stop messages. See [Ready message and notify channel](#ready-message-and-notify-channel) |
| `clusters[].frontends.discord.notifyChannel` | no | Discord channel ID (17-20 digits) that receives the same messages |
| `clusters[].frontends.discord.timeZone` | no | IANA time zone (for example `Asia/Tokyo`) in which [reservation](#reservations) times are entered and shown in notify messages. Default `UTC`; unknown names are rejected |

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
| Status (`/<command> status`, `GET /api/v1/workloads`) | Lists only workloads the subject may start or stop. Extend and Stop buttons are shown only when the subject may do that action and the operation mode allows it; both are checked again when the button is pressed. |
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
  A `notifyChannel` change applies to the next message without a restart.
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
3. Invite the bot with the OAuth2 scopes `applications.commands` and `bot`. The `bot` scope needs no extra permissions as long as no `notifyChannel` is set: replies are ephemeral interaction responses, so the bot does not need to read or send messages. With a [notify channel](#ready-message-and-notify-channel) the bot needs **View Channel** and **Send Messages** in that channel (grant them on the channel, not server-wide). No message content intent is needed.
4. Set `DISCORD_BOT_TOKEN` (chart: `discord.secretName`, optional `discord.guildId`). Prefer a guild ID while testing: guild commands are available immediately, global commands are not.

On startup the controller bulk-overwrites the application commands of the chosen scope (guild or global) with the configured commands. Other commands of the same application in that scope are removed, so use a dedicated application. Startup fails with a clear error when registration fails (usually a missing `applications.commands` scope).

The command has three subcommands, `start`, `status` (see [Status](#status)) and `schedule` (see [Reservations](#reservations)). Discord does not allow invoking a command that has subcommands without one ([Discord docs](https://discord.com/developers/docs/interactions/application-commands#subcommands-and-subcommand-groups)), so **`/<command>` alone no longer works: use `/<command> start`**. This is a breaking change for Discord users and runs through the same registration at startup, so no manual step is needed. A client that still sends the old bare command (a cached command shape) is treated as `start`.

Flow of `/<command> start` (every message is ephemeral and only the invoking user can use its components):

1. When the user may operate at most **5 namespaces with at most 10 entries each**, the command opens a modal: one checkbox group per namespace. Running entries are checked; checked entries run and unchecked ones stop. Merging of `-api` / `-app` / `-worker` and the 🟢 / 🟡 / 🔴 markers match Slack. Submitting shows the plan preview of every namespace.
2. Otherwise the command shows a namespace select menu (25 per page, Previous/Next buttons). Choosing a namespace shows its workloads in a multi-select (25 per page; running entries pre-selected, selections kept across pages). **Other namespace** goes back to the menu and keeps the selection, so one plan can cover several namespaces. **Apply** shows the plan preview.
3. The preview lists each namespace with **Confirm**, **Change duration** and **Cancel**.
4. **Confirm** applies the plan, one request per namespace, and replaces the message with the result. The operation user is the Discord user ID. The message then follows the [rollout](#rollout-progress).

A Discord modal must be the first response to the command, so the modal is built from the controller's in-memory state, which the background reconcile ([environment variables](#environment-variables)) keeps fresh (`RECONCILE_INTERVAL`); the message-based flow still reconciles on demand.

Per-interaction state (selection, page, plan) is kept in memory, keyed by a random token in each component's `custom_id` (`wk1:<kind>:<token>:<arg>`, well under Discord's 100-character limit). Flows expire after 15 minutes without use and are lost on restart; the message then asks the user to run the command again. This is why the controller must keep a single replica. The controller answers each interaction within Discord's 3-second window by deferring the slow steps (command start and Confirm).

Authorization and operation mode apply as for Slack. Namespaces and workloads the user's ID or roles may not operate are not listed, and plan and Confirm check the policy again, so a stale menu, a changed role or a forged component cannot widen access. In `scheduled-stop` mode the menu says stopping is disabled and a stop is rejected with the same message as Slack. Rules use `subjects.discord.users` (user IDs) and `subjects.discord.roles` (role IDs); enable Discord Developer Mode to copy IDs.

The chart renders `DISCORD_BOT_TOKEN` and `DISCORD_GUILD_ID` only when `discord.secretName` is set. Likewise `SLACK_APP_TOKEN` and `SLACK_BOT_TOKEN` are rendered only when `slack.secretName` is set. Set at least one of the two; the chart fails otherwise.

## Managed cluster access

For EKS, run the controller with an IAM role (IRSA), register that role in the cluster's
Access Entry with `kubernetes_groups: ["workload-knocker"]`, and apply
[`example/managed-cluster-rbac.yaml`](../../example/managed-cluster-rbac.yaml). The controller
needs (for `keda-prometheus`; `direct` has [its own list](#direct-actuator)) `get/list/watch` on `scaledobjects`, `get/list` on `deployments`, `statefulsets`, their
`scale` subresources and `pods` (discovery and rollout progress), and `get/create/patch` (state is written per env key with a merge patch) on
ConfigMaps in `controller.namespace`.

When a ScaledObject is discovered that has no entry in the state ConfigMap, the controller reads
its scale target (`Deployment` by default, or `StatefulSet`). A target with replicas above 0 is
registered as running (`workload_knocker_activation` 1, saved to the state ConfigMap) so KEDA does
not scale it down; otherwise, or if the target cannot be read (a warning is logged), it is
registered as stopped. Saved state always wins over the live replicas.

ScaledObjects are re-listed every `RECONCILE_INTERVAL`: a new one gets its series within one
interval and a deleted one loses it, without a chat command.

## Direct actuator

`actuator: direct` needs neither KEDA nor Prometheus. The controller manages the Deployments and
StatefulSets of the cluster that carry these annotations (all namespaces):

| Annotation | Default | Meaning |
|---|---|---|
| `workload-knocker.io/enabled` | unset | `"true"` opts the workload in |
| `workload-knocker.io/replicas` | `1` | Replicas while running; a positive integer |

The other `workload-knocker.io/*` annotations (run durations, ready message) work as for ScaledObjects.
A start sets the scale subresource to the running replicas and a stop sets it to `0`; the workload
object itself is never written. The initial state of a newly discovered workload is its current
replicas (above 0 is running), and the previous state lives in the state ConfigMap.

A workload is skipped, with one warning per change, when:

- `workload-knocker.io/replicas` is not a positive integer.
- A HorizontalPodAutoscaler targets it, because an HPA cannot scale to 0.
- A field manager from `GITOPS_MANAGERS` owns its `spec.replicas` in `metadata.managedFields`. The
  start or stop fails with `replicas is managed by <manager>; remove replicas from the manifest`,
  a warning is logged and `workload_knocker_gitops_conflicts` is set to 1 (0 once resolved). See
  [GitOps](gitops.md#direct-actuator).

If one of these is found at start or stop (for example an HPA added after the last discovery), the operation is
refused and the state is left as it was. Chat shows `⛔ <workload>: <reason>`, the `POST /api/v1/start` response lists
it under `refused`, and the audit event is a failure. A refused auto-stop is retried every reconcile and recorded once.

Example cluster entry (`example/config.example.yaml` has the full snippet):

```yaml
clusters:
  - env: small
    name: small-cluster
    actuator: direct
    provider:
      type: kubeconfig
      kubeconfig:
        path: ~/.kube/config
```

Extra RBAC beyond the default rules, all in `example/managed-cluster-rbac.yaml`:
`get/list/watch` on `deployments` and `statefulsets`, `get/update/patch` on `deployments/scale` and
`statefulsets/scale`, and `list` on `horizontalpodautoscalers`. `scaledobjects` access is not needed.

## Several actuators

`actuators` lists several actuators for one cluster entry. Every actuator discovers its own
workloads (ScaledObjects with its trigger, or annotated Deployments and StatefulSets for `direct`),
the lists are merged, and start / stop / status go to the actuator that discovered the workload.
Users see one command and one workload list.

```yaml
clusters:
  - env: home
    name: home-cluster
    actuators: [keda-external, direct]
    provider:
      type: kubeconfig
      kubeconfig:
        path: ~/.kube/config
    frontends:
      discord:
        command: workload-knocker
```

- Order is priority: when two actuators discover the same workload (for example a ScaledObject
  target that also carries `workload-knocker.io/enabled`), the first one wins and a warning is
  logged once.
- When a workload moves to another actuator (for example its ScaledObject is removed and the
  annotation is added), the previous actuator releases it on the next discovery.
- If any actuator fails to discover, the whole discovery fails and the previous list is kept, so
  workloads do not disappear from chat because of one actuator.
- The RBAC and controller settings of every listed actuator are required (for example
  `externalScaler.enabled` for `keda-external`).
- `actuator: x` and `actuators: [x]` are equivalent. Changing the list hot reloads the cluster.

## Rollout progress

After Confirm the controller polls the scale target (`Deployment` or `StatefulSet`) of every
applied workload in its cluster every `ROLLOUT_POLL_INTERVAL` and reports one line per workload:

| Line | Meaning |
|---|---|
| `⏳ name starting...` | Started; `readyReplicas` has not reached `spec.replicas` yet. This also covers the delay until KEDA changes the replica count (its polling interval). |
| `✅ name started` | `readyReplicas >= spec.replicas` and above 0. A [ready message](#ready-message-and-notify-channel) is appended as `✅ name started — <message>`. |
| `⏳ name stopping...` | Stop requested; replicas or ready pods remain. |
| `⏹ name stopped` | `spec.replicas` is 0 and no pod is ready. |
| `⚠ name not ready after 10m (0/1 ready, ImagePullBackOff)` | Not started within `ROLLOUT_WATCH_TIMEOUT`. The reason is the last `ready/desired` plus a waiting or scheduling reason of the pods (for example `ImagePullBackOff`, `Unschedulable`). A stop that did not finish reads `still running after 10m`. |
| `ℹ name applied (progress is not tracked for kind ...)` | The scale target kind is not a Deployment or StatefulSet. |

- Discord edits the interaction response, at most once per poll interval and only when a line changed. Interaction tokens last 15 minutes, hence the `ROLLOUT_WATCH_TIMEOUT` limit.
- Slack posts the result message with `⏳` lines right away, then one more ephemeral message with the final state once every workload is final. Slack cannot edit a `chat.postEphemeral` message, and the modal submission has no `response_url`, so there are no intermediate updates.
- Progress is in memory: a controller restart ends it, and the last message stays as it was.
- The controller keeps polling when a read fails (for example missing RBAC) and shows the error as the reason on timeout.

## Ready message and notify channel

### Ready message

Users learn that a workload is usable from the `✅ name started` line. A ScaledObject can add a hint to that line (how to connect, for example) with the annotation `workload-knocker.io/ready-message`, a Go [`text/template`](https://pkg.go.dev/text/template):

```yaml
metadata:
  annotations:
    workload-knocker.io/ready-message: "Connect to {{.Name}}.example.com:7777{{if .StopAt}} (until {{.StopAt}}){{end}}"
```

| Field | Value |
|---|---|
| `.Name` | Workload name |
| `.Namespace` | Namespace |
| `.Cluster` | The registry `env` |
| `.StartedBy` | The starter as recorded in the store: Discord user ID, Slack user name, or the `user` of an API call |
| `.StopAt` | Planned stop time as `15:04 MST`; empty when unknown, so guard it with `{{if .StopAt}}` |

Do not put secrets in the annotation: the text is shown in chat. The rendered text is limited to 500 characters.

- The template is checked when the ScaledObject is discovered or its annotation changes. An invalid template logs one `WARN` (`invalid ready-message template`) per change.
- A template that fails to render logs a `WARN` and the line falls back to the plain `✅ name started`.
- It is appended to the started line of the progress message (Discord edits the message; Slack puts it into the final message) and to the notify channel message.

### Notify channel

By default the bot only answers the user who ran the command, with ephemeral messages. With `notifyChannel` set on a cluster's Slack or Discord frontend, the controller also posts public messages to that channel, so others see what is running:

| Event | Message |
|---|---|
| Start requested (chat or `POST /api/v1/start`) | `🚀 <who> requested start (ENV / cluster / namespace)` with a `⏳ name starting...` line per workload. The same message is edited to `✅ name started — <ready message>` when each workload becomes ready, so there is one message per operation. Ready detection needs the rollout tracking described above. |
| Stop (chat) | `⏹ <who> stopped workloads (...)` |
| Stop-all (`POST /api/v1/stop-all`) | One message per cluster naming `nightly-stop` (the chart's CronJob) or `api` and the workloads stopped. Workloads kept by the authorization policy and clusters that failed are not listed. |
| Stop reminder | `⏰ name stops at 22:00 JST (ENV / namespace)` with an **Extend 1h** button. Auto-stop posts it `STOP_REMINDER` ahead of the stop. Pressing the button runs the same extend as the chat and API (authorization, the workload maximum and the `workload.extend` audit event apply) and answers the presser privately with the new stop time or the reason it was refused. |

Notes:

- Without `notifyChannel` nothing is posted and no extra permission is needed. Posts never block or fail an operation: a failed post is logged as `notify post failed`.
- Discord messages are sent with mentions disabled, so naming a user pings nobody. In Slack the `<@user>` mention can notify the user it names.
- Discord: grant the bot **View Channel** and **Send Messages** in the channel. Slack: the bot needs the `chat:write` scope (already in `slack-manifest.yaml`) and must be invited to the channel (`/invite @workload-knocker`); for a private channel use its `G...` ID.
- Edits for several workloads that become ready within one second are combined into one API call. Messages are kept in memory only: after a controller restart an in-flight start message is not edited any more.

## Slack workload modal

`/<command>` without an argument opens this modal. `/<command> status` lists the workloads instead (see [Status](#status)), `/<command> schedule` opens the [reservation](#reservations) form; any other argument gets a short usage message.

- When every namespace the user may operate fits one modal (one checkbox block per 10 entries of
  each namespace, at most 97 blocks), the modal lists all namespaces at once, labelled by namespace,
  with running entries checked. **Plan** previews every namespace with changes and **Apply** applies
  one request per namespace. Otherwise the modal starts with a namespace select.
- Namespaces and workloads are sorted a to z.
- `foo-api`, `foo-app` and `foo-worker` are merged into one `foo` checkbox when at least two of
  those suffixes exist. A partially running group is shown as 🟡 and checked.
- Slack allows 10 options per checkbox block and 100 blocks per modal, so at most 90 merged
  entries are shown. Hidden entries are excluded from the plan and are never stopped by accident.

## Status

`/<command> status` (Discord subcommand, Slack argument) answers with an ephemeral list of the workloads the user may operate, grouped by namespace (a to z). Each entry shows the state (🟢 running, 🟡 partly running, 🔴 stopped), who started it, when, and its stop time (`stops <time>`, or `no auto-stop`). Times are rendered in the viewer's time zone (Discord relative time, Slack date format). `-api` / `-app` / `-worker` workloads are merged as in the start flow.

The list is built from the controller's in-memory state only. It makes no cluster call and does not reconcile, so it answers inside Discord's 3-second window. The state is refreshed by the background reconcile (`RECONCILE_INTERVAL`).

Running entries carry buttons (the label names the entry):

- **Extend 1h** adds one hour to the stop time, like `POST /api/v1/extend`. It is shown only for entries with an auto-stop.
- **Stop** stops the entry directly, without a confirm step. It goes through the normal stop path: policy check, operation mode check, `workload.stop` audit event (source `discord` or `slack`) and the notify channel. It is not shown in `scheduled-stop` mode.

After a press the list is replaced in place with the outcome and the refreshed state. Both checks run again on press, so an outdated list cannot widen access.

Limits and caveats:

- Discord allows 25 buttons per message and 2000 characters of text; Slack allows 50 blocks. Entries beyond that still appear in the list (the text ends with a `… and N more` line when it is cut) but have no buttons; the message says so. Use `/<command> start` for those.
- The starter and start time are recorded when a start is applied and stored in the `workload-state` ConfigMap (`startedBy`, `startedAt`). Workloads that were found already running when the controller first saw them, and state written by older versions, show no starter or start time. The starter is the Discord user ID (shown as a mention), the Slack user name, or the `user` of an API call.
- Only the entries that are visible after the [authorization policy](#authorization) are listed.
- Discord keeps the button payloads in memory for 24 hours; after that, or after a restart, a stale button answers that it expired. Slack buttons carry their payload and keep working.

The REST equivalent is [`GET /api/v1/workloads`](#get-apiv1workloads).

## Run duration

A start can carry a run duration. The controller stores `stopAt` per workload in the `workload-state`
ConfigMap and the background loop (`RECONCILE_INTERVAL`, so the stop is accurate to about one interval)
stops expired workloads, also after a restart. Stopping a workload clears `stopAt`. The nightly stop keeps
working; whichever comes first stops the workload. State written by older versions has no `stopAt` and means
"no auto-stop". The same state also records `startedBy` and `startedAt` for the [status view](#status). With `RECONCILE_INTERVAL=0` no auto-stop fires.

Defaults can be set per workload with ScaledObject annotations; the environment variables are the fallback:

| Annotation | Fallback | Meaning |
|---|---|---|
| `workload-knocker.io/default-duration` | `DEFAULT_RUN_DURATION` | Go duration such as `4h`. `0` disables the default for this workload. Values between `0` and `15m` are ignored with a warning |
| `workload-knocker.io/max-duration` | `MAX_RUN_DURATION` | Go duration, at least `15m`. Invalid values are ignored with a warning |

A default above the maximum is lowered to the maximum.

- **Discord**: the plan preview shows each start with its stop time (in the viewer's time zone) and a
  **Change duration** button. It opens a modal with one input per started workload (at most 5, the first 5 if
  there are more) prefilled with the current value in hours. Enter hours as a number (`3`, `1.5`; minimum `0.25`);
  empty means the workload default. An invalid value gets an ephemeral reply with the reason and a **Try again**
  button that reopens the modal with the previous input.
- **Slack**: the workload modal has an optional **Run duration (hours)** number input (decimals allowed) that applies
  to every workload started in that submission. Empty means each workload's default. Out-of-range values are
  rejected with an inline error.
- **REST**: `POST /api/v1/start` accepts `duration` (Go duration, for example `"3h"`); see [HTTP API](#post-apiv1start).
- **Extend**: `POST /api/v1/extend` and the frontend service add time to a running workload's `stopAt`. It is
  authorized like a start and allowed in every operation mode. The remaining run time after the extension may not
  exceed the maximum; otherwise the extension is rejected.
- **Reminder**: `STOP_REMINDER` before `stopAt` the reconcile loop calls a `StopReminder` once per workload and stop
  time (remembered in memory, so a restart may send it again). With a notify channel it is posted there
  with an **Extend 1h** button; without one nothing is posted.

Audit events: a start records `workload_knocker.duration_seconds` and `workload_knocker.stop_at`; an extension
is `workload.extend`; an expiry is `workload.auto_stop` with source `scheduler`.

## Reservations

A one-off reservation makes workloads **Ready at a requested time**. Create one with `/<command> schedule` in
Discord or Slack, or [`POST /api/v1/reservations`](#apiv1reservations).

- **Slack form**: `/<slashCommand> schedule` opens a modal with one checkbox group per namespace (unchecked), a
  date and time picker shown in the user's own Slack time zone (no setting needed), and the run duration in hours.
  The modal holds every namespace when they fit Slack's block limit; otherwise use the API.

- **Discord form**: one checkbox group per namespace (at most 3 namespaces of 10 entries; use the API beyond),
  **Ready at** (`21:00` means today, or tomorrow once passed; `2026-11-14 21:00` for another day) in
  `frontends.discord.timeZone`, and the run duration in hours (empty uses the workload default). One reservation is
  created per namespace.
- **Execution**: the background reconcile loop (`RECONCILE_INTERVAL`) starts the workloads `RESERVATION_WARMUP`
  (default `5m`) before the time; `workload-knocker.io/warmup: "20m"` on a workload raises it (the largest one of the
  reservation applies, up to `6h`). The auto-stop is the reserved time plus the run duration. The start posts the usual
  start and ready messages to the notify channel, and `RESERVATION_REMINDER` before the start a reminder is posted.
- **Authorization**: checked at creation and again at execution with the creator's user and the roles sent with the
  creating interaction. A workload refused at execution (policy, GitOps conflict, removed) is listed in the
  reservation's note; the reservation fails when none started.
- **Restarts**: reservations are stored in the `workload-state` ConfigMap under `reservations.<env>.json` (no extra
  RBAC), so they survive a Pod restart. A reservation whose start passed while the controller was down still runs
  within `RESERVATION_GRACE` (default `15m`); later it is marked missed and the notify channel is told. The result is
  saved after the workloads start, so a controller that dies in between runs the reservation again on restart
  (starting a running workload changes nothing; the start message may be posted twice).
- **List and cancel**: `/<command> status` (Discord and Slack) lists the pending reservations with a **Cancel** button for the creator and
  for anyone allowed to stop all of their workloads. Finished reservations stay in `GET /api/v1/reservations` for a day.
- Audit events: `reservation.create`, `reservation.cancel` and one `reservation.execute` per workload, all with the
  reservation ID as `workload_knocker.request_id`.

Recurring schedules and learned warmup are tracked in
[#75](https://github.com/murasame29/workload-knocker/issues/75) and [#76](https://github.com/murasame29/workload-knocker/issues/76).

## Operation modes and nightly stop

The Helm chart sets the controller's `OPERATION_MODE` and `API_TOKEN` environment variables.

| `mode` | API start | API stop / stop-all | `nightlyStop` |
| --- | --- | --- | --- |
| `full` (default) | yes | yes | optional |
| `scheduled-stop` | yes | no (`403`; the nightly CronJob stops workloads) | required (`enabled: true`) |
| `off` | yes | no | must not be enabled |

`api.tokenSecret.name` is required when `nightlyStop.enabled` is true, because the CronJob calls
`/api/v1/stop-all`. Otherwise it is optional: leave it empty when you do not use the REST API, and
`API_TOKEN` is not rendered, so `/api/v1/*` answers `503` (fail closed) while chat keeps working.
To use the API, create the Secret before installing:

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
CronJob, the CronJob without `api.tokenSecret.name`) fail at `helm template`/`helm install`.

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


### `GET /api/v1/workloads`

Lists the workloads the `api` subject may start or stop, sorted by `env`, `namespace` and `name`, from the controller's
in-memory state (no cluster call). Allowed in every operation mode. Always `200` with a JSON object; the list is empty when
nothing is visible.

```json
{"workloads": [
  {"env": "stg", "namespace": "my-namespace", "name": "a", "kind": "Deployment", "state": "running",
   "startedBy": "ci-bot", "startedAt": "2026-11-08T09:00:00Z", "stopAt": "2026-11-08T12:00:00Z"},
  {"env": "stg", "namespace": "my-namespace", "name": "b", "kind": "Deployment", "state": "stopped",
   "startedBy": "", "startedAt": null, "stopAt": null}
]}
```

`state` is `running` or `stopped`. `startedBy`, `startedAt` and `stopAt` describe the current run: `startedBy` is empty and
the times are `null` for a stopped workload, `startedAt` is `null` when the start time is unknown, and `stopAt` is `null`
when no auto-stop is scheduled. Unlike the chat view this list is per workload (no merging), so there is no partly running state.
A request refused with `401` or `503` is not written to the audit log, because the audit events describe workload operations.

### `POST /api/v1/start`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a", "b"], "user": "ci-bot"}
```

`user` is optional (default `rest-api`). `duration` is optional (a Go duration such as `"3h"` or `"90m"`; empty
uses the workload default) and must be between `15m` and the workload's maximum, otherwise nothing is started and the
status is `400`. Returns `200` with `started`, `not_found` and `forbidden` lists, `stop_at` (RFC 3339 per started
workload with an auto-stop) and `refused` (workload to reason, for example a [GitOps conflict](#direct-actuator)),
`400` for invalid input, `403` when every requested workload is refused by the [authorization policy](#authorization),
`404` for an unknown `env` or when no workload was found, `409` when nothing started and the remaining workloads
were refused by the actuator.

### `POST /api/v1/extend`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a"], "duration": "1h"}
```

Adds `duration` (required, at least `15m`) to the `stopAt` of each running workload, checked like a start against
the `api` subject. Returns `200` with `extended` (workload to new stop time), `not_found`, `forbidden` and `failed`
(stopped workload, no auto-stop scheduled, or the maximum would be exceeded). `403` when everything was refused,
`404` for an unknown `env` or no matching workload, `409` when workloads exist but none could be extended, `400` for
invalid input.

### `/api/v1/reservations`

```json
{"env": "stg", "namespace": "games", "workloads": ["terraria"], "at": "2026-11-14T21:00:00+09:00", "duration": "3h"}
```

`POST` creates a [reservation](#reservations) as the `api` subject: `at` (RFC 3339, required) is when the workloads
should be Ready, `duration` (optional Go duration) the run length after it. Returns `201` with the reservation (`id`,
`state`, `at`, ...), `400` for invalid input (time in the past or more than 30 days ahead, duration out of range),
`403` when the policy refuses a workload, `404` for an unknown `env` or workload. `GET /api/v1/reservations?env=stg`
lists the reservations of an env (finished ones are kept for a day). `DELETE /api/v1/reservations/<id>?env=stg`
cancels a pending one (`404` when there is none).

### `POST /api/v1/stop-all`

Allowed only in `full` mode (`403` otherwise). Stops the workloads the `api` subject may stop in all managed
clusters (best effort); running workloads kept by the authorization policy are reported in `skipped`
per cluster and in `total_skipped`. Returns `200` with a per-cluster breakdown even when some clusters fail.

### Probes

`/livez` and `/healthz` return 200 once the process is up. `/readyz` returns 200 after the
initial sync of all clusters.

## KEDA external scaler

`actuator: keda-external` removes Prometheus from the path: KEDA connects to the controller over gRPC and the
controller pushes every start and stop immediately. One gRPC server serves all `keda-external` clusters.
It starts with the controller when at least one cluster uses it, so adding the first such cluster by hot
reload needs a restart. Enable the port in the chart with `externalScaler.enabled: true`
(`externalScaler.port`, default `9090`).

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: workload-knocker.workload-control.svc:9090
      cluster: stg            # clusters[].env (required)
      namespace: my-ns        # optional, defaults to the ScaledObject namespace
      workload: my-app        # optional, defaults to the scale target name
```

- A `ScaledObject` is managed when it has an `external-push` (or `external`) trigger whose `scalerAddress`
  contains `EXTERNAL_SCALER_ADDRESS_MATCH` and whose `cluster` equals `clusters[].env`.
- The workload is active while it is desired running: `IsActive` and the stream report that state, and the
  metric `workload_knocker_running` is `1` while running and `0` while stopped (target `1`).
- `StreamIsActive` pushes on stream start, on every start or stop, and every 30 seconds as a keepalive.
- The port has no authentication. Keep it cluster-internal (a ClusterIP Service). For TLS set
  `externalScaler.tls.secretName` to a `kubernetes.io/tls` Secret (cert-manager works) and give KEDA the CA
  with `caCert` (and `tlsClientCert` / `tlsClientKey` if you need them) in a `TriggerAuthentication`.
- When the controller restarts, KEDA keeps the last state until the stream reconnects. A call for a workload
  the controller does not know yet fails with `NotFound`, and KEDA applies the ScaledObject `fallback`.
  Set `fallback` (`failureThreshold`, `replicas`) so a long outage settles on a replica count you choose.
- The state still lives in the `workload-state` ConfigMap, so a restart restores it before the port opens.

Example: [`example/scaledobject-external.example.yaml`](../../example/scaledobject-external.example.yaml).

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
| `workload_knocker_auto_stops_total` | counter | `env`, `cluster`, `result` (`success`, `error`) |
| `workload_knocker_reservations` | gauge | `env`, `state` (`pending`, `started`, `failed`, `missed`, `cancelled`; finished ones are counted for a day) |
| `workload_knocker_reservation_executions_total` | counter | `env`, `result` (`success`, `partial`, `failed`, `missed`) |
| `workload_knocker_external_scaler_streams` | gauge | `cluster` (open `keda-external` push streams) |
| `workload_knocker_gitops_conflicts` | gauge | `env`, `cluster`, `namespace`, `workload` (`direct` actuator only; 1 = a GitOps manager owns `spec.replicas`) |

`operation` is `start`, `stop` or `stop_all`. `result` is `success`, `error` or `not_found`
(`error` is only reported by `stop_all`). User IDs and workload names are never used as labels
to keep cardinality bounded.
