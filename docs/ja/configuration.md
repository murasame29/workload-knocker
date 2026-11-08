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
| `AUTHZ_CONFIG_PATH` | 未設定 | 認可ポリシーファイル。未設定かつ `/etc/workload-knocker/authz.yaml` も無い場合は認可無効 (全員が全 workload を start/stop 可。起動時に info ログ 1 件)。設定時、または既定ファイルが存在する場合はパースできないと起動失敗 ([認可](#認可)) |
| `API_TOKEN` | 空 | `/api/v1/*` の Bearer トークン。空の場合 `/api/v1/*` は `503` (fail closed) |
| `SLACK_APP_TOKEN` | Slack 利用時のみ必須 | Slack App-level token (`xapp-...`)。いずれかの cluster が `frontends.slack` を設定したときだけ必要 |
| `SLACK_BOT_TOKEN` | Slack 利用時のみ必須 | Slack Bot token (`xoxb-...`)。いずれかの cluster が `frontends.slack` を設定したときだけ必要 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

`WORKLOAD_ENV` は非推奨で無視されます (警告ログのみ)。

## Cluster Registry

起動時に読み込み、以降はホットリロードされます ([ホットリロード](#ホットリロード))。

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
    frontends:
      slack:
        slashCommand: /workload-knocker-stg
  - env: local
    name: kind-local
    provider:
      type: kubeconfig
      kubeconfig:
        path: /etc/kubeconfig/kubeconfig
        context: kind-local                                             # 任意
    frontends:
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
| `clusters[].frontends.slack.slashCommand` | frontend ごと | Slack App に登録する一意の slash command |

各 cluster は `frontends` 配下に少なくとも 1 つの frontend を設定する必要があります。いずれかの cluster が設定した
frontend だけが有効になり、起動されます。

> **破壊的変更:** 従来の `clusters[].slack.slashCommand` は `clusters[].frontends.slack.slashCommand` に移動しました。
> 旧 `slack` キーは起動時にエラーになります。

## 認可

ポリシーファイルで、誰がどの workload を start/stop できるかを決めます。
[`example/authz.example.yaml`](../../example/authz.example.yaml) も参照してください。

```yaml
version: 1            # 必須。1 のみ
default: deny         # deny (既定) または allow。どのルールにも一致しないときに適用
rules:
  - env: stg                      # env の完全一致または glob。"*" は全 env
    namespaces: ["team-*"]        # glob (必須)
    workloads: ["checkout-*"]     # workload 名の glob。既定 ["*"]
    actions: [start]              # start / stop。既定は両方
    subjects:
      slack:
        users: [U01ALICE00]       # Slack ユーザー ID
        groups: [S01PLATFORM]     # Slack ユーザーグループ ID
      discord:
        users: ["123456789012345678"]
        roles: ["223456789012345678"]
      api: true                   # REST API 呼び出し元
```

- glob は Go の `path.Match` 構文 (`*` `?` `[a-z]`)。大文字小文字を区別し、`*` は `/` を跨ぎません。
- env・namespace・workload・action が一致し、かつ subject を列挙するルールが 1 つでもあれば許可です。
  **0.1.0 に deny ルールはありません**。広い allow から例外を除くことはできないため、allow は狭く書き `default: deny` を保ってください。
- merge された `foo-api` / `foo-app` / `foo-worker` は構成 workload ごとに判定します。
- 未知のキー、不正な glob、形式の違う ID (例: `users` にユーザーグループ ID)、subject のないルールは読み込み時に拒否します。
- ファイルは YAML でも JSON でも構いません。

### 適用箇所

| 対象 | 動作 |
|---|---|
| Slack の namespace / workload 一覧 | ユーザーが start または stop できるものだけ表示。1 件もなければ明確なメッセージを返す |
| Slack の Plan / Apply | start/stop を Plan 時と Apply 時に再判定。モーダルが送った値に関わらず、許可されない項目は理由付きで拒否 |
| `POST /api/v1/start` | subject `api` として workload ごとに判定。拒否分は `forbidden` に入り、1 件も start できず拒否があれば `403`。拒否は workload の存在を明かさない |
| `POST /api/v1/stop-all` | `api` が stop できる workload だけ停止。停止しなかった起動中 workload は cluster ごとの `skipped` と `total_skipped` に出る |

### チャットの動作モード

| `OPERATION_MODE` | チャット start | チャット stop |
|---|---|---|
| `full` | 可 | 可 |
| `scheduled-stop` | 可 | 不可。stop 項目は「停止は夜間ジョブが行う」旨のメッセージで拒否 |
| `off` | 可 | 可 |

モードはポリシーに加えて適用されます。API 側は [動作モードと夜間停止](#動作モードと夜間停止) を参照してください。

### subject の解決

| 取得元 | subject |
|---|---|
| Slack | ユーザー ID と所属ユーザーグループ。ポリシーが参照するグループだけを `usergroups.users.list` で取得し 60 秒キャッシュ。メンバー変更の反映に最大 1 分かかる |
| REST API | 固定の subject `api` |
| Discord | スキーマは `users` / `roles` を受け付け、subject 型も user + roles を持つ。Discord アダプタが追加された時点で供給される |

グループの取得には bot スコープ `usergroups:read` が必要です (`slack-manifest.yaml` に記載。このスコープ追加前に作成した App は再インストールが必要)。
`slack.groups` を使うルールがあるときだけ使われます。取得に失敗した場合は warning を出し、そのグループについてはユーザー ID だけで照合します。
fail open はしません。グループ経由でのみ許可されているユーザーは、取得に成功するまで拒否されます。

## ホットリロード

Cluster Registry と認可ポリシーは fsnotify で監視します。親ディレクトリを監視するため、エディタのアトミックな rename と
Kubernetes の ConfigMap 更新 (`..data` シンボリックリンクの差し替え) のどちらも検知します。イベントは 500 ms デバウンスします。
試行回数は `workload_knocker_config_reload_total{target,result}` で数えます。

- 有効な変更はアトミックに反映され、要約をログに出します。処理中のリクエストは旧設定のまま完了します。
- 不正な変更 (パース/検証エラー、ファイル削除) は拒否します。旧設定を維持し、エラーログと `result="error"` を記録します。
- Registry: cluster の追加・削除・変更に対応します。追加/変更された cluster は反映前に構築と bootstrap (各 30 秒) を行います。
  変更した cluster の bootstrap に失敗した場合は旧接続を維持して warning を出します。追加した cluster の bootstrap に失敗した場合は、
  起動時と同様に `cluster_reachable=0` のまま保持します。削除した cluster は slash command の解決から外れ、
  `workload_knocker_activation` と `workload_knocker_cluster_reachable` の系列も削除します。slash command は常に最新の Registry で解決します。
- 認可: 起動後に現れたポリシーファイルは認可を有効にします。ファイルを削除しても直前のポリシーを維持します。
- 再起動が必要な Registry 変更は明確なエラーで拒否し、旧設定を維持します。`controller.namespace` の変更、
  起動時に有効でなかった frontend の有効化、トークンなしでの有効化が該当します。
- 環境変数 (トークン、`OPERATION_MODE`、タイムアウト、パス) はホットリロードされません。

Kubernetes では ConfigMap の変更は kubelet の同期後、通常 1 分程度で Pod に届きます。chart は Registry とポリシーを
`/etc/workload-knocker` に 1 つの projected volume としてマウントし (`subPath` では更新されません)、どちらの ConfigMap にも
Pod テンプレートの checksum annotation を付けないため、変更で Pod は再起動しません。`kubectl rollout restart` が必要なのは上記の再起動必須ケースだけです。
このディレクトリは読み取り専用で chart が管理するため、kubeconfig など追加ファイルは別のパスにマウントしてください。

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

`user` は省略可 (既定 `rest-api`)。`200` で `started` / `not_found` / `forbidden` を返し、入力不正は `400`、
要求した workload がすべて [認可ポリシー](#認可) に拒否された場合は `403`、
未登録の `env` または 1 件も見つからない場合は `404` です。

### `POST /api/v1/stop-all`

`full` モードのみ可 (それ以外は `403`)。全 managed cluster のうち `api` subject が stop できる workload を停止します (best effort)。
認可ポリシーで止めなかった起動中 workload は cluster ごとの `skipped` と `total_skipped` に出ます。一部の cluster が失敗しても `200` で
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
| `workload_knocker_config_reload_total` | counter | `target` (`registry`, `authz`), `result` (`success`, `error`) |

`operation` は `start`、`stop`、`stop_all` のいずれかです。`result` は `success`、`error`、`not_found` のいずれかです
(`error` を返すのは `stop_all` のみ)。カーディナリティを抑えるため、ユーザー ID と workload 名はラベルに使いません。
