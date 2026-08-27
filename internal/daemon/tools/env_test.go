package tools

import (
	"os"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/providercatalog"
)

// TestRedactedEnvironStripsProviderCredentialsButKeepsUserEnv is the
// regression test for the env-leak fix: a model-driven shell must not see
// Kram's own provider API keys (a prompt-injected model could `env` them
// out), but must keep the user's own environment so legitimate commands
// still work.
func TestRedactedEnvironStripsProviderCredentials(t *testing.T) {
	credVar := providercatalog.EnvVars()[0] // e.g. ANTHROPIC_API_KEY
	t.Setenv(credVar, "sk-secret-should-not-leak")
	t.Setenv("KRAM_TEST_USER_VAR", "keep-me")

	env := redactedEnviron()

	for _, kv := range env {
		if strings.HasPrefix(kv, credVar+"=") {
			t.Fatalf("redactedEnviron leaked the provider credential %s", credVar)
		}
	}
	// The user's own env — and PATH — must survive.
	foundUser, foundPath := false, false
	for _, kv := range env {
		if kv == "KRAM_TEST_USER_VAR=keep-me" {
			foundUser = true
		}
		if strings.HasPrefix(kv, "PATH=") {
			foundPath = true
		}
	}
	if !foundUser {
		t.Error("redactedEnviron dropped a user-set variable it should keep")
	}
	if !foundPath && os.Getenv("PATH") != "" {
		t.Error("redactedEnviron dropped PATH, which legitimate commands need")
	}
}
