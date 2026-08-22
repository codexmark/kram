package agent

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/codexmark/kram/internal/daemon/session"
	"github.com/codexmark/kram/internal/openai"
)

var (
	textToolBlockRE = regexp.MustCompile(`(?s)<tool_call>\s*<function=([A-Za-z_][A-Za-z0-9_-]*)>\s*(.*?)\s*</function>\s*</tool_call>`)
	textToolParamRE = regexp.MustCompile(`(?s)<parameter=([A-Za-z_][A-Za-z0-9_-]*)>\s*(.*?)\s*</parameter>`)
)

// recoverTextToolCalls accepts only responses made entirely of one or more
// tool-call blocks and only names currently offered to the provider. This is
// intentionally narrower than XML parsing: it repairs a known compatibility
// defect without turning arbitrary assistant prose into executable commands.
func recoverTextToolCalls(content string, definitions []openai.Tool) ([]openai.ToolCall, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || len(definitions) == 0 {
		return nil, false
	}
	allowed := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		allowed[definition.Function.Name] = true
	}

	matches := textToolBlockRE.FindAllStringSubmatchIndex(trimmed, -1)
	if len(matches) == 0 {
		return nil, false
	}
	position := 0
	calls := make([]openai.ToolCall, 0, len(matches))
	for _, match := range matches {
		if strings.TrimSpace(trimmed[position:match[0]]) != "" {
			return nil, false
		}
		position = match[1]
		name := trimmed[match[2]:match[3]]
		if !allowed[name] {
			return nil, false
		}
		body := trimmed[match[4]:match[5]]
		arguments, ok := parseTextToolArguments(body)
		if !ok {
			return nil, false
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, false
		}
		calls = append(calls, openai.ToolCall{
			ID:   "text_tool_" + strings.TrimPrefix(session.NewID(), "ses_"),
			Type: "function",
			Function: openai.ToolCallFunction{
				Name: name, Arguments: string(encoded),
			},
		})
	}
	if strings.TrimSpace(trimmed[position:]) != "" {
		return nil, false
	}
	return calls, true
}

func parseTextToolArguments(body string) (map[string]any, bool) {
	arguments := map[string]any{}
	matches := textToolParamRE.FindAllStringSubmatchIndex(body, -1)
	position := 0
	for _, match := range matches {
		if strings.TrimSpace(body[position:match[0]]) != "" {
			return nil, false
		}
		position = match[1]
		name := body[match[2]:match[3]]
		if _, duplicate := arguments[name]; duplicate {
			return nil, false
		}
		arguments[name] = textToolValue(strings.TrimSpace(body[match[4]:match[5]]))
	}
	if strings.TrimSpace(body[position:]) != "" {
		return nil, false
	}
	return arguments, true
}

func textToolValue(raw string) any {
	if raw == "" {
		return ""
	}
	var value any
	if json.Valid([]byte(raw)) && json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return raw
}
