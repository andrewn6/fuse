package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// testComputer returns a computer whose exec layer records every argv instead
// of running it, and whose readiness always passes. A "scrot" invocation
// writes a real PNG to the requested path so the capture path works end to
// end without a display.
func testComputer(t *testing.T) (*computer, *[][]string) {
	t.Helper()
	var calls [][]string
	c := newComputer(":1")
	c.ready = func() error { return nil }
	c.run = func(_ context.Context, argv []string) (string, error) {
		calls = append(calls, argv)
		if argv[0] == "scrot" {
			if err := os.WriteFile(argv[2], makePNG(t, 8, 8), 0o644); err != nil {
				return "", err
			}
		}
		if argv[0] == "xdotool" && argv[1] == "getmouselocation" {
			return "x:5 y:7 screen:0 window:1\n", nil
		}
		if argv[0] == "xdotool" && argv[1] == "getdisplaygeometry" {
			return "1024 768\n", nil
		}
		return "", nil
	}
	return c, &calls
}

// makePNG builds a small image whose pixel values encode their coordinates,
// so a crop can be verified by looking at the pixels that survived.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func postAction(t *testing.T, c *computer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/computer/action", strings.NewReader(body))
	rec := httptest.NewRecorder()
	c.handleAction(rec, req)
	return rec
}

func TestComputerActionArgv(t *testing.T) {
	cases := []struct {
		name string
		body string
		want [][]string
	}{
		{
			name: "left_click at coordinate",
			body: `{"action":"left_click","coordinate":[100,200]}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "100", "200", "click", "1"}},
		},
		{
			name: "right_click without coordinate",
			body: `{"action":"right_click"}`,
			want: [][]string{{"xdotool", "click", "3"}},
		},
		{
			name: "double_click repeats",
			body: `{"action":"double_click","coordinate":[1,2]}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "1", "2", "click", "--repeat", "2", "--delay", "10", "1"}},
		},
		{
			name: "triple_click repeats",
			body: `{"action":"triple_click","coordinate":[1,2]}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "1", "2", "click", "--repeat", "3", "--delay", "10", "1"}},
		},
		{
			name: "click with held modifiers presses and releases around it",
			body: `{"action":"left_click","coordinate":[3,4],"text":"ctrl+shift"}`,
			want: [][]string{
				{"xdotool", "keydown", "--", "ctrl+shift"},
				{"xdotool", "mousemove", "--sync", "3", "4", "click", "1"},
				{"xdotool", "keyup", "--", "ctrl+shift"},
			},
		},
		{
			name: "mouse_move",
			body: `{"action":"mouse_move","coordinate":[9,9]}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "9", "9"}},
		},
		{
			name: "left_click_drag chains press move release",
			body: `{"action":"left_click_drag","start_coordinate":[1,1],"coordinate":[5,5]}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "1", "1", "mousedown", "1", "mousemove", "--sync", "5", "5", "mouseup", "1"}},
		},
		{
			name: "left_mouse_down",
			body: `{"action":"left_mouse_down","coordinate":[2,2]}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "2", "2", "mousedown", "1"}},
		},
		{
			name: "left_mouse_up",
			body: `{"action":"left_mouse_up"}`,
			want: [][]string{{"xdotool", "mouseup", "1"}},
		},
		{
			name: "type passes the payload verbatim behind --",
			body: `{"action":"type","text":"hello; rm -rf / $(boom)"}`,
			want: [][]string{{"xdotool", "type", "--delay", "12", "--", "hello; rm -rf / $(boom)"}},
		},
		{
			name: "key combo",
			body: `{"action":"key","text":"ctrl+shift+t"}`,
			want: [][]string{{"xdotool", "key", "--", "ctrl+shift+t"}},
		},
		{
			name: "scroll moves then clicks the wheel button",
			body: `{"action":"scroll","coordinate":[10,20],"scroll_direction":"down","scroll_amount":3}`,
			want: [][]string{{"xdotool", "mousemove", "--sync", "10", "20", "click", "--repeat", "3", "5"}},
		},
		{
			name: "scroll up without coordinate",
			body: `{"action":"scroll","scroll_direction":"up","scroll_amount":1}`,
			want: [][]string{{"xdotool", "click", "--repeat", "1", "4"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := testComputer(t)
			rec := postAction(t, c, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			// every input action also captures a screenshot; drop the trailing
			// scrot call before comparing the xdotool sequence.
			got := *calls
			if len(got) == 0 || got[len(got)-1][0] != "scrot" {
				t.Fatalf("expected a trailing screenshot capture, calls: %v", got)
			}
			got = got[:len(got)-1]
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argv = %v, want %v", got, tc.want)
			}
			var res computerResult
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if res.Screenshot == "" {
				t.Fatalf("expected a screenshot on %s", tc.name)
			}
		})
	}
}

