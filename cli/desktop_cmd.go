package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

// The viewer assets: the vendored noVNC RFB client (see novnc/README.md) and
// our page embedding it. Compiled into the binary so the viewer works with no
// network access beyond the orchestrator itself.
//
//go:embed novnc/core novnc/vendor novnc/viewer.html
var novncFS embed.FS

func newDesktopCmd() *cobra.Command {
	var (
		listen   string
		viewOnly bool
		noOpen   bool
	)

	cmd := &cobra.Command{
		Use:   "desktop <id>",
		Short: "Open a live view of an environment's desktop",
		Long: "Open a live, interactive view of a running environment's desktop in the\n" +
			"browser. The view carries input, so this is also how a human takes over a\n" +
			"session an agent is driving: log in, pass a captcha, then hand it back.\n\n" +
			"    fuse desktop vm-1\n" +
			"    fuse desktop vm-1 --view-only\n\n" +
			"The CLI serves the viewer on localhost and bridges it to the orchestrator's\n" +
			"authed stream route; nothing is exposed beyond this machine, and the guest's\n" +
			"vnc server is never reachable directly. The command runs until interrupted.\n\n" +
			"Requires an environment booted from a desktop image (a Fusefile with a\n" +
			"desktop block).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDesktop(cmd.Context(), args[0], listen, viewOnly, noOpen)
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0",
		"local address to serve the viewer on")
	cmd.Flags().BoolVar(&viewOnly, "view-only", false,
		"watch without sending mouse or keyboard input")
	cmd.Flags().BoolVar(&noOpen, "no-open", false,
		"print the viewer url without opening a browser")
	return cmd
}

func runDesktop(ctx context.Context, vmID, listen string, viewOnly, noOpen bool) error {
	cl, _, err := app.client()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Preflight the stream once, so "not a desktop image" or "vnc unit down"
	// is a clear error here rather than a failed connection in a browser tab.
	probe, err := cl.Environments.ComputerStream(ctx, vmID)
	if err != nil {
		return friendly(err)
	}
	_ = probe.Close()

	// The bridge binds localhost, but any local process could still reach the
	// port; a per-invocation bearer in the viewer url keeps the stream private
	// to whoever ran the command.
	token, err := randomHex(16)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, err := novncFS.ReadFile("novnc/viewer.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	// the embedded FS root contains "novnc/", so URL paths map to it directly
	mux.Handle("/novnc/", http.FileServer(http.FS(novncFS)))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		serveDesktopBridge(w, r, cl, vmID)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	q := url.Values{}
	q.Set("env", vmID)
	q.Set("token", token)
	if viewOnly {
		q.Set("view_only", "1")
	}
	viewURL := fmt.Sprintf("http://%s/?%s", ln.Addr(), q.Encode())

	fmt.Printf("desktop view of %s: %s\n", vmID, viewURL)
	if !noOpen {
		if err := openBrowser(viewURL); err != nil {
			fmt.Printf("could not open a browser (%v); open the url yourself\n", err)
		}
	}
	fmt.Println("serving until interrupted; press ctrl-c to stop")

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// serveDesktopBridge splices one websocket viewer onto one orchestrator
// stream. Each browser connection gets its own guest stream; x11vnc runs
// shared, so two tabs are two viewers of the same desktop.
func serveDesktopBridge(w http.ResponseWriter, r *http.Request, cl *fuse.Client, vmID string) {
	// open the guest stream before the websocket handshake, while an HTTP
	// error can still be reported
	stream, err := cl.Environments.ComputerStream(r.Context(), vmID)
	if err != nil {
		http.Error(w, friendly(err).Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = stream.Close() }()

	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, ws)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ws, stream)
		done <- struct{}{}
	}()

	// keepalive pings hold the connection open across quiet stretches; an
	// idle desktop produces zero RFB traffic. a failed ping means the viewer
	// is gone, which the copy loops will notice on their own.
	ticker := time.NewTicker(wsKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_ = ws.Ping()
		}
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser opens url in the platform's default browser, best effort.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return errors.New("xdg-open not found")
		}
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
}
