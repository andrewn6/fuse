package main

// The computer control surface: the guest half of Anthropic's computer use
// tool. Claude never reaches this directly — the caller's agent loop receives
// a tool_use block, turns it into one POST here, and returns the screenshot
// as the tool_result. fused only executes actions against the local display.
//
// Everything in an action is untrusted input that becomes a process argv.
// Invocations are built as argv slices and executed directly — no shell, so
// no element can be reinterpreted as a pipeline or a redirect — and key names
// are held to a charset, the same treatment the env-key validation in
// internal/fusefile/compile.go gives identifiers that reach a file the guest
// sources.
//
// The routes are registered on every image. On a non-desktop image the
// display socket is absent and every call answers 503 with a reason, which is
// what makes them safe to serve unconditionally.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// maxComputerBody bounds the action request; the largest legitimate field
	// is a typed string, and 1MB is far beyond any of those.
	maxComputerBody = 1 << 20
	// maxTextLen bounds type/key payloads.
	maxTextLen = 32768
	// maxCoordinate bounds any coordinate or region component; no supported
	// display is anywhere near this large.
	maxCoordinate = 10000
	// maxHoldSeconds bounds hold_key and wait durations.
	maxHoldSeconds = 100
	// settleDelay is how long an input action gets to repaint before the
	// screenshot that rides back on its response is captured.
	settleDelay = 300 * time.Millisecond
	// execTimeout bounds a single xdotool/scrot invocation.
	execTimeout = 10 * time.Second
)

// keyPattern admits xdotool keysym combos ("ctrl+shift+t", "Return",
// "XF86AudioMute"). Anything else — spaces, quotes, path separators — is
// rejected before it can become an argv element.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_+]{1,128}$`)

// coordinate is an x/y pixel pair. on the wire it stays the two-element
// [x, y] array Claude's tool_use input carries; only the go side gets named
// fields.
type coordinate struct {
	X int
	Y int
}

func (c coordinate) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int{c.X, c.Y})
}

func (c *coordinate) UnmarshalJSON(data []byte) error {
	var pair []int
	if err := json.Unmarshal(data, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("want [x, y], got %d elements", len(pair))
	}
	c.X, c.Y = pair[0], pair[1]
	return nil
}

// computerAction is the request body of POST /v1/computer/action. The field
// names mirror the tool_use input Claude emits so a caller's translation
// layer stays mechanical.
type computerAction struct {
	Action          string      `json:"action"`
	Coordinate      *coordinate `json:"coordinate,omitempty"`
	StartCoordinate *coordinate `json:"start_coordinate,omitempty"`
	Text            string      `json:"text,omitempty"`
	Region          []int       `json:"region,omitempty"`
	Duration        float64     `json:"duration,omitempty"`
	ScrollDirection string      `json:"scroll_direction,omitempty"`
	ScrollAmount    int         `json:"scroll_amount,omitempty"`
}

