# 監査ログ

start / stop の操作ごとに、ワークロード 1 件につき 1 つの監査イベントを stdout に JSON で出力します。
メッセージは常に `workload operation` なので、他のログを解析せずに抽出できます。

```json
{"timestamp":"2026-11-08T12:00:00.123Z","level":"INFO","message":"workload operation",
 "service.name":"workload-knocker","service.version":"0.2.0",
 "event.name":"workload.start","event.outcome":"success",
 "workload_knocker.source":"discord","workload_knocker.request_id":"4f2a9c0e7b1d4c3a8e5f6a7b8c9d0e1f",
 "enduser.id":"discord:123456789012345678",
 "k8s.cluster.name":"flowers","k8s.namespace.name":"terraria",
 "workload.name":"terraria","workload.kind":"StatefulSet"}
```

## スキーマ

| キー | 値 / 形式 |
|---|---|
| `event.name` | `workload.start`、`workload.stop`、`workload.stop_all`。今後の機能用に予約: `workload.extend`、`workload.auto_stop`、`reservation.create`、`reservation.cancel`、`reservation.execute` |
| `event.outcome` | `success`、`failure` (WARN。ワークロードが見つからない、クラスタが失敗した等)、`denied` (認可ポリシーまたは動作モードが拒否) |
| `workload_knocker.source` | `discord`、`slack`、`api`、`nightly-stop`。`scheduler` は予約 |
| `workload_knocker.request_id` | ランダムな 128 bit の 16 進数。チャットではプラン確認で生成され、Confirm で再利用されます。API ではリクエストごとに生成されます |
| `enduser.id` | `discord:<user id>`、`slack:<user id>`、または `api` |
| `k8s.cluster.name` | レジストリ上のクラスタ名 |
| `k8s.namespace.name`、`workload.name`、`workload.kind` | 対象ワークロード。ワークロードに紐づかないイベント (拒否された API リクエスト) や kind 不明の場合は出力されません |
| `workload_knocker.duration_seconds`、`workload_knocker.stop_at` | 期間を指定する機能用に予約。現在は出力されません |
| `error.type`、`error.message` | `failure` のときのみ |

レベルは `success` と `denied` が INFO、`failure` が WARN です。トークンやメッセージ本文は記録しません。

補足:

- `denied` は、チャットのプラン確認で拒否された行、Apply 時の同じ再チェック、API リクエストで出力されます。
- 401 / 403 で拒否された API リクエスト (トークン不正、または動作モードが禁止する `stop-all`) は、ワークロード項目なし・`enduser.id` が `api` の `denied` を 1 件出力します。
- `POST /api/v1/stop-all` は、リクエスト到着時に起動中だったワークロードごとに 1 件出力します。ポリシーで停止されなかったものは `denied`、クラスタが失敗したものは `failure` です。
- 夜間停止の CronJob は `X-Workload-Knocker-Source: nightly-stop` を送り、source が `nightly-stop` になります (`enduser.id` は `api` のまま)。他の値は `api` として記録されます。このヘッダーはラベルのみで、権限は与えません。

## 全ログ行に共通するキー

監査イベントかどうかにかかわらず、すべてのログ行は slog 標準の `time` / `msg` ではなく
`timestamp` / `message` を使い、`service.name` (`workload-knocker`) と `service.version` を持ちます。

## クエリ例

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

プラン確認から Confirm までの 1 操作:

```sh
kubectl logs deploy/workload-knocker | jq -c 'select(.["workload_knocker.request_id"] == "4f2a9c0e7b1d4c3a8e5f6a7b8c9d0e1f")'
```

## 0.1.x からの移行

`time` / `msg` を読むログパーサーは `timestamp` / `message` に変更してください。従来の個別の操作ログ
(`workload started`、`workload stopped`、`all workloads stopped`、`workload operation rejected`、
`workload started via REST API`、`start refused by authorization policy`、`start request completed`、
`stop-all completed`、`discord plan applied`) は監査イベントに置き換わりました。
