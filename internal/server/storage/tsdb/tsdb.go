// Package tsdb is a small from-scratch time-series database implementing
// storage.Storage. It is a simplified LSM-tree: writes go to a write-ahead log
// (durability) and an in-memory head (visibility); when the head grows large it
// is flushed to an immutable, mmap-readable chunk on disk and the WAL is
// cleared. On startup, existing chunks are loaded and the WAL is replayed into a
// fresh head, so data survives restarts and crashes.
package tsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/server/storage/tsdb/chunk"
	"metrics-system/internal/server/storage/tsdb/wal"
)

const (
	flushMaxAge    = 2 * time.Hour
	flushMaxPoints = 1_000_000
	syncInterval   = 100 * time.Millisecond
	flushInterval  = 10 * time.Second
)

// TSDB is the on-disk time-series database.
type TSDB struct {
	dir    string
	logger *slog.Logger
	lock   *os.File

	mu        sync.RWMutex // guards head, chunks, nextChunk; serializes WAL append+head write
	wal       *wal.WAL
	head      *head
	chunks    []*chunk.Reader
	nextChunk int

	// durability is the last error from the background fsync, or nil. It exists
	// because of the worst state this database can reach: the WAL's Write returns
	// nil (the bytes are in the kernel's page cache), the head serves them back on
	// query, and the fsync that would have made them survive a power cut has been
	// failing for an hour on a full disk. Nothing in the write path notices. The
	// only place that learns of it is syncLoop, which used to log the error and
	// drop it.
	//
	// Ping reads this, so a replica whose disk stopped accepting fsyncs fails its
	// readiness probe and leaves the load balancer instead of quietly accepting
	// writes it cannot keep.
	durability atomic.Pointer[error]

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open opens (or creates) a TSDB at dir. It acquires a file lock, loads existing
// chunks, replays the WAL into the head, and starts the background sync/flush
// loops.
func Open(dir string, logger *slog.Logger) (*TSDB, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}

	chunks, nextID, err := loadChunks(filepath.Join(dir, "chunks"))
	if err != nil {
		_ = releaseLock(lock)
		return nil, err
	}

	walPath := filepath.Join(dir, "wal", "current.wal")
	if err := os.MkdirAll(filepath.Dir(walPath), 0755); err != nil {
		_ = releaseLock(lock)
		closeReaders(chunks)
		return nil, err
	}

	h := newHead()
	if err := wal.Replay(walPath, func(payload []byte) error {
		m, derr := decodeMetric(payload)
		if derr != nil {
			return derr
		}
		h.write(m)
		return nil
	}); err != nil {
		_ = releaseLock(lock)
		closeReaders(chunks)
		return nil, fmt.Errorf("wal replay: %w", err)
	}

	w, err := wal.Open(walPath)
	if err != nil {
		_ = releaseLock(lock)
		closeReaders(chunks)
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	db := &TSDB{
		dir: dir, logger: logger, lock: lock,
		wal: w, head: h, chunks: chunks, nextChunk: nextID,
		cancel: cancel,
	}
	db.wg.Add(2)
	go db.syncLoop(ctx)
	go db.flushLoop(ctx)

	logger.Info("tsdb opened", "dir", dir, "chunks", len(chunks), "head_points", h.points)
	return db, nil
}

// Write persists one metric: WAL first (durability), then head (visibility).
func (db *TSDB) Write(m model.Metric) error {
	return db.WriteBatch([]model.Metric{m})
}

// WriteBatch persists many metrics under one lock acquisition.
func (db *TSDB) WriteBatch(metrics []model.Metric) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, m := range metrics {
		payload, err := encodeMetric(m)
		if err != nil {
			return err
		}
		if err := db.wal.Write(payload); err != nil {
			return err
		}
		db.head.write(m)
	}
	return nil
}

// Query merges points from the on-disk chunks and the in-memory head for each
// matching series, de-duplicating by timestamp, then applies the shared
// raw/aggregate logic.
func (db *TSDB) Query(q storage.Query) ([]model.Metric, error) {
	if q.Name == "" {
		return nil, errors.New("query name is required")
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	type acc struct {
		name   string
		typ    model.MetricType
		labels map[string]string
		points map[int64]float64 // ts(ns) -> value, de-dups chunk/head overlap
	}
	series := make(map[string]*acc)
	add := func(key, name string, typ model.MetricType, labels map[string]string, pts []storage.Point) {
		a, ok := series[key]
		if !ok {
			a = &acc{name: name, typ: typ, labels: labels, points: make(map[int64]float64)}
			series[key] = a
		}
		for _, p := range pts {
			a.points[p.Timestamp.UnixNano()] = p.Value
		}
	}

	// On-disk chunks (skip those whose time span can't overlap the query).
	for _, c := range db.chunks {
		if !overlaps(c, q) {
			continue
		}
		var readErr error
		c.ForEachSeries(func(sm chunk.SeriesMeta) {
			if readErr != nil || sm.Name != q.Name || !storage.MatchLabels(sm.Labels, q.Labels) {
				return
			}
			pts, err := c.ReadSeries(sm.Key, q.From, q.To)
			if err != nil {
				readErr = err
				return
			}
			add(sm.Key, sm.Name, sm.Type, sm.Labels, pts)
		})
		if readErr != nil {
			return nil, readErr
		}
	}

	// In-memory head (freshest data).
	for key, s := range db.head.series {
		if s.Name != q.Name || !storage.MatchLabels(s.Labels, q.Labels) {
			continue
		}
		add(key, s.Name, s.Type, s.Labels, storage.FilterTime(s.Points, q.From, q.To))
	}

	// Deterministic series order.
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var result []model.Metric
	for _, k := range keys {
		a := series[k]
		pts := make([]storage.Point, 0, len(a.points))
		for ns, v := range a.points {
			pts = append(pts, storage.Point{Timestamp: time.Unix(0, ns).UTC(), Value: v})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp.Before(pts[j].Timestamp) })

		for _, mm := range storage.ApplyQuery(q, a.name, a.typ, a.labels, pts) {
			result = append(result, mm)
			if q.Limit > 0 && len(result) >= q.Limit {
				return result, nil
			}
		}
	}
	return result, nil
}

