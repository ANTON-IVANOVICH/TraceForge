// Package chunk writes and reads immutable on-disk chunks. A chunk is a snapshot
// of the in-memory head: a binary `data` file of points plus a small JSON index
// mapping each series to its byte range. Chunks are written atomically (tmp dir
// + fsync + rename) and read back through mmap, so a range query is a binary
// search directly over mapped memory with no explicit I/O.
package chunk

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

const (
	magic      = "TSDB"
	version    = 1
	headerSize = 4 + 1 // magic(4) + version(1)
	pointSize  = 16    // timestamp(8) + value(8)
)

type seriesEntry struct {
	Name   string            `json:"name"`
	Type   model.MetricType  `json:"type"`
	Labels map[string]string `json:"labels,omitempty"`
	Offset int64             `json:"offset"`
	Length int64             `json:"length"`
}

type chunkIndex struct {
	Series  map[string]seriesEntry `json:"series"`
	MinTime int64                  `json:"min_time"`
	MaxTime int64                  `json:"max_time"`
	Points  int64                  `json:"points"`
}

// Write serializes series into an immutable chunk at dir. It writes into
// dir+".tmp", fsyncs, then renames — so a reader ever sees either no chunk or a
// complete one, never a half-written one.
func Write(dir string, series []storage.Series) error {
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0755); err != nil {
		return err
	}

	dataFile, err := os.Create(filepath.Join(tmp, "data"))
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(dataFile)
	if _, err := bw.WriteString(magic); err != nil {
		_ = dataFile.Close()
		return err
	}
	if err := bw.WriteByte(version); err != nil {
		_ = dataFile.Close()
		return err
	}

	// Deterministic order: sort series by canonical key.
	byKey := make(map[string]storage.Series, len(series))
	keys := make([]string, 0, len(series))
	for _, s := range series {
		k := storage.SeriesKey(s.Name, s.Labels)
		byKey[k] = s
		keys = append(keys, k)
	}
	sort.Strings(keys)

	idx := chunkIndex{Series: make(map[string]seriesEntry, len(keys))}
	haveTime := false
	offset := int64(headerSize)
	var pointBuf [pointSize]byte

	for _, key := range keys {
		s := byKey[key]
		pts := append([]storage.Point(nil), s.Points...)
		sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp.Before(pts[j].Timestamp) })

		var countBuf [4]byte
		binary.BigEndian.PutUint32(countBuf[:], uint32(len(pts)))
		if _, err := bw.Write(countBuf[:]); err != nil {
			_ = dataFile.Close()
			return err
		}
		for _, pt := range pts {
			ns := pt.Timestamp.UnixNano()
			binary.BigEndian.PutUint64(pointBuf[0:8], uint64(ns))
			binary.BigEndian.PutUint64(pointBuf[8:16], math.Float64bits(pt.Value))
			if _, err := bw.Write(pointBuf[:]); err != nil {
				_ = dataFile.Close()
				return err
			}
			if !haveTime {
				idx.MinTime, idx.MaxTime, haveTime = ns, ns, true
			} else {
				if ns < idx.MinTime {
					idx.MinTime = ns
				}
				if ns > idx.MaxTime {
					idx.MaxTime = ns
				}
			}
		}
		blockLen := int64(4 + pointSize*len(pts))
		idx.Series[key] = seriesEntry{
			Name:   s.Name,
			Type:   s.Type,
			Labels: storage.CloneLabels(s.Labels),
			Offset: offset,
			Length: blockLen,
		}
		idx.Points += int64(len(pts))
		offset += blockLen
	}

	if err := bw.Flush(); err != nil {
		_ = dataFile.Close()
		return err
	}
	if err := dataFile.Sync(); err != nil {
		_ = dataFile.Close()
		return err
	}
	if err := dataFile.Close(); err != nil {
		return err
	}

	indexBytes, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(tmp, "index.json"), indexBytes); err != nil {
		return err
	}

	return os.Rename(tmp, dir) // atomic publish
}

