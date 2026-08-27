package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
)

func TestBuildStaticAuthByKind(t *testing.T) {
	t.Setenv("FACTORY_TEST_KEY", "sk-test")

	cases := []struct {
		kind    string
		baseURL string
		wantErr bool
	}{
		{kind: "anthropic", wantErr: false},
		{kind: "gemini", wantErr: false},
		{kind: "openai-compat", baseURL: "http://127.0.0.1:9099", wantErr: false},
		{kind: "openai-compat", baseURL: "", wantErr: true}, // base_url required
		{kind: "unknown-kind", wantErr: true},
	}

	for _, tc := range cases {
		cfg := config.ProviderConfig{ID: "p", Kind: tc.kind, BaseURL: tc.baseURL, APIKeyEnv: "FACTORY_TEST_KEY"}
		p, err := Build(cfg, nil, 0)
		if tc.wantErr {
			if err == nil {
				t.Errorf("kind=%q base_url=%q: expected an error, got provider %+v", tc.kind, tc.baseURL, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("kind=%q: unexpected error: %v", tc.kind, err)
			continue
		}
		if p.Kind() != tc.kind {
			t.Errorf("built provider Kind() = %q, want %q", p.Kind(), tc.kind)
		}
		if p.ID() != "p" {
			t.Errorf("built provider ID() = %q, want %q", p.ID(), "p")
		}
	}
}

func TestBuildPropagatesAPIKeyError(t *testing.T) {
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", APIKeyEnv: "FACTORY_TEST_KEY_UNSET_XYZ"}
	_, err := Build(cfg, nil, 0)
	if err == nil {
		t.Fatal("expected an error for a required, unset API key env var")
	}
}

func TestBuildPropagatesCapabilities(t *testing.T) {
	t.Setenv("FACTORY_TEST_KEY", "sk-test")
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", APIKeyEnv: "FACTORY_TEST_KEY", SupportsImages: true, SupportsTools: true}
	p, err := Build(cfg, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !p.SupportsImages() || !p.SupportsTools() {
		t.Errorf("SupportsImages()=%v SupportsTools()=%v, want both true", p.SupportsImages(), p.SupportsTools())
	}
}

// TestBuildThreadsTemperatureForOpenAICompat confirms
// config.ProviderConfig.Temperature actually reaches the built
// provider's outgoing requests — Build is the one real construction
// site (internal/gateway wires providers via it), so this is the
// end-to-end proof the config field does something, not just that it
// parses.
func TestBuildThreadsTemperatureForOpenAICompat(t *testing.T) {
	var received openai.ChatCompletionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("FACTORY_TEST_KEY", "sk-test")

	pinned := 0.2
	cfg := config.ProviderConfig{ID: "p", Kind: "openai-compat", BaseURL: srv.URL, APIKeyEnv: "FACTORY_TEST_KEY", Temperature: &pinned}
	p, err := Build(cfg, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if received.Temperature == nil || *received.Temperature != 0.2 {
		t.Fatalf("request Temperature = %v, want a pointer to 0.2", received.Temperature)
	}
}

// TestBuildAppliesTimeout confirms the resolved provider timeout actually
// reaches the built adapter's phase watchdog — Build is the one real
// construction site, so this is the end-to-end proof the tunable does
// something. A zero timeout keeps the adapter's DefaultTimeout. The HTTP
// client itself must stay uncapped: a whole-call client timeout is what
// used to kill long streaming generations mid-answer (see timeout.go).
func TestBuildAppliesTimeout(t *testing.T) {
	t.Setenv("FACTORY_TEST_KEY", "sk-test")
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", APIKeyEnv: "FACTORY_TEST_KEY"}

	p, err := Build(cfg, nil, 42*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*Anthropic).timeout; got != 42*time.Second {
		t.Errorf("built adapter timeout = %v, want 42s", got)
	}
	if got := p.(*Anthropic).client.Timeout; got != 0 {
		t.Errorf("http client must have no whole-call timeout (kills long streams), got %v", got)
	}

	def, err := Build(cfg, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := def.(*Anthropic).timeout; got != DefaultTimeout {
		t.Errorf("adapter timeout with 0 override = %v, want DefaultTimeout %v", got, DefaultTimeout)
	}
}

func TestBuildOAuthRequiresResolver(t *testing.T) {
	cfg := config.ProviderConfig{ID: "p", Kind: "openai-responses", AuthMode: "oauth"}
	_, err := Build(cfg, nil, 0)
	if err == nil {
		t.Fatal("expected an error when auth_mode is oauth but resolve is nil")
	}
}

func TestBuildOAuthOnlySupportsOpenAIResponses(t *testing.T) {
	resolve := func(context.Context) (string, error) { return "tok", nil }
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", AuthMode: "oauth"}
	_, err := Build(cfg, resolve, 0)
	if err == nil {
		t.Fatal("expected an error: anthropic has no oauth-based adapter")
	}

	cfg2 := config.ProviderConfig{ID: "p", Kind: "openai-responses", AuthMode: "oauth"}
	p, err := Build(cfg2, resolve, 0)
	if err != nil {
		t.Fatalf("openai-responses with a resolver should build cleanly: %v", err)
	}
	if p.Kind() != "openai-responses" {
		t.Errorf("Kind() = %q, want openai-responses", p.Kind())
	}
}
