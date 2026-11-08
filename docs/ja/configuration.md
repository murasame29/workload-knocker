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
| `SLACK_APP_TOKEN` | 必須 | Slack App-level token (`xapp-...`) |
| `SLACK_BOT_TOKEN` | 必須 | Slack Bot token (`xoxb-...`) |
| `NEW_RELIC_LICENSE_KEY` | 空 | 設定すると New Relic APM を有効化 |
| `NEW_RELIC_APP_NAME` | `workload-knocker` | New Relic の Application 名 |
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

## HTTP API

認証はありません。ネットワークで到達範囲を制限してください。

### `POST /api/v1/start`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a", "b"], "user": "ci-bot"}
```

`user` は省略可 (既定 `rest-api`)。`200` で `started` / `not_found` を返し、入力不正は `400`、
未登録の `env` または 1 件も見つからない場合は `404` です。

### `POST /api/v1/stop-all`

全 managed cluster の workload を停止します (best effort)。一部の cluster が失敗しても `200` で
cluster ごとの内訳を返します。

### プローブ

`/livez` と `/healthz` はプロセス起動後に 200。`/readyz` は全 cluster の初期同期後に 200 になります。

## メトリクス

| メトリクス | 型 | ラベル |
|---|---|---|
| `workload_knocker_activation` | gauge | `env`, `cluster`, `namespace`, `workload` |
| `workload_knocker_cluster_reachable` | gauge | `cluster` |
| `workload_knocker_token_refresh_total` | counter | `cluster`, `result` |
