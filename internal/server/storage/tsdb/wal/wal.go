// Package wal implements a write-ahead log: an append-only file of length-
// prefixed, CRC-checked records. Every metric is written here (and fsynced)
// before it is considered durable, so a crash can be recovered by replaying the
// log on startup.
package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

const (
	recordTypeWrite = 1
	headerSize      = 4 + 4 + 1 // length(4) + crc(4) + type(1)

	// maxRecordSize bounds the payload length Replay will trust from a header
	// before allocating for it. A record is one JSON-encoded model.Metric — a
	// name, a float, a timestamp and a handful of labels — which runs to a few
	// hundred bytes; even a pathological label set is a few kilobytes. 16 MiB is
	// several orders of magnitude above that, so any header claiming more is not a
	// real record but a corrupt or torn one (a crash can leave 0xFFFFFFFF on the
	// wire, which would otherwise make startup allocate 4 GiB). Such a length is
	// treated exactly like a bad CRC: replay stops cleanly.
	maxRecordSize = 16 << 20
)

// WAL is a single append-only log segment.
type WAL struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	size   int64
}

// Open opens (or creates) the log at path for appending. O_APPEND makes every
// write land at end-of-file even across goroutines.
func Open(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open wal: %w", err)
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &WAL{
		file:   f,
		writer: bufio.NewWriterSize(f, 64*1024),
		size:   stat.Size(),
	}, nil
}

// Write appends one record to the buffer. It does not fsync — Sync does.
//
// A payload larger than maxRecordSize is rejected here rather than written,
// because Replay refuses to read one: writing a record the log cannot replay
// would be silent data loss on the next restart. Today no caller comes close —
// a record is one metric, and ingest caps the request body far below the limit —
// so this only holds the invariant for a future caller who batches or lifts that
// cap.
func (w *WAL) Write(payload []byte) error {
	if len(payload) > maxRecordSize {
		return fmt.Errorf("wal: record of %d bytes exceeds the %d-byte limit", len(payload), maxRecordSize)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var header [headerSize]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(payload))
	header[8] = recordTypeWrite

	if _, err := w.writer.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.writer.Write(payload); err != nil {
		return err
	}
	w.size += int64(headerSize + len(payload))
	return nil
}

// Sync flushes the buffer and fsyncs the file — the durability point.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Sync()
}

// Size returns the number of bytes written so far.
func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// Truncate clears the log after its records have been durably persisted into a
// chunk. The caller must guarantee no unflushed data remains in the log.
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writer.Flush(); err != nil {
		return err
	}
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	w.size = 0
	return nil
}

// Close flushes and closes the file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Close()
}

// Replay reads every intact record from the log at path and passes its payload
// to handler. A missing file, a torn header/payload (crash mid-write), a bad CRC
// or an implausibly large record length ends replay cleanly rather than
// erroring — the last record after a crash is often half-written, and that must
// be survivable.
func Replay(path string, handler func(payload []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReaderSize(f, 64*1024)
	var header [headerSize]byte
	for {
		_, err := io.ReadFull(reader, header[:])
		if err == io.EOF {
			return nil // clean end of log
		}
		if err == io.ErrUnexpectedEOF {
			return nil // torn header
		}
		if err != nil {
			return fmt.Errorf("read header: %w", err)
		}

		length := binary.BigEndian.Uint32(header[0:4])
		expectedCRC := binary.BigEndian.Uint32(header[4:8])
		// header[8] is the record type — only writes exist for now.

		if length > maxRecordSize {
			return nil // implausible length: corrupt or torn header, stop cleanly
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil // torn payload
		}
		if crc32.ChecksumIEEE(payload) != expectedCRC {
			return nil // corrupt record — stop cleanly
		}
		if err := handler(payload); err != nil {
			return err
		}
	}
}
