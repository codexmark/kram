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
	"sync"
	"time"

	"github.com/codexmark/kram-gateway/internal/daemon/compaction"
	"github.com/codexmark/kram-gateway/internal/daemon/gatewayclient"
	"github.com/codexmark/kram-gateway/internal/daemon/session"
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
	Attempts     []openai.AttemptInfo // fallback trail of the final (deciding) gateway call — kept for the simple footer view
	// RouteTrace is the full picture Attempts alone can't show: every
	// model call this run made (a turn can be several, across tool
	// round-trips), each with its own complete fallback trail — see
	// route.go for why this exists and what bug it fixes.
	RouteTrace  RouteTrace
	Usage       openai.Usage // summed across every gateway call this turn
	Compactions int
	ImageNotice string // set if images were attached but the combo can't accept them
}

const maxToolResultChars = 4000 // how much of a tool result ToolActivity keeps for display

// maxTurnToolOutputChars bounds the *combined* size of every tool result
// within one model turn's batch of tool calls — several individually-fine
// results (each under any single tool's own cap) can still add up to a
// large chunk of context in one turn. Not the same problem
// internal/artifact's spill solves (one oversized result); this is
// several fine-sized ones landing together. Deliberately enforced by
// truncation-with-notice here rather than retroactively spilling the
// batch into artifacts: that would mean buffering an entire turn's tool
// results before persisting any of them, a bigger change to a loop this
// project has already had one real "goes silent" bug in — see
// DECISIONS.md.
const maxTurnToolOutputChars = 180_000

// enforceTurnOutputBudget checks content against the running total
// (alreadyUsed) of everything already added to this turn's batch, against
// budget. Never silently drops content — either the full text fits, or
// it's truncated with an explicit notice explaining why, following the
// same "never return empty/silent" discipline as bash's exit framing and
// outputfilter's all-routine case (see DECISIONS.md).
func enforceTurnOutputBudget(content string, alreadyUsed, budget int) (truncated string, hit bool) {
	if alreadyUsed >= budget {
		return fmt.Sprintf("[kram: this turn's combined tool output already reached its %d-character budget; result withheld — call this tool again by itself if you need it]", budget), true
	}
	remaining := budget - alreadyUsed
	if len(content) <= remaining {
		return content, false
	}
	return content[:remaining] + fmt.Sprintf("\n\n[kram: truncated — this turn's combined tool output exceeded its %d-character budget; call this tool again by itself for the rest]", budget), true
}

// Service runs the agent loop for a workspace.
type Service struct {
	store   *store.Store
	gateway *gatewayclient.Client
	tools   *tools.Registry
	cfg     Config

	// pending backs ask_question: one entry per in-flight question,
	// keyed by a generated ID. AnswerQuestion looks the channel up and
	// sends the answer; sessionAsker.Ask (below) is the only reader, and
	// removes its own entry once done either way.
	pendingMu sync.Mutex
	pending   map[string]chan string

	// pendingApprovals backs the permission engine's Ask outcome — kept
	// separate from pending (ask_question) even though the shape is
	// identical, since the two are semantically different pauses (see
	// tools.Approver's doc comment) and conflating their id spaces would
	// let an answer meant for one satisfy the other.
	pendingApprovalsMu sync.Mutex
	pendingApprovals   map[string]chan tools.ApprovalDecision
}

// New builds an agent Service.
func New(st *store.Store, gw *gatewayclient.Client, tr *tools.Registry, cfg Config) *Service {
	return &Service{
		store: st, gateway: gw, tools: tr, cfg: cfg.withDefaults(),
		pending: make(map[string]chan string), pendingApprovals: make(map[string]chan tools.ApprovalDecision),
	}
}

// Tools passes through the registry's full tool listing (enabled or not)
// for the daemon's GET /tools endpoint.
func (s *Service) Tools() []tools.ToolInfo { return s.tools.AllTools() }

// Skills passes through the registry's discovered-skills listing for the
// same endpoint.
func (s *Service) Skills() []tools.Skill { return s.tools.Skills() }

