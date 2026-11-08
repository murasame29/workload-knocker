# リリース

リリースは [release-please](https://github.com/googleapis/release-please-action) で自動化しています。`CHANGELOG.md` と GitHub Release のノートは Conventional Commits から生成され、Renovate の依存更新 PR にも表示されます。

## リリースの流れ

1. PR は squash マージし、PR タイトルがコミットメッセージになります。タイトルは Conventional Commits (`feat` `fix` `docs` `refactor` `perf` `test` `build` `ci` `chore` `revert`、任意の scope、破壊的変更は `!`) に従います。`pr-title` チェックが検査します。
2. `main` への push ごとに release-please がリリース PR を作成・更新します。`.release-please-manifest.json`、`CHANGELOG.md`、`charts/workload-knocker/Chart.yaml` の `version` / `appVersion` が更新され、chart・appVersion・アプリのバージョンは常に一致します。
3. リリース PR をマージするとタグ `vX.Y.Z` と GitHub Release が作成されます。
4. タグで `.github/workflows/release.yml` が動き、イメージ `ghcr.io/murasame29/workload-knocker:X.Y.Z` を公開した後、OCI chart `oci://ghcr.io/murasame29/charts/workload-knocker` の `X.Y.Z` を公開し、`.tgz` を GitHub Release に添付します。

## 必須シークレット

リポジトリシークレット `RELEASE_PLEASE_TOKEN` (fine-grained PAT または GitHub App トークン) が必要です。権限は Contents: read/write と Pull requests: read/write。`GITHUB_TOKEN` で作成したタグは他のワークフローを起動しないため使えません。

## バージョニング

SemVer。1.0.0 未満では `feat` と破壊的変更が minor、`fix` が patch を上げます。manifest は `0.0.0` から始まるため最初のリリース PR は `0.1.0` を提案します。1.0.0 にするには `Release-As: 1.0.0` フッター付きのコミットを `main` に入れます。

## release ブランチからのパッチリリース (未実装)

`main` が先に進んだ後の旧系列の修正は、タグ `vX.Y.Z` から作った `release-X.Y` ブランチに入れ、同ブランチで release-please を動かして `vX.Y.(Z+1)` を作る想定です。`release.yml` は最新の安定タグのみ `latest` / `X.Y` / `X` を更新します。設定はまだ行っていません。
