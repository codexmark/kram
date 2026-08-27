package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTurnControlEndpoints covers the client side of the detachable-turn
// contract (#112/#110/#113): attach, interrupt, steer and rewind — each
// hitting the right path with the right payload and surfacing daemon
// errors.
func TestTurnControlEndpoints(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		switch {
		case strings.HasSuffix(r.URL.Path, "/turn"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"type\":\"delta\",\"content\":\"replayed\"}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			w.Write([]byte(`{"status":"interrupting"}`))
		case strings.HasSuffix(r.URL.Path, "/steer"):
			w.Write([]byte(`{"status":"queued"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/rewind":
			w.Write([]byte(`{"id":"abcdef0123456789","message":"auto checkpoint","created_at":"2026-08-27T10:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/rewind":
			w.Write([]byte(`{"restored":{"snapshot_id":"abcdef0123456789","changes":[{"path":"a.txt","status":"will be overwritten"}]},"snapshot":{"id":"abcdef0123456789","message":"auto checkpoint"}}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")

	stream, err := c.AttachTurn(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	evt, done, err := stream.Next()
	if err != nil || done || evt.Type != "delta" || evt.Content != "replayed" {
		t.Fatalf("attach replay = %+v done=%v err=%v", evt, done, err)
	}
	stream.Close()
	if gotPath != "GET /sessions/s1/turn" {
		t.Fatalf("attach path = %q", gotPath)
	}

	if err := c.Interrupt(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /sessions/s1/interrupt" {
		t.Fatalf("interrupt path = %q", gotPath)
	}

	if err := c.Steer(context.Background(), "s1", "also do X"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /sessions/s1/steer" || !strings.Contains(gotBody, "also do X") {
		t.Fatalf("steer = %q body=%q", gotPath, gotBody)
	}

	cp, err := c.RewindInfo(context.Background())
	if err != nil || cp.ShortID() != "abcdef012345" || cp.Message != "auto checkpoint" {
		t.Fatalf("rewind info = %+v err=%v", cp, err)
	}

	res, err := c.Rewind(context.Background(), cp.ID)
	if err != nil || len(res.Restored.Changes) != 1 || res.Restored.Changes[0].Path != "a.txt" {
		t.Fatalf("rewind = %+v err=%v", res, err)
	}
	var sentID struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(gotBody), &sentID) != nil || sentID.ID != cp.ID {
		t.Fatalf("rewind must pin the shown id, body=%q", gotBody)
	}
}

// TestTurnControlErrorsSurfaceDaemonMessage: every new endpoint's error
// body reaches the caller as a readable error.
func TestTurnControlErrorsSurfaceDaemonMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"no turn is running"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "")

	if _, err := c.AttachTurn(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "no turn is running") {
		t.Fatalf("attach err = %v", err)
	}
	if err := c.Steer(context.Background(), "s1", "x"); err == nil || !strings.Contains(err.Error(), "no turn is running") {
		t.Fatalf("steer err = %v", err)
	}
	if err := c.Interrupt(context.Background(), "s1"); err == nil {
		t.Fatal("interrupt err = nil")
	}
	if _, err := c.RewindInfo(context.Background()); err == nil {
		t.Fatal("rewind info err = nil")
	}
	if _, err := c.Rewind(context.Background(), "id"); err == nil {
		t.Fatal("rewind err = nil")
	}
}

func TestRewindCheckpointShortIDShortInput(t *testing.T) {
	if got := (RewindCheckpoint{ID: "short"}).ShortID(); got != "short" {
		t.Fatalf("ShortID(short) = %q", got)
	}
}
