package app

// Context-window panel strings. The category-label consts are the display
// VALUES only — the map keys they replace are the daemon's internal category
// ids and stay unchanged.
const (
	ctxCatMessages          = "messages"
	ctxCatSystemPrompt      = "system prompt"
	ctxCatToolOverview      = "tool overview"
	ctxCatToolDefinitions   = "tool definitions"
	ctxCatProjectContext    = "project context (AGENTS.md)"
	ctxCatMemory            = "cross-session memory"
	ctxCatCompactionSummary = "compaction summary"
	ctxCatResponseReserve   = "response reserve"
	ctxCatFree              = "free space"

	contextReadErrPrefix = "couldn't read context usage: "
	contextLoading       = "loading…"
	contextWindowLine    = "context window · %s / %s (%d%%)"
)
