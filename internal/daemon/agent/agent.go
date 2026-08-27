// Package agent is Kram's tool-calling loop: the piece that actually
// makes the daemon useful rather than a plain chat relay. Each turn it
// sends the session's history (and tool definitions) to the gateway; if
// the model asks to call tools, they run and their results feed back in,
// looping until the model answers in plain text, reaches a real stagnation
// guard, or exhausts the segmented emergency budget.
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

	"github.com/codexmark/kram/internal/daemon/compaction"
	"github.com/codexmark/kram/internal/daemon/contextpolicy"
	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/session"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/snapshot"
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
	// MaxTurns is the number of model calls in one automatic continuation
	// segment (tool round-trips included). Reaching it no longer ends a real
	// task by itself; MaxSegmentsPerRun controls the emergency total.
	MaxTurns int
	// MaxSegmentsPerRun lets a productive long task continue automatically
	// across MaxTurns-sized segments. Default 4 (50 * 4 = 200 calls). Segment
	// boundaries are ephemeral structured UI checkpoints, while the final
	// boundary still gets the existing tool-free soft landing.
	MaxSegmentsPerRun int
	// MaxCompactionsPerRun caps consecutive compaction attempts within a
	// single Run before giving up with ErrContextOverflow.
	MaxCompactionsPerRun int
	// MaxContextTokens is the effective-history budget before compaction
	// triggers (see internal/daemon/compaction).
	MaxContextTokens int
	// Workspace is the project root — used to load AGENTS.md/CLAUDE.md as
	// persistent project context, injected into every turn.
	Workspace string
	// MaxGatewayRounds bounds how many times callModelWithRetry retries a
	// whole gateway call (a fresh ranked-candidate pass) after a
	// retryable GatewayError, before giving up — see retry.go. Runs
	// entirely inside one iteration of runLoop's turn loop, so retrying
	// never consumes MaxTurns: no new logical decision by the model
	// happened, just another attempt at the same one. Default 3.
	MaxGatewayRounds int
	// PreferStreaming opts a session back into the streaming gateway
	// call path (see streamCall) instead of the buffered default (see
	// bufferedCall). Streaming commits to one candidate the moment
	// router.BoundedPeek sees a meaningful first signal — if that
	// candidate then fails mid-stream, the whole turn fails with it,
	// since HTTP headers are already sent and no further fallback is
	// possible. The buffered path doesn't have that problem: kram-
	// gateway's own non-streaming branch already tries every ranked
	// candidate to completion before writing anything back (see
	// internal/server/chat.go), so it's the default. False (buffered) is
	// what almost every caller wants; this exists as an escape hatch,
	// not a recommendation.
	PreferStreaming bool
	// ToolOrder curates the generated Tools overview's presentation
	// order (see compileToolsOverview) — it never changes which tools
	// are offered, only where each one is listed. nil means today's
	// plain alphabetical order. When set, it must contain
	// tools.ToolOrderRest exactly once, marking where every unlisted
	// tool is inserted (still alphabetical); New validates this,
	// including that every named tool actually exists in the registry,
	// and fails loudly rather than silently dropping a typo.
	ToolOrder []string
	// SystemPromptOverride, when non-empty, replaces the "base"
	// PromptPart's content (identity/workflow/skills/memory/delegation/
	// asking/writing-code/output/safety — see systemPrompt) with this
	// text instead of Kram's own. Every other preamble part — the
	// generated tools overview, background-job guidance, project
	// context, memory — is unaffected and still assembled normally
	// around it; this deliberately can't suppress those, since the
	// tools overview exists specifically so a tool can't go silently
	// unmentioned (see the VisibleTools() fix), and an override that
	// could also drop that would reopen the exact bug that fix closed.
	// Empty (the default) means today's systemPrompt(workspace) output,
	// unchanged. Sourcing this from a file or a CLI flag is left to the
	// caller — this field takes the resolved text directly, matching
	// how ToolOrder is a resolved value too, not a file path.
	SystemPromptOverride string
}

func (c Config) withDefaults() Config {
	if c.MaxTurns <= 0 {
		c.MaxTurns = 50
	}
	if c.MaxSegmentsPerRun <= 0 {
		c.MaxSegmentsPerRun = 4
	}
	if c.MaxCompactionsPerRun <= 0 {
		c.MaxCompactionsPerRun = 3
	}
	if c.MaxContextTokens <= 0 {
		c.MaxContextTokens = compaction.DefaultMaxTokens
	}
	if c.MaxGatewayRounds <= 0 {
		c.MaxGatewayRounds = defaultMaxGatewayRounds
	}
	return c
}

// ToolActivity records one tool call the loop made, for callers (the CLI)
// that want to show what the agent actually did, not just its final answer.
type ToolActivity struct {
	Name      string `json:"name"`
	Args      string `json:"args"`
	Result    string `json:"result"`
	OK        bool   `json:"ok"`
	ProcessID string `json:"process_id,omitempty"`
}

type toolStagnation struct {
	name   string
	args   string
	result string
	count  int
}

