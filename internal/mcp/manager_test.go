package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// errAlwaysFails is a sentinel dial failure used by every "reconnect never
// succeeds" test below — its message doesn't matter, only that dial()
// returns a non-nil error.
var errAlwaysFails = errors.New("dial failed")

// stdFakeHandler answers initialize and tools/list the way a well-behaved
// fake server would, reporting the given tool names — enough for Connect's
// handshake+loadTools sequence to succeed against a fakeTransport.
func stdFakeHandler(toolNames ...string) func(method string, id *int64, params json.RawMessage) (any, bool) {
	return func(method string, id *int64, params json.RawMessage) (any, bool) {
		switch method {
		case "initialize":
			return initializeResult{ProtocolVersion: protocolVersion}, false
		case "tools/list":
			tools := make([]Tool, len(toolNames))
			for i, n := range toolNames {
				tools[i] = Tool{Name: n}
			}
			return listToolsResult{Tools: tools}, false
		default:
			return nil, false
		}
	}
}

// fakeDial returns a dial function usable as Manager.dial: it builds a
// real Client wired to an in-memory fakeTransport and runs the real
// Connect sequence (handshake + loadTools) against it, so Manager's
// reconnect logic is exercised the same way it would be against a real
// process — just without one.
func fakeDial(handle func(method string, id *int64, params json.RawMessage) (any, bool)) func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	return func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		tr := newFakeTransport(handle)
		c := &Client{name: name, cfg: cfg, transport: tr, pending: make(map[int64]chan message), done: make(chan struct{})}
		go c.dispatch()
		if err := c.handshake(ctx); err != nil {
			c.Close()
			return nil, err
		}
		if err := c.loadTools(ctx); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}

// noopSleep never actually waits — used by every test below except the
// one that specifically verifies backoff *durations*, so that reconnect
// tests run in milliseconds instead of tens of seconds.
func noopSleep(ctx context.Context, d time.Duration) {}

func TestManagerConnectAllRegression(t *testing.T) {
	// Regression: ConnectAll's basic "connect everything enabled" behavior
	// must survive the reconnect/supervision changes unchanged.
	m := newManager(context.Background(), nil, fakeDial(stdFakeHandler("echo")), noopSleep)
	defer m.Close()

	m.connectAll(map[string]ServerConfig{
		"a": {Command: "x"},
		"b": {Command: "y", Enabled: boolPtr(false)},
	})

	clients := m.Clients()
	if len(clients) != 1 {
		t.Fatalf("expected exactly 1 connected client (b is disabled), got %d: %+v", len(clients), clients)
	}
	if _, ok := clients["a"]; !ok {
		t.Fatalf("expected server %q to be connected", "a")
	}
	if len(clients["a"].Tools()) != 1 || clients["a"].Tools()[0].Name != "echo" {
		t.Errorf("expected server a to have loaded its one tool, got %+v", clients["a"].Tools())
	}
}

func TestManagerReconnectsAfterDisconnect(t *testing.T) {
	m := newManager(context.Background(), nil, fakeDial(stdFakeHandler("echo")), noopSleep)
	defer m.Close()

	m.connectAll(map[string]ServerConfig{"a": {Command: "x"}})

	original := m.Clients()["a"]
	if original == nil {
		t.Fatal("server a should be connected")
	}
	originalTransport := original.transport.(*fakeTransport)

	// Simulate the server process dying: close its transport's inbound
	// channel, same as TestClientDisconnectUnblocksPendingCalls does.
	close(originalTransport.in)

	waitForCondition(t, 2*time.Second, func() bool {
		c, ok := m.Clients()["a"]
		return ok && c != original
	})

	reconnected := m.Clients()["a"]
	if reconnected == nil {
		t.Fatal("server a should have been reconnected, not left absent")
	}
	if reconnected == original {
		t.Error("expected a brand-new client after reconnect, got the same one back")
	}
}

