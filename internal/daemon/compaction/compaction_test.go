package compaction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/openai"
)

func TestEstimateTokensCountsContentAndToolCallChars(t *testing.T) {
	msgs := []store.Message{
		{Content: strings.Repeat("a", 40)}, // 40 chars
		{ToolCalls: []openai.ToolCall{{Function: openai.ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}}}}, // "bash" (4) + 12 = 16 chars
	}
	got := EstimateTokens(msgs)
	want := (40 + 16) / charsPerTokenEstimate
	if got != want {
		t.Errorf("EstimateTokens = %d, want %d", got, want)
	}
}

func TestEffectiveHistoryReturnsAllWhenNoMarker(t *testing.T) {
	all := []store.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
	got := EffectiveHistory(all)
	if len(got) != 2 {
		t.Errorf("EffectiveHistory = %+v, want the full unmarked history", got)
	}
}

func TestEffectiveHistoryStartsAtLastMarker(t *testing.T) {
	all := []store.Message{
		{Role: "user", Content: "first"},
		{Role: "system", Name: CompactionMarkerName, Content: "old summary"},
		{Role: "user", Content: "second"},
		{Role: "system", Name: CompactionMarkerName, Content: "new summary"},
		{Role: "user", Content: "third"},
	}
	got := EffectiveHistory(all)
	if len(got) != 2 || got[0].Content != "new summary" || got[1].Content != "third" {
		t.Errorf("EffectiveHistory = %+v, want [new summary, third] (from the last marker onward)", got)
	}
}

func TestEffectiveHistoryIgnoresSystemMessagesWithDifferentName(t *testing.T) {
	all := []store.Message{
		{Role: "system", Name: "some-other-system-message", Content: "not a compaction marker"},
		{Role: "user", Content: "hi"},
	}
	got := EffectiveHistory(all)
	if len(got) != 2 {
		t.Errorf("EffectiveHistory = %+v, want both messages (no real marker present)", got)
	}
}

func TestNeedsCompactionComparesAgainstBudget(t *testing.T) {
	small := []store.Message{{Content: "hi"}}
	if NeedsCompaction(small, 1000) {
		t.Error("a tiny history should not need compaction against a 1000-token budget")
	}

	big := []store.Message{{Content: strings.Repeat("a", 10000)}}
	if !NeedsCompaction(big, 100) {
		t.Error("a 10000-char history should need compaction against a 100-token budget")
	}
}

func TestNeedsCompactionUsesDefaultWhenBudgetIsZeroOrNegative(t *testing.T) {
	msgs := []store.Message{{Content: strings.Repeat("a", (DefaultMaxTokens+1)*charsPerTokenEstimate)}}
	if !NeedsCompaction(msgs, 0) {
		t.Error("maxTokens=0 should fall back to DefaultMaxTokens, and this history exceeds it")
	}
	if !NeedsCompaction(msgs, -5) {
		t.Error("a negative maxTokens should also fall back to DefaultMaxTokens")
	}
}

func TestPruneForModelLeavesShortHistoryUntouched(t *testing.T) {
	msgs := make([]store.Message, protectTailMessages)
	for i := range msgs {
		msgs[i] = store.Message{Role: "tool", Content: strings.Repeat("x", pruneContentThresholdChars+1)}
	}
	got := PruneForModel(msgs)
	for i, m := range got {
		if strings.Contains(m.Content, "pruned") {
			t.Errorf("message %d was pruned, but the whole history is within the protected tail: %+v", i, m)
		}
	}
}

func TestPruneForModelReplacesLargeOldToolResults(t *testing.T) {
	old := store.Message{Role: "tool", Content: strings.Repeat("x", pruneContentThresholdChars+1)}
	tail := make([]store.Message, protectTailMessages)
	for i := range tail {
		tail[i] = store.Message{Role: "user", Content: "recent"}
	}
	msgs := append([]store.Message{old}, tail...)

	got := PruneForModel(msgs)
	if !strings.Contains(got[0].Content, "pruned") {
		t.Errorf("old large tool result should have been pruned, got: %q", got[0].Content)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Content != "recent" {
			t.Errorf("protected tail message %d was altered: %+v", i, got[i])
		}
	}
}

