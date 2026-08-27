package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/config"
)

// startGateway boots a real gateway over cfg and waits for /health —
// the compressed-timescale fixture the #105-#108 bug class needed: a
// real adapter, real router peek, real watchdogs, against an upstream
// whose slowness is measured in milliseconds instead of minutes.
func startGateway(t *testing.T, cfg *config.Config) (base string, shutdown func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg, "", discardLogger(), nil) }()

	base = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, func() { cancel(); <-errCh }
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("gateway never became healthy")
	return "", nil
}

func slowProviderConfig(t *testing.T, upstream string, providerTimeout time.Duration) *config.Config {
	return &config.Config{
		Host: "127.0.0.1", Port: freePort(t),
		Providers: []config.ProviderConfig{
			{ID: "lab", Kind: "openai-compat", BaseURL: upstream, KeyOptional: true},
		},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"lab"}}},
		DefaultCombo: "default",
		Tunables:     config.Tunables{ProviderTimeout: config.Duration(providerTimeout)},
	}
}

const chatReq = `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestSlowThinkingProviderSurvivesEndToEnd: an upstream that stays
// completely silent past the short peek default before its first token —
// the compressed shape of a reasoning model thinking — must still serve
// the answer through a real gateway (last-candidate peek budget + phase
// watchdog + streaming path all engaged).
func TestSlowThinkingProviderSurvivesEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond) // silent thinking, longer than any per-event gap budget below
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	base, shutdown := startGateway(t, slowProviderConfig(t, upstream.URL, 600*time.Millisecond))
	defer shutdown()

	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(chatReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	if !strings.Contains(out, `"content":"ok"`) || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("slow-thinking upstream did not survive end to end:\n%s", out)
	}
}

// TestDeadUpstreamStillDiesLabeled: the same fixture proves the guard
// didn't just remove protection — an upstream that goes quiet forever
// after committing dies at the configured threshold, labeled as an idle
// timeout in the terminal error chunk.
func TestDeadUpstreamStillDiesLabeled(t *testing.T) {
	blocked := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"start\"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		<-blocked // silent forever
	}))
	defer upstream.Close()
	defer close(blocked)

	base, shutdown := startGateway(t, slowProviderConfig(t, upstream.URL, 120*time.Millisecond))
	defer shutdown()

	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(chatReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	if !strings.Contains(out, `"finish_reason":"error"`) || !strings.Contains(out, "idle timeout") {
		t.Fatalf("dead upstream should die labeled as an idle timeout:\n%s", out)
	}
}
