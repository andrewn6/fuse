package main

// GET /v1/computer/stream is the live view of the desktop: an HTTP/1.1
// upgrade (fuse-vnc/1) after which the connection carries raw RFB bytes
// between the caller and the local vnc server, both directions.
//
// The vnc server (x11vnc, fuse-vnc.service) binds 127.0.0.1 only, so this
// route is the sole way to reach it: the token check lives in the proxy hop
// (fused's bearer auth), not in the vnc server, and the port is never
// exposed raw. fused does no RFB parsing — it is a byte pump, the same role
// the orchestrator plays for attach.

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// vncProto is the upgrade token. Versioned like fuse-attach/1 so the
	// wire can change without breaking old clients silently.
	vncProto = "fuse-vnc/1"
	// vncAddr is where the image's fuse-vnc unit serves. Fixed like the
	// display number: not authorable until something needs it to be.
	vncAddr = "127.0.0.1:5900"
	// vncDialTimeout bounds the local dial only; the stream that follows
	// lives as long as the viewer does.
	vncDialTimeout = 5 * time.Second
)

// handleStream serves GET /v1/computer/stream.
func (c *computer) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), vncProto) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "stream requires Upgrade: " + vncProto,
		})
		return
	}
	if err := c.ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	// dial the vnc server before hijacking: once the connection is hijacked
	// there is no ResponseWriter left to report a failure through.
	vnc, err := net.DialTimeout("tcp", c.vncAddr, vncDialTimeout)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "vnc server not reachable at " + c.vncAddr + "; is the fuse-vnc unit running?",
		})
		return
	}
	defer func() { _ = vnc.Close() }()

	// ResponseController, not a direct type assertion, for the same reason
	// the orchestrator's attach handler uses it: only the controller walks
	// wrapped ResponseWriters to the transport underneath.
	rc := http.NewResponseController(w)
	client, buf, err := rc.Hijack()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "stream requires a hijackable HTTP/1.1 connection: " + err.Error(),
		})
		return
	}
	defer func() { _ = client.Close() }()

	// an idle desktop produces zero RFB traffic; a watched-but-quiet stream
	// must not be cut by the server's write deadline.
	_ = client.SetDeadline(time.Time{})

	if _, err := buf.WriteString(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: " + vncProto + "\r\n" +
			"Connection: Upgrade\r\n\r\n",
	); err != nil {
		return
	}
	if err := buf.Flush(); err != nil {
		return
	}

	// reads come from the bufio.Reader: hijack can hand back a reader that
	// already holds bytes the client sent right after its request head.
	spliceStream(client, buf.Reader, vnc)
}

// spliceStream pumps bytes in both directions until either end goes away;
// the deferred Closes in the caller unblock the surviving goroutine.
func spliceStream(client net.Conn, fromClient io.Reader, vnc net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(vnc, fromClient)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, vnc)
		done <- struct{}{}
	}()
	<-done
}
