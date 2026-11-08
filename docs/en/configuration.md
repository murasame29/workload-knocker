# Configuration

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `HTTP_PORT` | `8080` | HTTP port for `/metrics`, health probes and `/api/v1/*` |
| `CLUSTER_CONFIG_PATH` | `/etc/workload-knocker/config.yaml` | Cluster registry file |
| `CONTROLLER_NAMESPACE` | `workload-control` | Namespace of the `workload-state` ConfigMap in managed clusters |
| `CLUSTER_RECONCILE_TIMEOUT` | `2s` | Reconcile timeout when a Slack command arrives |
| `STOP_ALL_TIMEOUT` | `10s` | Per-cluster timeout for `/api/v1/stop-all` |
| `SHUTDOWN_TIMEOUT` | `5s` | Graceful shutdown timeout per component |
| `OPERATION_MODE` | `full` | `full`, `scheduled-stop` or `off`; any other value fails at startup (see [Operation modes](#operation-modes-and-nightly-stop)) |
| `API_TOKEN` | empty | Bearer token for `/api/v1/*`. When empty, `/api/v1/*` responds `503` (fail closed) |
| `SLACK_APP_TOKEN` | required with Slack | Slack app-level token (`xapp-...`); required only when a cluster configures `frontends.slack` |
| `SLACK_BOT_TOKEN` | required with Slack | Slack bot token (`xoxb-...`); required only when a cluster configures `frontends.slack` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

`WORKLOAD_ENV` is deprecated and ignored (a warning is logged).

## Cluster registry

The registry is read once at startup. Restart the Pod after changing it.

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
        path: /etc/workload-knocker/kubeconfig
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

Each cluster must configure at least one entry under `frontends`. A frontend is enabled when at least
one cluster configures it, and only enabled frontends are started.

> **Breaking change:** the former `clusters[].slack.slashCommand` moved to
> `clusters[].frontends.slack.slashCommand`. The old `slack` key is rejected at startup.

## Managed cluster access

For EKS, run the controller with an IAM role (IRSA), register that role in the cluster's
Access Entry with `kubernetes_groups: ["workload-knocker"]`, and apply
[`example/managed-cluster-rbac.yaml`](../../example/managed-cluster-rbac.yaml). The controller
needs `get/list/watch` on `scaledobjects` and `get/create/update` on ConfigMaps in
`controller.namespace`.

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
`Authorization: Bearer <token>` and fails on any non-2xx response.

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

`user` is optional (default `rest-api`). Returns `200` with `started` and `not_found` lists,
`400` for invalid input, `404` for an unknown `env` or when no workload was found.

### `POST /api/v1/stop-all`

Allowed only in `full` mode (`403` otherwise). Stops workloads in all managed clusters (best effort). Returns `200` with a per-cluster
breakdown even when some clusters fail.

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

`operation` is `start`, `stop` or `stop_all`. `result` is `success`, `error` or `not_found`
(`error` is only reported by `stop_all`). User IDs and workload names are never used as labels
to keep cardinality bounded.
