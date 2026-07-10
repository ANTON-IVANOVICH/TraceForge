package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"metrics-system/internal/alerting"
	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/auth"
	"metrics-system/internal/buildinfo"
	"metrics-system/internal/clock"
	"metrics-system/internal/container"
	"metrics-system/internal/promexport"
	"metrics-system/internal/server"
	"metrics-system/internal/server/grpcserver"
	"metrics-system/internal/server/health"
	"metrics-system/internal/server/live"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/ratelimit"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/server/storage/bolt"
	"metrics-system/internal/server/storage/tsdb"
	"metrics-system/internal/telemetry"
)

func main() {
	cfg := loadConfig()

	// Both of these must answer before anything is opened, bound or created:
	// -version is run on a laptop, and -health-check is run by the container
	// runtime inside a container that already has a server in it.
	if cfg.showVersion {
		fmt.Println("traceforge-server", buildinfo.Get())
		return
	}
	if cfg.healthCheck {
		if err := telemetry.SelfCheck(context.Background(), cfg.telemetryAddr); err != nil {
			fmt.Fprintln(os.Stderr, "health check failed:", err)
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(cfg.logLevel)}))
	logger.Info("traceforge server", "build", buildinfo.Get().String())

	// GOMEMLIMIT from the cgroup, if there is one. GOMAXPROCS is deliberately not
	// touched: the runtime already derives it from the CPU quota and keeps it up
	// to date, and setting it would freeze that. See internal/container.
	container.ApplyMemoryLimit(cfg.memoryLimitRatio, logger)

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

	// Build the authenticator (nil when auth is disabled — the default).
	authn, keySets, err := buildAuthenticator(cfg, logger)
	if err != nil {
		logger.Error("auth setup failed", "error", err)
		os.Exit(1)
	}

	limiter := ratelimit.New(cfg.rateLimitRPS, cfg.rateLimitBurst)
	handler := server.NewHandler(pipe, store, logger)

	// Optional alerting: evaluates rules against storage and notifies receivers.
	// Built before the UI so its alerts can be tapped into the dashboard.
	var alertSvc *alerting.Service
	if cfg.alertingEnabled {
		alertSvc, err = alerting.New(alerting.Config{
			RulesFile:   cfg.alertRulesFile,
			ConfigFile:  cfg.alertConfigFile,
			Lookback:    cfg.alertLookback,
			AlertBuffer: cfg.alertBuffer,
		}, store, clock.New(), logger)
		if err != nil {
			logger.Error("alerting setup failed", "error", err)
			os.Exit(1)
		}
		handler.SetAlerting(alertSvc)
	}

	// Optional embedded live dashboard: the hub taps the pipeline's store stage
	// and pushes updates over WebSocket to connected browsers.
	var hub *live.Hub
	if cfg.uiEnabled {
		hub = live.NewHub(logger)
		pipe.SetObserver(hub.PublishMetrics) // must be before pipe.Start
		handler.SetUI(hub, authn)
		if alertSvc != nil {
			alertSvc.SetObserver(func(a *alert.Alert) { hub.PublishAlert(toAlertEvent(a)) })
		}
	}
	// metrics (outer) -> recover -> request id -> logging -> rate limit -> [auth].
	//
	// Metrics sits outside Recover on purpose. Recover turns a panic into a 500 and
	// returns normally, so a metrics middleware wrapped *inside* it would never run
	// the code after next.ServeHTTP for a panicking request — and the requests that
	// panic are precisely the ones you want in the error rate. Outside, the panic is
	// already a 500 by the time it is counted.
	mux := handler.Routes()
	httpMetrics := server.NewHTTPMetrics()
	mws := []server.Middleware{
		httpMetrics.Middleware(mux),
		server.Recover(logger),
		server.RequestID,
		server.Logger(logger),
		server.RateLimit(limiter),
	}
	if authn != nil {
		mws = append(mws, server.Authenticate(authn, logger))
	}
	srv, err := server.New(cfg.addr, server.Chain(mux, mws...), logger)
	if err != nil {
		logger.Error("http listen failed", "addr", cfg.addr, "error", err)
		os.Exit(1)
	}

	// Optional gRPC transport, sharing the same pipeline, store and auth as HTTP.
	var grpcSrv *grpcserver.Server
	if cfg.grpcAddr != "" {
		svc := grpcserver.NewService(pipe, store, logger)
		grpcSrv, err = grpcserver.New(cfg.grpcAddr, svc, authn, logger)
		if err != nil {
			logger.Error("grpc listen failed", "addr", cfg.grpcAddr, "error", err)
			os.Exit(1)
		}
	}

	// Optional profiling listener, on its own port so pprof is never reachable
	// from wherever the API is.
	server.EnableProfileSampling(cfg.mutexProfileFraction, cfg.blockProfileRate, logger)
	var pprofSrv *server.Server
	if cfg.pprofAddr != "" {
		pprofSrv, err = server.NewProfilingServer(cfg.pprofAddr, logger)
		if err != nil {
			logger.Error("pprof listen failed", "addr", cfg.pprofAddr, "error", err)
			os.Exit(1)
		}
	}

	// The admin surface: the three probes and the Prometheus scrape, on a port an
	// operator can firewall and a user never sees.
	healthz := health.New(logger, health.Options{})
	healthz.Register("storage", store.Ping)

	gatherers := []promexport.Gatherer{
		server.BuildInfoGatherer(),
		telemetry.RuntimeGatherer(),
		server.PipelineGatherer(pipe),
		server.StorageGatherer(store),
		server.RateLimitGatherer(limiter),
		httpMetrics,
	}
	if alertSvc != nil {
		gatherers = append(gatherers, server.AlertingGatherer(alertSvc))
	}

	var telSrv *telemetry.Server
	if cfg.telemetryAddr != "" {
		telSrv, err = telemetry.New(telemetry.Config{
			Addr:      cfg.telemetryAddr,
			Health:    healthz,
			Gatherers: gatherers,
		}, logger)
		if err != nil {
			logger.Error("telemetry listen failed", "addr", cfg.telemetryAddr, "error", err)
			os.Exit(1)
		}
	}

	// Three contexts, because shutdown has three phases and they must not collapse
	// into one.
	//
	//   sigCtx  — cancelled by SIGTERM. Nothing serves off it directly.
	//   runCtx  — the API, gRPC and alerting. Cancelled after the drain delay.
	//   telCtx  — the probes and the scrape. Cancelled last, so the kubelet can
	//             still read "not ready" while the API drains, and so the final
	//             scrape before the pod disappears is not a connection refused.
	//
	// Deriving runCtx from sigCtx, as this used to, would cancel it the instant the
	// signal arrived — which is exactly the delay this rework exists to introduce.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	telCtx, stopTel := context.WithCancel(context.Background())
	defer stopTel()

	// Periodic JWKS refresh for RS256 key rotation.
	for _, ks := range keySets {
		go ks.Refresh(runCtx, logger)
	}

	// Live dashboard: run the hub and periodically push stats snapshots.
	if hub != nil {
		go hub.Run(runCtx)
		go publishStatsLoop(runCtx, hub, pipe, store)
	}

	pipe.Start()

	// Probe once before anything can be scraped, so the first /readyz poll reports
	// a measurement rather than "pending".
	healthz.Probe(runCtx)
	go func() { _ = healthz.Run(telCtx) }()

	// Run the transports concurrently. Each cancels the shared context on failure
	// so the others drain too.
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	run := func(name string, ctx context.Context, r interface{ Run(context.Context) error }) {
		defer wg.Done()
		if err := r.Run(ctx); err != nil {
			errs <- fmt.Errorf("%s: %w", name, err)
			stopRun()
		}
	}

	wg.Add(1)
	go run("http", runCtx, srv)

	// Log the addresses actually bound, not the ones requested: with ":0" the
	// kernel picks the port, and a test (or an operator) needs to be told which.
	// These lines are the e2e suite's readiness signal.
	fields := []any{"http_addr", srv.Addr(), "storage", cfg.storageType, "gomaxprocs", container.GOMAXPROCS()}
	if grpcSrv != nil {
		wg.Add(1)
		go run("grpc", runCtx, grpcSrv)
		fields = append(fields, "grpc_addr", grpcSrv.Addr())
	}
	if pprofSrv != nil {
		wg.Add(1)
		go run("pprof", runCtx, pprofSrv)
		fields = append(fields, "pprof_addr", pprofSrv.Addr())
	}
	// Alerting reads storage, so it stops with the servers — before the store is
	// closed below.
	if alertSvc != nil {
		wg.Add(1)
		go run("alerting", runCtx, alertSvc)
	}

	// The telemetry listener is joined separately, and never on wg: it outlives
	// runCtx by design, so waiting for it alongside the API would deadlock the
	// drain on a listener that is only stopped once the drain is finished.
	var telWG sync.WaitGroup
	var telErr error
	if telSrv != nil {
		telWG.Add(1)
		go func() {
			defer telWG.Done()
			telErr = telSrv.Run(telCtx)
		}()
		fields = append(fields, "telemetry_addr", telSrv.Addr())
	}

	// Everything is bound and serving. Only now is the process ready: /startupz
	// flips, /readyz starts answering 200, and the log line the e2e suite waits on
	// is printed.
	healthz.MarkStarted()
	healthz.SetReady(true)
	logger.Info("server started", fields...)

	awaitShutdown(sigCtx, runCtx, healthz, stopRun, cfg, logger)

	// A hard deadline for the whole drain. Kubernetes sends SIGKILL
	// terminationGracePeriodSeconds after SIGTERM and does not care what we were
	// in the middle of; exiting on our own terms a moment earlier at least leaves
	// a log line saying which step hung.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		wg.Wait()
		close(errs)

		// Every transport is fully stopped, so no handler is still running and no
		// Ingest call can race the drain below.
		pipe.Shutdown()

		// All ingested metrics are now flushed into storage; close it (flush WAL,
		// release the file lock, close the DB).
		if err := store.Close(); err != nil {
			logger.Error("storage close failed", "error", err)
		}
	}()

	select {
	case <-drained:
	case <-time.After(cfg.shutdownTimeout):
		logger.Error("shutdown timed out; exiting without a clean drain",
			"timeout", cfg.shutdownTimeout)
		os.Exit(1)
	}

	// The probes served throughout the drain. They stop last.
	stopTel()
	telWG.Wait()

	var failed bool
	if telErr != nil {
		logger.Error("server terminated", "error", fmt.Errorf("telemetry: %w", telErr))
		failed = true
	}
	for err := range errs {
		logger.Error("server terminated", "error", err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	logger.Info("server stopped")
}

// awaitShutdown blocks until the process should stop serving, then performs the
// two steps that must happen before a single listener is closed.
//
// The order is the whole point, and it is the one thing about a rollout that
// everybody gets wrong exactly once.
//
// http.Server.Shutdown closes the listeners immediately and then waits for the
// requests already in flight. What it cannot do is tell the load balancer. In
// Kubernetes, SIGTERM and the endpoint removal are dispatched concurrently: the
// kubelet signals the container while the endpoints controller propagates the
// deletion out to every kube-proxy and every ingress. Those take a moment. A
// server that closes its listener on the first instant of SIGTERM spends that
// moment refusing connections that are still being routed to it — which the
// client sees as a 502, once per pod, on every deploy.
//
// So: fail readiness first, so the endpoint is withdrawn; wait out the
// propagation; only then let the listeners close. The delay is not a guess at how
// long requests take — Shutdown already waits for those — it is a guess at how
// long the rest of the cluster takes to notice, and 5s is the customary answer.
//
// runCtx is watched too: a transport that dies on its own has already called
// stopRun, and this function must not sit waiting for a signal that will never
// come.
func awaitShutdown(
	sigCtx, runCtx context.Context,
	healthz *health.Checker,
	stopRun context.CancelFunc,
	cfg config,
	logger *slog.Logger,
) {
	select {
	case <-runCtx.Done():
		// A transport failed. There is nothing to drain gracefully towards; the
		// remaining servers are already being cancelled.
		logger.Warn("a transport stopped on its own; shutting down")
		return
	case <-sigCtx.Done():
	}

	logger.Info("shutdown requested: failing readiness before draining",
		"drain_delay", cfg.shutdownDelay)
	healthz.SetReady(false)

	if cfg.shutdownDelay > 0 {
		timer := time.NewTimer(cfg.shutdownDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-runCtx.Done():
			// A transport gave up mid-drain. Stop waiting.
		}
	}

	logger.Info("draining in-flight requests")
	stopRun()
}

// toAlertEvent adapts an alerting domain alert into the dashboard's wire shape.
// The live package stays independent of the alerting packages this way.
func toAlertEvent(a *alert.Alert) live.AlertEvent {
	return live.AlertEvent{
		Fingerprint: a.Fingerprint,
		Rule:        a.RuleName,
		Status:      string(a.Status),
		Severity:    a.Severity,
		Value:       a.Value,
		StartsAt:    a.StartsAt,
		Labels:      a.Labels,
		Annotations: a.Annotations,
	}
}

// publishStatsLoop pushes a stats snapshot to the live dashboard every 2s.
func publishStatsLoop(ctx context.Context, hub *live.Hub, pipe *pipeline.Pipeline, store storage.Storage) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hub.PublishStats(map[string]any{
				"pipeline": pipe.Stats(),
				"storage":  store.Stats(),
			})
		}
	}
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

