# Repository layout

`internal/` is grouped by concern. A group holds one or more packages; a package keeps its Go
package name, so only the import path tells the group.

| Path | Contents |
|---|---|
| `cmd/server/` | Entry point (`main.go`), including the `--version` flag |
| `charts/workload-knocker/` | Helm chart, with `ci/` values used by lint and release checks |
| `example/` | Sample registry, policy, Helm values, RBAC and `.env` |
| `docs/en/`, `docs/ja/` | Documentation (English and Japanese) |
| `hack/` | Release and license check scripts |
| `.github/workflows/` | CI, release-please and release workflows |
| `slack-manifest.yaml` | Slack app manifest |

## `internal/`

| Group | Package | Contents |
|---|---|---|
| `app/` | `container/` | Dependency wiring |
| | `server/` | Lifecycle manager that starts and stops every component |
| | `reload/` | Hot reload of the registry and the policy |
| `config/` | | Environment variables and the cluster registry |
| `policy/` | `authz/` | Authorization policy parsing and evaluation |
| | `mode/` | `OPERATION_MODE` |
| | `runlimit/` | Run duration defaults, limits, parsing and extension rules |
| `workload/` | `model/` | Shared workload types and the store role interfaces |
| | `store/` | Per-cluster and multi-cluster workload state |
| | `rollout/` | Rollout progress tracking after Confirm (polling, state machine, per-cluster readers) |
| `cluster/` | `k8s/` | Cluster clients, EKS and kubeconfig auth (`k8s/auth/`), `ScaledObject` and scale target access, state ConfigMap |
| `actuator/` | | `Discoverer` / `Actuator` interfaces, the `keda-prometheus`, `keda-external` and `direct` implementations, `multi` (several actuators per cluster), the per-cluster `factory` and the `actuatortest` contract suite |
| `schedule/` | `reconcile/` | Background reconcile loop with per-tick hooks |
| | `periodic/` | Wires every hook the loop runs each interval (auto-stop, reservations) in run order |
| | `autostop/` | Hook that stops expired workloads and calls the stop reminder |
| | `reservation/` | One-off reservations: create, list, cancel, and the hook that reminds, executes and marks missed |
| `frontend/` | | Chat-independent start/stop flow shared by Slack, Discord and the start / extend API, and the shared chat lines rendered through each platform's `Markup` |
| | `discord/` | Discord bot (gateway, slash command, modals) |
| | `slack/` | Slack bot (Socket Mode, slash command, modals) |
| | `api/` | REST handlers (`/api/v1/start`, `/extend`, `/stop-all`, `/workloads`, `/reservations`) and bearer auth middleware |
| | `http/` | HTTP server for `/metrics`, the probes and the API |
| | `notify/` | Ready message templates and the optional notify channel (platform-neutral notifier, `Poster` adapters, Extend button contract) |
| `observability/` | `audit/` | Audit event schema and recorder (see [audit-log.md](audit-log.md)) |
| | `metrics/` | Prometheus metrics, including `workload_knocker_activation` |
| | `logging/` | slog setup: `timestamp` / `message` keys, `service.*` attributes |
| | `version/` | Build identity (`Version`, `Commit`, `BuildDate`) |
