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
	// Mirror runLoop's own change-detection gate (see needsFreshInjection):
	// messageTokens above already counts a persisted project-context/memory
	// marker's tokens like any other history message, so unconditionally
	// adding a fresh "project-context"/"memory" category here whenever
	// content exists would double-count exactly the tokens this session's
	// next real turn would actually skip resending.
	injectProjectContext := haveProjectContext && needsFreshInjection(effective, projectContextMarkerName, formatProjectContextContent(projectContext))
	injectMemory := haveMemory && needsFreshInjection(effective, memoryMarkerName, memoryMsg.Content)
	parts := compilePreamble(s.cfg.Workspace, projectContext, injectProjectContext, memoryMsg, injectMemory, s.tools, s.cfg.ToolOrder, s.cfg.SystemPromptOverride, collectEnvContext(ctx, s.cfg.Workspace, s.activeModel()))
	toolTokens := estimateToolDefinitionTokens(s.tools.Definitions())
	partTokens := make(map[string]int, len(parts))
	for _, part := range parts {
		partTokens[part.ID] = len(part.Content) / 4
	}
	// Apply this session's token-estimate calibration to every chars/4
	// figure, so the panel stays in lockstep with the compaction trigger,
	// which calibrates the same way (see runLoop and calibration.go). The
	// Budget below is the real model window and is deliberately NOT scaled.
	calibration := s.calibrator.factor(sessionID)
	rawFixed := estimatePromptPartTokens(parts) + toolTokens
	summaryTokens = scaleTokens(summaryTokens, calibration)
	messageTokens = scaleTokens(messageTokens, calibration)
	toolTokens = scaleTokens(toolTokens, calibration)
	for id, tok := range partTokens {
		partTokens[id] = scaleTokens(tok, calibration)
	}
	fixedTokens := scaleTokens(rawFixed, calibration)
	policy := contextpolicy.New(s.cfg.MaxContextTokens, fixedTokens)
	used := summaryTokens + messageTokens + fixedTokens
	free := policy.RemainingHistoryTokens(summaryTokens + messageTokens)

	// system_prompt sums every part that isn't one of the other named
	// categories below — either the single "base" part
	// (SystemPromptOverride set) or the Model/Agent Profile phase's nine
	// named base sections (see compileBaseSections), whichever
	// compilePreamble actually produced. Summed rather than keyed by a
	// fixed ID list so this stays correct if a future phase adds more
	// base sections without this file needing a matching update.
	nonBaseCategoryIDs := map[string]bool{
		"tools-overview": true, "background-job-guidance": true,
		"project-context": true, "memory": true, "env-context": true,
	}
	systemPromptTokens := 0
	for id, tok := range partTokens {
		if !nonBaseCategoryIDs[id] {
			systemPromptTokens += tok
		}
	}

	categories := []ContextCategory{
		{Name: "messages", Tokens: messageTokens},
		{Name: "system_prompt", Tokens: systemPromptTokens},
		{Name: "tool_overview", Tokens: partTokens["tools-overview"]},
		{Name: "tool_definitions", Tokens: toolTokens},
	}
	if partTokens["background-job-guidance"] > 0 {
		categories = append(categories, ContextCategory{Name: "background_job_guidance", Tokens: partTokens["background-job-guidance"]})
	}
	if partTokens["env-context"] > 0 {
		categories = append(categories, ContextCategory{Name: "env_context", Tokens: partTokens["env-context"]})
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
