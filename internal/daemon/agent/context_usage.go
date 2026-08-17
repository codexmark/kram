package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/codexmark/kram-gateway/internal/daemon/compaction"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

// ContextCategory is one real, distinct contributor to a session's context
// window usage — not a placeholder. Kram doesn't have MCP tools or a
// skills system yet, so this only reports what actually gets sent to the
// model: conversation content, tool definitions, and any compaction
// summary standing in for older history.
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

	toolTokens := estimateToolDefinitionTokens(s.tools.Definitions())

	used := summaryTokens + messageTokens + toolTokens
	free := s.cfg.MaxContextTokens - used
	if free < 0 {
		free = 0
	}

	categories := []ContextCategory{
		{Name: "messages", Tokens: messageTokens},
		{Name: "tool_definitions", Tokens: toolTokens},
	}
	if summaryTokens > 0 {
		categories = append(categories, ContextCategory{Name: "compaction_summary", Tokens: summaryTokens})
	}
	categories = append(categories, ContextCategory{Name: "free", Tokens: free})

	return ContextUsage{Budget: s.cfg.MaxContextTokens, Used: used, Free: free, Categories: categories}, nil
}

func estimateToolDefinitionTokens(defs any) int {
	b, err := json.Marshal(defs)
	if err != nil {
		return 0
	}
	return len(b) / 4
}