func (s *toolStagnation) observe(activity ToolActivity) int {
	// Repeated process_output polling is legitimate while a background job
	// is still running; every other byte-identical result means no observable
	// progress, regardless of whether that tool encodes failure in error or in
	// its textual result (several built-ins deliberately do the latter).
	if activity.Name == "process_output" && strings.Contains(activity.Result, "[still running]") {
		*s = toolStagnation{}
		return 0
	}
	if activity.Name == s.name && activity.Args == s.args && activity.Result == s.result {
		s.count++
		return s.count
	}
	s.name = activity.Name
	s.args = activity.Args
	s.result = activity.Result
	s.count = 1
	return s.count
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

	// heartbeatInterval overrides the package-level heartbeatInterval
	// default — unexported, only ever set directly by a same-package
	// test that wants bufferedCall's ticker on a testable timescale
	// instead of waiting out the real interval.
	heartbeatInterval time.Duration

	// calibrator corrects the chars/4 token estimate toward each session's
	// real prompt_tokens, so the compaction budget tracks the model's
	// actual tokenizer instead of a fixed approximation — see calibration.go.
	calibrator *tokenCalibrator

	// steerMu guards steering: user messages queued while a turn runs,
	// drained into the session at the next model-call boundary — see
	// QueueSteering and drainSteering.
	steerMu  sync.Mutex
	steering map[string][]string

	// comboMu guards activeCombo — the gateway combo new top-level runs
	// route to. Seeded from cfg.Model at startup and switched at runtime via
	// SetActiveCombo (the in-app routing picker); a run captures it once at
	// start, so an in-flight turn is never rerouted mid-stream.
	comboMu     sync.RWMutex
	activeCombo string
}

// New builds an agent Service.
// New builds a Service, or fails if cfg.ToolOrder is malformed or names
// a tool tr doesn't actually have registered — the "fail loud instead of
// a typo silently vanishing" guarantee the tool-order feature exists to
// provide (see tools.ValidateToolOrder / tools.UnknownToolOrderNames).
func New(st *store.Store, gw *gatewayclient.Client, tr *tools.Registry, cfg Config) (*Service, error) {
	if err := tools.ValidateToolOrder(cfg.ToolOrder); err != nil {
		return nil, fmt.Errorf("invalid ToolOrder: %w", err)
	}
	if tr != nil && cfg.ToolOrder != nil {
		known := make(map[string]bool)
		for _, info := range tr.AllTools() {
			known[info.Name] = true
		}
		if unknown := tools.UnknownToolOrderNames(cfg.ToolOrder, known); len(unknown) > 0 {
			return nil, fmt.Errorf("ToolOrder names unregistered tool(s): %v", unknown)
		}
	}
	resolved := cfg.withDefaults()
	return &Service{
		store: st, gateway: gw, tools: tr, cfg: resolved,
		pending: make(map[string]chan string), pendingApprovals: make(map[string]chan tools.ApprovalDecision),
		heartbeatInterval: heartbeatInterval,
		calibrator:        newTokenCalibrator(),
		activeCombo:       resolved.Model,
		steering:          make(map[string][]string),
	}, nil
}

// activeModel returns the combo new top-level runs currently route to.
func (s *Service) activeModel() string {
	s.comboMu.RLock()
	defer s.comboMu.RUnlock()
	return s.activeCombo
}

