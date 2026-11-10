# Releasing

Releases are automated with [release-please](https://github.com/googleapis/release-please-action).
`CHANGELOG.md` and the GitHub Release notes are generated from Conventional Commits, so Renovate can show them in dependency-update PRs.

## How a release works

1. PRs are squash-merged. The PR title becomes the commit message, so it must follow Conventional Commits (`feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`; optional scope; `!` for breaking). The `pr-title` check enforces this.
2. On every push to `main`, `.github/workflows/release-please.yml` opens or updates a release PR. It bumps `.release-please-manifest.json`, `CHANGELOG.md`, and `version` / `appVersion` in `charts/workload-knocker/Chart.yaml` (marked with `x-release-please-version`), so chart version, appVersion and app version are always equal.
3. Merging the release PR creates the tag `vX.Y.Z` and the GitHub Release. The release body is the generated changelog (Features, Bug Fixes, Performance Improvements, Dependencies, Reverts, and a BREAKING CHANGES section).
4. The tag triggers `.github/workflows/release.yml`:
   - `image` publishes `ghcr.io/murasame29/workload-knocker:X.Y.Z` (plus `X.Y`, `X`, `latest` when it is the highest stable tag) with OCI labels `source`, `version`, `revision`, `created`, and bakes the version, commit and build date into the binary (`workload-knocker --version`).
   - `chart` (after `image`) checks that tag == Chart `version` == `appVersion`, that the image exists, and that the package carries LICENSE/NOTICE, then pushes `oci://ghcr.io/murasame29/charts/workload-knocker` version `X.Y.Z` and attaches the `.tgz` to the GitHub Release.

Published image and chart versions are immutable: a re-run adopts an existing artifact only if it matches, otherwise it fails.

## Required secret

Create a repository secret `RELEASE_PLEASE_TOKEN`: a fine-grained PAT or a GitHub App installation token for this repository with:

- Contents: read and write
- Pull requests: read and write

`GITHUB_TOKEN` cannot be used. Tags and releases created with it do not trigger other workflows, so `release.yml` would never run, and PRs it opens do not run CI.

## Versioning

Semantic versioning with the 0.x rules (`bump-minor-pre-major`, `bump-patch-for-minor-pre-major`): while below 1.0.0, breaking changes (`!` or a `BREAKING CHANGE:` footer) bump the minor version, and `feat` and `fix` bump the patch. Each feature is released and verified on its own; a milestone such as 0.2.0 is cut with a `Release-As` footer or `release-as` in `release-please-config.json`. The manifest starts at `0.0.0`, so the first release PR proposes `0.1.0`.

To cut 1.0.0, add a commit (or an empty commit) on `main` with the footer `Release-As: 1.0.0`; the release PR then proposes 1.0.0. From 1.0.0 on, breaking changes bump the major version and `feat` the minor version.

## Patch releases from a release branch (not implemented yet)

When `main` has moved on to a newer minor, a fix for an old line would go to a `release-X.Y` branch created from tag `vX.Y.Z`. The same release-please workflow would then also run for that branch (add it to `on.push.branches`, with a manifest on that branch) and produce `vX.Y.(Z+1)`. `release.yml` already moves `latest`, `X.Y` and `X` only for the highest stable tag, so such a patch does not take over those aliases. This setup is described here only and is not configured.
