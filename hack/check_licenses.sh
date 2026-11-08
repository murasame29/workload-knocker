#!/usr/bin/env bash
set -euo pipefail

go_licenses='github.com/google/go-licenses/v2@v2.0.1'
allowed_licenses='Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC'

tool_dir="$(mktemp -d)"
trap 'rm -rf "${tool_dir}"' EXIT
GOBIN="${tool_dir}" go install "${go_licenses}"

for arch in amd64 arm64; do
  echo "checking Go dependency licenses for linux/${arch}"
  GOOS=linux GOARCH="${arch}" CGO_ENABLED=0 \
    "${tool_dir}/go-licenses" check \
      --allowed_licenses="${allowed_licenses}" \
      ./cmd/server
done

cmp --silent LICENSE charts/workload-knocker/LICENSE
cmp --silent NOTICE charts/workload-knocker/NOTICE

grep --fixed-strings --quiet \
  'artifacthub.io/license: Apache-2.0' \
  charts/workload-knocker/Chart.yaml
