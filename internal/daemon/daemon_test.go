package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestRunStartsAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	err := Run(ctx, Config{Host: "127.0.0.1", Port: freePort(t), DBPath: filepath.Join(t.TempDir(), "daemon.db"), GatewayURL: "http://127.0.0.1:1", Workspace: t.TempDir(), MaxTurns: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsStoreOpenFailure(t *testing.T) {
	err := Run(context.Background(), Config{DBPath: t.TempDir(), Workspace: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("Run with directory DB path succeeded")
	}
}
