.PHONY: tidy build test vet lint lint-install run-server run-agent

tidy:
	go mod tidy

build:
	mkdir -p bin
	go build -o bin/server ./cmd/server
	go build -o bin/agent ./cmd/agent

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
