# workload-knocker

Start and stop KEDA-scaled Kubernetes workloads on demand from Slack or Discord.

[Japanese](docs/ja/configuration.md) | [Configuration](docs/en/configuration.md) | [GitOps](docs/en/gitops.md) | [Repository layout](docs/en/repository-layout.md)

workload-knocker keeps dev and staging workloads at zero until someone needs them. A
Slack or Discord slash command lists the `ScaledObject`s of a cluster. Selecting a
workload makes the controller emit the Prometheus gauge `workload_knocker_activation`, and KEDA
scales the workload up from that metric through its Prometheus trigger. One controller can manage several clusters (EKS or any cluster reachable
through a kubeconfig). Clusters without KEDA and Prometheus can use the `direct` actuator, which scales annotated
Deployments and StatefulSets itself (see [Direct actuator](docs/en/configuration.md#direct-actuator)).

```
Slack / Discord  <->  workload-knocker  --- EKS / kubeconfig --->  managed cluster A, B, ...
               |  /metrics                                 ScaledObject (Prometheus trigger)
               +-----------------> Prometheus <----------- KEDA
```

## How it works

- The controller loads a cluster registry (YAML), hot reloads it on change, and lists every
  `ScaledObject` whose Prometheus trigger queries `workload_knocker_activation`.
- The Discord slash command (`frontends.discord.command`) has two subcommands. `/<command> start` opens
  a modal with one checkbox group per namespace (select menus beyond 5 namespaces or 10 entries) and a plan
  preview with Confirm/Cancel; see [Discord](docs/en/configuration.md#discord).
  The bare `/<command>` no longer works (breaking change).
- The Slack slash command (`frontends.slack.slashCommand`) opens a modal. Workloads that differ only by an `api` / `app` / `worker`
  suffix are shown as one checkbox.
- `/<command> status` (Discord subcommand, Slack argument) lists the workloads the user may operate with their state,
  starter and stop time, plus Extend 1h and Stop buttons; `GET /api/v1/workloads` returns the same data as JSON.
  See [Status](docs/en/configuration.md#status).
- A `workload-knocker.io/ready-message` annotation on a ScaledObject adds a hint to the started
  line, and an optional `notifyChannel` posts start, ready and stop messages publicly; see
  [Ready message and notify channel](docs/en/configuration.md#ready-message-and-notify-channel).
- Starting a workload sets `workload_knocker_activation{env,cluster,namespace,workload} = 1`.
  Stopping sets it to 0. The requested state is stored in a `workload-state` ConfigMap in each
  managed cluster, so it survives restarts.
- `POST /api/v1/start`, `POST /api/v1/extend` and `POST /api/v1/stop-all` provide the same operations over HTTP.
  See [HTTP API](docs/en/configuration.md#http-api).
- A start can carry a run duration: the workload is stopped automatically (also across restarts) and can be
  extended. See [Run duration](docs/en/configuration.md#run-duration).
- Every start and stop writes a structured audit event to stdout; see [Audit log](docs/en/audit-log.md).

Without Prometheus, set `actuator: keda-external` on a cluster and use KEDA's `external-push` trigger; the
controller pushes starts and stops over gRPC. See [KEDA external scaler](docs/en/configuration.md#keda-external-scaler).
`actuators: [keda-external, direct]` combines actuators behind one command; each workload uses the actuator that
found it. See [Several actuators](docs/en/configuration.md#several-actuators).

By default your `ScaledObject` needs a Prometheus trigger such as:

```yaml
triggers:
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      query: workload_knocker_activation{env="stg",namespace="my-ns",workload="my-app"}
      threshold: "1"
```

## Install

```bash
helm install workload-knocker oci://ghcr.io/murasame29/charts/workload-knocker \
  --namespace workload-control --create-namespace \
  --values values.yaml
```

1. Create a Slack app from [`slack-manifest.yaml`](slack-manifest.yaml) (Socket Mode) and store
   the app-level and bot tokens in a Secret (`app-token`, `bot-token`).
   For Discord, create a bot, invite it with the `applications.commands` and `bot` scopes, and store
   the bot token in a Secret (`bot-token`, `discord.secretName`). The bot needs no message
   permissions unless you set a `notifyChannel`: then grant View Channel and Send Messages in that channel.
2. Write the cluster registry in `config.inline` ([`example/values.example.yaml`](example/values.example.yaml)).
3. Grant the controller access to each managed cluster
   ([`example/managed-cluster-rbac.yaml`](example/managed-cluster-rbac.yaml)).
4. Restrict who may start or stop what with an authorization policy
   ([`example/authz.example.yaml`](example/authz.example.yaml), `authz.enabled` in the chart).
   Without a policy file, authorization is disabled and everyone may do everything.

See [docs/en/configuration.md](docs/en/configuration.md) for every option, the HTTP API, and metrics.
If Argo CD or Flux manages the workloads, read [GitOps](docs/en/gitops.md) first.

## Operation modes

`OPERATION_MODE` (chart value `mode`) controls who may stop workloads:

| Mode | API start | API stop / stop-all | Chat stop |
|---|---|---|---|
| `full` (default) | yes | yes | yes |
| `scheduled-stop` | yes | no (`403`) | no; the nightly CronJob (`nightlyStop`) stops workloads |
| `off` | yes | no (`403`) | yes |

Details and the `nightlyStop` values: [Operation modes](docs/en/configuration.md#operation-modes-and-nightly-stop).

## Security

`/api/v1/*` requires `Authorization: Bearer <API_TOKEN>`. If `API_TOKEN` is unset the API
responds `503` (fail closed). `OPERATION_MODE` limits which operations the API accepts. Still
restrict network access to the Service, and do not expose it publicly.

The authorization policy decides, per env, namespace, workload and action, which Slack users and
usergroups, Discord users and roles, and the API caller may start or stop. Chat lists only what the user may operate and
re-checks every change on apply. Without a policy file every chat user and the API caller may do
everything. The registry and the policy are hot reloaded; an invalid file is
rejected and the previous config stays active. See
[docs/en/configuration.md](docs/en/configuration.md#authorization).

## Development

```bash
make test           # go vet + go test -race
make test-property  # property-based tests
make docker-build
go run ./cmd/server --version
```

See [CONTRIBUTING.md](CONTRIBUTING.md), [docs/en/repository-layout.md](docs/en/repository-layout.md) and
[docs/en/release.md](docs/en/release.md). Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
