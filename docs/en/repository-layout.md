# Repository layout

| Path | Contents |
|---|---|
| `cmd/server/` | Entry point (`main.go`), including the `--version` flag |
| `internal/api/` | REST handlers (`/api/v1/start`, `/api/v1/stop-all`) and bearer auth middleware |
| `internal/audit/` | Audit event schema and recorder (see [audit-log.md](audit-log.md)) |
| `internal/authz/` | Authorization policy parsing and evaluation |
| `internal/config/` | Environment variables and the cluster registry |
| `internal/container/` | Dependency wiring |
| `internal/frontend/` | Chat-independent start/stop flow shared by Slack and Discord |
| `internal/k8s/` | Cluster clients, EKS and kubeconfig auth, `ScaledObject` access, state ConfigMap |
| `internal/logging/` | slog setup: `timestamp` / `message` keys, `service.*` attributes |
| `internal/metrics/` | Prometheus metrics, including `workload_knocker_activation` |
| `internal/mode/` | `OPERATION_MODE` |
| `internal/model/` | Shared workload types |
| `internal/reconcile/` | Background reconcile loop with per-tick hooks |
| `internal/reload/` | Hot reload of the registry and the policy |
| `internal/server/` | Lifecycle manager, HTTP server, Slack and Discord bots |
| `internal/store/` | Per-cluster and multi-cluster workload state |
| `internal/version/` | Build identity (`Version`, `Commit`, `BuildDate`) |
| `charts/workload-knocker/` | Helm chart, with `ci/` values used by lint and release checks |
| `example/` | Sample registry, policy, Helm values, RBAC and `.env` |
| `docs/en/`, `docs/ja/` | Documentation (English and Japanese) |
| `hack/` | Release and license check scripts |
| `.github/workflows/` | CI, release-please and release workflows |
| `slack-manifest.yaml` | Slack app manifest |
