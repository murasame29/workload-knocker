# Changelog

## [0.1.1](https://github.com/murasame29/workload-knocker/compare/v0.1.0...v0.1.1) (2026-11-08)


### Bug Fixes

* **chart:** make the Slack Secret optional ([#27](https://github.com/murasame29/workload-knocker/issues/27)) ([b4af1de](https://github.com/murasame29/workload-knocker/commit/b4af1de3bb4a64c2183d9a0716f3ccfab2d8d01b))

## 0.1.0 (2026-11-08)


### ⚠ BREAKING CHANGES

* the cluster registry key slack is now frontends.slack. Slack credentials are required only when the Slack frontend is enabled.
* /api/v1/* requires bearer authentication and is disabled when API_TOKEN is unset. Adds OPERATION_MODE.
* NEW_RELIC_* settings and chart newRelic values are removed. Workload operations are exposed as Prometheus metrics.

### Features

* add --version flag and refresh the documentation ([#22](https://github.com/murasame29/workload-knocker/issues/22)) ([6baf70d](https://github.com/murasame29/workload-knocker/commit/6baf70d69f7b0f7e9d518e6793dbe06385d4cd80))
* add authorization policy and hot reload ([#21](https://github.com/murasame29/workload-knocker/issues/21)) ([9f25d0d](https://github.com/murasame29/workload-knocker/commit/9f25d0d1966b54d34ca526bb696d2b7cb470ab34))
* add Discord frontend ([#20](https://github.com/murasame29/workload-knocker/issues/20)) ([65794f9](https://github.com/murasame29/workload-knocker/commit/65794f9dfc23a46410c110456466b67ebb530eb2))
* **chart:** add operation mode and nightly stop CronJob ([#16](https://github.com/murasame29/workload-knocker/issues/16)) ([85c2ec0](https://github.com/murasame29/workload-knocker/commit/85c2ec0f0b881e954dccd42d657847c67b46096b))
* initial open source release ([3837257](https://github.com/murasame29/workload-knocker/commit/38372572bdd7cab625bbabd7052f858de5400a2e))
* require API authentication and add operation modes ([#18](https://github.com/murasame29/workload-knocker/issues/18)) ([7c28f69](https://github.com/murasame29/workload-knocker/commit/7c28f6992164a45b88dbdfe461e272162e01d859))


### Code Refactoring

* extract a chat frontend layer from the Slack package ([#19](https://github.com/murasame29/workload-knocker/issues/19)) ([4d56184](https://github.com/murasame29/workload-knocker/commit/4d561840bf29e3d330ee12b7ba73be7a1d7bab0c))
* remove New Relic and expose Prometheus metrics only ([#17](https://github.com/murasame29/workload-knocker/issues/17)) ([ba0d739](https://github.com/murasame29/workload-knocker/commit/ba0d739bed5952833463681767d6de24bbc6f9f4))
