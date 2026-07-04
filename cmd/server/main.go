package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"metrics-system/internal/server"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/ratelimit"
	"metrics-system/internal/server/storage"
)

func main() {
	cfg := loadConfig()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(cfg.logLevel)}))

	store := storage.NewMemoryStorage()
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pipe.Start()

	logger.Info("server started", "addr", cfg.addr)
	runErr := srv.Run(ctx)

	// srv.Run has returned => the HTTP server is fully stopped and no handler is
	// still running, so no Ingest call can race the drain below.
	pipe.Shutdown()

	if runErr != nil {
		logger.Error("server terminated", "error", runErr)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

// config holds the resolved server settings. Priority: defaults -> env -> flags.
type config struct {
	addr            string
	logLevel        string
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
		logLevel:        envString("SERVER_LOG_LEVEL", "info"),
		ingestBuffer:    envInt("INGEST_BUFFER", 1000),
		validateWorkers: envInt("VALIDATE_WORKERS", runtime.NumCPU()),
		enrichWorkers:   envInt("ENRICH_WORKERS", runtime.NumCPU()),
		storeWorkers:    envInt("STORE_WORKERS", 1),
		rateLimitRPS:    envFloat("RATE_LIMIT_RPS", 100),
		rateLimitBurst:  envInt("RATE_LIMIT_BURST", 200),
	}

	flag.StringVar(&cfg.addr, "addr", cfg.addr, "listen address")
	flag.StringVar(&cfg.logLevel, "log-level", cfg.logLevel, "log level: debug|info|warn|error")
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