// SetActiveCombo switches the combo new top-level runs route to. It
// validates comboID against the combos the gateway currently advertises so
// a typo is a clean error rather than a silent fallback to default_combo on
// the next call. In-flight runs keep the combo they started with — each run
// captures activeModel() once at its start.
func (s *Service) SetActiveCombo(ctx context.Context, comboID string) error {
	if comboID == "" {
		return fmt.Errorf("combo must not be empty")
	}
	status, err := s.gateway.Status(ctx)
	if err != nil {
		return fmt.Errorf("checking combos: %w", err)
	}
	for _, c := range status.Combos {
		if c.ID == comboID {
			s.comboMu.Lock()
			s.activeCombo = comboID
			s.comboMu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("unknown combo %q", comboID)
}

// Tools passes through the registry's full tool listing (enabled or not)
// for the daemon's GET /tools endpoint.
func (s *Service) Tools() []tools.ToolInfo { return s.tools.AllTools() }

// AutoCheckpointPrefix marks the snapshots runLoop takes on its own
// before a turn's first mutating tool call — the rewind endpoint filters
// on it so one-key rewind only ever targets automatic checkpoints, never
// a snapshot the model or user created deliberately.
const AutoCheckpointPrefix = "auto checkpoint"

// QueueSteering enqueues a mid-turn user message for sessionID. The
// running turn drains the queue at its next model-call boundary — after
// the current batch's tool results, or after a final answer (which then
// keeps the turn going instead of ending it). Queued content survives a
// lost race with the turn's end: the session's next run drains leftovers
// before its first model call, so a steering message is never dropped.
func (s *Service) QueueSteering(sessionID, content string) {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	s.steering[sessionID] = append(s.steering[sessionID], content)
}

// drainSteering persists any queued steering messages as real user
// messages (so the next model call simply sees them in history) and
// reports whether any were applied.
func (s *Service) drainSteering(sessionID string, onEvent EventFunc) (bool, error) {
	s.steerMu.Lock()
	queued := s.steering[sessionID]
	delete(s.steering, sessionID)
	s.steerMu.Unlock()
	if len(queued) == 0 {
		return false, nil
	}
	for _, content := range queued {
		if _, err := s.store.AppendMessage(sessionID, store.Message{Role: "user", Content: content}); err != nil {
			return false, fmt.Errorf("persisting steering message: %w", err)
		}
	}
	emit(onEvent, Event{Kind: EventNotice, Notice: fmt.Sprintf("picked up %d queued message(s) from you", len(queued))})
	return true, nil
}

// LatestAutoCheckpoint returns the newest automatic pre-mutation
// checkpoint, or ok=false when none exists yet.
func (s *Service) LatestAutoCheckpoint(ctx context.Context) (snapshot.Snapshot, bool, error) {
	snaps, err := s.tools.Snapshots().List(ctx)
	if err != nil {
		return snapshot.Snapshot{}, false, err
	}
	for _, sn := range snaps { // List is newest-first
		if strings.HasPrefix(sn.Message, AutoCheckpointPrefix) {
			return sn, true, nil
		}
	}
	return snapshot.Snapshot{}, false, nil
}

// Rewind restores the workspace to the given snapshot id and returns
// what changed plus the snapshot's own metadata for display.
// It first captures the *current* state as a "pre-rewind" snapshot.
// That does two jobs at once: it makes the rewind itself undoable (the
// pre-rewind snapshot restores like any other), and it brings files
// created after the target checkpoint under the snapshot repository's
// knowledge — Restore deliberately never touches a file no snapshot ever
// captured (see snapshot.Store.Restore), so without this capture a file
// the turn *created* would survive the rewind as an orphan.
func (s *Service) Rewind(ctx context.Context, id string) (snapshot.RestoreResult, snapshot.Snapshot, error) {
	if _, err := s.tools.Snapshots().Create(ctx, "pre-rewind state (restore this to undo the rewind)"); err != nil {
		return snapshot.RestoreResult{}, snapshot.Snapshot{}, fmt.Errorf("capturing pre-rewind state: %w", err)
	}
	res, err := s.tools.Snapshots().Restore(ctx, id)
	if err != nil {
		return snapshot.RestoreResult{}, snapshot.Snapshot{}, err
	}
	snaps, _ := s.tools.Snapshots().List(ctx)
	for _, sn := range snaps {
		if sn.ID == res.SnapshotID || strings.HasPrefix(sn.ID, id) || strings.HasPrefix(res.SnapshotID, sn.ID) {
			return res, sn, nil
		}
	}
	return res, snapshot.Snapshot{ID: res.SnapshotID}, nil
}

// Skills passes through the registry's discovered-skills listing for the
// same endpoint.
func (s *Service) Skills() []tools.Skill { return s.tools.Skills() }

// ReplaceDisabledTools applies a persisted tools/skills profile to this live
// daemon so the first post-setup session cannot observe startup-time settings.
func (s *Service) ReplaceDisabledTools(names []string) { s.tools.ReplaceDisabled(names) }

// BackgroundProcesses and BackgroundProcessOutput are read-only pass-throughs
// for the local TUI. Keeping the server dependent on Service, rather than on a
// second Registry reference, preserves the daemon's construction boundary.
func (s *Service) BackgroundProcesses() []tools.BackgroundProcessInfo {
	return s.tools.BackgroundProcesses()
}

func (s *Service) BackgroundProcessOutput(id string, cursor *int64) (tools.BackgroundProcessOutput, bool) {
	return s.tools.BackgroundProcessOutput(id, cursor)
}

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

func (a *sessionApprover) Approve(ctx context.Context, toolName, subject, diff string) (tools.ApprovalDecision, error) {
	// No live event sink means no human is watching this run's stream —
	// the subagent case (RunTask passes onEvent: nil). Emitting an
	// approval prompt nobody can see and then blocking on a channel
	// nobody can feed would just stall for the full approvalTimeout (and
	// with delegate_task running several subagents concurrently, several
	// such stalls at once). Deny immediately instead: a subagent must
	// never be able to auto-approve what a human operator would have been
	// asked to sign off on.
	if a.onEvent == nil {
		return tools.ApprovalDeny, nil
	}
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

	emit(a.onEvent, Event{Kind: EventApproval, ApprovalID: id, ApprovalTool: toolName, ApprovalSubject: subject, ApprovalDiff: diff})

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
	// Same reasoning as sessionApprover.Approve: with no live event sink
	// (the subagent case — RunTask passes onEvent: nil) the question
	// reaches no one and the wait would block for the full
	// askQuestionTimeout. Return an error immediately so the subagent's
	// tool call fails fast with a clear reason it can act on, rather than
	// hanging the whole delegation.
	if a.onEvent == nil {
		return "", fmt.Errorf("ask_question is unavailable in this context (no interactive session attached)")
	}
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

	// Capture the active combo once, so an image-capability check and the
	// run itself can't read two different combos across a concurrent switch,
	// and a mid-run SetActiveCombo only affects the NEXT top-level run.
	model := s.activeModel()

	imageNotice := ""
	if len(images) > 0 {
		ok, err := s.comboSupportsImages(ctx, model)
		if err == nil && !ok {
			imageNotice = fmt.Sprintf("images were attached, but no provider in combo %q supports image input — sent as text only", model)
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

	return s.runLoop(ctx, sessionID, model, 0, imageNotice, onEvent)
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
		useModel = s.activeModel()
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
	ctx = tools.WithRunModel(ctx, model) // so delegate_task defaults a subagent to this run's combo
	ctx = tools.WithAsker(ctx, &sessionAsker{svc: s, onEvent: onEvent})
	ctx = tools.WithApprover(ctx, &sessionApprover{svc: s, onEvent: onEvent})
	// One opaque ID for this whole run — every model call streamCall
	// makes below, across every tool round-trip, carries it, so the
	// gateway's Sticky routing can tell this run apart from a later,
	// unrelated turn in the same session (see gatewayclient.WithRunID).
	ctx = gatewayclient.WithRunID(ctx, session.NewID())
	ctx = gatewayclient.WithPromptCacheKey(ctx, "kram-"+sessionID)

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
	checkpointed := false
	var stagnation toolStagnation
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

	totalTurns := s.cfg.MaxTurns * s.cfg.MaxSegmentsPerRun
	emit(onEvent, Event{Kind: EventSegment, Segment: 1, Segments: s.cfg.MaxSegmentsPerRun})
	for turn := 0; turn < totalTurns; turn++ {
		if turn > 0 && turn%s.cfg.MaxTurns == 0 {
			segment := turn/s.cfg.MaxTurns + 1
			emit(onEvent, Event{Kind: EventSegment, Segment: segment, Segments: s.cfg.MaxSegmentsPerRun})
		}
		// Steering queued while the previous batch ran (or left over from
		// a prior run's final moments) becomes part of this call's history.
		if _, err := s.drainSteering(sessionID, onEvent); err != nil {
			return RunResult{}, err
		}
		all, err := s.store.ListMessages(sessionID)
		if err != nil {
			return RunResult{}, fmt.Errorf("loading history: %w", err)
		}
		effective := compaction.EffectiveHistory(all)

		nearBudget := turn == totalTurns-1
		projectContext, haveProjectContext := loadProjectContext(s.cfg.Workspace)
		// Only actually inject a fresh project-context/memory part when its
		// content isn't already sitting, unchanged, in this session's own
		// effective history from an earlier turn — see needsFreshInjection's
		// doc comment. haveProjectContext/haveMemory above stay "is there
		// any context/memory at all"; injectProjectContext/injectMemory is
		// the narrower "does the model need a fresh copy on this call"
		// compilePreamble actually consumes.
		injectProjectContext := haveProjectContext && needsFreshInjection(effective, projectContextMarkerName, formatProjectContextContent(projectContext))
		injectMemory := haveMemory && needsFreshInjection(effective, memoryMarkerName, memoryMsg.Content)
		preambleParts := compilePreamble(s.cfg.Workspace, projectContext, injectProjectContext, memoryMsg, injectMemory, s.tools, s.cfg.ToolOrder, s.cfg.SystemPromptOverride)
		postscriptParts := compileTurnPostscript(emptyRetryUsed, nearBudget)
		// Keep the visible definitions separately from the subset offered on
		// this turn. The final soft-landing turn deliberately offers no tools,
		// but a local model can still print a textual <tool_call> from habit.
		// We need the visible allowlist to recognize and sanitize that markup
		// without executing it past the turn budget.
		visibleToolDefs := s.tools.Definitions()
		toolDefs := visibleToolDefs
		if nearBudget {
			toolDefs = nil
		}
		fixedTokens := estimatePromptPartTokens(append(append([]PromptPart{}, preambleParts...), postscriptParts...)) + estimateToolDefinitionTokens(toolDefs)
		// Correct the chars/4 estimates toward this session's real
		// prompt_tokens before comparing against the budget — see
		// calibration.go. Factor is 1.0 until the first response is observed,
		// so the pre-calibration behavior is unchanged for a session's first
		// call.
		calibration := s.calibrator.factor(sessionID)
		policy := contextpolicy.New(s.cfg.MaxContextTokens, scaleTokens(fixedTokens, calibration))
		pruned := compaction.PruneForModel(effective)
		switch policy.Action(scaleTokens(compaction.EstimateTokens(effective), calibration), scaleTokens(compaction.EstimateTokens(pruned), calibration)) {
		case contextpolicy.Compact:
			if compactions >= s.cfg.MaxCompactionsPerRun {
				return RunResult{}, ErrContextOverflow
			}
			marker, err := compaction.Compact(ctx, s.gateway, model, pruned)
			if err != nil {
				// The summarizer itself needs a model call, and the model
				// being unreachable is precisely when compaction tends to be
				// needed — failing the turn here turned a transient
				// summarizer error into a dead session. Emergency-prune this
				// call's context instead (drop oldest whole user-turns; see
				// compaction.EmergencyPrune) and proceed: nothing is
				// persisted, the full history stays intact, and the next
				// healthy compaction summarizes as usual.
				emergency := compaction.EmergencyPrune(pruned, s.cfg.MaxContextTokens)
				if len(emergency) >= len(pruned) {
					return RunResult{}, fmt.Errorf("compacting session: %w", err)
				}
				effective = emergency
				emit(onEvent, Event{Kind: EventNotice, Notice: "summary model unavailable — oldest turns left out of this call's context (the session keeps them)"})
			} else {
				if _, err := s.store.AppendMessage(sessionID, marker); err != nil {
					return RunResult{}, fmt.Errorf("persisting compaction summary: %w", err)
				}
				compactions++
				result.Compactions = compactions
				emit(onEvent, Event{Kind: EventNotice, Notice: "session history was compacted to stay in budget"})
				continue // reload the now-much-shorter effective history before calling the model
			}
		case contextpolicy.Prune:
			effective = pruned
		}

		// Persist a breadcrumb for whichever of project-context/memory is
		// actually being injected fresh this call — the record
		// needsFreshInjection reads back on a later turn to decide it can
		// skip resending unchanged content (see promptcompiler.go). Only
		// reached once we're actually proceeding to call the model this
		// iteration (the Compact case above continues before this point),
		// so a marker is never persisted for a decision that gets discarded.
		if injectProjectContext {
			if _, err := s.store.AppendMessage(sessionID, store.Message{
				Role: "system", Name: projectContextMarkerName, Content: formatProjectContextContent(projectContext),
			}); err != nil {
				return RunResult{}, fmt.Errorf("persisting project-context marker: %w", err)
			}
		}
		if injectMemory {
			if _, err := s.store.AppendMessage(sessionID, store.Message{
				Role: "system", Name: memoryMarkerName, Content: memoryMsg.Content,
			}); err != nil {
				return RunResult{}, fmt.Errorf("persisting memory marker: %w", err)
			}
		}

		// Preamble order, most general first: who you are and how to work
		// (systemPrompt) → this project's own rules (AGENTS.md) → facts
		// remembered about this user/project (memory) → the conversation
		// → any turn-specific reminders. Project-context/memory are the
		// two exceptions to "none of the preamble is persisted": once
		// injected, a marker breadcrumb (not recomputed from scratch) lives
		// on in history so an unchanged copy doesn't need resending on
		// every later turn — see needsFreshInjection above. Everything
		// else in the preamble is still rebuilt every turn from its
		// current source, so editing AGENTS.md or writing a memory mid-
		// conversation still takes effect on the very next message.
		// Built via the Prompt Compiler (promptcompiler.go) — same
		// messages this block always produced, just assembled through
		// PromptPart values instead of inline literals, so the ordering
		// and per-part refresh cadence are real, inspectable data instead
		// of implicit in append-call order.
		preamble := partsToMessages(preambleParts)
		modelMessages := append(preamble, toModelMessages(effective)...)
		modelMessages = append(modelMessages, partsToMessages(postscriptParts)...)

		// Raw (uncalibrated) chars/4 estimate of exactly what we're about to
		// send — history as finally pruned, plus the fixed preamble/tools.
		// Paired with the real prompt_tokens the response reports, this is
		// what the calibrator learns from (see callModelWithRetry).
		sentEstimate := compaction.EstimateTokens(effective) + fixedTokens

		// Soft landing (Hermes's pattern) on the final allowed turn: stop
		// offering tools and ask directly for a wrap-up, rather than
		// hard-cutting mid-tool-loop. Tool *visibility* is a runtime/policy
		// concern, not prompt content — deliberately kept out of the
		// compiler above.
		emit(onEvent, Event{Kind: EventRouteStart})
		callResult, err := s.callModelWithRetry(ctx, sessionID, sentEstimate, model, modelMessages, toolDefs, onEvent)
		if err != nil {
			return RunResult{}, fmt.Errorf("gateway call failed: %w", humanizeGatewayFailure(err, s.cfg.MaxGatewayRounds))
		}
		result.Attempts = callResult.Attempts
		result.Usage = sumUsage(result.Usage, callResult.Usage)
		routeCall := result.RouteTrace.addCall(model, callResult.Strategy, callResult.Attempts, callResult.Ranking)
		emit(onEvent, Event{Kind: EventRouteDone, RouteCall: &routeCall})

		// Some OpenAI-compatible/local models occasionally print the tool-call
		// protocol as literal text instead of returning structured tool_calls.
		// A whole-response, allowlisted parser recovers that provider defect so
		// the turn does not end silently on raw <tool_call> markup. Free-form
		// prose containing a tag is deliberately never executed.
		if len(callResult.ToolCalls) == 0 {
			if recovered, ok := recoverTextToolCalls(callResult.Content, visibleToolDefs); ok {
				if nearBudget {
					names := make([]string, len(recovered))
					for i, call := range recovered {
						names[i] = call.Function.Name
					}
					callResult.Content = fmt.Sprintf(
						"(stopped: reached the turn limit while the model was still trying to call %s)",
						strings.Join(names, ", "),
					)
					emit(onEvent, Event{Kind: EventNotice, Notice: "provider attempted textual tool markup at the turn limit; Kram stopped it instead of exposing raw markup"})
				} else {
					callResult.ToolCalls = recovered
					callResult.Content = ""
					emit(onEvent, Event{Kind: EventNotice, Notice: "provider returned textual tool markup; Kram normalized it and continued"})
				}
			}
		}

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
				Role: "assistant", Content: content, Provider: callResult.Provider, ProviderItems: callResult.ProviderItems,
			})
			if err != nil {
				return RunResult{}, fmt.Errorf("persisting assistant message: %w", err)
			}
			result.Message = assistantMsg
			// The user redirected mid-answer: the finished answer stands,
			// and the turn keeps going — the next iteration's history is
			// this answer plus their new message.
			if steered, err := s.drainSteering(sessionID, onEvent); err != nil {
				return RunResult{}, err
			} else if steered {
				continue
			}
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

		// This turn produced tool calls — a productive turn — so clear the
		// empty-retry flag. Without this, a single empty response anywhere
		// in the run latches emptyRetryUsed on forever, and every
		// subsequent turn (including ones mid-productive-tool-loop) keeps
		// getting the "your previous response was empty, answer in plain
		// text now" nudge — the exact opposite of what a model working
		// through a chain of tool calls needs. The nudge is meant only for
		// the one retry immediately after an empty response.
		emptyRetryUsed = false

		if _, err := s.store.AppendMessage(sessionID, store.Message{
			Role: "assistant", Content: callResult.Content, ToolCalls: callResult.ToolCalls, Provider: callResult.Provider, ProviderItems: callResult.ProviderItems,
		}); err != nil {
			return RunResult{}, fmt.Errorf("persisting assistant tool-call message: %w", err)
		}

		// turnOutputChars tracks the combined size of every tool result in
		// *this* batch of tool calls (one model turn can request several).
		// A per-call cap (bashMaxOutputBytes and friends) bounds any one
		// result, but several individually-fine results still add up —
		// see enforceTurnOutputBudget's doc comment.
		turnOutputChars := 0
		turnOutputBudget := policy.ToolOutputBudgetChars(compaction.EstimateTokens(effective), maxTurnToolOutputChars)
		// Automatic checkpoint, lazily: the first batch of this run that
		// contains a mutating call snapshots the workspace *before* it runs,
		// so one key can rewind everything this turn changed (see the
		// daemon's /rewind endpoint). Read-only turns never pay for it, and
		// a failure (git missing, exotic workspace) is logged into the run
		// as a notice rather than blocking the turn — best-effort by design.
		if !checkpointed && batchHasMutation(callResult.ToolCalls) {
			checkpointed = true
			if snap, err := s.tools.Snapshots().Create(ctx, AutoCheckpointPrefix+" — before this turn's first change"); err == nil {
				emit(onEvent, Event{Kind: EventNotice, Notice: "checkpoint " + snap.ShortID() + " saved (ctrl+g rewinds)"})
			}
		}
		outcomes := s.runToolBatch(ctx, callResult.ToolCalls, onEvent)
		for tcIdx, tc := range callResult.ToolCalls {
			activity, toolMsg := outcomes[tcIdx].activity, outcomes[tcIdx].msg
			repeatedFailure := stagnation.observe(activity)
			if repeatedFailure >= 3 {
				guard := fmt.Sprintf(
					"[Kram stagnation guard: %s returned an identical result with identical arguments %d times consecutively. Do not repeat it unchanged; choose a different strategy or explain the blocker.]",
					activity.Name, repeatedFailure,
				)
				toolMsg.Content += "\n\n" + guard
				activity.Result += "\n\n" + guard
				emit(onEvent, Event{Kind: EventNotice, Notice: fmt.Sprintf("stagnation detected in %s (%d identical failures)", activity.Name, repeatedFailure)})
			}

			original := len(toolMsg.Content)
			if truncated, hit := enforceTurnOutputBudget(toolMsg.Content, turnOutputChars, turnOutputBudget); hit {
				toolMsg.Content = truncated
				activity.Result = truncated
			}
			turnOutputChars += original

			emit(onEvent, Event{
				Kind: EventToolResult, ToolName: tc.Function.Name, ToolResult: activity.Result,
				ToolOK: activity.OK, ProcessID: activity.ProcessID,
			})
			result.ToolActivity = append(result.ToolActivity, activity)
			if _, err := s.store.AppendMessage(sessionID, toolMsg); err != nil {
				return RunResult{}, fmt.Errorf("persisting tool result: %w", err)
			}
			if repeatedFailure >= 4 {
				content := fmt.Sprintf(
					"(blocked: the model repeated the %s call with identical arguments and result %d times, including after Kram required a strategy change)",
					activity.Name, repeatedFailure,
				)
				assistantMsg, err := s.store.AppendMessage(sessionID, store.Message{
					Role: "assistant", Content: content, Provider: callResult.Provider,
				})
				if err != nil {
					return RunResult{}, fmt.Errorf("persisting stagnation stop: %w", err)
				}
				result.Message = assistantMsg
				return result, nil
			}
		}
		// Loop continues: the tool results just persisted become part of
		// the history the next iteration sends back to the model.
	}

	return RunResult{}, fmt.Errorf("agent loop exhausted %d segments (%d turns) without a final answer", s.cfg.MaxSegmentsPerRun, totalTurns)
}

// heartbeatInterval is how often the quiet stretches of a turn emit
// EventHeartbeat — a buffered gateway call in flight (bufferedCall), a
// streaming call whose provider hasn't sent anything yet or is pausing
// mid-generation (streamCall), and a long-running tool between
// tool_start and tool_result (runTool). Chosen with margin under the
// CLI's own stall-warning threshold (internal/cli/app/view.go's
// stallThreshold, 8s at time of writing) so at least one heartbeat lands
// before that warning would otherwise paint during a longer-but-healthy
// wait.
const heartbeatInterval = 4 * time.Second

// callModel picks the buffered or streaming gateway call path per
// s.cfg.PreferStreaming — see that field's doc comment for why buffered
// is the default.
func (s *Service) callModel(ctx context.Context, model string, messages []openai.ChatMessage, toolDefs []openai.Tool, onEvent EventFunc) (gatewayclient.Result, error) {
	if s.cfg.PreferStreaming {
		return s.streamCall(ctx, model, messages, toolDefs, onEvent)
	}
	return s.bufferedCall(ctx, model, messages, toolDefs, onEvent)
}

// bufferedCall makes one gateway call over the non-streaming path and
// waits for the complete result — kram-gateway's own non-streaming
// branch already tries every ranked candidate to completion before
// writing anything back (see internal/server/chat.go), so unlike
// streamCall this never commits to one candidate before knowing whether
// it actually succeeded. Since there's no incremental per-token output
// to relay, EventHeartbeat fires periodically instead purely so a
// caller's own liveness/stall-detection clock doesn't misread a longer
// (but healthy) multi-candidate wait as a hang.
func (s *Service) bufferedCall(ctx context.Context, model string, messages []openai.ChatMessage, toolDefs []openai.Tool, onEvent EventFunc) (gatewayclient.Result, error) {
	type outcome struct {
		result gatewayclient.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := s.gateway.ChatCompletion(ctx, model, messages, toolDefs)
		done <- outcome{r, err}
	}()

	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.result, out.err
		case <-ticker.C:
			emit(onEvent, Event{Kind: EventHeartbeat})
		}
	}
}

