# 設定

## 環境変数

| 変数 | デフォルト | 説明 |
|---|---|---|
| `HTTP_PORT` | `8080` | `/metrics`、ヘルスチェック、`/api/v1/*` の HTTP ポート |
| `CLUSTER_CONFIG_PATH` | `/etc/workload-knocker/config.yaml` | Cluster Registry ファイル |
| `CONTROLLER_NAMESPACE` | `workload-control` | managed cluster 上の `workload-state` ConfigMap の namespace |
| `CLUSTER_RECONCILE_TIMEOUT` | `2s` | チャット (Slack / Discord) コマンド受信時の Reconcile タイムアウト |
| `RECONCILE_INTERVAL` | `1m` | 全 cluster の ScaledObject をバックグラウンドで再同期する間隔。cluster ごとに `CLUSTER_RECONCILE_TIMEOUT` で打ち切ります。`0` で無効、負の値は起動時エラー。Chart: `settings.reconcileInterval` |
| `STOP_ALL_TIMEOUT` | `10s` | `/api/v1/stop-all` の cluster ごとのタイムアウト |
| `ROLLOUT_POLL_INTERVAL` | `5s` | Confirm 後に scale target をポーリングする間隔 (`1s` 以上) |
| `ROLLOUT_WATCH_TIMEOUT` | `10m` | rollout の進捗表示を打ち切り、最後に観測した理由を表示するまでの時間。ポーリング間隔より大きく `15m` 未満 (Discord の interaction token の寿命) である必要があり、範囲外は起動時にエラーになります |
| `DEFAULT_RUN_DURATION` | `0` | 時間を指定しない start の自動停止までの時間。`0` は自動停止なし (従来の動作)。`0` か `15m` 以上で、`MAX_RUN_DURATION` を超えられません。Chart: `settings.defaultRunDuration` |
| `MAX_RUN_DURATION` | `24h` | 実行時間、および延長後の残り時間の上限。`15m` 以上。Chart: `settings.maxRunDuration` |
| `STOP_REMINDER` | `15m` | 自動停止の何分前にリマインドするか。`0` で無効。配信には通知チャンネルが必要です ([実行時間](#実行時間))。Chart: `settings.stopReminder` |
| `SHUTDOWN_TIMEOUT` | `5s` | コンポーネントごとの graceful shutdown タイムアウト |
| `OPERATION_MODE` | `full` | `full` / `scheduled-stop` / `off`。それ以外は起動時エラー ([動作モード](#動作モードと夜間停止)) |
| `AUTHZ_CONFIG_PATH` | 未設定 | 認可ポリシーファイル。未設定かつ `/etc/workload-knocker/authz.yaml` も無い場合は認可無効 (全員が全 workload を start/stop 可。起動時に info ログ 1 件)。設定時、または既定ファイルが存在する場合はパースできないと起動失敗 ([認可](#認可)) |
| `API_TOKEN` | 空 | `/api/v1/*` の Bearer トークン。空の場合 `/api/v1/*` は `503` (fail closed) |
| `SLACK_APP_TOKEN` | Slack 利用時のみ必須 | Slack App-level token (`xapp-...`)。いずれかの cluster が `frontends.slack` を設定したときだけ必要 |
| `SLACK_BOT_TOKEN` | Slack 利用時のみ必須 | Slack Bot token (`xoxb-...`)。いずれかの cluster が `frontends.slack` を設定したときだけ必要 |
| `DISCORD_BOT_TOKEN` | Discord 利用時のみ必須 | Discord Bot token。いずれかの cluster が `frontends.discord` を設定したときだけ必要 |
| `DISCORD_GUILD_ID` | 空 | Guild (サーバー) ID。設定するとその guild にコマンドを登録し即時反映されます。空ならグローバル登録 (反映に時間がかかることがあります) |
| `EXTERNAL_SCALER_PORT` | `9090` | KEDA external scaler の gRPC ポート。`actuator: keda-external` の cluster がある間だけ待ち受ける。Chart: `externalScaler.port` |
| `EXTERNAL_SCALER_TLS_CERT` / `EXTERNAL_SCALER_TLS_KEY` | 空 | このポートで TLS を有効にする証明書と鍵のパス (両方指定するか、どちらも指定しない。ハンドシェイクごとに再読込)。Chart: `externalScaler.tls.secretName` |
| `EXTERNAL_SCALER_ADDRESS_MATCH` | `workload-knocker` | trigger の `scalerAddress` にこの文字列を含むものを担当する。Chart は Service 名を設定する |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`。ログは `timestamp` / `message` キーの JSON です ([監査ログ](audit-log.md)) |

`WORKLOAD_ENV` は非推奨で無視されます (警告ログのみ)。

## バージョン

`workload-knocker --version` (または `-version`) は、設定を読み込む前に次の 1 行を出力して終了します。

```text
workload-knocker version=0.1.0 commit=<sha> build_date=<RFC3339 UTC>
```

リリースイメージでは 3 つの値をビルド引数から埋め込みます。`go build` や `go install` でビルドした場合は Go の build info
(モジュールバージョンと VCS の revision / time) を使い、取得できない項目は `dev` / `unknown` になります。同じ項目は起動時のログにも出ます。

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
| `clusters[].env` | ◯ | 一意の識別子 (英数字、`-`、`_`、`.`)。メトリクスの `env` ラベルと、`workload-state` ConfigMap の state キー `state.<env>.json` に使う。同じ cluster を指すエントリを複数置ける (actuator を変える場合など)。それぞれ別のキーに保存する |
| `clusters[].name` | ◯ | cluster 名。`eks:DescribeCluster` の引数と `cluster` ラベル |
| `clusters[].actuator` | | workload の検出と起動・停止の方式。`keda-prometheus` (既定) は `workload_knocker_activation` の Prometheus trigger を持つ `ScaledObject` を管理し、そのゲージを公開する。`keda-external` は Prometheus なしで KEDA の external scaler gRPC プロトコルを提供する ([KEDA external scaler](#keda-external-scaler) を参照)。未知の値は拒否される。変更するとその cluster をホットリロードする。[GitOps](gitops.md) を参照 |
| `clusters[].provider.type` | ◯ | `eks` または `kubeconfig` |
| `clusters[].provider.eks.region` | eks | AWS リージョン |
| `clusters[].provider.eks.assumeRoleArn` | | cross-account 時に AssumeRole する Role |
| `clusters[].provider.kubeconfig.path` | kubeconfig | kubeconfig のパス |
| `clusters[].provider.kubeconfig.context` | | context。省略時は current-context |
| `clusters[].frontends.slack.slashCommand` | frontend ごと | Slack App に登録する一意の slash command |
| `clusters[].frontends.discord.command` | frontend ごと | 一意の Discord slash command 名。1〜32 文字の `a-z` `0-9` `-` `_`、先頭の `/` なし |
| `clusters[].frontends.slack.notifyChannel` | | start / ready / stop を公開メッセージで投稿する Slack チャンネル ID (例 `C0123ABCD`。チャンネル名は拒否)。[ready メッセージと通知チャンネル](#ready-メッセージと通知チャンネル)を参照 |
| `clusters[].frontends.discord.notifyChannel` | | 同じメッセージを投稿する Discord チャンネル ID (17〜20 桁の数字) |

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
  **deny ルールはありません**。広い allow から例外を除くことはできないため、allow は狭く書き `default: deny` を保ってください。
- merge された `foo-api` / `foo-app` / `foo-worker` は構成 workload ごとに判定します。
- 未知のキー、不正な glob、形式の違う ID (例: `users` にユーザーグループ ID)、subject のないルールは読み込み時に拒否します。
- ファイルは YAML でも JSON でも構いません。

### 適用箇所

| 対象 | 動作 |
|---|---|
| Slack の namespace / workload 一覧 | ユーザーが start または stop できるものだけ表示。1 件もなければ明確なメッセージを返す |
| Slack の Plan / Apply | start/stop を Plan 時と Apply 時に再判定。モーダルが送った値に関わらず、許可されない項目は理由付きで拒否 |
| ステータス (`/<command> status`、`GET /api/v1/workloads`) | subject が start または stop できる workload だけ表示。Extend / Stop ボタンは、その操作が許可され動作モードも許す場合だけ表示し、ボタン押下時にも再判定する |
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
| Discord | ユーザー ID と、interaction の member データに含まれるロール ID (追加の API 呼び出しなし)。操作のたびに作り直すため、ロール変更は次のクリックから反映。DM にはロールがないため role ルールは適用されない |

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
  `notifyChannel` の変更は再起動なしで次のメッセージから反映します。
  Discord: Registry のコマンド集合と登録済みの集合を 15 秒ごとに比較し、変わっていれば application command を再度一括上書きします (失敗はログに出して再試行)。グローバルコマンドはクライアントへの反映が遅れることがあり、guild ID を指定すると即時です。Discord を初めて有効にする変更は再起動が必要です。
- 認可: 起動後に現れたポリシーファイルは認可を有効にします。ファイルを削除しても直前のポリシーを維持します。
- 再起動が必要な Registry 変更は明確なエラーで拒否し、旧設定を維持します。`controller.namespace` の変更、
  起動時に有効でなかった frontend の有効化、トークンなしでの有効化が該当します。
- 環境変数 (トークン、`OPERATION_MODE`、タイムアウト、パス) はホットリロードされません。

Kubernetes では ConfigMap の変更は kubelet の同期後、通常 1 分程度で Pod に届きます。chart は Registry とポリシーを
`/etc/workload-knocker` に 1 つの projected volume としてマウントし (`subPath` では更新されません)、どちらの ConfigMap にも
Pod テンプレートの checksum annotation を付けないため、変更で Pod は再起動しません。`kubectl rollout restart` が必要なのは上記の再起動必須ケースだけです。
このディレクトリは読み取り専用で chart が管理するため、kubeconfig など追加ファイルは別のパスにマウントしてください。

## Discord

Discord は Gateway (WebSocket) で接続するため、公開 HTTP エンドポイントは不要です。

```yaml
clusters:
  - env: stg
    name: example-staging
    provider: {type: eks, eks: {region: ap-northeast-1}}
    frontends:
      discord:
        command: workload-knocker-stg
```

セットアップ:

1. [Discord Developer Portal](https://discord.com/developers/applications) で Application と Bot を作成し、Bot token を Secret (`bot-token`) に保存します。
2. Privileged Gateway Intent は不要です (`Guilds` intent のみ使用)。
3. OAuth2 scope `applications.commands` と `bot` で Bot を招待します。`notifyChannel` を設定しない限り、`bot` scope に追加の権限は不要です (応答は ephemeral な interaction response のため、メッセージの読み取り・送信権限は使いません)。[通知チャンネル](#ready-メッセージと通知チャンネル)を使う場合のみ、そのチャンネルで **View Channel** と **Send Messages** を Bot に付与します (サーバー全体ではなくチャンネル単位で付与)。メッセージ内容の intent は不要です。
4. `DISCORD_BOT_TOKEN` (chart: `discord.secretName`、任意で `discord.guildId`) を設定します。試用中は guild ID の指定を推奨します (guild コマンドは即時反映、グローバルは遅延)。

起動時に、対象スコープ (guild またはグローバル) の application command を設定済みコマンドで一括上書きします。同スコープの他のコマンドは削除されるため、専用の Application を使ってください。登録に失敗すると (多くは `applications.commands` scope の不足) エラーで起動に失敗します。

コマンドには `start` と `status` の 2 つのサブコマンドがあります ([ステータス](#ステータス)参照)。Discord はサブコマンドを持つコマンドをサブコマンドなしで実行することを許可しないため ([Discord ドキュメント](https://discord.com/developers/docs/interactions/application-commands#subcommands-and-subcommand-groups))、**`/<command>` 単体は使えなくなり、`/<command> start` を使います**。Discord ユーザーにとっては破壊的変更ですが、起動時の登録で自動的に反映されるため手動作業は不要です。古いコマンド形状 (クライアントのキャッシュ) のまま送られた単体コマンドは `start` として扱います。

`/<command> start` のフロー (すべて ephemeral メッセージで、コンポーネントは実行したユーザーのみ操作できます):

1. サブコマンドで namespace のセレクトメニューを表示します (1 ページ 25 件、前へ/次へボタン)。
2. namespace を選ぶと workload の複数選択を表示します (1 ページ 25 件)。起動中のエントリは選択済みで、ページをまたいで選択が保持されます。`-api` / `-app` / `-worker` の merge と 🟢 / 🟡 / 🔴 は Slack と同じです。
3. **Apply** で Plan のプレビューと **Confirm** / **Cancel** を表示します。
4. **Confirm** で適用し、メッセージを結果に置き換えます。操作ユーザーは Discord のユーザー ID です。続けて[rollout の進捗](#rollout-の進捗)を表示します。

操作ごとの状態 (選択・ページ・Plan) はメモリ上に保持し、各コンポーネントの `custom_id` (`wk1:<kind>:<token>:<arg>`、100 文字制限内) のランダムなトークンで引きます。15 分操作がないと失効し、再起動でも失われるため、その場合は再度コマンドの実行を促します。このため controller は 1 レプリカで運用してください。3 秒以内の応答は、時間のかかる手順 (コマンド開始と Confirm) を defer して満たします。

認可と動作モードは Slack と同じです。ユーザー ID またはロールで操作できない namespace / workload は一覧に出ず、Plan と Confirm でポリシーを再判定するため、古いメニュー・ロール変更・偽造したコンポーネントでアクセスを広げることはできません。`scheduled-stop` では停止不可の注記を表示し、stop は Slack と同じメッセージで拒否します。ルールは `subjects.discord.users` (ユーザー ID) と `subjects.discord.roles` (ロール ID) で書きます。ID のコピーには Discord の開発者モードを有効にします。

chart は `discord.secretName` を設定したときだけ `DISCORD_BOT_TOKEN` と `DISCORD_GUILD_ID` を出力します。同様に `SLACK_APP_TOKEN` と `SLACK_BOT_TOKEN` は `slack.secretName` を設定したときだけ出力します。どちらか一方は必須で、両方空だと chart はエラーにします。

## managed cluster へのアクセス

EKS では controller に IAM Role (IRSA) を付与し、その Role を Access Entry に
`kubernetes_groups: ["workload-knocker"]` で登録して、
[`example/managed-cluster-rbac.yaml`](../../example/managed-cluster-rbac.yaml) を適用します。
必要な権限は `scaledobjects` の `get/list/watch`、`deployments` / `statefulsets` とその `scale`
サブリソースおよび `pods` の `get/list` (検出と rollout の進捗表示)、`controller.namespace` の
ConfigMap の `get/create/patch` です (state は env ごとのキーを merge patch で更新します)。

state ConfigMap にエントリが無い ScaledObject を新たに検出すると、scale target
(既定は `Deployment`、または `StatefulSet`) の replicas を読みます。replicas が 0 より大きければ
running として登録し (`workload_knocker_activation` を 1、state ConfigMap にも保存)、KEDA に
scale down させません。0 の場合や読み取りに失敗した場合 (warning ログを出力) は stopped で登録します。
保存済みの state は常に実際の replicas より優先されます。

ScaledObject は `RECONCILE_INTERVAL` ごとに再取得します。新規作成された ScaledObject は 1 interval 以内に
series が作られ、削除されたものは series が消えます (チャットコマンド不要)。

## rollout の進捗

Confirm の後、適用した各 workload の scale target (`Deployment` または `StatefulSet`) を
`ROLLOUT_POLL_INTERVAL` ごとに managed cluster でポーリングし、workload ごとに 1 行で表示します。

| 表示 | 意味 |
|---|---|
| `⏳ name starting...` | start 済みで `readyReplicas` が `spec.replicas` に達していない状態。KEDA が replica 数を変えるまでの待ち (KEDA の polling interval) も含みます。 |
| `✅ name started` | `readyReplicas >= spec.replicas` かつ 0 より大きい。[ready メッセージ](#ready-メッセージと通知チャンネル)があれば `✅ name started — <メッセージ>` の形で付きます。 |
| `⏳ name stopping...` | stop 済みで replica または ready な Pod が残っている。 |
| `⏹ name stopped` | `spec.replicas` が 0 で ready な Pod がない。 |
| `⚠ name not ready after 10m (0/1 ready, ImagePullBackOff)` | `ROLLOUT_WATCH_TIMEOUT` 内に起動しなかった。理由は最後の `ready/desired` と Pod の waiting / スケジューリング理由 (`ImagePullBackOff`、`Unschedulable` など)。停止が終わらなかった場合は `still running after 10m` になります。 |
| `ℹ name applied (progress is not tracked for kind ...)` | scale target の kind が Deployment / StatefulSet ではない。 |

- Discord は interaction response を編集します。編集は poll 間隔につき最大 1 回で、行が変わったときだけです。interaction token は 15 分有効なので `ROLLOUT_WATCH_TIMEOUT` に上限があります。
- Slack は `⏳` 行入りの結果メッセージをすぐ投稿し、全 workload が確定した時点で最終状態を ephemeral メッセージでもう 1 件投稿します。`chat.postEphemeral` のメッセージは編集できず、モーダル送信には `response_url` もないため、途中経過の更新はありません。
- 進捗はメモリ上で追跡します。controller が再起動すると追跡は終わり、最後のメッセージはそのまま残ります。
- 読み取りに失敗しても (RBAC 不足など) ポーリングを続け、タイムアウト時にそのエラーを理由として表示します。

## ready メッセージと通知チャンネル

### ready メッセージ

利用可能になったかどうかは `✅ name started` の行で分かります。ScaledObject のアノテーション `workload-knocker.io/ready-message` (Go の [`text/template`](https://pkg.go.dev/text/template)) で、この行に接続方法などの案内を足せます。

```yaml
metadata:
  annotations:
    workload-knocker.io/ready-message: "Connect to {{.Name}}.example.com:7777{{if .StopAt}} (until {{.StopAt}}){{end}}"
```

| フィールド | 値 |
|---|---|
| `.Name` | workload 名 |
| `.Namespace` | namespace |
| `.Cluster` | Registry の `env` |
| `.StartedBy` | ストアに記録された起動者。Discord はユーザー ID、Slack はユーザー名、API は呼び出しの `user` |
| `.StopAt` | 停止予定時刻 (`15:04 MST` 形式)。不明なときは空なので `{{if .StopAt}}` で囲みます |

アノテーションに秘密情報を書かないでください (チャットに表示されます)。描画結果は 500 文字までです。

- テンプレートは ScaledObject の検出時とアノテーションの変更時に検証します。不正なテンプレートは変更ごとに 1 回 `WARN` (`invalid ready-message template`) を出します。
- 描画に失敗した場合は `WARN` を出し、通常の `✅ name started` にフォールバックします。
- 進捗メッセージの started の行 (Discord はメッセージ編集、Slack は最終メッセージ) と通知チャンネルのメッセージに追記します。

### 通知チャンネル

既定では Bot はコマンドを実行したユーザーにだけ ephemeral メッセージで応答します。cluster の Slack / Discord frontend に `notifyChannel` を設定すると、そのチャンネルにも公開メッセージを投稿し、他のメンバーから何が動いているか見えるようになります。

| イベント | メッセージ |
|---|---|
| start 要求 (チャットまたは `POST /api/v1/start`) | `🚀 <実行者> requested start (ENV / cluster / namespace)` と workload ごとの `⏳ name starting...`。各 workload が ready になると同じメッセージを `✅ name started — <ready メッセージ>` に編集するため、1 操作につきメッセージは 1 件です。ready の検出には上記の rollout 追跡を使います。 |
| stop (チャット) | `⏹ <実行者> stopped workloads (...)` |
| stop-all (`POST /api/v1/stop-all`) | cluster ごとに 1 件。実行者は `nightly-stop` (chart の CronJob) または `api`。認可ポリシーで残した workload と失敗した cluster は含めません。 |
| 停止リマインダー | `⏰ name stops at 22:00 JST (ENV / namespace)` と **Extend 1h** ボタン。自動停止が `STOP_REMINDER` だけ停止の前に投稿します。ボタンはチャットや API と同じ延長処理を実行し (認可、workload の最大時間、`workload.extend` 監査イベントが適用)、押した本人にだけ新しい停止時刻または拒否理由を返します。 |

補足:

- `notifyChannel` がなければ何も投稿せず、追加の権限も不要です。投稿が失敗しても操作は止まらず、`notify post failed` をログに出すだけです。
- Discord のメッセージはメンションを無効にして送るため、ユーザーを指名しても通知は飛びません。Slack では `<@user>` のメンションで該当ユーザーに通知が届くことがあります。
- Discord: チャンネルで Bot に **View Channel** と **Send Messages** を付与します。Slack: Bot に `chat:write` scope (`slack-manifest.yaml` に含まれます) が必要で、チャンネルへの招待 (`/invite @workload-knocker`) も必要です。プライベートチャンネルは `G...` の ID を使います。
- 1 秒以内に複数の workload が ready になった場合、編集は 1 回の API 呼び出しにまとめます。メッセージの状態はメモリ上にのみあり、コントローラーの再起動後は進行中の start メッセージは編集されません。

## Slack の workload モーダル

引数なしの `/<command>` でこのモーダルを開きます。`/<command> status` は代わりに workload 一覧を表示します ([ステータス](#ステータス)参照)。その他の引数には短い使い方を返します。

- namespace と workload は a→z 順です。
- `foo-api` / `foo-app` / `foo-worker` のうち 2 件以上あれば 1 つの `foo` チェックボックスに
  merge されます。一部だけ起動中なら 🟡 でチェック ON になります。
- Slack の制限 (checkbox 10 件 / モーダル 100 block) により merge 後 90 エントリまで表示します。
  非表示分は Plan から除外されるため、意図せず停止されることはありません。

## ステータス

`/<command> status` (Discord ではサブコマンド、Slack では引数) は、ユーザーが操作できる workload を namespace ごと (a→z) に ephemeral で一覧します。各エントリに状態 (🟢 起動中、🟡 一部起動中、🔴 停止中)、起動した人、起動時刻、停止時刻 (`stops <時刻>`、なければ `no auto-stop`) を表示します。時刻は閲覧者のタイムゾーンで表示されます (Discord は相対時刻、Slack は日付形式)。`-api` / `-app` / `-worker` は start のフローと同じく 1 つにまとめます。

一覧は controller のメモリ上の状態だけから作ります。クラスタ呼び出しも reconcile もしないため、Discord の 3 秒以内に応答できます。状態はバックグラウンドの reconcile (`RECONCILE_INTERVAL`) で更新されます。

起動中のエントリにはボタンが付きます (ラベルにエントリ名が入ります):

- **Extend 1h**: `POST /api/v1/extend` と同様に停止時刻を 1 時間延ばします。自動停止があるエントリにだけ表示します。
- **Stop**: 確認なしで直接停止します。通常の停止と同じ経路 (ポリシー判定、動作モード判定、source が `discord` / `slack` の `workload.stop` 監査イベント、通知チャンネル) を通ります。`scheduled-stop` モードでは表示しません。

押下後は、その場で一覧を結果と最新の状態に置き換えます。判定は押下時にも再実行するため、古い一覧で権限を広げることはできません。

制限と注意:

- Discord は 1 メッセージ 25 ボタン・本文 2000 文字、Slack は 50 block までです。超えたエントリも一覧には出ますが (本文が切れる場合は末尾に `… and N more`)、ボタンは付かず、その旨を表示します。その場合は `/<command> start` を使ってください。
- 起動した人と起動時刻は start 適用時に記録し、`workload-state` ConfigMap (`startedBy`、`startedAt`) に保存します。controller が最初から起動中として検出した workload や、古いバージョンが書いた state では、起動した人と起動時刻は表示されません。起動した人は Discord ではユーザー ID (メンション表示)、Slack ではユーザー名、API では呼び出しの `user` です。
- [認可ポリシー](#認可)で見えるエントリだけを表示します。
- Discord はボタンのペイロードを 24 時間メモリに保持します。それを過ぎた後や再起動後に古いボタンを押すと、失効した旨を返します。Slack のボタンはペイロードを持つため引き続き動作します。

REST では [`GET /api/v1/workloads`](#get-apiv1workloads) が同じ情報を返します。

## 実行時間

start には実行時間を付けられます。コントローラは workload ごとの `stopAt` を `workload-state` ConfigMap に保存し、
バックグラウンドループ (`RECONCILE_INTERVAL`。停止の精度は約 1 interval) が期限切れの workload を停止します。
再起動後も有効です。停止すると `stopAt` は消えます。夜間停止も従来どおり動き、先に来た方が workload を止めます。
古いバージョンが書いた state には `stopAt` が無く「自動停止なし」として扱われます。同じ state は[ステータス](#ステータス)用に `startedBy` と `startedAt` も記録します。`RECONCILE_INTERVAL=0` では
自動停止は動きません。

既定値は ScaledObject の annotation で workload ごとに指定でき、無ければ環境変数を使います。

| annotation | フォールバック | 意味 |
|---|---|---|
| `workload-knocker.io/default-duration` | `DEFAULT_RUN_DURATION` | `4h` のような Go duration。`0` でこの workload の既定を無効化。`0` 超 `15m` 未満は警告して無視 |
| `workload-knocker.io/max-duration` | `MAX_RUN_DURATION` | `15m` 以上の Go duration。不正な値は警告して無視 |

既定値が上限を超える場合は上限に切り下げます。

- **Discord**: プランのプレビューに各 start の停止時刻 (閲覧者のタイムゾーン) と **Change duration** ボタンが出ます。
  ボタンは start 対象ごとに 1 つ (最大 5、超える場合は先頭 5 件) の入力欄を持つモーダルを開き、現在値が時間単位で入っています。
  入力は時間の数値 (`3`、`1.5`、最小 `0.25`)、空は workload の既定です。不正な値には ephemeral で理由と **Try again** ボタンを返し、
  ボタンで前回の入力を入れたままモーダルを開き直せます。
- **Slack**: workload モーダルに任意の **Run duration (hours)** 数値入力 (小数可) があり、その送信で start する全 workload に適用されます。
  空は各 workload の既定です。範囲外の値はインラインエラーで拒否します。
- **REST**: `POST /api/v1/start` は `duration` (`"3h"` のような Go duration) を受け付けます ([HTTP API](#post-apiv1start))。
- **延長**: `POST /api/v1/extend` とフロントエンドサービスが、起動中 workload の `stopAt` を後ろにずらします。start と同じ認可で、
  どの動作モードでも可です。延長後の残り時間が上限を超える場合は拒否します。
- **リマインド**: `stopAt` の `STOP_REMINDER` 前に、reconcile ループが workload と停止時刻ごとに 1 回 `StopReminder` を呼びます
  (メモリで記憶するため再起動後は再送されることがあります)。通知チャンネルがあれば **Extend 1h** ボタン付きで投稿し、なければ何も投稿しません。

監査イベント: start は `workload_knocker.duration_seconds` と `workload_knocker.stop_at` を記録し、延長は `workload.extend`、
期限切れの停止は source `scheduler` の `workload.auto_stop` です。

## 動作モードと夜間停止

Helm chart は controller の環境変数 `OPERATION_MODE` と `API_TOKEN` を設定します。

| `mode` | API start | API stop / stop-all | `nightlyStop` |
| --- | --- | --- | --- |
| `full` (既定) | 可 | 可 | 任意 |
| `scheduled-stop` | 可 | 不可 (`403`。夜間 CronJob が停止) | 必須 (`enabled: true`) |
| `off` | 可 | 不可 | 有効化不可 |

`nightlyStop.enabled` が true の場合は CronJob が `/api/v1/stop-all` を呼ぶため、`api.tokenSecret.name` が必須です。
それ以外は任意です。REST API を使わない場合は空にすると `API_TOKEN` を出力せず、`/api/v1/*` は
`503` (fail closed) を返し、チャットからの操作はそのまま使えます。API を使う場合はインストール前に Secret を作成します。

```sh
kubectl create secret generic workload-knocker-api \
  --namespace workload-knocker \
  --from-literal=api-token='<random-token>'
```

`nightlyStop` は、クラスタ内 Service へ `Authorization: Bearer <token>` 付きで
`POST /api/v1/stop-all` を送る CronJob を作成し (監査ログの source 用に `X-Workload-Knocker-Source: nightly-stop` ヘッダー付き)、2xx 以外は失敗として扱います。

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
`api.tokenSecret.name` なしの CronJob) は `helm template` / `helm install` で失敗します。

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


### `GET /api/v1/workloads`

`api` subject が start または stop できる workload を、`env`、`namespace`、`name` 順に、controller のメモリ上の状態から返します
(クラスタ呼び出しなし)。どの動作モードでも利用できます。常に `200` で JSON オブジェクトを返し、見えるものがなければ空配列です。

```json
{"workloads": [
  {"env": "stg", "namespace": "my-namespace", "name": "a", "kind": "Deployment", "state": "running",
   "startedBy": "ci-bot", "startedAt": "2026-11-08T09:00:00Z", "stopAt": "2026-11-08T12:00:00Z"},
  {"env": "stg", "namespace": "my-namespace", "name": "b", "kind": "Deployment", "state": "stopped",
   "startedBy": "", "startedAt": null, "stopAt": null}
]}
```

`state` は `running` または `stopped` です。`startedBy`、`startedAt`、`stopAt` は現在の実行を表します。停止中の workload は
`startedBy` が空で時刻は `null`、起動時刻が不明なら `startedAt` が `null`、自動停止がなければ `stopAt` が `null` です。
チャットの表示と違い workload ごとの一覧 (merge なし) のため、一部起動中の状態はありません。
`401` / `503` で拒否されたリクエストは、監査イベントが workload の操作を表すものなので監査ログには出力しません。

### `POST /api/v1/start`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a", "b"], "user": "ci-bot"}
```

`user` は省略可 (既定 `rest-api`)。`duration` も省略可 (`"3h"` や `"90m"` のような Go duration。空は workload の既定) で、
`15m` から workload の上限までの範囲外なら何も start せず `400` です。`200` で `started` / `not_found` / `forbidden` と
`stop_at` (自動停止がある start 済み workload ごとの RFC 3339) を返し、入力不正は `400`、
要求した workload がすべて [認可ポリシー](#認可) に拒否された場合は `403`、
未登録の `env` または 1 件も見つからない場合は `404` です。

### `POST /api/v1/extend`

```json
{"env": "stg", "namespace": "my-namespace", "workloads": ["a"], "duration": "1h"}
```

起動中の各 workload の `stopAt` に `duration` (必須、`15m` 以上) を加えます。`api` subject に対して start と同じ認可を行います。
`200` で `extended` (workload と新しい停止時刻)、`not_found`、`forbidden`、`failed` (停止中、自動停止なし、上限超過) を返し、
すべて拒否された場合は `403`、未登録の `env` または該当 workload なしは `404`、workload はあるが延長できなかった場合は `409`、
入力不正は `400` です。

### `POST /api/v1/stop-all`

`full` モードのみ可 (それ以外は `403`)。全 managed cluster のうち `api` subject が stop できる workload を停止します (best effort)。
認可ポリシーで止めなかった起動中 workload は cluster ごとの `skipped` と `total_skipped` に出ます。一部の cluster が失敗しても `200` で
cluster ごとの内訳を返します。

### プローブ

`/livez` と `/healthz` はプロセス起動後に 200。`/readyz` は全 cluster の初期同期後に 200 になります。

## KEDA external scaler

`actuator: keda-external` は経路から Prometheus を外します。KEDA がコントローラへ gRPC で接続し、コントローラは start / stop を
即座に push します。gRPC サーバーはプロセスに 1 つで、すべての `keda-external` cluster を担当します。
いずれかの cluster が使っている場合にコントローラと一緒に起動するため、最初の `keda-external` cluster をホットリロードで
追加した場合は再起動が必要です。Chart では `externalScaler.enabled: true` でポートを公開します (`externalScaler.port`、既定 `9090`)。

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: workload-knocker.workload-control.svc:9090
      cluster: stg            # clusters[].env (必須)
      namespace: my-ns        # 任意。既定は ScaledObject の namespace
      workload: my-app        # 任意。既定は scale target の名前
```

- `external-push` (または `external`) trigger を持ち、`scalerAddress` が `EXTERNAL_SCALER_ADDRESS_MATCH` を含み、`cluster` が
  `clusters[].env` と一致する `ScaledObject` を管理します。
- 起動すべき状態の間 active です。`IsActive` とストリームがその状態を返し、メトリクス `workload_knocker_running` は起動中 `1`、
  停止中 `0` (target は `1`) です。
- `StreamIsActive` はストリーム開始時、start / stop のたび、そして keepalive として 30 秒ごとに push します。
- このポートに認証はありません。cluster 内部に留めてください (ClusterIP の Service)。TLS は `externalScaler.tls.secretName` に
  `kubernetes.io/tls` Secret (cert-manager 可) を指定し、KEDA 側は `TriggerAuthentication` の `caCert` (必要なら
  `tlsClientCert` / `tlsClientKey`) で CA を渡します。
- コントローラの再起動中、KEDA はストリームが再接続するまで最後の状態を保ちます。コントローラがまだ知らない workload への
  呼び出しは `NotFound` で失敗し、KEDA は ScaledObject の `fallback` を適用します。長い停止でも希望の台数に落ち着くよう
  `fallback` (`failureThreshold`、`replicas`) を設定してください。
- 状態は引き続き `workload-state` ConfigMap に保存され、再起動後はポートを開く前に復元されます。

例: [`example/scaledobject-external.example.yaml`](../../example/scaledobject-external.example.yaml)

## メトリクス

| メトリクス | 型 | ラベル |
|---|---|---|
| `workload_knocker_activation` | gauge | `env`, `cluster`, `namespace`, `workload` |
| `workload_knocker_cluster_reachable` | gauge | `cluster` |
| `workload_knocker_token_refresh_total` | counter | `cluster`, `result` |
| `workload_knocker_operations_total` | counter | `env`, `cluster`, `operation`, `result` |
| `workload_knocker_operation_duration_seconds` | histogram | `env`, `cluster`, `operation` |
| `workload_knocker_config_reload_total` | counter | `target` (`registry`, `authz`), `result` (`success`, `error`) |
| `workload_knocker_reconcile_total` | counter | `cluster`, `result` (`success`, `error`) |
| `workload_knocker_auto_stops_total` | counter | `env`, `cluster`, `result` (`success`, `error`) |
| `workload_knocker_external_scaler_streams` | gauge | `cluster` (開いている `keda-external` push ストリーム数) |

`operation` は `start`、`stop`、`stop_all` のいずれかです。`result` は `success`、`error`、`not_found` のいずれかです
(`error` を返すのは `stop_all` のみ)。カーディナリティを抑えるため、ユーザー ID と workload 名はラベルに使いません。