func TestPruneForModelNeverTouchesTheStore(t *testing.T) {
	old := store.Message{Role: "tool", Content: strings.Repeat("x", pruneContentThresholdChars+1)}
	tail := make([]store.Message, protectTailMessages)
	msgs := append([]store.Message{old}, tail...)
	originalContent := old.Content

	PruneForModel(msgs)

	if msgs[0].Content != originalContent {
		t.Error("PruneForModel mutated the caller's slice in place, but it must only affect the returned copy")
	}
}

func TestPruneForModelLeavesSmallToolResultsAlone(t *testing.T) {
	small := store.Message{Role: "tool", Content: "short result"}
	tail := make([]store.Message, protectTailMessages)
	msgs := append([]store.Message{small}, tail...)

	got := PruneForModel(msgs)
	if got[0].Content != "short result" {
		t.Errorf("a small tool result should never be pruned, got %q", got[0].Content)
	}
}

func TestPruneForModelOnlyPrunesToolMessages(t *testing.T) {
	bigUserMsg := store.Message{Role: "user", Content: strings.Repeat("x", pruneContentThresholdChars+1)}
	tail := make([]store.Message, protectTailMessages)
	msgs := append([]store.Message{bigUserMsg}, tail...)

	got := PruneForModel(msgs)
	if strings.Contains(got[0].Content, "pruned") {
		t.Error("a large user message should never be pruned, only tool results")
	}
}

func TestCompactSummarizesAndWrapsAsReferenceOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
			t.Errorf("expected [system prompt, transcript user turn], got %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "Goal: ship the feature."}}},
		})
	}))
	defer srv.Close()

	gw := gatewayclient.New(srv.URL)
	msg, err := Compact(context.Background(), gw, "default", []store.Message{
		{Role: "user", Content: "please do X"},
		{Role: "assistant", Content: "done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != "system" || msg.Name != CompactionMarkerName {
		t.Errorf("compaction message = %+v, want Role=system Name=%s", msg, CompactionMarkerName)
	}
	if !strings.Contains(msg.Content, "reference only") {
		t.Error("compaction summary must be wrapped as reference-only, not a new instruction")
	}
	if !strings.Contains(msg.Content, "Goal: ship the feature.") {
		t.Errorf("compaction summary should include the model's actual summary text, got %q", msg.Content)
	}
}

// TestCompactFoldsPriorSummaryForwardButSkipsOtherSystemMessages pins the
// chained-compaction fix: a prior compaction marker must be carried into
// the new summary's input (or a second compaction permanently drops the
// session's earliest arc), while every OTHER system message in effective
// history — the ephemeral project-context/memory re-injection markers —
// stays excluded, since those are rebuilt fresh each turn.
func TestCompactFoldsPriorSummaryForwardButSkipsOtherSystemMessages(t *testing.T) {
	var gotTranscript string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotTranscript = req.Messages[len(req.Messages)-1].Content
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "summary"}}},
		})
	}))
	defer srv.Close()

	gw := gatewayclient.New(srv.URL)
	_, err := Compact(context.Background(), gw, "default", []store.Message{
		{Role: "system", Name: CompactionMarkerName, Content: "EARLIER ARC: shipped feature X"},
		{Role: "system", Name: "kram:project_context", Content: "PROJECT CONTEXT reinjection marker"},
		{Role: "user", Content: "actual turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotTranscript, "EARLIER ARC: shipped feature X") {
		t.Errorf("a prior compaction summary must be folded into the new summary's input, got %q", gotTranscript)
	}
	if strings.Contains(gotTranscript, "PROJECT CONTEXT reinjection marker") {
		t.Error("non-compaction system markers (project-context/memory) should still be excluded from the transcript")
	}
	if !strings.Contains(gotTranscript, "actual turn") {
		t.Errorf("transcript should include the real conversation turn, got %q", gotTranscript)
	}
}

func TestCompactPropagatesGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	gw := gatewayclient.New(srv.URL)
	_, err := Compact(context.Background(), gw, "default", []store.Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Error("expected Compact to propagate a gateway failure")
	}
}
