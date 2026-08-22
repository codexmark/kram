package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func TestRunContinuesAcrossAutomaticSegments(t *testing.T) {
	workspace := t.TempDir()
	processList := openai.ToolCall{
		ID: "call_process_list", Type: "function",
		Function: openai.ToolCallFunction{Name: "process_list", Arguments: `{}`},
	}
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{processList}},
		{toolCalls: []openai.ToolCall{processList}},
		{content: "finished after the boundary"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{
		Workspace: workspace, MaxTurns: 2, MaxSegmentsPerRun: 2,
	})
	newTestSession(t, s, "segmented-session")
	var segments []int

	result, err := s.Run(context.Background(), "segmented-session", "keep working", nil, func(event Event) {
		if event.Kind == EventSegment {
			segments = append(segments, event.Segment)
			if event.Segments != 2 {
				t.Errorf("segment total = %d, want 2", event.Segments)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Content != "finished after the boundary" || len(requests()) != 3 {
		t.Fatalf("result=%+v requests=%d", result, len(requests()))
	}
	if len(result.ToolActivity) != 2 {
		t.Fatalf("tool activity = %+v", result.ToolActivity)
	}
	if len(segments) != 2 || segments[0] != 1 || segments[1] != 2 {
		t.Fatalf("segment events = %v, want [1 2]", segments)
	}
}

func TestRunStopsAfterRepeatedIdenticalToolFailure(t *testing.T) {
	workspace := t.TempDir()
	missingRead := openai.ToolCall{
		ID: "call_read", Type: "function",
		Function: openai.ToolCallFunction{Name: "read_file", Arguments: `{"path":"missing.txt"}`},
	}
	script := []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{missingRead}},
		{toolCalls: []openai.ToolCall{missingRead}},
		{toolCalls: []openai.ToolCall{missingRead}},
		{toolCalls: []openai.ToolCall{missingRead}},
	}
	srv, requests := fakeGateway(t, script)
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{
		Workspace: workspace, MaxTurns: 10, MaxSegmentsPerRun: 1,
	})
	newTestSession(t, s, "stagnant-session")

	result, err := s.Run(context.Background(), "stagnant-session", "read it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests()) != 4 || len(result.ToolActivity) != 4 {
		t.Fatalf("requests=%d activity=%d", len(requests()), len(result.ToolActivity))
	}
	if !strings.Contains(result.Message.Content, "blocked:") || !strings.Contains(result.Message.Content, "read_file") {
		t.Fatalf("unexpected stagnation result: %q", result.Message.Content)
	}
	thirdResultReachedModel := false
	for _, message := range requests()[3] {
		if message.Role == "tool" && strings.Contains(message.Content, "stagnation guard") {
			thirdResultReachedModel = true
		}
	}
	if !thirdResultReachedModel {
		t.Fatal("third identical failure did not instruct the model to change strategy")
	}
}

func TestStagnationAllowsBackgroundPollingWhileStillRunning(t *testing.T) {
	var guard toolStagnation
	activity := ToolActivity{
		Name: "process_output", Args: `{"id":"bg1"}`,
		Result: "[still running] (no output yet)", OK: true,
	}
	for i := 0; i < 10; i++ {
		if got := guard.observe(activity); got != 0 {
			t.Fatalf("poll %d counted as stagnation: %d", i+1, got)
		}
	}
}