// AnswerQuestion delivers ans to the ask_question call waiting on id, if
// any is still pending. Returns false if id is unknown — already
// answered, timed out, or never existed — so the caller (the daemon's
// HTTP handler) can report a clear 404 instead of silently no-opping.
func (s *Service) AnswerQuestion(id, ans string) bool {
	s.pendingMu.Lock()
	ch, ok := s.pending[id]
	s.pendingMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- ans:
	default:
	}
	return true
}

// askQuestionTimeout bounds how long a turn blocks waiting for the user
// to answer an ask_question call — generous (it's a human, not a retry),
// but bounded so a session can't hang forever if they never respond.
const askQuestionTimeout = 10 * time.Minute

// approvalTimeout bounds how long a turn blocks waiting for the user to
// approve a policy-gated tool call. Same duration as askQuestionTimeout
// for the same reason (a human, not a retry), but on expiry it fails
// *closed* (ApprovalDeny), not open — an unanswered permission prompt must
// never silently become a yes.
const approvalTimeout = 10 * time.Minute

// AnswerApproval delivers the user's decision ("once", "always", or
// "deny") to the pending approval waiting on id, if any is still pending.
// Returns false if id is unknown (already answered, timed out, or never
// existed) or decision isn't one of the three valid values, so the caller
// (the daemon's HTTP handler) can report a clear error instead of silently
// no-opping.
func (s *Service) AnswerApproval(id, decision string) bool {
	d := tools.ApprovalDecision(decision)
	if d != tools.ApprovalOnce && d != tools.ApprovalAlways && d != tools.ApprovalDeny {
		return false
	}
	s.pendingApprovalsMu.Lock()
	ch, ok := s.pendingApprovals[id]
	s.pendingApprovalsMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- d:
	default:
	}
	return true
}

// sessionApprover implements tools.Approver for one turn, mirroring
// sessionAsker — constructed fresh per runLoop call since it closes over
// that turn's live onEvent callback.
type sessionApprover struct {
	svc     *Service
	onEvent EventFunc
}

func (a *sessionApprover) Approve(ctx context.Context, toolName, subject string) (tools.ApprovalDecision, error) {
	id := session.NewID()
	ch := make(chan tools.ApprovalDecision, 1)

	a.svc.pendingApprovalsMu.Lock()
	a.svc.pendingApprovals[id] = ch
	a.svc.pendingApprovalsMu.Unlock()
	defer func() {
		a.svc.pendingApprovalsMu.Lock()
		delete(a.svc.pendingApprovals, id)
		a.svc.pendingApprovalsMu.Unlock()
	}()

	emit(a.onEvent, Event{Kind: EventApproval, ApprovalID: id, ApprovalTool: toolName, ApprovalSubject: subject})

	select {
	case dec := <-ch:
		return dec, nil
	case <-ctx.Done():
		return tools.ApprovalDeny, ctx.Err()
	case <-time.After(approvalTimeout):
		return tools.ApprovalDeny, fmt.Errorf("timed out waiting for approval")
	}
}

// sessionAsker implements tools.Asker for one turn — constructed fresh
// per runLoop call (not stored on Service) since it closes over that
// turn's live onEvent callback and session ID, which vary per call unlike
// the fixed dependencies Delegator needs.
type sessionAsker struct {
	svc     *Service
	onEvent EventFunc
}

func (a *sessionAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	id := session.NewID()
	ch := make(chan string, 1)

	a.svc.pendingMu.Lock()
	a.svc.pending[id] = ch
	a.svc.pendingMu.Unlock()
	defer func() {
		a.svc.pendingMu.Lock()
		delete(a.svc.pending, id)
		a.svc.pendingMu.Unlock()
	}()

	emit(a.onEvent, Event{Kind: EventQuestion, QuestionID: id, Question: question, Options: options})

	select {
	case ans := <-ch:
		return ans, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(askQuestionTimeout):
		return "", fmt.Errorf("timed out waiting for an answer")
	}
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

	return s.runLoop(ctx, sessionID, s.cfg.Model, 0, imageNotice, onEvent)
}

