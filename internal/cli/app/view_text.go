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
	thinkingStalledLabel    = "NO STREAM EVENTS"
	thinkingStalledMetaFmt  = "%s ago · total %s · esc interrupts"
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
