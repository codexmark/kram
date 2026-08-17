// Package agent is Kram's tool-calling loop: the piece that actually
// makes the daemon useful rather than a plain chat relay. Each turn it
// sends the session's history (and tool definitions) to the gateway; if
// the model asks to call tools, they run and their results feed back in,
// looping until the model answers in plain text or the iteration budget
// runs out.
//
// Design choices are grounded in patterns that recur across production
// agent loops (opencode/Crush, Hermes Agent — see the research notes in
// this session): tool execution waits for a complete (non-streaming)
// model response rather than interleaving with token streaming — every
// project that documents this decouples the two; the iteration budget
// has a soft landing (a warning, then one forced "final answer" call)
// rather than a hard cutoff, matching Hermes's approach; and context
// compaction is capped at a handful of attempts per run to guard against
// the "model re-executes its own summary" infinite loop documented in
// both opencode and Crush (see internal/daemon/compaction).
package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/codexmark/kram-gateway/internal/daemon/compaction"
	"github.com/codexmark/kram-gateway/internal/daemon/gatewayclient"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
	"github.com/codexmark/kram-gateway/internal/daemon/tools"
	"github.com/codexmark/kram-gateway/internal/openai"
)

// ErrNotFound is returned when a session ID doesn't exist.
var ErrNotFound = errors.New("session not found")

// ErrContextOverflow is returned when compaction couldn't bring the
// conversation back under budget within MaxCompactionsPerRun attempts —
// a deliberate hard failure instead of an infinite compact/retry loop.
var ErrContextOverflow = errors.New("context kept overflowing after repeated compaction attempts")

// Config tunes the loop's limits. All fields have sane defaults applied
// by New if left zero.
type Config struct {
	// Model selects which gateway combo this session's calls go to.
	Model string
	// MaxTurns bounds how many model calls one Run makes (tool round-trips
	// included) before forcing a stop. Default 50 — deliberately far below
	// Hermes's 500 default, since Kram has no delegation/subagent budget
	// yet to absorb runaway loops the way Hermes's does.
	MaxTurns int
	// MaxCompactionsPerRun caps consecutive compaction attempts within a
	// single Run before giving up with ErrContextOverflow.
	MaxCompactionsPerRun int
	// MaxContextTokens is the effective-history budget before compaction
	// triggers (see internal/daemon/compaction).
	MaxContextTokens int
	// Workspace is the project root — used to load AGENTS.md/CLAUDE.md as
	// persistent project context, injected into every turn.
	Workspace string
}

func (c Config) withDefaults() Config {
	if c.MaxTurns <= 0 {
		c.MaxTurns = 50
	}
	if c.MaxCompactionsPerRun <= 0 {
		c.MaxCompactionsPerRun = 3
	}
	if c.MaxContextTokens <= 0 {
		c.MaxContextTokens = compaction.DefaultMaxTokens
	}
	return c
}

// ToolActivity records one tool call the loop made, for callers (the CLI)
// that want to show what the agent actually did, not just its final answer.
type ToolActivity struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
	OK     bool   `json:"ok"`
}

// RunResult is everything a caller gets back from one user turn — which
// may have involved any number of tool round-trips and compactions under
// the hood.
type RunResult struct {
	Message      store.Message
	ToolActivity []ToolActivity
	Attempts     []openai.AttemptInfo // fallback trail of the final (deciding) gateway call
	Usage        openai.Usage         // summed across every gateway call this turn
	Compactions  int
	ImageNotice  string // set if images were attached but the combo can't accept them
}

const maxToolResultChars = 4000 // how much of a tool result ToolActivity keeps for display

// Service runs the agent loop for a workspace.
type Service struct {
	store   *store.Store
	gateway *gatewayclient.Client
	tools   *tools.Registry
	cfg     Config
}

// New builds an agent Service.
func New(st *store.Store, gw *gatewayclient.Client, tr *tools.Registry, cfg Config) *Service {
	return &Service{store: st, gateway: gw, tools: tr, cfg: cfg.withDefaults()}
}