// buildAuthenticator assembles the auth Chain from config. It returns a nil
// authenticator when auth is disabled (fully backward compatible), plus any
// JWKS key sets whose background refresh the caller should start.
func buildAuthenticator(cfg config, logger *slog.Logger) (auth.Authenticator, []*auth.KeySet, error) {
	if !cfg.authEnabled {
		return nil, nil, nil
	}

	var opts []auth.VerifierOption
	if cfg.jwtIssuer != "" {
		opts = append(opts, auth.WithIssuer(cfg.jwtIssuer))
	}
	if cfg.jwtAudience != "" {
		opts = append(opts, auth.WithAudience(cfg.jwtAudience))
	}

	var chain auth.Chain
	var keySets []*auth.KeySet

	if cfg.apiKeysFile != "" {
		akCfg, err := auth.LoadAPIKeyConfig(cfg.apiKeysFile)
		if err != nil {
			return nil, nil, fmt.Errorf("load api keys: %w", err)
		}
		ak, err := auth.NewAPIKeyAuthenticator(akCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("api keys: %w", err)
		}
		chain = append(chain, ak)
		logger.Info("auth: api keys enabled", "count", len(akCfg.Keys))
	}
	if cfg.jwtHS256 != "" {
		v, err := auth.NewHS256Verifier([]byte(cfg.jwtHS256), opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("hs256: %w", err)
		}
		chain = append(chain, auth.NewJWTAuthenticator(v))
		logger.Info("auth: HS256 JWT enabled")
	}
	if cfg.jwksURL != "" {
		ks := auth.NewKeySet(cfg.jwksURL, 5*time.Minute)
		v, err := auth.NewRS256Verifier(ks, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("rs256: %w", err)
		}
		chain = append(chain, auth.NewJWTAuthenticator(v))
		keySets = append(keySets, ks)
		logger.Info("auth: RS256 JWT (JWKS) enabled", "url", cfg.jwksURL)
	}

	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("auth enabled but no method configured (set -api-keys, -jwt-hs256-secret or -jwks-url)")
	}
	return chain, keySets, nil
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

	authEnabled bool
	apiKeysFile string
	jwtHS256    string
	jwksURL     string
	jwtIssuer   string
	jwtAudience string

	uiEnabled bool

	pprofAddr            string
	mutexProfileFraction int
	blockProfileRate     int

	telemetryAddr    string
	shutdownDelay    time.Duration
	shutdownTimeout  time.Duration
	memoryLimitRatio float64
	showVersion      bool
	healthCheck      bool

	alertingEnabled bool
	alertRulesFile  string
	alertConfigFile string
	alertLookback   time.Duration
	alertBuffer     int
}

