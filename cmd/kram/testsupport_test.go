package main

import (
	"testing"

	"github.com/codexmark/kram/internal/providercatalog"
)

// isolateReconcileTest points the config/stores at a fresh temp dir and
// clears every catalog provider's env var. This machine's real shell
// environment may have real keys exported (e.g. a developer's own
// OPENROUTER_API_KEY), and the gateway-config autodetection reads
// os.Getenv directly, so without this a test asserting an exact provider
// count would be at the mercy of whatever happens to be exported outside
// the test. (A sibling copy lives in internal/gatewayconfig's own tests —
// the two packages exercise the same env-dependent logic from different
// layers and can't share an unexported test helper.)
func isolateReconcileTest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, p := range providercatalog.Providers {
		t.Setenv(p.EnvVar, "")
	}
}
