package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/provider"
)

func localConfig() *config.Config {
	return &config.Config{Host: "127.0.0.1", Port: -1, Providers: []config.ProviderConfig{{ID: "local", Kind: "openai-compat", BaseURL: "http://127.0.0.1:1", KeyOptional: true}}, Combos: []config.ComboConfig{{ID: "main", Strategy: "priority", Providers: []string{"local"}}}, DefaultCombo: "main"}
}

func TestRunOAuthWithoutStoreAndRouterFailure(t *testing.T) {
	cfg := localConfig()
	cfg.Providers[0].AuthMode = "oauth"
	cfg.Providers[0].APIKeyEnv = "token"
	if err := Run(context.Background(), cfg, "", nil, nil); err == nil || !strings.Contains(err.Error(), "requires credentials") {
		t.Fatalf("err=%v", err)
	}
	cfg = localConfig()
	cfg.Combos[0].Strategy = "not-real"
	if err := Run(context.Background(), cfg, "", discardLogger(), nil); err == nil || !strings.Contains(err.Error(), "building router") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunReturnsListenErrorAndAdapterUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := localConfig()
	err := Run(ctx, cfg, "", discardLogger(), nil)
	cancel()
	if err == nil {
		t.Fatal("invalid negative port should fail ListenAndServe")
	}
	if oauthRefreshAdapter("unknown-provider") != nil {
		t.Fatal("unknown provider should not invent refresh adapter")
	}
	refresh := oauthRefreshAdapter("openai-chatgpt")
	if refresh == nil {
		t.Fatal("OpenAI OAuth must expose refresh adapter")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := refresh(canceled, "refresh-token"); err == nil {
		t.Fatal("canceled refresh should propagate an error")
	}
}

func TestSanitizeWithNoSurvivingDefaultOrCombos(t *testing.T) {
	cfg := &config.Config{Combos: []config.ComboConfig{{ID: "gone", Providers: []string{"missing"}}}, DefaultCombo: "gone"}
	got := sanitizeCombosForBuiltProviders(cfg, map[string]provider.Provider{}, discardLogger())
	if len(got.Combos) != 0 || got.DefaultCombo != "gone" {
		t.Fatalf("sanitized=%#v", got)
	}
}
