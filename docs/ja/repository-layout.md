# リポジトリ構成

`internal/` は関心ごとにまとめています。グループの下に 1 つ以上のパッケージがあり、Go のパッケージ名は
変えていないため、どのグループかは import パスでわかります。

| パス | 内容 |
|---|---|
| `cmd/server/` | エントリポイント (`main.go`)。`--version` フラグを含む |
| `charts/workload-knocker/` | Helm chart。`ci/` は lint とリリース検証用の values |
| `example/` | Registry、ポリシー、Helm values、RBAC、`.env` のサンプル |
| `docs/en/`、`docs/ja/` | ドキュメント (英語・日本語) |
| `hack/` | リリースとライセンス検査のスクリプト |
| `.github/workflows/` | CI、release-please、リリースのワークフロー |
| `slack-manifest.yaml` | Slack App マニフェスト |

## `internal/`

| グループ | パッケージ | 内容 |
|---|---|---|
| `app/` | `container/` | 依存の組み立て |
| | `server/` | 全コンポーネントを起動・停止する Lifecycle マネージャ |
| | `reload/` | Registry とポリシーのホットリロード |
| `config/` | | 環境変数と Cluster Registry |
| `policy/` | `authz/` | 認可ポリシーのパースと評価 |
| | `mode/` | `OPERATION_MODE` |
| | `runlimit/` | 実行時間の既定値・上限・パース・延長ルール |
| `workload/` | `model/` | 共有の workload 型と、store の役割別インターフェース |
| | `store/` | cluster ごと・複数 cluster の workload 状態 |
| | `rollout/` | Confirm 後の rollout 進捗追跡 (ポーリング、状態遷移、cluster ごとの reader) |
| `cluster/` | `k8s/` | cluster クライアント、EKS / kubeconfig 認証 (`k8s/auth/`)、`ScaledObject` と scale target の取得、state ConfigMap |
| `actuator/` | | `Discoverer` / `Actuator` インターフェース、`keda-prometheus` / `keda-external` / `direct` 実装、複数 actuator を束ねる `multi`、cluster ごとの `factory`、契約テスト `actuatortest` |
| `schedule/` | `reconcile/` | バックグラウンド reconcile ループ (tick ごとのフック付き) |
| | `periodic/` | ループが毎回実行するフック (自動停止、予約) を実行順に組み立てる |
| | `autostop/` | 期限切れの workload を停止し、停止リマインドを呼ぶフック |
| | `reservation/` | 単発予約の作成・一覧・取り消しと、リマインド・実行・missed 判定を行うフック |
| `frontend/` | | Slack、Discord、start / extend API が共有する、チャットに依存しない start/stop フローと、各プラットフォームの `Markup` で描画する共通のチャット行 |
| | `discord/` | Discord bot (gateway、スラッシュコマンド、modal) |
| | `slack/` | Slack bot (Socket Mode、スラッシュコマンド、modal) |
| | `api/` | REST ハンドラ (`/api/v1/start`、`/extend`、`/stop-all`、`/workloads`、`/reservations`) と Bearer 認証ミドルウェア |
| | `http/` | `/metrics`、プローブ、API を提供する HTTP サーバー |
| | `notify/` | ready メッセージのテンプレートと任意の通知チャンネル (プラットフォーム非依存の notifier、`Poster` アダプタ、Extend ボタンの契約) |
| `observability/` | `audit/` | 監査イベントのスキーマと recorder ([audit-log.md](audit-log.md)) |
| | `metrics/` | Prometheus メトリクス (`workload_knocker_activation` など) |
| | `logging/` | slog の設定 (`timestamp` / `message` キー、`service.*` 属性) |
| | `version/` | ビルド情報 (`Version` `Commit` `BuildDate`) |
