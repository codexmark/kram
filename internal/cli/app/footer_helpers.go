package app

import "fmt"

// lastAssistantTokens formats the token usage of the most recent request,
// or "" if nothing has been sent yet.
func (m Model) lastAssistantTokens() string {
	if m.lastUsage.TotalTokens == 0 {
		return ""
	}
	return fmt.Sprintf("%d tok", m.lastUsage.TotalTokens)
}