func loadConfig() config {
	cfg := config{
		addr:         envString("SERVER_ADDR", ":8080"),
		grpcAddr:     envString("GRPC_ADDR", ":9090"),
		logLevel:     envString("SERVER_LOG_LEVEL", "info"),
		storageType:  envString("STORAGE", "memory"),
		dataDir:      envString("DATA_DIR", "./data"),
		ingestBuffer: envInt("INGEST_BUFFER", 1000),
		// GOMAXPROCS, not runtime.NumCPU().
		//
		// NumCPU reports the logical CPUs the process may run on: the machine's
		// cores, narrowed by the CPU affinity mask. It knows nothing about the
		// cgroup CPU *quota*, which is what a container limit actually is. Run this
		// server on an eight-core host with `--cpus=1.5` and NumCPU still answers 8,
		// so it used to start eight validate workers and eight enrich workers to
		// share one and a half cores' worth of quota — sixteen goroutines contending
		// for two Ps, and every extra one only adds scheduling and cache pressure.
		//
		// GOMAXPROCS is the number the runtime derives from that quota, and since Go
		// 1.25 it does so automatically. It is the right size for a worker pool.
		validateWorkers: envInt("VALIDATE_WORKERS", container.GOMAXPROCS()),
		enrichWorkers:   envInt("ENRICH_WORKERS", container.GOMAXPROCS()),
		storeWorkers:    envInt("STORE_WORKERS", 1),
		rateLimitRPS:    envFloat("RATE_LIMIT_RPS", 100),
		rateLimitBurst:  envInt("RATE_LIMIT_BURST", 200),
		authEnabled:     envBool("AUTH", false),
		apiKeysFile:     envString("API_KEYS_FILE", ""),
		jwtHS256:        envString("JWT_HS256_SECRET", ""),
		jwksURL:         envString("JWKS_URL", ""),
		jwtIssuer:       envString("JWT_ISSUER", ""),
		jwtAudience:     envString("JWT_AUDIENCE", ""),
		uiEnabled:       envBool("UI", true),

		pprofAddr:            envString("PPROF_ADDR", ""),
		mutexProfileFraction: envInt("MUTEX_PROFILE_FRACTION", 0),
		blockProfileRate:     envInt("BLOCK_PROFILE_RATE", 0),

		// On by default, unlike every other subsystem in this project.
		//
		// The rule elsewhere — auth, alerting, packet capture, pprof — is that a new
		// subsystem ships off, because it costs something or exposes something. The
		// probes cost nothing and expose nothing, and a probe you have to remember
		// to enable is a probe that is missing on the day it matters. /metrics does
		// expose the process's internals, which is why it shares a listener that is
		// trivially firewalled rather than sitting on the API port. Set
		// -telemetry-addr="" to turn the whole listener off.
		telemetryAddr:    envString("TELEMETRY_ADDR", ":9091"),
		shutdownDelay:    envDuration("SHUTDOWN_DELAY", 0),
		shutdownTimeout:  envDuration("SHUTDOWN_TIMEOUT", 25*time.Second),
		memoryLimitRatio: envFloat("MEMORY_LIMIT_RATIO", 0.9),

		alertingEnabled: envBool("ALERTING", false),
		alertRulesFile:  envString("ALERT_RULES_FILE", ""),
		alertConfigFile: envString("ALERT_CONFIG_FILE", ""),
		alertLookback:   envDuration("ALERT_LOOKBACK", 5*time.Minute),
		alertBuffer:     envInt("ALERT_BUFFER", 1024),
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
	flag.BoolVar(&cfg.authEnabled, "auth", cfg.authEnabled, "enable authentication + RBAC + tenant isolation")
	flag.StringVar(&cfg.apiKeysFile, "api-keys", cfg.apiKeysFile, "path to API-keys JSON file")
	flag.StringVar(&cfg.jwtHS256, "jwt-hs256-secret", cfg.jwtHS256, "HS256 shared secret for JWT auth")
	flag.StringVar(&cfg.jwksURL, "jwks-url", cfg.jwksURL, "JWKS URL for RS256 JWT auth")
	flag.StringVar(&cfg.jwtIssuer, "jwt-issuer", cfg.jwtIssuer, "required JWT issuer (optional)")
	flag.StringVar(&cfg.jwtAudience, "jwt-audience", cfg.jwtAudience, "required JWT audience (optional)")
	flag.BoolVar(&cfg.uiEnabled, "ui", cfg.uiEnabled, "serve the embedded live dashboard at /")
	flag.StringVar(&cfg.telemetryAddr, "telemetry-addr", cfg.telemetryAddr, "listen address for /healthz, /readyz, /startupz and /metrics (empty to disable)")
	flag.DurationVar(&cfg.shutdownDelay, "shutdown-delay", cfg.shutdownDelay, "on SIGTERM, fail readiness and wait this long before closing listeners (set to ~5s behind a load balancer)")
	flag.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", cfg.shutdownTimeout, "hard deadline for the whole drain; must be under the orchestrator's grace period")
	flag.Float64Var(&cfg.memoryLimitRatio, "memory-limit-ratio", cfg.memoryLimitRatio, "fraction of the cgroup memory limit to hand to GOMEMLIMIT (ignored when GOMEMLIMIT is set)")
	flag.BoolVar(&cfg.showVersion, "version", false, "print the build's version, commit and platform, then exit")
	flag.BoolVar(&cfg.healthCheck, "health-check", false, "probe this container's own /readyz on -telemetry-addr and exit 0 or 1; for HEALTHCHECK in an image with no shell")
	flag.StringVar(&cfg.pprofAddr, "pprof-addr", cfg.pprofAddr, "listen address for net/http/pprof (empty to disable; use a loopback address)")
	flag.IntVar(&cfg.mutexProfileFraction, "mutex-profile-fraction", cfg.mutexProfileFraction, "record 1 in N mutex contention events (0 to disable)")
	flag.IntVar(&cfg.blockProfileRate, "block-profile-rate", cfg.blockProfileRate, "sample one blocking event per N nanoseconds blocked (0 to disable)")
	flag.BoolVar(&cfg.alertingEnabled, "alerting", cfg.alertingEnabled, "enable rule evaluation and notifications")
	flag.StringVar(&cfg.alertRulesFile, "alert-rules", cfg.alertRulesFile, "path to a bootstrap alerting rules JSON file")
	flag.StringVar(&cfg.alertConfigFile, "alert-config", cfg.alertConfigFile, "path to the alerting receivers/routing JSON file")
	flag.DurationVar(&cfg.alertLookback, "alert-lookback", cfg.alertLookback, "how far back a rule's instant selector may look")
	flag.IntVar(&cfg.alertBuffer, "alert-buffer", cfg.alertBuffer, "evaluator-to-notifier channel buffer size")
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

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
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

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
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
