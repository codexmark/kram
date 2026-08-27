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

// One-key rewind (Ctrl+G) notices — see rewind.go.
const (
	rewindTurnRunningNotice  = "a turn is running — esc interrupts it first, then ctrl+g rewinds"
	rewindNoCheckpointNotice = "no checkpoint yet — one is saved before a turn's first file change"
	rewindArmedNoticeFmt     = "rewind to checkpoint %s (%s)? ctrl+g confirms · esc cancels"
	rewindRestoringNotice    = "restoring checkpoint…"
	rewindFailedPrefix       = "rewind failed: "
	rewindDoneNoticeFmt      = "✓ rewound %d file(s) to checkpoint %s"
)

// Mid-turn steering notices (see submit's waiting branch).
const (
	steerQueuedNotice = "queued mid-turn — the agent picks this up at its next step"
	steerImagesNotice = "images can't be queued mid-turn — wait for the turn to finish"
	steerRaceNotice   = "the turn just ended — press enter to send normally"
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