// RunTask implements tools.Delegator: runs goal (plus optional context) to
// completion in a brand-new session, isolated from every other
// conversation — the subagent sees only what's passed here, not the
// parent's history, matching Hermes Agent's "spawn a junior engineer"
// model rather than a shared-context delegation. model, if empty, falls
// back to the parent's own combo. depth is the nesting level the *child*
// will run at (the caller — delegate_task — passes its own depth+1); it's
// threaded through runLoop so a grandchild's own delegate_task call sees
// the right depth and gets blocked once maxSpawnDepth is reached.
func (s *Service) RunTask(ctx context.Context, goal, taskContext, model string, depth int) (string, error) {
	id := session.NewID()
	if _, err := s.store.CreateSession(id, fmt.Sprintf("subagent: %.60s", goal)); err != nil {
		return "", fmt.Errorf("creating subagent session: %w", err)
	}

	prompt := goal
	if taskContext != "" {
		prompt = goal + "\n\nContext:\n" + taskContext
	}
	if _, err := s.store.AppendMessage(id, store.Message{Role: "user", Content: prompt}); err != nil {
		return "", fmt.Errorf("seeding subagent task: %w", err)
	}

	useModel := model
	if useModel == "" {
		useModel = s.cfg.Model
	}

	result, err := s.runLoop(ctx, id, useModel, depth, "", nil)
	if err != nil {
		return "", fmt.Errorf("subagent run failed: %w", err)
	}
	return result.Message.Content, nil
}

