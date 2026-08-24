package app

import "fmt"

// lastAssistantTokens formats the token usage of the most recent request,
// or "" if nothing has been sent yet.
func (m Model) lastAssistantTokens() string {
	if m.lastUsage.TotalTokens == 0 {
		return ""
	}
	usage := m.lastUsage
	label := fmt.Sprintf("%d tok", usage.TotalTokens)
	if usage.CachedPromptTokens > 0 && usage.PromptTokens > 0 {
		label += fmt.Sprintf(" · cache %d%%", usage.CachedPromptTokens*100/usage.PromptTokens)
	}
	if usage.ReasoningTokens > 0 {
		label += fmt.Sprintf(" · raciocínio %d", usage.ReasoningTokens)
	}
	if usage.EstimatedCostMicros > 0 {
		label += fmt.Sprintf(" · ≈US$ %.4f", float64(usage.EstimatedCostMicros)/1_000_000)
	}
	return label
}
