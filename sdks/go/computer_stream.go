package fuse

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/url"
)

// VNCProto is the Upgrade token that opens a live desktop stream.
const VNCProto = "fuse-vnc/1"

// ComputerStream is a live duplex connection to an environment's desktop.
// The bytes are raw RFB (VNC), verbatim from the vnc server inside the
// guest: hand the stream to any RFB client (the `fuse desktop` CLI command
// bridges it to a browser viewer). There is no frame protocol on top.
type ComputerStream struct {
	conn net.Conn
	r    *bufio.Reader
}

func (s *ComputerStream) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *ComputerStream) Write(p []byte) (int, error) { return s.conn.Write(p) }

// Close tears down the stream.
func (s *ComputerStream) Close() error { return s.conn.Close() }

// ComputerStream opens the live view of a running environment's desktop:
// a raw RFB byte stream carrying both the display and input, so it is also
// how a human takes over a session an agent is driving.
//
// It requires an environment booted from a desktop image; on any other
// image the server answers 503 with a reason. Unlike Attach it accepts API
// keys. The caller owns the stream and must Close it.
func (s *EnvironmentsService) ComputerStream(ctx context.Context, vmID string) (*ComputerStream, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}

	path := "/v1/environments/" + url.PathEscape(vmID) + "/computer/stream"
	conn, br, err := s.t.dialUpgrade(ctx, path, url.Values{}, VNCProto, "computer stream")
	if err != nil {
		return nil, err
	}
	return &ComputerStream{conn: conn, r: br}, nil
}