// streamCall makes one gateway call over the streaming path and
// accumulates it into the same shape the non-streaming call returns,
// emitting each text fragment via onEvent as it arrives. Only reached
// when Config.PreferStreaming opts a session into it — see callModel.
//
// Like bufferedCall, it emits EventHeartbeat while the channel is quiet:
// a reasoning model can legitimately send nothing for tens of seconds
// before its first token (and pause mid-generation), and without a
// liveness signal the CLI's stall detector paints a scary warning over a
// perfectly healthy wait. Single goroutine, so event emission stays
// serialized.
func (s *Service) streamCall(ctx context.Context, model string, messages []openai.ChatMessage, toolDefs []openai.Tool, onEvent EventFunc) (gatewayclient.Result, error) {
	deltas, err := s.gateway.ChatCompletionStream(ctx, model, messages, toolDefs)
	if err != nil {
		return gatewayclient.Result{}, err
	}

	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()
	var content strings.Builder
	var result gatewayclient.Result
	for {
		select {
		case d, ok := <-deltas:
			if !ok {
				return result, nil
			}
			if d.Err != nil {
				// Return whatever partial answer already streamed alongside
				// the error — callModelWithRetry uses it to *resume* the
				// answer on the next Gateway Round instead of regenerating
				// (and re-displaying) everything from zero. Callers that
				// don't salvage just see the error as before.
				return gatewayclient.Result{Content: content.String()}, d.Err
			}
			if d.Content != "" {
				content.WriteString(d.Content)
				emit(onEvent, Event{Kind: EventDelta, Content: d.Content})
			} else if d.Reasoning != "" {
				emit(onEvent, Event{Kind: EventReasoning, Reasoning: d.Reasoning})
			}
			if d.Done {
				result = gatewayclient.Result{
					Content: content.String(), ToolCalls: d.ToolCalls,
					Provider: d.Provider, Attempts: d.Attempts, Usage: d.Usage, ProviderItems: d.ProviderItems,
					Ranking: d.Ranking, Strategy: d.Strategy,
				}
			}
		case <-ticker.C:
			emit(onEvent, Event{Kind: EventHeartbeat})
		}
	}
}