func TestManagerReconnectRefreshesTools(t *testing.T) {
	// The reconnect dial in this test reports a different tool set than
	// the original connect did — simulating a server that came back up
	// with an updated schema. The Manager must pick up the new list, not
	// keep serving the pre-disconnect snapshot.
	var dialCount int
	var mu sync.Mutex
	dial := func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		mu.Lock()
		dialCount++
		n := dialCount
		mu.Unlock()
		handler := stdFakeHandler("v1-tool")
		if n > 1 {
			handler = stdFakeHandler("v2-tool-a", "v2-tool-b")
		}
		return fakeDial(handler)(ctx, name, cfg)
	}

	m := newManager(context.Background(), nil, dial, noopSleep)
	defer m.Close()

	m.connectAll(map[string]ServerConfig{"a": {Command: "x"}})
	original := m.Clients()["a"]
	if len(original.Tools()) != 1 || original.Tools()[0].Name != "v1-tool" {
		t.Fatalf("expected the initial connect to load v1-tool, got %+v", original.Tools())
	}

	close(original.transport.(*fakeTransport).in)

	waitForCondition(t, 2*time.Second, func() bool {
		c, ok := m.Clients()["a"]
		return ok && c != original
	})

	reconnected := m.Clients()["a"]
	tools := reconnected.Tools()
	if len(tools) != 2 || tools[0].Name != "v2-tool-a" || tools[1].Name != "v2-tool-b" {
		t.Errorf("expected the reconnected client to carry the post-reconnect tool list, got %+v", tools)
	}
}

func TestManagerGivesUpAfterMaxReconnectAttempts(t *testing.T) {
	m := newManager(context.Background(), nil, fakeDial(stdFakeHandler("echo")), noopSleep)
	defer m.Close()

	m.connectAll(map[string]ServerConfig{"a": {Command: "x"}})
	original := m.Clients()["a"]

	// From here on, every reconnect dial fails — the server is gone for
	// good (uninstalled, machine down, whatever).
	m.dial = func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		return nil, errAlwaysFails
	}

	close(original.transport.(*fakeTransport).in)

	// The server must disappear from Clients() once reconnect attempts are
	// exhausted, and — critically — the supervisor goroutine watching it
	// must actually exit rather than looping forever. m.wg tracks exactly
	// that goroutine, so Wait() returning is proof it's gone.
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor goroutine did not exit after exhausting reconnect attempts — goroutine leak")
	}

	if _, ok := m.Clients()["a"]; ok {
		t.Error("a server that failed every reconnect attempt should be absent from Clients(), not silently kept")
	}
}

func TestBackoffDelayGrowsExponentiallyAndCaps(t *testing.T) {
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // capped — would be 64s uncapped
		60 * time.Second,
	}
	for attempt, w := range want {
		if got := backoffDelay(attempt + 1); got != w {
			t.Errorf("backoffDelay(%d) = %s, want %s", attempt+1, got, w)
		}
	}
}

func TestManagerReconnectSleepsWithGrowingBackoff(t *testing.T) {
	// Deterministic, no real waiting: the sleep hook just records what it
	// was asked to wait for, and returns immediately.
	var mu sync.Mutex
	var delays []time.Duration
	recordingSleep := func(ctx context.Context, d time.Duration) {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
	}

	m := newManager(context.Background(), nil, nil, recordingSleep)
	m.cfgs["a"] = ServerConfig{Command: "x"}
	m.dial = func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		return nil, errAlwaysFails // fail every attempt, to observe every backoff step
	}

	ok := m.reconnect("a")
	if ok {
		t.Fatal("expected reconnect to give up when every dial fails")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delays) != reconnectMaxAttempts {
		t.Fatalf("expected %d backoff sleeps (one per attempt), got %d: %v", reconnectMaxAttempts, len(delays), delays)
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] <= delays[i-1] && delays[i-1] < reconnectMaxDelay {
			t.Errorf("expected backoff to grow: delays[%d]=%s should be > delays[%d]=%s (below the cap)", i, delays[i], i-1, delays[i-1])
		}
	}
}

func TestManagerCloseStopsSupervisionWithoutReconnecting(t *testing.T) {
	// Close()'ing the Manager must not make a supervisor goroutine
	// mistake the resulting transport shutdown for a real disconnect and
	// try to reconnect into a Manager that's going away.
	dialAttempts := 0
	var mu sync.Mutex
	m := newManager(context.Background(), nil, func(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
		mu.Lock()
		dialAttempts++
		mu.Unlock()
		return fakeDial(stdFakeHandler("echo"))(ctx, name, cfg)
	}, noopSleep)

	m.connectAll(map[string]ServerConfig{"a": {Command: "x"}})

	m.Close() // cancels ctx, waits for the supervisor, then closes the client

	mu.Lock()
	attempts := dialAttempts
	mu.Unlock()
	if attempts != 1 {
		t.Errorf("expected exactly 1 dial (the initial connect) and no reconnect attempts after Close, got %d", attempts)
	}
}

func boolPtr(b bool) *bool { return &b }

// waitForCondition polls cond until it's true or timeout elapses, failing
// the test otherwise. Used instead of a fixed sleep so these tests aren't
// flaky under load, while still not depending on any production timing
// (noopSleep makes the reconnect path itself instant; this just waits for
// the supervisor goroutine to get scheduled and do its work).
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
