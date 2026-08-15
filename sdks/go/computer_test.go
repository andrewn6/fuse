package fuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestComputer(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"screenshot":"cGl4ZWxz"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Environments.Computer(context.Background(), "vm-1", ComputerActionRequest{
		Action:     "left_click",
		Coordinate: []int{100, 200},
	})
	if err != nil {
		t.Fatalf("Computer: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/environments/vm-1/computer" {
		t.Fatalf("request = %s %s, want POST /v1/environments/vm-1/computer", gotMethod, gotPath)
	}
	var sent ComputerActionRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("sent body is not json: %v", err)
	}
	if sent.Action != "left_click" || len(sent.Coordinate) != 2 {
		t.Fatalf("sent = %+v", sent)
	}
	if res.Screenshot != "cGl4ZWxz" {
		t.Fatalf("screenshot = %q", res.Screenshot)
	}
}

func TestComputerDisplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/vm-1/computer" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"display":":1","up":true,"width":1280,"height":800}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Environments.ComputerDisplay(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("ComputerDisplay: %v", err)
	}
	if !res.Up || res.Width != 1280 || res.Height != 800 {
		t.Fatalf("display = %+v", res)
	}
}

func TestComputerValidation(t *testing.T) {
	c, err := New("http://127.0.0.1:0", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Environments.Computer(context.Background(), "", ComputerActionRequest{Action: "screenshot"}); err == nil {
		t.Fatal("empty vm id must be rejected before any request is made")
	}
	if _, err := c.Environments.Computer(context.Background(), "vm-1", ComputerActionRequest{}); err == nil {
		t.Fatal("empty action must be rejected before any request is made")
	}
	if _, err := c.Environments.ComputerDisplay(context.Background(), ""); err == nil {
		t.Fatal("empty vm id must be rejected before any request is made")
	}
}
