BINARY   := server
CMD_PATH := ./cmd/server
IMAGE    ?= ghcr.io/murasame29/workload-knocker
TAG      ?= dev

.PHONY: all test test-property build docker-build licenses clean

all: test build

## test: go vet + go test (race detector)
test:
	go vet ./...
	go test -race ./...

## test-property: property-based tests (pgregory.net/rapid)
test-property:
	go test ./... -run Property -v

## build: build the binary
build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) $(CMD_PATH)

## docker-build: build the container image locally
docker-build:
	docker build -t $(IMAGE):$(TAG) .

## licenses: verify dependency licenses
licenses:
	bash hack/check_licenses.sh

clean:
	rm -f $(BINARY)
