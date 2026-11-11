# Changelog

## [0.1.9](https://github.com/murasame29/workload-knocker/compare/v0.1.8...v0.1.9) (2026-11-11)


### Features

* **discord:** select workloads in a modal across namespaces ([#73](https://github.com/murasame29/workload-knocker/issues/73)) ([029a953](https://github.com/murasame29/workload-knocker/commit/029a95340e2e36bb2b8fdc3c8740a24c8ca62b32)), closes [#32](https://github.com/murasame29/workload-knocker/issues/32)

## [0.1.8](https://github.com/murasame29/workload-knocker/compare/v0.1.7...v0.1.8) (2026-11-11)


### Features

* show actuator refusals such as GitOps conflicts in chat ([#69](https://github.com/murasame29/workload-knocker/issues/69)) ([50c518f](https://github.com/murasame29/workload-knocker/commit/50c518f2c03aa25a48b731d943959671cfa94a00)), closes [#63](https://github.com/murasame29/workload-knocker/issues/63)

## [0.1.7](https://github.com/murasame29/workload-knocker/compare/v0.1.6...v0.1.7) (2026-11-11)


### Features

* combine several actuators per cluster ([#66](https://github.com/murasame29/workload-knocker/issues/66)) ([47a263d](https://github.com/murasame29/workload-knocker/commit/47a263d7984e5b5150f15eacf86b94843f0a1a56)), closes [#65](https://github.com/murasame29/workload-knocker/issues/65)

## [0.1.6](https://github.com/murasame29/workload-knocker/compare/v0.1.5...v0.1.6) (2026-11-10)


### Features

* direct actuator without KEDA and Prometheus ([#59](https://github.com/murasame29/workload-knocker/issues/59)) ([6474d7f](https://github.com/murasame29/workload-knocker/commit/6474d7fdd28ce290b69b54e08ac542ff5de25bc8))


### Bug Fixes

* **store:** keep the state of each cluster env under its own key ([#62](https://github.com/murasame29/workload-knocker/issues/62)) ([cf839b5](https://github.com/murasame29/workload-knocker/commit/cf839b50b6a76824c7223e034c9a3516c3494734))

## [0.1.5](https://github.com/murasame29/workload-knocker/compare/v0.1.4...v0.1.5) (2026-11-10)


### Features

* KEDA external-push actuator without Prometheus ([#60](https://github.com/murasame29/workload-knocker/issues/60)) ([2daf58d](https://github.com/murasame29/workload-knocker/commit/2daf58d3d7f18d813d7af5246c1c3adc86a0af1f))

## [0.1.4](https://github.com/murasame29/workload-knocker/compare/v0.1.3...v0.1.4) (2026-11-10)


### Bug Fixes

* **chart:** make the API token Secret optional unless nightly stop is enabled ([#55](https://github.com/murasame29/workload-knocker/issues/55)) ([cc82efd](https://github.com/murasame29/workload-knocker/commit/cc82efd4b2d804e762e289e73c162d850e7404e5))

## [0.1.3](https://github.com/murasame29/workload-knocker/compare/v0.1.2...v0.1.3) (2026-11-08)


### Bug Fixes

* drop the hard-coded auto-stop line from result messages ([#53](https://github.com/murasame29/workload-knocker/issues/53)) ([a8f5dca](https://github.com/murasame29/workload-knocker/commit/a8f5dcaeb32c7055ff6c16d80d7c6aafc1b956b6))

## [0.1.2](https://github.com/murasame29/workload-knocker/compare/v0.1.1...v0.1.2) (2026-11-08)


### Bug Fixes

* **chart:** bump appVersion together with the chart version ([#50](https://github.com/murasame29/workload-knocker/issues/50)) ([6affa9f](https://github.com/murasame29/workload-knocker/commit/6affa9f2c5eb2a4dd3b45372a3ebbe35166a4c2f))

## [0.1.1](https://github.com/murasame29/workload-knocker/compare/v0.1.0...v0.1.1) (2026-11-08)


### ⚠ BREAKING CHANGES

* the Discord command now has the subcommands start and status; run /<command> start for the previous flow.
* log keys time and msg are renamed to timestamp and message, and every line carries service.name and service.version.

### Features

* emit structured audit events with a stable schema ([#43](https://github.com/murasame29/workload-knocker/issues/43)) ([dabbb20](https://github.com/murasame29/workload-knocker/commit/dabbb20fce12e143b4fc1d49c1bc6a6d82c8244c))
* ready message and optional notify channel ([#47](https://github.com/murasame29/workload-knocker/issues/47)) ([1bd37cf](https://github.com/murasame29/workload-knocker/commit/1bd37cff32efede5c4ba3ba9f5ece31e36d39172))
* register running workloads on discovery and reconcile in the background ([#42](https://github.com/murasame29/workload-knocker/issues/42)) ([6481465](https://github.com/murasame29/workload-knocker/commit/6481465af22e3b400a043f01780fdf24123bf5f4))
* run duration per start with auto-stop, reminder and extend ([#46](https://github.com/murasame29/workload-knocker/issues/46)) ([94ae486](https://github.com/murasame29/workload-knocker/commit/94ae486f0f787e77b0fe6add9efac9b7b5c41266))
* show per-workload rollout progress after Confirm ([#44](https://github.com/murasame29/workload-knocker/issues/44)) ([e718357](https://github.com/murasame29/workload-knocker/commit/e718357c75e84201205d00da0721e58f5f18a747))
* status command listing running workloads and stop times ([#49](https://github.com/murasame29/workload-knocker/issues/49)) ([140aa26](https://github.com/murasame29/workload-knocker/commit/140aa263442a52113d3252aa732405dbf51fa695))

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
