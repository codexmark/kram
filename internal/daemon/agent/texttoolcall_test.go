package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func textToolDefinitions(names ...string) []openai.Tool {
	definitions := make([]openai.Tool, len(names))
	for i, name := range names {
		definitions[i] = openai.Tool{Type: "function", Function: openai.ToolFunction{Name: name}}
	}
	return definitions
}

func TestRecoverTextToolCallsObservedProcessOutputMarkup(t *testing.T) {
	content := `<tool_call> <function=process_output> <parameter=id> bg25 </parameter> </function> </tool_call>`
	calls, ok := recoverTextToolCalls(content, textToolDefinitions("process_output"))
	if !ok || len(calls) != 1 {
		t.Fatalf("recovery = %#v, %v", calls, ok)
	}
	if calls[0].Function.Name != "process_output" || calls[0].Type != "function" || calls[0].ID == "" {
		t.Fatalf("call = %+v", calls[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil || args["id"] != "bg25" {
		t.Fatalf("arguments = %q, %#v, %v", calls[0].Function.Arguments, args, err)
	}
}

func TestRecoverTextToolCallsRejectsProseUnknownAndMalformedMarkup(t *testing.T) {
	definitions := textToolDefinitions("process_output")
	bad := []string{
		`I suggest <tool_call> <function=process_output><parameter=id>bg25</parameter></function></tool_call>`,
		`<tool_call><function=bash><parameter=command>rm something</parameter></function></tool_call>`,
		`<tool_call><function=process_output><parameter=id>bg25</function></tool_call>`,
		`<tool_call><function=process_output>surprise<parameter=id>bg25</parameter></function></tool_call>`,
	}
	for _, input := range bad {
		if calls, ok := recoverTextToolCalls(input, definitions); ok {
			t.Errorf("unexpected recovery for %q: %+v", input, calls)
		}
	}
}

func TestRunContinuesAfterTextualToolCall(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{content: `<tool_call> <function=process_output> <parameter=id> bg25 </parameter> </function> </tool_call>`},
		{content: "background check completed"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 4})
	newTestSession(t, s, "text-tool-session")
	var notices []string
	result, err := s.Run(context.Background(), "text-tool-session", "check it", nil, func(event Event) {
		if event.Kind == EventNotice {
			notices = append(notices, event.Notice)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Content != "background check completed" || len(result.ToolActivity) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.ToolActivity[0].Name != "process_output" || !strings.Contains(result.ToolActivity[0].Result, "no background process") {
		t.Fatalf("tool activity = %+v", result.ToolActivity)
	}
	if len(requests()) != 2 || len(notices) != 1 || !strings.Contains(notices[0], "normalized") {
		t.Fatalf("requests=%d notices=%v", len(requests()), notices)
	}
}

func TestRunDoesNotExposeTextualToolCallAtTurnLimit(t *testing.T) {
	workspace := t.TempDir()
	srv, _ := fakeGateway(t, []scriptedChatResponse{
		{content: `<tool_call><function=process_output><parameter=id>bg25</parameter></function></tool_call>`},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 1})
	newTestSession(t, s, "text-tool-limit-session")

	result, err := s.Run(context.Background(), "text-tool-limit-session", "check it", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Message.Content, "<tool_call>") {
		t.Fatalf("raw textual tool markup escaped into the final answer: %q", result.Message.Content)
	}
	if !strings.Contains(result.Message.Content, "turn limit") || !strings.Contains(result.Message.Content, "process_output") {
		t.Fatalf("final answer does not explain the bounded stop: %q", result.Message.Content)
	}
	if len(result.ToolActivity) != 0 {
		t.Fatalf("tool executed past the final-turn budget: %+v", result.ToolActivity)
	}
}
