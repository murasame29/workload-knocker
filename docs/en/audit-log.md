# Audit log

Every start and stop emits one audit event per workload to stdout (JSON). The message is always
`workload operation`, so the events can be selected without parsing other log lines.

```json
{"timestamp":"2026-11-08T12:00:00.123Z","level":"INFO","message":"workload operation",
 "service.name":"workload-knocker","service.version":"0.2.0",
 "event.name":"workload.start","event.outcome":"success",
 "workload_knocker.source":"discord","workload_knocker.request_id":"4f2a9c0e7b1d4c3a8e5f6a7b8c9d0e1f",
 "enduser.id":"discord:123456789012345678",
 "k8s.cluster.name":"flowers","k8s.namespace.name":"terraria",
 "workload.name":"terraria","workload.kind":"StatefulSet"}
```

## Schema

| Key | Values / format |
|---|---|
| `event.name` | `workload.start`, `workload.stop`, `workload.stop_all`, `workload.extend`, `workload.auto_stop`. Reserved for upcoming features: `reservation.create`, `reservation.cancel`, `reservation.execute` |
| `event.outcome` | `success`; `failure` (WARN, e.g. the workload was not found or its cluster failed); `denied` (authorization policy or operation mode refused it) |
| `workload_knocker.source` | `discord`, `slack`, `api`, `nightly-stop`, `scheduler` (auto-stop) |
| `workload_knocker.request_id` | Random 128-bit hex. In chat it is created at the plan preview and reused by the confirm; for the API it is created per request |
| `enduser.id` | `discord:<user id>`, `slack:<user id>` or `api` |
| `k8s.cluster.name` | Cluster name from the registry |
| `k8s.namespace.name`, `workload.name`, `workload.kind` | Target workload. Omitted when the event is not tied to a workload (a rejected API request) or the kind is unknown |
| `workload_knocker.duration_seconds`, `workload_knocker.stop_at` | Run duration of a start or the added time of an extension; the stop time. Omitted when there is none |
| `error.type`, `error.message` | Only for `failure` |

Levels are INFO for `success` and `denied`, WARN for `failure`. Tokens and message content are never logged.

Notes:

- Denied events appear for plan lines refused at preview (chat), for the same check repeated at apply, and for API requests.
- An API request refused with 401 or 403 (bad token, or `stop-all` in a mode that forbids it) emits one `denied` event without workload fields and with `enduser.id` `api`.
- `POST /api/v1/stop-all` emits one event per workload that was running when the request arrived. Running workloads kept by the policy are `denied`; workloads of a cluster that failed are `failure`.
- The nightly CronJob sends `X-Workload-Knocker-Source: nightly-stop`, which sets the source to `nightly-stop` (`enduser.id` stays `api`). Any other value is recorded as `api`. The header only labels the event and grants nothing.

## Common keys of all log lines

Every log line, audit or not, uses `timestamp` and `message` instead of slog's `time` and `msg`, and
carries `service.name` (`workload-knocker`) and `service.version`.

## Example queries

jq:

```sh
kubectl logs deploy/workload-knocker | jq -c 'select(.message == "workload operation" and .["event.outcome"] == "denied")'
```

NRQL:

```sql
SELECT timestamp, `enduser.id`, `event.name`, `workload.name` FROM Log
WHERE message = 'workload operation' AND `event.outcome` = 'denied' SINCE 1 day ago
```

LogQL:

```logql
{app_kubernetes_io_name="workload-knocker"} | json | message = "workload operation" | event_outcome = "denied"
```

One operation from preview to confirm:

```sh
kubectl logs deploy/workload-knocker | jq -c 'select(.["workload_knocker.request_id"] == "4f2a9c0e7b1d4c3a8e5f6a7b8c9d0e1f")'
```

## Upgrading from 0.1.x

Log parsers that read `time` or `msg` must read `timestamp` or `message`. The previous ad-hoc
operation lines (`workload started`, `workload stopped`, `all workloads stopped`,
`workload operation rejected`, `workload started via REST API`, `start refused by authorization policy`,
`start request completed`, `stop-all completed`, `discord plan applied`) are replaced by the audit events.
