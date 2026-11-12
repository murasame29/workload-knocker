ARG GO_VERSION=1.27.4
ARG DEBIAN_VERSION=trixie
ARG GO_LICENSES_VERSION=v2.0.1

# build
FROM golang:${GO_VERSION}-${DEBIAN_VERSION} AS build
ARG GO_LICENSES_VERSION
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
SHELL ["/bin/bash", "-eo", "pipefail", "-c"]
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -buildvcs=false \
    -o /build/main \
    -ldflags="-w -s \
      -X github.com/murasame29/workload-knocker/internal/observability/version.Version=${VERSION} \
      -X github.com/murasame29/workload-knocker/internal/observability/version.Commit=${COMMIT} \
      -X github.com/murasame29/workload-knocker/internal/observability/version.BuildDate=${BUILD_DATE}" \
    ./cmd/server && \
    go_root="$(go env GOROOT)" && \
    go_version="$(go env GOVERSION)" && \
    mkdir -p /build/licenses/workload-knocker && \
    CGO_ENABLED=0 go run "github.com/google/go-licenses/v2@${GO_LICENSES_VERSION}" report \
      --ignore github.com/murasame29/workload-knocker \
      ./cmd/server \
      > /build/licenses/workload-knocker/THIRD_PARTY_COMPONENTS.csv && \
    printf '%s,%s,%s\n' \
      "go.dev/stdlib@${go_version}" \
      'https://go.dev/LICENSE' \
      'BSD-3-Clause' \
      >> /build/licenses/workload-knocker/THIRD_PARTY_COMPONENTS.csv && \
    CGO_ENABLED=0 go run "github.com/google/go-licenses/v2@${GO_LICENSES_VERSION}" save \
      --ignore github.com/murasame29/workload-knocker \
      --save_path=/build/licenses/third_party \
      ./cmd/server && \
    mkdir -p /build/licenses/third_party/go.dev/go && \
    cp "${go_root}/LICENSE" "${go_root}/PATENTS" "${go_root}/VERSION" \
      /build/licenses/third_party/go.dev/go/ && \
    cp LICENSE NOTICE /build/licenses/workload-knocker/

# prod
FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /app
COPY --from=build --chmod=755 /build/main /app/main
COPY --from=build /build/licenses /licenses
USER 1000
ENTRYPOINT ["/app/main"]
