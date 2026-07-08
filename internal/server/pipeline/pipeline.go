// Package pipeline turns incoming batches into stored metrics through a chain
// of channel-connected stages: ingest -> unpack -> validate -> enrich -> store.
// Stages fan out to configurable worker pools and shut down cleanly by cascading
// channel closes, so no in-flight metric is lost on drain.
package pipeline

import (
	"log/slog"
	"runtime"
	"sync"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

// Config controls buffer sizing and per-stage parallelism.
type Config struct {
	IngestBuffer    int // ingestCh capacity — the backpressure knob
	ValidateWorkers int
	EnrichWorkers   int
	StoreWorkers    int // usually 1: store is serialized behind the storage lock
}

func (c Config) withDefaults() Config {
	if c.IngestBuffer <= 0 {
		c.IngestBuffer = 1000
	}
	if c.ValidateWorkers <= 0 {
		c.ValidateWorkers = runtime.NumCPU()
	}
	if c.EnrichWorkers <= 0 {
		c.EnrichWorkers = runtime.NumCPU()
	}
	if c.StoreWorkers <= 0 {
		c.StoreWorkers = 1
	}
	return c
}

// Pipeline encapsulates all processing stages. Ingest is the only entry point
// for the HTTP layer.
type Pipeline struct {
	ingestCh   chan model.Batch
	validateCh chan model.Metric
	enrichCh   chan model.Metric
	storeCh    chan model.Metric

	storage storage.Storage
	logger  *slog.Logger
	stats   *Stats
	cfg     Config

	// onStored, if set, is called with each batch of just-stored metrics (used
	// by the live dashboard). Set before Start. The callback must copy the slice
	// if it retains it — the pipeline reuses the backing array.
	onStored func([]model.Metric)

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// SetObserver registers a callback invoked with each batch of stored metrics.
// It must be called before Start.
func (p *Pipeline) SetObserver(f func([]model.Metric)) { p.onStored = f }

// New builds a pipeline. cfg zero-values are replaced with sensible defaults.
func New(store storage.Storage, cfg Config, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()
	return &Pipeline{
		ingestCh:   make(chan model.Batch, cfg.IngestBuffer),
		validateCh: make(chan model.Metric, cfg.IngestBuffer*10),
		enrichCh:   make(chan model.Metric, cfg.IngestBuffer*10),
		storeCh:    make(chan model.Metric, cfg.IngestBuffer*10),
		storage:    store,
		logger:     logger,
		stats:      NewStats(),
		cfg:        cfg,
	}
}

// Ingest is a non-blocking attempt to enqueue a batch. It returns false when
// the ingest buffer is full (backpressure -> the handler answers HTTP 503).
//
// Ingest must not be called after Shutdown; the caller (HTTP server) is
// responsible for stopping request handling before draining the pipeline.
func (p *Pipeline) Ingest(batch model.Batch) bool {
	select {
	case p.ingestCh <- batch:
		p.stats.IncIngested(int64(len(batch.Metrics)))
		return true
	default:
		p.stats.IncDropped(int64(len(batch.Metrics)))
		return false
	}
}

// Stats returns a snapshot of the pipeline counters.
func (p *Pipeline) Stats() Snapshot { return p.stats.Snapshot() }

// Start launches every stage goroutine and returns immediately. Safe to call
// more than once; only the first call has effect.
func (p *Pipeline) Start() { p.startOnce.Do(p.start) }

func (p *Pipeline) start() {
	// Stage 1: unpack batches into individual metrics. One goroutine owns
	// validateCh and closes it once ingestCh is drained.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.validateCh)
		p.unpackStage()
	}()

	// Stage 2: validate workers. A dedicated closer goroutine closes enrichCh
	// only after ALL validators have finished (double-close would panic).
	var validateWg sync.WaitGroup
	for i := 0; i < p.cfg.ValidateWorkers; i++ {
		validateWg.Add(1)
		p.wg.Add(1)
		go func(id int) {
			defer p.wg.Done()
			defer validateWg.Done()
			p.validateStage(id)
		}(i)
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		validateWg.Wait()
		close(p.enrichCh)
	}()

	// Stage 3: enrich workers, with the same closer pattern for storeCh.
	var enrichWg sync.WaitGroup
	for i := 0; i < p.cfg.EnrichWorkers; i++ {
		enrichWg.Add(1)
		p.wg.Add(1)
		go func(id int) {
			defer p.wg.Done()
			defer enrichWg.Done()
			p.enrichStage(id)
		}(i)
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		enrichWg.Wait()
		close(p.storeCh)
	}()

	// Stage 4: store workers drain storeCh into the storage backend.
	for i := 0; i < p.cfg.StoreWorkers; i++ {
		p.wg.Add(1)
		go func(id int) {
			defer p.wg.Done()
			p.storeStage(id)
		}(i)
	}

	p.logger.Info("pipeline started",
		"ingest_buffer", p.cfg.IngestBuffer,
		"validate_workers", p.cfg.ValidateWorkers,
		"enrich_workers", p.cfg.EnrichWorkers,
		"store_workers", p.cfg.StoreWorkers,
	)
}

// Shutdown stops accepting new batches and blocks until every metric already
// ingested has flowed through all stages and been stored. Closing ingestCh
// cascades down the chain (validateCh -> enrichCh -> storeCh), guaranteeing no
// data loss. Safe to call more than once.
//
// The caller MUST ensure no Ingest call races with or follows Shutdown.
func (p *Pipeline) Shutdown() {
	p.stopOnce.Do(func() {
		p.logger.Info("pipeline draining")
		close(p.ingestCh)
		p.wg.Wait()
		p.logger.Info("pipeline stopped", "stats", p.stats.Snapshot())
	})
}
