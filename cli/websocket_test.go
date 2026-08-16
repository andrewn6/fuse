package main

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

// wsTestClient is the browser half of the handshake, done by hand.
type wsTestClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialWS(t *testing.T, srv *httptest.Server, path string) *wsTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// the RFC 6455 sample key, so the accept value is a known vector
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: viewer\r\n" +
		"Connection: keep-alive, Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept = %q, want the RFC 6455 sample value", got)
	}
	return &wsTestClient{conn: conn, br: br}
}

// send writes one masked client frame.
func (c *wsTestClient) send(t *testing.T, opcode byte, payload []byte) {
	t.Helper()
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		frame = append(frame, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i&3])
	}
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("send frame: %v", err)
	}
}

// recv reads one server frame (servers never mask).
func (c *wsTestClient) recv(t *testing.T) (opcode byte, payload []byte) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		t.Fatalf("read frame head: %v", err)
	}
	if head[1]&0x80 != 0 {
		t.Fatal("server frame is masked; servers must not mask")
	}
	length := int(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			t.Fatalf("read extended length: %v", err)
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			t.Fatalf("read extended length: %v", err)
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return head[0] & 0x0f, payload
}

// echoWS accepts one websocket and echoes its data bytes back.
func echoWS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsUpgrade(w, r)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		buf := make([]byte, 1024)
		for {
			n, err := ws.Read(buf)
			if err != nil {
				return
			}
			if _, err := ws.Write(buf[:n]); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebsocketEchoRoundTrip(t *testing.T) {
	c := dialWS(t, echoWS(t), "/")

	c.send(t, wsOpBinary, []byte("rfb bytes"))
	op, payload := c.recv(t)
	if op != wsOpBinary || string(payload) != "rfb bytes" {
		t.Fatalf("got op %d payload %q, want binary %q", op, payload, "rfb bytes")
	}

	// a payload above the write chunk comes back split; reassemble it
	big := make([]byte, wsWriteChunk+100)
	for i := range big {
		big[i] = byte(i)
	}
	c.send(t, wsOpBinary, big)
	var back []byte
	for len(back) < len(big) {
		_, p := c.recv(t)
		back = append(back, p...)
	}
	if string(back) != string(big) {
		t.Fatal("large payload did not survive the round trip")
	}
}

func TestWebsocketPingAnswered(t *testing.T) {
	c := dialWS(t, echoWS(t), "/")
	c.send(t, wsOpPing, []byte("hb"))
	op, payload := c.recv(t)
	if op != wsOpPong || string(payload) != "hb" {
		t.Fatalf("got op %d payload %q, want pong %q", op, payload, "hb")
	}
}

func TestWebsocketCloseEndsStream(t *testing.T) {
	ended := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsUpgrade(w, r)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		_, err = ws.Read(make([]byte, 16))
		ended <- err
	}))
	defer srv.Close()

	c := dialWS(t, srv, "/")
	c.send(t, wsOpClose, []byte{0x03, 0xe8})

	if err := <-ended; err != io.EOF {
		t.Fatalf("read after close = %v, want io.EOF", err)
	}
	// the server echoes the close frame before tearing down
	op, _ := c.recv(t)
	if op != wsOpClose {
		t.Fatalf("got op %d, want close", op)
	}
}

func TestWebsocketRejectsUnmaskedClientFrame(t *testing.T) {
	ended := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsUpgrade(w, r)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		_, err = ws.Read(make([]byte, 16))
		ended <- err
	}))
	defer srv.Close()

	c := dialWS(t, srv, "/")
	// unmasked binary frame: mask bit clear
	if _, err := c.conn.Write([]byte{0x80 | wsOpBinary, 0x03, 'a', 'b', 'c'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := <-ended
	if err == nil || !strings.Contains(err.Error(), "unmasked") {
		t.Fatalf("read = %v, want an unmasked-frame error", err)
	}
}

func TestWebsocketUpgradeRequiresHandshakeHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = wsUpgrade(w, r)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a plain GET", resp.StatusCode)
	}
}
