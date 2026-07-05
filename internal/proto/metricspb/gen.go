package metricspb

// Regenerate the protobuf + gRPC Go code from the .proto definition.
// Prefer `make proto` (it puts the protoc-gen-go* plugins on PATH); this
// directive lets `go generate ./...` do the same when the plugins are already
// installed (see `make proto-tools`).
//
//go:generate protoc --go_out=../../../ --go_opt=module=metrics-system --go-grpc_out=../../../ --go-grpc_opt=module=metrics-system --proto_path=../../../ proto/metrics/v1/metrics.proto
