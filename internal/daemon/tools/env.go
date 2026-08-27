package tools

import (
	"os"
	"strings"

	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/providercatalog"
)

// credentialEnvNames is every environment-variable name Kram itself
// populates with a provider credential — the catalog accounts' env vars
// (cmd/kram's loadStoredCredentials os.Setenv's these at startup) plus
// each registered custom provider's synthesized env var. These must never
// be visible to a model-driven shell command: without this, a prompt-
// injected model could exfiltrate keys with a plain `env` /
// `echo $OPENROUTER_API_KEY`. Built from the same catalogs the gateway
// resolves credentials from, so it stays in sync automatically instead of
// hardcoding a list that drifts.
func credentialEnvNames() map[string]bool {
	names := make(map[string]bool)
	for _, e := range providercatalog.EnvVars() {
		names[e] = true
	}
	if cs, err := customprovider.Load(); err == nil {
		for _, cp := range cs.All() {
			names[cp.EnvVar] = true
		}
	}
	return names
}

// redactedEnviron is os.Environ() with every Kram-injected provider
// credential removed — the environment a model-driven shell command (bash,
// a custom manifest tool, a run_background process) runs with. Only the
// keys Kram put into the process are stripped; the user's own environment
// (PATH, HOME, their own GITHUB_TOKEN, and anything else they exported) is
// left intact, so legitimate commands that need real env still work. This
// is a denylist of exactly Kram's own secrets, not an allowlist, precisely
// to avoid breaking a command that depends on some unrelated variable.
func redactedEnviron() []string {
	drop := credentialEnvNames()
	if len(drop) == 0 {
		return os.Environ()
	}
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