// Stats reports unique series and total points across chunks and head.
func (db *TSDB) Stats() storage.Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()

	seen := make(map[string]struct{})
	var points int64
	for _, c := range db.chunks {
		c.ForEachSeries(func(sm chunk.SeriesMeta) { seen[sm.Key] = struct{}{} })
		points += c.Points()
	}
	for k := range db.head.series {
		seen[k] = struct{}{}
	}
	points += db.head.points
	return storage.Stats{Series: len(seen), Points: points}
}

// Close stops the background loops, fsyncs and closes the WAL, unmaps chunks and
// releases the file lock. Head data remains in the WAL and is replayed on the
// next Open.
func (db *TSDB) Close() error {
	db.cancel()
	db.wg.Wait()

	db.mu.Lock()
	defer db.mu.Unlock()

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	keep(db.wal.Sync())
	keep(db.wal.Close())
	closeReaders(db.chunks)
	keep(releaseLock(db.lock))
	return firstErr
}

// flush snapshots the head into a new immutable chunk, then clears the WAL and
// resets the head. Holds the write lock so no write interleaves.
func (db *TSDB) flush() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.flushLocked()
}

func (db *TSDB) flushLocked() error {
	if db.head.isEmpty() {
		return nil
	}
	name := fmt.Sprintf("%06d", db.nextChunk)
	dir := filepath.Join(db.dir, "chunks", name)

	if err := chunk.Write(dir, db.head.snapshot()); err != nil {
		return err
	}
	r, err := chunk.Open(dir)
	if err != nil {
		return err
	}
	db.chunks = append(db.chunks, r)
	db.nextChunk++

	// Data is now durable in the chunk; clear the WAL and start a fresh head.
	if err := db.wal.Truncate(); err != nil {
		return err
	}
	db.head = newHead()
	db.logger.Info("tsdb flushed head to chunk", "chunk", name, "points", r.Points())
	return nil
}

func (db *TSDB) syncLoop(ctx context.Context) {
	defer db.wg.Done()
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			db.recordSync(db.wal.Sync())
		}
	}
}

// recordSync publishes the outcome of the last fsync so Ping can report it.
// A success clears a previous failure: a disk that filled up and was cleaned up
// should bring the replica back without a restart.
func (db *TSDB) recordSync(err error) {
	if err != nil {
		db.logger.Error("wal sync failed", "error", err)
		db.durability.Store(&err)
		return
	}
	db.durability.Store(nil)
}

// Ping reports whether writes are still reaching stable storage.
//
// It reads a value the background fsync publishes rather than fsyncing itself.
// Calling Sync here would mean the readiness probe issues an fsync every few
// seconds — turning a health check into write amplification — and would block
// behind the very disk it is trying to ask about, which is the one thing a probe
// must never do.
//
// Note what this does not check: that the head is consistent, or that a query
// would succeed. Those failures kill the process, and a dead process needs no
// probe. What can fail silently, indefinitely, and only matters at the next power
// cut is the fsync.
func (db *TSDB) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errp := db.durability.Load(); errp != nil {
		return fmt.Errorf("wal not syncing: %w", *errp)
	}
	return nil
}

func (db *TSDB) flushLoop(ctx context.Context) {
	defer db.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			db.mu.RLock()
			should := db.head.shouldFlush(flushMaxAge, flushMaxPoints)
			db.mu.RUnlock()
			if should {
				if err := db.flush(); err != nil {
					db.logger.Error("tsdb flush failed", "error", err)
				}
			}
		}
	}
}

// overlaps reports whether a chunk's time span can contain points in the query
// window (cheap pruning).
func overlaps(c *chunk.Reader, q storage.Query) bool {
	if !q.From.IsZero() && c.MaxTime().Before(q.From) {
		return false
	}
	if !q.To.IsZero() && c.MinTime().After(q.To) {
		return false
	}
	return true
}

// loadChunks opens every chunk directory in sorted order and returns the readers
// plus the next chunk id to use.
func loadChunks(chunksDir string) ([]*chunk.Reader, int, error) {
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 1, nil
		}
		return nil, 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var readers []*chunk.Reader
	nextID := 1
	for _, name := range names {
		r, err := chunk.Open(filepath.Join(chunksDir, name))
		if err != nil {
			closeReaders(readers)
			return nil, 0, fmt.Errorf("open chunk %s: %w", name, err)
		}
		readers = append(readers, r)
		if id, err := strconv.Atoi(name); err == nil && id >= nextID {
			nextID = id + 1
		}
	}
	return readers, nextID, nil
}

func closeReaders(readers []*chunk.Reader) {
	for _, r := range readers {
		_ = r.Close()
	}
}

// encodeMetric/decodeMetric are the WAL payload codec. JSON keeps it simple and
// reuses model.Metric's marshaling; the binary format lives in the chunks.
func encodeMetric(m model.Metric) ([]byte, error) { return json.Marshal(m) }

func decodeMetric(b []byte) (model.Metric, error) {
	var m model.Metric
	err := json.Unmarshal(b, &m)
	return m, err
}
