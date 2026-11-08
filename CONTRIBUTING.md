# Contributing

## Development setup

- Install the Go version declared in `go.mod`.
- `make test` runs `go vet` and `go test -race`.
- `make test-property` runs the property-based tests.
- `make docker-build` builds the container image locally.
- `helm lint charts/workload-knocker` lints the chart.

## Commits and pull requests

Commit messages and PR titles follow [Conventional Commits](https://www.conventionalcommits.org/).

- Types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
- Scope examples: `chart`, `store`, `slack`, `api`.
- Append `!` for breaking changes, for example `feat(api)!: require auth`.

PRs are squash merged, so the PR title becomes the commit message. Link issues with
`Closes #N` in the PR body.

## Changelog and releases

Release-please generates `CHANGELOG.md` and releases from commit messages. Do not edit
`CHANGELOG.md` by hand.
