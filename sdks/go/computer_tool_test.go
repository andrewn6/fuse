package fuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputerToolResult(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"x:5 y:7","screenshot":"cGl4ZWxz"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"action":"left_click","coordinate":[1,2],"future_field":true}`)
	res, err := c.Environments.ComputerToolResult(context.Background(), "vm-1", input)
	if err != nil {
		t.Fatalf("ComputerToolResult: %v", err)
	}

	// the tool input travels verbatim, unknown fields included: the guest
	// owns the action schema, not this SDK.
	if !strings.Contains(string(gotBody), "future_field") {
		t.Fatalf("sent body dropped unknown fields: %s", gotBody)
	}

	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if len(res.Content) != 2 {
		t.Fatalf("content = %+v, want a text and an image block", res.Content)
	}
	if res.Content[0].Type != "text" || res.Content[0].Text != "x:5 y:7" {
		t.Fatalf("text block = %+v", res.Content[0])
	}
	img := res.Content[1]
	if img.Type != "image" || img.Source == nil ||
		img.Source.Type != "base64" || img.Source.MediaType != "image/png" || img.Source.Data != "cGl4ZWxz" {
		t.Fatalf("image block = %+v", img)
	}

	// the blocks marshal into the exact Messages API shape.
	b, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"text","text":"x:5 y:7"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cGl4ZWxz"}}]`
	if string(b) != want {
		t.Fatalf("marshalled content = %s\nwant %s", b, want)
	}
}

// a refused action is the model's problem to fix, so it becomes error content
// for the tool_result rather than a Go error that would crash the loop.
func TestComputerToolResultRefusalBecomesErrorContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"display :1 is not up"}}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Environments.ComputerToolResult(context.Background(), "vm-1",
		json.RawMessage(`{"action":"screenshot"}`))
	if err != nil {
		t.Fatalf("refusal should not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("result = %+v, want IsError", res)
	}
	if len(res.Content) != 1 || !strings.Contains(res.Content[0].Text, "display :1 is not up") {
		t.Fatalf("content = %+v, want the refusal reason", res.Content)
	}
}

// auth failures the model cannot fix stay Go errors.
func TestComputerToolResultAuthFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Environments.ComputerToolResult(context.Background(), "vm-1",
		json.RawMessage(`{"action":"screenshot"}`)); err == nil {
		t.Fatal("a 401 must surface as a Go error, not as tool content")
	}
}

func TestComputerToolResultNoAction(t *testing.T) {
	c, err := New("http://127.0.0.1:0", "tok")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Environments.ComputerToolResult(context.Background(), "vm-1", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("missing action should be error content, got: %v", err)
	}
	if !res.IsError {
		t.Fatalf("result = %+v, want IsError", res)
	}
}
