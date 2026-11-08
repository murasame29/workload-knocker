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
| `SLACK_APP_TOKEN` | required | Slack app-level token (`xapp-...`) |
| `SLACK_BOT_TOKEN` | required | Slack bot token (`xoxb-...`) |
| `NEW_RELIC_LICENSE_KEY` | empty | Enables New Relic APM when set |
| `NEW_RELIC_APP_NAME` | `workload-knocker` | New Relic application name |
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
    slack:
      slashCommand: /workload-knocker-stg
  - env: local
    name: kind-local
    provider:
      type: kubeconfig
      kubeconfig:
        path: /etc/workload-knocker/kubeconfig
        context: kind-local                                             # optional
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
| `clusters[].slack.slashCommand` | yes | Unique slash command registered in the Slack app |

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

## HTTP API

Unauthenticated. Restrict network access.

### `POST /api/v1/start`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a", "b"], "user": "ci-bot"}
```

`user` is optional (default `rest-api`). Returns `200` with `started` and `not_found` lists,
`400` for invalid input, `404` for an unknown `env` or when no workload was found.

### `POST /api/v1/stop-all`

Stops workloads in all managed clusters (best effort). Returns `200` with a per-cluster
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