// runLoop is the tool-calling loop shared by Run (the top-level
// conversation, depth 0) and RunTask (a delegated subagent, depth >= 1) —
// everything below was originally Run's body, parameterized on model and
// depth instead of always reading s.cfg.Model and assuming depth 0.
func (s *Service) runLoop(ctx context.Context, sessionID, model string, depth int, imageNotice string, onEvent EventFunc) (RunResult, error) {
	ctx = tools.WithDepth(ctx, depth)
	ctx = tools.WithAsker(ctx, &sessionAsker{svc: s, onEvent: onEvent})
	ctx = tools.WithApprover(ctx, &sessionApprover{svc: s, onEvent: onEvent})
	// One opaque ID for this whole run — every model call streamCall
	// makes below, across every tool round-trip, carries it, so the
	// gateway's Sticky routing can tell this run apart from a later,
	// unrelated turn in the same session (see gatewayclient.WithRunID).
	ctx = gatewayclient.WithRunID(ctx, session.NewID())

	// Memory is snapshotted once per run rather than re-read every turn,
	// borrowing Hermes Agent's "frozen at session start" idea for the
	// reason behind it: the preamble is a prompt *prefix*, and a prefix
	// that changes between calls throws away the provider's prefix cache
	// on every tool round-trip — which is exactly where a tool-calling
	// loop makes most of its calls. Kram freezes per run (one user
	// message and all its tool round-trips) rather than per session, so a
	// fact written mid-conversation still shows up on the user's very
	// next message instead of only in a new session.
	memoryMsg, haveMemory := s.recentMemoryMessage()

	result := RunResult{ImageNotice: imageNotice}
	compactions := 0
	graceUsed := false
	// emptyRetryUsed guards against a real, observed failure mode of weak
	// free-tier models: after a tool result that reads like a failure (an
	// "[exit error: ...]" from a command whose non-zero exit is actually
	// normal — grep finding nothing, say), the model sometimes gives up
	// and returns a genuinely empty final answer instead of explaining or
	// retrying. Without this, that dead-ends the turn silently — the
	// daemon reports "done" with real telemetry, so nothing looks wrong,
	// but the user sees no response at all. One retry with an explicit
	// nudge; if it happens twice in a row, say so instead of persisting
	// blank content.
	emptyRetryUsed := false

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
				marker, err := compaction.Compact(ctx, s.gateway, model, pruned)
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

		// Preamble order, most general first: who you are and how to work
		// (systemPrompt) → this project's own rules (AGENTS.md) → facts
		// remembered about this user/project (memory) → the conversation.
		// None of it is persisted into history: each is rebuilt every turn
		// from its current source, so editing AGENTS.md or writing a memory
		// mid-conversation takes effect on the very next message instead of
		// requiring a restart.
		var preamble []openai.ChatMessage
		preamble = append(preamble, openai.ChatMessage{Role: "system", Content: systemPrompt(s.cfg.Workspace)})
		if projectContext, found := loadProjectContext(s.cfg.Workspace); found {
			preamble = append(preamble, openai.ChatMessage{
				Role:    "system",
				Content: "Project context (from AGENTS.md/CLAUDE.md):\n\n" + projectContext,
			})
		}
		if haveMemory {
			preamble = append(preamble, memoryMsg)
		}
		modelMessages := append(preamble, toModelMessages(effective)...)
		if emptyRetryUsed {
			// Only true on the turn right after an empty final answer —
			// see the ToolCalls==0 branch below for where this is set.
			modelMessages = append(modelMessages, openai.ChatMessage{
				Role:    "system",
				Content: "Your previous response was empty. Answer the user directly in plain text now — do not return another empty response.",
			})
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

		emit(onEvent, Event{Kind: EventRouteStart})
		callResult, err := s.streamCall(ctx, model, modelMessages, toolDefs, onEvent)
		if err != nil {
			return RunResult{}, fmt.Errorf("gateway call failed: %w", err)
		}
		result.Attempts = callResult.Attempts
		result.Usage = sumUsage(result.Usage, callResult.Usage)
		routeCall := result.RouteTrace.addCall(model, callResult.Strategy, callResult.Attempts, callResult.Ranking)
		emit(onEvent, Event{Kind: EventRouteDone, RouteCall: &routeCall})

		if len(callResult.ToolCalls) == 0 {
			if strings.TrimSpace(callResult.Content) == "" && !emptyRetryUsed {
				emptyRetryUsed = true
				continue // one retry with a nudge (added to the preamble above) before giving up
			}
			content := callResult.Content
			if strings.TrimSpace(content) == "" {
				content = "(no response — the model returned nothing twice in a row; try rephrasing, or check whether the provider is degraded)"
			}
			assistantMsg, err := s.store.AppendMessage(sessionID, store.Message{
				Role: "assistant", Content: content, Provider: callResult.Provider,
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

		// turnOutputChars tracks the combined size of every tool result in
		// *this* batch of tool calls (one model turn can request several).
		// A per-call cap (bashMaxOutputBytes and friends) bounds any one
		// result, but several individually-fine results still add up —
		// see enforceTurnOutputBudget's doc comment.
		turnOutputChars := 0
		for _, tc := range callResult.ToolCalls {
			emit(onEvent, Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
			activity, toolMsg := s.runTool(ctx, tc)

			original := len(toolMsg.Content)
			if truncated, hit := enforceTurnOutputBudget(toolMsg.Content, turnOutputChars, maxTurnToolOutputChars); hit {
				toolMsg.Content = truncated
				activity.Result = truncated
			}
			turnOutputChars += original

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
func (s *Service) streamCall(ctx context.Context, model string, messages []openai.ChatMessage, toolDefs []openai.Tool, onEvent EventFunc) (gatewayclient.Result, error) {
	deltas, err := s.gateway.ChatCompletionStream(ctx, model, messages, toolDefs)
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
				Ranking: d.Ranking, Strategy: d.Strategy,
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
		// Ids are included so memory_write's replace/remove operations
		// have something to target without a memory_search round-trip
		// first — consolidation is only realistic if the model can see
		// what it's consolidating.
		fmt.Fprintf(&b, "- #%d [%s] %s\n", e.ID, scope, e.Content)
	}
	return openai.ChatMessage{
		Role:    "system",
		Content: "Persistent memory from previous sessions (use memory_search for anything not listed here):\n\n" + b.String(),
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
