# リリース

リリースは [release-please](https://github.com/googleapis/release-please-action) で自動化しています。`CHANGELOG.md` と GitHub Release のノートは Conventional Commits から生成され、Renovate の依存更新 PR にも表示されます。

## リリースの流れ

1. PR は squash マージし、PR タイトルがコミットメッセージになります。タイトルは Conventional Commits (`feat` `fix` `docs` `refactor` `perf` `test` `build` `ci` `chore` `revert`、任意の scope、破壊的変更は `!`) に従います。`pr-title` チェックが検査します。
2. `main` への push ごとに release-please がリリース PR を作成・更新します。`.release-please-manifest.json`、`CHANGELOG.md`、`charts/workload-knocker/Chart.yaml` の `version` / `appVersion` が更新され、chart・appVersion・アプリのバージョンは常に一致します。
3. リリース PR をマージするとタグ `vX.Y.Z` と GitHub Release が作成されます。リリースノートは生成された changelog (Features / Bug Fixes / Performance Improvements / Dependencies / Reverts と BREAKING CHANGES) です。
4. タグで `.github/workflows/release.yml` が動きます。
   - `image` は `ghcr.io/murasame29/workload-knocker:X.Y.Z` を公開します (最新の安定タグのときだけ `X.Y` / `X` / `latest` も付与)。OCI ラベル `source` / `version` / `revision` / `created` を付け、バージョン・コミット・ビルド日時をバイナリに埋め込みます (`workload-knocker --version`)。
   - `chart` (`image` の後) はタグ == Chart の `version` == `appVersion`、イメージの存在、パッケージに LICENSE/NOTICE が含まれることを確認し、`oci://ghcr.io/murasame29/charts/workload-knocker` の `X.Y.Z` を公開して `.tgz` を GitHub Release に添付します。

公開済みのイメージと chart のバージョンは不変です。再実行は一致する成果物だけを引き継ぎ、そうでなければ失敗します。

## 必須シークレット

リポジトリシークレット `RELEASE_PLEASE_TOKEN` (fine-grained PAT または GitHub App トークン) が必要です。権限は Contents: read/write と Pull requests: read/write。`GITHUB_TOKEN` は使えません。これで作成したタグやリリースは他のワークフローを起動せず、`release.yml` が動かず、開いた PR でも CI が走りません。

## バージョニング

SemVer。1.0.0 未満では破壊的変更が minor、`feat` と `fix` が patch を上げます (`bump-patch-for-minor-pre-major`)。機能ごとにリリースして検証し、0.2.0 のようなマイルストーンは `Release-As` フッターか `release-please-config.json` の `release-as` で切ります。manifest は `0.0.0` から始まるため最初のリリース PR は `0.1.0` を提案します。1.0.0 にするには `Release-As: 1.0.0` フッター付きのコミットを `main` に入れます (空コミットでも可)。1.0.0 以降は破壊的変更が major、`feat` が minor を上げます。

## release ブランチからのパッチリリース (未実装)

`main` が先に進んだ後の旧系列の修正は、タグ `vX.Y.Z` から作った `release-X.Y` ブランチに入れ、同ブランチで release-please を動かして `vX.Y.(Z+1)` を作る想定です。`release.yml` は最新の安定タグのみ `latest` / `X.Y` / `X` を更新するため、このパッチがそれらのエイリアスを奪うことはありません。この構成はここに記述しているだけで、設定していません (同ブランチを `on.push.branches` に加え、ブランチ側に manifest が必要)。
