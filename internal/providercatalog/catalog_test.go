package providercatalog

import "testing"

func TestEveryProviderHasARequiredEnvVar(t *testing.T) {
	for _, p := range Providers {
		if p.EnvVar == "" {
			t.Errorf("provider %q has no EnvVar", p.ID)
		}
		if p.ID == "" {
			t.Errorf("a provider with EnvVar %q has no ID", p.EnvVar)
		}
	}
}

func TestEveryAccountHasAMatchingProviderEnvVar(t *testing.T) {
	// The accounts screen's dedup only makes sense if every Account's
	// EnvVar actually appears somewhere in Providers — otherwise a key
	// entered there would be saved but never read by anything.
	providerEnvVars := map[string]bool{}
	for _, p := range Providers {
		providerEnvVars[p.EnvVar] = true
	}
	for _, a := range Accounts {
		if !providerEnvVars[a.EnvVar] {
			t.Errorf("account %q references env var %q, which no catalog Provider reads", a.ID, a.EnvVar)
		}
	}
}

func TestEnvVarsIsDeduplicated(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range EnvVars() {
		if seen[v] {
			t.Errorf("EnvVars() returned %q more than once", v)
		}
		seen[v] = true
	}
	// OpenRouter's three free-model entries share one env var — this is
	// the case that would silently break if EnvVars() stopped deduping.
	if len(seen) >= len(Providers) {
		t.Error("expected fewer distinct env vars than providers, since some providers share a key")
	}
}

func TestSupportsOAuthHasARealFlow(t *testing.T) {
	// Every account claiming OAuth support must have a real Authorize
	// function in internal/oauthflow — this only guards against an
	// account being flagged SupportsOAuth before its flow exists (a
	// catalog/oauthflow drift), not against the flow actually working
	// against the real provider.
	hasFlow := map[string]bool{
		"anthropic":      true, // oauthflow.AnthropicAuthorize
		"openai-chatgpt": true, // oauthflow.OpenAIAuthorize
		"openrouter":     true, // oauthflow.OpenRouterAuthorize
	}
	for _, a := range Accounts {
		if a.SupportsOAuth && !hasFlow[a.ID] {
			t.Errorf("account %q claims OAuth support, but has no known flow in internal/oauthflow", a.ID)
		}
	}
}

func TestOAuthOnlyAccountsSupportOAuth(t *testing.T) {
	// OAuthOnly without SupportsOAuth would hide the only way to connect
	// the account at all.
	for _, a := range Accounts {
		if a.OAuthOnly && !a.SupportsOAuth {
			t.Errorf("account %q is OAuthOnly but doesn't SupportsOAuth — it would have no way to connect", a.ID)
		}
	}
}
