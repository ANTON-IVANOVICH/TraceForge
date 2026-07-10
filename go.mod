module metrics-system

go 1.26.2

// The six standard-library advisories govulncheck reports against go1.26.2 are
// all fixed by go1.26.5, and none of them can be fixed any other way: the code is
// in crypto/tls, net/http, html/template and net. Pinning the toolchain here means
// `go build` fetches it, so a developer on an older Go does not silently produce a
// vulnerable binary. `make vuln` is the check that keeps this honest.
toolchain go1.26.5

require (
	github.com/shirou/gopsutil/v4 v4.26.4
	github.com/spf13/cobra v1.10.2
	go.etcd.io/bbolt v1.5.0
	golang.org/x/sys v0.47.0
	golang.org/x/term v0.45.0
	golang.org/x/time v0.15.0
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.10
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)
