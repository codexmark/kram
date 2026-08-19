package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

// TestAnthropicEmitsToolCallProgress is the regression test for the
// confirmed liveness gap: Anthropic's content_block_delta events for a
// tool_use block accumulate partial_json fragments internally with no
// matching StreamEvent, leaving router.BoundedPeek with zero visibility
// into a provider actively streaming a tool call's arguments.
func TestAnthropicEmitsToolCallProgress(t *testing.T) {
	lines := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"list_dir"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
	}))
	defer srv.Close()

	p := NewAnthropic("test", srv.URL, "key", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)

	progressEvents := 0
	for _, e := range got {
		if e.ToolCallProgress {
			progressEvents++
		}
	}
	if progressEvents != 1 {
		t.Errorf("expected 1 ToolCallProgress event (one input_json_delta fragment), got %d: %+v", progressEvents, got)
	}
}
