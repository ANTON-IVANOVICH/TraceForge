.PHONY: tidy build test vet lint lint-install run-server run-agent run-ctl proto proto-tools install-ctl

GOBIN := $(shell go env GOPATH)/bin

# Version reported by `metricsctl version`, taken from the SemVer tag when there
# is one. -s -w drops the symbol table and DWARF info, shrinking the binary.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

tidy:
	go mod tidy

# Install the protoc Go plugins used by `make proto`.
proto-tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# Regenerate protobuf + gRPC code from proto/ into internal/proto/metricspb.
# Requires protoc on PATH and `make proto-tools` already run.
proto:
	PATH="$(GOBIN):$$PATH" protoc \
		--go_out=. --go_opt=module=metrics-system \
		--go-grpc_out=. --go-grpc_opt=module=metrics-system \
		proto/metrics/v1/metrics.proto

build:
	mkdir -p bin
	go build -o bin/server ./cmd/server
	go build -o bin/agent ./cmd/agent
	go build -ldflags '$(LDFLAGS)' -o bin/metricsctl ./cmd/metricsctl

# Install metricsctl onto $GOPATH/bin, so it lands on PATH.
install-ctl:
	go install -ldflags '$(LDFLAGS)' ./cmd/metricsctl

test:
	go test -race -cover ./...

vet:
	go vet ./...

lint-install:
	@mkdir -p bin
	@if [ ! -x ./bin/golangci-lint ]; then \
		curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b ./bin v2.12.2; \
	fi

lint: lint-install
	@mkdir -p .cache/golangci-lint .cache/go-build
	GOCACHE=$(CURDIR)/.cache/go-build GOLANGCI_LINT_CACHE=$(CURDIR)/.cache/golangci-lint ./bin/golangci-lint run ./...

run-server:
	go run ./cmd/server

run-agent:
	go run ./cmd/agent -server=http://localhost:8080/api/v1/metrics

run-ctl:
	go run -ldflags '$(LDFLAGS)' ./cmd/metricsctl $(ARGS)