func writeFileSync(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Reader reads an immutable chunk. Its data file is memory-mapped (or read into
// memory on platforms without mmap).
type Reader struct {
	dir     string
	data    []byte
	index   chunkIndex
	closeFn func() error
}

// Open loads a chunk's index and maps its data file.
func Open(dir string) (*Reader, error) {
	indexBytes, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var idx chunkIndex
	if err := json.Unmarshal(indexBytes, &idx); err != nil {
		return nil, fmt.Errorf("parse chunk index %s: %w", dir, err)
	}

	data, closeFn, err := mapData(filepath.Join(dir, "data"))
	if err != nil {
		return nil, err
	}
	if err := ParseHeader(data); err != nil {
		_ = closeFn()
		return nil, fmt.Errorf("chunk %s: %w", dir, err)
	}
	return &Reader{dir: dir, data: data, index: idx, closeFn: closeFn}, nil
}

// SeriesMeta describes one series in a chunk.
type SeriesMeta struct {
	Key    string
	Name   string
	Type   model.MetricType
	Labels map[string]string
}

// ForEachSeries calls fn for each series stored in the chunk.
func (r *Reader) ForEachSeries(fn func(SeriesMeta)) {
	for key, e := range r.index.Series {
		fn(SeriesMeta{Key: key, Name: e.Name, Type: e.Type, Labels: e.Labels})
	}
}

// MinTime and MaxTime report the chunk's time span (used to skip chunks that
// don't overlap a query).
func (r *Reader) MinTime() time.Time { return time.Unix(0, r.index.MinTime).UTC() }
func (r *Reader) MaxTime() time.Time { return time.Unix(0, r.index.MaxTime).UTC() }

// Points returns the total number of points stored in the chunk.
func (r *Reader) Points() int64 { return r.index.Points }

// ReadSeries returns the points of one series within [from, to] (open bounds if
// zero), read straight from mapped memory via binary search. It is defensive
// against a corrupt index so it never reads out of bounds.
func (r *Reader) ReadSeries(key string, from, to time.Time) ([]storage.Point, error) {
	e, ok := r.index.Series[key]
	if !ok {
		return nil, nil
	}
	if e.Offset < headerSize || e.Length < 4 || e.Offset+e.Length > int64(len(r.data)) {
		return nil, fmt.Errorf("chunk %s: series %q out of bounds", r.dir, key)
	}
	block := r.data[e.Offset : e.Offset+e.Length]
	count := int(binary.BigEndian.Uint32(block[0:4]))
	pts := block[4:]
	if count < 0 || count*pointSize > len(pts) {
		return nil, fmt.Errorf("chunk %s: truncated series %q", r.dir, key)
	}

	hasFrom, hasTo := !from.IsZero(), !to.IsZero()
	fromNs, toNs := from.UnixNano(), to.UnixNano()

	start := 0
	if hasFrom {
		start = sort.Search(count, func(i int) bool {
			ts := int64(binary.BigEndian.Uint64(pts[i*pointSize : i*pointSize+8]))
			return ts >= fromNs
		})
	}

	var result []storage.Point
	for i := start; i < count; i++ {
		ts := int64(binary.BigEndian.Uint64(pts[i*pointSize : i*pointSize+8]))
		if hasTo && ts > toNs {
			break
		}
		val := math.Float64frombits(binary.BigEndian.Uint64(pts[i*pointSize+8 : i*pointSize+16]))
		result = append(result, storage.Point{Timestamp: time.Unix(0, ts).UTC(), Value: val})
	}
	return result, nil
}

// Close unmaps/closes the chunk's data.
func (r *Reader) Close() error {
	if r.closeFn != nil {
		return r.closeFn()
	}
	return nil
}

// ParseHeader validates a chunk data blob's magic + version. It must never panic
// on arbitrary input — it is a fuzz target.
func ParseHeader(data []byte) error {
	if len(data) < headerSize {
		return fmt.Errorf("short chunk header")
	}
	if string(data[0:4]) != magic {
		return fmt.Errorf("bad chunk magic %q", data[0:4])
	}
	if data[4] != version {
		return fmt.Errorf("unsupported chunk version %d", data[4])
	}
	return nil
}
