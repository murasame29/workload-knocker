# workload-knocker

Start and stop KEDA-scaled Kubernetes workloads on demand from Slack.

[日本語](docs/ja/configuration.md)

workload-knocker keeps dev and staging workloads at zero until someone needs them. A
Slack slash command opens a modal listing the `ScaledObject`s of a cluster. Selecting a
workload makes the controller emit a Prometheus gauge, and KEDA scales the workload up
from that metric. One controller can manage several clusters (EKS or any cluster reachable
through a kubeconfig).

```
Slack  <->  workload-knocker  --- EKS / kubeconfig --->  managed cluster A, B, ...
               |  /metrics                                 ScaledObject (Prometheus trigger)
               +-----------------> Prometheus <----------- KEDA
```

## How it works

- The controller loads a cluster registry (YAML) at startup and lists every
  `ScaledObject` whose Prometheus trigger queries `workload_knocker_activation`.
- `/<slashCommand>` opens a modal. Workloads that differ only by an `api` / `app` / `worker`
  suffix are shown as one checkbox.
- Starting a workload sets `workload_knocker_activation{env,cluster,namespace,workload} = 1`.
  Stopping sets it to 0. The requested state is stored in a `workload-state` ConfigMap in each
  managed cluster, so it survives restarts.
- `POST /api/v1/start` and `POST /api/v1/stop-all` provide the same operations over HTTP.

Your `ScaledObject` needs a Prometheus trigger such as:

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
2. Write the cluster registry in `config.inline` ([`example/values.example.yaml`](example/values.example.yaml)).
3. Grant the controller access to each managed cluster
   ([`example/managed-cluster-rbac.yaml`](example/managed-cluster-rbac.yaml)).

See [docs/en/configuration.md](docs/en/configuration.md) for every option, the HTTP API, and metrics.

## Security

`/api/v1/*` requires `Authorization: Bearer <API_TOKEN>`. If `API_TOKEN` is unset the API
responds `503` (fail closed). `OPERATION_MODE` limits which operations the API accepts. Still
restrict network access to the Service, and do not expose it publicly.

## Development

```bash
make test           # go vet + go test -race
make test-property  # property-based tests
make docker-build
```

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
