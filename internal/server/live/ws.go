// Package live implements a minimal, dependency-free WebSocket server (RFC 6455)
// and a broadcast hub that pushes live metrics and stats to browser dashboards.
//
// The WebSocket layer is written from scratch on the standard library (net/http
// hijack, crypto/sha1, encoding/binary) — it implements just enough of RFC 6455
// to push text frames to browsers and keep the connection alive: the opening
// handshake, frame (de)coding with client-frame unmasking, fragmentation
// reassembly, and ping/pong/close control frames.
package live

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wsGUID is the RFC 6455 magic value mixed into the accept-key hash.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

const (
	maxPayload = 1 << 20 // 1 MiB frame cap
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second // no client frame within this => stale, drop
	maxMessage = 4 << 20          // cap reassembled message size
)

var (
	errNotWebSocket = errors.New("not a websocket upgrade")
	errClosed       = errors.New("websocket closed")
	errProtocol     = errors.New("websocket protocol error")
)

// Conn is a server-side WebSocket connection. Writes are serialized; a single
// reader (ReadLoop) is expected.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	wmu    sync.Mutex
	closed bool
}

// Upgrade completes the WebSocket handshake and hijacks the connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !tokenInHeader(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errNotWebSocket
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, fmt.Errorf("%w: bad version", errNotWebSocket)
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("%w: missing key", errNotWebSocket)
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijacking")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, br: brw.Reader, bw: brw.Writer}, nil
}

func acceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// tokenInHeader reports whether a comma-separated header value contains token
// (case-insensitive), e.g. Connection: keep-alive, Upgrade.
func tokenInHeader(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// WriteText sends a text frame (unmasked, as required for server->client).
func (c *Conn) WriteText(b []byte) error { return c.writeFrame(opText, b) }

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errClosed
	}

	var hdr [10]byte
	hdr[0] = 0x80 | opcode // FIN set, single-frame message
	n := len(payload)
	var hn int
	switch {
	case n <= 125:
		hdr[1] = byte(n)
		hn = 2
	case n <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		hn = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		hn = 10
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	if _, err := c.bw.Write(hdr[:hn]); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// Ping sends a ping control frame (used for keepalive).
func (c *Conn) Ping() error { return c.writeFrame(opPing, nil) }

// Close sends a close frame and tears down the connection (idempotent).
func (c *Conn) Close() error {
	c.wmu.Lock()
	if c.closed {
		c.wmu.Unlock()
		return nil
	}
	c.closed = true
	c.wmu.Unlock()
	// Best-effort close frame, then close the socket.
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	_, _ = c.bw.Write([]byte{0x88, 0x00}) // FIN|close, zero-length payload
	_ = c.bw.Flush()
	return c.conn.Close()
}

// ReadLoop consumes inbound frames until the peer closes or errors. It answers
// pings with pongs and discards application data (the dashboard is server-push
// only). It returns when the connection ends.
func (c *Conn) ReadLoop() error {
	var fragOpcode byte
	var fragLen int
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return err
		}
		switch opcode {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return err
			}
		case opPong:
			// keepalive ack; ignore
		case opClose:
			_ = c.Close()
			return errClosed
		case opText, opBinary:
			if !fin {
				fragOpcode = opcode
				fragLen = len(payload)
			}
			// application data is ignored
		case opContinuation:
			if fragOpcode == 0 {
				return errProtocol // continuation with no start frame
			}
			fragLen += len(payload)
			if fragLen > maxMessage {
				return errProtocol
			}
			if fin {
				fragOpcode = 0
				fragLen = 0
			}
		default:
			return errProtocol
		}
	}
}

// readFrame reads and unmasks one frame from a client.
func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	if err = c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return
	}
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 { // reserved bits must be zero
		err = errProtocol
		return
	}
	opcode = h[0] & 0x0F
	if h[1]&0x80 == 0 { // client->server frames MUST be masked
		err = errProtocol
		return
	}
	n := int(h[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u > maxPayload {
			err = errProtocol
			return
		}
		n = int(u)
	}
	if isControl(opcode) && (n > 125 || !fin) {
		err = errProtocol // control frames must be short and unfragmented
		return
	}
	if n > maxPayload {
		err = errProtocol
		return
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, opcode, payload, nil
}

func isControl(opcode byte) bool { return opcode&0x08 != 0 }