// Run handles one user message end to end: persist it, run the tool loop
// until the model produces a final text answer (or the budget runs out),
// and return that answer plus everything that happened along the way.
// onEvent (may be nil) receives a live play-by-play — text deltas as the
// model generates them, tool start/result, and notices — as they happen
// rather than only in the returned RunResult once everything is done.
func (s *Service) Run(ctx context.Context, sessionID, userContent string, images []string, onEvent EventFunc) (RunResult, error) {
	if _, err := s.store.GetSession(sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunResult{}, ErrNotFound
		}
		return RunResult{}, err
	}

	imageNotice := ""
	if len(images) > 0 {
		ok, err := s.comboSupportsImages(ctx)
		if err == nil && !ok {
			imageNotice = fmt.Sprintf("images were attached, but no provider in combo %q supports image input — sent as text only", s.cfg.Model)
			images = nil
			emit(onEvent, Event{Kind: EventNotice, Notice: imageNotice})
		}
		// If the status check itself failed, fail open and still attach
		// images rather than silently dropping them over a transient
		// gateway hiccup.
	}

	if _, err := s.store.AppendMessage(sessionID, store.Message{Role: "user", Content: userContent, Images: images}); err != nil {
		return RunResult{}, fmt.Errorf("persisting user message: %w", err)
	}

	result := RunResult{ImageNotice: imageNotice}
	compactions := 0
	graceUsed := false

	for turn := 0; turn < s.cfg.MaxTurns; turn++ {
		all, err := s.store.ListMessages(sessionID)
		if err != nil {
			return RunResult{}, fmt.Errorf("loading history: %w", err)
		}
		effective := compaction.EffectiveHistory(all)

		if compaction.NeedsCompaction(effective, s.cfg.MaxContextTokens) {
			if compactions >= s.cfg.MaxCompactionsPerRun {
				return RunResult{}, ErrContextOverflow
			}
			pruned := compaction.PruneForModel(effective)
			if compaction.NeedsCompaction(pruned, s.cfg.MaxContextTokens) {
				marker, err := compaction.Compact(ctx, s.gateway, s.cfg.Model, pruned)
				if err != nil {
					return RunResult{}, fmt.Errorf("compacting session: %w", err)
				}
				if _, err := s.store.AppendMessage(sessionID, marker); err != nil {
					return RunResult{}, fmt.Errorf("persisting compaction summary: %w", err)
				}
				compactions++
				result.Compactions = compactions
				emit(onEvent, Event{Kind: EventNotice, Notice: "session history was compacted to stay in budget"})
				continue // reload the now-much-shorter effective history before calling the model
			}
			effective = pruned
		}

		modelMessages := toModelMessages(effective)
		if memoryMsg, ok := s.recentMemoryMessage(); ok {
			// Prepended fresh each turn (not persisted into history) so
			// memory written mid-conversation is visible on the very next
			// turn, same as project context below.
			modelMessages = append([]openai.ChatMessage{memoryMsg}, modelMessages...)
		}
		if projectContext, found := loadProjectContext(s.cfg.Workspace); found {
			// Prepended, not persisted: this reflects the file's current
			// contents on every turn, so an edit takes effect on the very
			// next message rather than requiring a daemon restart.
			modelMessages = append([]openai.ChatMessage{
				{Role: "system", Content: "Project context (from AGENTS.md/CLAUDE.md):\n\n" + projectContext},
			}, modelMessages...)
		}

		nearBudget := turn == s.cfg.MaxTurns-1
		toolDefs := s.tools.Definitions()
		if nearBudget {
			// Soft landing (Hermes's pattern): stop offering tools on the
			// final allowed call and ask directly for a wrap-up, rather
			// than hard-cutting mid-tool-loop.
			toolDefs = nil
			modelMessages = append(modelMessages, openai.ChatMessage{
				Role:    "system",
				Content: "You are at your turn limit for this task. Provide your best final answer now in plain text — no further tool calls.",
			})
		}

		callResult, err := s.streamCall(ctx, modelMessages, toolDefs, onEvent)
		if err != nil {
			return RunResult{}, fmt.Errorf("gateway call failed: %w", err)
		}
		result.Attempts = callResult.Attempts
		result.Usage = sumUsage(result.Usage, callResult.Usage)

		if len(callResult.ToolCalls) == 0 {
			assistantMsg, err := s.store.AppendMessage(sessionID, store.Message{
				Role: "assistant", Content: callResult.Content, Provider: callResult.Provider,
			})
			if err != nil {
				return RunResult{}, fmt.Errorf("persisting assistant message: %w", err)
			}
			result.Message = assistantMsg
			return result, nil
		}

		if nearBudget {
			// The model ignored the wrap-up request and tried to call
			// tools anyway with none offered — this shouldn't happen, but
			// if it does, stop rather than looping past the budget.
			if graceUsed {
				assistantMsg, _ := s.store.AppendMessage(sessionID, store.Message{
					Role: "assistant", Content: "(stopped: reached the turn limit for this task)", Provider: callResult.Provider,
				})
				result.Message = assistantMsg
				return result, nil
			}
			graceUsed = true
		}

		if _, err := s.store.AppendMessage(sessionID, store.Message{
			Role: "assistant", Content: callResult.Content, ToolCalls: callResult.ToolCalls, Provider: callResult.Provider,
		}); err != nil {
			return RunResult{}, fmt.Errorf("persisting assistant tool-call message: %w", err)
		}

		for _, tc := range callResult.ToolCalls {
			emit(onEvent, Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
			activity, toolMsg := s.runTool(ctx, tc)
			emit(onEvent, Event{Kind: EventToolResult, ToolName: tc.Function.Name, ToolResult: activity.Result, ToolOK: activity.OK})
			result.ToolActivity = append(result.ToolActivity, activity)
			if _, err := s.store.AppendMessage(sessionID, toolMsg); err != nil {
				return RunResult{}, fmt.Errorf("persisting tool result: %w", err)
			}
		}
		// Loop continues: the tool results just persisted become part of
		// the history the next iteration sends back to the model.
	}

	return RunResult{}, fmt.Errorf("agent loop exhausted %d turns without a final answer", s.cfg.MaxTurns)
}