func TestCoordinateJSONRoundTrip(t *testing.T) {
	var a computerAction
	body := `{"action":"left_click","coordinate":[100,200]}`
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Coordinate == nil || a.Coordinate.X != 100 || a.Coordinate.Y != 200 {
		t.Fatalf("coordinate = %+v, want {100 200}", a.Coordinate)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// the struct must keep encoding as the [x, y] array claude emits.
	if string(out) != body {
		t.Fatalf("marshal = %s, want %s", out, body)
	}
}

func TestComputerActionValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing action", `{}`, "action is required"},
		{"unknown action", `{"action":"format_disk"}`, "unknown action"},
		{"malformed json", `{`, "malformed action"},
		{"click coordinate wrong arity", `{"action":"left_click","coordinate":[1]}`, "want [x, y]"},
		{"click coordinate negative", `{"action":"left_click","coordinate":[-1,5]}`, "out of range"},
		{"click coordinate huge", `{"action":"left_click","coordinate":[1,99999]}`, "out of range"},
		{"mouse_move requires coordinate", `{"action":"mouse_move"}`, "want [x, y]"},
		{"drag requires start", `{"action":"left_click_drag","coordinate":[5,5]}`, "start_coordinate"},
		{"type requires text", `{"action":"type"}`, "text is required"},
		{"key requires text", `{"action":"key"}`, "text is required"},
		{"key rejects shell metacharacters", `{"action":"key","text":"ctrl+c; reboot"}`, "not a valid keysym"},
		{"key rejects spaces", `{"action":"key","text":"ctrl t"}`, "not a valid keysym"},
		{"modifier click rejects bad combo", `{"action":"left_click","coordinate":[1,1],"text":"ctrl-c()"}`, "not a valid keysym"},
		{"hold_key requires duration", `{"action":"hold_key","text":"ctrl"}`, "duration must be"},
		{"hold_key caps duration", `{"action":"hold_key","text":"ctrl","duration":9999}`, "duration must be"},
		{"wait caps duration", `{"action":"wait","duration":101}`, "duration must be"},
		{"scroll requires direction", `{"action":"scroll","scroll_amount":3}`, "scroll_direction"},
		{"scroll rejects bad direction", `{"action":"scroll","scroll_direction":"sideways","scroll_amount":3}`, "scroll_direction"},
		{"scroll requires amount", `{"action":"scroll","scroll_direction":"up"}`, "scroll_amount"},
		{"zoom requires region", `{"action":"zoom"}`, "region wants"},
		{"zoom rejects inverted region", `{"action":"zoom","region":[10,10,5,20]}`, "positive width"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := testComputer(t)
			rec := postAction(t, c, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %q does not mention %q", rec.Body.String(), tc.want)
			}
			for _, argv := range *calls {
				if argv[0] == "xdotool" {
					t.Fatalf("rejected action still reached xdotool: %v", argv)
				}
			}
		})
	}
}

func TestComputerZoomCropsRegion(t *testing.T) {
	c, _ := testComputer(t)
	rec := postAction(t, c, `{"action":"zoom","region":[2,2,6,6]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var res computerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(res.Screenshot)
	if err != nil {
		t.Fatalf("screenshot is not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("screenshot is not a png: %v", err)
	}
	if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 4 {
		t.Fatalf("zoom size = %dx%d, want 4x4", img.Bounds().Dx(), img.Bounds().Dy())
	}
	// pixel (0,0) of the crop is pixel (2,2) of the capture, whose red
	// channel encodes its original x.
	r, _, _, _ := img.At(0, 0).RGBA()
	if uint8(r>>8) != 20 {
		t.Fatalf("crop origin pixel red = %d, want 20 (source x=2)", uint8(r>>8))
	}
}

func TestComputerCursorPositionSkipsScreenshot(t *testing.T) {
	c, calls := testComputer(t)
	rec := postAction(t, c, `{"action":"cursor_position"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var res computerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !strings.Contains(res.Output, "x:5 y:7") {
		t.Fatalf("output = %q, want cursor location", res.Output)
	}
	if res.Screenshot != "" {
		t.Fatal("cursor_position should not capture a screenshot")
	}
	for _, argv := range *calls {
		if argv[0] == "scrot" {
			t.Fatal("cursor_position invoked scrot")
		}
	}
}

func TestComputerScreenshotRoute(t *testing.T) {
	c, _ := testComputer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/computer/screenshot", nil)
	rec := httptest.NewRecorder()
	c.handleScreenshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var res computerResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if _, err := base64.StdEncoding.DecodeString(res.Screenshot); err != nil {
		t.Fatalf("screenshot is not base64: %v", err)
	}
}

func TestComputerNoDisplayAnswers503(t *testing.T) {
	c := newComputer(":93")
	c.ready = func() error { return errors.New("display :93 is not up") }
	rec := postAction(t, c, `{"action":"screenshot"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not up") {
		t.Fatalf("503 body should say why: %s", rec.Body.String())
	}
}

func TestComputerDisplayRouteReportsDown(t *testing.T) {
	c := newComputer(":93")
	c.ready = func() error { return errors.New("display :93 is not up") }
	req := httptest.NewRequest(http.MethodGet, "/v1/computer/display", nil)
	rec := httptest.NewRecorder()
	c.handleDisplay(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (this route answers up=false, not 503)", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["up"] != false {
		t.Fatalf("up = %v, want false", body["up"])
	}
}

func TestComputerDisplayRouteReportsGeometry(t *testing.T) {
	c, _ := testComputer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/computer/display", nil)
	rec := httptest.NewRecorder()
	c.handleDisplay(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["up"] != true || body["width"] != float64(1024) || body["height"] != float64(768) {
		t.Fatalf("display = %+v, want up 1024x768", body)
	}
}

func TestComputerRoutesRequireToken(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1", display: ":1"}, "secret-token", nil, 0, false, nil))
	defer srv.Close()
	for _, path := range []string{"/v1/computer/action", "/v1/computer/screenshot", "/v1/computer/display"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestComputerSocketPath(t *testing.T) {
	cases := map[string]string{
		":1":   "/tmp/.X11-unix/X1",
		":1.0": "/tmp/.X11-unix/X1",
		":99":  "/tmp/.X11-unix/X99",
	}
	for display, want := range cases {
		if got := newComputer(display).socketPath(); got != want {
			t.Fatalf("socketPath(%s) = %s, want %s", display, got, want)
		}
	}
}
