package provider

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoWholeCallTimeoutInAdapters is the static guard against the #106
// regression: a whole-call http.Client.Timeout in a streaming adapter
// kills any legitimately long generation mid-answer ("stream read:
// context deadline exceeded ... while reading body"). Liveness is the
// phase watchdog's job (timeout.go); the client itself must stay
// uncapped. This scans the package's sources so reintroducing the
// pattern fails here, immediately, instead of in production minutes into
// a real generation.
func TestNoWholeCallTimeoutInAdapters(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "http.Client{Timeout") {
			t.Errorf("%s constructs an http.Client with a whole-call Timeout — that caps reading the streamed body and kills long generations (see timeout.go); bound phases with newStreamWatchdog instead", name)
		}
	}
}

// TestAdaptersConstructUncappedClients is the runtime half of the same
// guard, checked against the real constructors rather than source text.
func TestAdaptersConstructUncappedClients(t *testing.T) {
	clients := map[string]*http.Client{
		"anthropic":        NewAnthropic("a", "", "k", "", capabilities{}).client,
		"gemini":           NewGemini("g", "", "k", "", capabilities{}).client,
		"openai-compat":    NewOpenAICompatible("o", "http://x", "k", "", nil, capabilities{}).client,
		"openai-responses": NewOpenAIResponses("r", "", func(ctx context.Context) (string, error) { return "t", nil }, "", capabilities{}).client,
	}
	for kind, c := range clients {
		if c.Timeout != 0 {
			t.Errorf("%s adapter's http.Client has whole-call Timeout %v, want 0 (phase watchdog owns liveness)", kind, c.Timeout)
		}
	}
}