// computerResult is the response body. Screenshot is a base64 PNG, present on
// every action that implies one.
type computerResult struct {
	Output     string `json:"output,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

// scrollButtons maps a scroll direction to the X button that emits it.
var scrollButtons = map[string]string{
	"up":    "4",
	"down":  "5",
	"left":  "6",
	"right": "7",
}

// computer executes actions against one X display. run and ready are fields
// so tests can assert the argv an action compiles to, and drive the handlers,
// without an X server present.
type computer struct {
	display string
	// vncAddr is where the stream route finds the local vnc server; a field
	// so tests can point it at a stub listener.
	vncAddr string
	run     func(ctx context.Context, argv []string) (string, error)
	ready   func() error
}

func newComputer(display string) *computer {
	if display == "" {
		display = ":1"
	}
	c := &computer{display: display, vncAddr: vncAddr}
	c.run = c.execRun
	c.ready = c.checkReady
	return c
}

// socketPath returns the unix socket the display serves on, derived from the
// display string (":1" or ":1.0" -> /tmp/.X11-unix/X1).
func (c *computer) socketPath() string {
	num := strings.TrimPrefix(c.display, ":")
	if i := strings.IndexByte(num, '.'); i >= 0 {
		num = num[:i]
	}
	return "/tmp/.X11-unix/X" + num
}

// checkReady reports why the surface cannot serve, or nil when it can. The
// checks name the missing piece so a caller on a non-desktop image learns
// what is absent rather than getting a bare 503.
func (c *computer) checkReady() error {
	if _, err := exec.LookPath("xdotool"); err != nil {
		return fmt.Errorf("xdotool not installed; this is not a desktop image")
	}
	if _, err := exec.LookPath("scrot"); err != nil {
		return fmt.Errorf("scrot not installed; this is not a desktop image")
	}
	if _, err := os.Stat(c.socketPath()); err != nil {
		return fmt.Errorf("display %s is not up (no socket at %s)", c.display, c.socketPath())
	}
	return nil
}

// execRun executes one argv against the display and returns its stdout.
func (c *computer) execRun(ctx context.Context, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "DISPLAY="+c.display)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", argv[0], msg)
	}
	return out.String(), nil
}

// capturePNG takes a screenshot of the whole display and returns the PNG
// bytes. scrot writes to a file, so the capture goes through a temp path that
// is removed either way.
func (c *computer) capturePNG(ctx context.Context) ([]byte, error) {
	f, err := os.CreateTemp("", "fused-screenshot-*.png")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()

	if _, err := c.run(ctx, []string{"scrot", "-o", path}); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("screenshot capture produced an empty file")
	}
	return b, nil
}

// geometry asks the display for its size.
func (c *computer) geometry(ctx context.Context) (w, h int, err error) {
	out, err := c.run(ctx, []string{"xdotool", "getdisplaygeometry"})
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected getdisplaygeometry output %q", out)
	}
	w, err = strconv.Atoi(fields[0])
	if err == nil {
		h, err = strconv.Atoi(fields[1])
	}
	return w, h, err
}

// actionError is a request-shape failure that should answer 400 rather
// than 500.
type actionError struct{ msg string }

func (e *actionError) Error() string { return e.msg }

func badAction(format string, args ...any) error {
	return &actionError{msg: fmt.Sprintf(format, args...)}
}

// validCoordinate checks an [x, y] pair.
func validCoordinate(name string, c *coordinate) error {
	if c == nil {
		return badAction("%s: want [x, y], got nothing", name)
	}
	for _, v := range []int{c.X, c.Y} {
		if v < 0 || v > maxCoordinate {
			return badAction("%s: %d out of range 0..%d", name, v, maxCoordinate)
		}
	}
	return nil
}

// validKeys checks a keysym combo the way env keys are checked before they
// reach a file the guest sources: charset first, questions later.
func validKeys(text string) error {
	if !keyPattern.MatchString(text) {
		return badAction("key: %q is not a valid keysym combo (letters, digits, _ and + only)", text)
	}
	return nil
}

// moveArgs returns the chained xdotool words that position the pointer, or
// nothing when the action did not name a coordinate.
func moveArgs(c *coordinate) []string {
	if c == nil {
		return nil
	}
	return []string{"mousemove", "--sync", strconv.Itoa(c.X), strconv.Itoa(c.Y)}
}

// clickButtons maps the click actions to their X button and repeat count.
var clickActions = map[string]struct {
	button string
	repeat int
}{
	"left_click":   {"1", 1},
	"right_click":  {"3", 1},
	"middle_click": {"2", 1},
	"double_click": {"1", 2},
	"triple_click": {"1", 3},
}

// perform executes one decoded action and reports its textual output (if
// any) and whether a screenshot should ride back on the response.
func (c *computer) perform(ctx context.Context, a computerAction) (output string, screenshot bool, err error) {
	switch a.Action {
	case "screenshot":
		return "", true, nil

	case "cursor_position":
		out, err := c.run(ctx, []string{"xdotool", "getmouselocation"})
		if err != nil {
			return "", false, err
		}
		return strings.TrimSpace(out), false, nil

	case "mouse_move":
		if err := validCoordinate("coordinate", a.Coordinate); err != nil {
			return "", false, err
		}
		_, err := c.run(ctx, append([]string{"xdotool"}, moveArgs(a.Coordinate)...))
		return "", true, err

	case "left_click", "right_click", "middle_click", "double_click", "triple_click":
		spec := clickActions[a.Action]
		if a.Coordinate != nil {
			if err := validCoordinate("coordinate", a.Coordinate); err != nil {
				return "", false, err
			}
		}
		// optional held modifiers ride in text ("ctrl+shift"); they are
		// pressed, the click happens, and the release is guaranteed even
		// when the click itself fails, so a failed action cannot leave the
		// session with a stuck modifier.
		if a.Text != "" {
			if err := validKeys(a.Text); err != nil {
				return "", false, err
			}
			if _, err := c.run(ctx, []string{"xdotool", "keydown", "--", a.Text}); err != nil {
				return "", false, err
			}
			defer func() { _, _ = c.run(ctx, []string{"xdotool", "keyup", "--", a.Text}) }()
		}
		argv := append([]string{"xdotool"}, moveArgs(a.Coordinate)...)
		argv = append(argv, "click")
		if spec.repeat > 1 {
			argv = append(argv, "--repeat", strconv.Itoa(spec.repeat), "--delay", "10")
		}
		argv = append(argv, spec.button)
		_, err := c.run(ctx, argv)
		return "", true, err

	case "left_click_drag":
		if err := validCoordinate("start_coordinate", a.StartCoordinate); err != nil {
			return "", false, err
		}
		if err := validCoordinate("coordinate", a.Coordinate); err != nil {
			return "", false, err
		}
		argv := append([]string{"xdotool"}, moveArgs(a.StartCoordinate)...)
		argv = append(argv, "mousedown", "1")
		argv = append(argv, moveArgs(a.Coordinate)...)
		argv = append(argv, "mouseup", "1")
		_, err := c.run(ctx, argv)
		return "", true, err

	case "left_mouse_down", "left_mouse_up":
		verb := "mousedown"
		if a.Action == "left_mouse_up" {
			verb = "mouseup"
		}
		if a.Coordinate != nil {
			if err := validCoordinate("coordinate", a.Coordinate); err != nil {
				return "", false, err
			}
		}
		argv := append([]string{"xdotool"}, moveArgs(a.Coordinate)...)
		argv = append(argv, verb, "1")
		_, err := c.run(ctx, argv)
		return "", true, err

	case "type":
		if a.Text == "" {
			return "", false, badAction("type: text is required")
		}
		if len(a.Text) > maxTextLen {
			return "", false, badAction("type: text exceeds %d bytes", maxTextLen)
		}
		// the payload sits behind `--` as a single argv element, so it is
		// typed verbatim whatever it contains.
		_, err := c.run(ctx, []string{"xdotool", "type", "--delay", "12", "--", a.Text})
		return "", true, err

	case "key":
		if a.Text == "" {
			return "", false, badAction("key: text is required")
		}
		if err := validKeys(a.Text); err != nil {
			return "", false, err
		}
		_, err := c.run(ctx, []string{"xdotool", "key", "--", a.Text})
		return "", true, err

	case "hold_key":
		if a.Text == "" {
			return "", false, badAction("hold_key: text is required")
		}
		if err := validKeys(a.Text); err != nil {
			return "", false, err
		}
		if a.Duration <= 0 || a.Duration > maxHoldSeconds {
			return "", false, badAction("hold_key: duration must be in (0, %d] seconds", maxHoldSeconds)
		}
		if _, err := c.run(ctx, []string{"xdotool", "keydown", "--", a.Text}); err != nil {
			return "", false, err
		}
		select {
		case <-time.After(time.Duration(a.Duration * float64(time.Second))):
		case <-ctx.Done():
		}
		_, err := c.run(ctx, []string{"xdotool", "keyup", "--", a.Text})
		return "", true, err

	case "scroll":
		button, ok := scrollButtons[a.ScrollDirection]
		if !ok {
			return "", false, badAction("scroll: scroll_direction must be up, down, left or right")
		}
		if a.ScrollAmount <= 0 || a.ScrollAmount > 100 {
			return "", false, badAction("scroll: scroll_amount must be in 1..100")
		}
		if a.Coordinate != nil {
			if err := validCoordinate("coordinate", a.Coordinate); err != nil {
				return "", false, err
			}
		}
		argv := append([]string{"xdotool"}, moveArgs(a.Coordinate)...)
		argv = append(argv, "click", "--repeat", strconv.Itoa(a.ScrollAmount), button)
		_, err := c.run(ctx, argv)
		return "", true, err

	case "wait":
		if a.Duration <= 0 || a.Duration > maxHoldSeconds {
			return "", false, badAction("wait: duration must be in (0, %d] seconds", maxHoldSeconds)
		}
		select {
		case <-time.After(time.Duration(a.Duration * float64(time.Second))):
		case <-ctx.Done():
		}
		return "", true, nil

	case "zoom":
		// handled by the caller: it needs the cropped screenshot itself
		// rather than a full capture appended afterwards.
		return "", false, badAction("zoom: internal dispatch error")

	case "":
		return "", false, badAction("action is required")

	default:
		return "", false, badAction("unknown action %q", a.Action)
	}
}

// cropPNG decodes a full-display capture and returns the region
// [x1, y1, x2, y2] re-encoded at full resolution, which is exactly what the
// zoom action of computer_20251124 expects.
func cropPNG(full []byte, region []int) ([]byte, error) {
	if len(region) != 4 {
		return nil, badAction("zoom: region wants [x1, y1, x2, y2], got %d elements", len(region))
	}
	for _, v := range region {
		if v < 0 || v > maxCoordinate {
			return nil, badAction("zoom: region value %d out of range 0..%d", v, maxCoordinate)
		}
	}
	x1, y1, x2, y2 := region[0], region[1], region[2], region[3]
	if x2 <= x1 || y2 <= y1 {
		return nil, badAction("zoom: region must have positive width and height")
	}
	img, err := png.Decode(bytes.NewReader(full))
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}
	rect := image.Rect(x1, y1, x2, y2).Intersect(img.Bounds())
	if rect.Empty() {
		return nil, badAction("zoom: region lies outside the display")
	}
	out := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(out, out.Bounds(), img, rect.Min, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("encode zoom region: %w", err)
	}
	return buf.Bytes(), nil
}

// handleAction serves POST /v1/computer/action.
func (c *computer) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := c.ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	var a computerAction
	body := http.MaxBytesReader(w, r.Body, maxComputerBody)
	if err := json.NewDecoder(body).Decode(&a); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed action: " + err.Error()})
		return
	}

	// zoom is its own path: the screenshot is the action.
	if a.Action == "zoom" {
		full, err := c.capturePNG(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		cropped, err := cropPNG(full, a.Region)
		if err != nil {
			writeActionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, computerResult{Screenshot: base64.StdEncoding.EncodeToString(cropped)})
		return
	}

	output, wantShot, err := c.perform(r.Context(), a)
	if err != nil {
		writeActionError(w, err)
		return
	}
	res := computerResult{Output: output}
	if wantShot {
		if a.Action != "screenshot" {
			time.Sleep(settleDelay)
		}
		shot, err := c.capturePNG(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		res.Screenshot = base64.StdEncoding.EncodeToString(shot)
	}
	writeJSON(w, http.StatusOK, res)
}

// handleScreenshot serves GET /v1/computer/screenshot, the cheap read-only
// path for callers that want pixels without side effects.
func (c *computer) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := c.ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	shot, err := c.capturePNG(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, computerResult{Screenshot: base64.StdEncoding.EncodeToString(shot)})
}

// handleDisplay serves GET /v1/computer/display: geometry and liveness, so a
// caller can populate display_width_px / display_height_px in its tool
// definition without hardcoding them. Unlike the action routes this answers
// 200 with up=false when there is no display, because "is there a desktop" is
// exactly the question it exists to answer.
func (c *computer) handleDisplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body := map[string]any{"display": c.display, "up": false, "width": 0, "height": 0}
	if err := c.ready(); err != nil {
		body["error"] = err.Error()
		writeJSON(w, http.StatusOK, body)
		return
	}
	wpx, hpx, err := c.geometry(r.Context())
	if err != nil {
		body["error"] = err.Error()
		writeJSON(w, http.StatusOK, body)
		return
	}
	body["up"] = true
	body["width"] = wpx
	body["height"] = hpx
	writeJSON(w, http.StatusOK, body)
}

// writeActionError maps request-shape failures to 400 and everything else
// (xdotool failures, capture failures) to 500.
func writeActionError(w http.ResponseWriter, err error) {
	var ae *actionError
	if errors.As(err, &ae) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}