// runTool executes one requested tool call. Between EventToolStart and
// the tool's result there are no incremental events at all, so a long
// tool run (a test suite, a build) emits EventHeartbeat on the same
// cadence the model-call paths use — otherwise the CLI's stall detector
// reads a healthy 30s `go test` exactly like a hung connection.
func (s *Service) runTool(ctx context.Context, tc openai.ToolCall, onEvent EventFunc) (ToolActivity, store.Message) {
	var out toolOutcome
	s.heartbeatWhile(onEvent, func() {
		out = s.execTool(ctx, tc)
	})
	return out.activity, out.msg
}

// heartbeatWhile runs fn on its own goroutine and emits EventHeartbeat
// from this one until it returns — the single emitter, so concurrent
// work under fn never races on onEvent.
func (s *Service) heartbeatWhile(onEvent EventFunc, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			emit(onEvent, Event{Kind: EventHeartbeat})
		}
	}
}

// toolOutcome pairs what one executed tool call produced.
type toolOutcome struct {
	activity ToolActivity
	msg      store.Message
}

// execTool is runTool's synchronous core: execute one call, shape the
// activity and the persistable message. Emits nothing — safe to run
// concurrently (see runToolBatch).
func (s *Service) execTool(ctx context.Context, tc openai.ToolCall) toolOutcome {
	resultText, err := s.tools.Execute(ctx, tc.Function.Name, []byte(tc.Function.Arguments))
	ok := err == nil
	if err != nil {
		resultText = fmt.Sprintf("error: %v", err)
	}

	display := resultText
	if len(display) > maxToolResultChars {
		display = display[:maxToolResultChars] + "…"
	}

	activity := ToolActivity{
		Name: tc.Function.Name, Args: tc.Function.Arguments, Result: display, OK: ok,
		ProcessID: tools.StartedBackgroundProcessID(resultText),
	}
	toolMsg := store.Message{Role: "tool", Content: resultText, ToolCallID: tc.ID, Name: tc.Function.Name}
	return toolOutcome{activity: activity, msg: toolMsg}
}

