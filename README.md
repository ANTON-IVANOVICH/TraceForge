# metrics-system

MVP распределённой системы метрик на Go.

Агент:

- раз в N секунд собирает CPU, RAM, disk, uptime
- формирует `Batch` и отправляет на сервер по HTTP

Сервер:

- принимает метрики (`POST /api/v1/metrics`)
- хранит метрики в памяти
- отдаёт метрики (`GET /api/v1/metrics`)
- health-check (`GET /healthz`)

## Layout

```text
.
├── cmd/
│   ├── agent/
│   │   └── main.go
│   └── server/
│       └── main.go
├── internal/
│   ├── agent/
│   │   ├── collector.go
│   │   ├── cpu.go
│   │   ├── disk.go
│   │   ├── memory.go
│   │   ├── uptime.go
│   │   ├── sender.go
│   │   └── agent.go
│   ├── model/
│   │   └── metric.go
│   └── server/
│       ├── handler.go
│       ├── storage.go
│       └── server.go
└── pkg/
    └── httpx/
        └── client.go
```

## Запуск

```bash
make tidy
make test
```

Терминал 1:

```bash
make run-server
```

Терминал 2:

```bash
make run-agent
```

Проверка:

```bash
curl -s http://localhost:8080/api/v1/metrics | jq
curl -i http://localhost:8080/healthz
```

## Конфигурация (flags + env)

Server:

- `-addr` или `SERVER_ADDR` (default `:8080`)
- `-log-level` или `SERVER_LOG_LEVEL` (debug|info|warn|error)

Agent:

- `-server` или `AGENT_SERVER` (default `http://localhost:8080/api/v1/metrics`)
- `-interval` или `AGENT_INTERVAL` (default `5s`)
- `-id` или `AGENT_ID` (default hostname)
- `-disk-path` или `AGENT_DISK_PATH` (default `/`)
- `-http-timeout` или `AGENT_HTTP_TIMEOUT` (default `10s`)
- `-http-retries` или `AGENT_HTTP_RETRIES` (default `2`)
- `-http-backoff` или `AGENT_HTTP_BACKOFF` (default `200ms`)
- `-log-level` или `AGENT_LOG_LEVEL` (debug|info|warn|error)
