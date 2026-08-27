package app

// Live thinking-line labels and meta segments (see thinkingLine/activityLabel).
const (
	activityModelActive       = "MODEL ACTIVE"
	activityRunningToolPrefix = "RUNNING · "
	activityRunningTool       = "RUNNING TOOL"
	activityAnalyzingResult   = "ANALYZING RESULT"
	activityWriting           = "WRITING"
	activityPreparingRoute    = "PREPARING ROUTE"

	thinkingPulseFmt        = " · pulse %d"
	thinkingSegmentFmt      = " · segment %d/%d"
	thinkingReasoningPrefix = " · thinking: "
	thinkingInterruptHint   = " · esc interrupts"
	thinkingStalledLabel    = "NO DATA"
	thinkingStalledMetaFmt  = "%s · quiet %s · total %s · esc interrupts"

	// stallCtx* say what was in flight when the stream went quiet (see
	// stallContext) — the phase is the most useful fact the client has.
	stallCtxModel     = "waiting for the model's first output"
	stallCtxMidAnswer = "stream went quiet mid-answer"
	stallCtxTool      = "a tool is still running"
	stallCtxToolFmt   = "tool %s still running"
	stallCtxAnalyzing = "model re-reading tool results"
)

// Transcript and top-level view chrome.
const (
	transcriptErrPrefix    = "error: "
	viewStarting           = "starting…"
	toolPreviewOverflowFmt = "%s… +%d lines"
)

// Footer keyboard-shortcut hints.
const (
	footerHintProcesses = "^b processes"
	footerHintRoute     = "^r route"
	footerHintContext   = "^t context"
	footerHintStrategy  = "^s strategy"
	footerHintDetails   = "^p details"
)
