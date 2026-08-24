package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/codexmark/kram/internal/daemon/compaction"
	"github.com/codexmark/kram/internal/daemon/contextpolicy"
	"github.com/codexmark/kram/internal/daemon/store"
)

// ContextCategory is one real, distinct contributor to a session's context
// window usage — not a placeholder. MCP and skill discovery are exposed
// through registered tool definitions, so their cost is included in the
// tool category alongside conversation content and compaction summaries.
type ContextCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// ContextUsage is a session's current context-window breakdown, estimated
// the same way internal/daemon/compaction decides when to compact —
// same chars/4 approximation, so this panel and the compaction trigger
// never disagree with each other.
type ContextUsage struct {
	Budget     int               `json:"budget"`
	Used       int               `json:"used"`
	Free       int               `json:"free"`
	Categories []ContextCategory `json:"categories"`
}

// ContextUsage reports what's actually consuming this session's context
// budget right now.
func (s *Service) ContextUsage(ctx context.Context, sessionID string) (ContextUsage, error) {
	if _, err := s.store.GetSession(sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ContextUsage{}, ErrNotFound
		}
		return ContextUsage{}, err
	}

	all, err := s.store.ListMessages(sessionID)
	if err != nil {
		return ContextUsage{}, fmt.Errorf("loading history: %w", err)
	}
	effective := compaction.EffectiveHistory(all)

	var summaryTokens, messageTokens int
	for i, m := range effective {
		tok := compaction.EstimateTokens([]store.Message{m})
		if i == 0 && m.Role == "system" && m.Name == compaction.CompactionMarkerName {
			summaryTokens += tok
		} else {
			messageTokens += tok
		}
	}

	memoryMsg, haveMemory := s.recentMemoryMessage()
	projectContext, haveProjectContext := loadProjectContext(s.cfg.Workspace)
	parts := compilePreamble(s.cfg.Workspace, projectContext, haveProjectContext, memoryMsg, haveMemory, s.tools, s.cfg.ToolOrder, s.cfg.SystemPromptOverride)
	toolTokens := estimateToolDefinitionTokens(s.tools.Definitions())
	partTokens := make(map[string]int, len(parts))
	for _, part := range parts {
		partTokens[part.ID] = len(part.Content) / 4
	}
	fixedTokens := estimatePromptPartTokens(parts) + toolTokens
	policy := contextpolicy.New(s.cfg.MaxContextTokens, fixedTokens)
	used := summaryTokens + messageTokens + fixedTokens
	free := policy.RemainingHistoryTokens(summaryTokens + messageTokens)

	categories := []ContextCategory{
		{Name: "messages", Tokens: messageTokens},
		{Name: "system_prompt", Tokens: partTokens["base"]},
		{Name: "tool_overview", Tokens: partTokens["tools-overview"]},
		{Name: "tool_definitions", Tokens: toolTokens},
	}
	if partTokens["background-job-guidance"] > 0 {
		categories = append(categories, ContextCategory{Name: "background_job_guidance", Tokens: partTokens["background-job-guidance"]})
	}
	if partTokens["project-context"] > 0 {
		categories = append(categories, ContextCategory{Name: "project_context", Tokens: partTokens["project-context"]})
	}
	if partTokens["memory"] > 0 {
		categories = append(categories, ContextCategory{Name: "memory", Tokens: partTokens["memory"]})
	}
	if summaryTokens > 0 {
		categories = append(categories, ContextCategory{Name: "compaction_summary", Tokens: summaryTokens})
	}
	categories = append(categories, ContextCategory{Name: "response_reserve", Tokens: policy.ResponseReserveTokens})
	categories = append(categories, ContextCategory{Name: "free", Tokens: free})

	return ContextUsage{Budget: s.cfg.MaxContextTokens, Used: used, Free: free, Categories: categories}, nil
}
