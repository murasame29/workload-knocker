# リポジトリ構成

| パス | 内容 |
|---|---|
| `cmd/server/` | エントリポイント (`main.go`)。`--version` フラグを含む |
| `internal/api/` | REST ハンドラ (`/api/v1/start`、`/api/v1/stop-all`) と Bearer 認証ミドルウェア |
| `internal/audit/` | 監査イベントのスキーマと recorder ([audit-log.md](audit-log.md)) |
| `internal/authz/` | 認可ポリシーのパースと評価 |
| `internal/config/` | 環境変数と Cluster Registry |
| `internal/container/` | 依存の組み立て |
| `internal/frontend/` | Slack と Discord が共有する、チャットに依存しない start/stop フロー |
| `internal/k8s/` | cluster クライアント、EKS / kubeconfig 認証、`ScaledObject` の取得、state ConfigMap |
| `internal/logging/` | slog の設定 (`timestamp` / `message` キー、`service.*` 属性) |
| `internal/metrics/` | Prometheus メトリクス (`workload_knocker_activation` など) |
| `internal/mode/` | `OPERATION_MODE` |
| `internal/model/` | 共有の workload 型 |
| `internal/reconcile/` | バックグラウンド reconcile ループ (tick ごとのフック付き) |
| `internal/reload/` | Registry とポリシーのホットリロード |
| `internal/server/` | Lifecycle マネージャ、HTTP サーバー、Slack / Discord bot |
| `internal/store/` | cluster ごと・複数 cluster の workload 状態 |
| `internal/version/` | ビルド情報 (`Version` `Commit` `BuildDate`) |
| `charts/workload-knocker/` | Helm chart。`ci/` は lint とリリース検証用の values |
| `example/` | Registry、ポリシー、Helm values、RBAC、`.env` のサンプル |
| `docs/en/`、`docs/ja/` | ドキュメント (英語・日本語) |
| `hack/` | リリースとライセンス検査のスクリプト |
| `.github/workflows/` | CI、release-please、リリースのワークフロー |
| `slack-manifest.yaml` | Slack App マニフェスト |
