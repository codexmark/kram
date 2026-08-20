package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRunReportsConfigLoadFailureBeforeStartingGateway(t *testing.T) {
	err := run(context.Background(), t.TempDir()+"/missing.yaml", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "loading config") {
		t.Fatalf("run error = %v, want contextual config-load failure", err)
	}
}
