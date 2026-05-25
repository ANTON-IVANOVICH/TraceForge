package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"metrics-system/internal/server"
)

func main() {
	defaultAddr := envString("SERVER_ADDR", ":8080")
	defaultLevel := envString("SERVER_LOG_LEVEL", "info")

	var (
		addr     = flag.String("addr", defaultAddr, "server listen address")
		logLevel = flag.String("log-level", defaultLevel, "log level: debug|info|warn|error")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))

	storage := server.NewStorage()
	handler := server.NewHandler(storage, logger)
	srv := server.New(*addr, handler.Routes(), logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("server started", "addr", *addr)
	if err := srv.Run(ctx); err != nil {
		logger.Error("server terminated", "error", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
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
