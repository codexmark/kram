package agent

// EventKind identifies what an Event carries.
type EventKind string

const (
	// EventDelta is a text fragment as the model generates it — the
	// live-streaming case. Only fires for genuine content; tool-call
	// turns produce no deltas (the model isn't "saying" anything, it's
	// deciding what to call).
	EventDelta EventKind = "delta"
	// EventToolStart fires right before a tool call executes.
	EventToolStart EventKind = "tool_start"
	// EventToolResult fires right after, success or failure either way.
	EventToolResult EventKind = "tool_result"
	// EventNotice carries an out-of-band notice (image capability
	// fallback, a compaction just happened) as soon as it's known, rather
	// than only at the very end.
	EventNotice EventKind = "notice"
	// EventQuestion fires when the ask_question tool pauses the turn —
	// the caller shows Question/Options and is expected to answer via
	// Service.AnswerQuestion(QuestionID, ...) on a separate call, since the
	// turn (and this SSE stream) stays blocked until it does.
	EventQuestion EventKind = "question"
	// EventApproval fires when the permission policy marks a tool call
	// Ask — distinct from EventQuestion: the model isn't uncertain here,
	// it knows exactly what it wants to do, but policy requires the
	// user's sign-off first. The caller shows ApprovalTool/ApprovalSubject
	// and is expected to answer via Service.AnswerApproval(ApprovalID,
	// "once"|"always"|"deny") on a separate call, same blocking shape as
	// EventQuestion.
	EventApproval EventKind = "approval"
	// EventRouteStart fires right before a model call goes out — the
	// earliest point a live route bar can show "routing…" instead of
	// staying blank until the whole call finishes. Real per-attempt
	// progress (which candidate is being tried right now, mid-fallback)
	// isn't observable here: the gateway's fallback loop happens inside
	// one HTTP round-trip the daemon only sees the result of, so the next
	// signal is EventRouteDone — see DECISIONS.md, "Live route events are
	// per model call, not per attempt."
	EventRouteStart EventKind = "route_start"
	// EventRouteDone fires once a model call's full routing story is
	// known: every attempt made, the winner, and (for a scoring strategy)
	// the full candidate ranking — see RouteCall.
	EventRouteDone EventKind = "route_done"
)

// Event is one thing that happened during Run, emitted live via the
// OnEvent callback so a caller (the daemon's HTTP layer, ultimately the
// CLI) can show it as it happens instead of only after the whole turn
// completes.
type Event struct {
	Kind       EventKind
	Content    string   // EventDelta
	ToolName   string   // EventToolStart, EventToolResult
	ToolArgs   string   // EventToolStart
	ToolResult string   // EventToolResult
	ToolOK     bool     // EventToolResult
	Notice     string   // EventNotice
	QuestionID string   // EventQuestion
	Question   string   // EventQuestion
	Options    []string // EventQuestion

	ApprovalID      string // EventApproval
	ApprovalTool    string // EventApproval
	ApprovalSubject string // EventApproval

	RouteCall *RouteCall // EventRouteDone
}

// EventFunc receives live events during Run. A nil EventFunc is valid —
// Run behaves the same either way, just without the live callback.
type EventFunc func(Event)

func emit(fn EventFunc, evt Event) {
	if fn != nil {
		fn(evt)
	}
}
