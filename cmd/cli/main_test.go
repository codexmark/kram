package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunWithTitleSurfacesDaemonFailureBeforeOpeningTUI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	err := run(srv.URL, "http://gateway.invalid", "", "new session", "default", "")
	if err == nil || !strings.Contains(err.Error(), "could not reach kram-daemon") {
		t.Fatalf("run error = %v, want daemon reachability context", err)
	}
}
