.PHONY: tidy build test vet lint lint-install run-server run-agent run-ctl proto proto-tools install-ctl \
	test-unit test-integration test-e2e test-all cover cover-html fuzz fuzz-long bench bench-save bench-compare \
	mutate profile-cpu profile-heap profile-trace tools

GOBIN := $(shell go env GOPATH)/bin

# Version reported by `metricsctl version`, taken from the SemVer tag when there
# is one. -s -w drops the symbol table and DWARF info, shrinking the binary.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# -count=1 disables Go's test result cache. The cache is a fine default locally
# and a liability everywhere else: it hides flakes, which are exactly the tests
# worth knowing about.
GOTEST := go test -count=1

# Where benchmark runs and profiles land. Both are gitignored.
BENCHDIR ?= .bench
PROFDIR  ?= .prof

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

# The development tools this repo ships rather than depends on: a benchmark
# comparator and a mutation tester.
tools:
	@mkdir -p bin
	go build -o bin/benchcmp ./cmd/benchcmp
	go build -o bin/mutate ./cmd/mutate

# Install metricsctl onto $GOPATH/bin, so it lands on PATH.
install-ctl:
	go install -ldflags '$(LDFLAGS)' ./cmd/metricsctl

# ---------------------------------------------------------------------------
# The test pyramid. `make test` runs the wide, fast base; the narrower, slower
# layers are opt-in, because a suite that takes ten minutes is a suite that
# developers stop running.
# ---------------------------------------------------------------------------

test: test-unit

# Unit: no build tag, no IO beyond t.TempDir(), milliseconds each. -race always:
# a data race is not reproducible on demand, but the detector finds it every time.
test-unit:
	$(GOTEST) -race ./...

# Integration: one component against one real dependency (a bbolt file on disk,
# an httptest server, a bufconn gRPC link). Seconds each.
test-integration:
	$(GOTEST) -race -tags=integration ./...

# End-to-end: the real binaries, as separate processes. Tens of seconds each.
# No -race here: it instruments this process, not the children it spawns, so it
# would cost time and prove nothing.
test-e2e:
	$(GOTEST) -tags=e2e -timeout=15m ./test/e2e/...

test-all: test-unit test-integration test-e2e

vet:
	go vet ./...
	go vet -tags=integration ./...
	go vet -tags=e2e ./test/e2e/...

lint-install:
	@mkdir -p bin
	@if [ ! -x ./bin/golangci-lint ]; then \
		curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b ./bin v2.12.2; \
	fi

lint: lint-install
	@mkdir -p .cache/golangci-lint .cache/go-build
	GOCACHE=$(CURDIR)/.cache/go-build GOLANGCI_LINT_CACHE=$(CURDIR)/.cache/golangci-lint ./bin/golangci-lint run ./...

# ---------------------------------------------------------------------------
# Coverage. An indicator, never a target: a test that calls a function and
# asserts nothing covers every line of it. Use `make mutate` to ask whether the
# covered lines are actually checked.
# ---------------------------------------------------------------------------

# -covermode=atomic is mandatory alongside -race; the default `set` mode races
# on its own counters.
cover:
	$(GOTEST) -race -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

cover-html: cover
	go tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html"

# ---------------------------------------------------------------------------
# Fuzzing. A plain `go test` already replays every corpus entry under
# testdata/fuzz, so these targets exist to find *new* inputs.
# ---------------------------------------------------------------------------

FUZZTIME ?= 15s

# A short smoke fuzz of every target. Go fuzzes one target at a time, so they
# run in sequence.
fuzz:
	@for pkg in $$(go list ./...); do \
		for target in $$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
			echo "==> $$target ($$pkg)"; \
			go test -run=XXX -fuzz="^$$target$$" -fuzztime=$(FUZZTIME) $$pkg || exit 1; \
		done; \
	done

# The nightly run. A fuzzer that finds nothing in fifteen seconds often finds
# something in ten minutes: coverage-guided mutation needs time to reach the
# deep branches.
fuzz-long:
	$(MAKE) fuzz FUZZTIME=10m

# ---------------------------------------------------------------------------
# Benchmarks. -benchmem always: B/op and allocs/op matter more than ns/op on a
# hot path, because allocations feed the GC and the GC stops the world.
# ---------------------------------------------------------------------------

bench:
	go test -run=XXX -bench=. -benchmem ./...

# -count=10 is the point of bench-save. One run of a benchmark is an anecdote;
# ten runs are a sample that benchcmp can test for significance.
BENCH ?= .
NAME  ?= run
bench-save: tools
	@mkdir -p $(BENCHDIR)
	go test -run=XXX -bench='$(BENCH)' -benchmem -count=10 ./... | tee $(BENCHDIR)/$(NAME).txt
	@echo "saved $(BENCHDIR)/$(NAME).txt"

# Usage:
#   git switch main   && make bench-save NAME=base
#   git switch mybranch && make bench-save NAME=new
#   make bench-compare OLD=base NEW=new
bench-compare: tools
	./bin/benchcmp $(BENCHDIR)/$(OLD).txt $(BENCHDIR)/$(NEW).txt

# ---------------------------------------------------------------------------
# Mutation testing: does the suite check the lines it covers, or merely run
# them? One `go test` per mutant, so scope it to a package and run it nightly.
#   make mutate PKG=./internal/alerting/rules
# ---------------------------------------------------------------------------

PKG ?= ./internal/server/storage
mutate: tools
	./bin/mutate -workers $$(( $$(getconf _NPROCESSORS_ONLN) / 2 )) $(PKG)

# ---------------------------------------------------------------------------
# Profiling. A benchmark says what is slow; a profile says where inside it.
# ---------------------------------------------------------------------------

PROF_PKG   ?= ./internal/server/storage
PROF_BENCH ?= .

profile-cpu:
	@mkdir -p $(PROFDIR)
	go test -run=XXX -bench='$(PROF_BENCH)' -benchtime=3s -cpuprofile=$(PROFDIR)/cpu.prof $(PROF_PKG)
	go tool pprof -http=:8081 $(PROFDIR)/cpu.prof

# alloc_space, not the default inuse_space: on a benchmark nothing is retained,
# so inuse shows an empty heap while alloc shows every allocation the hot loop
# made — which is what you came to see.
profile-heap:
	@mkdir -p $(PROFDIR)
	go test -run=XXX -bench='$(PROF_BENCH)' -benchtime=3s -memprofile=$(PROFDIR)/mem.prof $(PROF_PKG)
	go tool pprof -http=:8081 -sample_index=alloc_space $(PROFDIR)/mem.prof

# The execution tracer: every goroutine, every GC, every syscall, on a timeline.
# The tool for "why does this not scale across cores" — pprof cannot answer that,
# because idle cores consume no samples.
profile-trace:
	@mkdir -p $(PROFDIR)
	go test -run=XXX -bench='$(PROF_BENCH)' -benchtime=3s -trace=$(PROFDIR)/trace.out $(PROF_PKG)
	go tool trace $(PROFDIR)/trace.out

# ---------------------------------------------------------------------------

run-server:
	go run ./cmd/server

run-agent:
	go run ./cmd/agent -server=http://localhost:8080/api/v1/metrics

run-ctl:
	go run -ldflags '$(LDFLAGS)' ./cmd/metricsctl $(ARGS)