// maxParallelReadOnlyTools bounds how many read-only calls of one batch
// run at once — enough to collapse the common "read three files, grep
// two patterns" round-trip, small enough to stay gentle on disk and CPU.
const maxParallelReadOnlyTools = 4

// batchHasMutation reports whether any call in the batch is not on the
// read-only allowlist — the trigger for the automatic pre-mutation
// checkpoint above.
func batchHasMutation(calls []openai.ToolCall) bool {
	for _, tc := range calls {
		if !tools.IsReadOnly(tc.Function.Name) {
			return true
		}
	}
	return false
}

// runToolBatch executes one model turn's tool calls, preserving request
// order in the returned outcomes. Contiguous stretches of read-only
// calls (tools.IsReadOnly) run concurrently under one central heartbeat;
// anything else runs strictly sequentially, ordered relative to
// everything — a write between two reads keeps its exact position.
// EventToolStart is emitted in request order (a parallel group's starts
// all fire before the group runs, which is also the honest display: they
// really are in flight together).
func (s *Service) runToolBatch(ctx context.Context, calls []openai.ToolCall, onEvent EventFunc) []toolOutcome {
	outcomes := make([]toolOutcome, len(calls))
	i := 0
	for i < len(calls) {
		if !tools.IsReadOnly(calls[i].Function.Name) {
			tc := calls[i]
			emit(onEvent, Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
			activity, msg := s.runTool(ctx, tc, onEvent)
			outcomes[i] = toolOutcome{activity: activity, msg: msg}
			i++
			continue
		}
		j := i
		for j < len(calls) && tools.IsReadOnly(calls[j].Function.Name) {
			j++
		}
		group := calls[i:j]
		for _, tc := range group {
			emit(onEvent, Event{Kind: EventToolStart, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
		}
		groupOut := outcomes[i:j]
		s.heartbeatWhile(onEvent, func() {
			sem := make(chan struct{}, maxParallelReadOnlyTools)
			var wg sync.WaitGroup
			for idx := range group {
				wg.Add(1)
				sem <- struct{}{}
				go func(idx int) {
					defer wg.Done()
					defer func() { <-sem }()
					groupOut[idx] = s.execTool(ctx, group[idx])
				}(idx)
			}
			wg.Wait()
		})
		i = j
	}
	return outcomes
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

func (s *Service) comboSupportsImages(ctx context.Context, model string) (bool, error) {
	status, err := s.gateway.Status(ctx)
	if err != nil {
		return false, err
	}
	return status.ComboSupportsImages(model), nil
}

func toModelMessages(msgs []store.Message) []openai.ChatMessage {
	out := make([]openai.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, openai.ChatMessage{
			Role: m.Role, Content: m.Content, Images: m.Images,
			ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, Name: m.Name,
			ProviderItems: m.ProviderItems,
		})
	}
	return out
}

func sumUsage(a, b openai.Usage) openai.Usage {
	return openai.AddUsage(a, b)
}
