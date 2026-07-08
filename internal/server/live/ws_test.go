package live

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serverConn wraps one end of a net.Pipe as a server-side WebSocket Conn.
func serverConn(nc net.Conn) *Conn {
	return &Conn{conn: nc, br: bufio.NewReader(nc), bw: bufio.NewWriter(nc)}
}

// readServerFrame decodes one (unmasked) server->client frame from raw bytes.
func readServerFrame(t *testing.T, r *bufio.Reader) (opcode byte, payload []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	opcode = h[0] & 0x0F
	n := int(h[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		_, _ = io.ReadFull(r, e[:])
		n = int(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		_, _ = io.ReadFull(r, e[:])
		n = int(binary.BigEndian.Uint64(e[:]))
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return opcode, payload
}

// writeClientFrame writes a masked client->server text frame.
func writeClientFrame(w io.Writer, opcode byte, payload []byte) {
	mask := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	hdr := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	_, _ = w.Write(hdr)
	_, _ = w.Write(mask[:])
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, _ = w.Write(masked)
}

func TestAcceptKey(t *testing.T) {
	t.Parallel()
	// RFC 6455 §1.3 worked example.
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("acceptKey = %q, want RFC example", got)
	}
}

func TestServerWriteFrameEncoding(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	sc := serverConn(c2)
	go func() { _ = sc.WriteText([]byte("hi")) }()

	br := bufio.NewReader(c1)
	op, payload := readServerFrame(t, br)
	if op != opText || string(payload) != "hi" {
		t.Fatalf("got op=%x payload=%q", op, payload)
	}
}

func TestServerReadFrameUnmasksClient(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	sc := serverConn(c2)
	go writeClientFrame(c1, opText, []byte("hello"))

	fin, op, payload, err := sc.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !fin || op != opText || string(payload) != "hello" {
		t.Fatalf("got fin=%v op=%x payload=%q", fin, op, payload)
	}
}

func TestServerRejectsUnmaskedClientFrame(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	sc := serverConn(c2)
	// Unmasked client frame is a protocol violation.
	go func() { _, _ = c1.Write([]byte{0x81, 0x02, 'h', 'i'}) }()
	if _, _, _, err := sc.readFrame(); err == nil {
		t.Fatal("expected protocol error for unmasked client frame")
	}
}

func TestReadLoopAnswersPing(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	sc := serverConn(c2)
	go func() { _ = sc.ReadLoop() }()

	// Client sends a ping; expect a pong back.
	go writeClientFrame(c1, opPing, []byte("p"))
	br := bufio.NewReader(c1)
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	op, payload := readServerFrame(t, br)
	if op != opPong || string(payload) != "p" {
		t.Fatalf("got op=%x payload=%q, want pong", op, payload)
	}
}

func TestUpgradeHandshake(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		_ = conn.WriteText([]byte("welcome"))
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close()
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	req := "GET /ws HTTP/1.1\r\n" +
		"Host: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("handshake status = %q err=%v", status, err)
	}
	// consume the rest of the headers
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if strings.HasPrefix(line, "Sec-WebSocket-Accept:") {
			if !strings.Contains(line, "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=") {
				t.Fatalf("bad accept header: %q", line)
			}
		}
	}
	op, payload := readServerFrame(t, br)
	if op != opText || string(payload) != "welcome" {
		t.Fatalf("expected pushed 'welcome' text frame, got op=%x %q", op, payload)
	}
}
