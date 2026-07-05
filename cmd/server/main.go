package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"metrics-system/internal/server"
	"metrics-system/internal/server/grpcserver"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/ratelimit"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/server/storage/bolt"
	"metrics-system/internal/server/storage/tsdb"
)

func main() {
	cfg := loadConfig()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(cfg.logLevel)}))

	store, err := openStorage(cfg, logger)
	if err != nil {
		logger.Error("open storage failed", "type", cfg.storageType, "error", err)
		os.Exit(1)
	}

	pipe := pipeline.New(store, pipeline.Config{
		IngestBuffer:    cfg.ingestBuffer,
		ValidateWorkers: cfg.validateWorkers,
		EnrichWorkers:   cfg.enrichWorkers,
		StoreWorkers:    cfg.storeWorkers,
	}, logger)

	limiter := ratelimit.New(cfg.rateLimitRPS, cfg.rateLimitBurst)
	handler := server.NewHandler(pipe, store, logger)
	routes := server.Chain(
		handler.Routes(),
		server.Recover(logger), // outermost: catches panics from everything below
		server.RequestID,
		server.Logger(logger),
		server.RateLimit(limiter),
	)
	srv := server.New(cfg.addr, routes, logger)

	// Optional gRPC transport, sharing the same pipeline and store as HTTP.
	var grpcSrv *grpcserver.Server
	if cfg.grpcAddr != "" {
		svc := grpcserver.NewService(pipe, store, logger)
		grpcSrv, err = grpcserver.New(cfg.grpcAddr, svc, logger)
		if err != nil {
			logger.Error("grpc listen failed", "addr", cfg.grpcAddr, "error", err)
			os.Exit(1)
		}
	}

	// signal.NotifyContext's stop only detaches the signal relay; use a separate
	// cancel so one server's failure can bring the other down too.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	pipe.Start()

	// Run both transports concurrently. Each cancels the shared context on
	// failure so the other drains too.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	run := func(name string, r interface{ Run(context.Context) error }) {
		defer wg.Done()
		if err := r.Run(ctx); err != nil {
			errs <- fmt.Errorf("%s: %w", name, err)
			cancel()
		}
	}

	wg.Add(1)
	go run("http", srv)
	if grpcSrv != nil {
		wg.Add(1)
		go run("grpc", grpcSrv)
		logger.Info("server started", "http_addr", cfg.addr, "grpc_addr", cfg.grpcAddr, "storage", cfg.storageType)
	} else {
		logger.Info("server started", "http_addr", cfg.addr, "storage", cfg.storageType)
	}

	wg.Wait()
	close(errs)

	// Both servers are fully stopped => no handler is still running, so no Ingest
	// call can race the drain below.
	pipe.Shutdown()

	// All ingested metrics are now flushed into storage; close it (flush WAL,
	// release the file lock, close the DB).
	if err := store.Close(); err != nil {
		logger.Error("storage close failed", "error", err)
	}

	var failed bool
	for err := range errs {
		logger.Error("server terminated", "error", err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	logger.Info("server stopped")
}

// openStorage builds the storage backend selected by -storage.
func openStorage(cfg config, logger *slog.Logger) (storage.Storage, error) {
	switch cfg.storageType {
	case "memory":
		return storage.NewMemoryStorage(), nil
	case "bolt":
		if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
			return nil, err
		}
		return bolt.New(filepath.Join(cfg.dataDir, "metrics.bolt"))
	case "tsdb":
		return tsdb.Open(filepath.Join(cfg.dataDir, "tsdb"), logger)
	default:
		return nil, fmt.Errorf("unknown storage type %q (want memory|bolt|tsdb)", cfg.storageType)
	}
}

// config holds the resolved server settings. Priority: defaults -> env -> flags.
type config struct {
	addr            string
	grpcAddr        string
	logLevel        string
	storageType     string
	dataDir         string
	ingestBuffer    int
	validateWorkers int
	enrichWorkers   int
	storeWorkers    int
	rateLimitRPS    float64
	rateLimitBurst  int
}

func loadConfig() config {
	cfg := config{
		addr:            envString("SERVER_ADDR", ":8080"),
		grpcAddr:        envString("GRPC_ADDR", ":9090"),
		logLevel:        envString("SERVER_LOG_LEVEL", "info"),
		storageType:     envString("STORAGE", "memory"),
		dataDir:         envString("DATA_DIR", "./data"),
		ingestBuffer:    envInt("INGEST_BUFFER", 1000),
		validateWorkers: envInt("VALIDATE_WORKERS", runtime.NumCPU()),
		enrichWorkers:   envInt("ENRICH_WORKERS", runtime.NumCPU()),
		storeWorkers:    envInt("STORE_WORKERS", 1),
		rateLimitRPS:    envFloat("RATE_LIMIT_RPS", 100),
		rateLimitBurst:  envInt("RATE_LIMIT_BURST", 200),
	}

	flag.StringVar(&cfg.addr, "addr", cfg.addr, "HTTP listen address")
	flag.StringVar(&cfg.grpcAddr, "grpc-addr", cfg.grpcAddr, "gRPC listen address (empty to disable)")
	flag.StringVar(&cfg.logLevel, "log-level", cfg.logLevel, "log level: debug|info|warn|error")
	flag.StringVar(&cfg.storageType, "storage", cfg.storageType, "storage backend: memory|bolt|tsdb")
	flag.StringVar(&cfg.dataDir, "data-dir", cfg.dataDir, "data directory for persistent backends")
	flag.IntVar(&cfg.ingestBuffer, "ingest-buffer", cfg.ingestBuffer, "ingest channel buffer size")
	flag.IntVar(&cfg.validateWorkers, "validate-workers", cfg.validateWorkers, "number of validate workers")
	flag.IntVar(&cfg.enrichWorkers, "enrich-workers", cfg.enrichWorkers, "number of enrich workers")
	flag.IntVar(&cfg.storeWorkers, "store-workers", cfg.storeWorkers, "number of store workers")
	flag.Float64Var(&cfg.rateLimitRPS, "rate-limit-rps", cfg.rateLimitRPS, "per-agent requests per second")
	flag.IntVar(&cfg.rateLimitBurst, "rate-limit-burst", cfg.rateLimitBurst, "per-agent burst")
	flag.Parse()

	return cfg
}

func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envString(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envFloat(key string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}
