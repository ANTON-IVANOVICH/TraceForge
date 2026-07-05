// Package bolt implements storage.Storage on top of bbolt (etcd's B+tree
// key-value store). Timestamps are big-endian encoded so bbolt's lexicographic
// key order equals chronological order, turning a time-range query into a cheap
// cursor range scan.
package bolt

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"go.etcd.io/bbolt"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

var (
	bucketMeta   = []byte("meta")   // seriesKey -> JSON series metadata
	bucketPoints = []byte("points") // seriesKey -> nested bucket of ts -> value
	errStop      = fmt.Errorf("stop iteration")
)

// Storage is a bbolt-backed persistent store.
type Storage struct {
	db *bbolt.DB
}

type seriesMeta struct {
	Name   string            `json:"name"`
	Type   model.MetricType  `json:"type"`
	Labels map[string]string `json:"labels,omitempty"`
}

// New opens (or creates) a bbolt database at path and ensures the root buckets
// exist. The open Timeout means we fail fast instead of hanging if another
// process holds the file lock.
func New(path string) (*Storage, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{
		Timeout:      time.Second,
		FreelistType: bbolt.FreelistMapType,
	})
	if err != nil {
		return nil, fmt.Errorf("open bolt: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketMeta, bucketPoints} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}
	return &Storage{db: db}, nil
}

// Write persists a single metric (one transaction).
func (s *Storage) Write(m model.Metric) error {
	return s.WriteBatch([]model.Metric{m})
}

// WriteBatch persists many metrics in a single read-write transaction. bbolt
// serializes writers, so batching is the way to get throughput.
func (s *Storage) WriteBatch(metrics []model.Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		metaB := tx.Bucket(bucketMeta)
		pointsB := tx.Bucket(bucketPoints)
		for _, m := range metrics {
			key := []byte(storage.SeriesKey(m.Name, m.Labels))

			// Register the series metadata once.
			if metaB.Get(key) == nil {
				meta, err := json.Marshal(seriesMeta{Name: m.Name, Type: m.Type, Labels: m.Labels})
				if err != nil {
					return err
				}
				if err := metaB.Put(key, meta); err != nil {
					return err
				}
			}

			// Append the point into the series' nested bucket.
			sp, err := pointsB.CreateBucketIfNotExists(key)
			if err != nil {
				return err
			}
			if err := sp.Put(encodeTime(m.Timestamp), encodeFloat(m.Value)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Query scans matching series and their points in [From, To], then applies the
// shared raw/aggregate logic.
func (s *Storage) Query(q storage.Query) ([]model.Metric, error) {
	if q.Name == "" {
		return nil, fmt.Errorf("query name is required")
	}

	fromKey := encodeTime(q.From)
	toKey := encodeTime(q.To)
	hasFrom := !q.From.IsZero()
	hasTo := !q.To.IsZero()

	var result []model.Metric
	err := s.db.View(func(tx *bbolt.Tx) error {
		metaB := tx.Bucket(bucketMeta)
		pointsB := tx.Bucket(bucketPoints)

		return metaB.ForEach(func(key, metaBytes []byte) error {
			var meta seriesMeta
			if err := json.Unmarshal(metaBytes, &meta); err != nil {
				return err
			}
			if meta.Name != q.Name || !storage.MatchLabels(meta.Labels, q.Labels) {
				return nil
			}
			sp := pointsB.Bucket(key)
			if sp == nil {
				return nil
			}

			// Range scan: Seek to the first key >= from, walk until we pass to.
			var points []storage.Point
			c := sp.Cursor()
			var k, v []byte
			if hasFrom {
				k, v = c.Seek(fromKey)
			} else {
				k, v = c.First()
			}
			for ; k != nil; k, v = c.Next() {
				if hasTo && bytes.Compare(k, toKey) > 0 {
					break
				}
				points = append(points, storage.Point{Timestamp: decodeTime(k), Value: decodeFloat(v)})
			}

			for _, mm := range storage.ApplyQuery(q, meta.Name, meta.Type, meta.Labels, points) {
				result = append(result, mm)
				if q.Limit > 0 && len(result) >= q.Limit {
					return errStop
				}
			}
			return nil
		})
	})
	if err == errStop {
		err = nil
	}
	return result, err
}

// Stats counts series and total points.
func (s *Storage) Stats() storage.Stats {
	var st storage.Stats
	_ = s.db.View(func(tx *bbolt.Tx) error {
		st.Series = tx.Bucket(bucketMeta).Stats().KeyN
		pointsB := tx.Bucket(bucketPoints)
		return pointsB.ForEach(func(k, _ []byte) error {
			if sub := pointsB.Bucket(k); sub != nil {
				st.Points += int64(sub.Stats().KeyN)
			}
			return nil
		})
	})
	return st
}

// Close closes the database (releasing its file lock).
func (s *Storage) Close() error { return s.db.Close() }

// encodeTime encodes a timestamp as big-endian uint64 nanoseconds so that
// lexicographic byte order matches chronological order.
func encodeTime(t time.Time) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(t.UnixNano()))
	return b
}

func decodeTime(b []byte) time.Time {
	return time.Unix(0, int64(binary.BigEndian.Uint64(b))).UTC()
}

func encodeFloat(v float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
	return b
}

func decodeFloat(b []byte) float64 {
	return math.Float64frombits(binary.BigEndian.Uint64(b))
}
