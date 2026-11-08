# 設定

## 環境変数

| 変数 | デフォルト | 説明 |
|---|---|---|
| `HTTP_PORT` | `8080` | `/metrics`、ヘルスチェック、`/api/v1/*` の HTTP ポート |
| `CLUSTER_CONFIG_PATH` | `/etc/workload-knocker/config.yaml` | Cluster Registry ファイル |
| `CONTROLLER_NAMESPACE` | `workload-control` | managed cluster 上の `workload-state` ConfigMap の namespace |
| `CLUSTER_RECONCILE_TIMEOUT` | `2s` | Slack コマンド受信時の Reconcile タイムアウト |
| `STOP_ALL_TIMEOUT` | `10s` | `/api/v1/stop-all` の cluster ごとのタイムアウト |
| `SHUTDOWN_TIMEOUT` | `5s` | コンポーネントごとの graceful shutdown タイムアウト |
| `OPERATION_MODE` | `full` | `full` / `scheduled-stop` / `off`。それ以外は起動時エラー ([動作モード](#動作モードと夜間停止)) |
| `API_TOKEN` | 空 | `/api/v1/*` の Bearer トークン。空の場合 `/api/v1/*` は `503` (fail closed) |
| `SLACK_APP_TOKEN` | 必須 | Slack App-level token (`xapp-...`) |
| `SLACK_BOT_TOKEN` | 必須 | Slack Bot token (`xoxb-...`) |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

`WORKLOAD_ENV` は非推奨で無視されます (警告ログのみ)。

## Cluster Registry

起動時に一度だけ読み込みます。変更したら Pod を再起動してください。

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
        assumeRoleArn: arn:aws:iam::123456789012:role/cross-account   # 任意
    slack:
      slashCommand: /workload-knocker-stg
  - env: local
    name: kind-local
    provider:
      type: kubeconfig
      kubeconfig:
        path: /etc/workload-knocker/kubeconfig
        context: kind-local                                             # 任意
    slack:
      slashCommand: /workload-knocker-local
```

| フィールド | 必須 | 説明 |
|---|---|---|
| `controller.namespace` | ◯ | state ConfigMap を置く namespace |
| `clusters[].env` | ◯ | 一意の識別子。メトリクスの `env` ラベルに使う |
| `clusters[].name` | ◯ | cluster 名。`eks:DescribeCluster` の引数と `cluster` ラベル |
| `clusters[].provider.type` | ◯ | `eks` または `kubeconfig` |
| `clusters[].provider.eks.region` | eks | AWS リージョン |
| `clusters[].provider.eks.assumeRoleArn` | | cross-account 時に AssumeRole する Role |
| `clusters[].provider.kubeconfig.path` | kubeconfig | kubeconfig のパス |
| `clusters[].provider.kubeconfig.context` | | context。省略時は current-context |
| `clusters[].slack.slashCommand` | ◯ | Slack App に登録する一意の slash command |

## managed cluster へのアクセス

EKS では controller に IAM Role (IRSA) を付与し、その Role を Access Entry に
`kubernetes_groups: ["workload-knocker"]` で登録して、
[`example/managed-cluster-rbac.yaml`](../../example/managed-cluster-rbac.yaml) を適用します。
必要な権限は `scaledobjects` の `get/list/watch` と、`controller.namespace` の ConfigMap の
`get/create/update` です。

## Slack の workload モーダル

- namespace と workload は a→z 順です。
- `foo-api` / `foo-app` / `foo-worker` のうち 2 件以上あれば 1 つの `foo` チェックボックスに
  merge されます。一部だけ起動中なら 🟡 でチェック ON になります。
- Slack の制限 (checkbox 10 件 / モーダル 100 block) により merge 後 90 エントリまで表示します。
  非表示分は Plan から除外されるため、意図せず停止されることはありません。

## 動作モードと夜間停止

Helm chart は controller の環境変数 `OPERATION_MODE` と `API_TOKEN` を設定します。

| `mode` | API start | API stop / stop-all | `nightlyStop` |
| --- | --- | --- | --- |
| `full` (既定) | 可 | 可 | 任意 |
| `scheduled-stop` | 可 | 不可 (`403`。夜間 CronJob が停止) | 必須 (`enabled: true`) |
| `off` | 可 | 不可 | 有効化不可 |

`/api/v1/*` を無認証にしないため、どのモードでも `api.tokenSecret.name` は必須です。
インストール前に Secret を作成します。

```sh
kubectl create secret generic workload-knocker-api \
  --namespace workload-knocker \
  --from-literal=api-token='<random-token>'
```

`nightlyStop` は、クラスタ内 Service へ `Authorization: Bearer <token>` 付きで
`POST /api/v1/stop-all` を送る CronJob を作成し、2xx 以外は失敗として扱います。

| 値 | 既定 |
| --- | --- |
| `nightlyStop.enabled` | `false` |
| `nightlyStop.schedule` / `timeZone` | `0 22 * * *` / `Asia/Tokyo` |
| `nightlyStop.suspend` | `false` |
| `nightlyStop.startingDeadlineSeconds` | `300` |
| `nightlyStop.concurrencyPolicy` | `Forbid` |
| `nightlyStop.successfulJobsHistoryLimit` / `failedJobsHistoryLimit` | `1` / `3` |
| `nightlyStop.backoffLimit` | `2` |
| `nightlyStop.requestTimeoutSeconds` / `curlRetries` | `60` / `3` |
| `nightlyStop.image` | `curlimages/curl` (digest 固定) |

不正な組み合わせ (未知の `mode`、CronJob なしの `scheduled-stop`、CronJob ありの `off`、
`api.tokenSecret.name` が空) は `helm template` / `helm install` で失敗します。

## HTTP API

### 認証

`/api/v1/*` には `Authorization: Bearer <token>` が必要です (token は controller の `API_TOKEN`)。
`/metrics`、`/livez`、`/healthz`、`/readyz` は認証不要です。

```sh
curl -X POST "http://workload-knocker.workload-knocker.svc:8080/api/v1/start" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"env":"stg","namespace":"my-namespace","workloads":["a"]}'
```

チェック順は 認証 → 動作モード → 入力検証 です。

| ステータス | 意味 |
|---|---|
| `401` | token がない、または不正 (`WWW-Authenticate: Bearer`) |
| `403` | 現在の `OPERATION_MODE` では許可されない操作 |
| `503` | `API_TOKEN` 未設定のため API は無効 |


### `POST /api/v1/start`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a", "b"], "user": "ci-bot"}
```

`user` は省略可 (既定 `rest-api`)。`200` で `started` / `not_found` を返し、入力不正は `400`、
未登録の `env` または 1 件も見つからない場合は `404` です。

### `POST /api/v1/stop-all`

`full` モードのみ可 (それ以外は `403`)。全 managed cluster の workload を停止します (best effort)。一部の cluster が失敗しても `200` で
cluster ごとの内訳を返します。

### プローブ

`/livez` と `/healthz` はプロセス起動後に 200。`/readyz` は全 cluster の初期同期後に 200 になります。

## メトリクス

| メトリクス | 型 | ラベル |
|---|---|---|
| `workload_knocker_activation` | gauge | `env`, `cluster`, `namespace`, `workload` |
| `workload_knocker_cluster_reachable` | gauge | `cluster` |
| `workload_knocker_token_refresh_total` | counter | `cluster`, `result` |
| `workload_knocker_operations_total` | counter | `env`, `cluster`, `operation`, `result` |
| `workload_knocker_operation_duration_seconds` | histogram | `env`, `cluster`, `operation` |

`operation` は `start`、`stop`、`stop_all` のいずれかです。`result` は `success`、`error`、`not_found` のいずれかです
(`error` を返すのは `stop_all` のみ)。カーディナリティを抑えるため、ユーザー ID と workload 名はラベルに使いません。
