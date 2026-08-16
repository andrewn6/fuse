package main

// A minimal RFC 6455 websocket server, just enough to carry the browser half
// of the desktop viewer's byte stream. The repo deliberately has no
// server-side websocket anywhere (docs/attach.md); the browser is the one
// peer that cannot speak a raw HTTP upgrade, so the websocket lives here at
// the very edge, in the CLI's localhost bridge, rather than in the
// orchestrator or the guest.
//
// Scope: binary byte streaming only. Message boundaries are not surfaced —
// RFB is a byte stream and noVNC does not care how it is chunked — which is
// what keeps this small enough to be dependency-free.

import (
	"bufio"
	"crypto/sha1" // #nosec G505 -- mandated by RFC 6455, not used for secrecy
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

// wsMagic is the fixed GUID every websocket handshake concatenates with the
// client key, per RFC 6455 section 1.3.
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpContinuation = 0x0
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xa
)

// wsMaxControlPayload is the RFC's cap on control frame payloads.
const wsMaxControlPayload = 125

// wsWriteChunk bounds one outgoing data frame. Anything works; small enough
// that the viewer starts painting before a full framebuffer update arrives.
const wsWriteChunk = 32 << 10

// wsKeepaliveInterval paces server pings. Localhost rarely needs them, but
// they are cheap and they keep the bridge honest if it ever fronts a proxy.
const wsKeepaliveInterval = 30 * time.Second

// wsConn is an accepted websocket connection exposed as a byte stream: Read
// returns data frame payloads (control frames are handled internally), Write
// sends binary frames. Reads are single-reader; writes are serialized
// internally because the keepalive ping fires from its own goroutine.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex

	// read state of the data frame currently being drained
	remaining  int64
	masked     bool
	mask       [4]byte
	maskOffset int
}

// wsUpgrade answers the handshake and hands back the connection. On failure
// it has already written an HTTP error response.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(r.Header.Get("Connection"), "upgrade") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a websocket handshake")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("missing websocket key")
	}

	sum := sha1.Sum([]byte(key + wsMagic)) // #nosec G401 -- see import comment
	accept := base64.StdEncoding.EncodeToString(sum[:])

	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "connection is not hijackable", http.StatusInternalServerError)
		return nil, err
	}
	// the stream idles whenever the desktop is quiet; no deadline may cut it
	_ = conn.SetDeadline(time.Time{})

	if _, err := buf.WriteString(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n",
	); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsConn{conn: conn, br: buf.Reader}, nil
}

// headerHasToken reports whether a comma-separated header value contains the
// token, case-insensitively. Browsers send "Connection: keep-alive, Upgrade",
// so an equality check is not enough.
func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// Read returns the next data bytes, transparently answering pings and
// finishing with io.EOF when the client closes.
func (c *wsConn) Read(p []byte) (int, error) {
	for c.remaining == 0 {
		if err := c.nextDataFrame(); err != nil {
			return 0, err
		}
	}

	n := len(p)
	if int64(n) > c.remaining {
		n = int(c.remaining)
	}
	if _, err := io.ReadFull(c.br, p[:n]); err != nil {
		return 0, err
	}
	if c.masked {
		for i := 0; i < n; i++ {
			p[i] ^= c.mask[c.maskOffset&3]
			c.maskOffset++
		}
	}
	c.remaining -= int64(n)
	return n, nil
}

// nextDataFrame advances past control frames to the head of the next data
// frame, leaving its payload length and mask in the read state.
func (c *wsConn) nextDataFrame() error {
	for {
		var head [2]byte
		if _, err := io.ReadFull(c.br, head[:]); err != nil {
			return err
		}
		opcode := head[0] & 0x0f
		masked := head[1]&0x80 != 0

		length := int64(head[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return err
			}
			length = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return err
			}
			v := binary.BigEndian.Uint64(ext[:])
			if v > 1<<62 {
				return fmt.Errorf("websocket: frame length overflow: %d", v)
			}
			length = int64(v)
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return err
			}
		}

		// control frames are handled here and never surface to the caller
		if opcode >= 0x8 {
			if length > wsMaxControlPayload {
				return fmt.Errorf("websocket: oversized control frame: %d", length)
			}
			payload := make([]byte, length)
			if _, err := io.ReadFull(c.br, payload); err != nil {
				return err
			}
			if masked {
				for i := range payload {
					payload[i] ^= mask[i&3]
				}
			}
			switch opcode {
			case wsOpClose:
				// echo the close and end the stream
				_ = c.writeFrame(wsOpClose, payload)
				return io.EOF
			case wsOpPing:
				if err := c.writeFrame(wsOpPong, payload); err != nil {
					return err
				}
			}
			// pongs are noted by their arrival and otherwise ignored
			continue
		}

		// RFC 6455 5.1: client frames must be masked. A peer that does not
		// mask is broken; refuse rather than desync.
		if !masked {
			return errors.New("websocket: unmasked client data frame")
		}

		// data frame: text, binary, or a continuation of either. RFB is a
		// byte stream, so all are drained identically and fragmentation
		// needs no reassembly.
		_ = opcode // wsOpContinuation and wsOpBinary are treated alike
		c.remaining = length
		c.masked = masked
		c.mask = mask
		c.maskOffset = 0
		return nil
	}
}

// Write sends p as one or more binary frames.
func (c *wsConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > wsWriteChunk {
			n = wsWriteChunk
		}
		if err := c.writeFrame(wsOpBinary, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// writeFrame sends one complete (fin=1) unmasked frame, as servers must.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	head := make([]byte, 2, 10)
	head[0] = 0x80 | opcode
	switch {
	case len(payload) < 126:
		head[1] = byte(len(payload))
	case len(payload) <= 0xffff:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(len(payload)))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(len(payload)))
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(head); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// Ping sends a keepalive ping.
func (c *wsConn) Ping() error { return c.writeFrame(wsOpPing, nil) }

// Close sends a normal-closure frame best effort and tears the socket down.
func (c *wsConn) Close() error {
	_ = c.writeFrame(wsOpClose, []byte{0x03, 0xe8}) // 1000, normal closure
	return c.conn.Close()
}