// streamCall makes one gateway call over the streaming path and
// accumulates it into the same shape the old non-streaming call
// returned, emitting each text fragment via onEvent as it arrives. Every
// turn goes through here now, including tool-calling turns — those just
// happen to never emit a delta, since the model isn't producing visible
// text when it's deciding what to call.
func (s *Service) streamCall(ctx context.Context, messages []openai.ChatMessage, toolDefs []openai.Tool, onEvent EventFunc) (gatewayclient.Result, error) {
	deltas, err := s.gateway.ChatCompletionStream(ctx, s.cfg.Model, messages, toolDefs)
	if err != nil {
		return gatewayclient.Result{}, err
	}

	var content strings.Builder
	var result gatewayclient.Result
	for d := range deltas {
		if d.Err != nil {
			return gatewayclient.Result{}, d.Err
		}
		if d.Content != "" {
			content.WriteString(d.Content)
			emit(onEvent, Event{Kind: EventDelta, Content: d.Content})
		}
		if d.Done {
			result = gatewayclient.Result{
				Content: content.String(), ToolCalls: d.ToolCalls,
				Provider: d.Provider, Attempts: d.Attempts, Usage: d.Usage,
			}
		}
	}
	return result, nil
}

func (s *Service) runTool(ctx context.Context, tc openai.ToolCall) (ToolActivity, store.Message) {
	resultText, err := s.tools.Execute(ctx, tc.Function.Name, []byte(tc.Function.Arguments))
	ok := err == nil
	if err != nil {
		resultText = fmt.Sprintf("error: %v", err)
	}

	display := resultText
	if len(display) > maxToolResultChars {
		display = display[:maxToolResultChars] + "…"
	}

	activity := ToolActivity{Name: tc.Function.Name, Args: tc.Function.Arguments, Result: display, OK: ok}
	toolMsg := store.Message{Role: "tool", Content: resultText, ToolCallID: tc.ID, Name: tc.Function.Name}
	return activity, toolMsg
}

// recentMemoryLimit bounds the automatic injection — small and cheap on
// purpose. Anything older or from a less-relevant angle is still
// reachable through the memory_search tool; this is only the "handoff"
// slice, not the whole memory store.
const recentMemoryLimit = 8

// recentMemoryMessage builds the system message carrying pinned/recent
// cross-session memory (this workspace plus store.GlobalScope), or
// reports ok=false if there's nothing to inject (a fresh workspace, or
// the lookup itself failed — memory is a nice-to-have, never worth
// failing the turn over).
func (s *Service) recentMemoryMessage() (openai.ChatMessage, bool) {
	entries, err := s.store.RecentMemory([]string{s.cfg.Workspace, store.GlobalScope}, recentMemoryLimit)
	if err != nil || len(entries) == 0 {
		return openai.ChatMessage{}, false
	}

	var b strings.Builder
	for _, e := range entries {
		scope := "project"
		if e.Scope == store.GlobalScope {
			scope = "global"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", scope, e.Content)
	}
	return openai.ChatMessage{
		Role:    "system",
		Content: "Persistent memory from previous sessions (search memory_search for anything not listed here):\n\n" + b.String(),
	}, true
}

func (s *Service) comboSupportsImages(ctx context.Context) (bool, error) {
	status, err := s.gateway.Status(ctx)
	if err != nil {
		return false, err
	}
	return status.ComboSupportsImages(s.cfg.Model), nil
}

func toModelMessages(msgs []store.Message) []openai.ChatMessage {
	out := make([]openai.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, openai.ChatMessage{
			Role: m.Role, Content: m.Content, Images: m.Images,
			ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, Name: m.Name,
		})
	}
	return out
}

func sumUsage(a, b openai.Usage) openai.Usage {
	return openai.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
}
